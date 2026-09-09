package replay

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/marketdata"
)

// State is where a replay run is.
type State string

const (
	StateIdle    State = "idle"
	StateRunning State = "running"
	StatePaused  State = "paused"
	StateStopped State = "stopped"
	StateDone    State = "done"
	StateFailed  State = "failed"
)

// Terminal reports whether no further stepping is possible.
func (s State) Terminal() bool {
	return s == StateStopped || s == StateDone || s == StateFailed
}

// Stepper is the slice of the scheduler the engine drives.
//
// An interface rather than the concrete *scheduler.Scheduler so this package
// does not import scheduler, which imports orchestrator, oms, store and most
// of the rest. The dependency would point outwards and the import cycle would
// be real: the scheduler has to be constructible without knowing about replay.
type Stepper interface {
	ReplayStep(ctx context.Context, onPhase func(phase string)) error
	ReplayReconcile(ctx context.Context) error
}

// Counters are what a run observed.
//
// Recorded per run because "the system did nothing" and "the system was never
// asked" look identical without them, and the difference is the whole question
// a paper-forward run exists to answer.
type Counters struct {
	Steps         int `json:"steps"`
	BarsProcessed int `json:"bars_processed"`
	Errors        int `json:"errors"`
}

// Engine drives one replay at a time.
type Engine struct {
	mu sync.Mutex

	registry *Registry
	provider *marketdata.ReplayProvider
	clock    *Clock
	switcher *SwitchableClock
	stepper  Stepper
	log      *logging.Logger

	// detach removes bar aggregation for the duration of a run, returning a
	// function that restores it. Nil is legitimate: an engine built in a test
	// without an ingestor has nothing to detach.
	detach func() (restore func())
	// aggregator is detached during a replay and restored afterwards.
	//
	// The dataset supplies complete bars directly, so folding one quote per bar
	// into a bar as well would write a degenerate candle
	// (open=high=low=close) over the real one. Two writers for the same
	// (instrument, timeframe, open_time) is exactly the ambiguity the bar
	// series must not have.
	restoreAggregator func()

	run     *Run
	dataset Dataset
	// cursor is the index of the NEXT row to play.
	cursor   int
	state    State
	speed    float64
	counters Counters

	preflight Preflight
	// onStart clears market data left by a previous run. See
	// store.PurgeReplayMarketData for why a replay cannot begin on top of
	// another one's quotes.
	onStart func(ctx context.Context) error
	// onFinish clears the market data the run wrote.
	//
	// Purging only at Start was not enough. A finished replay left its
	// future-dated quotes in market_quotes_latest, so a process restarted on
	// the mock provider saw every real quote as `timestamp_regressed` and the
	// instrument stayed INVALID for good -- a broken feed whose cause was a
	// replay that had ended cleanly hours earlier.
	onFinish func(ctx context.Context) error
	// beforeStep is an app-supplied hook run at each replay instant, before
	// the pipeline. It exists for market data the dataset does not carry --
	// currently FX rates, which the sizing path needs and which would
	// otherwise be six months stale at replay time.
	beforeStep func(ctx context.Context, now time.Time) error
	// record persists the run. See SetRecorder.
	record Recorder
	// lastPersist throttles the progress writes so a long run does not spend
	// its time in the database. State CHANGES are never throttled.
	lastPersist time.Time
	// advancing guards the background Advance goroutine.
	advancing bool
	// warnings are conditions that will stop this run trading, reported at
	// Start rather than discovered from an empty result.
	warnings []string
}

// Run is the record of one replay.
//
// # Why every field is here
//
// A replay result is only evidence if it can be tied to exactly what produced
// it. Dataset hash catches an edited fixture; code SHA catches a changed
// strategy or risk rule; config hash catches a changed threshold; the seed
// catches deliberate randomness. Without all four, "the same run" is a claim
// rather than a fact, and comparing two runs means nothing.
type Run struct {
	ID          uuid.UUID `json:"id"`
	DatasetID   string    `json:"dataset_id"`
	DatasetHash string    `json:"dataset_hash"`
	CodeSHA     string    `json:"code_sha"`
	ConfigHash  string    `json:"config_hash"`
	Seed        int64     `json:"seed"`
	// FromTime and ToTime are dataset times, not wall times. A run that
	// covered three replay days in nine seconds is described by the days.
	FromTime  time.Time `json:"from_time"`
	ToTime    time.Time `json:"to_time"`
	StartedAt time.Time `json:"started_at"`
	// FinishedAt is wall time, because "how long did this take to run" is an
	// operational question about the machine, not about the market.
	FinishedAt *time.Time `json:"finished_at"`
	State      State      `json:"state"`
	Counters   Counters   `json:"counters"`
	Error      string     `json:"error,omitempty"`
}

// Recorder persists a run record.
//
// A function rather than an interface implemented by the store, because the
// import would point the wrong way: this package sits above marketdata, which
// sits above store. The application supplies a closure that maps a Run onto
// store.ReplayRun, which is the only place the two vocabularies meet.
//
// Nil is legitimate. An engine built in a test without a database still runs;
// it simply leaves no record, and the tests that care assert on the recorder
// they injected.
type Recorder func(ctx context.Context, run Run, warnings []string) error

// Options configure a run.
type Options struct {
	DatasetID  string
	Seed       int64
	Speed      string
	CodeSHA    string
	ConfigHash string
}

// How often a run's progress reaches the database, and how long that write
// gets. Progress is written for durability, not for correctness, so it is
// deliberately cheap: a replay that spent its time persisting counters would
// be slower than the market it is replaying.
const (
	persistInterval = 2 * time.Second
	persistTimeout  = 5 * time.Second
)

// Errors callers distinguish.
var (
	ErrNoDataset     = errors.New("replay: no such dataset")
	ErrAlreadyActive = errors.New("replay: a run is already active")
	ErrNotActive     = errors.New("replay: no run is active")
	ErrTerminal      = errors.New("replay: this run has finished")
	ErrNotPaused     = errors.New("replay: the run is not paused")
)

// NewEngine builds an engine.
//
// The provider and the switchable clock are the ones the application composed,
// not new ones: engaging a replay has to affect the running pipeline, and an
// engine holding its own copies would drive nothing.
func NewEngine(reg *Registry, provider *marketdata.ReplayProvider,
	switcher *SwitchableClock, stepper Stepper, log *logging.Logger) *Engine {

	return &Engine{
		registry: reg,
		provider: provider,
		switcher: switcher,
		stepper:  stepper,
		log:      log,
		state:    StateIdle,
		speed:    1,
	}
}

// SetAggregatorControl gives the engine a way to detach and restore bar
// aggregation for the duration of a run.
func (e *Engine) SetAggregatorControl(detach func() (restore func())) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.detach = detach
}

// Start loads a dataset, engages replay time and prepares to step.
//
// It does NOT begin stepping. A run that started advancing the moment it was
// created could not be inspected before its first decision, and the most
// common thing an operator wants is to look at bar one.
func (e *Engine) Start(opts Options) (Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.state == StateRunning || e.state == StatePaused {
		return Run{}, ErrAlreadyActive
	}

	ds, ok := e.registry.Get(opts.DatasetID)
	if !ok {
		return Run{}, fmt.Errorf("%w: %q", ErrNoDataset, opts.DatasetID)
	}
	speed, err := ParseSpeed(opts.Speed)
	if err != nil {
		return Run{}, err
	}

	// Clear market data from any previous run FIRST.
	//
	// A dataset starting earlier than the last run's final bar would otherwise
	// see that bar as a quote from the future, and the data-quality policy
	// would refuse most of the run -- correctly, for a condition the harness
	// created. Measured before this existed: 415 of 1082 strategy runs
	// skipped.
	if e.onStart != nil {
		if err := e.onStart(context.Background()); err != nil {
			return Run{}, fmt.Errorf("replay: clearing previous market data: %w", err)
		}
	}

	// The provider is loaded with the whole series per instrument. It refuses
	// to serve a bar the cursor has not reached, so having the future in
	// memory is not the same as exposing it -- and that guarantee is asserted
	// in internal/marketdata.
	for _, instrument := range ds.Instruments() {
		if serr := e.provider.SetSeries(instrument, ds.Bars(instrument)); serr != nil {
			return Run{}, serr
		}
	}
	e.provider.Reset()

	from, to := ds.Span()
	// The clock starts at the first bar's CLOSE, because that is when the
	// first quote is true. Starting at the open would make the first quote
	// one bar-duration stale and the first step would refuse everything.
	firstDur, _ := ds.Rows[0].Timeframe.Duration()
	e.clock = NewClock(from.Add(firstDur))
	e.switcher.Engage(e.clock)

	if e.detach != nil {
		e.restoreAggregator = e.detach()
	}

	e.dataset = ds
	e.cursor = 0
	e.counters = Counters{}
	e.speed = speed
	e.state = StatePaused

	e.run = &Run{
		ID:          uuid.New(),
		DatasetID:   ds.ID,
		DatasetHash: ds.Hash,
		CodeSHA:     opts.CodeSHA,
		ConfigHash:  opts.ConfigHash,
		Seed:        opts.Seed,
		FromTime:    from,
		ToTime:      to,
		StartedAt:   time.Now().UTC(),
		State:       StatePaused,
	}

	e.warnings = nil
	if e.preflight != nil {
		e.warnings = e.preflight(from, to)
	}
	// An unstamped build cannot claim a code identity.
	//
	// This is the dangerous direction, not a cosmetic gap: two runs of a
	// `go run` binary both record code_sha "unknown", which COMPARES EQUAL --
	// so a comparison would assert that the same code produced both when
	// nothing of the kind was established. The warning is durable and travels
	// with the run record, so a determinism claim made from these two rows has
	// to be made in spite of it rather than in ignorance of it.
	if !plausibleCodeSHA(opts.CodeSHA) {
		e.warnings = append(e.warnings,
			"this build carries no commit identity ("+describeSHA(opts.CodeSHA)+"), so this "+
				"run cannot be compared to another on code identity: build with the "+
				"version ldflags to record one")
	}
	for _, w := range e.warnings {
		e.log.Warn("replay preflight", "run", e.run.ID.String(), "warning", w)
	}

	// Written before the run is announced, and a failure aborts the start.
	// The clock has to be released again by hand here: finishLocked would
	// record a stopped run, which is exactly the write that just failed.
	if e.record != nil {
		run := *e.run
		run.Counters = e.counters
		ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
		rerr := e.record(ctx, run, append([]string(nil), e.warnings...))
		cancel()
		if rerr != nil {
			e.switcher.Disengage()
			if e.restoreAggregator != nil {
				e.restoreAggregator()
				e.restoreAggregator = nil
			}
			e.state = StateIdle
			e.run = nil
			return Run{}, fmt.Errorf("replay: recording the run: %w", rerr)
		}
		e.lastPersist = time.Now()
	}

	e.log.Warn("market replay engaged: this process is now on replay time",
		"run", e.run.ID.String(), "dataset", ds.ID, "dataset_hash", ds.Hash[:12],
		"rows", len(ds.Rows), "from", from.Format(time.RFC3339), "to", to.Format(time.RFC3339))
	return *e.run, nil
}

// Step advances the run by n dataset rows, driving the real pipeline for each.
//
// Returns the run as it stands. Stepping past the end finishes the run rather
// than erroring: reaching the end of the data is the expected way for a replay
// to conclude.
func (e *Engine) Step(ctx context.Context, n int) (Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.run == nil {
		return Run{}, ErrNotActive
	}
	if e.state.Terminal() {
		return *e.run, ErrTerminal
	}
	if n <= 0 {
		n = 1
	}

	for i := 0; i < n; i++ {
		if e.cursor >= len(e.dataset.Rows) {
			e.finishLocked(StateDone, nil)
			return *e.run, nil
		}
		if err := e.stepOnceLocked(ctx); err != nil {
			e.counters.Errors++
			e.finishLocked(StateFailed, err)
			return *e.run, err
		}
		if err := ctx.Err(); err != nil {
			e.run.Counters = e.counters
			e.persistLocked(true)
			return *e.run, err
		}
	}
	e.run.Counters = e.counters
	e.persistLocked(false)
	return *e.run, nil
}

// stepOnceLocked plays exactly one dataset row.
func (e *Engine) stepOnceLocked(ctx context.Context) error {
	row := e.dataset.Rows[e.cursor]
	dur, err := row.Timeframe.Duration()
	if err != nil {
		return err
	}

	// 1. Move the provider to this bar.
	//
	// The provider's cursor already sits on row 0 after SetSeries, so the
	// FIRST step plays that row without advancing. Stepping unconditionally
	// was an off-by-one that made the provider serve bar N+1 while the clock
	// said bar N -- so every quote arrived from the future and the
	// data-quality policy correctly rejected the whole run as
	// `future_timestamp`. The symptom looked like a broken clock; the cause
	// was the order of these two lines.
	if e.cursor > 0 {
		e.provider.Step(1)
	}

	// 2. Move the application clock to this bar's close, which is when its
	//    quote is true. The clock must be current BEFORE ingestion reads it,
	//    or every quote is judged against the previous step's time.
	e.clock.Advance(row.Timestamp.Add(dur))

	// 3. Market data the dataset does not carry.
	//
	// FX rates are the case that forced this. The account is ZAR and gold is
	// quoted in USD, so sizing cannot be computed without a USD/ZAR rate --
	// and the seeded rate is dated at seed time, which a dataset deliberately
	// ahead of the seed makes months stale. The converter refuses a stale
	// rate, correctly, and every strategy run failed with
	// "fx: conversion rate is stale: USD/ZAR is 4310h old".
	//
	// The dataset does not model FX moves, so the rate is held CONSTANT at
	// its seeded value and re-stamped at the replay instant. That is an
	// explicit assumption rather than a hidden one: a replay's P&L is
	// therefore free of currency movement, which is documented in
	// docs/MARKET_REPLAY.md.
	if e.beforeStep != nil {
		if err := e.beforeStep(ctx, e.clock.Now()); err != nil {
			return fmt.Errorf("replay: before-step hook: %w", err)
		}
	}

	// 4. Drive the real pipeline.
	stepErr := e.stepper.ReplayStep(ctx, func(string) { e.clock.Tick() })

	e.cursor++
	e.counters.Steps++
	e.counters.BarsProcessed++
	if stepErr != nil {
		return stepErr
	}

	e.pace(dur)
	return nil
}

// pace sleeps to approximate the requested speed.
//
// Pacing only. It must never touch a price, a timestamp, a size or an
// accounting decision -- a run at 100x has to produce identical financial
// output to the same run at 1x, or the fast mode is a different system and the
// slow mode's results do not transfer. That property is asserted in the
// determinism tests.
//
// Capped, because a dataset of 4h bars at 1x would sleep for hours and nobody
// wants a "1x" that is unusable.
func (e *Engine) pace(barDuration time.Duration) {
	if e.speed <= 0 {
		return // max: no delay
	}
	delay := time.Duration(float64(barDuration) / e.speed)
	const cap = 2 * time.Second
	if delay > cap {
		delay = cap
	}
	if delay > 0 {
		time.Sleep(delay)
	}
}

// Advance runs to the end of the dataset in the BACKGROUND and returns
// immediately.
//
// # Why it cannot be synchronous
//
// A dataset of a few hundred bars drives a few hundred full pipeline passes,
// each of which is dozens of queries and an HTTP call to the research service.
// Held inside one request that is minutes long, it exhausted the request
// deadline and surfaced as "db: begin: timeout: context deadline exceeded" --
// a database error for what was really a control-flow mistake.
//
// So the caller starts it and polls GET /api/v1/replay. The context
// deliberately is NOT the request's: a run must not be abandoned half way
// because a client disconnected, since that would leave the process on replay
// time with a partially played dataset.
func (e *Engine) Advance(base context.Context) (Run, error) {
	e.mu.Lock()
	if e.run == nil {
		e.mu.Unlock()
		return Run{}, ErrNotActive
	}
	if e.state.Terminal() {
		run := *e.run
		e.mu.Unlock()
		return run, ErrTerminal
	}
	if e.advancing {
		run := *e.run
		e.mu.Unlock()
		return run, nil // already running to the end; polling shows progress
	}
	e.advancing = true
	e.state = StateRunning
	e.run.State = StateRunning
	run := *e.run
	e.mu.Unlock()

	// Detached from the request. Cancellation comes from Stop, not from a
	// client closing a socket.
	go func() {
		ctx := context.WithoutCancel(base)
		defer func() {
			e.mu.Lock()
			e.advancing = false
			e.mu.Unlock()
		}()
		for {
			r, err := e.Step(ctx, 1)
			if err != nil || r.State.Terminal() {
				return
			}
			e.mu.Lock()
			paused := e.state == StatePaused
			e.mu.Unlock()
			if paused {
				return
			}
		}
	}()
	return run, nil
}

// Reconcile runs reconciliation at the current replay instant.
func (e *Engine) Reconcile(ctx context.Context) error {
	e.mu.Lock()
	stepper := e.stepper
	active := e.run != nil && !e.state.Terminal()
	e.mu.Unlock()
	if !active {
		return ErrNotActive
	}
	return stepper.ReplayReconcile(ctx)
}

// Pause holds the run.
func (e *Engine) Pause() (Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.run == nil {
		return Run{}, ErrNotActive
	}
	if e.state.Terminal() {
		return *e.run, ErrTerminal
	}
	e.state = StatePaused
	e.run.State = StatePaused
	e.persistLocked(true)
	return *e.run, nil
}

// Resume releases a paused run.
func (e *Engine) Resume() (Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.run == nil {
		return Run{}, ErrNotActive
	}
	if e.state.Terminal() {
		return *e.run, ErrTerminal
	}
	if e.state != StatePaused {
		return *e.run, ErrNotPaused
	}
	e.state = StateRunning
	e.run.State = StateRunning
	e.persistLocked(true)
	return *e.run, nil
}

// Stop ends the run and returns the process to real time.
func (e *Engine) Stop() (Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.run == nil {
		return Run{}, ErrNotActive
	}
	e.finishLocked(StateStopped, nil)
	return *e.run, nil
}

// Reset rewinds to the start of the same dataset without disengaging.
func (e *Engine) Reset() (Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.run == nil {
		return Run{}, ErrNotActive
	}
	e.provider.Reset()
	e.cursor = 0
	e.counters = Counters{}
	from, _ := e.dataset.Span()
	dur, _ := e.dataset.Rows[0].Timeframe.Duration()
	// A fresh clock rather than rewinding the existing one: Advance refuses to
	// move backwards, deliberately, so rewinding it would silently do nothing.
	e.clock = NewClock(from.Add(dur))
	e.switcher.Engage(e.clock)
	e.state = StatePaused
	e.run.State = StatePaused
	e.run.Counters = e.counters
	e.run.Error = ""
	e.run.FinishedAt = nil
	e.persistLocked(true)
	return *e.run, nil
}

// SetSpeed changes pacing mid-run.
func (e *Engine) SetSpeed(name string) (Run, error) {
	speed, err := ParseSpeed(name)
	if err != nil {
		return Run{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.run == nil {
		return Run{}, ErrNotActive
	}
	e.speed = speed
	return *e.run, nil
}

// Status reports the current run, or false when none is active.
func (e *Engine) Status() (Run, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.run == nil {
		return Run{}, false
	}
	run := *e.run
	run.Counters = e.counters
	return run, true
}

// Progress reports how far through the dataset the run is.
func (e *Engine) Progress() (played, total int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cursor, len(e.dataset.Rows)
}

// ReplayNow reports the current replay instant, for a caller that needs to
// reason about dataset time.
func (e *Engine) ReplayNow() (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.clock == nil {
		return time.Time{}, false
	}
	return e.clock.Now(), true
}

// finishLocked ends a run and restores real time.
//
// Restoring the clock is not optional and not deferred: a process left on
// replay time after a run would judge every real quote against a date in the
// dataset, refuse everything, and report a healthy feed while doing it.
func (e *Engine) finishLocked(state State, cause error) {
	e.state = state
	if e.run != nil {
		now := time.Now().UTC()
		e.run.State = state
		e.run.FinishedAt = &now
		e.run.Counters = e.counters
		if cause != nil {
			e.run.Error = cause.Error()
		}
	}
	e.switcher.Disengage()
	if e.restoreAggregator != nil {
		e.restoreAggregator()
		e.restoreAggregator = nil
	}
	e.provider.SetOutage(false)

	// Clear the market data this run wrote. Bounded and never fatal: a run
	// that has already produced its result must not fail because the cleanup
	// did, and the next Start purges again anyway.
	if e.onFinish != nil {
		ctx, cancel := context.WithTimeout(
			context.WithoutCancel(context.Background()), persistTimeout)
		if perr := e.onFinish(ctx); perr != nil {
			e.log.Error("clearing the replay's market data failed: a later "+
				"process on a real provider will see these quotes as arriving "+
				"from the future", "error", perr)
		}
		cancel()
	}

	// Forced: the final state and counters are the record. Everything written
	// before this was a progress update.
	e.persistLocked(true)
	if e.run != nil {
		e.log.Warn("market replay disengaged: this process is back on real time",
			"run", e.run.ID.String(), "state", string(state),
			"steps", e.counters.Steps)
	}
}

// Engaged reports whether a replay currently owns the application clock.
func (e *Engine) Engaged() bool { return e.switcher.Engaged() }

// SetOutage simulates a provider disconnect mid-run, for scenario F.
func (e *Engine) SetOutage(down bool) error {
	e.mu.Lock()
	active := e.run != nil && !e.state.Terminal()
	e.mu.Unlock()
	if !active {
		return ErrNotActive
	}
	e.provider.SetOutage(down)
	return nil
}

// SetSpreadFraction widens the book mid-run, for scenario E.
func (e *Engine) SetSpreadFraction(instrumentID string, fraction string) error {
	e.mu.Lock()
	active := e.run != nil && !e.state.Terminal()
	e.mu.Unlock()
	if !active {
		return ErrNotActive
	}
	f, err := decimal.NewFromString(fraction)
	if err != nil {
		return fmt.Errorf("replay: spread fraction %q is not a number", fraction)
	}
	if f.IsNegative() {
		return fmt.Errorf("replay: spread fraction %q is negative", fraction)
	}
	e.provider.SetSpreadFraction(instrumentID, f)
	return nil
}

// Datasets lists the allowlisted datasets.
func (e *Engine) Datasets() []Dataset { return e.registry.List() }

// Preflight is an optional check run before a dataset is engaged.
//
// # Why a run needs one
//
// A replay puts the process on dataset time, and several controls are
// evaluated at that time and are entirely right to refuse. Trading authority
// is the clearest case: a dataset dated ahead of a ninety-day authority
// window means every strategy run is skipped with "Trading authority has
// expired". The refusal is correct and its cause is invisible from the
// outside -- the run simply does nothing, and 610 skipped evaluations look
// like a broken pipeline.
//
// So the application supplies a check, and a run that cannot possibly trade
// says so before it starts rather than after it has produced nothing.
type Preflight func(from, to time.Time) []string

// SetOnStart installs a hook run when a dataset is engaged.
func (e *Engine) SetOnStart(fn func(ctx context.Context) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onStart = fn
}

// SetOnFinish installs the hook that clears a finished run's market data.
//
// Symmetrical with SetOnStart deliberately: a run that tidies up after itself
// cannot poison the next process, and a run that tidies up only BEFORE itself
// leaves its last quote lying in the live feed's table.
func (e *Engine) SetOnFinish(fn func(ctx context.Context) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onFinish = fn
}

// SetBeforeStep installs a hook run at each replay instant.
func (e *Engine) SetBeforeStep(fn func(ctx context.Context, now time.Time) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.beforeStep = fn
}

// SetPreflight installs the check.
func (e *Engine) SetPreflight(p Preflight) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.preflight = p
}

// SetRecorder installs the function that persists a run record.
//
// Set before Start. A run whose opening record cannot be written is REFUSED
// rather than run unrecorded: the value of a replay is that its result can be
// tied to the dataset, code and configuration that produced it, and a result
// nobody can tie to anything is not worth the minutes it costs to produce.
func (e *Engine) SetRecorder(fn Recorder) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.record = fn
}

// persistLocked writes the current run record.
//
// Failures after the opening write are logged and counted, never fatal.
// Discarding a finished run's real financial result because a bookkeeping
// write failed would destroy the evidence to protect the filing system.
func (e *Engine) persistLocked(force bool) {
	if e.record == nil || e.run == nil {
		return
	}
	if !force && time.Since(e.lastPersist) < persistInterval {
		return
	}
	run := *e.run
	run.Counters = e.counters
	warnings := append([]string(nil), e.warnings...)
	// Detached from any request context: the record of a run that has just
	// ended must not depend on a client still being connected.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), persistTimeout)
	defer cancel()
	if err := e.record(ctx, run, warnings); err != nil {
		e.log.Error("recording the replay run failed",
			"run", run.ID.String(), "state", string(run.State), "error", err)
		return
	}
	e.lastPersist = time.Now()
}

// plausibleCodeSHA reports whether a string could be a commit identity.
//
// Deliberately a shape check and not a lookup: the engine has no business
// running git, and a build stamped with a SHA from a repository this process
// cannot see is still a usable identity. Seven hex characters is the shortest
// abbreviation git itself will hand out.
func plausibleCodeSHA(sha string) bool {
	if len(sha) < 7 {
		return false
	}
	for _, c := range sha {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// describeSHA quotes what was recorded, so the warning says which of "empty"
// and "unknown" it was rather than leaving the reader to guess.
func describeSHA(sha string) string {
	if strings.TrimSpace(sha) == "" {
		return "no code SHA was supplied"
	}
	return "code SHA " + strconv.Quote(sha)
}

// Warnings returns the preflight findings for the current run.
func (e *Engine) Warnings() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.warnings))
	copy(out, e.warnings)
	return out
}
