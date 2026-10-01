package domain

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// Turning a correlation into a risk decision.
//
// # The exposure this closes
//
// Every existing exposure check is per instrument or per portfolio total. A
// book of gold, silver and platinum passes all of them and is one position:
// the per-instrument ceiling is respected three times over while the account
// carries triple the intended risk on a single move. Concentration is checked
// by instrument count, which counts three names and sees diversification.
//
// # Why REDUCE before REJECT
//
// The platform's standing rule is that risk may only reduce. A correlated
// proposal is usually not wrong, it is too big -- the second leg of a
// correlated pair at half the size carries the intended risk. Rejecting
// outright is reserved for correlation so high that the proposal is
// indistinguishable from adding to the existing position, where sizing it down
// would be a worse answer than not taking it.
//
// # Why UNKNOWN reduces rather than permits or refuses
//
// An unmeasurable correlation cannot mean "independent" -- that is the most
// permissive reading available and it is wrong exactly when history is thin.
// It also cannot mean "refuse", or a fresh account could never take a second
// position at all. So it takes the conservative-but-tradable path: treat the
// pair as correlated enough to reduce, never enough to reject, and say in the
// explanation that the reduction rests on an absence of evidence rather than
// on a measurement. An operator can then see the difference.

// CorrelationAction is what the risk engine should do.
type CorrelationAction string

const (
	// CorrelationAllow means the proposal stands unchanged.
	CorrelationAllow CorrelationAction = "ALLOW"
	// CorrelationReduce means the size should be scaled down.
	CorrelationReduce CorrelationAction = "REDUCE"
	// CorrelationReject means the proposal is effectively adding to an
	// existing position and should not be taken.
	CorrelationReject CorrelationAction = "REJECT"
)

// CorrelationRiskPolicy is the versioned threshold set.
type CorrelationRiskPolicy struct {
	Version string
	// ReduceAbove is the absolute coefficient at which a correlated proposal
	// is scaled down.
	ReduceAbove decimal.Decimal
	// RejectAbove is the absolute coefficient at which it is refused.
	RejectAbove decimal.Decimal
	// ReductionFactor multiplies the requested size in the REDUCE case.
	ReductionFactor decimal.Decimal
	// UnknownReductionFactor applies when the correlation cannot be measured.
	// Milder than ReductionFactor: the platform is being careful, not
	// asserting that the instruments move together.
	UnknownReductionFactor decimal.Decimal
	// MinCorrelatedExposure is the existing exposure below which correlation
	// is not worth considering. Without a floor, the first small position in
	// an account would start reducing the second.
	MinCorrelatedExposure decimal.Decimal
	// TreatStaleAsKnown decides whether an old measurement still counts. A
	// stale +0.9 is real evidence that two instruments move together, so the
	// default is yes, and the explanation records that it was stale.
	TreatStaleAsKnown bool
	// UseNegativeCorrelation decides whether a strongly NEGATIVE correlation
	// is treated as a hedge and allowed.
	//
	// Default false, deliberately. A negative correlation that reverses --
	// which is what happens in a flight from risk, when correlations converge
	// towards one -- turns an apparent hedge into a doubled position. The
	// platform therefore judges on ABSOLUTE correlation unless configured
	// otherwise, and this comment is the argument for the default.
	UseNegativeCorrelation bool
}

// DefaultCorrelationRiskPolicy is the shipped configuration.
func DefaultCorrelationRiskPolicy() CorrelationRiskPolicy {
	return CorrelationRiskPolicy{
		Version:                "correlation-risk-v1",
		ReduceAbove:            decimal.RequireFromString("0.60"),
		RejectAbove:            decimal.RequireFromString("0.90"),
		ReductionFactor:        decimal.RequireFromString("0.50"),
		UnknownReductionFactor: decimal.RequireFromString("0.75"),
		MinCorrelatedExposure:  decimal.RequireFromString("100"),
		TreatStaleAsKnown:      true,
		UseNegativeCorrelation: false,
	}
}

// CorrelatedExposure is one existing position the proposal might duplicate.
type CorrelatedExposure struct {
	InstrumentID string
	// Exposure is the notional in the account's currency, always positive.
	Exposure decimal.Decimal
	// Side matters only when negative correlation is being credited: a short
	// gold position against a long silver one is not additive.
	Side OrderSide
}

// CorrelationVerdict is the decision and its account of itself.
type CorrelationVerdict struct {
	Action        CorrelationAction
	PolicyVersion string
	// ScaleFactor is what the requested quantity should be multiplied by. One
	// for ALLOW, less than one for REDUCE, zero for REJECT.
	ScaleFactor decimal.Decimal
	// Driver is the exposure that caused the verdict, if any.
	Driver *CorrelatedExposure
	// Correlation is the measurement, or the absence of one.
	Correlation Correlation
	Explanation string
}

// AssessCorrelationRisk decides what to do about a proposal against a book. A
// pure function.
//
// The strongest single relationship decides, not an average. Averaging would
// let a book of one highly correlated instrument and four unrelated ones look
// diversified, when the risk is entirely in the first pair.
func AssessCorrelationRisk(proposed string, side OrderSide,
	book []CorrelatedExposure, matrix *CorrelationMatrix,
	policy CorrelationRiskPolicy) CorrelationVerdict {

	verdict := CorrelationVerdict{
		Action:        CorrelationAllow,
		PolicyVersion: policy.Version,
		ScaleFactor:   decimal.NewFromInt(1),
		Explanation:   "no existing exposure is correlated with this proposal",
	}
	if matrix == nil {
		verdict.Explanation = "no correlation matrix was available, so no correlation " +
			"adjustment was applied; this is a gap and not a measurement"
		return verdict
	}

	var (
		worst      *CorrelatedExposure
		worstCorr  Correlation
		worstScore decimal.Decimal
	)
	for i := range book {
		e := book[i]
		if e.InstrumentID == proposed {
			// The same instrument is handled by the per-instrument exposure
			// ceiling. Counting it here would apply two different rules to one
			// fact.
			continue
		}
		if e.Exposure.LessThan(policy.MinCorrelatedExposure) {
			continue
		}
		c := matrix.Get(proposed, e.InstrumentID)

		score, ok := correlationScore(c, side, e, policy)
		if !ok {
			continue
		}
		if worst == nil || score.GreaterThan(worstScore) {
			ex := e
			worst, worstCorr, worstScore = &ex, c, score
		}
	}

	if worst == nil {
		return verdict
	}
	verdict.Driver = worst
	verdict.Correlation = worstCorr

	measured := worstCorr.Usable() &&
		(worstCorr.State == CorrelationKnown || policy.TreatStaleAsKnown)

	switch {
	case measured && worstScore.GreaterThanOrEqual(policy.RejectAbove):
		verdict.Action = CorrelationReject
		verdict.ScaleFactor = decimal.Zero
		verdict.Explanation = fmt.Sprintf(
			"refused: %s is correlated with the existing %s exposure of %s at %s, "+
				"which is at or above the rejection threshold of %s, so sizing this down "+
				"would still be adding to that position",
			proposed, worst.InstrumentID, worst.Exposure.StringFixed(2),
			worstScore.StringFixed(3), policy.RejectAbove.StringFixed(2))

	case measured && worstScore.GreaterThanOrEqual(policy.ReduceAbove):
		verdict.Action = CorrelationReduce
		verdict.ScaleFactor = policy.ReductionFactor
		verdict.Explanation = fmt.Sprintf(
			"reduced to %s of the requested size: %s is correlated with the existing "+
				"%s exposure of %s at %s, above the reduction threshold of %s",
			policy.ReductionFactor.StringFixed(2), proposed, worst.InstrumentID,
			worst.Exposure.StringFixed(2), worstScore.StringFixed(3),
			policy.ReduceAbove.StringFixed(2))

	case !measured:
		verdict.Action = CorrelationReduce
		verdict.ScaleFactor = policy.UnknownReductionFactor
		verdict.Explanation = fmt.Sprintf(
			"reduced to %s of the requested size because the correlation between %s "+
				"and the existing %s exposure of %s CANNOT BE MEASURED (%s). This "+
				"reduction rests on an absence of evidence, not on a measurement: "+
				"treating an unmeasured pair as independent would be the most "+
				"permissive reading available and is wrong exactly when history is thin",
			policy.UnknownReductionFactor.StringFixed(2), proposed,
			worst.InstrumentID, worst.Exposure.StringFixed(2), worstCorr.State)

	default:
		verdict.Explanation = fmt.Sprintf(
			"allowed: the strongest relationship is with %s at %s, below the "+
				"reduction threshold of %s",
			worst.InstrumentID, worstScore.StringFixed(3),
			policy.ReduceAbove.StringFixed(2))
	}

	if worstCorr.State == CorrelationStale && measured {
		verdict.Explanation += " (the measurement is stale; an old correlation is " +
			"still evidence that two instruments move together)"
	}
	return verdict
}

// correlationScore reduces a pair to the number the thresholds are compared
// against, and reports whether it should be considered at all.
//
// Absolute by default. A negative correlation looks like a hedge and behaves
// like one until a flight from risk pulls every correlation towards one, at
// which point the apparent hedge is a doubled position. Crediting it is
// therefore opt-in.
func correlationScore(c Correlation, side OrderSide, e CorrelatedExposure,
	policy CorrelationRiskPolicy) (decimal.Decimal, bool) {

	if !c.Usable() {
		// Unmeasurable pairs are still considered: that is the whole point of
		// not treating UNKNOWN as zero. The score is meaningless, so the
		// caller distinguishes on state rather than magnitude.
		return decimal.Zero, true
	}
	coefficient := c.Coefficient

	if policy.UseNegativeCorrelation {
		// A negatively correlated pair taken on OPPOSITE sides is additive
		// risk; on the same side it is a hedge. Same-sign logic, spelled out
		// rather than left to the reader.
		sameSide := side == e.Side
		if (coefficient.IsNegative() && sameSide) ||
			(coefficient.IsPositive() && !sameSide) {
			return decimal.Zero, true
		}
	}
	return coefficient.Abs(), true
}
