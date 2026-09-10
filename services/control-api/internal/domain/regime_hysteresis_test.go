package domain

import (
	"strings"
	"testing"
)

// Hysteresis exists because InferRegime is a pure function of one instant, so
// on a noisy boundary it alternates -- and every alternation switches a
// strategy set on and off. These tests drive the two transitions the milestone
// names, with the boundary deliberately noisy.

func assess(r Regime) RegimeAssessment {
	return RegimeAssessment{Regime: r, PolicyVersion: "test"}
}

// feed pushes a sequence of raw verdicts through a tracker and returns the
// regime in force after each one. The in-force sequence is the thing that
// matters: it is what decisions are recorded against.
func feed(t *RegimeTracker, raw ...Regime) []Regime {
	out := make([]Regime, 0, len(raw))
	for _, r := range raw {
		out = append(out, t.Observe(assess(r)).Regime)
	}
	return out
}

func TestATrackerStartsUnknownAndNotTradable(t *testing.T) {
	// A tracker that has seen nothing must not licence a trade. Starting at
	// TRENDING would make the first bar after every restart tradable on no
	// evidence at all.
	tr := NewRegimeTracker(DefaultRegimePolicy())
	if tr.Current() != RegimeUnknown {
		t.Fatalf("initial regime = %s, want UNKNOWN", tr.Current())
	}
	if tr.Current().Tradable() {
		t.Error("a fresh tracker is tradable")
	}
}

func TestAnOrdinaryChangeNeedsConfirming(t *testing.T) {
	// TRENDING -> HIGH_VOLATILITY, the milestone's first named transition,
	// with a single contradicting observation in the middle. The change must
	// not land until the evidence is consistent.
	policy := DefaultRegimePolicy() // ConfirmationsToChange: 2
	tr := NewRegimeTracker(policy)

	// Settle into TRENDING first.
	feed(tr, RegimeTrending, RegimeTrending)
	if tr.Current() != RegimeTrending {
		t.Fatalf("did not settle into TRENDING, got %s", tr.Current())
	}

	// One volatile bar is not a regime change.
	got := feed(tr, RegimeHighVolatility)
	if got[0] != RegimeTrending {
		t.Errorf("one volatile observation changed the regime to %s", got[0])
	}
	if pending, n := tr.Pending(); pending != RegimeHighVolatility || n != 1 {
		t.Errorf("pending = %s (%d), want HIGH_VOLATILITY (1)", pending, n)
	}

	// A contradicting bar resets the count. Without this, an oscillation
	// accumulates confirmations across observations that disagree, which is
	// exactly the flapping the mechanism exists to stop.
	feed(tr, RegimeTrending)
	if pending, n := tr.Pending(); pending != "" || n != 0 {
		t.Errorf("a contradicting observation left pending = %s (%d)", pending, n)
	}

	// Two consecutive now carry it.
	got = feed(tr, RegimeHighVolatility, RegimeHighVolatility)
	if got[0] != RegimeTrending {
		t.Errorf("the first of two confirmations already changed the regime")
	}
	if got[1] != RegimeHighVolatility {
		t.Errorf("two confirmations did not change the regime, got %s", got[1])
	}
}

func TestAlternatingEvidenceNeverChangesTheRegime(t *testing.T) {
	// The failure this prevents, stated directly. A spread oscillating either
	// side of a threshold produced TRENDING, HIGH_VOLATILITY, TRENDING... and
	// every alternation was a strategy set becoming valid and invalid again.
	tr := NewRegimeTracker(DefaultRegimePolicy())
	feed(tr, RegimeTrending, RegimeTrending)

	inForce := feed(tr,
		RegimeHighVolatility, RegimeTrending,
		RegimeHighVolatility, RegimeTrending,
		RegimeHighVolatility, RegimeTrending,
		RegimeHighVolatility, RegimeTrending,
	)
	for i, r := range inForce {
		if r != RegimeTrending {
			t.Fatalf("observation %d flapped the regime to %s", i, r)
		}
	}
}

func TestEnteringRiskOffIsImmediate(t *testing.T) {
	// Asymmetry, half one. The cost of being cautious a bar early is a missed
	// trade; the cost of being cautious a bar late is a position taken in the
	// conditions the state exists to avoid. Waiting for confirmation prices
	// those the same.
	tr := NewRegimeTracker(DefaultRegimePolicy())
	feed(tr, RegimeTrending, RegimeTrending)

	got := feed(tr, RegimeRiskOff)
	if got[0] != RegimeRiskOff {
		t.Fatalf("RISK_OFF waited for confirmation, regime = %s", got[0])
	}
	tn := tr.LastTransition()
	if tn == nil || !tn.Immediate {
		t.Fatalf("the transition was not recorded as immediate: %+v", tn)
	}
	if tn.From != RegimeTrending || tn.To != RegimeRiskOff {
		t.Errorf("transition = %s -> %s", tn.From, tn.To)
	}
	if !strings.Contains(tn.Reason, "immediately") {
		t.Errorf("the transition carries no account of itself: %q", tn.Reason)
	}
}

func TestLeavingRiskOffIsSlowAndSurvivesANoisyBoundary(t *testing.T) {
	// Asymmetry, half two, and the milestone's second named transition:
	// RISK_OFF -> NORMAL under noise. Three consecutive clean observations are
	// required, and one dirty observation in the middle restarts the count.
	policy := DefaultRegimePolicy() // ConfirmationsToLeaveRiskOff: 3
	tr := NewRegimeTracker(policy)
	feed(tr, RegimeRiskOff)
	if tr.Current() != RegimeRiskOff {
		t.Fatalf("setup failed, regime = %s", tr.Current())
	}

	// Two clean, then a relapse. The relapse must reset, not accumulate.
	got := feed(tr, RegimeTrending, RegimeTrending)
	for i, r := range got {
		if r != RegimeRiskOff {
			t.Fatalf("clean observation %d released RISK_OFF early (%s)", i, r)
		}
	}
	feed(tr, RegimeRiskOff)
	if pending, n := tr.Pending(); pending != "" || n != 0 {
		t.Errorf("the relapse left pending = %s (%d)", pending, n)
	}

	// Two clean again is still not enough: the count restarted.
	got = feed(tr, RegimeTrending, RegimeTrending)
	if got[1] != RegimeRiskOff {
		t.Fatalf("two clean observations after a relapse released RISK_OFF (%s)", got[1])
	}
	// The third carries it.
	got = feed(tr, RegimeTrending)
	if got[0] != RegimeTrending {
		t.Fatalf("three clean observations did not release RISK_OFF, regime = %s", got[0])
	}
	tn := tr.LastTransition()
	if tn.Immediate {
		t.Error("leaving RISK_OFF was recorded as immediate")
	}
	if tn.Confirmations < policy.ConfirmationsToLeaveRiskOff {
		t.Errorf("released after %d confirmations, want %d",
			tn.Confirmations, policy.ConfirmationsToLeaveRiskOff)
	}
	if !strings.Contains(tn.Reason, "leaving RISK_OFF") {
		t.Errorf("reason = %q", tn.Reason)
	}
}

func TestLeavingRiskOffIsStrictlySlowerThanEnteringIt(t *testing.T) {
	// The asymmetry as a property rather than two separate examples. If a
	// future edit made the counts equal, both tests above would still pass on
	// their own numbers; this one would not.
	policy := DefaultRegimePolicy()
	if policy.ConfirmationsToLeaveRiskOff <= policy.ConfirmationsToChange {
		t.Errorf("leaving RISK_OFF needs %d confirmations and an ordinary change needs %d: "+
			"the protection is not asymmetric",
			policy.ConfirmationsToLeaveRiskOff, policy.ConfirmationsToChange)
	}
}

func TestAHeldRegimeSaysSoInTheDecisionItIsRecordedAgainst(t *testing.T) {
	// A decision must record what it ACTED on, not what the instant's evidence
	// happened to say. Otherwise a stored decision reads HIGH_VOLATILITY while
	// the strategy that ran was the one valid in TRENDING, and the record
	// contradicts the behaviour.
	tr := NewRegimeTracker(DefaultRegimePolicy())
	feed(tr, RegimeTrending, RegimeTrending)

	out := tr.Observe(assess(RegimeHighVolatility))
	if out.Regime != RegimeTrending {
		t.Fatalf("the assessment reports %s, but TRENDING was in force", out.Regime)
	}
	hold, ok := reasonFor(out, ReasonHysteresisHold)
	if !ok {
		t.Fatal("a held verdict carries no hysteresis reason, so the difference is invisible")
	}
	if !strings.Contains(hold.Observed, "HIGH_VOLATILITY") ||
		!strings.Contains(hold.Detail, "TRENDING remains in force") {
		t.Errorf("the hold reason does not explain itself: %+v", hold)
	}
}

func TestUnknownIsConfirmedLikeAnyOtherChange(t *testing.T) {
	// Losing the ability to characterise the market is itself a change, and a
	// single missing bar should not drop a settled regime to UNKNOWN --
	// which, being untradable, would stop trading on one bad observation.
	tr := NewRegimeTracker(DefaultRegimePolicy())
	feed(tr, RegimeRanging, RegimeRanging)

	if got := feed(tr, RegimeUnknown); got[0] != RegimeRanging {
		t.Errorf("one UNKNOWN observation dropped the regime to %s", got[0])
	}
	if got := feed(tr, RegimeUnknown); got[0] != RegimeUnknown {
		t.Errorf("two UNKNOWN observations did not take effect, regime = %s", got[0])
	}
}

func TestResetClearsTheMemorySoARewindIsDeterministic(t *testing.T) {
	// A replay that rewinds must not carry confirmations from the previous
	// pass. If it did, the first bars of a run would depend on which run
	// preceded them, and the result would not be reproducible -- which is the
	// whole claim a replay makes.
	tr := NewRegimeTracker(DefaultRegimePolicy())
	feed(tr, RegimeRiskOff, RegimeTrending, RegimeTrending)

	tr.Reset()
	if tr.Current() != RegimeUnknown {
		t.Errorf("after Reset the regime is %s, want UNKNOWN", tr.Current())
	}
	if pending, n := tr.Pending(); pending != "" || n != 0 {
		t.Errorf("after Reset pending = %s (%d)", pending, n)
	}
	if tr.LastTransition() != nil {
		t.Error("after Reset a transition from the previous run survives")
	}
}

func TestTwoTrackersWithTheSameInputAgree(t *testing.T) {
	// Determinism at the level that matters for a replay: the tracker is
	// stateful, so it has to be shown that the state depends only on the
	// observation sequence.
	seq := []Regime{
		RegimeTrending, RegimeHighVolatility, RegimeTrending, RegimeRiskOff,
		RegimeTrending, RegimeTrending, RegimeTrending, RegimeRanging,
		RegimeRanging, RegimeUnknown, RegimeUnknown, RegimeTrending,
	}
	a := feed(NewRegimeTracker(DefaultRegimePolicy()), seq...)
	b := feed(NewRegimeTracker(DefaultRegimePolicy()), seq...)

	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("step %d diverged: %s against %s", i, a[i], b[i])
		}
	}
	if len(a) != len(seq) {
		t.Fatalf("got %d verdicts for %d observations", len(a), len(seq))
	}
}
