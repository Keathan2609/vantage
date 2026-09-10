package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// The exposure these close: every other exposure check is per instrument or
// per portfolio total, so a book of gold, silver and platinum passes all of
// them while being one position. The concentration check counts three names
// and sees diversification.

func matrixWith(pairs map[[2]string]Correlation) *CorrelationMatrix {
	m := &CorrelationMatrix{
		Policy: DefaultCorrelationPolicy(), ComputedAt: corrNow,
		pairs: map[string]Correlation{},
	}
	for k, v := range pairs {
		v.A, v.B = k[0], k[1]
		m.pairs[pairKey(k[0], k[1])] = v
	}
	return m
}

func known(coefficient string) Correlation {
	return Correlation{
		State: CorrelationKnown, Samples: 60,
		Coefficient: decimal.RequireFromString(coefficient),
		ComputedAt:  corrNow,
	}
}

func gold(exposure string) CorrelatedExposure {
	return CorrelatedExposure{
		InstrumentID: "XAUUSD.m",
		Exposure:     decimal.RequireFromString(exposure),
		Side:         SideBuy,
	}
}

func TestAnUncorrelatedProposalIsAllowedUnchanged(t *testing.T) {
	// The check must not tax ordinary diversification, or it stops being a
	// risk control and becomes a size cap.
	m := matrixWith(map[[2]string]Correlation{
		{"XAGUSD", "XAUUSD.m"}: known("0.10"),
	})
	v := AssessCorrelationRisk("XAGUSD", SideBuy,
		[]CorrelatedExposure{gold("1000")}, m, DefaultCorrelationRiskPolicy())

	if v.Action != CorrelationAllow {
		t.Fatalf("action = %s, want ALLOW: %s", v.Action, v.Explanation)
	}
	if !v.ScaleFactor.Equal(decimal.NewFromInt(1)) {
		t.Errorf("scale = %s, want 1", v.ScaleFactor)
	}
}

func TestAModeratelyCorrelatedProposalIsReducedNotRefused(t *testing.T) {
	// The milestone's example: strategy A already holds substantial
	// USD-sensitive exposure and strategy B proposes another correlated one.
	//
	// REDUCE rather than REJECT because the proposal is usually not wrong, it
	// is too big -- the second leg of a correlated pair at half the size
	// carries the intended risk. The platform's standing rule is that risk may
	// only reduce.
	m := matrixWith(map[[2]string]Correlation{
		{"XAGUSD", "XAUUSD.m"}: known("0.75"),
	})
	policy := DefaultCorrelationRiskPolicy()
	v := AssessCorrelationRisk("XAGUSD", SideBuy,
		[]CorrelatedExposure{gold("1000")}, m, policy)

	if v.Action != CorrelationReduce {
		t.Fatalf("action = %s, want REDUCE: %s", v.Action, v.Explanation)
	}
	if !v.ScaleFactor.Equal(policy.ReductionFactor) {
		t.Errorf("scale = %s, want %s", v.ScaleFactor, policy.ReductionFactor)
	}
	// The explanation has to name the driver, the coefficient and the
	// threshold, or an operator cannot tell a correlation reduction from any
	// other size change.
	for _, want := range []string{"XAUUSD.m", "0.750", "0.60", "1000.00"} {
		if !strings.Contains(v.Explanation, want) {
			t.Errorf("explanation omits %q: %s", want, v.Explanation)
		}
	}
	if v.Driver == nil || v.Driver.InstrumentID != "XAUUSD.m" {
		t.Errorf("driver = %+v, want the gold exposure", v.Driver)
	}
}

func TestAnAlmostIdenticalProposalIsRefused(t *testing.T) {
	// At 0.95 the proposal is indistinguishable from adding to the existing
	// position. Sizing it down would be a worse answer than not taking it,
	// because the account would carry the same directional risk in two names
	// and the per-instrument ceilings would both report room.
	m := matrixWith(map[[2]string]Correlation{
		{"XAGUSD", "XAUUSD.m"}: known("0.95"),
	})
	v := AssessCorrelationRisk("XAGUSD", SideBuy,
		[]CorrelatedExposure{gold("1000")}, m, DefaultCorrelationRiskPolicy())

	if v.Action != CorrelationReject {
		t.Fatalf("action = %s, want REJECT: %s", v.Action, v.Explanation)
	}
	if !v.ScaleFactor.IsZero() {
		t.Errorf("scale = %s, want 0", v.ScaleFactor)
	}
	if !strings.Contains(v.Explanation, "adding to that position") {
		t.Errorf("explanation does not say why rejection beats reduction: %s", v.Explanation)
	}
}

func TestAStronglyNegativeCorrelationIsTreatedAsRiskByDefault(t *testing.T) {
	// The judgement call, made explicitly. A negative correlation looks like a
	// hedge and behaves like one until a flight from risk pulls correlations
	// towards one, at which point the apparent hedge is a doubled position.
	//
	// So the default judges on ABSOLUTE correlation, and crediting the hedge
	// is opt-in.
	m := matrixWith(map[[2]string]Correlation{
		{"USDZAR", "XAUUSD.m"}: known("-0.95"),
	})
	policy := DefaultCorrelationRiskPolicy()

	v := AssessCorrelationRisk("USDZAR", SideBuy,
		[]CorrelatedExposure{gold("1000")}, m, policy)
	if v.Action != CorrelationReject {
		t.Fatalf("action = %s, want REJECT on absolute correlation: %s", v.Action, v.Explanation)
	}

	// Opted in, a same-side negatively correlated pair is a hedge and stands.
	policy.UseNegativeCorrelation = true
	v = AssessCorrelationRisk("USDZAR", SideBuy,
		[]CorrelatedExposure{gold("1000")}, m, policy)
	if v.Action != CorrelationAllow {
		t.Errorf("action = %s, want ALLOW once negative correlation is credited: %s",
			v.Action, v.Explanation)
	}
}

func TestAnUnmeasurableCorrelationReducesAndSaysWhy(t *testing.T) {
	// The case the whole design turns on. An unmeasured pair must not be
	// treated as independent -- that is the most permissive reading available
	// and it is wrong exactly when history is thin. It must also not refuse,
	// or a fresh account could never take a second position.
	m := matrixWith(map[[2]string]Correlation{
		{"XAGUSD", "XAUUSD.m"}: {State: CorrelationInsufficientData, Samples: 4},
	})
	policy := DefaultCorrelationRiskPolicy()
	v := AssessCorrelationRisk("XAGUSD", SideBuy,
		[]CorrelatedExposure{gold("1000")}, m, policy)

	if v.Action != CorrelationReduce {
		t.Fatalf("action = %s, want REDUCE: %s", v.Action, v.Explanation)
	}
	if !v.ScaleFactor.Equal(policy.UnknownReductionFactor) {
		t.Errorf("scale = %s, want the unknown factor %s",
			v.ScaleFactor, policy.UnknownReductionFactor)
	}
	// And the explanation must distinguish this from a measured reduction, or
	// an operator cannot tell caution from evidence.
	if !strings.Contains(v.Explanation, "CANNOT BE MEASURED") ||
		!strings.Contains(v.Explanation, "absence of evidence") {
		t.Errorf("explanation does not distinguish caution from measurement: %s", v.Explanation)
	}
}

func TestTheUnknownReductionIsMilderThanTheMeasuredOne(t *testing.T) {
	// A property, not an example. Being careful about an unmeasured pair
	// should cost less than a measured correlation does, or the platform
	// punishes missing data harder than known risk.
	policy := DefaultCorrelationRiskPolicy()
	if !policy.UnknownReductionFactor.GreaterThan(policy.ReductionFactor) {
		t.Errorf("unknown factor %s is not milder than the measured factor %s",
			policy.UnknownReductionFactor, policy.ReductionFactor)
	}
}

func TestAnUnmeasurableCorrelationIsNeverRejected(t *testing.T) {
	// The other half: absence of evidence must not be able to stop trading
	// altogether, or an account with no history is frozen.
	for _, state := range []CorrelationState{
		CorrelationInsufficientData, CorrelationUndefined,
	} {
		m := matrixWith(map[[2]string]Correlation{
			{"XAGUSD", "XAUUSD.m"}: {State: state},
		})
		v := AssessCorrelationRisk("XAGUSD", SideBuy,
			[]CorrelatedExposure{gold("100000")}, m, DefaultCorrelationRiskPolicy())
		if v.Action == CorrelationReject {
			t.Errorf("%s produced a rejection: %s", state, v.Explanation)
		}
	}
}

func TestAStaleMeasurementStillCounts(t *testing.T) {
	// A stale +0.9 is real evidence that two instruments move together.
	// Discarding it would replace a measurement with an absence, which is
	// strictly less information.
	m := matrixWith(map[[2]string]Correlation{
		{"XAGUSD", "XAUUSD.m"}: {
			State: CorrelationStale, Samples: 60,
			Coefficient: decimal.RequireFromString("0.95"),
		},
	})
	policy := DefaultCorrelationRiskPolicy()
	v := AssessCorrelationRisk("XAGUSD", SideBuy,
		[]CorrelatedExposure{gold("1000")}, m, policy)

	if v.Action != CorrelationReject {
		t.Fatalf("action = %s, want REJECT on a stale 0.95: %s", v.Action, v.Explanation)
	}
	if !strings.Contains(v.Explanation, "stale") {
		t.Errorf("the explanation does not disclose the staleness: %s", v.Explanation)
	}

	// And when configured not to trust stale data, it becomes the unknown
	// path -- reduced, not refused, and labelled as caution.
	policy.TreatStaleAsKnown = false
	v = AssessCorrelationRisk("XAGUSD", SideBuy,
		[]CorrelatedExposure{gold("1000")}, m, policy)
	if v.Action != CorrelationReduce {
		t.Errorf("action = %s, want REDUCE when stale data is distrusted", v.Action)
	}
}

func TestSmallExistingExposureIsIgnored(t *testing.T) {
	// Without a floor, the first tiny position in an account would start
	// shrinking the second, and the control would express itself as a
	// mysterious halving of every early trade.
	m := matrixWith(map[[2]string]Correlation{
		{"XAGUSD", "XAUUSD.m"}: known("0.99"),
	})
	v := AssessCorrelationRisk("XAGUSD", SideBuy,
		[]CorrelatedExposure{gold("5")}, m, DefaultCorrelationRiskPolicy())

	if v.Action != CorrelationAllow {
		t.Errorf("action = %s, want ALLOW below the exposure floor: %s", v.Action, v.Explanation)
	}
}

func TestTheSameInstrumentIsLeftToTheExposureCeiling(t *testing.T) {
	// Adding to an existing gold position is governed by the per-instrument
	// exposure limit. Applying correlation as well would apply two different rules
	// to one fact and refuse every scale-in.
	m := matrixWith(map[[2]string]Correlation{})
	v := AssessCorrelationRisk("XAUUSD.m", SideBuy,
		[]CorrelatedExposure{gold("1000")}, m, DefaultCorrelationRiskPolicy())

	if v.Action != CorrelationAllow {
		t.Errorf("action = %s, want ALLOW: the same instrument is not a correlation "+
			"question: %s", v.Action, v.Explanation)
	}
}

func TestTheStrongestRelationshipDecidesRatherThanAnAverage(t *testing.T) {
	// Averaging would let a book of one highly correlated instrument and four
	// unrelated ones look diversified, when the risk is entirely in the first
	// pair.
	m := matrixWith(map[[2]string]Correlation{
		{"XAGUSD", "XAUUSD.m"}: known("0.96"),
		{"XAGUSD", "EURUSD"}:   known("0.02"),
		{"XAGUSD", "USDJPY"}:   known("0.01"),
		{"XAGUSD", "GBPUSD"}:   known("0.03"),
	})
	book := []CorrelatedExposure{
		gold("1000"),
		{InstrumentID: "EURUSD", Exposure: decimal.RequireFromString("1000"), Side: SideBuy},
		{InstrumentID: "USDJPY", Exposure: decimal.RequireFromString("1000"), Side: SideBuy},
		{InstrumentID: "GBPUSD", Exposure: decimal.RequireFromString("1000"), Side: SideBuy},
	}
	v := AssessCorrelationRisk("XAGUSD", SideBuy, book, m, DefaultCorrelationRiskPolicy())

	if v.Action != CorrelationReject {
		t.Fatalf("action = %s, want REJECT: one pair at 0.96 among three unrelated "+
			"ones is still one position: %s", v.Action, v.Explanation)
	}
	if v.Driver == nil || v.Driver.InstrumentID != "XAUUSD.m" {
		t.Errorf("driver = %+v, want the correlated pair", v.Driver)
	}
}

func TestAMissingMatrixIsRecordedAsAGapRatherThanAnAllowance(t *testing.T) {
	// If correlation cannot be assessed at all, the verdict must say so.
	// Reporting a plain ALLOW would look like a measurement that found nothing
	// wrong.
	v := AssessCorrelationRisk("XAGUSD", SideBuy,
		[]CorrelatedExposure{gold("1000")}, nil, DefaultCorrelationRiskPolicy())

	if v.Action != CorrelationAllow {
		t.Errorf("action = %s; with no matrix there is nothing to act on", v.Action)
	}
	if !strings.Contains(v.Explanation, "gap and not a measurement") {
		t.Errorf("explanation does not disclose the gap: %s", v.Explanation)
	}
}

func TestTheVerdictCarriesItsPolicyVersion(t *testing.T) {
	v := AssessCorrelationRisk("XAGUSD", SideBuy, nil,
		matrixWith(nil), DefaultCorrelationRiskPolicy())
	if v.PolicyVersion != "correlation-risk-v1" {
		t.Errorf("policy version = %q", v.PolicyVersion)
	}
}

func TestRejectionIsStricterThanReduction(t *testing.T) {
	// A property. If the thresholds were ever inverted, every reduction would
	// become a rejection and the "risk may only reduce" rule would be broken
	// by configuration rather than by code.
	policy := DefaultCorrelationRiskPolicy()
	if !policy.RejectAbove.GreaterThan(policy.ReduceAbove) {
		t.Errorf("reject threshold %s is not above the reduce threshold %s",
			policy.RejectAbove, policy.ReduceAbove)
	}
	if policy.ReductionFactor.GreaterThanOrEqual(decimal.NewFromInt(1)) {
		t.Errorf("reduction factor %s does not reduce", policy.ReductionFactor)
	}
	if !policy.ReductionFactor.IsPositive() {
		t.Errorf("reduction factor %s is not positive, so REDUCE is a silent REJECT",
			policy.ReductionFactor)
	}
}

func TestCorrelationPolicyLookbackIsLongerThanItsMaxAge(t *testing.T) {
	// A measurement window shorter than the staleness allowance would make
	// every fresh measurement stale on arrival.
	p := DefaultCorrelationPolicy()
	if p.Lookback <= p.MaxAge {
		t.Errorf("lookback %s is not longer than max age %s", p.Lookback, p.MaxAge)
	}
	if p.MinSamples < 2 {
		t.Errorf("min samples = %d; correlation needs at least two observations", p.MinSamples)
	}
	if p.MaxAge <= time.Duration(0) {
		t.Error("max age is not positive, so nothing is ever stale")
	}
}
