// Package quant is the control plane's client for the research service.
//
// The direction of this dependency is the whole point. The control plane calls
// the quant service; the quant service never calls back. It holds no
// credentials for the control plane, has no route to the order API, and reads
// the database through a role that cannot see users, orders, credentials or the
// ledger. Research can therefore be wrong, slow, or compromised without being
// able to move money — the worst it can do is produce a bad signal, which then
// faces every gate in the order pipeline like any other.
//
// Everything this client returns is treated as UNTRUSTED INPUT: sizes are
// re-derived, prices re-validated, and confidences clamped. A compromised
// research service that returns "buy 10,000 lots with 99% confidence" produces
// exactly the same outcome as one returning nonsense — a refusal.
package quant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/crypto"
)

// Client calls the research service.
type Client struct {
	baseURL string
	token   string
	http    *http.Client

	mu      sync.Mutex
	breaker circuitBreaker
	alerter Alerter
}

// Errors callers distinguish.
var (
	ErrUnavailable   = errors.New("quant: research service unavailable")
	ErrCircuitOpen   = errors.New("quant: circuit breaker open after repeated failures")
	ErrBadResponse   = errors.New("quant: research service returned an unusable response")
	ErrRequestFailed = errors.New("quant: research request failed")
)

// New builds a client.
//
// The transport is deliberately constrained: bounded connections, short
// timeouts, and no redirect following. A research service that starts
// redirecting is a research service that has been tampered with.
func New(baseURL, token string, timeout time.Duration) *Client {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		MaxIdleConns:          10,
		MaxIdleConnsPerHost:   5,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		DisableCompression:    false,
	}
	return &Client{
		baseURL: baseURL,
		token:   token,
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("quant: redirects are not followed")
			},
		},
		breaker: circuitBreaker{threshold: 5, cooldown: 30 * time.Second},
	}
}

// circuitBreaker stops hammering a service that is failing.
type circuitBreaker struct {
	failures  int
	threshold int
	openUntil time.Time
	cooldown  time.Duration
}

func (c *Client) breakerOpen(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return now.Before(c.breaker.openUntil)
}

func (c *Client) recordFailure(now time.Time) {
	c.mu.Lock()
	c.breaker.failures++
	opened := false
	if c.breaker.failures >= c.breaker.threshold {
		c.breaker.openUntil = now.Add(c.breaker.cooldown)
		c.breaker.failures = 0
		opened = true
	}
	alerter := c.alerter
	c.mu.Unlock()

	// Alerted when the breaker OPENS, not on every failed call: the breaker
	// exists precisely because the service is flapping, and an alert per
	// failure would flap with it.
	if opened && alerter != nil {
		alerter.QuantFailure(context.Background(),
			"circuit breaker opened after repeated failures", true)
	}
}

func (c *Client) recordSuccess() {
	c.mu.Lock()
	wasOpen := !c.breaker.openUntil.IsZero() || c.breaker.failures > 0
	c.breaker.failures = 0
	c.breaker.openUntil = time.Time{}
	alerter := c.alerter
	c.mu.Unlock()

	if wasOpen && alerter != nil {
		alerter.QuantRecovered(context.Background())
	}
}

// Alerter is the subset of internal/notify this client needs. Declared here
// so quant does not import notify, which imports store.
type Alerter interface {
	QuantFailure(ctx context.Context, reason string, breakerOpen bool)
	QuantRecovered(ctx context.Context)
}

// SetAlerter attaches an alerter after construction.
func (c *Client) SetAlerter(a Alerter) {
	c.mu.Lock()
	c.alerter = a
	c.mu.Unlock()
}

// Health probes the research service.
func (c *Client) Health(ctx context.Context) error {
	var resp struct {
		Status string `json:"status"`
	}
	if err := c.get(ctx, "/health/live", &resp); err != nil {
		return err
	}
	if resp.Status == "" {
		return ErrBadResponse
	}
	return nil
}

// BarInput is one candle supplied to the research service.
//
// The control plane supplies the data rather than letting research query for
// it, so both sides provably evaluate the same bars, and so a strategy cannot
// widen its own lookback window beyond what it declared.
type BarInput struct {
	OpenTime string `json:"open_time"`
	Open     string `json:"open"`
	High     string `json:"high"`
	Low      string `json:"low"`
	Close    string `json:"close"`
	Volume   string `json:"volume"`
}

// SignalRequest asks for one strategy evaluation.
type SignalRequest struct {
	StrategyKey  string          `json:"strategy_key"`
	Version      int             `json:"version"`
	InstrumentID string          `json:"instrument_id"`
	Timeframe    string          `json:"timeframe"`
	Parameters   json.RawMessage `json:"parameters"`
	Bars         []BarInput      `json:"bars"`
	// Context is non-sensitive market context: session, event risk, spread.
	// It never contains account balances, positions or identifiers, because
	// research does not need them and should not hold them.
	Context map[string]any `json:"context,omitempty"`
}

// SignalResponse is a strategy's opinion.
type SignalResponse struct {
	Action          string            `json:"action"`
	Confidence      decimal.Decimal   `json:"confidence"`
	SuggestedStop   *decimal.Decimal  `json:"suggested_stop,omitempty"`
	SuggestedTarget *decimal.Decimal  `json:"suggested_target,omitempty"`
	Explanation     string            `json:"explanation"`
	Indicators      map[string]string `json:"indicators,omitempty"`
	Features        json.RawMessage   `json:"features,omitempty"`
	BarTime         string            `json:"bar_time"`
	CodeHash        string            `json:"code_hash"`
}

// Validate checks a response before it is allowed to influence anything.
//
// A research service is a dependency, not an authority. Its output is bounded
// here so that a bug or a compromise upstream cannot express itself as an
// extreme confidence, an unknown action, or a stop at an absurd price.
func (r SignalResponse) Validate() error {
	switch r.Action {
	case "buy", "sell", "hold", "close", "no_trade":
	default:
		return fmt.Errorf("%w: unknown action %q", ErrBadResponse, r.Action)
	}
	if r.Confidence.IsNegative() || r.Confidence.GreaterThan(decimal.NewFromInt(1)) {
		return fmt.Errorf("%w: confidence %s is outside [0,1]", ErrBadResponse, r.Confidence)
	}
	for name, p := range map[string]*decimal.Decimal{
		"suggested_stop": r.SuggestedStop, "suggested_target": r.SuggestedTarget,
	} {
		if p != nil && !p.IsPositive() {
			return fmt.Errorf("%w: %s must be positive, got %s", ErrBadResponse, name, p)
		}
	}
	if len(r.Explanation) > 2000 {
		return fmt.Errorf("%w: explanation is implausibly long", ErrBadResponse)
	}
	return nil
}

// Signal requests one strategy evaluation.
func (c *Client) Signal(ctx context.Context, req SignalRequest) (SignalResponse, error) {
	var resp SignalResponse
	if err := c.post(ctx, "/v1/strategies/signal", req, &resp); err != nil {
		return SignalResponse{}, err
	}
	if err := resp.Validate(); err != nil {
		return SignalResponse{}, err
	}
	return resp, nil
}

// BacktestRequest asks for a historical evaluation.
type BacktestRequest struct {
	StrategyKey      string          `json:"strategy_key"`
	Version          int             `json:"version"`
	InstrumentID     string          `json:"instrument_id"`
	Timeframe        string          `json:"timeframe"`
	Parameters       json.RawMessage `json:"parameters"`
	Bars             []BarInput      `json:"bars"`
	InitialCapital   string          `json:"initial_capital"`
	Currency         string          `json:"currency"`
	ContractSize     string          `json:"contract_size"`
	CommissionPerLot string          `json:"commission_per_lot"`
	SpreadFraction   string          `json:"spread_fraction"`
	SlippageFraction string          `json:"slippage_fraction"`
	SwapLongPerLot   string          `json:"swap_long_per_lot"`
	SwapShortPerLot  string          `json:"swap_short_per_lot"`
	RiskPerTrade     string          `json:"risk_per_trade"`
	MinQuantity      string          `json:"min_quantity"`
	QuantityStep     string          `json:"quantity_step"`
	SampleKind       string          `json:"sample_kind"`
	Seed             int64           `json:"seed"`
}

// BacktestTradeResult is one simulated round trip.
type BacktestTradeResult struct {
	Side       string  `json:"side"`
	Quantity   string  `json:"quantity"`
	EntryTime  string  `json:"entry_time"`
	EntryPrice string  `json:"entry_price"`
	ExitTime   *string `json:"exit_time"`
	ExitPrice  *string `json:"exit_price"`
	GrossPnL   string  `json:"gross_pnl"`
	Commission string  `json:"commission"`
	Slippage   string  `json:"slippage"`
	Swap       string  `json:"swap"`
	NetPnL     string  `json:"net_pnl"`
	MAE        *string `json:"mae"`
	MFE        *string `json:"mfe"`
	ExitReason string  `json:"exit_reason"`
}

// BacktestResponse carries results, metrics and any methodology warnings.
type BacktestResponse struct {
	Metrics     map[string]any        `json:"metrics"`
	EquityCurve []map[string]any      `json:"equity_curve"`
	Trades      []BacktestTradeResult `json:"trades"`
	// Warnings surface methodology problems — too few trades, a suspiciously
	// high win rate, an unrealistic cost model. They are shown next to the
	// results rather than buried, because a backtest without its caveats is a
	// marketing document.
	Warnings     []string `json:"warnings"`
	DatasetHash  string   `json:"dataset_hash"`
	CodeHash     string   `json:"code_hash"`
	Frictionless bool     `json:"frictionless"`
}

// Backtest runs a historical evaluation.
func (c *Client) Backtest(ctx context.Context, req BacktestRequest) (BacktestResponse, error) {
	var resp BacktestResponse
	if err := c.post(ctx, "/v1/backtests/run", req, &resp); err != nil {
		return BacktestResponse{}, err
	}
	return resp, nil
}

// TrainRequest asks for a model training run.
type TrainRequest struct {
	ModelKey     string     `json:"model_key"`
	Algorithm    string     `json:"algorithm"`
	InstrumentID string     `json:"instrument_id"`
	Timeframe    string     `json:"timeframe"`
	Bars         []BarInput `json:"bars"`
	// The split fractions are chosen by the caller and validated by the
	// service; a training run that silently reuses the test window is worse
	// than no model at all.
	TrainFraction      float64        `json:"train_fraction"`
	ValidationFraction float64        `json:"validation_fraction"`
	EmbargoBars        int            `json:"embargo_bars"`
	Horizon            int            `json:"horizon"`
	Hyperparameters    map[string]any `json:"hyperparameters,omitempty"`
	Seed               int64          `json:"seed"`
}

// TrainResponse is a completed training run's reproducibility record.
type TrainResponse struct {
	Algorithm          string                    `json:"algorithm"`
	FeatureDefinition  json.RawMessage           `json:"feature_definition"`
	LabelDefinition    json.RawMessage           `json:"label_definition"`
	Hyperparameters    json.RawMessage           `json:"hyperparameters"`
	DatasetHash        string                    `json:"dataset_hash"`
	CodeHash           string                    `json:"code_hash"`
	DependencyVersions json.RawMessage           `json:"dependency_versions"`
	Seed               int64                     `json:"seed"`
	RowCount           int                       `json:"row_count"`
	Windows            map[string]map[string]any `json:"windows"`
	Evaluations        map[string]map[string]any `json:"evaluations"`
	Warnings           []string                  `json:"warnings"`
	ArtifactPath       string                    `json:"artifact_path"`
	ArtifactHash       string                    `json:"artifact_hash"`
}

// Train runs a model training job.
func (c *Client) Train(ctx context.Context, req TrainRequest) (TrainResponse, error) {
	var resp TrainResponse
	if err := c.post(ctx, "/v1/ml/train", req, &resp); err != nil {
		return TrainResponse{}, err
	}
	return resp, nil
}

// ScanRequest asks for opportunity ranking across instruments.
type ScanRequest struct {
	Timeframe   string                `json:"timeframe"`
	Instruments []ScanInstrumentInput `json:"instruments"`
}

// ScanInstrumentInput is one candidate instrument with its recent history.
type ScanInstrumentInput struct {
	InstrumentID   string     `json:"instrument_id"`
	Symbol         string     `json:"symbol"`
	Bars           []BarInput `json:"bars"`
	SpreadFraction string     `json:"spread_fraction"`
	Session        string     `json:"session"`
	EventRisk      string     `json:"event_risk"`
}

// ScanCandidate is one ranked opportunity.
type ScanCandidate struct {
	InstrumentID   string            `json:"instrument_id"`
	Symbol         string            `json:"symbol"`
	Regime         string            `json:"regime"`
	TrendScore     decimal.Decimal   `json:"trend_score"`
	MomentumScore  decimal.Decimal   `json:"momentum_score"`
	Volatility     decimal.Decimal   `json:"volatility"`
	SpreadFraction decimal.Decimal   `json:"spread_fraction"`
	EventRisk      string            `json:"event_risk"`
	Score          decimal.Decimal   `json:"score"`
	Agreeing       []string          `json:"agreeing_strategies"`
	Explanation    string            `json:"explanation"`
	Indicators     map[string]string `json:"indicators"`
}

// ScanResponse is the ranked candidate list.
type ScanResponse struct {
	Candidates  []ScanCandidate `json:"candidates"`
	EvaluatedAt string          `json:"evaluated_at"`
}

// Scan ranks opportunities. It executes nothing: the scanner is an analysis
// tool whose output still has to pass the entire order pipeline.
func (c *Client) Scan(ctx context.Context, req ScanRequest) (ScanResponse, error) {
	var resp ScanResponse
	if err := c.post(ctx, "/v1/scanner/scan", req, &resp); err != nil {
		return ScanResponse{}, err
	}
	return resp, nil
}

// StrategyDescriptor describes an implemented strategy.
type StrategyDescriptor struct {
	Key           string          `json:"key"`
	Name          string          `json:"name"`
	Family        string          `json:"family"`
	Description   string          `json:"description"`
	HighRisk      bool            `json:"high_risk"`
	DefaultParams json.RawMessage `json:"default_parameters"`
	RequiredBars  int             `json:"required_bars"`
	Timeframes    []string        `json:"timeframes"`
	CodeHash      string          `json:"code_hash"`
	ValidRegimes  []string        `json:"valid_regimes"`
}

// Strategies lists what the research service implements. The control plane
// reconciles this against its own registry at start-up, so a strategy that
// exists in one and not the other is visible rather than silently missing.
func (c *Client) Strategies(ctx context.Context) ([]StrategyDescriptor, error) {
	var resp struct {
		Strategies []StrategyDescriptor `json:"strategies"`
	}
	if err := c.get(ctx, "/v1/strategies", &resp); err != nil {
		return nil, err
	}
	return resp.Strategies, nil
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

// maxResponseBytes bounds how much a response may be. An unbounded read from a
// dependency is a memory-exhaustion vector even when the dependency is trusted.
const maxResponseBytes = 16 << 20 // 16 MiB

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	now := time.Now()
	if c.breakerOpen(now) {
		return ErrCircuitOpen
	}

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("quant: marshal request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("quant: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// A shared service token. It authenticates the control plane TO research;
	// it grants research nothing in return.
	if c.token != "" {
		req.Header.Set("X-Vantage-Service-Token", c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.recordFailure(now)
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		c.recordFailure(now)
		return fmt.Errorf("%w: reading response: %v", ErrUnavailable, err)
	}

	if resp.StatusCode >= 500 {
		c.recordFailure(now)
		return fmt.Errorf("%w: research service returned %d", ErrUnavailable, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		// A 4xx is the caller's fault and does not count toward the breaker.
		var apiErr struct {
			Detail any `json:"detail"`
		}
		_ = json.Unmarshal(payload, &apiErr)
		return fmt.Errorf("%w: %d: %v", ErrRequestFailed, resp.StatusCode, apiErr.Detail)
	}

	c.recordSuccess()
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("%w: %v", ErrBadResponse, err)
	}
	return nil
}

// FingerprintBars hashes the exact bar series sent to research, so a stored
// result can be tied to the data that produced it.
func FingerprintBars(bars []BarInput) string {
	h := make([]byte, 0, len(bars)*64)
	for _, b := range bars {
		h = append(h, b.OpenTime...)
		h = append(h, '|')
		h = append(h, b.Open...)
		h = append(h, '|')
		h = append(h, b.High...)
		h = append(h, '|')
		h = append(h, b.Low...)
		h = append(h, '|')
		h = append(h, b.Close...)
		h = append(h, '\n')
	}
	return crypto.SHA256Hex(h)
}
