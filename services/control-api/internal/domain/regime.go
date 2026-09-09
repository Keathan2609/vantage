package domain

import "strings"

// Regime is the market condition a decision was taken in.
//
// # Why this exists in the control plane at all
//
// The regime is inferred by the research plane, so it arrived here as a bare
// string and was stored, displayed and compared as one. That worked until
// something had to reason about it: a consensus policy that discards a
// strategy whose declared regimes exclude the current one cannot be trusted
// when "TRENDING", "trending" and "Trend" are three different values, and a
// typo silently means "no strategy is valid here".
//
// So the canonical list lives here, on the side of the boundary that makes
// refusals, and a value that is not one of these is UNKNOWN rather than
// accepted.
type Regime string

const (
	// RegimeTrending is a directional market: a trend strategy's home.
	RegimeTrending Regime = "TRENDING"
	// RegimeRanging is a mean-reverting market with no sustained direction.
	RegimeRanging Regime = "RANGING"
	// RegimeHighVolatility is a market moving far enough that ordinary stop
	// distances are noise. Position sizing, not direction, is the first
	// question here.
	RegimeHighVolatility Regime = "HIGH_VOLATILITY"
	// RegimeLowVolatility is a compressed market. Breakout strategies wait in
	// it; trend strategies find nothing to follow.
	RegimeLowVolatility Regime = "LOW_VOLATILITY"
	// RegimeEventRisk means a high-impact release is close enough that price
	// behaviour before it does not predict behaviour after it.
	RegimeEventRisk Regime = "EVENT_RISK"
	// RegimeRiskOff is a broad flight from risk, where correlations converge
	// and instrument-level analysis stops being independent.
	//
	// NOTE: no classifier currently INFERS this. It is reachable only from a
	// model that reports it (see orchestrator.ModelOpinion) or from an
	// operator. That is recorded rather than hidden, because a regime the
	// system can name but never detect is a gap, not a feature.
	RegimeRiskOff Regime = "RISK_OFF"
	// RegimeUnknown is a real answer, and the most important one.
	//
	// It is returned when the evidence is thin -- too few bars, an indicator
	// that has not warmed up, measures that contradict each other. It must
	// never be coerced into another label to let a strategy run: a strategy
	// that declares itself valid only in trends should not run in a market
	// nobody can characterise, and forcing a label is how that happens.
	RegimeUnknown Regime = "UNKNOWN"
)

// AllRegimes is every regime, for validation and for the API's
// self-description.
func AllRegimes() []Regime {
	return []Regime{
		RegimeTrending,
		RegimeRanging,
		RegimeHighVolatility,
		RegimeLowVolatility,
		RegimeEventRisk,
		RegimeRiskOff,
		RegimeUnknown,
	}
}

// ParseRegime normalises a value from the research plane.
//
// An unrecognised value becomes UNKNOWN and reports false, rather than being
// passed through. Fail closed: a regime nobody recognises is precisely a
// market nobody can characterise, so the honest label is the one that already
// means that.
func ParseRegime(raw string) (Regime, bool) {
	normalised := Regime(strings.ToUpper(strings.TrimSpace(raw)))
	for _, r := range AllRegimes() {
		if normalised == r {
			return r, true
		}
	}
	return RegimeUnknown, false
}

// Known reports whether the market has actually been characterised.
func (r Regime) Known() bool { return r != RegimeUnknown && r != "" }

// Tradable reports whether the regime alone permits an automated directional
// position.
//
// EVENT_RISK and RISK_OFF are excluded for different reasons. EVENT_RISK is
// temporary and specific: the release resolves it. RISK_OFF is a statement
// that instrument-level analysis has stopped being independent, which no
// single-instrument strategy is equipped to handle. UNKNOWN is excluded
// because it is an absence of information, and the platform's posture is that
// an absence of information is not a licence.
//
// This is one input among many, not the decision. A tradable regime does not
// make a trade permissible -- risk, authority and the consensus policy all
// still apply.
func (r Regime) Tradable() bool {
	switch r {
	case RegimeTrending, RegimeRanging, RegimeHighVolatility, RegimeLowVolatility:
		return true
	default:
		return false
	}
}

// Describe explains the regime in one sentence, for an operator reading a
// decision after the fact.
func (r Regime) Describe() string {
	switch r {
	case RegimeTrending:
		return "directional: price is sustaining a move"
	case RegimeRanging:
		return "range-bound: price is reverting rather than trending"
	case RegimeHighVolatility:
		return "volatile: ranges are wide relative to their own recent history"
	case RegimeLowVolatility:
		return "compressed: ranges are narrow relative to their own recent history"
	case RegimeEventRisk:
		return "event risk: a high-impact release is close enough to dominate price"
	case RegimeRiskOff:
		return "risk-off: a broad flight from risk, in which correlations converge"
	default:
		return "unknown: the evidence does not characterise this market"
	}
}
