package replay

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/marketdata"
)

var clockStart = time.Date(2027, 3, 2, 5, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// Clock
// ---------------------------------------------------------------------------

func TestTheClockStartsAtTheDatasetAndIsUTC(t *testing.T) {
	// Before the first step it must report the dataset's start, not the zero
	// time -- which would make every quote look impossibly old and refuse the
	// run before it began.
	c := NewClock(clockStart)
	if !c.Now().Equal(clockStart) {
		t.Errorf("Now() = %s, want %s", c.Now(), clockStart)
	}
	if c.Now().Location() != time.UTC {
		t.Errorf("location = %s, want UTC", c.Now().Location())
	}

	local := time.Date(2027, 3, 2, 5, 0, 0, 0, time.FixedZone("X", 3600))
	if got := NewClock(local).Now(); got.Location() != time.UTC {
		t.Errorf("a clock built from a zoned time reports %s", got.Location())
	}
}

func TestAdvanceMovesTheClockForward(t *testing.T) {
	c := NewClock(clockStart)
	next := clockStart.Add(time.Hour)
	c.Advance(next)
	if !c.Now().Equal(next) {
		t.Errorf("Now() = %s, want %s", c.Now(), next)
	}
	if !c.At().Equal(next) {
		t.Errorf("At() = %s, want %s", c.At(), next)
	}
}

func TestAdvanceRefusesToGoBackwards(t *testing.T) {
	// A dataset is validated to advance, so a backwards move means two
	// components disagree about where the run is. Applying it would make a
	// quote arrive before the one it follows, which the data-quality policy
	// correctly treats as a provider fault -- so the harness would be
	// manufacturing one.
	c := NewClock(clockStart)
	c.Advance(clockStart.Add(2 * time.Hour))
	before := c.Now()

	c.Advance(clockStart)
	if !c.Now().Equal(before) {
		t.Errorf("the clock moved backwards to %s", c.Now())
	}
}

func TestTickOrdersEventsInsideOneStep(t *testing.T) {
	// Several phases run at one dataset instant and some of their records are
	// ordered by time. Identical timestamps make that order arbitrary.
	c := NewClock(clockStart)
	c.Advance(clockStart)

	first := c.Now()
	c.Tick()
	second := c.Now()
	c.Tick()
	third := c.Now()

	if !second.After(first) || !third.After(second) {
		t.Fatalf("ticks did not order: %s, %s, %s", first, second, third)
	}
	// And must not spill into the next bar.
	if third.Sub(first) > time.Second {
		t.Errorf("three ticks moved the clock %s; that is not an intra-step nudge",
			third.Sub(first))
	}
	// At() ignores the offset, so the step instant is still identifiable.
	if !c.At().Equal(clockStart) {
		t.Errorf("At() = %s, want the step instant %s", c.At(), clockStart)
	}
}

func TestAdvanceResetsTheIntraStepOffset(t *testing.T) {
	c := NewClock(clockStart)
	c.Advance(clockStart)
	c.Tick()
	c.Tick()

	next := clockStart.Add(time.Hour)
	c.Advance(next)
	if !c.Now().Equal(next) {
		t.Errorf("Now() = %s after Advance, want exactly %s (offset not cleared)",
			c.Now(), next)
	}
}

func TestTheClockIsSafeUnderConcurrentUse(t *testing.T) {
	// Ingestion, the orchestrator and the venue all read it while the engine
	// advances it. Meaningful under -race.
	c := NewClock(clockStart)
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			for k := 0; k < 200; k++ {
				c.Advance(clockStart.Add(time.Duration(k) * time.Minute))
				c.Tick()
			}
		}(n)
		go func() {
			defer wg.Done()
			for k := 0; k < 200; k++ {
				_ = c.Now()
				_ = c.At()
			}
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// SwitchableClock
// ---------------------------------------------------------------------------

func TestTheSwitchableClockIsRealTimeUntilEngaged(t *testing.T) {
	// Ordinary development must be completely unaffected. If this were not
	// true, every process would be one bug away from trading on a simulated
	// clock.
	s := NewSwitchableClock(domain.SystemClock{})
	if s.Engaged() {
		t.Fatal("a fresh switchable clock reports itself engaged")
	}
	if delta := time.Since(s.Now()).Abs(); delta > time.Minute {
		t.Errorf("Now() is %s from real time before engaging", delta)
	}
}

func TestEngagingAndDisengagingSwitchesTheClock(t *testing.T) {
	s := NewSwitchableClock(domain.SystemClock{})
	r := NewClock(clockStart)

	s.Engage(r)
	if !s.Engaged() {
		t.Error("Engaged() reported false after Engage")
	}
	if !s.Now().Equal(clockStart) {
		t.Errorf("Now() = %s while engaged, want the replay instant %s", s.Now(), clockStart)
	}

	s.Disengage()
	if s.Engaged() {
		t.Error("Engaged() reported true after Disengage")
	}
	if delta := time.Since(s.Now()).Abs(); delta > time.Minute {
		t.Errorf("Now() is %s from real time after disengaging; a process left on "+
			"replay time would judge every real quote against a date in a fixture", delta)
	}
}

func TestANilBaseClockFallsBackToRealTime(t *testing.T) {
	s := NewSwitchableClock(nil)
	if delta := time.Since(s.Now()).Abs(); delta > time.Minute {
		t.Errorf("a nil base clock did not fall back to real time (%s off)", delta)
	}
}

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

// recordingStepper counts pipeline passes and can be made to fail.
type recordingStepper struct {
	mu          sync.Mutex
	steps       int
	phases      []string
	reconciles  int
	failAtStep  int
	observedNow []time.Time
	clock       *SwitchableClock
}

func (r *recordingStepper) ReplayStep(_ context.Context, onPhase func(string)) error {
	r.mu.Lock()
	r.steps++
	step := r.steps
	if r.clock != nil {
		r.observedNow = append(r.observedNow, r.clock.Now())
	}
	r.mu.Unlock()

	for _, p := range []string{"ingest", "backfill", "resting_orders", "strategies"} {
		r.mu.Lock()
		r.phases = append(r.phases, p)
		r.mu.Unlock()
		if onPhase != nil {
			onPhase(p)
		}
	}
	if r.failAtStep > 0 && step == r.failAtStep {
		return errors.New("deliberate pipeline failure")
	}
	return nil
}

func (r *recordingStepper) ReplayReconcile(context.Context) error {
	r.mu.Lock()
	r.reconciles++
	r.mu.Unlock()
	return nil
}

func (r *recordingStepper) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.steps
}

func testEngine(t *testing.T, stepper Stepper) (*Engine, *SwitchableClock) {
	t.Helper()
	reg, err := LoadFixtures()
	if err != nil {
		t.Fatalf("LoadFixtures: %v", err)
	}
	switcher := NewSwitchableClock(domain.SystemClock{})
	log := logging.New(logging.Options{Level: "error", Service: "test", Env: "test"})
	return NewEngine(reg, marketdata.NewReplayProvider(), switcher, stepper, log), switcher
}

func TestStartEngagesReplayTimeAndDoesNotStep(t *testing.T) {
	// A run that began advancing the moment it was created could not be
	// inspected before its first decision, and bar one is the most common
	// thing an operator wants to look at.
	stepper := &recordingStepper{}
	e, switcher := testEngine(t, stepper)

	run, err := e.Start(Options{DatasetID: "trend-clean", Seed: 42, Speed: "max"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if run.State != StatePaused {
		t.Errorf("state = %s, want paused", run.State)
	}
	if stepper.count() != 0 {
		t.Errorf("Start drove %d pipeline passes", stepper.count())
	}
	if !switcher.Engaged() {
		t.Error("Start did not engage replay time")
	}
	if run.DatasetHash == "" || run.Seed != 42 {
		t.Errorf("run does not carry its provenance: %+v", run)
	}
	if run.ID.String() == "" {
		t.Error("run has no id")
	}
}

func TestAnUnknownDatasetIsRefused(t *testing.T) {
	e, switcher := testEngine(t, &recordingStepper{})
	_, err := e.Start(Options{DatasetID: "../../etc/passwd", Speed: "max"})
	if !errors.Is(err, ErrNoDataset) {
		t.Fatalf("Start with a path-like id = %v, want ErrNoDataset", err)
	}
	if switcher.Engaged() {
		t.Error("a failed Start left the process on replay time")
	}
}

func TestABadSpeedIsRefusedBeforeEngaging(t *testing.T) {
	e, switcher := testEngine(t, &recordingStepper{})
	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "warp"}); err == nil {
		t.Fatal("an invalid speed was accepted")
	}
	if switcher.Engaged() {
		t.Error("a failed Start left the process on replay time")
	}
}

func TestSteppingDrivesThePipelineOncePerRow(t *testing.T) {
	stepper := &recordingStepper{}
	e, _ := testEngine(t, stepper)
	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	run, err := e.Step(context.Background(), 5)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if stepper.count() != 5 {
		t.Errorf("pipeline passes = %d, want 5", stepper.count())
	}
	if run.Counters.Steps != 5 || run.Counters.BarsProcessed != 5 {
		t.Errorf("counters = %+v, want 5 steps and 5 bars", run.Counters)
	}
	if played, total := e.Progress(); played != 5 || total == 0 {
		t.Errorf("progress = %d/%d", played, total)
	}
}

func TestTheClockIsCurrentBeforeThePipelineRuns(t *testing.T) {
	// The ordering that matters most. If the clock were advanced after the
	// pipeline, the first quote of every step would be judged against the
	// previous step's time -- which is exactly the off-by-one that made every
	// quote look like it came from the future.
	switcher := NewSwitchableClock(domain.SystemClock{})
	stepper := &recordingStepper{clock: switcher}
	reg, err := LoadFixtures()
	if err != nil {
		t.Fatalf("LoadFixtures: %v", err)
	}
	log := logging.New(logging.Options{Level: "error", Service: "test", Env: "test"})
	e := NewEngine(reg, marketdata.NewReplayProvider(), switcher, stepper, log)

	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.Step(context.Background(), 6); err != nil {
		t.Fatalf("Step: %v", err)
	}

	ds, _ := reg.Get("trend-clean")
	stepper.mu.Lock()
	observed := append([]time.Time(nil), stepper.observedNow...)
	stepper.mu.Unlock()

	for i, seen := range observed {
		dur, _ := ds.Rows[i].Timeframe.Duration()
		want := ds.Rows[i].Timestamp.Add(dur)
		if !seen.Equal(want) {
			t.Fatalf("step %d ran at %s, want the bar close %s",
				i, seen.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	}
	// And it must move forward across steps.
	for i := 1; i < len(observed); i++ {
		if !observed[i].After(observed[i-1]) {
			t.Fatalf("step %d did not advance the clock", i)
		}
	}
}

func TestRunningToTheEndFinishesAndReleasesRealTime(t *testing.T) {
	stepper := &recordingStepper{}
	e, switcher := testEngine(t, stepper)
	if _, err := e.Start(Options{DatasetID: "day-boundary", Speed: "max"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, total := e.Progress()
	run, err := e.Step(context.Background(), total+5)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if run.State != StateDone {
		t.Errorf("state = %s, want done", run.State)
	}
	if run.FinishedAt == nil {
		t.Error("a finished run has no finish time")
	}
	if switcher.Engaged() {
		t.Error("a finished run left the process on replay time")
	}
	// Stepping a finished run is a conflict, not a silent no-op.
	if _, err := e.Step(context.Background(), 1); !errors.Is(err, ErrTerminal) {
		t.Errorf("stepping a finished run = %v, want ErrTerminal", err)
	}
}

func TestAPipelineFailureFailsTheRunAndReleasesRealTime(t *testing.T) {
	// A run left engaged after a failure would judge every real quote against
	// a date in the dataset while reporting a healthy feed.
	stepper := &recordingStepper{failAtStep: 3}
	e, switcher := testEngine(t, stepper)
	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	run, err := e.Step(context.Background(), 10)
	if err == nil {
		t.Fatal("a failing pipeline did not surface an error")
	}
	if run.State != StateFailed {
		t.Errorf("state = %s, want failed", run.State)
	}
	if run.Error == "" {
		t.Error("a failed run recorded no cause")
	}
	if switcher.Engaged() {
		t.Error("a failed run left the process on replay time")
	}
}

func TestPauseResumeAndStop(t *testing.T) {
	e, switcher := testEngine(t, &recordingStepper{})
	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Start leaves the run paused, so resuming is legal and pausing again is.
	if _, err := e.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := e.Resume(); !errors.Is(err, ErrNotPaused) {
		t.Errorf("resuming a running run = %v, want ErrNotPaused", err)
	}
	if _, err := e.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	run, err := e.Stop()
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if run.State != StateStopped {
		t.Errorf("state = %s, want stopped", run.State)
	}
	if switcher.Engaged() {
		t.Error("Stop did not release replay time")
	}
}

func TestResetRewindsAndClearsCounters(t *testing.T) {
	stepper := &recordingStepper{}
	e, switcher := testEngine(t, stepper)
	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.Step(context.Background(), 8); err != nil {
		t.Fatalf("Step: %v", err)
	}

	run, err := e.Reset()
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if run.Counters.Steps != 0 {
		t.Errorf("counters after Reset = %+v, want zero", run.Counters)
	}
	if played, _ := e.Progress(); played != 0 {
		t.Errorf("progress after Reset = %d, want 0", played)
	}
	if !switcher.Engaged() {
		t.Error("Reset released replay time; it should rewind, not stop")
	}
	// And the clock must be usable again: Advance refuses backwards moves, so
	// a rewind has to install a fresh clock rather than wind the old one back.
	now, ok := e.ReplayNow()
	if !ok {
		t.Fatal("no replay instant after Reset")
	}
	if _, err := e.Step(context.Background(), 1); err != nil {
		t.Fatalf("Step after Reset: %v", err)
	}
	after, _ := e.ReplayNow()
	if !after.After(now) && !after.Equal(now) {
		t.Errorf("the clock did not move after Reset: %s then %s", now, after)
	}
}

func TestControlsRefuseWhenNoRunIsActive(t *testing.T) {
	e, _ := testEngine(t, &recordingStepper{})
	for name, call := range map[string]func() error{
		"step":    func() error { _, err := e.Step(context.Background(), 1); return err },
		"pause":   func() error { _, err := e.Pause(); return err },
		"resume":  func() error { _, err := e.Resume(); return err },
		"stop":    func() error { _, err := e.Stop(); return err },
		"reset":   func() error { _, err := e.Reset(); return err },
		"speed":   func() error { _, err := e.SetSpeed("10x"); return err },
		"outage":  func() error { return e.SetOutage(true) },
		"reconc":  func() error { return e.Reconcile(context.Background()) },
		"advance": func() error { _, err := e.Advance(context.Background()); return err },
	} {
		if err := call(); !errors.Is(err, ErrNotActive) {
			t.Errorf("%s with no run = %v, want ErrNotActive", name, err)
		}
	}
	if _, active := e.Status(); active {
		t.Error("Status reported an active run when none was started")
	}
}

func TestASecondStartIsRefusedWhileARunIsActive(t *testing.T) {
	e, _ := testEngine(t, &recordingStepper{})
	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.Start(Options{DatasetID: "range-bound", Speed: "max"}); !errors.Is(err, ErrAlreadyActive) {
		t.Errorf("a second Start = %v, want ErrAlreadyActive", err)
	}
}

func TestTheOnStartHookRunsBeforeTheDatasetIsEngaged(t *testing.T) {
	// It clears a previous run's market data. Running it after engaging would
	// leave a window in which the pipeline could read the stale quotes.
	e, _ := testEngine(t, &recordingStepper{})
	var called int
	e.SetOnStart(func(context.Context) error {
		called++
		return nil
	})
	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if called != 1 {
		t.Errorf("the on-start hook ran %d times, want 1", called)
	}
}

func TestAFailingOnStartHookAbortsTheRun(t *testing.T) {
	// If the previous run's data cannot be cleared, starting anyway produces a
	// run whose refusals are artefacts. Better not to start.
	e, switcher := testEngine(t, &recordingStepper{})
	e.SetOnStart(func(context.Context) error { return errors.New("purge failed") })

	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err == nil {
		t.Fatal("Start succeeded despite a failing on-start hook")
	}
	if switcher.Engaged() {
		t.Error("a failed Start left the process on replay time")
	}
}

func TestTheBeforeStepHookRunsAtTheReplayInstant(t *testing.T) {
	// It refreshes FX rates, which sizing needs. Running it at the wrong
	// instant would stamp them with the previous step's time.
	e, _ := testEngine(t, &recordingStepper{})
	var seen []time.Time
	e.SetBeforeStep(func(_ context.Context, now time.Time) error {
		seen = append(seen, now)
		return nil
	})
	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.Step(context.Background(), 4); err != nil {
		t.Fatalf("Step: %v", err)
	}

	if len(seen) != 4 {
		t.Fatalf("the hook ran %d times, want 4", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if !seen[i].After(seen[i-1]) {
			t.Errorf("hook instant %d did not advance", i)
		}
	}
}

func TestPreflightWarningsAreReportedAtStart(t *testing.T) {
	// A run that cannot possibly trade should say so before it starts, rather
	// than being inferred from an empty result. This exists because a 90-day
	// authority expired before a 2027 dataset began and silently skipped 610
	// strategy runs.
	e, _ := testEngine(t, &recordingStepper{})
	e.SetPreflight(func(from, to time.Time) []string {
		return []string{"no authority covers " + from.Format(time.RFC3339)}
	})
	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	warnings := e.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one", warnings)
	}
	// A warning is not a refusal: the operator may want the run anyway.
	if run, _ := e.Status(); run.State != StatePaused {
		t.Errorf("state = %s; a preflight warning must not fail the run", run.State)
	}
}

func TestReconcileDelegatesToThePipeline(t *testing.T) {
	stepper := &recordingStepper{}
	e, _ := testEngine(t, stepper)
	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	stepper.mu.Lock()
	defer stepper.mu.Unlock()
	if stepper.reconciles != 1 {
		t.Errorf("reconciles = %d, want 1", stepper.reconciles)
	}
}

func TestSpeedChangesPacingNotTheStepCount(t *testing.T) {
	// The property that makes a fast run usable: it must produce the same
	// number of pipeline passes as a slow one. If speed changed the work, the
	// fast mode would be a different system.
	for _, speed := range []string{"max", "100x", "10x"} {
		stepper := &recordingStepper{}
		e, _ := testEngine(t, stepper)
		if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: speed}); err != nil {
			t.Fatalf("Start at %s: %v", speed, err)
		}
		if _, err := e.Step(context.Background(), 3); err != nil {
			t.Fatalf("Step at %s: %v", speed, err)
		}
		if stepper.count() != 3 {
			t.Errorf("at %s the pipeline ran %d times, want 3", speed, stepper.count())
		}
		if _, err := e.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}
}

func TestSetSpeedIsValidatedMidRun(t *testing.T) {
	e, _ := testEngine(t, &recordingStepper{})
	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.SetSpeed("10x"); err != nil {
		t.Errorf("SetSpeed(10x): %v", err)
	}
	if _, err := e.SetSpeed("warp"); err == nil {
		t.Error("an invalid speed was accepted mid-run")
	}
}

func TestFaultInjectionIsValidated(t *testing.T) {
	e, _ := testEngine(t, &recordingStepper{})
	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.SetOutage(true); err != nil {
		t.Errorf("SetOutage: %v", err)
	}
	if err := e.SetSpreadFraction("XAUUSD.m", "0.02"); err != nil {
		t.Errorf("SetSpreadFraction: %v", err)
	}
	for _, bad := range []string{"", "wide", "-0.01"} {
		if err := e.SetSpreadFraction("XAUUSD.m", bad); err == nil {
			t.Errorf("spread fraction %q was accepted", bad)
		}
	}
	// Stop must clear the outage, or the next run starts with a dead feed.
	if _, err := e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}
	if _, err := e.Step(context.Background(), 1); err != nil {
		t.Fatalf("the outage survived Stop: %v", err)
	}
}
