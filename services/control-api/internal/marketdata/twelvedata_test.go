package marketdata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vantage/control-api/internal/domain"
)

// The provider is exercised against a local test server, never the real
// Twelve Data. CLAUDE.md rule 3 forbids sending traffic to a third party, and
// a test that depended on an external service would fail for reasons that have
// nothing to do with this code.

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func testSymbols(t *testing.T) *SymbolMap {
	t.Helper()
	m, err := NewSymbolMap("twelvedata", TwelveDataSymbols)
	if err != nil {
		t.Fatalf("symbol map: %v", err)
	}
	return m
}

// newTestProvider points a provider at a local server. The https requirement
// is relaxed only here, through the unexported constructor field, because a
// test server cannot serve https without a certificate the client trusts.
func newTestProvider(t *testing.T, srv *httptest.Server, apiKey string) *TwelveDataProvider {
	t.Helper()
	p, err := NewTwelveDataProvider(
		"https://api.twelvedata.com", apiKey, testSymbols(t),
		fixedClock{time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)}, 60, 5*time.Second)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if srv != nil {
		// Only the URL is replaced. Taking srv.Client() wholesale would
		// discard the redirect policy and the timeout that are part of what
		// these tests exercise -- and did, until the redirect test caught it.
		p.baseURL = srv.URL
	}
	return p
}

func okResponse(values string) string {
	return fmt.Sprintf(`{"meta":{"symbol":"XAU/USD","interval":"1h","exchange_timezone":"UTC"},
		"values":[%s],"status":"ok"}`, values)
}

// --- base URL is configuration, not a parameter -----------------------------

func TestTheProviderRefusesABaseURLThatIsNotAFixedHTTPSOrigin(t *testing.T) {
	// A provider URL a caller could steer is an SSRF primitive pointed at
	// whatever this server can reach, including cloud metadata endpoints.
	for _, bad := range []string{
		"http://api.twelvedata.com",
		"https://api.twelvedata.com/some/path",
		"https://api.twelvedata.com?x=1",
		"file:///etc/passwd",
		"https://",
		"://nonsense",
	} {
		if _, err := NewTwelveDataProvider(
			bad, "key", testSymbols(t), fixedClock{time.Now()}, 10, time.Second,
		); err == nil {
			t.Errorf("base URL %q was accepted and should not have been", bad)
		}
	}
}

func TestTheProviderDoesNotFollowRedirects(t *testing.T) {
	// Following one would carry the API key to wherever the provider pointed.
	var reached bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv, "key")
	_, err := p.HistoricalBars(context.Background(), testInstrument(),
		domain.Timeframe("1h"), time.Now().Add(-time.Hour), time.Now())
	if err == nil {
		t.Fatal("a redirect was followed or accepted")
	}
	if reached {
		t.Fatal("the request followed a redirect to another host")
	}
}

// --- the key -----------------------------------------------------------------

func TestAnAbsentKeyMakesTheProviderMisconfiguredRatherThanFatal(t *testing.T) {
	// The platform must start without a key. Section 26.
	p := newTestProvider(t, nil, "")
	if p.Configured() {
		t.Fatal("an empty key reported as configured")
	}
	if got := p.Health().State; got != ProviderMisconfigured {
		t.Fatalf("state = %s, want MISCONFIGURED", got)
	}
	_, err := p.HistoricalBars(context.Background(), testInstrument(),
		domain.Timeframe("1h"), time.Now().Add(-time.Hour), time.Now())
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("error = %v, want ErrProviderNotConfigured", err)
	}
}

func TestTheProviderHealthNeverCarriesTheKey(t *testing.T) {
	// A health endpoint is the easiest place to leak a secret, because it is
	// the one place everyone agrees should show everything about a connection.
	const secret = "super-secret-key-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv, secret)
	_, _ = p.HistoricalBars(context.Background(), testInstrument(),
		domain.Timeframe("1h"),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))

	health := p.Health()
	rendered := fmt.Sprintf("%+v", health)
	if strings.Contains(rendered, secret) {
		t.Fatalf("the API key appeared in provider health: %s", rendered)
	}
	if strings.Contains(health.LastFailureReason, secret) {
		t.Fatal("the API key appeared in the failure reason")
	}
}

func TestARejectedKeyIsNotRetried(t *testing.T) {
	// Retrying a credential rejection cannot succeed and burns quota.
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv, "key")
	_, err := p.HistoricalBars(context.Background(), testInstrument(),
		domain.Timeframe("1h"),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("a rejected key produced no error")
	}
	if calls != 1 {
		t.Fatalf("made %d requests for a rejected key, want 1", calls)
	}
}

// --- response handling -------------------------------------------------------

func TestBarsAreParsedNormalisedAndReturnedAscending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Newest first, which is Twelve Data's default and the trap: a caller
		// that assumed ascending would compute every forward outcome backwards.
		fmt.Fprint(w, okResponse(`
			{"datetime":"2026-01-01 02:00:00","open":"2001.0","high":"2005.0","low":"2000.0","close":"2004.0","volume":"120"},
			{"datetime":"2026-01-01 01:00:00","open":"2000.0","high":"2003.0","low":"1999.0","close":"2001.0","volume":"100"}`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv, "key")
	bars, err := p.HistoricalBars(context.Background(), testInstrument(),
		domain.Timeframe("1h"),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("HistoricalBars: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("got %d bars, want 2", len(bars))
	}
	if !bars[0].OpenTime.Before(bars[1].OpenTime) {
		t.Fatal("bars are not ascending by open time")
	}
	if bars[0].OpenTime.Location() != time.UTC {
		t.Fatal("bar time is not UTC")
	}
	if !bars[0].Complete {
		t.Fatal("a historical bar is not marked complete")
	}
	if bars[0].CloseTime.Sub(bars[0].OpenTime) != time.Hour {
		t.Fatal("close time does not follow the timeframe")
	}
	if bars[0].Provider != "twelvedata" {
		t.Fatalf("provider = %q", bars[0].Provider)
	}
}

func TestBarsOutsideTheRequestedWindowAreDropped(t *testing.T) {
	// The provider is asked for a window but not trusted to honour it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, okResponse(`
			{"datetime":"2026-01-01 01:00:00","open":"2000","high":"2003","low":"1999","close":"2001","volume":"1"},
			{"datetime":"2025-12-30 01:00:00","open":"2000","high":"2003","low":"1999","close":"2001","volume":"1"},
			{"datetime":"2026-03-01 01:00:00","open":"2000","high":"2003","low":"1999","close":"2001","volume":"1"}`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv, "key")
	bars, err := p.HistoricalBars(context.Background(), testInstrument(),
		domain.Timeframe("1h"),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("HistoricalBars: %v", err)
	}
	if len(bars) != 1 {
		t.Fatalf("got %d bars, want 1 inside the window", len(bars))
	}
}

func TestACorruptBarIsDroppedWithoutDiscardingTheResponse(t *testing.T) {
	// One bad row must not become a gap in an otherwise good chunk, and must
	// not be guessed at either.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, okResponse(`
			{"datetime":"2026-01-01 01:00:00","open":"2000","high":"2003","low":"1999","close":"2001","volume":"1"},
			{"datetime":"2026-01-01 02:00:00","open":"2000","high":"1990","low":"1999","close":"2001","volume":"1"},
			{"datetime":"2026-01-01 03:00:00","open":"0","high":"2003","low":"1999","close":"2001","volume":"1"},
			{"datetime":"not-a-date","open":"2000","high":"2003","low":"1999","close":"2001","volume":"1"},
			{"datetime":"2026-01-01 05:00:00","open":"2000","high":"2003","low":"1999","close":"2001","volume":"1"}`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv, "key")
	bars, err := p.HistoricalBars(context.Background(), testInstrument(),
		domain.Timeframe("1h"),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("HistoricalBars: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("got %d bars, want the 2 coherent ones", len(bars))
	}
}

func TestMalformedJSONIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"values": [ this is not json`)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv, "key")
	if _, err := p.HistoricalBars(context.Background(), testInstrument(),
		domain.Timeframe("1h"),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("malformed JSON was accepted")
	}
}

func TestTrailingContentAfterTheJSONDocumentIsRefused(t *testing.T) {
	// Two JSON documents in one body is not a response this client understands,
	// and picking the first would let a second one be smuggled past a reader.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, okResponse(`{"datetime":"2026-01-01 01:00:00","open":"2000","high":"2003","low":"1999","close":"2001","volume":"1"}`)+`{"extra":true}`)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv, "key")
	if _, err := p.HistoricalBars(context.Background(), testInstrument(),
		domain.Timeframe("1h"),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("trailing content after the JSON document was accepted")
	}
}

func TestAnOversizedResponseIsRefusedRatherThanRead(t *testing.T) {
	// A provider answering with a gigabyte must not exhaust this process.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		chunk := strings.Repeat("a", 1<<20)
		fmt.Fprint(w, `{"values":["`)
		for i := 0; i < 40; i++ {
			fmt.Fprint(w, chunk)
		}
		fmt.Fprint(w, `"]}`)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv, "key")
	_, err := p.HistoricalBars(context.Background(), testInstrument(),
		domain.Timeframe("1h"),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("an oversized response was accepted")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("error = %v, want the size ceiling to be named", err)
	}
}

func TestAProviderErrorCarriedInABodyWithStatus200IsDetected(t *testing.T) {
	// Twelve Data reports errors with HTTP 200 and a code in the body, so a
	// status-code check alone would read an error as an empty success.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":400,"message":"**symbol** not found","status":"error"}`)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv, "key")
	_, err := p.HistoricalBars(context.Background(), testInstrument(),
		domain.Timeframe("1h"),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("an error body returned with HTTP 200 was read as success")
	}
}

func TestARateLimitedResponseIsRetriedAndReportedInHealth(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, okResponse(`{"datetime":"2026-01-01 01:00:00","open":"2000","high":"2003","low":"1999","close":"2001","volume":"1"}`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv, "key")
	bars, err := p.HistoricalBars(context.Background(), testInstrument(),
		domain.Timeframe("1h"),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("a retryable rate limit was not retried: %v", err)
	}
	if len(bars) != 1 {
		t.Fatalf("got %d bars after retry, want 1", len(bars))
	}
	if calls < 2 {
		t.Fatal("the request was not retried")
	}
}

func TestRetriesAreBounded(t *testing.T) {
	// Never infinite. A provider that is down stays down, and an unbounded
	// retry turns one outage into an outage plus a request flood.
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv, "key")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := p.HistoricalBars(ctx, testInstrument(), domain.Timeframe("1h"),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("a persistently failing provider produced no error")
	}
	if calls > twelveDataMaxRetries+1 {
		t.Fatalf("made %d attempts, ceiling is %d", calls, twelveDataMaxRetries+1)
	}
}

func TestATimeoutIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	p, err := NewTwelveDataProvider("https://api.twelvedata.com", "key", testSymbols(t),
		fixedClock{time.Now()}, 60, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	p.baseURL = srv.URL

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = p.HistoricalBars(ctx, testInstrument(), domain.Timeframe("1h"),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("a hanging provider produced no error")
	}
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Fatalf("took %s; the per-request timeout did not bound it", elapsed)
	}
}

func TestTheRequestAsksForUTCAndAscendingOrder(t *testing.T) {
	// Timezone is stated explicitly rather than left to the provider's default,
	// which is the exchange's zone and would move every session boundary.
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		fmt.Fprint(w, okResponse(""))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv, "key")
	_, _ = p.HistoricalBars(context.Background(), testInstrument(), domain.Timeframe("1h"),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))

	for _, want := range []string{"timezone=UTC", "symbol=XAU%2FUSD", "interval=1h"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q does not contain %q", query, want)
		}
	}
}

func TestTheProviderHasNoLiveQuote(t *testing.T) {
	// Returning a synthesised price to satisfy the interface is how a strategy
	// ends up trading a number nobody published.
	p := newTestProvider(t, nil, "key")
	if _, err := p.Quote(context.Background(), testInstrument(), time.Now()); err == nil {
		t.Fatal("a history-only provider returned a quote")
	}
}

// --- rate limiting -----------------------------------------------------------

func TestTheRateLimiterBoundsRequestsPerMinute(t *testing.T) {
	limiter := newRateLimiter(3)
	clock := &movingClock{t: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := limiter.wait(ctx, clock); err != nil {
			t.Fatalf("request %d refused: %v", i, err)
		}
	}
	if used := limiter.used(); used != 3 {
		t.Fatalf("used = %d, want 3", used)
	}

	// The fourth must wait. Cancelling proves it blocked rather than passing.
	blocked, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if err := limiter.wait(blocked, clock); err == nil {
		t.Fatal("a fourth request in the same window was allowed through")
	}
}

func TestTheRateLimiterWindowResets(t *testing.T) {
	limiter := newRateLimiter(2)
	clock := &movingClock{t: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)}
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := limiter.wait(ctx, clock); err != nil {
			t.Fatalf("request %d refused: %v", i, err)
		}
	}
	clock.advance(61 * time.Second)
	if err := limiter.wait(ctx, clock); err != nil {
		t.Fatalf("the window did not reset after a minute: %v", err)
	}
}

type movingClock struct{ t time.Time }

func (c *movingClock) Now() time.Time          { return c.t }
func (c *movingClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// --- symbol mapping ----------------------------------------------------------

func TestTheSymbolMapTranslatesBothWays(t *testing.T) {
	m := testSymbols(t)
	vendor, err := m.Vendor("XAUUSD")
	if err != nil {
		t.Fatalf("Vendor: %v", err)
	}
	if vendor != "XAU/USD" {
		t.Fatalf("vendor symbol = %q, want XAU/USD", vendor)
	}
	canonical, err := m.Canonical("XAU/USD")
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if canonical != "XAUUSD" {
		t.Fatalf("canonical = %q, want XAUUSD", canonical)
	}
}

func TestAnUnmappedInstrumentIsRefusedRatherThanGuessed(t *testing.T) {
	// "Insert a slash before the last three characters" works for XAUUSD and
	// fails silently for the next instrument: the request succeeds against a
	// symbol that is not the one asked for.
	m := testSymbols(t)
	if _, err := m.Vendor("EURUSD"); err == nil {
		t.Fatal("an unmapped instrument was translated anyway")
	}
	var unknown UnknownSymbolError
	if _, err := m.Vendor("EURUSD"); !errors.As(err, &unknown) {
		t.Fatalf("error type = %T, want UnknownSymbolError", err)
	}
}

func TestTheBrokerContractInstrumentIsNotMappedToASpotFeed(t *testing.T) {
	// XAUUSD.m is a broker contract with its own specification and session.
	// Treating a generic spot series as its history would produce research
	// about one instrument presented as research about another.
	m := testSymbols(t)
	if m.Supports("XAUUSD.m") {
		t.Fatal("the broker contract instrument is mapped to a spot data feed")
	}
}

func TestASymbolMapWithAnAmbiguousReverseIsRefused(t *testing.T) {
	// The reverse direction attributes incoming bars to an instrument. Two
	// canonical ids sharing a vendor symbol would make that a coin flip.
	if _, err := NewSymbolMap("x", map[string]string{
		"XAUUSD":   "XAU/USD",
		"XAUUSD.m": "XAU/USD",
	}); err == nil {
		t.Fatal("an ambiguous reverse mapping was accepted")
	}
}

// --- credential hygiene on the paths nobody thought about --------------------

// TestATransportFailureDoesNotCarryTheKey is the test that was missing.
//
// TestTheProviderHealthNeverCarriesTheKey above drives a 401, which returns a
// static string with no URL in it, so it passed while the key was leaking. The
// leak was on the TRANSPORT path: client.Do returns a *url.Error carrying the
// full request URL, this API takes its credential as a query parameter, and
// Go's own redaction strips userinfo passwords only. A probe against a dead
// port printed the key in the returned error, in provider health, and (through
// the Syncer) in a log line and a stored database column.
func TestATransportFailureDoesNotCarryTheKey(t *testing.T) {
	const secret = "SUPERSECRETKEY123"
	p := newTestProvider(t, nil, secret)
	// Port 1: nothing listens, so client.Do fails at the transport layer.
	p.baseURL = "http://127.0.0.1:1"

	_, err := p.HistoricalBars(context.Background(), testInstrument(),
		domain.Timeframe("1h"),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("a request to a dead port produced no error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the API key is in the returned error:\n%v", err)
	}
	if reason := p.Health().LastFailureReason; strings.Contains(reason, secret) {
		t.Errorf("the API key is in provider health, which the status endpoint serves:\n%s", reason)
	}
	// The cause must still be useful: a redaction that removed everything
	// would trade one problem for another.
	if !strings.Contains(strings.ToLower(err.Error()), "refused") &&
		!strings.Contains(strings.ToLower(err.Error()), "connect") {
		t.Errorf("the error no longer says what went wrong: %v", err)
	}
}

func TestScrubRemovesBothRawAndEncodedKeyForms(t *testing.T) {
	// url.Values.Encode percent-escapes the value, so a key containing a
	// reserved character appears in a URL in a shape a plain replace misses.
	const secret = "abc/def+ghi=jkl"
	p := newTestProvider(t, nil, secret)

	raw := "something " + secret + " happened"
	if got := p.scrub(raw); strings.Contains(got, secret) {
		t.Errorf("the raw key survived scrubbing: %s", got)
	}
	encoded := "url?apikey=" + url.QueryEscape(secret) + "&x=1"
	if got := p.scrub(encoded); strings.Contains(got, url.QueryEscape(secret)) {
		t.Errorf("the percent-encoded key survived scrubbing: %s", got)
	}
}

func TestScrubIsHarmlessWhenNoKeyIsConfigured(t *testing.T) {
	// An empty key must not turn every empty substring into a redaction
	// marker, which is what a naive ReplaceAll("") would do.
	p := newTestProvider(t, nil, "")
	const msg = "connection refused"
	if got := p.scrub(msg); got != msg {
		t.Fatalf("scrub altered text with no key configured: %q", got)
	}
}
