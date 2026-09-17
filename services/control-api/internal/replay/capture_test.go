package replay

import (
	"context"
	"errors"
	"testing"
)

// A flat account and a failed snapshot must not look the same.
//
// `StartingPositions` is an `int`, so a run that began on a genuinely flat
// account and a run whose portfolio snapshot failed both recorded 0. An empty
// position set is valid data; a failed snapshot is missing data; a calibration
// run that cannot tell them apart attributes its result to a starting state it
// never observed.

func TestAnUnsetCaptureStatusIsNotTreatedAsACapture(t *testing.T) {
	// Silence is not an observation. A gatherer that returns inputs without
	// saying whether it observed anything has not captured a starting state,
	// and the zero value of CaptureStatus is the empty string -- which must
	// never read as CAPTURED.
	var zero CaptureStatus
	if zero.Trustworthy() {
		t.Fatal("the zero CaptureStatus reports itself trustworthy, so a gatherer " +
			"that set nothing would be read as having captured the account")
	}
	if CaptureNotAttempted.Trustworthy() {
		t.Error("NOT_ATTEMPTED reports itself trustworthy")
	}
	if CaptureFailed.Trustworthy() {
		t.Error("CAPTURE_FAILED reports itself trustworthy")
	}
	if !CaptureCaptured.Trustworthy() {
		t.Error("CAPTURED does not report itself trustworthy, so nothing ever is")
	}
}

func TestTheThreeCaptureStatesAreDistinct(t *testing.T) {
	// Each answers a different question, and collapsing any two loses the
	// distinction this exists for:
	//
	//   NOT_ATTEMPTED  -- nobody looked
	//   CAPTURED       -- looked, and this is what was there
	//   CAPTURE_FAILED -- looked, and could not tell
	seen := map[CaptureStatus]bool{}
	for _, c := range []CaptureStatus{CaptureNotAttempted, CaptureCaptured, CaptureFailed} {
		if c == "" {
			t.Fatal("a capture state is the empty string, which is the zero value " +
				"and cannot be distinguished from an unset field")
		}
		if seen[c] {
			t.Errorf("capture state %q is used twice", c)
		}
		seen[c] = true
	}
	if len(seen) != 3 {
		t.Fatalf("%d distinct capture states; three different facts need three", len(seen))
	}
}

// testDatasetID is a fixture every replay test uses; the dataset's contents
// are irrelevant here, only that Start gets as far as gathering inputs.
const testDatasetID = "trend-clean"

// newTestEngine builds an engine with a stepper that records nothing, because
// these tests never step.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	e, _ := testEngine(t, &recordingStepper{})
	return e
}

// failingGatherer stands in for a portfolio snapshot that could not be read.
func failingGatherer(error) InputGatherer {
	return func(context.Context) (RunInputs, error) {
		return RunInputs{}, errors.New("computing the portfolio snapshot: connection refused")
	}
}

// silentGatherer returns inputs but never says whether it observed them, which
// is what a caller written before the status existed does.
func silentGatherer() InputGatherer {
	return func(context.Context) (RunInputs, error) {
		return RunInputs{StartingBalance: "500.00", StartingCurrency: "ZAR"}, nil
	}
}

func capturingGatherer() InputGatherer {
	return func(context.Context) (RunInputs, error) {
		return RunInputs{
			StartingBalance: "500.00", StartingCurrency: "ZAR",
			StartingPositions:    0,
			StartingStateCapture: CaptureCaptured,
		}, nil
	}
}

func TestAResearchRunRefusesToStartWithoutACapturedStartingState(t *testing.T) {
	// FAIL CLOSED. A run whose opening balance and position set are unknown
	// produces numbers that look like evidence and are attributable to
	// nothing, so requiring capture must refuse rather than warn.
	for _, c := range []struct {
		name     string
		gatherer InputGatherer
		want     CaptureStatus
	}{
		{"the gatherer failed", failingGatherer(nil), CaptureFailed},
		{"the gatherer said nothing", silentGatherer(), CaptureNotAttempted},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEngine(t)
			e.SetInputGatherer(c.gatherer)

			_, err := e.Start(Options{
				DatasetID: testDatasetID, Seed: 42, Speed: "max",
				RequireStartingState: true,
			})
			if !errors.Is(err, ErrStartingStateUnavailable) {
				t.Fatalf("a research run started with an uncaptured starting state "+
					"(err=%v). Its result would not be attributable to a known "+
					"starting point.", err)
			}

			// And the refusal must leave the process OFF dataset time. A run
			// that aborts while still engaged leaves every later request
			// behaving inexplicably.
			if e.Engaged() {
				t.Error("the engine is still engaged after refusing to start, so the " +
					"process is on dataset time with no run")
			}
			if _, active := e.Status(); active {
				t.Error("a run is still current after the start was refused")
			}
		})
	}
}

func TestAnExploratoryRunMayProceedWithoutCapture(t *testing.T) {
	// The waiver exists for the question "what does this dataset look like",
	// which does not depend on the account at all. It must be explicit: the
	// caller asks for it rather than getting it by omission.
	e := newTestEngine(t)
	e.SetInputGatherer(failingGatherer(nil))

	run, err := e.Start(Options{
		DatasetID: testDatasetID, Seed: 42, Speed: "max",
		RequireStartingState: false,
	})
	if err != nil {
		t.Fatalf("an exploratory run was refused: %v", err)
	}
	if run.Inputs.StartingStateCapture != CaptureFailed {
		t.Errorf("the run records capture %q; it should say CAPTURE_FAILED so a "+
			"reader knows its starting point was not observed",
			run.Inputs.StartingStateCapture)
	}
	if run.Inputs.StartingStateError == "" {
		t.Error("a failed capture carries no reason, so an operator cannot tell " +
			"whether it is fixable")
	}
}

func TestACapturedStartingStateStartsAndIsRecorded(t *testing.T) {
	e := newTestEngine(t)
	e.SetInputGatherer(capturingGatherer())

	run, err := e.Start(Options{
		DatasetID: testDatasetID, Seed: 42, Speed: "max",
		RequireStartingState: true,
	})
	if err != nil {
		t.Fatalf("a run with a captured starting state was refused: %v", err)
	}
	if run.Inputs.StartingStateCapture != CaptureCaptured {
		t.Fatalf("capture recorded as %q", run.Inputs.StartingStateCapture)
	}
	if run.Inputs.StartingStateError != "" {
		t.Errorf("a successful capture carries an error: %q", run.Inputs.StartingStateError)
	}
	// Zero positions on a CAPTURED run is the case this whole mechanism
	// exists to make readable: the account really was flat.
	if run.Inputs.StartingPositions != 0 {
		t.Errorf("the fixture is not testing the ambiguous case: %d positions",
			run.Inputs.StartingPositions)
	}
}
