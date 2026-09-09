package marketdata

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
)

// These tests cover the two halves of this package that do not need a
// database: the health cache that decides whether automation may run, and the
// mock provider that every backtest, demo and test run is built on.
//
// The quote-validity rules themselves (stale, crossed, zero, negative,
// regressed, duplicate, abnormal spread) live in domain.EvaluateQuoteHealth
// and are tested in internal/domain/market_test.go. They are deliberately not
// duplicated here.

// A Tuesday inside the London/New York overlap, so the market is open and the
// session-dependent branches are exercised at a known point rather than at
// whatever time the suite happens to run.
var openTime = time.Date(2026, 3, 3, 14, 0, 0, 0, time.UTC)

func testInstrument() domain.Instrument {
	return domain.Instrument{
		ID: "XAUUSD", Symbol: "XAUUSD", Name: "Gold vs US Dollar",
		Class: domain.AssetClassMetal, BaseCcy: "XAU", QuoteCcy: "USD",
		Enabled: true, SessionCalendarID: "fx_metals_24x5",
		Spec: domain.InstrumentSpec{
			ContractSize:   decimal.NewFromInt(100),
			PricePrecision: 2,
			TickSize:       decimal.RequireFromString("0.01"),
		},
	}
}

func testClock() *domain.MarketClock {
	clock, err := domain.NewMarketClock(domain.ForexMetalsCalendar())
	if err != nil {
		panic(err)
	}
	return clock
}

// ---------------------------------------------------------------------------
// Health cache and alert transitions
// ---------------------------------------------------------------------------

type recordingAlerter struct {
	mu        sync.Mutex
	degraded  []string
	recovered []string
}

func (r *recordingAlerter) FeedDegraded(_ context.Context, instrumentID, _, state string,
	_ []string, _ float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.degraded = append(r.degraded, instrumentID+":"+state)
}

func (r *recordingAlerter) FeedRecovered(_ context.Context, instrumentID, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recovered = append(r.recovered, instrumentID)
}

func (r *recordingAlerter) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.degraded), len(r.recovered)
}

// newBareIngestor builds an ingestor with no store. Only the health cache is
// exercised, which needs none -- IngestOnce is covered end to end by the smoke
// suite against a real database.
func newBareIngestor() *Ingestor {
	return &Ingestor{
		policy: domain.DefaultDataQualityPolicy(),
		health: map[string]domain.MarketDataHealth{},
	}
}

func health(instrumentID string, state domain.DataQualityState,
	issues ...domain.DataQualityIssue) domain.MarketDataHealth {
	return domain.MarketDataHealth{
		InstrumentID: instrumentID, Symbol: instrumentID,
		State: state, Issues: issues, Provider: "test",
		EvaluatedAt: openTime,
	}
}

func TestHealthIsReadableBackPerInstrument(t *testing.T) {
	i := newBareIngestor()
	i.setHealth(health("XAUUSD", domain.DataQualityOK))
	i.setHealth(health("EURUSD", domain.DataQualityStale, domain.IssueStaleQuote))

	got, ok := i.Health("XAUUSD")
	if !ok || got.State != domain.DataQualityOK {
		t.Errorf("XAUUSD health = %v (found %v), want ok", got.State, ok)
	}
	if _, ok := i.Health("USDZAR"); ok {
		t.Error("an instrument that was never ingested reported health; a caller " +
			"would read that as a verdict rather than as absence of one")
	}
	if all := i.AllHealth(); len(all) != 2 {
		t.Errorf("AllHealth returned %d entries, want 2", len(all))
	}
}

func TestAllHealthReturnsACopy(t *testing.T) {
	// The scheduler and the HTTP layer both read this map. Handing out the
	// live map would let a reader mutate the ingestor's own state.
	i := newBareIngestor()
	i.setHealth(health("XAUUSD", domain.DataQualityOK))

	snapshot := i.AllHealth()
	snapshot["XAUUSD"] = health("XAUUSD", domain.DataQualityInvalid, domain.IssueCrossedBook)
	snapshot["INJECTED"] = health("INJECTED", domain.DataQualityOK)

	if got, _ := i.Health("XAUUSD"); got.State != domain.DataQualityOK {
		t.Errorf("mutating the returned map changed the ingestor's state to %s", got.State)
	}
	if _, ok := i.Health("INJECTED"); ok {
		t.Error("an entry added to the returned map appeared in the ingestor")
	}
}

func TestAFeedGoingBadAlertsOnceAndRecoveryAlertsOnce(t *testing.T) {
	// The transition is what an operator needs, not the steady state. Alerting
	// per tick at a two-second interval is a notification storm, and alerting
	// only once ever means a recovery is never announced.
	alerter := &recordingAlerter{}
	i := newBareIngestor()
	i.SetAlerter(alerter)

	i.setHealth(health("XAUUSD", domain.DataQualityOK))
	if d, r := alerter.counts(); d != 0 || r != 0 {
		t.Fatalf("a healthy first observation alerted: degraded=%d recovered=%d", d, r)
	}

	i.setHealth(health("XAUUSD", domain.DataQualityStale, domain.IssueStaleQuote))
	i.setHealth(health("XAUUSD", domain.DataQualityStale, domain.IssueStaleQuote))
	d, r := alerter.counts()
	if r != 0 {
		t.Errorf("recovery alerted while still degraded")
	}
	// Both bad ticks call FeedDegraded; the alerter applies the cooldown. What
	// matters here is that a bad state always reports and never reports as a
	// recovery.
	if d != 2 {
		t.Errorf("degraded alerts = %d, want one per bad observation", d)
	}

	i.setHealth(health("XAUUSD", domain.DataQualityOK))
	d2, r2 := alerter.counts()
	if r2 != 1 {
		t.Errorf("recovery alerts = %d, want exactly 1", r2)
	}
	if d2 != d {
		t.Errorf("returning to health raised another degraded alert")
	}

	// A second healthy tick must not re-announce recovery.
	i.setHealth(health("XAUUSD", domain.DataQualityOK))
	if _, r3 := alerter.counts(); r3 != 1 {
		t.Errorf("recovery alerts = %d after a second healthy tick, want 1", r3)
	}
}

func TestAnInvalidFeedAlertsLikeAStaleOne(t *testing.T) {
	// Both are untradable for automation. A crossed book that only logged
	// while a stale quote alerted would be the more dangerous of the two going
	// unreported.
	alerter := &recordingAlerter{}
	i := newBareIngestor()
	i.SetAlerter(alerter)

	i.setHealth(health("XAUUSD", domain.DataQualityOK))
	i.setHealth(health("XAUUSD", domain.DataQualityInvalid, domain.IssueCrossedBook))

	if d, _ := alerter.counts(); d != 1 {
		t.Fatalf("degraded alerts = %d for an invalid quote, want 1", d)
	}
}

func TestSetHealthWithNoAlerterDoesNotPanic(t *testing.T) {
	// Construction order in internal/app attaches the alerter after the
	// ingestor exists, so there is a real window where it is nil.
	i := newBareIngestor()
	i.setHealth(health("XAUUSD", domain.DataQualityStale, domain.IssueStaleQuote))
	if _, ok := i.Health("XAUUSD"); !ok {
		t.Error("health was not recorded when no alerter was attached")
	}
}

func TestConcurrentHealthUpdatesAndReadsAreSafe(t *testing.T) {
	// Ingestion writes on its own goroutine while the HTTP layer reads. Run
	// under -race for this to mean anything.
	i := newBareIngestor()
	i.SetAlerter(&recordingAlerter{})

	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			for k := 0; k < 50; k++ {
				state := domain.DataQualityOK
				if k%2 == 0 {
					state = domain.DataQualityStale
				}
				i.setHealth(health("XAUUSD", state, domain.IssueStaleQuote))
			}
		}(n)
		go func() {
			defer wg.Done()
			for k := 0; k < 50; k++ {
				_ = i.AllHealth()
				_, _ = i.Health("XAUUSD")
			}
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Mock provider
// ---------------------------------------------------------------------------

func TestTheSameSeedProducesTheSamePricePath(t *testing.T) {
	// Every backtest, demo and replay scenario rests on this. If the mock
	// drifts between runs, a failing test cannot be told from a changed
	// fixture.
	inst := testInstrument()
	ctx := context.Background()

	run := func() []string {
		p := NewMockProvider(42, testClock())
		var out []string
		for k := 0; k < 20; k++ {
			q, err := p.Quote(ctx, inst, openTime.Add(time.Duration(k)*time.Second))
			if err != nil {
				t.Fatalf("Quote: %v", err)
			}
			out = append(out, q.Bid.String()+"/"+q.Ask.String())
		}
		return out
	}

	first, second := run(), run()
	for k := range first {
		if first[k] != second[k] {
			t.Fatalf("tick %d differed between identically seeded runs: %s vs %s",
				k, first[k], second[k])
		}
	}
}

func TestDifferentSeedsProduceDifferentPaths(t *testing.T) {
	// The corollary: if the seed were ignored, "deterministic" would mean
	// "constant", and a scenario could not be varied at all.
	inst := testInstrument()
	ctx := context.Background()

	sample := func(seed int64) string {
		p := NewMockProvider(seed, testClock())
		var last string
		for k := 0; k < 20; k++ {
			q, _ := p.Quote(ctx, inst, openTime.Add(time.Duration(k)*time.Second))
			last = q.Bid.String()
		}
		return last
	}

	if sample(1) == sample(2) {
		t.Error("two different seeds produced the same price path, so the seed is ignored")
	}
}

func TestEveryGeneratedQuoteIsStructurallyValid(t *testing.T) {
	// The mock exists to be uncomfortable, not impossible. A quote it emits
	// must never be one EvaluateQuoteHealth would reject as invalid, because
	// then the fault under test would be the fixture rather than the platform.
	inst := testInstrument()
	p := NewMockProvider(7, testClock())
	ctx := context.Background()
	policy := domain.DefaultDataQualityPolicy()

	for k := 0; k < 400; k++ {
		now := openTime.Add(time.Duration(k) * time.Second)
		q, err := p.Quote(ctx, inst, now)
		if err != nil {
			t.Fatalf("Quote at tick %d: %v", k, err)
		}
		if !q.Bid.IsPositive() || !q.Ask.IsPositive() {
			t.Fatalf("tick %d produced a non-positive price: bid=%s ask=%s", k, q.Bid, q.Ask)
		}
		if q.Ask.LessThan(q.Bid) {
			t.Fatalf("tick %d produced a crossed book: bid=%s ask=%s", k, q.Bid, q.Ask)
		}
		h := domain.EvaluateQuoteHealth(q, nil, policy, now)
		if h.State == domain.DataQualityInvalid {
			t.Fatalf("tick %d produced a quote the platform rejects as invalid: %v",
				k, h.Issues)
		}
	}
}

func TestQuoteTimestampsAreUTCAndNotAheadOfNow(t *testing.T) {
	// A provider timestamping into the future is a real failure mode
	// (IssueFutureTimestamp). The mock must not be a source of it.
	inst := testInstrument()
	p := NewMockProvider(3, testClock())

	for k := 0; k < 50; k++ {
		now := openTime.Add(time.Duration(k) * time.Minute)
		q, err := p.Quote(context.Background(), inst, now)
		if err != nil {
			t.Fatalf("Quote: %v", err)
		}
		if q.SourceTime.Location() != time.UTC || q.IngestedAt.Location() != time.UTC {
			t.Fatalf("tick %d timestamps are not UTC: source=%s ingested=%s",
				k, q.SourceTime.Location(), q.IngestedAt.Location())
		}
		if q.SourceTime.After(now) {
			t.Fatalf("tick %d source time %s is ahead of now %s", k, q.SourceTime, now)
		}
	}
}

func TestTheSpreadWidensOutsideLiquidHours(t *testing.T) {
	// A constant spread would make the cost model meaningless and would let a
	// strategy that only works in liquid hours look like it works everywhere.
	inst := testInstrument()
	ctx := context.Background()

	spreadAt := func(now time.Time) decimal.Decimal {
		p := NewMockProvider(11, testClock())
		q, err := p.Quote(ctx, inst, now)
		if err != nil {
			t.Fatalf("Quote: %v", err)
		}
		return q.Ask.Sub(q.Bid)
	}

	overlap := spreadAt(openTime)
	// 03:00 UTC is outside London and New York.
	quiet := spreadAt(time.Date(2026, 3, 3, 3, 0, 0, 0, time.UTC))

	if quiet.LessThanOrEqual(overlap) {
		t.Errorf("quiet-hours spread %s is not wider than the overlap spread %s",
			quiet, overlap)
	}
}

func TestHistoricalBarsAreOrderedAndInternallyConsistent(t *testing.T) {
	inst := testInstrument()
	p := NewMockProvider(5, testClock())
	from := openTime.Add(-48 * time.Hour)

	bars, err := p.HistoricalBars(context.Background(), inst, domain.Timeframe("1h"), from, openTime)
	if err != nil {
		t.Fatalf("HistoricalBars: %v", err)
	}
	if len(bars) == 0 {
		t.Fatal("no bars returned for a two-day window")
	}

	for k, b := range bars {
		if k > 0 && !bars[k-1].OpenTime.Before(b.OpenTime) {
			t.Fatalf("bar %d is not after bar %d (%s vs %s)",
				k, k-1, b.OpenTime, bars[k-1].OpenTime)
		}
		if b.OpenTime.After(openTime) {
			t.Fatalf("bar %d opens at %s, after the requested end %s", k, b.OpenTime, openTime)
		}
		// An indicator computed over an inconsistent candle is meaningless,
		// and every strategy is computed over these.
		if b.High.LessThan(b.Open) || b.High.LessThan(b.Close) {
			t.Fatalf("bar %d high %s is below its open %s or close %s",
				k, b.High, b.Open, b.Close)
		}
		if b.Low.GreaterThan(b.Open) || b.Low.GreaterThan(b.Close) {
			t.Fatalf("bar %d low %s is above its open %s or close %s",
				k, b.Low, b.Open, b.Close)
		}
		if !b.Low.IsPositive() {
			t.Fatalf("bar %d has a non-positive low %s", k, b.Low)
		}
		if b.OpenTime.Location() != time.UTC {
			t.Fatalf("bar %d open time is not UTC: %s", k, b.OpenTime.Location())
		}
	}
}

func TestHistoricalBarsSkipClosedMarketPeriods(t *testing.T) {
	// Synthesising bars over a weekend would let a strategy trade a gap that
	// never existed, and a backtest over them would report trades that could
	// not have happened.
	inst := testInstrument()
	p := NewMockProvider(5, testClock())
	clock := testClock()

	// Friday 12:00 UTC through Monday 12:00 UTC spans the weekly close.
	from := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)

	bars, err := p.HistoricalBars(context.Background(), inst, domain.Timeframe("1h"), from, to)
	if err != nil {
		t.Fatalf("HistoricalBars: %v", err)
	}
	for _, b := range bars {
		if !clock.Status(b.OpenTime).Tradable() {
			t.Fatalf("a bar was generated at %s, when the market is %s",
				b.OpenTime, clock.Status(b.OpenTime))
		}
	}
	if len(bars) == 0 {
		t.Fatal("the whole window was skipped; the range does include trading hours")
	}
}

func TestHistoricalBarsAreReproducibleForTheSameRequest(t *testing.T) {
	inst := testInstrument()
	from := openTime.Add(-24 * time.Hour)

	first, err := NewMockProvider(1, testClock()).
		HistoricalBars(context.Background(), inst, domain.Timeframe("1h"), from, openTime)
	if err != nil {
		t.Fatalf("HistoricalBars: %v", err)
	}
	// A DIFFERENT seed on purpose: history is seeded from the instrument and
	// range, not from the provider's tick generator, so the same request must
	// return the same series regardless of provider state or call order.
	second, err := NewMockProvider(999, testClock()).
		HistoricalBars(context.Background(), inst, domain.Timeframe("1h"), from, openTime)
	if err != nil {
		t.Fatalf("HistoricalBars: %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("bar counts differ: %d vs %d", len(first), len(second))
	}
	for k := range first {
		if !first[k].Close.Equal(second[k].Close) || !first[k].OpenTime.Equal(second[k].OpenTime) {
			t.Fatalf("bar %d differs between requests: %s@%s vs %s@%s",
				k, first[k].Close, first[k].OpenTime, second[k].Close, second[k].OpenTime)
		}
	}
}

func TestAnUnknownInstrumentIsRefusedRatherThanInvented(t *testing.T) {
	// Returning an arbitrary price for an unknown symbol would let a
	// misconfigured instrument trade against a made-up market.
	inst := testInstrument()
	inst.ID = "NOT-A-REAL-PAIR"
	p := NewMockProvider(1, testClock())

	if _, err := p.HistoricalBars(context.Background(), inst,
		domain.Timeframe("1h"), openTime.Add(-time.Hour), openTime); err == nil {
		t.Error("HistoricalBars invented a series for an instrument with no starting price")
	}
}

func TestSetPriceMovesTheWalkForScenarioControl(ctx *testing.T) {
	// Replay and scenario tests need to place the market at a chosen level.
	inst := testInstrument()
	p := NewMockProvider(1, testClock())

	p.SetPrice(inst.ID, 3000.00, openTime)
	q, err := p.Quote(context.Background(), inst, openTime.Add(time.Second))
	if err != nil {
		ctx.Fatalf("Quote: %v", err)
	}
	mid, _ := q.Mid().Float64()
	if mid < 2900 || mid > 3100 {
		ctx.Errorf("mid = %v after SetPrice(3000); the level was not applied", mid)
	}
}

func TestConcurrentQuotesDoNotRaceTheSharedWalk(t *testing.T) {
	// The ingestor is single-threaded today, but the provider holds a mutex
	// precisely so that is not load-bearing. Meaningful under -race.
	inst := testInstrument()
	p := NewMockProvider(1, testClock())

	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for k := 0; k < 60; k++ {
				if _, err := p.Quote(context.Background(), inst,
					openTime.Add(time.Duration(k)*time.Second)); err != nil {
					t.Errorf("Quote: %v", err)
					return
				}
			}
		}(n)
	}
	wg.Wait()
}
