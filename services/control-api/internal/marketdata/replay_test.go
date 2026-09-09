package marketdata

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
)

// The replay provider is the foundation the autonomous scenarios stand on, so
// its own guarantees have to hold absolutely: no look-ahead, deterministic
// stepping, and a series that fails loudly rather than quietly misbehaving.

var replayStart = time.Date(2026, 3, 3, 8, 0, 0, 0, time.UTC)

func spec(count int) SeriesSpec {
	return SeriesSpec{
		InstrumentID: "XAUUSD",
		Timeframe:    domain.Timeframe("1h"),
		Start:        replayStart,
		Interval:     time.Hour,
		StartPrice:   decimal.RequireFromString("2650"),
		Count:        count,
	}
}

func loadedReplay(t *testing.T, bars []domain.Bar) *ReplayProvider {
	t.Helper()
	r := NewReplayProvider()
	if err := r.SetSeries("XAUUSD", bars); err != nil {
		t.Fatalf("SetSeries: %v", err)
	}
	return r
}

// ---------------------------------------------------------------------------
// No look-ahead
// ---------------------------------------------------------------------------

func TestHistoryNeverIncludesABarTheCursorHasNotReached(t *testing.T) {
	// The single most important property. A strategy asking for history during
	// a replay must not be handed the future it is about to be tested
	// against -- that would make every scenario a look-ahead test that passes
	// for the wrong reason.
	bars := TrendingSeries(spec(60), decimal.RequireFromString("0.002"))
	r := loadedReplay(t, bars)
	inst := testInstrument()
	ctx := context.Background()

	for step := 0; step < 40; step++ {
		history, err := r.HistoricalBars(ctx, inst, domain.Timeframe("1h"),
			replayStart.Add(-time.Hour), replayStart.Add(1000*time.Hour))
		if err != nil {
			t.Fatalf("step %d: HistoricalBars: %v", step, err)
		}
		if len(history) != step+1 {
			t.Fatalf("step %d returned %d bars, want %d", step, len(history), step+1)
		}
		current, ok := r.CurrentBar("XAUUSD")
		if !ok {
			t.Fatalf("step %d: no current bar", step)
		}
		last := history[len(history)-1]
		if last.OpenTime.After(current.OpenTime) {
			t.Fatalf("step %d: history ends at %s, past the cursor at %s",
				step, last.OpenTime, current.OpenTime)
		}
		r.Step(1)
	}
}

func TestAQuoteReflectsOnlyTheCurrentBar(t *testing.T) {
	bars := TrendingSeries(spec(30), decimal.RequireFromString("0.003"))
	r := loadedReplay(t, bars)
	inst := testInstrument()

	for step := 0; step < 20; step++ {
		q, err := r.Quote(context.Background(), inst, replayStart)
		if err != nil {
			t.Fatalf("step %d: Quote: %v", step, err)
		}
		current, _ := r.CurrentBar("XAUUSD")
		// The mid is built from this bar's close, within a tick of rounding.
		diff := q.Mid().Sub(current.Close).Abs()
		if diff.GreaterThan(inst.Spec.TickSize.Mul(decimal.NewFromInt(2))) {
			t.Fatalf("step %d: quote mid %s does not match the current close %s",
				step, q.Mid(), current.Close)
		}
		r.Step(1)
	}
}

func TestTheQuoteCarriesTheBarsOwnTimeNotTheCallers(t *testing.T) {
	// This is what makes quote AGE controllable, and therefore what makes the
	// stale-feed refusal testable without waiting for it.
	r := loadedReplay(t, FlatSeries(spec(10)))
	inst := testInstrument()
	callerNow := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	q, err := r.Quote(context.Background(), inst, callerNow)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	current, _ := r.CurrentBar("XAUUSD")
	if !q.SourceTime.Equal(current.CloseTime.UTC()) {
		t.Errorf("source time = %s, want the bar's close time %s", q.SourceTime, current.CloseTime)
	}
	if !q.IngestedAt.Equal(callerNow) {
		t.Errorf("ingested at = %s, want the caller's now %s", q.IngestedAt, callerNow)
	}
	// And therefore the platform sees it as ancient, which is correct.
	health := domain.EvaluateQuoteHealth(q, nil, domain.DefaultDataQualityPolicy(), callerNow)
	if health.Healthy() {
		t.Error("a quote from 2026 read as healthy against a 2030 clock")
	}
}

// ---------------------------------------------------------------------------
// Stepping, pausing, resetting
// ---------------------------------------------------------------------------

func TestSteppingIsDeterministicAcrossRuns(t *testing.T) {
	run := func() []string {
		r := loadedReplay(t, TrendingSeries(spec(50), decimal.RequireFromString("0.002")))
		inst := testInstrument()
		var out []string
		for k := 0; k < 30; k++ {
			q, err := r.Quote(context.Background(), inst, replayStart)
			if err != nil {
				t.Fatalf("Quote: %v", err)
			}
			out = append(out, q.Bid.String()+"/"+q.Ask.String())
			r.Step(1)
		}
		return out
	}
	first, second := run(), run()
	for k := range first {
		if first[k] != second[k] {
			t.Fatalf("step %d differed between runs: %s vs %s", k, first[k], second[k])
		}
	}
}

func TestSteppingPastTheEndHoldsAtTheLastBar(t *testing.T) {
	// Wrapping would silently restart the series and look like a brand new
	// trend. Failing would make an over-stepping scenario error rather than
	// simply run out of market. Holding is the honest option: the feed goes
	// stale, which is a real condition the platform already handles.
	r := loadedReplay(t, FlatSeries(spec(5)))

	if remaining := r.Step(100); remaining != 0 {
		t.Errorf("remaining = %d after over-stepping, want 0", remaining)
	}
	played, total := r.Progress("XAUUSD")
	if played != 5 || total != 5 {
		t.Errorf("progress = %d/%d, want 5/5", played, total)
	}

	before, _ := r.CurrentBar("XAUUSD")
	r.Step(10)
	after, _ := r.CurrentBar("XAUUSD")
	if !before.OpenTime.Equal(after.OpenTime) {
		t.Error("the series moved after reaching its end")
	}
}

func TestRemainingCountsDownToZero(t *testing.T) {
	r := loadedReplay(t, FlatSeries(spec(4)))
	// Cursor starts on bar 0, so three steps remain.
	for expected := 3; expected > 0; expected-- {
		if got := r.Step(0); got != 1 {
			t.Fatalf("remaining = %d before the end, want 1 instrument with bars left", got)
		}
		r.Step(1)
	}
	if got := r.Step(0); got != 0 {
		t.Errorf("remaining = %d at the end, want 0", got)
	}
}

func TestPauseHoldsTheSeriesAndResumeReleasesIt(t *testing.T) {
	// A scenario needs to arm a fault or revoke authority without the market
	// moving underneath it.
	r := loadedReplay(t, TrendingSeries(spec(20), decimal.RequireFromString("0.002")))

	r.Step(3)
	held, _ := r.CurrentBar("XAUUSD")
	r.Pause()
	if !r.Paused() {
		t.Error("Paused() reported false after Pause()")
	}
	r.Step(5)
	stillHeld, _ := r.CurrentBar("XAUUSD")
	if !held.OpenTime.Equal(stillHeld.OpenTime) {
		t.Error("the series advanced while paused")
	}

	r.Resume()
	r.Step(1)
	moved, _ := r.CurrentBar("XAUUSD")
	if held.OpenTime.Equal(moved.OpenTime) {
		t.Error("the series did not advance after Resume()")
	}
}

func TestResetRewindsAndClearsScenarioOverrides(t *testing.T) {
	// A spread override or an outage leaking into the next scenario is the
	// kind of fixture bug that makes an unrelated test fail for a reason
	// nobody can find.
	r := loadedReplay(t, FlatSeries(spec(10)))
	inst := testInstrument()

	r.Step(5)
	r.SetSpreadFraction("XAUUSD", decimal.RequireFromString("0.02"))
	r.SetOutage(true)
	r.Pause()

	r.Reset()

	if r.Paused() {
		t.Error("Reset left the provider paused")
	}
	if played, _ := r.Progress("XAUUSD"); played != 1 {
		t.Errorf("Reset left the cursor at %d, want the first bar", played)
	}
	q, err := r.Quote(context.Background(), inst, replayStart)
	if err != nil {
		t.Fatalf("Reset left the outage in place: %v", err)
	}
	if q.SpreadFraction().GreaterThan(decimal.RequireFromString("0.001")) {
		t.Errorf("Reset left the widened spread in place: %s", q.SpreadFraction())
	}
}

// ---------------------------------------------------------------------------
// Scenario controls
// ---------------------------------------------------------------------------

func TestAWidenedSpreadLeavesThePriceUnchanged(t *testing.T) {
	// Scenario E: a strategy still sees the same opportunity, and only the
	// cost of taking it moves. If widening the spread also moved the mid, the
	// scenario would be testing two things at once.
	r := loadedReplay(t, FlatSeries(spec(10)))
	inst := testInstrument()

	// Quote at the bar's own close time, so both the received age and the
	// venue age are zero and the spread is the only thing the health check
	// can object to. A stale quote returns before the spread is examined --
	// correctly, since the spread of a price that is no longer true is not the
	// interesting fact about it.
	current, _ := r.CurrentBar("XAUUSD")
	fresh := current.CloseTime

	before, err := r.Quote(context.Background(), inst, fresh)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	r.SetSpreadFraction("XAUUSD", decimal.RequireFromString("0.02"))
	after, err := r.Quote(context.Background(), inst, fresh)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	if after.Mid().Sub(before.Mid()).Abs().GreaterThan(inst.Spec.TickSize) {
		t.Errorf("the mid moved from %s to %s when only the spread changed",
			before.Mid(), after.Mid())
	}
	if !after.SpreadFraction().GreaterThan(before.SpreadFraction().Mul(decimal.NewFromInt(10))) {
		t.Errorf("the spread only went from %s to %s",
			before.SpreadFraction(), after.SpreadFraction())
	}
	// And the platform must consider the widened book abnormal.
	health := domain.EvaluateQuoteHealth(after, nil, domain.DefaultDataQualityPolicy(), fresh)
	if !health.HasIssue(domain.IssueSpreadAbnormal) {
		t.Errorf("a 2%% spread was not flagged abnormal: %v", health.Issues)
	}
}

func TestAnOutageIsAnErrorNotAnEmptyQuote(t *testing.T) {
	// Scenario F. A provider that returns a zero-value quote would be recorded
	// as a price of zero; one that errors is marked unhealthy and stops
	// automation, which is the behaviour under test.
	r := loadedReplay(t, FlatSeries(spec(10)))
	inst := testInstrument()

	r.SetOutage(true)
	q, err := r.Quote(context.Background(), inst, replayStart)
	if err == nil {
		t.Fatalf("an outage returned a quote: %+v", q)
	}
	if !q.Bid.IsZero() || q.InstrumentID != "" {
		t.Errorf("the failed quote carried usable-looking data: %+v", q)
	}

	r.SetOutage(false)
	if _, err := r.Quote(context.Background(), inst, replayStart); err != nil {
		t.Errorf("clearing the outage did not restore quotes: %v", err)
	}
}

func TestAnInstrumentWithNoSeriesIsRefused(t *testing.T) {
	r := NewReplayProvider()
	inst := testInstrument()

	if _, err := r.Quote(context.Background(), inst, replayStart); err == nil {
		t.Error("a provider with no series returned a quote")
	}
	if _, err := r.HistoricalBars(context.Background(), inst,
		domain.Timeframe("1h"), replayStart, replayStart.Add(time.Hour)); err == nil {
		t.Error("a provider with no series returned history")
	}
}

func TestTheProviderNamesItselfDistinctlyFromTheMock(t *testing.T) {
	// A quote recorded during a replay has to be identifiable as one
	// afterwards. Research that cannot tell replayed data from generated data
	// will eventually treat one as the other.
	if NewReplayProvider().Name() == NewMockProvider(1, testClock()).Name() {
		t.Error("the replay and mock providers share a name")
	}
}

// ---------------------------------------------------------------------------
// Series validation
// ---------------------------------------------------------------------------

func TestSetSeriesRejectsAnEmptySeries(t *testing.T) {
	if err := NewReplayProvider().SetSeries("XAUUSD", nil); err == nil {
		t.Error("an empty series was accepted")
	}
}

func TestSetSeriesRejectsStructurallyInvalidBars(t *testing.T) {
	// A bad candle in a fixture produces a test that fails for the wrong
	// reason. Better to reject it where the fixture is written.
	base := FlatSeries(spec(5))

	cases := []struct {
		name   string
		mutate func([]domain.Bar)
	}{
		{"non-positive low", func(b []domain.Bar) { b[2].Low = decimal.Zero }},
		{"negative low", func(b []domain.Bar) { b[2].Low = decimal.NewFromInt(-1) }},
		{"high below low", func(b []domain.Bar) {
			b[2].High, b[2].Low = b[2].Low, b[2].High
		}},
		{"high below close", func(b []domain.Bar) {
			b[2].High = b[2].Close.Sub(decimal.NewFromInt(10))
		}},
		{"low above open", func(b []domain.Bar) {
			b[2].Low = b[2].Open.Add(decimal.NewFromInt(10))
		}},
		{"duplicate timestamps", func(b []domain.Bar) { b[2].OpenTime = b[1].OpenTime }},
	}

	for _, c := range cases {
		bars := make([]domain.Bar, len(base))
		copy(bars, base)
		c.mutate(bars)
		if err := NewReplayProvider().SetSeries("XAUUSD", bars); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

func TestSetSeriesSortsOutOfOrderBars(t *testing.T) {
	// Out of order is a fixture inconvenience rather than a lie, so it is
	// sorted. Duplicated times are a genuine ambiguity and are rejected above.
	base := FlatSeries(spec(5))
	shuffled := []domain.Bar{base[3], base[0], base[4], base[1], base[2]}

	r := NewReplayProvider()
	if err := r.SetSeries("XAUUSD", shuffled); err != nil {
		t.Fatalf("SetSeries: %v", err)
	}
	first, _ := r.CurrentBar("XAUUSD")
	if !first.OpenTime.Equal(base[0].OpenTime) {
		t.Errorf("the cursor starts at %s, want the earliest bar %s",
			first.OpenTime, base[0].OpenTime)
	}
}

func TestSetSeriesCopiesTheCallersSlice(t *testing.T) {
	// A scenario that reuses its slice for the next instrument must not
	// retroactively change the first one's series.
	bars := FlatSeries(spec(5))
	r := loadedReplay(t, bars)

	original, _ := r.CurrentBar("XAUUSD")
	bars[0].Close = decimal.NewFromInt(1)
	after, _ := r.CurrentBar("XAUUSD")

	if !original.Close.Equal(after.Close) {
		t.Error("mutating the caller's slice changed the loaded series")
	}
}

// ---------------------------------------------------------------------------
// Series generators
// ---------------------------------------------------------------------------

func TestEveryGeneratedSeriesIsAcceptedByItsOwnValidator(t *testing.T) {
	// The generators and the validator have to agree, or a scenario cannot be
	// built at all.
	s := spec(80)
	series := map[string][]domain.Bar{
		"trending up":   TrendingSeries(s, decimal.RequireFromString("0.002")),
		"trending down": TrendingSeries(s, decimal.RequireFromString("-0.002")),
		"ranging":       RangingSeries(s, decimal.RequireFromString("0.004")),
		"shock":         VolatilityShockSeries(s, 40, decimal.RequireFromString("0.02")),
		"drawdown":      DrawdownSeries(s, decimal.RequireFromString("0.004")),
		"flat":          FlatSeries(s),
	}
	for name, bars := range series {
		if len(bars) != 80 {
			t.Errorf("%s produced %d bars, want 80", name, len(bars))
		}
		if err := NewReplayProvider().SetSeries("XAUUSD", bars); err != nil {
			t.Errorf("%s was rejected by SetSeries: %v", name, err)
		}
	}
}

func TestATrendingSeriesActuallyTrends(t *testing.T) {
	// A scenario that asserts "a trend strategy finds this trend" is worthless
	// if the series does not trend. Asserted here so the fixture is not the
	// thing under suspicion later.
	up := TrendingSeries(spec(80), decimal.RequireFromString("0.002"))
	if !up[len(up)-1].Close.GreaterThan(up[0].Open.Mul(decimal.RequireFromString("1.05"))) {
		t.Errorf("an up-trend rose only from %s to %s", up[0].Open, up[len(up)-1].Close)
	}

	down := TrendingSeries(spec(80), decimal.RequireFromString("-0.002"))
	if !down[len(down)-1].Close.LessThan(down[0].Open.Mul(decimal.RequireFromString("0.95"))) {
		t.Errorf("a down-trend fell only from %s to %s", down[0].Open, down[len(down)-1].Close)
	}
}

func TestARangingSeriesHasNoNetDrift(t *testing.T) {
	bars := RangingSeries(spec(80), decimal.RequireFromString("0.004"))
	first, last := bars[0].Open, bars[len(bars)-1].Close
	drift := last.Sub(first).Div(first).Abs()
	if drift.GreaterThan(decimal.RequireFromString("0.01")) {
		t.Errorf("a ranging series drifted %s%%, which is a trend",
			drift.Mul(decimal.NewFromInt(100)).StringFixed(2))
	}
	// It has to actually oscillate, or it is a flat series by another name.
	high, low := bars[0].High, bars[0].Low
	for _, b := range bars {
		if b.High.GreaterThan(high) {
			high = b.High
		}
		if b.Low.LessThan(low) {
			low = b.Low
		}
	}
	if !high.Sub(low).Div(first).GreaterThan(decimal.RequireFromString("0.005")) {
		t.Error("the ranging series barely moves; it would not exercise a range strategy")
	}
}

func TestAVolatilityShockIsCalmBeforeAndViolentAfter(t *testing.T) {
	const shockAt = 40
	bars := VolatilityShockSeries(spec(80), shockAt, decimal.RequireFromString("0.02"))

	avgRange := func(from, to int) decimal.Decimal {
		total := decimal.Zero
		for _, b := range bars[from:to] {
			total = total.Add(b.High.Sub(b.Low))
		}
		return total.Div(decimal.NewFromInt(int64(to - from)))
	}

	calm := avgRange(5, shockAt-1)
	violent := avgRange(shockAt+1, 79)
	if !violent.GreaterThan(calm.Mul(decimal.NewFromInt(5))) {
		t.Errorf("the shock section (avg range %s) is not materially more volatile than "+
			"the calm section (%s); risk controls would have nothing to react to",
			violent, calm)
	}
}

func TestADrawdownSeriesFallsMonotonically(t *testing.T) {
	bars := DrawdownSeries(spec(50), decimal.RequireFromString("0.004"))
	for i := 1; i < len(bars); i++ {
		if !bars[i].Close.LessThan(bars[i-1].Close) {
			t.Fatalf("bar %d closed at %s, not below the previous %s",
				i, bars[i].Close, bars[i-1].Close)
		}
	}
}

func TestSeriesTimestampsAreUTCAndEvenlySpaced(t *testing.T) {
	bars := TrendingSeries(spec(24), decimal.RequireFromString("0.001"))
	for i, b := range bars {
		if b.OpenTime.Location() != time.UTC || b.CloseTime.Location() != time.UTC {
			t.Fatalf("bar %d is not UTC", i)
		}
		if !b.CloseTime.Equal(b.OpenTime.Add(time.Hour)) {
			t.Fatalf("bar %d close time %s is not one interval after its open %s",
				i, b.CloseTime, b.OpenTime)
		}
		if i > 0 && !b.OpenTime.Equal(bars[i-1].OpenTime.Add(time.Hour)) {
			t.Fatalf("bar %d is not one interval after bar %d", i, i-1)
		}
	}
}

func TestAZeroCountSeriesIsEmptyRatherThanPanicking(t *testing.T) {
	if got := TrendingSeries(spec(0), decimal.RequireFromString("0.002")); len(got) != 0 {
		t.Errorf("a zero-count spec produced %d bars", len(got))
	}
}
