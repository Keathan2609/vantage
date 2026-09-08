// Package marketdata ingests prices and judges whether they are fit to trade on.
//
// Two responsibilities live here and they are deliberately separate:
//
//	ingestion  getting prices in, from whatever provider supplies them
//	quality    deciding whether what arrived can be acted on
//
// The second is not a filter applied at the edge; it is a verdict recorded
// alongside the data, consulted by the order pipeline, and surfaced in the UI.
// A trading system that cannot say "I do not trust this price right now" will
// eventually act on a price it should not have.
package marketdata

import (
	"context"
	"fmt"
	"math"
	// math/rand is deliberate here. The development price generator is seeded
	// so the same run produces the same market, which is what makes strategy
	// and backtest tests reproducible. Nothing security-relevant reads it.
	// nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used
	"math/rand"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/metrics"
	"github.com/vantage/control-api/internal/store"
)

// Provider supplies market data. Implementations are swappable: the mock here,
// an MT5 bridge later, a licensed data vendor after that. Nothing downstream
// knows which one is in use.
type Provider interface {
	Name() string
	// Quote returns the current two-sided price.
	Quote(ctx context.Context, instrument domain.Instrument, now time.Time) (domain.Quote, error)
	// HistoricalBars returns completed candles in ascending time order. It is
	// used to seed research data and to warm indicators.
	HistoricalBars(ctx context.Context, instrument domain.Instrument, tf domain.Timeframe, from, to time.Time) ([]domain.Bar, error)
}

// Ingestor pulls from a provider, records the data, and evaluates its quality.
type Ingestor struct {
	store    *store.Store
	provider Provider
	clock    domain.Clock
	market   *domain.MarketClock
	policy   domain.DataQualityPolicy

	mu     sync.RWMutex
	health map[string]domain.MarketDataHealth

	// alerter is optional: the ingestor works without it, and a nil alerter
	// keeps the tests free of a database.
	alerter Alerter
}

// Alerter is the subset of internal/notify the ingestor needs. Declared here
// rather than imported so marketdata does not depend on notify, which depends
// on store — the dependency would point outwards.
type Alerter interface {
	FeedDegraded(ctx context.Context, instrumentID, symbol, state string, issues []string, ageSeconds float64)
	FeedRecovered(ctx context.Context, instrumentID, symbol string)
}

// SetAlerter attaches an alerter after construction.
func (i *Ingestor) SetAlerter(a Alerter) {
	i.mu.Lock()
	i.alerter = a
	i.mu.Unlock()
}

// NewIngestor builds an ingestor.
func NewIngestor(s *store.Store, p Provider, clock domain.Clock, market *domain.MarketClock) *Ingestor {
	return &Ingestor{
		store:    s,
		provider: p,
		clock:    clock,
		market:   market,
		policy:   domain.DefaultDataQualityPolicy(),
		health:   map[string]domain.MarketDataHealth{},
	}
}

// IngestOnce pulls one quote per enabled instrument.
//
// Failures are per-instrument: a provider that cannot price gold must not stop
// the platform pricing everything else, and the instrument that failed is
// marked unhealthy rather than left with a silently stale quote.
func (i *Ingestor) IngestOnce(ctx context.Context) error {
	instruments, err := i.store.Market.ListInstruments(ctx, true)
	if err != nil {
		return fmt.Errorf("marketdata: list instruments: %w", err)
	}
	now := i.clock.Now()
	log := logging.FromContext(ctx)

	// While the market is closed there is nothing legitimate to ingest.
	// Synthesising prices over a weekend would let a strategy trade a gap that
	// never existed.
	if !i.market.Status(now).Tradable() {
		for _, inst := range instruments {
			i.setHealth(domain.MarketDataHealth{
				InstrumentID: inst.ID, Symbol: inst.Symbol,
				State: domain.DataQualityNoData, Provider: i.provider.Name(),
				EvaluatedAt: now,
				Issues:      []domain.DataQualityIssue{},
			})
		}
		return nil
	}

	for _, inst := range instruments {
		quote, err := i.provider.Quote(ctx, inst, now)
		if err != nil {
			log.Warn("quote unavailable", "symbol", inst.Symbol, "error", err.Error())
			i.setHealth(domain.MarketDataHealth{
				InstrumentID: inst.ID, Symbol: inst.Symbol,
				State: domain.DataQualityNoData, Provider: i.provider.Name(),
				Issues: []domain.DataQualityIssue{domain.IssueProviderDown}, EvaluatedAt: now,
			})
			metrics.DataQuality.WithLabelValues(inst.Symbol).Set(0)
			continue
		}

		prev, _, perr := i.store.Market.LatestQuote(ctx, inst.ID)
		var prevPtr *domain.Quote
		if perr == nil {
			prevPtr = &prev
		}

		health := domain.EvaluateQuoteHealth(quote, prevPtr, i.policy, now)

		// An invalid quote is not stored. Persisting a crossed book would put
		// a value into the system that every downstream consumer then has to
		// defend against.
		if health.State == domain.DataQualityInvalid {
			log.Warn("rejecting invalid quote",
				"symbol", inst.Symbol, "issues", fmt.Sprint(health.Issues),
				"bid", quote.Bid.String(), "ask", quote.Ask.String())
			for _, issue := range health.Issues {
				metrics.DataQualityIssues.WithLabelValues(inst.Symbol, string(issue)).Inc()
			}
			i.setHealth(health)
			_ = i.store.Market.RecordHealth(ctx, health)
			metrics.DataQuality.WithLabelValues(inst.Symbol).Set(0)
			continue
		}

		if err := i.store.Market.RecordQuote(ctx, quote); err != nil {
			log.Error("failed to record quote", "symbol", inst.Symbol, "error", err.Error())
			continue
		}

		i.setHealth(health)
		metrics.QuoteAge.WithLabelValues(inst.Symbol).Set(health.QuoteAge.Seconds())
		metrics.ProviderLatency.WithLabelValues(quote.Provider).
			Observe(quote.IngestedAt.Sub(quote.SourceTime).Seconds())
		if health.Healthy() {
			metrics.DataQuality.WithLabelValues(inst.Symbol).Set(1)
		} else {
			metrics.DataQuality.WithLabelValues(inst.Symbol).Set(0)
			for _, issue := range health.Issues {
				metrics.DataQualityIssues.WithLabelValues(inst.Symbol, string(issue)).Inc()
			}
		}
	}
	return nil
}

// Run ingests on an interval until the context is cancelled.
func (i *Ingestor) Run(ctx context.Context, interval time.Duration, onTick func(context.Context)) {
	log := logging.FromContext(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := i.IngestOnce(ctx); err != nil {
		log.Error("initial market data ingest failed", "error", err.Error())
	}
	for {
		select {
		case <-ctx.Done():
			log.Info("market data ingestion stopped")
			return
		case <-ticker.C:
			if err := i.IngestOnce(ctx); err != nil {
				log.Error("market data ingest failed", "error", err.Error())
				continue
			}
			if onTick != nil {
				onTick(ctx)
			}
		}
	}
}

// Health returns the cached verdict for an instrument.
func (i *Ingestor) Health(instrumentID string) (domain.MarketDataHealth, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	h, ok := i.health[instrumentID]
	return h, ok
}

// AllHealth returns every cached verdict.
func (i *Ingestor) AllHealth() map[string]domain.MarketDataHealth {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make(map[string]domain.MarketDataHealth, len(i.health))
	for k, v := range i.health {
		out[k] = v
	}
	return out
}

func (i *Ingestor) setHealth(h domain.MarketDataHealth) {
	i.mu.Lock()
	previous, existed := i.health[h.InstrumentID]
	i.health[h.InstrumentID] = h
	alerter := i.alerter
	i.mu.Unlock()

	if alerter == nil {
		return
	}
	// Alert on the STATE, not on every tick. The alerter applies its own
	// cooldown, but transitions are what an operator wants to see: a feed
	// going bad, and a feed coming back.
	nowBad := !h.State.TradableForAutomation()
	wasBad := existed && !previous.State.TradableForAutomation()

	ctx := context.Background()
	switch {
	case nowBad:
		issues := make([]string, 0, len(h.Issues))
		for _, issue := range h.Issues {
			issues = append(issues, string(issue))
		}
		alerter.FeedDegraded(ctx, h.InstrumentID, h.Symbol, string(h.State), issues, h.QuoteAge.Seconds())
	case wasBad:
		alerter.FeedRecovered(ctx, h.InstrumentID, h.Symbol)
	}
}

// ---------------------------------------------------------------------------
// Mock provider
// ---------------------------------------------------------------------------

// MockProvider generates a plausible price path for development.
//
// The path is NOT a claim about how gold behaves. It is a bounded random walk
// with a session-dependent spread and volatility, built so that indicators,
// risk checks and the execution simulator all receive input with the right
// SHAPE — a two-sided book that widens outside liquid hours, prices that gap
// occasionally, and no impossible values.
//
// Seeded with the same value it produces the same path, so tests and demos are
// reproducible.
type MockProvider struct {
	mu    sync.Mutex
	rng   *rand.Rand
	state map[string]*walkState
	// BaseSpreadFraction is the spread during liquid hours.
	BaseSpreadFraction decimal.Decimal
	market             *domain.MarketClock
}

type walkState struct {
	price      float64
	lastUpdate time.Time
}

// StartingPrices seed the walk. XAUUSD near recent levels, majors near parity
// conventions — approximate values chosen so that lot sizes, margin and the
// R500 account example all exercise realistic arithmetic.
var StartingPrices = map[string]float64{
	"XAUUSD":   2650.00,
	"XAUUSD.m": 2650.00,
	"EURUSD":   1.0850,
	"GBPUSD":   1.2700,
	"USDZAR":   18.2500,
	"XAGUSD":   31.500,
}

// NewMockProvider builds a deterministic price generator.
func NewMockProvider(seed int64, market *domain.MarketClock) *MockProvider {
	return &MockProvider{
		rng:                rand.New(rand.NewSource(seed)),
		state:              map[string]*walkState{},
		BaseSpreadFraction: decimal.RequireFromString("0.00012"),
		market:             market,
	}
}

// Name identifies the provider in stored data.
func (m *MockProvider) Name() string { return "mock" }

// Quote returns a generated two-sided price.
func (m *MockProvider) Quote(ctx context.Context, inst domain.Instrument, now time.Time) (domain.Quote, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st, ok := m.state[inst.ID]
	if !ok {
		start, known := StartingPrices[inst.ID]
		if !known {
			return domain.Quote{}, fmt.Errorf("marketdata: mock provider has no starting price for %s", inst.ID)
		}
		st = &walkState{price: start, lastUpdate: now}
		m.state[inst.ID] = st
	}

	elapsed := now.Sub(st.lastUpdate).Seconds()
	if elapsed <= 0 {
		elapsed = 1
	}
	if elapsed > 3600 {
		elapsed = 3600 // a long gap should not produce an absurd jump
	}

	// Volatility scales with the session: the London/New York overlap moves
	// more than the Asian afternoon, which is why session filters exist as a
	// strategy family at all.
	vol := m.sessionVolatility(now)
	drift := 0.0
	shock := m.rng.NormFloat64() * vol * math.Sqrt(elapsed/60.0)
	st.price = st.price * (1 + drift + shock)

	// Keep the walk within a sane band so a long-running dev instance does not
	// drift to an impossible price.
	if base, ok := StartingPrices[inst.ID]; ok {
		lo, hi := base*0.75, base*1.35
		if st.price < lo {
			st.price = lo
		}
		if st.price > hi {
			st.price = hi
		}
	}
	st.lastUpdate = now

	mid := decimal.NewFromFloat(st.price)
	spreadFraction := m.sessionSpread(now)
	halfSpread := mid.Mul(spreadFraction).Div(decimal.NewFromInt(2))

	bid := inst.Spec.RoundPrice(mid.Sub(halfSpread))
	ask := inst.Spec.RoundPrice(mid.Add(halfSpread))
	// Rounding must never produce a crossed or zero-width book.
	if ask.LessThanOrEqual(bid) {
		ask = bid.Add(inst.Spec.TickSize)
	}

	return domain.Quote{
		InstrumentID: inst.ID,
		Symbol:       inst.Symbol,
		Bid:          bid,
		Ask:          ask,
		// A real feed's source timestamp precedes its arrival. Modelling that
		// gap means the freshness logic is exercised rather than trivially
		// satisfied.
		SourceTime: now.Add(-120 * time.Millisecond),
		IngestedAt: now,
		Provider:   m.Name(),
	}, nil
}

// sessionVolatility returns per-minute volatility for the active session.
func (m *MockProvider) sessionVolatility(now time.Time) float64 {
	if m.market == nil {
		return 0.0004
	}
	sessions := m.market.ActiveSessions(now)
	for _, s := range sessions {
		if s == domain.SessionOverlap {
			return 0.0009
		}
	}
	for _, s := range sessions {
		switch s {
		case domain.SessionLondon, domain.SessionNewYork:
			return 0.0006
		}
	}
	return 0.0003
}

// sessionSpread widens the book outside liquid hours, which is exactly when a
// naive strategy would otherwise think it is getting a good price.
func (m *MockProvider) sessionSpread(now time.Time) decimal.Decimal {
	base := m.BaseSpreadFraction
	if m.market == nil {
		return base
	}
	sessions := m.market.ActiveSessions(now)
	for _, s := range sessions {
		if s == domain.SessionOverlap {
			return base
		}
	}
	for _, s := range sessions {
		switch s {
		case domain.SessionLondon, domain.SessionNewYork:
			return base.Mul(decimal.RequireFromString("1.3"))
		case domain.SessionNoActive:
			return base.Mul(decimal.RequireFromString("4.0"))
		}
	}
	return base.Mul(decimal.RequireFromString("2.2"))
}

// HistoricalBars generates a deterministic candle series.
//
// Bars are only produced for periods the market was actually open, so a
// backtest cannot trade a weekend, and the OHLC relationships are constructed
// to be internally coherent — which the database also enforces.
func (m *MockProvider) HistoricalBars(ctx context.Context, inst domain.Instrument, tf domain.Timeframe, from, to time.Time) ([]domain.Bar, error) {
	dur, err := tf.Duration()
	if err != nil {
		return nil, err
	}
	start, known := StartingPrices[inst.ID]
	if !known {
		return nil, fmt.Errorf("marketdata: mock provider has no starting price for %s", inst.ID)
	}

	// A dedicated generator seeded from the instrument and range, so the same
	// request always yields the same history regardless of call order.
	seed := int64(len(inst.ID)) * 7919
	for _, r := range inst.ID {
		seed += int64(r)
	}
	seed += from.Unix() ^ to.Unix()
	rng := rand.New(rand.NewSource(seed))

	price := start
	// Walk backwards from the present level so the series ends near the live
	// price rather than somewhere unrelated to it.
	var bars []domain.Bar
	for t := from.UTC().Truncate(dur); t.Before(to); t = t.Add(dur) {
		if m.market != nil && !m.market.Status(t).Tradable() {
			continue
		}
		vol := 0.0025
		if m.market != nil {
			for _, s := range m.market.ActiveSessions(t) {
				if s == domain.SessionOverlap {
					vol = 0.0045
				}
			}
		}

		open := price
		steps := 12
		high, low := open, open
		for i := 0; i < steps; i++ {
			price *= 1 + rng.NormFloat64()*vol/math.Sqrt(float64(steps))
			if price > high {
				high = price
			}
			if price < low {
				low = price
			}
		}
		closePx := price

		if base, ok := StartingPrices[inst.ID]; ok {
			lo, hi := base*0.75, base*1.35
			if price < lo {
				price = lo
			}
			if price > hi {
				price = hi
			}
		}

		rp := func(f float64) decimal.Decimal {
			return inst.Spec.RoundPrice(decimal.NewFromFloat(f))
		}
		o, h, l, c := rp(open), rp(high), rp(low), rp(closePx)
		// Guarantee OHLC coherence after rounding.
		if h.LessThan(o) {
			h = o
		}
		if h.LessThan(c) {
			h = c
		}
		if l.GreaterThan(o) {
			l = o
		}
		if l.GreaterThan(c) {
			l = c
		}

		bars = append(bars, domain.Bar{
			InstrumentID: inst.ID,
			Timeframe:    tf,
			OpenTime:     t,
			CloseTime:    t.Add(dur),
			Open:         o,
			High:         h,
			Low:          l,
			Close:        c,
			Volume:       decimal.NewFromInt(int64(500 + rng.Intn(4500))),
			Complete:     true,
			Provider:     m.Name(),
		})
	}
	return bars, nil
}

// SetPrice forces an instrument's price. Tests use it to drive a specific
// scenario — a gap, a stop being touched — instead of waiting for the walk.
func (m *MockProvider) SetPrice(instrumentID string, price float64, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state[instrumentID] = &walkState{price: price, lastUpdate: now}
}

var _ Provider = (*MockProvider)(nil)
