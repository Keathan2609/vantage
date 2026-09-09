package replay

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// The run record is what makes a replay result evidence rather than an
// anecdote, so these tests are about the record surviving every path a run can
// take -- including the one where recording itself fails.

// recordingRecorder captures every write, in order.
type recordingRecorder struct {
	mu       sync.Mutex
	runs     []Run
	warnings [][]string
	fail     error
	// failAfter lets a recorder succeed at Start and fail afterwards, which is
	// the case that must not destroy a run.
	calls     int
	failAfter int
}

func (r *recordingRecorder) record(_ context.Context, run Run, warnings []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.fail != nil && (r.failAfter == 0 || r.calls > r.failAfter) {
		return r.fail
	}
	r.runs = append(r.runs, run)
	r.warnings = append(r.warnings, warnings)
	return nil
}

func (r *recordingRecorder) last() (Run, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.runs) == 0 {
		return Run{}, false
	}
	return r.runs[len(r.runs)-1], true
}

func (r *recordingRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

func TestStartRecordsTheRunBeforeItBegins(t *testing.T) {
	// The record has to exist from the outset. A run recorded only at the end
	// leaves an interrupted replay -- a crash, a kill, a power cut -- with no
	// trace at all, which is precisely the run somebody will later want to
	// account for.
	rec := &recordingRecorder{}
	e, _ := testEngine(t, &recordingStepper{})
	e.SetRecorder(rec.record)

	run, err := e.Start(Options{DatasetID: "trend-clean", Seed: 42, Speed: "max",
		CodeSHA: "abc123", ConfigHash: "cfg99"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("recorder called %d times at Start, want 1", rec.count())
	}
	got, _ := rec.last()
	if got.ID != run.ID {
		t.Errorf("recorded id %s, want %s", got.ID, run.ID)
	}
	// The four identity fields are the reason the table exists. A record
	// missing any of them cannot support a claim about determinism.
	if got.DatasetHash == "" || got.CodeSHA != "abc123" ||
		got.ConfigHash != "cfg99" || got.Seed != 42 {
		t.Errorf("identity incomplete: hash=%q sha=%q cfg=%q seed=%d",
			got.DatasetHash, got.CodeSHA, got.ConfigHash, got.Seed)
	}
	if got.State != StatePaused {
		t.Errorf("recorded state %s, want paused", got.State)
	}
	if got.FinishedAt != nil {
		t.Error("a run that has just started has a finish time")
	}
}

func TestAnUnrecordableRunIsRefusedAndLeavesRealTimeIntact(t *testing.T) {
	// Two properties, and the second matters more.
	//
	// A replay that cannot be recorded is refused, because a result nobody can
	// tie to a dataset, a code SHA and a configuration is not worth producing.
	// But refusing HALF WAY would be far worse than running unrecorded: the
	// clock is engaged before the record is written, and a process left on
	// dataset time judges every real quote against a date in the fixture,
	// refuses everything, and reports a healthy feed while doing it.
	rec := &recordingRecorder{fail: errors.New("database is down")}
	e, switcher := testEngine(t, &recordingStepper{})
	e.SetRecorder(rec.record)

	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err == nil {
		t.Fatal("Start succeeded with a failing recorder")
	}
	if switcher.Engaged() {
		t.Fatal("a refused Start left the process on replay time")
	}
	if _, active := e.Status(); active {
		t.Error("a refused Start left a run behind")
	}
	// And the engine is still usable: a transient database failure must not
	// wedge replay until the process restarts.
	rec.fail = nil
	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err != nil {
		t.Fatalf("Start after a recorder failure: %v", err)
	}
	if !switcher.Engaged() {
		t.Error("the retried Start did not engage replay time")
	}
}

func TestARecorderFailureAfterStartDoesNotDestroyTheRun(t *testing.T) {
	// The opposite trade-off from the test above, and deliberately so. Before
	// the run exists, refusing costs nothing. Once it has produced orders,
	// fills and ledger entries, throwing the result away because a bookkeeping
	// write failed would destroy the evidence to protect the filing system.
	rec := &recordingRecorder{fail: errors.New("database went away"), failAfter: 1}
	stepper := &recordingStepper{}
	e, switcher := testEngine(t, stepper)
	e.SetRecorder(rec.record)

	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max", CodeSHA: "sha"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.Step(context.Background(), 3); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if _, err := e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stepper.count() != 3 {
		t.Errorf("pipeline ran %d times, want 3", stepper.count())
	}
	run, _ := e.Status()
	if run.State != StateStopped {
		t.Errorf("state = %s, want stopped", run.State)
	}
	if run.Counters.Steps != 3 {
		t.Errorf("steps = %d, want 3", run.Counters.Steps)
	}
	// And real time is restored regardless: the clock must not depend on the
	// database.
	if switcher.Engaged() {
		t.Error("the process is still on replay time after a stop")
	}
}

func TestTheFinalRecordCarriesTheTerminalStateAndCounters(t *testing.T) {
	// The last write is the one that will be read later. If the terminal state
	// or the counters were only ever throttled progress updates, a completed
	// run would be indistinguishable from one that stalled part way.
	rec := &recordingRecorder{}
	e, _ := testEngine(t, &recordingStepper{})
	e.SetRecorder(rec.record)

	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max", CodeSHA: "sha"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.Step(context.Background(), 5); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if _, err := e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	got, ok := rec.last()
	if !ok {
		t.Fatal("nothing was recorded")
	}
	if got.State != StateStopped {
		t.Errorf("final recorded state = %s, want stopped", got.State)
	}
	if got.Counters.Steps != 5 {
		t.Errorf("final recorded steps = %d, want 5", got.Counters.Steps)
	}
	if got.FinishedAt == nil {
		t.Error("a stopped run was recorded without a finish time")
	}
}

func TestAFailedRunRecordsWhyItFailed(t *testing.T) {
	// A failed replay whose record says only "failed" sends the next person to
	// the logs, which by then have rotated.
	rec := &recordingRecorder{}
	e, _ := testEngine(t, &recordingStepper{failAtStep: 2})
	e.SetRecorder(rec.record)

	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max", CodeSHA: "sha"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.Step(context.Background(), 5); err == nil {
		t.Fatal("Step succeeded through a failing pipeline")
	}

	got, _ := rec.last()
	if got.State != StateFailed {
		t.Errorf("recorded state = %s, want failed", got.State)
	}
	if got.Error == "" {
		t.Error("a failed run was recorded with no cause")
	}
	if got.Counters.Errors == 0 {
		t.Error("a failed run was recorded with an error count of zero")
	}
}

func TestResetRecordsTheRewindRatherThanLeavingAFinishedRun(t *testing.T) {
	// Reset reuses the run id, so the stored row is the same row. Leaving
	// finished_at set on it would make a run that is currently replaying look
	// like one that ended.
	rec := &recordingRecorder{}
	e, _ := testEngine(t, &recordingStepper{})
	e.SetRecorder(rec.record)

	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max", CodeSHA: "sha"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.Step(context.Background(), 3); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if _, err := e.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	got, _ := rec.last()
	if got.FinishedAt != nil {
		t.Error("a reset run kept its finish time")
	}
	if got.State != StatePaused {
		t.Errorf("recorded state = %s, want paused", got.State)
	}
	if got.Counters.Steps != 0 {
		t.Errorf("recorded steps = %d after a reset, want 0", got.Counters.Steps)
	}
}

func TestPreflightWarningsReachTheRecord(t *testing.T) {
	// The difference between "the platform decided not to trade" and "the
	// platform was never allowed to" is invisible in a result of zero orders,
	// and it is the whole question a paper-forward run exists to answer.
	rec := &recordingRecorder{}
	e, _ := testEngine(t, &recordingStepper{})
	e.SetRecorder(rec.record)
	e.SetPreflight(func(from, to time.Time) []string {
		return []string{"trading authority does not cover the dataset"}
	})

	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max", CodeSHA: "sha"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.warnings) == 0 {
		t.Fatal("no warnings were recorded")
	}
	found := false
	for _, w := range rec.warnings[0] {
		if strings.Contains(w, "trading authority does not cover") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings recorded = %v, want the preflight finding among them", rec.warnings[0])
	}
}

func TestAnEngineWithNoRecorderStillRuns(t *testing.T) {
	// A test engine, and a development process without a database, must not
	// need one. The alternative is a package whose tests cannot run without
	// Postgres, which is how test suites stop being run.
	stepper := &recordingStepper{}
	e, _ := testEngine(t, stepper)

	if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.Step(context.Background(), 2); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if stepper.count() != 2 {
		t.Errorf("pipeline ran %d times, want 2", stepper.count())
	}
}

func TestAnUnstampedBuildIsRecordedAsUnableToClaimACodeIdentity(t *testing.T) {
	// The failure this prevents is a false claim of sameness. Two runs of a
	// `go run` binary both record "unknown", which compares equal, so a
	// determinism comparison would report identical code where none was
	// established. The warning is persisted with the run.
	for _, sha := range []string{"", "unknown", "dev", "not-a-sha"} {
		rec := &recordingRecorder{}
		e, _ := testEngine(t, &recordingStepper{})
		e.SetRecorder(rec.record)

		if _, err := e.Start(Options{DatasetID: "trend-clean", Speed: "max", CodeSHA: sha}); err != nil {
			t.Fatalf("Start(%q): %v", sha, err)
		}
		warnings := e.Warnings()
		found := false
		for _, w := range warnings {
			if strings.Contains(w, "no commit identity") {
				found = true
			}
		}
		if !found {
			t.Errorf("CodeSHA %q produced no warning: %v", sha, warnings)
		}
		rec.mu.Lock()
		recorded := len(rec.warnings) > 0 && len(rec.warnings[0]) > 0
		rec.mu.Unlock()
		if !recorded {
			t.Errorf("CodeSHA %q: the warning did not reach the record", sha)
		}
		if _, err := e.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}
}

func TestARealCommitIdentityProducesNoSuchWarning(t *testing.T) {
	// The complement, so the warning stays meaningful. A check that fires on
	// every run is one nobody reads.
	e, _ := testEngine(t, &recordingStepper{})
	if _, err := e.Start(Options{
		DatasetID: "trend-clean", Speed: "max",
		CodeSHA: "d1dff1f4db1cdbdb098908aa5a8869b592f94271",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, w := range e.Warnings() {
		if strings.Contains(w, "no commit identity") {
			t.Errorf("a stamped build was warned about: %q", w)
		}
	}
}

func TestAnAbbreviatedSHAIsAccepted(t *testing.T) {
	// Seven hex characters is what git itself abbreviates to, and a CI job
	// stamping the short form should not be told it has no identity.
	if !plausibleCodeSHA("d1dff1f") {
		t.Error("a seven-character git abbreviation was rejected")
	}
	if plausibleCodeSHA("d1dff1") {
		t.Error("six characters was accepted; git does not abbreviate that short")
	}
	if plausibleCodeSHA("D1DFF1F4DB") {
		t.Error("uppercase was accepted; git writes lowercase and a mixed case " +
			"would compare unequal to the same commit")
	}
}
