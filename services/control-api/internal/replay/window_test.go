package replay

import (
	"testing"
	"time"
)

// Every committed dataset must declare a coherent historical context, and the
// warm-up has to be counted in market INSTANTS rather than rows -- otherwise a
// two-instrument dataset warms for half as long as a one-instrument dataset
// covering the same hours.

func TestEveryFixtureDeclaresACoherentWindow(t *testing.T) {
	reg, err := LoadFixtures()
	if err != nil {
		t.Fatalf("LoadFixtures: %v", err)
	}
	for _, ds := range reg.List() {
		w := ds.Window(false)
		if !w.Active {
			t.Errorf("%s produced an inactive window", ds.ID)
			continue
		}
		if verr := w.Validate(); verr != nil {
			t.Errorf("%s: %v", ds.ID, verr)
		}
		from, to := ds.Span()
		if !w.WarmupStart.Equal(from) {
			t.Errorf("%s: warm-up starts %s, dataset starts %s", ds.ID, w.WarmupStart, from)
		}
		if w.EvaluationEnd.Before(to) {
			t.Errorf("%s: evaluation ends %s, before the dataset's last bar %s",
				ds.ID, w.EvaluationEnd, to)
		}
		// A dataset whose evaluation period is empty would pass every
		// invariant by trading nothing, which is the most misleading way for a
		// scenario to succeed.
		if !w.EvaluationStart.Before(w.EvaluationEnd) {
			t.Errorf("%s: evaluation window is empty (%s to %s); the scenario "+
				"would pass by doing nothing", ds.ID, w.EvaluationStart, w.EvaluationEnd)
		}
	}
}

func TestTheWarmupIsCountedInInstantsNotRows(t *testing.T) {
	// correlated-pair carries two instruments interleaved by timestamp, so it
	// has twice as many rows as instants. Counting rows would warm it for half
	// the market time of a single-instrument dataset and leave its indicators
	// colder at the evaluation start.
	reg, err := LoadFixtures()
	if err != nil {
		t.Fatalf("LoadFixtures: %v", err)
	}
	pair, ok := reg.Get("correlated-pair")
	if !ok {
		t.Skip("the correlated-pair fixture is not registered")
	}
	instants := pair.Instants()
	if len(instants) >= len(pair.Rows) {
		t.Fatalf("correlated-pair has %d rows and %d instants; it is not "+
			"multi-instrument and proves nothing here", len(pair.Rows), len(instants))
	}

	w := pair.Window(false)
	// The evaluation start is the close of the last warm-up INSTANT.
	warmup := DefaultWarmupInstants
	if pair.WarmupInstants > 0 {
		warmup = pair.WarmupInstants
	}
	dur, _ := pair.Rows[0].Timeframe.Duration()
	want := instants[warmup-1].Add(dur)
	if !w.EvaluationStart.Equal(want) {
		t.Errorf("evaluation starts %s, want the close of instant %d (%s)",
			w.EvaluationStart, warmup, want)
	}
}

func TestInstantsAreDistinctAndOrdered(t *testing.T) {
	reg, err := LoadFixtures()
	if err != nil {
		t.Fatalf("LoadFixtures: %v", err)
	}
	for _, ds := range reg.List() {
		instants := ds.Instants()
		if len(instants) == 0 {
			t.Errorf("%s has no instants", ds.ID)
			continue
		}
		var last time.Time
		for i, at := range instants {
			if i > 0 && !at.After(last) {
				t.Errorf("%s: instant %d (%s) does not follow %s", ds.ID, i, at, last)
			}
			last = at
		}
	}
}

func TestADatasetShorterThanItsWarmupEvaluatesNothingRatherThanPanicking(t *testing.T) {
	// A legitimate configuration -- a deliberately tiny dataset, or a large
	// declared warm-up -- and it must not panic or produce a window whose
	// evaluation start is past the end of the data.
	reg, err := LoadFixtures()
	if err != nil {
		t.Fatalf("LoadFixtures: %v", err)
	}
	ds, _ := reg.Get("day-boundary")
	ds.WarmupInstants = len(ds.Instants()) + 500

	w := ds.Window(false)
	if verr := w.Validate(); verr != nil {
		t.Fatalf("an over-long warm-up produced an invalid window: %v", verr)
	}
	if w.EvaluationStart.After(w.EvaluationEnd) {
		t.Errorf("evaluation starts %s, after it ends %s", w.EvaluationStart, w.EvaluationEnd)
	}
}
