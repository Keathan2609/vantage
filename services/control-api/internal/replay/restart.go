package replay

import (
	"context"
	"fmt"
	"time"
)

// Restart semantics for a market replay.
//
// # The policy, chosen rather than inherited
//
// A replay STOPS at a process restart and requires an explicit operator resume
// from a verified durable cursor. It never resumes by itself.
//
// The alternative -- resuming automatically at boot -- was rejected for one
// reason: engaging a replay puts the WHOLE PROCESS on dataset time. A control
// plane that came back up and silently moved its own clock to 2027 because a
// row in a table said a run was in progress would be making a decision on the
// operator's behalf, at the moment nobody is watching, in the state where
// least is known about why it went down. Autopilot defaults to off for the
// same reason.
//
// So the boot path finds any run left in a non-terminal state, marks it
// INTERRUPTED with the reason, and leaves the process on real time. A resume
// is a deliberate act.
//
// # Why resuming from a throttled cursor is safe
//
// The cursor is persisted on every state change and at most every two seconds
// while stepping, so after a crash it can lag the work actually done. Resuming
// from it therefore re-plays some instants that were already evaluated.
//
// That is safe because of a guarantee that exists for a different reason: a
// strategy is evaluated once per completed bar, enforced by a unique index on
// (strategy, version, account, instrument, bar_time). A re-played instant
// finds its bar already evaluated and produces nothing. The financial
// consequence of a lagging cursor is therefore zero, and the alternative --
// persisting the cursor on every instant -- would put a synchronous write in
// the middle of the pipeline for no gain.
//
// What this does NOT protect against is a dataset that changed between the
// crash and the resume, which is why the hash is verified below.

// StateInterrupted means a run was active when its process stopped.
//
// A distinct state from `failed`, which means the pipeline errored, and from
// `stopped`, which means an operator ended the run. Conflating them would lose
// the one fact an operator needs: this run did not finish and nobody decided
// that.
const StateInterrupted State = "interrupted"

// ResumableRun is a run that can be continued, with everything needed to
// decide whether continuing is safe.
type ResumableRun struct {
	ID          string
	DatasetID   string
	DatasetHash string
	CodeSHA     string
	Cursor      int
	State       string
	StartedAt   time.Time
	// Reason explains why it is or is not resumable, for an operator reading
	// the list rather than the code.
	Reason string
	// Resumable is false when the dataset has changed, the run finished
	// normally, or the record is incomplete.
	Resumable bool
}

// InterruptedRunSink marks runs interrupted at boot. Supplied by the
// application, for the same reason the Recorder is: this package sits above
// store and cannot import it.
type InterruptedRunSink func(ctx context.Context, reason string) (int64, error)

// SetInterruptedRunSink installs it.
func (e *Engine) SetInterruptedRunSink(fn InterruptedRunSink) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.markInterrupted = fn
}

// AdoptInterruptedRuns is called once at boot.
//
// It does NOT resume anything. It closes the books on runs whose process died,
// so the record says what happened rather than leaving a row that claims to be
// running while nothing is.
func (e *Engine) AdoptInterruptedRuns(ctx context.Context) error {
	e.mu.Lock()
	sink := e.markInterrupted
	e.mu.Unlock()
	if sink == nil {
		return nil
	}

	const reason = "the process stopped while this run was active; a replay " +
		"never resumes by itself because engaging one puts the whole process " +
		"on dataset time, and that must be a deliberate act"

	n, err := sink(ctx, reason)
	if err != nil {
		return fmt.Errorf("replay: adopting interrupted runs: %w", err)
	}
	if n > 0 {
		e.log.Warn("marked replay runs as interrupted at startup; none was resumed",
			"runs", n, "policy", "operator resume required")
	}
	return nil
}

// VerifyResumable decides whether an interrupted run may be continued.
//
// The dataset hash is the thing that matters. A run resumed against an edited
// fixture would produce a result attributed to the original data, which is
// worse than refusing: the record would look complete and be wrong.
func (e *Engine) VerifyResumable(run ResumableRun) ResumableRun {
	switch run.State {
	case string(StateInterrupted), string(StatePaused):
		// Continuable in principle.
	case string(StateDone):
		run.Reason = "this run finished normally; there is nothing to resume"
		return run
	case string(StateStopped):
		run.Reason = "an operator ended this run deliberately"
		return run
	case string(StateFailed):
		run.Reason = "this run failed; investigate the failure rather than resuming it"
		return run
	default:
		run.Reason = "this run is in state " + run.State + ", which cannot be resumed"
		return run
	}

	ds, ok := e.registry.Get(run.DatasetID)
	if !ok {
		run.Reason = fmt.Sprintf("dataset %q is no longer registered in this build",
			run.DatasetID)
		return run
	}
	if ds.Hash != run.DatasetHash {
		run.Reason = fmt.Sprintf(
			"the dataset has CHANGED since this run started: recorded %s, now %s. "+
				"Resuming would attribute a result to data that no longer exists",
			short(run.DatasetHash), short(ds.Hash))
		return run
	}
	if run.Cursor < 0 || run.Cursor > len(ds.Instants()) {
		run.Reason = fmt.Sprintf("the recorded cursor %d is outside the dataset's %d instants",
			run.Cursor, len(ds.Instants()))
		return run
	}

	run.Resumable = true
	run.Reason = fmt.Sprintf(
		"resumable from instant %d of %d. The cursor is persisted at most every "+
			"two seconds, so a resume may re-play a few instants -- those bars are "+
			"already evaluated and the per-bar guard produces nothing for them",
		run.Cursor, len(ds.Instants()))
	return run
}

// ResumeInterrupted continues a verified run from its durable cursor.
//
// Deliberately a separate entry point from Start rather than a flag on it: the
// two differ in what they promise. Start begins a run from nothing and clears
// state scoped to it; this one continues a run whose earlier instants already
// produced financial records, and must not clear them.
func (e *Engine) ResumeInterrupted(run ResumableRun, opts Options) (Run, error) {
	verified := e.VerifyResumable(run)
	if !verified.Resumable {
		return Run{}, fmt.Errorf("replay: %s", verified.Reason)
	}

	// Start does the heavy lifting -- engaging the clock, loading the series,
	// publishing the window -- and then the cursor is advanced to where the
	// interrupted run had reached.
	opts.DatasetID = run.DatasetID
	started, err := e.Start(opts)
	if err != nil {
		return Run{}, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.run == nil {
		return started, ErrNotActive
	}
	if verified.Cursor > 0 {
		e.fastForwardLocked(verified.Cursor)
	}
	e.log.Warn("resumed an interrupted replay from its durable cursor",
		"run", e.run.ID.String(), "resumed_from", run.ID,
		"cursor", verified.Cursor, "dataset", run.DatasetID)
	return *e.run, nil
}

// fastForwardLocked moves the provider and the clock to an instant WITHOUT
// driving the pipeline.
//
// The bars between the start and the cursor were already ingested and
// evaluated by the interrupted process. Driving them again would re-ingest
// them -- harmless, the store upserts -- but would also re-run reconciliation
// and the outbox at instants that are in the past for this run, which is work
// nobody asked for and time nobody accounted for.
func (e *Engine) fastForwardLocked(toInstant int) {
	instants := e.dataset.Instants()
	if toInstant >= len(instants) {
		toInstant = len(instants)
	}
	consumed := 0
	for consumed < len(e.dataset.Rows) && e.cursor < len(e.dataset.Rows) {
		row := e.dataset.Rows[e.cursor]
		if consumed >= toInstant {
			break
		}
		// Advance one whole instant at a time, matching how a step consumes
		// the dataset.
		n := 1
		for e.cursor+n < len(e.dataset.Rows) &&
			e.dataset.Rows[e.cursor+n].Timestamp.Equal(row.Timestamp) {
			n++
		}
		if e.cursor > 0 {
			e.provider.Step(1)
		}
		dur, _ := row.Timeframe.Duration()
		e.clock.Advance(row.Timestamp.Add(dur))
		e.cursor += n
		consumed++
	}
	e.counters.Steps = consumed
	e.counters.BarsProcessed = e.cursor
	if e.run != nil {
		e.run.Counters = e.counters
	}
}

func short(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}
