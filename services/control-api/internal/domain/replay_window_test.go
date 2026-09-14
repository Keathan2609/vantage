package domain

import (
	"strings"
	"testing"
	"time"
)

// The window is what stops a replay's result depending on what happened to be
// in the database. These tests are about the two halves of that: the floor
// that bounds what a run may SEE, and the warm-up gate that bounds when it may
// ACT.

var winStart = time.Date(2027, 3, 2, 4, 0, 0, 0, time.UTC)

func testWindow() ReplayWindow {
	return ReplayWindow{
		Active:          true,
		WarmupStart:     winStart,
		EvaluationStart: winStart.Add(60 * time.Hour),
		EvaluationEnd:   winStart.Add(140 * time.Hour),
	}
}

func TestAnInactiveWindowImposesNoFloor(t *testing.T) {
	// Ordinary operation must be untouched. A zero floor applied to a live
	// process would silently truncate every historical read, and the symptom
	// -- strategies refusing for want of history -- looks nothing like its
	// cause.
	w := NoReplayWindow()
	if !w.Floor().IsZero() {
		t.Errorf("an inactive window imposed a floor of %s", w.Floor())
	}
	if w.InWarmup(winStart) {
		t.Error("an inactive window reported itself in warm-up")
	}
	if !strings.Contains(w.Describe(), "unbounded") {
		t.Errorf("describe = %q", w.Describe())
	}
}

func TestAnActiveWindowFloorsAtItsWarmupStart(t *testing.T) {
	// The floor is the warm-up start, not the evaluation start: warm-up data
	// is part of the run's declared inputs and the indicators need it. Flooring
	// at the evaluation start would starve every indicator of the history the
	// warm-up exists to provide.
	w := testWindow()
	if !w.Floor().Equal(winStart) {
		t.Errorf("floor = %s, want the warm-up start %s", w.Floor(), winStart)
	}
}

func TestWarmupSuppressesTradingUntilTheEvaluationStart(t *testing.T) {
	w := testWindow()

	for _, at := range []time.Time{
		winStart,
		winStart.Add(time.Hour),
		w.EvaluationStart.Add(-time.Nanosecond),
	} {
		if !w.InWarmup(at) {
			t.Errorf("%s is before the evaluation start and was not treated as warm-up", at)
		}
	}
	for _, at := range []time.Time{
		w.EvaluationStart,
		w.EvaluationStart.Add(time.Hour),
		w.EvaluationEnd,
	} {
		if w.InWarmup(at) {
			t.Errorf("%s is at or after the evaluation start and was treated as warm-up", at)
		}
	}
}

func TestWarmupTradingCanBeEnabledExplicitly(t *testing.T) {
	// The brief requires the suppression to be configurable rather than
	// absolute. The DEFAULT is the safe one, and this proves the opt-in
	// exists without weakening it.
	w := testWindow()
	w.AllowWarmupTrading = true

	if w.InWarmup(winStart) {
		t.Error("warm-up trading was enabled and the gate still closed")
	}
	// The floor is unaffected: permitting trading during warm-up does not
	// permit seeing data from before the dataset.
	if !w.Floor().Equal(winStart) {
		t.Errorf("enabling warm-up trading moved the floor to %s", w.Floor())
	}
	if !strings.Contains(w.Describe(), "warm-up trading: true") {
		t.Errorf("describe does not disclose the opt-in: %q", w.Describe())
	}
}

func TestAnIncoherentWindowIsRefused(t *testing.T) {
	// A window whose evaluation starts before its warm-up, or ends before it
	// starts, would produce a run nobody could interpret. Refusing at Start is
	// better than a run that quietly evaluates nothing.
	w := testWindow()
	w.EvaluationStart = w.WarmupStart.Add(-time.Hour)
	if err := w.Validate(); err == nil {
		t.Error("an evaluation starting before the warm-up was accepted")
	}

	w = testWindow()
	w.EvaluationEnd = w.EvaluationStart.Add(-time.Hour)
	if err := w.Validate(); err == nil {
		t.Error("an evaluation ending before it starts was accepted")
	}

	if err := NoReplayWindow().Validate(); err != nil {
		t.Errorf("an inactive window was refused: %v", err)
	}
	if err := testWindow().Validate(); err != nil {
		t.Errorf("a coherent window was refused: %v", err)
	}
}

func TestTheWindowDescribesItselfWithItsTimestamps(t *testing.T) {
	// A run record that said only "warm-up applied" would leave the reader
	// unable to check what the run actually saw.
	d := testWindow().Describe()
	for _, want := range []string{"2027-03-02T04:00:00Z", "warm-up from", "evaluation from"} {
		if !strings.Contains(d, want) {
			t.Errorf("describe omits %q: %s", want, d)
		}
	}
}
