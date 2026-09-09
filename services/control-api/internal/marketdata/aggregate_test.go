package marketdata

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
)

// A bar is permanent in a way a quote is not: a strategy reads it as fact, an
// indicator computes over it, and a backtest of the same period will use it.
// So the aggregator's invariants matter more than its throughput.

var aggStart = time.Date(2026, 3, 3, 10, 0, 0, 0, time.UTC)

func quoteAt(t time.Time, mid string) domain.Quote {
	m := decimal.RequireFromString(mid)
	half := decimal.RequireFromString("0.10")
	return domain.Quote{
		InstrumentID: "XAUUSD", Symbol: "XAUUSD",
		Bid: m.Sub(half), Ask: m.Add(half),
		SourceTime: t, IngestedAt: t, Provider: "test",
	}
}

const tf1h = domain.Timeframe("1h")

func TestTheFirstQuoteOpensABar(t *testing.T) {
	a := NewAggregator("test", tf1h)

	bars, err := a.Observe(quoteAt(aggStart, "2650"), tf1h)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(bars) != 1 {
		t.Fatalf("bars = %d, want 1", len(bars))
	}
	b := bars[0]
	if !b.Open.Equal(decimal.RequireFromString("2650")) {
		t.Errorf("open = %s, want 2650", b.Open)
	}
	if !b.OpenTime.Equal(aggStart) {
		t.Errorf("open time = %s, want %s", b.OpenTime, aggStart)
	}
	if !b.CloseTime.Equal(aggStart.Add(time.Hour)) {
		t.Errorf("close time = %s, want one hour later", b.CloseTime)
	}
	if b.Complete {
		t.Error("the first quote produced a COMPLETE bar; it is still forming")
	}
}

func TestAFormingBarIsNeverMarkedComplete(t *testing.T) {
	// Strategies read only complete bars. A bar marked complete while it is
	// still forming would be acted on, and then change.
	a := NewAggregator("test", tf1h)
	for k := 0; k < 30; k++ {
		bars, err := a.Observe(quoteAt(aggStart.Add(time.Duration(k)*time.Minute),
			decimal.NewFromInt(2650+int64(k)).String()), tf1h)
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
		for _, b := range bars {
			if b.Complete {
				t.Fatalf("tick %d produced a complete bar inside the interval", k)
			}
		}
	}
}

func TestCrossingAnIntervalClosesTheBarAndOpensTheNext(t *testing.T) {
	a := NewAggregator("test", tf1h)

	if _, err := a.Observe(quoteAt(aggStart, "2650"), tf1h); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if _, err := a.Observe(quoteAt(aggStart.Add(30*time.Minute), "2660"), tf1h); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	bars, err := a.Observe(quoteAt(aggStart.Add(time.Hour+time.Minute), "2670"), tf1h)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("bars = %d, want the closed bar and the new one", len(bars))
	}
	closed, forming := bars[0], bars[1]

	if !closed.Complete {
		t.Error("the bar that rolled over was not marked complete")
	}
	if !closed.Open.Equal(decimal.RequireFromString("2650")) ||
		!closed.Close.Equal(decimal.RequireFromString("2660")) {
		t.Errorf("closed bar o=%s c=%s, want 2650/2660", closed.Open, closed.Close)
	}
	if forming.Complete {
		t.Error("the new bar is already complete")
	}
	if !forming.OpenTime.Equal(aggStart.Add(time.Hour)) {
		t.Errorf("new bar opens at %s, want the interval boundary %s",
			forming.OpenTime, aggStart.Add(time.Hour))
	}
	// The new bar opens at the tick that crossed, not at the old close. A
	// gap between bars is real information and must not be smoothed away.
	if !forming.Open.Equal(decimal.RequireFromString("2670")) {
		t.Errorf("new bar opens at %s, want the crossing tick's price 2670", forming.Open)
	}
}

func TestHighAndLowTrackTheExtremesOfTheInterval(t *testing.T) {
	a := NewAggregator("test", tf1h)
	for _, mid := range []string{"2650", "2680", "2630", "2665"} {
		if _, err := a.Observe(quoteAt(aggStart.Add(time.Minute), mid), tf1h); err != nil {
			t.Fatalf("Observe: %v", err)
		}
	}
	// Roll over to inspect the finished bar.
	bars, err := a.Observe(quoteAt(aggStart.Add(time.Hour+time.Minute), "2700"), tf1h)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	closed := bars[0]

	if !closed.High.Equal(decimal.RequireFromString("2680")) {
		t.Errorf("high = %s, want 2680", closed.High)
	}
	if !closed.Low.Equal(decimal.RequireFromString("2630")) {
		t.Errorf("low = %s, want 2630", closed.Low)
	}
	if !closed.Close.Equal(decimal.RequireFromString("2665")) {
		t.Errorf("close = %s, want the last price in the interval", closed.Close)
	}
}

func TestEveryProducedBarIsInternallyConsistent(t *testing.T) {
	// The property the replay provider's validator enforces, asserted at the
	// source. An inconsistent bar makes every indicator over it meaningless.
	a := NewAggregator("test", tf1h)
	prices := []string{"2650", "2661", "2644", "2670", "2639", "2655", "2690", "2620"}

	for k := 0; k < 200; k++ {
		q := quoteAt(aggStart.Add(time.Duration(k)*time.Minute), prices[k%len(prices)])
		bars, err := a.Observe(q, tf1h)
		if err != nil {
			t.Fatalf("tick %d: %v", k, err)
		}
		for _, b := range bars {
			if b.High.LessThan(b.Open) || b.High.LessThan(b.Close) {
				t.Fatalf("tick %d: high %s below open %s or close %s", k, b.High, b.Open, b.Close)
			}
			if b.Low.GreaterThan(b.Open) || b.Low.GreaterThan(b.Close) {
				t.Fatalf("tick %d: low %s above open %s or close %s", k, b.Low, b.Open, b.Close)
			}
			if !b.Low.IsPositive() {
				t.Fatalf("tick %d: non-positive low %s", k, b.Low)
			}
			if !b.CloseTime.After(b.OpenTime) {
				t.Fatalf("tick %d: close time is not after open time", k)
			}
		}
	}
}

func TestADuplicateTickChangesNothing(t *testing.T) {
	// Quotes arrive every two seconds and often repeat. Writing on every
	// identical tick would mean 1800 writes per hour per timeframe for a
	// market that has not moved.
	a := NewAggregator("test", tf1h)
	if _, err := a.Observe(quoteAt(aggStart, "2650"), tf1h); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	bars, err := a.Observe(quoteAt(aggStart.Add(time.Second), "2650"), tf1h)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(bars) != 0 {
		t.Errorf("a duplicate tick produced %d bar write(s), want none", len(bars))
	}
}

func TestAnOutOfOrderTickIsDroppedNotFoldedBackIn(t *testing.T) {
	// Rewriting a bar that has already been marked complete would change
	// history a strategy may already have acted on. A late tick is dropped.
	a := NewAggregator("test", tf1h)

	if _, err := a.Observe(quoteAt(aggStart, "2650"), tf1h); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	// Roll into the next interval.
	if _, err := a.Observe(quoteAt(aggStart.Add(time.Hour+time.Minute), "2700"), tf1h); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	// Now a tick from the PREVIOUS interval arrives late, at an extreme price.
	bars, err := a.Observe(quoteAt(aggStart.Add(10*time.Minute), "9999"), tf1h)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(bars) != 0 {
		t.Fatalf("a late tick produced %d bar write(s); it would rewrite a closed bar",
			len(bars))
	}
}

func TestAnInvalidQuoteIsRefusedRatherThanAggregated(t *testing.T) {
	// The ingestor already rejects these before storing. This is the second
	// line, because a bar outlives the quote it came from.
	a := NewAggregator("test", tf1h)

	cases := map[string]domain.Quote{
		"zero bid": func() domain.Quote {
			q := quoteAt(aggStart, "2650")
			q.Bid = decimal.Zero
			return q
		}(),
		"negative ask": func() domain.Quote {
			q := quoteAt(aggStart, "2650")
			q.Ask = decimal.NewFromInt(-1)
			return q
		}(),
		"crossed book": func() domain.Quote {
			q := quoteAt(aggStart, "2650")
			q.Bid, q.Ask = q.Ask, q.Bid
			return q
		}(),
	}
	for name, q := range cases {
		if _, err := a.Observe(q, tf1h); err == nil {
			t.Errorf("%s was aggregated into a bar", name)
		}
	}
}

func TestTheMidIsUsedRatherThanTheBidOrAsk(t *testing.T) {
	// A bar built from bids and one built from asks differ by the spread, and
	// every indicator over them inherits that bias -- worst exactly when the
	// spread widens, which is when the market is least kind.
	a := NewAggregator("test", tf1h)
	q := domain.Quote{
		InstrumentID: "XAUUSD", Symbol: "XAUUSD",
		Bid: decimal.RequireFromString("2600"), Ask: decimal.RequireFromString("2700"),
		SourceTime: aggStart, IngestedAt: aggStart, Provider: "test",
	}

	bars, err := a.Observe(q, tf1h)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !bars[0].Open.Equal(decimal.RequireFromString("2650")) {
		t.Errorf("open = %s, want the mid 2650 (not the bid 2600 or the ask 2700)",
			bars[0].Open)
	}
}

func TestBarsAreBucketedOnTheVenueTimestampNotOurs(t *testing.T) {
	// A quote's SourceTime is when the price was true. Bucketing on
	// IngestedAt would put a quote received just after an interval boundary
	// into the wrong bar, and during a replay would put every bar in the
	// present.
	a := NewAggregator("test", tf1h)
	q := quoteAt(aggStart.Add(20*time.Minute), "2650")
	q.IngestedAt = aggStart.Add(3 * time.Hour)

	bars, err := a.Observe(q, tf1h)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !bars[0].OpenTime.Equal(aggStart) {
		t.Errorf("bar opened at %s, want the source-time bucket %s", bars[0].OpenTime, aggStart)
	}
}

func TestEachTimeframeIsAggregatedIndependently(t *testing.T) {
	// A 15m bar closing must not close the 1h bar it sits inside.
	a := NewAggregator("test", domain.Timeframe("15m"), tf1h)

	for k := 0; k <= 20; k++ {
		at := aggStart.Add(time.Duration(k) * time.Minute)
		mid := decimal.NewFromInt(2650 + int64(k)).String()
		if _, err := a.Observe(quoteAt(at, mid), domain.Timeframe("15m")); err != nil {
			t.Fatalf("15m tick %d: %v", k, err)
		}
		bars, err := a.Observe(quoteAt(at, mid), tf1h)
		if err != nil {
			t.Fatalf("1h tick %d: %v", k, err)
		}
		for _, b := range bars {
			if b.Complete {
				t.Fatalf("the 1h bar completed at minute %d, inside its own interval", k)
			}
		}
	}
}

func TestTimeframesAreOrderedSmallestFirst(t *testing.T) {
	// Not cosmetic: a smaller bar closes more often, so building it first
	// means a strategy on the fast timeframe sees its bar as early as
	// possible rather than behind a 4h write.
	a := NewAggregator("test", domain.Timeframe("4h"), domain.Timeframe("15m"), tf1h)
	got := a.Timeframes()
	want := []string{"15m", "1h", "4h"}
	for i, tf := range got {
		if string(tf) != want[i] {
			t.Fatalf("timeframes = %v, want smallest first %v", got, want)
		}
	}
}

func TestTimeframesReturnsACopy(t *testing.T) {
	a := NewAggregator("test", tf1h)
	got := a.Timeframes()
	got[0] = domain.Timeframe("tampered")
	if a.Timeframes()[0] != tf1h {
		t.Error("mutating the returned slice changed the aggregator's configuration")
	}
}

func TestAnUnknownTimeframeIsAnErrorNotASilentSkip(t *testing.T) {
	a := NewAggregator("test", tf1h)
	if _, err := a.Observe(quoteAt(aggStart, "2650"), domain.Timeframe("7q")); err == nil {
		t.Error("an unparseable timeframe was accepted")
	}
}

func TestADefaultAggregatorBuildsTheTimeframesStrategiesUse(t *testing.T) {
	// Constructed with none, so a caller that forgets does not silently get an
	// aggregator that builds nothing -- which would reproduce the frozen-bar
	// failure this type exists to fix.
	got := NewAggregator("test").Timeframes()
	if len(got) == 0 {
		t.Fatal("an aggregator built with no timeframes builds nothing")
	}
	seen := map[string]bool{}
	for _, tf := range got {
		seen[string(tf)] = true
	}
	for _, need := range []string{"15m", "1h"} {
		if !seen[need] {
			t.Errorf("the default set is missing %s, which strategy versions declare", need)
		}
	}
}

func TestSeedContinuesAnIncompleteBarAcrossRestart(t *testing.T) {
	// Without this, every restart mid-interval opens a new bar at the restart
	// moment and leaves a short, misleading one in the series.
	a := NewAggregator("test", tf1h)
	stored := domain.Bar{
		InstrumentID: "XAUUSD", Timeframe: tf1h,
		OpenTime: aggStart, CloseTime: aggStart.Add(time.Hour),
		Open:  decimal.RequireFromString("2600"),
		High:  decimal.RequireFromString("2680"),
		Low:   decimal.RequireFromString("2590"),
		Close: decimal.RequireFromString("2650"),
		// Incomplete: this is the bar that was forming when the process died.
		Complete: false,
	}
	a.Seed("XAUUSD", tf1h, stored)

	bars, err := a.Observe(quoteAt(aggStart.Add(40*time.Minute), "2670"), tf1h)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(bars) != 1 {
		t.Fatalf("bars = %d, want 1 (continuing, not rolling over)", len(bars))
	}
	b := bars[0]
	if !b.Open.Equal(decimal.RequireFromString("2600")) {
		t.Errorf("open = %s; the restart opened a new bar instead of continuing", b.Open)
	}
	if !b.High.Equal(decimal.RequireFromString("2680")) {
		t.Errorf("high = %s, want the pre-restart extreme 2680 preserved", b.High)
	}
	if !b.Close.Equal(decimal.RequireFromString("2670")) {
		t.Errorf("close = %s, want the new tick 2670", b.Close)
	}
}

func TestSeedIgnoresACompleteBar(t *testing.T) {
	// A complete bar needs no continuation. Reopening it would let a new tick
	// modify a bar a strategy has already acted on.
	a := NewAggregator("test", tf1h)
	stored := domain.Bar{
		InstrumentID: "XAUUSD", Timeframe: tf1h,
		OpenTime: aggStart, CloseTime: aggStart.Add(time.Hour),
		Open: decimal.RequireFromString("2600"), High: decimal.RequireFromString("2680"),
		Low: decimal.RequireFromString("2590"), Close: decimal.RequireFromString("2650"),
		Complete: true,
	}
	a.Seed("XAUUSD", tf1h, stored)

	bars, err := a.Observe(quoteAt(aggStart.Add(40*time.Minute), "2670"), tf1h)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !bars[0].Open.Equal(decimal.RequireFromString("2670")) {
		t.Errorf("open = %s; a complete bar was reopened and extended", bars[0].Open)
	}
}

func TestVolumeCountsTicksRatherThanBeingLeftAtZero(t *testing.T) {
	// There is no real volume in a quote feed. Tick count is an honest proxy
	// and is what the indicators that reference volume will see; leaving it
	// zero would make every volume-aware calculation degenerate.
	a := NewAggregator("test", tf1h)
	for k := 0; k < 5; k++ {
		if _, err := a.Observe(quoteAt(aggStart.Add(time.Duration(k)*time.Minute),
			decimal.NewFromInt(2650+int64(k)).String()), tf1h); err != nil {
			t.Fatalf("Observe: %v", err)
		}
	}
	bars, err := a.Observe(quoteAt(aggStart.Add(time.Hour+time.Minute), "2700"), tf1h)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !bars[0].Volume.Equal(decimal.NewFromInt(5)) {
		t.Errorf("volume = %s, want the 5 ticks observed", bars[0].Volume)
	}
}

func TestTheProviderNameIsStampedOnEveryBar(t *testing.T) {
	// A bar built during a replay must be distinguishable from one built from
	// generated data afterwards.
	a := NewAggregator("replay", tf1h)
	bars, err := a.Observe(quoteAt(aggStart, "2650"), tf1h)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if bars[0].Provider != "replay" {
		t.Errorf("provider = %q, want replay", bars[0].Provider)
	}
}
