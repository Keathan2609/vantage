package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/metrics"
)

// TwelveDataProvider acquires historical bars from Twelve Data.
//
// # What it is and is not
//
// It is a HISTORY source. Vantage owns the data once it arrives: bars are
// validated, normalised and stored, and everything downstream reads Vantage's
// store rather than the provider. The provider is replaceable; the stored
// series is the asset.
//
// It is NOT a broker, and it does not execute anything. It is also not a
// quote source for the live path in this milestone -- Quote returns an error
// rather than a synthesised price, because a provider that invents a live
// quote to satisfy an interface is how a strategy ends up trading a number
// nobody published.
//
// # The response is untrusted
//
// It arrives over a network from a system this project does not control, so
// it is read with a byte ceiling, decoded strictly, and every bar is checked
// for the shapes that break a consumer -- non-finite numbers, non-positive
// prices, incoherent OHLC, timestamps that do not parse. A provider having a
// bad day must not be able to put a corrupt bar into the database.
//
// # The base URL is configuration, never a parameter
//
// A provider URL a caller could set is an SSRF primitive pointed at whatever
// the server can reach. It is fixed at construction, validated to be https,
// and no request path is ever built from user input.
type TwelveDataProvider struct {
	baseURL string
	apiKey  string
	symbols *SymbolMap
	client  *http.Client
	clock   domain.Clock

	limiter *rateLimiter

	mu     sync.RWMutex
	health ProviderHealth
}

// ProviderState is the coarse condition of an external data provider.
type ProviderState string

const (
	ProviderConnected     ProviderState = "CONNECTED"
	ProviderDegraded      ProviderState = "DEGRADED"
	ProviderRateLimited   ProviderState = "RATE_LIMITED"
	ProviderUnavailable   ProviderState = "UNAVAILABLE"
	ProviderMisconfigured ProviderState = "MISCONFIGURED"
)

// ProviderHealth is what an operator needs to judge a data source.
//
// It carries no credential and no response body. A health endpoint is one of
// the easiest places to leak a secret, because it is the one place everybody
// agrees should show "everything about the connection".
type ProviderHealth struct {
	Provider            string        `json:"provider"`
	State               ProviderState `json:"state"`
	Configured          bool          `json:"configured"`
	LastSuccessAt       *time.Time    `json:"last_success_at,omitempty"`
	LastFailureAt       *time.Time    `json:"last_failure_at,omitempty"`
	LastFailureReason   string        `json:"last_failure_reason,omitempty"`
	LastLatencyMillis   int64         `json:"last_latency_ms,omitempty"`
	LastMarketTimestamp *time.Time    `json:"last_market_timestamp,omitempty"`
	RequestsThisMinute  int           `json:"requests_this_minute"`
	RequestsPerMinute   int           `json:"requests_per_minute"`
}

// Limits Twelve Data imposes that this client must respect.
const (
	// twelveDataMaxOutputSize is the largest outputsize the API accepts in one
	// request. Chunking exists because of this number.
	twelveDataMaxOutputSize = 5000
	// twelveDataMaxResponseBytes caps what will be read from the wire. A
	// provider that answers with a gigabyte -- through fault or compromise --
	// must not exhaust this process's memory.
	twelveDataMaxResponseBytes = 32 << 20 // 32 MiB
	// twelveDataMaxRetries bounds retry. Never infinite: a provider that is
	// down stays down, and a retry loop with no ceiling turns one outage into
	// an outage plus a request flood.
	twelveDataMaxRetries = 3
)

// NewTwelveDataProvider builds a provider.
//
// An empty apiKey is accepted and yields a provider that reports MISCONFIGURED
// and refuses every request. That is deliberate: the platform must start
// without a key (see config.TwelveDataAPIKey), and a nil provider would push a
// nil check into every caller.
func NewTwelveDataProvider(
	baseURL, apiKey string, symbols *SymbolMap, clock domain.Clock,
	requestsPerMinute int, timeout time.Duration,
) (*TwelveDataProvider, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("marketdata: twelve data base URL is not a URL: %w", err)
	}
	// https only, and no path or query. Both are checked because a base URL
	// carrying a path would let a relative endpoint escape it.
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf(
			"marketdata: twelve data base URL must be https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, errors.New("marketdata: twelve data base URL has no host")
	}
	if parsed.RawQuery != "" || strings.Trim(parsed.Path, "/") != "" {
		return nil, fmt.Errorf(
			"marketdata: twelve data base URL must be an origin with no path or query, got %q",
			baseURL)
	}
	if requestsPerMinute <= 0 {
		requestsPerMinute = 7
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	p := &TwelveDataProvider{
		baseURL: strings.TrimRight(parsed.String(), "/"),
		apiKey:  strings.TrimSpace(apiKey),
		symbols: symbols,
		clock:   clock,
		client: &http.Client{
			Timeout: timeout,
			// Redirects are refused. A provider that redirects is a provider
			// pointing this client somewhere it was not configured to go, and
			// following it would carry the API key to that destination.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		limiter: newRateLimiter(requestsPerMinute),
	}
	p.health = ProviderHealth{
		Provider:          p.Name(),
		Configured:        p.apiKey != "",
		RequestsPerMinute: requestsPerMinute,
	}
	if p.apiKey == "" {
		p.health.State = ProviderMisconfigured
		p.health.LastFailureReason = "TWELVE_DATA_API_KEY is not configured"
	} else {
		p.health.State = ProviderConnected
	}
	return p, nil
}

// Name identifies the provider in stored bars and provenance.
func (p *TwelveDataProvider) Name() string { return "twelvedata" }

// Configured reports whether an API key is present.
func (p *TwelveDataProvider) Configured() bool { return p.apiKey != "" }

// Health returns the current provider condition. Never carries the key.
func (p *TwelveDataProvider) Health() ProviderHealth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	h := p.health
	h.RequestsThisMinute = p.limiter.used()
	return h
}

// Quote is not supported.
//
// Returning an error rather than a synthesised price is the point. This
// provider supplies history; a live two-sided quote it does not have would be
// invented, and an invented quote is indistinguishable downstream from a real
// one.
func (p *TwelveDataProvider) Quote(context.Context, domain.Instrument, time.Time) (domain.Quote, error) {
	return domain.Quote{}, fmt.Errorf(
		"marketdata: %s supplies historical bars only and has no live quote", p.Name())
}

// ErrProviderNotConfigured is returned when no API key is present.
var ErrProviderNotConfigured = errors.New("marketdata: twelve data API key is not configured")

// HistoricalBars fetches completed bars for one instrument and timeframe.
//
// The window is closed-open [from, to) and the result is ascending by open
// time. Only COMPLETE bars are returned: the provider's most recent candle is
// usually still forming, and a forming bar stored as history is read by a
// strategy as fact and then changes underneath it.
func (p *TwelveDataProvider) HistoricalBars(
	ctx context.Context, inst domain.Instrument, tf domain.Timeframe, from, to time.Time,
) ([]domain.Bar, error) {
	if !p.Configured() {
		return nil, ErrProviderNotConfigured
	}
	vendorSymbol, err := p.symbols.Vendor(inst.ID)
	if err != nil {
		return nil, err
	}
	interval, err := twelveDataInterval(tf)
	if err != nil {
		return nil, err
	}
	dur, err := tf.Duration()
	if err != nil {
		return nil, err
	}

	payload, err := p.timeSeries(ctx, vendorSymbol, interval, from, to)
	if err != nil {
		return nil, err
	}

	bars := make([]domain.Bar, 0, len(payload.Values))
	for _, v := range payload.Values {
		bar, berr := v.toBar(inst, tf, dur, p.Name())
		if berr != nil {
			// One malformed row does not discard the response, but it is never
			// guessed at either: the bar is dropped and counted, so a provider
			// quietly degrading shows up as a metric rather than as a gap
			// nobody noticed.
			metrics.MarketDataInvalidBars.WithLabelValues(p.Name(), inst.ID).Inc()
			continue
		}
		// The provider is asked for a window but is not trusted to honour it.
		if bar.OpenTime.Before(from) || !bar.OpenTime.Before(to) {
			continue
		}
		bars = append(bars, bar)
	}

	// SORTED, not reversed.
	//
	// The request asks for ascending order and Twelve Data's default is
	// newest-first, so an unconditional reversal is right exactly when the
	// provider ignores the parameter and wrong when it honours it. Sorting is
	// correct either way, and the order matters more than it looks: a caller
	// that received these backwards would compute every forward outcome
	// against the wrong future.
	sort.Slice(bars, func(i, j int) bool {
		return bars[i].OpenTime.Before(bars[j].OpenTime)
	})
	return bars, nil
}

// timeSeriesResponse is the subset of the API response this client reads.
type timeSeriesResponse struct {
	Meta struct {
		Symbol           string `json:"symbol"`
		Interval         string `json:"interval"`
		ExchangeTimezone string `json:"exchange_timezone"`
		Type             string `json:"type"`
	} `json:"meta"`
	Values []timeSeriesValue `json:"values"`
	Status string            `json:"status"`
	// Errors arrive with HTTP 200 and a code in the body, which is why a
	// status-code check alone is not enough.
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type timeSeriesValue struct {
	Datetime string `json:"datetime"`
	Open     string `json:"open"`
	High     string `json:"high"`
	Low      string `json:"low"`
	Close    string `json:"close"`
	Volume   string `json:"volume"`
}

func (v timeSeriesValue) toBar(
	inst domain.Instrument, tf domain.Timeframe, dur time.Duration, provider string,
) (domain.Bar, error) {
	// UTC explicitly. The request asks for UTC and the response is parsed as
	// UTC; nothing here consults the host's timezone, because a developer's
	// locale is not a property of the market.
	openTime, err := parseTwelveDataTime(v.Datetime)
	if err != nil {
		return domain.Bar{}, err
	}

	price := func(raw string) (decimal.Decimal, error) {
		d, derr := decimal.NewFromString(strings.TrimSpace(raw))
		if derr != nil {
			return decimal.Zero, fmt.Errorf("unparseable price %q: %w", raw, derr)
		}
		if !d.IsPositive() {
			return decimal.Zero, fmt.Errorf("non-positive price %q", raw)
		}
		return d, nil
	}

	o, err := price(v.Open)
	if err != nil {
		return domain.Bar{}, err
	}
	h, err := price(v.High)
	if err != nil {
		return domain.Bar{}, err
	}
	l, err := price(v.Low)
	if err != nil {
		return domain.Bar{}, err
	}
	c, err := price(v.Close)
	if err != nil {
		return domain.Bar{}, err
	}

	// OHLC coherence, checked here rather than left to the database
	// constraint. The constraint would reject the whole batch; this rejects
	// the one bad bar and lets the rest through, and the database check then
	// stands as the second lock.
	if h.LessThan(l) || h.LessThan(o) || h.LessThan(c) || l.GreaterThan(o) || l.GreaterThan(c) {
		return domain.Bar{}, fmt.Errorf("incoherent OHLC at %s", v.Datetime)
	}

	volume := decimal.Zero
	if trimmed := strings.TrimSpace(v.Volume); trimmed != "" {
		if parsed, verr := decimal.NewFromString(trimmed); verr == nil && !parsed.IsNegative() {
			volume = parsed
		}
	}

	return domain.Bar{
		InstrumentID: inst.ID,
		Timeframe:    tf,
		OpenTime:     openTime,
		CloseTime:    openTime.Add(dur),
		Open:         inst.Spec.RoundPrice(o),
		High:         inst.Spec.RoundPrice(h),
		Low:          inst.Spec.RoundPrice(l),
		Close:        inst.Spec.RoundPrice(c),
		Volume:       volume,
		Complete:     true,
		Provider:     provider,
	}, nil
}

// twelveDataDateLayout is the request date format the API documents.
//
// The RESPONSE uses "2006-01-02 15:04:05" (space) or "2006-01-02", which is
// why parseTwelveDataTime below accepts those. The REQUEST is documented
// differently, and the two are not interchangeable just because both describe
// the same instant.
const twelveDataDateLayout = "2006-01-02T15:04:05"

// parseTwelveDataTime reads the two datetime shapes the API emits, as UTC.
func parseTwelveDataTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable datetime %q", raw)
}

// twelveDataInterval maps a Vantage timeframe to the provider's spelling.
func twelveDataInterval(tf domain.Timeframe) (string, error) {
	switch string(tf) {
	case "1m":
		return "1min", nil
	case "5m":
		return "5min", nil
	case "15m":
		return "15min", nil
	case "30m":
		return "30min", nil
	case "1h":
		return "1h", nil
	case "4h":
		return "4h", nil
	case "1d":
		return "1day", nil
	default:
		return "", fmt.Errorf("marketdata: twelve data has no interval for timeframe %q", tf)
	}
}

// timeSeries performs one bounded, throttled, retried request.
func (p *TwelveDataProvider) timeSeries(
	ctx context.Context, vendorSymbol, interval string, from, to time.Time,
) (*timeSeriesResponse, error) {
	query := url.Values{}
	query.Set("symbol", vendorSymbol)
	query.Set("interval", interval)
	// The DOCUMENTED date format is "2006-01-02" or "2006-01-02T15:04:05".
	// This sent a space separator instead, which the API may or may not
	// tolerate -- and a date the provider silently reinterprets is worse than
	// one it rejects, because the response still looks like a success and the
	// window is quietly not the window that was asked for.
	//
	// NOT verified against the live API: no key is configured on this machine.
	// It is verified against the published documentation and pinned by a test.
	query.Set("start_date", from.UTC().Format(twelveDataDateLayout))
	query.Set("end_date", to.UTC().Format(twelveDataDateLayout))
	query.Set("outputsize", strconv.Itoa(twelveDataMaxOutputSize))
	query.Set("timezone", "UTC")
	query.Set("order", "ASC")
	query.Set("format", "JSON")
	query.Set("apikey", p.apiKey)

	endpoint := p.baseURL + "/time_series?" + query.Encode()

	var lastErr error
	for attempt := 0; attempt <= twelveDataMaxRetries; attempt++ {
		if attempt > 0 {
			// Bounded exponential backoff. The ceiling matters more than the
			// growth: an unbounded sleep inside a request handler is an
			// availability problem of our own making.
			delay := time.Duration(1<<uint(attempt-1)) * time.Second
			if delay > 8*time.Second {
				delay = 8 * time.Second
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		if err := p.limiter.wait(ctx, p.clock); err != nil {
			return nil, err
		}

		payload, retryable, err := p.doRequest(ctx, endpoint)
		if err == nil {
			p.recordSuccess(payload)
			return payload, nil
		}
		lastErr = err
		p.recordFailure(err)
		if !retryable {
			return nil, err
		}
	}
	return nil, fmt.Errorf("marketdata: twelve data failed after %d attempts: %w",
		twelveDataMaxRetries+1, lastErr)
}

// doRequest performs one HTTP call. The bool reports whether a retry could help.
func (p *TwelveDataProvider) doRequest(
	ctx context.Context, endpoint string,
) (*timeSeriesResponse, bool, error) {
	started := p.clock.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, fmt.Errorf("marketdata: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	metrics.MarketDataProviderRequests.WithLabelValues(p.Name()).Inc()
	resp, err := p.client.Do(req)
	if err != nil {
		metrics.MarketDataProviderFailures.WithLabelValues(p.Name(), "network").Inc()
		// The error from client.Do is a *url.Error carrying the FULL request
		// URL, and this API takes its credential as a QUERY PARAMETER. Go's
		// own redaction strips userinfo passwords and leaves the query
		// untouched, so wrapping this verbatim put the key into the health
		// endpoint, the stored segment record and the log line. It did: a
		// probe against a dead port printed the key three times.
		//
		// Only the transport CAUSE is kept, and it is scrubbed as well. A
		// network error is worth one more try; a DNS failure is not
		// distinguishable here, so the retry ceiling is what bounds it.
		return nil, true, fmt.Errorf("marketdata: twelve data request failed: %s",
			p.scrub(transportCause(err)))
	}
	defer func() { _ = resp.Body.Close() }()

	p.setLatency(p.clock.Now().Sub(started))

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		metrics.MarketDataProviderFailures.WithLabelValues(p.Name(), "rate_limited").Inc()
		metrics.MarketDataRateLimitEvents.WithLabelValues(p.Name()).Inc()
		p.setState(ProviderRateLimited)
		return nil, true, errors.New("marketdata: twelve data rate limit reached")
	case resp.StatusCode >= 500:
		metrics.MarketDataProviderFailures.WithLabelValues(p.Name(), "server").Inc()
		return nil, true, fmt.Errorf("marketdata: twelve data returned %d", resp.StatusCode)
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		metrics.MarketDataProviderFailures.WithLabelValues(p.Name(), "auth").Inc()
		// Not retryable and deliberately vague: the key is the thing being
		// rejected, and an error string that echoed it would put it in a log.
		return nil, false, errors.New("marketdata: twelve data rejected the API key")
	case resp.StatusCode != http.StatusOK:
		metrics.MarketDataProviderFailures.WithLabelValues(p.Name(), "status").Inc()
		return nil, false, fmt.Errorf("marketdata: twelve data returned %d", resp.StatusCode)
	}

	// Bounded read. A provider that answers with more than the ceiling is
	// truncated and the decode then fails, which is the correct outcome: a
	// half-read JSON document must not become half a dataset.
	body, err := io.ReadAll(io.LimitReader(resp.Body, twelveDataMaxResponseBytes+1))
	if err != nil {
		return nil, true, fmt.Errorf("marketdata: reading twelve data response: %w", err)
	}
	if len(body) > twelveDataMaxResponseBytes {
		metrics.MarketDataProviderFailures.WithLabelValues(p.Name(), "oversized").Inc()
		return nil, false, fmt.Errorf(
			"marketdata: twelve data response exceeded %d bytes", twelveDataMaxResponseBytes)
	}

	decoder := json.NewDecoder(strings.NewReader(string(body)))
	// Unknown fields are allowed -- a provider adding a field must not break
	// ingestion -- but the document must be exactly one JSON value.
	var payload timeSeriesResponse
	if err := decoder.Decode(&payload); err != nil {
		metrics.MarketDataProviderFailures.WithLabelValues(p.Name(), "malformed_json").Inc()
		return nil, false, fmt.Errorf("marketdata: twelve data sent malformed JSON: %w", err)
	}
	if decoder.More() {
		metrics.MarketDataProviderFailures.WithLabelValues(p.Name(), "malformed_json").Inc()
		return nil, false, errors.New("marketdata: twelve data sent trailing content after the JSON document")
	}

	// Twelve Data reports errors with HTTP 200 and a code in the body.
	if payload.Status == "error" || payload.Code != 0 {
		switch payload.Code {
		case 429:
			p.setState(ProviderRateLimited)
			metrics.MarketDataRateLimitEvents.WithLabelValues(p.Name()).Inc()
			return nil, true, errors.New("marketdata: twelve data rate limit reached")
		case 401, 403:
			return nil, false, errors.New("marketdata: twelve data rejected the API key")
		default:
			return nil, false, fmt.Errorf(
				"marketdata: twelve data error %d: %s", payload.Code, sanitiseProviderMessage(payload.Message))
		}
	}
	return &payload, false, nil
}

// sanitiseProviderMessage bounds and cleans a message before it reaches a log.
//
// The provider's text is attacker-influenced in the general case and is at
// minimum outside our control. It is truncated so it cannot flood a log, and
// newlines are stripped so it cannot forge additional log lines.
func sanitiseProviderMessage(msg string) string {
	msg = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, strings.TrimSpace(msg))
	const limit = 200
	if len(msg) > limit {
		return msg[:limit] + "…"
	}
	return msg
}

func (p *TwelveDataProvider) recordSuccess(payload *timeSeriesResponse) {
	now := p.clock.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.health.State = ProviderConnected
	p.health.LastSuccessAt = &now
	p.health.LastFailureReason = ""
	if len(payload.Values) > 0 {
		if t, err := parseTwelveDataTime(payload.Values[len(payload.Values)-1].Datetime); err == nil {
			p.health.LastMarketTimestamp = &t
		}
	}
}

func (p *TwelveDataProvider) recordFailure(err error) {
	now := p.clock.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.health.LastFailureAt = &now
	// The error text is ours, not the provider's raw body, and never the key.
	// Scrubbed as well as sanitised. sanitiseProviderMessage bounds the length
	// and strips newlines; it does not know what a credential looks like, and
	// this field is served by the market-data status endpoint.
	p.health.LastFailureReason = p.scrub(sanitiseProviderMessage(err.Error()))
	if p.health.State != ProviderRateLimited {
		p.health.State = ProviderDegraded
	}
}

func (p *TwelveDataProvider) setState(state ProviderState) {
	p.mu.Lock()
	p.health.State = state
	p.mu.Unlock()
}

func (p *TwelveDataProvider) setLatency(d time.Duration) {
	p.mu.Lock()
	p.health.LastLatencyMillis = d.Milliseconds()
	p.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

// rateLimiter is a fixed-window request throttle.
//
// Deliberately simple and deliberately conservative. The alternative -- react
// to 429 responses -- means being throttled is the normal operating mode, and
// a provider that throttles reads downstream as an outage.
type rateLimiter struct {
	mu          sync.Mutex
	perMinute   int
	windowStart time.Time
	count       int
}

func newRateLimiter(perMinute int) *rateLimiter {
	return &rateLimiter{perMinute: perMinute}
}

func (r *rateLimiter) used() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// wait blocks until a request may proceed, or the context ends.
func (r *rateLimiter) wait(ctx context.Context, clock domain.Clock) error {
	for {
		r.mu.Lock()
		now := clock.Now()
		if r.windowStart.IsZero() || now.Sub(r.windowStart) >= time.Minute {
			r.windowStart = now
			r.count = 0
		}
		if r.count < r.perMinute {
			r.count++
			r.mu.Unlock()
			return nil
		}
		sleep := time.Minute - now.Sub(r.windowStart)
		r.mu.Unlock()

		if sleep <= 0 {
			sleep = time.Millisecond
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}
	}
}

var _ Provider = (*TwelveDataProvider)(nil)

// ---------------------------------------------------------------------------
// Credential hygiene
// ---------------------------------------------------------------------------

// scrub removes the API key from any text on its way out of this package.
//
// The SECOND lock. The first is not putting the key into an error at all --
// see the client.Do path in doRequest -- and this exists because that lock
// already failed once. Twelve Data takes its credential as a query parameter,
// so every error, log line and status field that can carry a URL can carry the
// key, and there are more of those than anyone enumerates correctly the first
// time: a redirect error, a TLS error, a proxy error, a future code path
// nobody has written yet.
//
// Applied at the boundary rather than at each call site, for the same reason.
func (p *TwelveDataProvider) scrub(s string) string {
	if p.apiKey == "" {
		return s
	}
	// Both the raw key and its percent-encoded form: url.Values.Encode escapes
	// the value, so a key containing a reserved character appears in the URL in
	// a shape that a plain replace of the raw key would miss entirely.
	s = strings.ReplaceAll(s, p.apiKey, redactedKey)
	if encoded := url.QueryEscape(p.apiKey); encoded != p.apiKey {
		s = strings.ReplaceAll(s, encoded, redactedKey)
	}
	return s
}

// redactedKey is what replaces a credential in outgoing text. Deliberately
// obvious: a reader seeing it should know a value was removed rather than
// wonder whether the field was empty.
const redactedKey = "«api key redacted»"

// transportCause unwraps a *url.Error down to the error that actually
// happened, discarding the URL the wrapper carries.
//
// url.Error.Error() renders as `Get "<full url>": <cause>`, and the URL is the
// part that holds the credential. The cause alone -- "connection refused",
// "context deadline exceeded", "no such host" -- is what an operator needs and
// carries nothing secret.
func transportCause(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err.Error()
	}
	return err.Error()
}
