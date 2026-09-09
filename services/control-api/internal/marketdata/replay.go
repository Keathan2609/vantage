package marketdata

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
)

// ReplayProvider feeds a fixed series as though time were advancing.
//
// # Why this exists
//
// Every autonomous-behaviour test so far has depended on the real wall clock
// and the real generated market. That has two costs, and both were paid in
// this repository:
//
//   - The venue models a daily maintenance break (17:00-18:00 New York). Any
//     suite whose setup waits for a tradable feed simply cannot run during it,
//     and the failure reads as broken market data rather than as a closed
//     market. A whole test run was lost to that.
//   - "Trend market" and "volatility shock" are not conditions you can wait
//     for. Asserting that a trend strategy finds a trend requires a market
//     that IS trending, on demand, identically every time.
//
// So a replay provider drives the series instead of the clock: the scenario
// declares the bars, and stepping the provider is what makes time pass. The
// same run produces the same decisions, and a scenario that needs a volatility
// shock at bar 40 gets one at bar 40.
//
// # What it is not
//
// It is not a backtester. The backtester evaluates a strategy over history in
// one pass with its own cost model. This drives the LIVE pipeline -- ingestion,
// data quality, regime, strategies, risk, OMS, the mock venue -- against a
// known series, which is the only way to test the parts a backtest skips
// entirely.
//
// It is also not a market model. The series is whatever the caller supplies,
// and nothing here claims it resembles gold.
type ReplayProvider struct {
	mu sync.Mutex

	// series is the full script per instrument, in ascending time order.
	series map[string][]domain.Bar
	// cursor is how many bars of each series have been played.
	cursor map[string]int
	// spread is the fraction of mid applied to build a two-sided quote from a
	// one-sided bar. Configurable per instrument so a spread-spike scenario
	// can widen it mid-run.
	spread map[string]decimal.Decimal

	defaultSpread decimal.Decimal
	// paused stops Step from advancing, so a scenario can hold the market
	// still while it changes something else -- arming a fault, revoking
	// authority -- without the series moving underneath it.
	paused bool
	// outage makes every Quote fail, which is the "provider disconnect" case.
	// Distinct from an empty series: a provider that answers with nothing is
	// not the same as one that cannot be reached.
	outage bool
}

// NewReplayProvider builds an empty provider. Load a series with SetSeries.
func NewReplayProvider() *ReplayProvider {
	return &ReplayProvider{
		series:        map[string][]domain.Bar{},
		cursor:        map[string]int{},
		spread:        map[string]decimal.Decimal{},
		defaultSpread: decimal.RequireFromString("0.00012"),
	}
}

// Name identifies the provider in stored data.
//
// Deliberately distinct from "mock". A quote recorded during a replay must be
// identifiable as one afterwards: research that cannot tell replayed data from
// generated data will eventually treat one as the other.
func (r *ReplayProvider) Name() string { return "replay" }

// SetSeries installs the script for one instrument and rewinds its cursor.
//
// The bars are sorted and validated: an out-of-order or structurally invalid
// candle in a scenario fixture produces a test that fails for the wrong
// reason, which is worse than one that fails loudly here.
func (r *ReplayProvider) SetSeries(instrumentID string, bars []domain.Bar) error {
	if len(bars) == 0 {
		return fmt.Errorf("marketdata: replay series for %s is empty", instrumentID)
	}
	copied := make([]domain.Bar, len(bars))
	copy(copied, bars)
	sort.Slice(copied, func(i, j int) bool {
		return copied[i].OpenTime.Before(copied[j].OpenTime)
	})

	for i, b := range copied {
		if !b.Low.IsPositive() {
			return fmt.Errorf("marketdata: replay bar %d for %s has a non-positive low %s",
				i, instrumentID, b.Low)
		}
		if b.High.LessThan(b.Low) {
			return fmt.Errorf("marketdata: replay bar %d for %s has high %s below low %s",
				i, instrumentID, b.High, b.Low)
		}
		if b.High.LessThan(b.Open) || b.High.LessThan(b.Close) ||
			b.Low.GreaterThan(b.Open) || b.Low.GreaterThan(b.Close) {
			return fmt.Errorf(
				"marketdata: replay bar %d for %s is inconsistent (o=%s h=%s l=%s c=%s)",
				i, instrumentID, b.Open, b.High, b.Low, b.Close)
		}
		if i > 0 && !copied[i-1].OpenTime.Before(b.OpenTime) {
			return fmt.Errorf("marketdata: replay bars %d and %d for %s share a time",
				i-1, i, instrumentID)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.series[instrumentID] = copied
	// Start at the first bar rather than before it, so a provider with a
	// series always has a price. A scenario that wants no price yet uses
	// SetOutage.
	r.cursor[instrumentID] = 0
	return nil
}

// SetSpreadFraction overrides the spread for one instrument.
//
// Scenario E (spread spike) is exactly this: the price series is unchanged and
// only the cost of crossing it moves, which is the case where a strategy still
// sees an opportunity and the platform must still refuse.
func (r *ReplayProvider) SetSpreadFraction(instrumentID string, fraction decimal.Decimal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spread[instrumentID] = fraction
}

// SetOutage makes every quote request fail, simulating a disconnected feed.
func (r *ReplayProvider) SetOutage(down bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outage = down
}

// Pause and Resume hold the series still.
func (r *ReplayProvider) Pause()  { r.mu.Lock(); r.paused = true; r.mu.Unlock() }
func (r *ReplayProvider) Resume() { r.mu.Lock(); r.paused = false; r.mu.Unlock() }

// Paused reports whether the series is held.
func (r *ReplayProvider) Paused() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused
}

// Step advances every series by n bars and reports how many instruments still
// have bars remaining.
//
// Advancing past the end holds at the last bar rather than wrapping or
// failing: a scenario that steps too far should end with a stale market, which
// is a real condition, not with a series that silently restarts and looks like
// a new trend.
func (r *ReplayProvider) Step(n int) (remaining int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused || n <= 0 {
		return r.remainingLocked()
	}
	for id, bars := range r.series {
		next := r.cursor[id] + n
		if next > len(bars)-1 {
			next = len(bars) - 1
		}
		r.cursor[id] = next
	}
	return r.remainingLocked()
}

// Reset rewinds every series to its first bar and clears outage and pause.
//
// Spread overrides are cleared too: a scenario that widened the spread and
// then reset would otherwise leak that into the next one, and a leaked
// override is the kind of fixture bug that makes an unrelated test fail.
func (r *ReplayProvider) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.series {
		r.cursor[id] = 0
	}
	r.spread = map[string]decimal.Decimal{}
	r.paused = false
	r.outage = false
}

// Progress reports the cursor and length for one instrument.
func (r *ReplayProvider) Progress(instrumentID string) (played, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	bars, ok := r.series[instrumentID]
	if !ok {
		return 0, 0
	}
	return r.cursor[instrumentID] + 1, len(bars)
}

// CurrentBar returns the bar the cursor is on.
func (r *ReplayProvider) CurrentBar(instrumentID string) (domain.Bar, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	bars, ok := r.series[instrumentID]
	if !ok || len(bars) == 0 {
		return domain.Bar{}, false
	}
	return bars[r.cursor[instrumentID]], true
}

func (r *ReplayProvider) remainingLocked() int {
	n := 0
	for id, bars := range r.series {
		if r.cursor[id] < len(bars)-1 {
			n++
		}
	}
	return n
}

// Quote builds a two-sided quote from the current bar's close.
//
// # On the timestamp
//
// SourceTime is the bar's own time, not the caller's `now`. That is the point
// of a replay: the data carries the time it belongs to. It also means a
// scenario controls quote AGE, and therefore controls whether the platform
// considers the feed stale -- which is how the stale-data refusal becomes
// testable without waiting ten seconds.
//
// IngestedAt is the caller's `now`, because that genuinely is when this
// process received it.
func (r *ReplayProvider) Quote(_ context.Context, inst domain.Instrument,
	now time.Time) (domain.Quote, error) {

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.outage {
		return domain.Quote{}, fmt.Errorf(
			"marketdata: replay provider is simulating a disconnected feed")
	}
	bars, ok := r.series[inst.ID]
	if !ok || len(bars) == 0 {
		return domain.Quote{}, fmt.Errorf(
			"marketdata: replay provider has no series for %s", inst.ID)
	}

	bar := bars[r.cursor[inst.ID]]
	mid := bar.Close

	fraction, ok := r.spread[inst.ID]
	if !ok {
		fraction = r.defaultSpread
	}
	half := mid.Mul(fraction).Div(decimal.NewFromInt(2))

	bid := inst.Spec.RoundPrice(mid.Sub(half))
	ask := inst.Spec.RoundPrice(mid.Add(half))
	// Rounding a very tight spread on a coarse tick size can collapse or
	// cross the book. A crossed quote from a fixture is a fixture bug that
	// would surface as a data-quality refusal in an unrelated test, so it is
	// corrected here to the smallest legal spread.
	if !ask.GreaterThan(bid) {
		ask = bid.Add(inst.Spec.TickSize)
	}

	return domain.Quote{
		InstrumentID: inst.ID,
		Symbol:       inst.Symbol,
		Bid:          bid,
		Ask:          ask,
		SourceTime:   bar.CloseTime.UTC(),
		IngestedAt:   now.UTC(),
		Provider:     r.Name(),
	}, nil
}

// HistoricalBars returns the bars played so far, clipped to the request.
//
// It never returns a bar the cursor has not reached. That is the single most
// important property of this type: a strategy asking for history during a
// replay must not be handed the future it is about to be tested against. The
// backtester has an embargo for the same reason; this is the live pipeline's
// version of it.
func (r *ReplayProvider) HistoricalBars(_ context.Context, inst domain.Instrument,
	tf domain.Timeframe, from, to time.Time) ([]domain.Bar, error) {

	r.mu.Lock()
	defer r.mu.Unlock()

	bars, ok := r.series[inst.ID]
	if !ok {
		return nil, fmt.Errorf("marketdata: replay provider has no series for %s", inst.ID)
	}

	played := bars[:r.cursor[inst.ID]+1]
	out := make([]domain.Bar, 0, len(played))
	for _, b := range played {
		if b.OpenTime.Before(from) || !b.OpenTime.Before(to) {
			continue
		}
		if tf != "" && b.Timeframe != "" && b.Timeframe != tf {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

var _ Provider = (*ReplayProvider)(nil)

// ---------------------------------------------------------------------------
// Scenario series
// ---------------------------------------------------------------------------

// SeriesSpec describes a synthetic series to build.
type SeriesSpec struct {
	InstrumentID string
	Timeframe    domain.Timeframe
	Start        time.Time
	Interval     time.Duration
	StartPrice   decimal.Decimal
	Count        int
}

// TrendingSeries builds a series that rises (or falls) steadily with small
// pullbacks.
//
// Deterministic and closed-form rather than randomly generated: a scenario
// asserting "a trend strategy finds this trend" has to be reproducible, and a
// seeded RNG still couples the fixture to the generator's implementation.
//
// stepFraction is the per-bar drift. Negative produces a downtrend.
func TrendingSeries(spec SeriesSpec, stepFraction decimal.Decimal) []domain.Bar {
	return buildSeries(spec, func(i int, price decimal.Decimal) decimal.Decimal {
		// A three-bar rhythm: two with the trend, one against it by a third.
		// Enough to keep ADX high without producing an unbroken line, which no
		// indicator would ever see.
		if i%3 == 2 {
			return price.Mul(decimal.NewFromInt(1).Sub(stepFraction.Div(decimal.NewFromInt(3))))
		}
		return price.Mul(decimal.NewFromInt(1).Add(stepFraction))
	}, decimal.RequireFromString("0.0008"))
}

// RangingSeries builds a series that oscillates around its starting price.
//
// The oscillation is a fixed cycle, so the series has no net drift. A trend
// strategy that trades this is over-trading, which is what scenario B asserts.
func RangingSeries(spec SeriesSpec, amplitudeFraction decimal.Decimal) []domain.Bar {
	base := spec.StartPrice
	cycle := []int{0, 1, 2, 1, 0, -1, -2, -1}
	i := 0
	return buildSeries(spec, func(_ int, _ decimal.Decimal) decimal.Decimal {
		offset := decimal.NewFromInt(int64(cycle[i%len(cycle)]))
		i++
		return base.Mul(decimal.NewFromInt(1).Add(amplitudeFraction.Mul(offset)))
	}, decimal.RequireFromString("0.0006"))
}

// VolatilityShockSeries builds a calm series that becomes violent partway
// through.
//
// shockAt is the bar index where the range multiplies. Scenario C asserts that
// risk controls react to it, so the calm section has to be genuinely calm --
// otherwise the "before" state is already volatile and the shock proves
// nothing.
func VolatilityShockSeries(spec SeriesSpec, shockAt int,
	shockFraction decimal.Decimal) []domain.Bar {

	return buildSeriesVariableRange(spec,
		func(i int, price decimal.Decimal) decimal.Decimal {
			if i < shockAt {
				// Alternating tiny steps: quiet, and not a trend.
				if i%2 == 0 {
					return price.Mul(decimal.RequireFromString("1.0002"))
				}
				return price.Mul(decimal.RequireFromString("0.9998"))
			}
			if i%2 == 0 {
				return price.Mul(decimal.NewFromInt(1).Sub(shockFraction))
			}
			return price.Mul(decimal.NewFromInt(1).Add(shockFraction.Div(decimal.NewFromInt(2))))
		},
		func(i int) decimal.Decimal {
			if i < shockAt {
				return decimal.RequireFromString("0.0002")
			}
			return shockFraction
		})
}

// DrawdownSeries builds a series that moves steadily against a long position.
//
// Scenario H needs an account to actually lose money, repeatedly, so the
// daily-loss and drawdown protections have something real to react to.
func DrawdownSeries(spec SeriesSpec, dropFraction decimal.Decimal) []domain.Bar {
	return buildSeries(spec, func(_ int, price decimal.Decimal) decimal.Decimal {
		return price.Mul(decimal.NewFromInt(1).Sub(dropFraction))
	}, decimal.RequireFromString("0.0010"))
}

// FlatSeries builds a series that barely moves, for scenarios where the price
// path is not the subject.
func FlatSeries(spec SeriesSpec) []domain.Bar {
	return buildSeries(spec, func(i int, price decimal.Decimal) decimal.Decimal {
		if i%2 == 0 {
			return price.Mul(decimal.RequireFromString("1.00005"))
		}
		return price.Mul(decimal.RequireFromString("0.99995"))
	}, decimal.RequireFromString("0.0002"))
}

func buildSeries(spec SeriesSpec, next func(int, decimal.Decimal) decimal.Decimal,
	rangeFraction decimal.Decimal) []domain.Bar {
	return buildSeriesVariableRange(spec, next, func(int) decimal.Decimal { return rangeFraction })
}

// buildSeriesVariableRange is the shared constructor.
//
// The wick construction is deliberate: high and low are placed OUTSIDE the
// open/close body, so every bar satisfies the consistency rules SetSeries
// enforces and indicators that read true range see a real range rather than a
// body height.
func buildSeriesVariableRange(spec SeriesSpec, next func(int, decimal.Decimal) decimal.Decimal,
	rangeAt func(int) decimal.Decimal) []domain.Bar {

	if spec.Count <= 0 {
		return nil
	}
	interval := spec.Interval
	if interval <= 0 {
		interval = time.Hour
	}

	bars := make([]domain.Bar, 0, spec.Count)
	price := spec.StartPrice
	for i := 0; i < spec.Count; i++ {
		open := price
		closePx := next(i, price)
		price = closePx

		high, low := open, open
		if closePx.GreaterThan(high) {
			high = closePx
		}
		if closePx.LessThan(low) {
			low = closePx
		}
		wick := high.Add(low).Div(decimal.NewFromInt(2)).Mul(rangeAt(i))
		high = high.Add(wick)
		low = low.Sub(wick)

		openTime := spec.Start.Add(time.Duration(i) * interval).UTC()
		bars = append(bars, domain.Bar{
			InstrumentID: spec.InstrumentID,
			Timeframe:    spec.Timeframe,
			OpenTime:     openTime,
			CloseTime:    openTime.Add(interval),
			Open:         open,
			High:         high,
			Low:          low,
			Close:        closePx,
			Volume:       decimal.NewFromInt(1000),
			Complete:     true,
			Provider:     "replay",
		})
	}
	return bars
}
