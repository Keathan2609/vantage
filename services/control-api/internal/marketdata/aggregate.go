package marketdata

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/metrics"
)

// Aggregator builds bars from the live quote stream.
//
// # Why this exists, and what was broken without it
//
// `UpsertBars` had exactly one caller: the seeder. Nothing turned live quotes
// into bars and nothing fetched new history on a schedule, so the bar series
// was frozen at whatever moment the database was seeded. Three consequences,
// each worse than the last:
//
//  1. Every strategy evaluated the same final bar forever.
//  2. The orchestrator's per-bar guard -- which exists to stop one signal being
//     traded twice -- then suppressed every subsequent evaluation with "this
//     bar has already been evaluated". A correct guard, permanently jammed.
//  3. Meanwhile the live quote walked away from the frozen bars. Observed on a
//     running stack: the last 1h bar closed at 2567.27 while the live quote was
//     2646.50, seven hours and 79 dollars apart. A stop derived from the bar
//     was on the wrong side of the live market, and the orchestrator refused
//     the order as `price_invalid` -- correctly, and for a reason that looked
//     like a strategy bug.
//
// So autonomous PAPER trading could not run for longer than the seeded bar
// window, which is precisely what this milestone set out to establish. This is
// the fix.
//
// # Design
//
// Quotes arrive every two seconds; bars are 15m, 1h and 4h. The current
// (incomplete) bar for each timeframe is held in memory and written on every
// tick with `complete = false`, so a live chart moves. When a tick crosses an
// interval boundary the bar is written once more with `complete = true` and a
// fresh one starts.
//
// **Only complete bars are given to strategies** -- `store.Bars` filters on
// `complete` -- so a strategy never sees a bar that is still forming. That is
// the same discipline the backtester keeps, and for the same reason: acting on
// a partial bar is acting on information that will change.
type Aggregator struct {
	mu      sync.Mutex
	current map[string]*formingBar
	// timeframes is what gets built, smallest first.
	timeframes []domain.Timeframe
	provider   string
}

type formingBar struct {
	bar domain.Bar
	// lastMid guards against a duplicate tick re-widening a bar's range. A
	// repeated identical quote should change nothing.
	lastMid decimal.Decimal
	ticks   int
}

// NewAggregator builds an aggregator for the given timeframes.
//
// The timeframes are the ones strategies actually declare. Building one nobody
// reads costs a write every two seconds for no benefit, and NOT building one a
// strategy declares leaves that strategy permanently starved -- which is the
// failure this type exists to fix, so it is worth being deliberate about.
func NewAggregator(provider string, timeframes ...domain.Timeframe) *Aggregator {
	if len(timeframes) == 0 {
		timeframes = []domain.Timeframe{
			domain.Timeframe("15m"), domain.Timeframe("1h"), domain.Timeframe("4h"),
		}
	}
	sorted := make([]domain.Timeframe, len(timeframes))
	copy(sorted, timeframes)
	sort.Slice(sorted, func(i, j int) bool {
		a, _ := sorted[i].Duration()
		b, _ := sorted[j].Duration()
		return a < b
	})
	return &Aggregator{
		current:    map[string]*formingBar{},
		timeframes: sorted,
		provider:   provider,
	}
}

// Timeframes reports what this aggregator builds.
func (a *Aggregator) Timeframes() []domain.Timeframe {
	out := make([]domain.Timeframe, len(a.timeframes))
	copy(out, a.timeframes)
	return out
}

// Observe folds one quote into every timeframe and returns the bars to persist.
//
// Returns both the bar that just closed (if any) and the one now forming. The
// caller writes both: the closed one is final, and the forming one keeps a
// live view current.
//
// The mid price is used rather than the bid or the ask. A bar built from bids
// and one built from asks differ by the spread, and every indicator computed
// over them would inherit that bias -- which matters most exactly when the
// spread widens, i.e. when the market is least kind.
func (a *Aggregator) Observe(q domain.Quote, tf domain.Timeframe) ([]domain.Bar, error) {
	dur, err := tf.Duration()
	if err != nil {
		return nil, fmt.Errorf("marketdata: aggregate %s: %w", tf, err)
	}
	if !q.Bid.IsPositive() || !q.Ask.IsPositive() || q.Ask.LessThan(q.Bid) {
		// An invalid quote must not reach a bar. The ingestor rejects these
		// before storing them; this is the second line, because a bar is
		// permanent in a way a quote is not.
		return nil, fmt.Errorf("marketdata: refusing to aggregate an invalid quote for %s",
			q.InstrumentID)
	}

	mid := q.Mid()
	openTime := q.SourceTime.UTC().Truncate(dur)
	key := q.InstrumentID + "|" + string(tf)

	a.mu.Lock()
	defer a.mu.Unlock()

	forming, ok := a.current[key]
	if !ok {
		a.current[key] = newFormingBar(q, tf, openTime, dur, mid, a.provider)
		return []domain.Bar{a.current[key].bar}, nil
	}

	// A tick belonging to an earlier interval than the one forming is
	// out-of-order. Dropped rather than folded in: rewriting a bar that has
	// already been marked complete would change history a strategy may
	// already have acted on.
	if openTime.Before(forming.bar.OpenTime) {
		return nil, nil
	}

	if openTime.After(forming.bar.OpenTime) {
		closed := forming.bar
		closed.Complete = true
		a.current[key] = newFormingBar(q, tf, openTime, dur, mid, a.provider)
		return []domain.Bar{closed, a.current[key].bar}, nil
	}

	// Same interval: extend the forming bar.
	if mid.Equal(forming.lastMid) {
		// A duplicate tick. Nothing about the bar changes, so nothing is
		// written -- this is the difference between a bar with 1800 ticks and
		// one with 1800 identical writes.
		return nil, nil
	}
	forming.lastMid = mid
	forming.ticks++
	if mid.GreaterThan(forming.bar.High) {
		forming.bar.High = mid
	}
	if mid.LessThan(forming.bar.Low) {
		forming.bar.Low = mid
	}
	forming.bar.Close = mid
	forming.bar.Volume = decimal.NewFromInt(int64(forming.ticks))
	return []domain.Bar{forming.bar}, nil
}

func newFormingBar(q domain.Quote, tf domain.Timeframe, openTime time.Time,
	dur time.Duration, mid decimal.Decimal, provider string) *formingBar {

	return &formingBar{
		bar: domain.Bar{
			InstrumentID: q.InstrumentID,
			Timeframe:    tf,
			OpenTime:     openTime,
			CloseTime:    openTime.Add(dur),
			Open:         mid,
			High:         mid,
			Low:          mid,
			Close:        mid,
			Volume:       decimal.NewFromInt(1),
			// Incomplete until an interval boundary is crossed. Strategies
			// only read complete bars.
			Complete: false,
			Provider: provider,
		},
		lastMid: mid,
		ticks:   1,
	}
}

// Seed primes a forming bar from the last stored bar for an instrument.
//
// Called at start-up so a restart mid-interval continues the bar it was
// building rather than opening a new one at the restart moment. Without this,
// every restart would leave a short, misleading bar in the series.
func (a *Aggregator) Seed(instrumentID string, tf domain.Timeframe, bar domain.Bar) {
	if bar.Complete {
		// A complete bar needs no continuation; the next tick opens the next
		// interval.
		return
	}
	if _, err := tf.Duration(); err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	// Ticks continue from the stored VOLUME, not from 1.
	//
	// Volume is written as the tick count on every update, so seeding with 1
	// would make the next tick report a bar of volume 2 after an interval that
	// had accumulated hundreds -- the restart would be visible in the data as
	// a volume collapse, which is the kind of artefact a strategy reads as a
	// liquidity event.
	ticks := 1
	if v := bar.Volume.IntPart(); v > 1 {
		ticks = int(v)
	}
	a.current[instrumentID+"|"+string(tf)] = &formingBar{
		bar:     bar,
		lastMid: bar.Close,
		ticks:   ticks,
	}
}

// AggregateInto folds a quote into every configured timeframe and persists the
// results.
//
// Errors are logged per timeframe rather than returned: a 4h bar that cannot be
// written must not stop the 15m bar a strategy is waiting on.
func (a *Aggregator) AggregateInto(ctx context.Context, s barWriter, q domain.Quote) {
	log := logging.FromContext(ctx)
	for _, tf := range a.timeframes {
		bars, err := a.Observe(q, tf)
		if err != nil {
			log.Warn("could not aggregate quote into a bar",
				"instrument", q.InstrumentID, "timeframe", string(tf), "error", err.Error())
			continue
		}
		if len(bars) == 0 {
			continue
		}
		if err := s.UpsertBars(ctx, bars); err != nil {
			log.Error("could not persist aggregated bars",
				"instrument", q.InstrumentID, "timeframe", string(tf), "error", err.Error())
			continue
		}

		// The age of the newest bar this instrument and timeframe has.
		//
		// This gauge was declared and never set by anything, which is the one
		// measurement that would catch rule 9 recurring. The series froze at
		// seed time once: every strategy re-evaluated a single bar for ever,
		// the per-bar guard jammed shut, the live quote drifted 79 dollars from
		// the newest bar, and nothing reported unhealthy. A frozen series is
		// invisible in every other signal — ingestion still succeeds, the
		// quote is still fresh, the feed is still "ok" — and shows up here
		// immediately as an age that climbs and never resets.
		//
		// Set from the bar's own close time against the quote's clock rather
		// than from time.Now(), so a replay measures dataset time like
		// everything else on this path.
		newest := bars[len(bars)-1]
		metrics.MarketDataLatestAge.
			WithLabelValues(q.InstrumentID, string(tf)).
			Set(q.SourceTime.Sub(newest.CloseTime).Seconds())
	}
}

// barWriter is the slice of the market store the aggregator needs.
type barWriter interface {
	UpsertBars(ctx context.Context, bars []domain.Bar) error
}
