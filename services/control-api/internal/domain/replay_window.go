package domain

import (
	"fmt"
	"time"
)

// The historical context a replay is allowed to see.
//
// # The contamination this closes
//
// A strategy is evaluated over "the last 300 bars the store holds", and a
// replay's market-data purge removes only rows the replay itself provided --
// deliberately, so it does not destroy the seeded fixture. So a replay's
// decisions were taken over a window that MIXED seeded history with dataset
// bars, and the regime, the indicators and the features partly described the
// seed rather than the dataset.
//
// Measured before this existed: a deliberately range-bound fixture was
// classified TRENDING twelve times and RANGING never, while the same close
// series classified in isolation gave ADX 11.2 and RANGING. The dataset was
// range-bound; the window it was judged in was not. A result that depends on
// what happened to be seeded is not reproducible research.
//
// # Warm-up and evaluation
//
// Indicators need history before they mean anything: ADX over 14 periods is
// noise for the first 14 bars, a 50-bar baseline needs 50 bars, and a
// correlation needs its minimum sample count. A replay that began trading at
// its first bar would trade that noise.
//
// So a dataset has two phases. During WARM-UP the pipeline ingests bars and
// builds state -- indicators, regime, correlation -- and generates no
// executable intent. At the EVALUATION START, which is a recorded timestamp,
// the autonomous pipeline may begin. Both are part of the run's declared
// inputs, so "what did this run see" has one answer.
type ReplayWindow struct {
	// Active is false whenever no replay owns the clock. Every consumer must
	// check it: the floor below is meaningless otherwise, and applying a zero
	// floor to ordinary operation would be a silent no-op that looks like a
	// control.
	Active bool

	// WarmupStart is the first instant of the dataset. NOTHING before it may
	// influence the run, which is what makes the run reproducible from its
	// declared inputs.
	WarmupStart time.Time
	// EvaluationStart is when executable intents become permissible.
	EvaluationStart time.Time
	// EvaluationEnd is the last instant of the dataset.
	EvaluationEnd time.Time

	// AllowWarmupTrading is the explicit escape hatch the brief requires:
	// warm-up must not generate executable decisions "unless explicitly
	// configured". Default false.
	AllowWarmupTrading bool
}

// NoReplayWindow is the ordinary state: no replay, no floor.
func NoReplayWindow() ReplayWindow { return ReplayWindow{} }

// InWarmup reports whether the instant is inside the warm-up phase.
//
// False when no replay is active, so an ordinary process is never in warm-up
// and can never be accidentally suppressed by this.
func (w ReplayWindow) InWarmup(now time.Time) bool {
	if !w.Active || w.AllowWarmupTrading {
		return false
	}
	return now.Before(w.EvaluationStart)
}

// Floor is the earliest bar a historical read may return, or the zero time
// when there is no replay.
//
// Returned as a value rather than applied here because the store is the only
// place that knows how to express it as SQL, and a floor computed in two
// places is a floor that will eventually differ between them.
func (w ReplayWindow) Floor() time.Time {
	if !w.Active {
		return time.Time{}
	}
	return w.WarmupStart
}

// Describe explains the window for an operator and for a run record.
func (w ReplayWindow) Describe() string {
	if !w.Active {
		return "no replay is active; historical reads are unbounded"
	}
	return fmt.Sprintf(
		"replay window: warm-up from %s, evaluation from %s to %s (warm-up trading: %t)",
		w.WarmupStart.UTC().Format(time.RFC3339),
		w.EvaluationStart.UTC().Format(time.RFC3339),
		w.EvaluationEnd.UTC().Format(time.RFC3339),
		w.AllowWarmupTrading)
}

// Validate refuses a window that could not produce a meaningful run.
func (w ReplayWindow) Validate() error {
	if !w.Active {
		return nil
	}
	if w.EvaluationStart.Before(w.WarmupStart) {
		return fmt.Errorf("replay window: evaluation starts %s, before the warm-up start %s",
			w.EvaluationStart.UTC().Format(time.RFC3339),
			w.WarmupStart.UTC().Format(time.RFC3339))
	}
	if w.EvaluationEnd.Before(w.EvaluationStart) {
		return fmt.Errorf("replay window: evaluation ends %s, before it starts %s",
			w.EvaluationEnd.UTC().Format(time.RFC3339),
			w.EvaluationStart.UTC().Format(time.RFC3339))
	}
	return nil
}
