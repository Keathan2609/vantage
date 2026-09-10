package domain

import "fmt"

// Regime hysteresis.
//
// # The failure this prevents
//
// InferRegime is a pure function of one instant's evidence, so on a noisy
// boundary it alternates. A spread oscillating either side of a threshold
// produces RISK_OFF, normal, RISK_OFF, normal on consecutive bars, and every
// alternation is a strategy set becoming valid and invalid again. The
// downstream effect is not cosmetic: a strategy that declares itself valid
// only in TRENDING is switched on and off, and the platform trades the
// threshold rather than the market.
//
// # Why the hysteresis is asymmetric
//
// Entering RISK_OFF takes effect immediately. Leaving it needs several
// consecutive clean observations.
//
// The two errors are not symmetric. Being cautious for three extra bars costs
// a missed trade. Relaxing one bar early takes a position in exactly the
// conditions the state exists to avoid. Treating those as equivalent -- which
// is what a symmetric confirmation count does -- prices a missed opportunity
// the same as a loss taken during a feed outage.
//
// # Why this is stateful and separate
//
// InferRegime stays pure and testable on one instant. The memory lives here,
// per instrument, so a tracker can be reset, inspected, and driven
// deterministically from a replay.

// RegimeTransition records one accepted change, for an operator asking why the
// regime is what it is.
type RegimeTransition struct {
	From          Regime
	To            Regime
	Confirmations int
	Immediate     bool
	Reason        string
}

// RegimeTracker applies hysteresis to a stream of assessments for ONE
// instrument.
//
// Not safe for concurrent use. The scheduler evaluates one instrument at a
// time within a run, and a mutex here would hide the fact that two writers
// would mean two competing histories.
type RegimeTracker struct {
	policy RegimePolicy

	// current is the regime in force, which is what a decision records.
	current Regime
	// candidate is a different regime awaiting confirmation, with a count.
	candidate      Regime
	candidateCount int
	// lastTransition is kept for explanation, not for logic.
	lastTransition *RegimeTransition
}

// NewRegimeTracker starts at UNKNOWN.
//
// Deliberately not at TRENDING or any tradable value: a tracker that has seen
// nothing must not licence a trade, and UNKNOWN is the label that already
// means "not characterised".
func NewRegimeTracker(policy RegimePolicy) *RegimeTracker {
	return &RegimeTracker{policy: policy, current: RegimeUnknown}
}

// Current is the regime in force.
func (t *RegimeTracker) Current() Regime { return t.current }

// Pending reports a regime awaiting confirmation and how far it has got, so an
// operator can see a change coming rather than only after it lands.
func (t *RegimeTracker) Pending() (Regime, int) { return t.candidate, t.candidateCount }

// LastTransition is the most recent accepted change, or nil.
func (t *RegimeTracker) LastTransition() *RegimeTransition { return t.lastTransition }

// Observe folds one assessment in and returns the regime now in force.
//
// The returned assessment is the caller's to record: its Regime field is
// rewritten to the regime actually IN FORCE, and a hysteresis_hold reason is
// appended when that differs from what this instant's evidence said. The
// decision must record what it acted on, not what it might have acted on.
func (t *RegimeTracker) Observe(a RegimeAssessment) RegimeAssessment {
	observed := a.Regime

	// Same as what is in force: clear any pending candidate. A candidate that
	// survived a contradicting observation would accumulate confirmations
	// across an oscillation, which is the flapping this exists to stop.
	if observed == t.current {
		t.candidate = ""
		t.candidateCount = 0
		return a
	}

	// Entering RISK_OFF is immediate.
	if observed == RegimeRiskOff {
		t.transition(observed, 1, true,
			"entering RISK_OFF takes effect immediately: the cost of being "+
				"cautious early is a missed trade, and of being cautious late "+
				"is a position taken in the conditions this state exists to avoid")
		t.candidate = ""
		t.candidateCount = 0
		a.Regime = t.current
		return a
	}

	// Every other change, including LEAVING RISK_OFF, needs confirming.
	needed := t.policy.ConfirmationsToChange
	why := fmt.Sprintf("an ordinary regime change needs %d consecutive observations", needed)
	if t.current == RegimeRiskOff {
		needed = t.policy.ConfirmationsToLeaveRiskOff
		why = fmt.Sprintf("leaving RISK_OFF needs %d consecutive clean observations", needed)
	}
	if needed < 1 {
		needed = 1
	}

	if t.candidate == observed {
		t.candidateCount++
	} else {
		t.candidate = observed
		t.candidateCount = 1
	}

	if t.candidateCount >= needed {
		t.transition(observed, t.candidateCount, false, why)
		t.candidate = ""
		t.candidateCount = 0
		a.Regime = t.current
		return a
	}

	// Held. The regime in force is unchanged, and the assessment says so.
	a.Reasons = append(a.Reasons, RegimeReason{
		Code:        ReasonHysteresisHold,
		Observed:    fmt.Sprintf("%s (%d of %d confirmations)", observed, t.candidateCount, needed),
		Threshold:   fmt.Sprintf("%d consecutive", needed),
		Contributes: false,
		Detail: fmt.Sprintf("this instant's evidence says %s; %s remains in force because %s",
			observed, t.current, why),
	})
	a.Regime = t.current
	return a
}

func (t *RegimeTracker) transition(to Regime, confirmations int, immediate bool, reason string) {
	t.lastTransition = &RegimeTransition{
		From: t.current, To: to, Confirmations: confirmations,
		Immediate: immediate, Reason: reason,
	}
	t.current = to
}

// Reset returns the tracker to its initial state.
//
// Used when a replay rewinds: a tracker carrying confirmations from a previous
// run would make the first bars of the next one depend on which run preceded
// them, and a replay whose result depends on history is not deterministic.
func (t *RegimeTracker) Reset() {
	t.current = RegimeUnknown
	t.candidate = ""
	t.candidateCount = 0
	t.lastTransition = nil
}
