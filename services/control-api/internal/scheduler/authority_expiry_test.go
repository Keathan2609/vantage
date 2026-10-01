package scheduler

import (
	"testing"
	"time"
)

// The horizons must be ordered narrowest-first, because the warning reports the
// FIRST one it matches.
//
// Unsorted, a thirty-day horizon declared before a one-day horizon would match
// first with one day left, and the operator would be told "expires in 30 days"
// on the day it expires.
func TestExpiryHorizonsAreNarrowestFirst(t *testing.T) {
	got := orderedHorizons()
	if len(got) == 0 {
		t.Fatal("no expiry horizons are declared, so nothing would ever warn")
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("horizons are not strictly widening at %d: %v", i, got)
		}
	}
	if got[0] > 48*time.Hour {
		t.Errorf("the narrowest warning is %v out; an operator gets no last call", got[0])
	}
}

// A declaration written out of order must still produce an ordered result —
// which is the only reason orderedHorizons sorts rather than trusting the slice.
func TestHorizonOrderDoesNotDependOnDeclarationOrder(t *testing.T) {
	original := authorityExpiryHorizons
	t.Cleanup(func() { authorityExpiryHorizons = original })

	authorityExpiryHorizons = []time.Duration{
		30 * 24 * time.Hour,
		time.Hour,
		7 * 24 * time.Hour,
	}
	if got := orderedHorizons(); got[0] != time.Hour {
		t.Fatalf("narrowest horizon is %v, want 1h; a one-hour warning declared "+
			"last would otherwise never be the one reported", got[0])
	}
}
