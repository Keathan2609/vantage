package domain

import (
	"fmt"
	"sort"
	"strings"

	"github.com/shopspring/decimal"
)

// Regime inference, including the RISK_OFF state nothing could previously
// detect.
//
// # Why this is here and not in the research plane
//
// The research plane classifies the SHAPE of a market -- trending, ranging,
// compressed, volatile -- from price history, and it should, because that is a
// statistical question about bars. RISK_OFF is not that. It is a statement
// that the conditions for taking a directional position have broken down, and
// the evidence for it is spread, feed health, event proximity, provider
// stability and the account's own drawdown. All of that lives on this side of
// the boundary, and most of it never reaches the research plane at all.
//
// It also has to fail closed, and a refusal belongs where refusals are made.
//
// # Why every verdict carries its reasons
//
// A regime that appears without an account of itself is a black box in the
// middle of the decision path, and an operator asked to trust "RISK_OFF" with
// no reasons will eventually turn it off. Each reason names the check, what
// was observed and what the threshold was, so a verdict can be argued with.
//
// # Why the hysteresis is asymmetric
//
// Entering RISK_OFF is protective and takes effect immediately. Leaving it
// requires several consecutive clean observations. That asymmetry is
// deliberate: the cost of being cautious for three extra bars is a missed
// trade, and the cost of relaxing one bar early is a position taken in exactly
// the conditions the state exists to avoid. Symmetric hysteresis would treat
// those as equivalent.

// RegimeReasonCode identifies one component of a regime verdict.
type RegimeReasonCode string

const (
	// ReasonExtremeVolatility is a spread- or range-based measure far outside
	// the instrument's own recent behaviour.
	ReasonExtremeVolatility RegimeReasonCode = "extreme_volatility"
	// ReasonAbnormalSpread is a book wide enough that the cost of entry
	// dominates the expected edge.
	ReasonAbnormalSpread RegimeReasonCode = "abnormal_spread"
	// ReasonMarketDataDegraded is a feed that is not fully healthy.
	ReasonMarketDataDegraded RegimeReasonCode = "market_data_degraded"
	// ReasonEventRisk is a high-impact release inside its blackout window.
	ReasonEventRisk RegimeReasonCode = "event_risk"
	// ReasonPortfolioDrawdown is the account's own drawdown from peak equity.
	ReasonPortfolioDrawdown RegimeReasonCode = "portfolio_drawdown"
	// ReasonProviderInstability is a feed that keeps reconnecting: each
	// individual quote may look fine while the source is unreliable.
	ReasonProviderInstability RegimeReasonCode = "provider_instability"
	// ReasonReported is the research plane's own classification, carried
	// through when nothing above overrides it.
	ReasonReported RegimeReasonCode = "reported_by_research"
	// ReasonInsufficientEvidence is why UNKNOWN was returned.
	ReasonInsufficientEvidence RegimeReasonCode = "insufficient_evidence"
	// ReasonHysteresisHold means the verdict was held at its previous value
	// because a change had not been confirmed enough times.
	ReasonHysteresisHold RegimeReasonCode = "hysteresis_hold"
)

// RegimeReason is one component of a verdict, with the numbers behind it.
type RegimeReason struct {
	Code RegimeReasonCode
	// Observed and Threshold are strings because they are heterogeneous: a
	// duration, a fraction, a state name. They are for a human reading the
	// decision, and a typed union would buy nothing.
	Observed  string
	Threshold string
	// Contributes reports whether this reason pushed towards RISK_OFF. A
	// reason that was checked and passed is still recorded -- "spread was
	// fine" is evidence, and its absence makes a verdict unfalsifiable.
	Contributes bool
	Detail      string
}

// RegimePolicy is the versioned threshold set.
//
// Versioned for the same reason the consensus policy is: a decision recorded
// under one set of thresholds cannot be compared with one taken under another
// unless both say which they used.
type RegimePolicy struct {
	Version string

	// MaxSpreadFraction above which the book is abnormal for RISK_OFF
	// purposes. Deliberately looser than the risk engine's own spread check:
	// this is "the market has broken down", not "this trade is too expensive".
	MaxSpreadFraction decimal.Decimal
	// ExtremeSpreadMultiple of the instrument's normal spread that counts on
	// its own, without needing a second reason.
	ExtremeSpreadMultiple decimal.Decimal
	// MaxDrawdownFraction of peak equity above which the account itself is the
	// risk.
	MaxDrawdownFraction decimal.Decimal
	// MaxProviderReconnects within the observation window before the feed is
	// treated as unstable.
	MaxProviderReconnects int
	// RiskOffReasonsRequired is how many contributing reasons make RISK_OFF.
	// One is too trigger-happy for the ordinary reasons; an extreme one
	// short-circuits this (see decisive()).
	RiskOffReasonsRequired int
	// ConfirmationsToLeaveRiskOff is the asymmetric half of the hysteresis.
	ConfirmationsToLeaveRiskOff int
	// ConfirmationsToChange applies to every ordinary transition, so a regime
	// does not flap between TRENDING and HIGH_VOLATILITY on one noisy bar.
	ConfirmationsToChange int
}

// DefaultRegimePolicy is the shipped threshold set.
//
// The numbers are development defaults chosen to be defensible rather than
// tuned: there is no historical study behind them and this comment is the
// honest place to say so. They are versioned so that when there is one, the
// change is visible.
func DefaultRegimePolicy() RegimePolicy {
	return RegimePolicy{
		Version: "regime-v1",
		// 0.5% of price. A gold book at 0.5% is not a market a 1%-risk trade
		// survives.
		MaxSpreadFraction: decimal.RequireFromString("0.005"),
		// Five times normal is decisive on its own: that is a venue problem or
		// a liquidity hole, not a wide quote.
		ExtremeSpreadMultiple: decimal.RequireFromString("5"),
		// 10% off peak equity. The risk engine's own limit is 15%, so this
		// engages first and reduces rather than waiting for the hard stop.
		MaxDrawdownFraction:         decimal.RequireFromString("0.10"),
		MaxProviderReconnects:       3,
		RiskOffReasonsRequired:      2,
		ConfirmationsToLeaveRiskOff: 3,
		ConfirmationsToChange:       2,
	}
}

// RegimeEvidence is everything the classifier may look at.
//
// A struct rather than a long argument list because the set will grow, and
// because a caller that forgets a field gets a zero value that the classifier
// can recognise as absent instead of a silently plausible number.
type RegimeEvidence struct {
	// Reported is the research plane's classification. UNKNOWN or empty is
	// normal and means the classifier has no shape to work from.
	Reported Regime

	// Health is the feed at decision time.
	Health MarketDataHealth
	// SpreadBaseline is the spread this instrument is expected to trade at.
	//
	// Currently the account's CONFIGURED acceptable ceiling rather than a
	// measured rolling normal, and the difference matters: the ceiling is a
	// policy number, so a multiple of it detects a liquidity hole but not a
	// book that has quietly doubled from its own recent behaviour. A measured
	// baseline needs rolling spread history the platform does not yet keep.
	// Zero means unknown, and the check then reports that it could not run
	// rather than passing.
	SpreadBaseline decimal.Decimal

	// Blackout reports a high-impact release inside its window, with the name
	// for the reason text.
	Blackout  bool
	EventName string

	// DrawdownFraction is the account's drawdown from peak equity.
	DrawdownFraction decimal.Decimal

	// ProviderReconnects is how many times the feed has reconnected in the
	// observation window.
	ProviderReconnects int

	// HasQuote and BarsAvailable establish whether there is enough to say
	// anything at all. Without them UNKNOWN and "everything looks fine" are
	// the same answer.
	HasQuote      bool
	BarsAvailable int
}

// MinBarsForRegime is the least history that supports any verdict other than
// UNKNOWN.
//
// Below it the classifier refuses rather than guessing. A freshly seeded
// database with four bars must not be characterised as LOW_VOLATILITY merely
// because nothing has moved yet.
const MinBarsForRegime = 30

// RegimeAssessment is a verdict with its evidence.
type RegimeAssessment struct {
	Regime        Regime
	PolicyVersion string
	Reasons       []RegimeReason
	// Contributing is how many reasons pushed towards RISK_OFF.
	Contributing int
	// Decisive names a single reason severe enough to stand alone, if any.
	Decisive RegimeReasonCode
}

// Explain renders the verdict as sentences, for a decision-details view and
// for an audit record.
func (a RegimeAssessment) Explain() []string {
	out := make([]string, 0, len(a.Reasons))
	for _, r := range a.Reasons {
		mark := "ok"
		if r.Contributes {
			mark = "CONTRIBUTES"
		}
		line := fmt.Sprintf("%s [%s]: observed %s", r.Code, mark, r.Observed)
		if r.Threshold != "" {
			line += fmt.Sprintf(", threshold %s", r.Threshold)
		}
		if r.Detail != "" {
			line += ": " + r.Detail
		}
		out = append(out, line)
	}
	return out
}

// InferRegime classifies the market from evidence. A pure function.
//
// The order matters and is deliberate:
//
//  1. Not enough evidence at all -> UNKNOWN. Never guess.
//  2. RISK_OFF, if the conditions for holding a directional view have broken
//     down. This outranks EVENT_RISK because a release during a feed outage is
//     not merely an event.
//  3. EVENT_RISK, which is temporary and specific.
//  4. Whatever the research plane reported, which is a statement about shape
//     and only applies once the conditions above are absent.
func InferRegime(ev RegimeEvidence, policy RegimePolicy) RegimeAssessment {
	a := RegimeAssessment{PolicyVersion: policy.Version}

	// ---- 1. Is there anything to reason about? -----------------------------
	if !ev.HasQuote || ev.BarsAvailable < MinBarsForRegime {
		a.Regime = RegimeUnknown
		a.Reasons = append(a.Reasons, RegimeReason{
			Code:      ReasonInsufficientEvidence,
			Observed:  fmt.Sprintf("quote=%t bars=%d", ev.HasQuote, ev.BarsAvailable),
			Threshold: fmt.Sprintf("quote=true bars>=%d", MinBarsForRegime),
			// Not "contributing": UNKNOWN is not a step towards RISK_OFF, it is
			// a refusal to characterise. Conflating them would make a fresh
			// database look like a crisis.
			Contributes: false,
			Detail:      "too little history to characterise this market",
		})
		return a
	}

	// ---- 2. The RISK_OFF components ---------------------------------------
	//
	// Each is checked and RECORDED whether or not it fires. A verdict that
	// lists only its triggers cannot be argued with.

	// Feed health.
	healthy := ev.Health.State == DataQualityOK
	a.Reasons = append(a.Reasons, RegimeReason{
		Code:        ReasonMarketDataDegraded,
		Observed:    string(ev.Health.State) + issueSuffix(ev.Health.Issues),
		Threshold:   string(DataQualityOK),
		Contributes: !healthy,
		Detail:      "a feed that is not fully healthy cannot support a directional view",
	})

	// Spread, absolute.
	spreadWide := ev.Health.SpreadPct.GreaterThan(policy.MaxSpreadFraction)
	a.Reasons = append(a.Reasons, RegimeReason{
		Code:        ReasonAbnormalSpread,
		Observed:    ev.Health.SpreadPct.StringFixed(6),
		Threshold:   policy.MaxSpreadFraction.StringFixed(6),
		Contributes: spreadWide,
		Detail:      "cost of entry against the expected edge",
	})

	// Spread, relative to this instrument's own normal. A separate reason
	// because a 0.4% spread is ordinary for one instrument and a liquidity
	// hole for another.
	extremeSpread := false
	if ev.SpreadBaseline.IsPositive() {
		limit := ev.SpreadBaseline.Mul(policy.ExtremeSpreadMultiple)
		extremeSpread = ev.Health.SpreadPct.GreaterThan(limit)
		a.Reasons = append(a.Reasons, RegimeReason{
			Code:        ReasonExtremeVolatility,
			Observed:    ev.Health.SpreadPct.StringFixed(6),
			Threshold:   limit.StringFixed(6),
			Contributes: extremeSpread,
			Detail: fmt.Sprintf("%s× this instrument's expected spread of %s",
				policy.ExtremeSpreadMultiple.String(),
				ev.SpreadBaseline.StringFixed(6)),
		})
	} else {
		// Recorded as unmeasurable rather than omitted, so the absence of the
		// check is visible.
		a.Reasons = append(a.Reasons, RegimeReason{
			Code:        ReasonExtremeVolatility,
			Observed:    "no expected spread for this instrument",
			Contributes: false,
			Detail:      "cannot judge a multiple without a normal",
		})
	}

	// Event proximity.
	a.Reasons = append(a.Reasons, RegimeReason{
		Code:        ReasonEventRisk,
		Observed:    fmt.Sprintf("blackout=%t", ev.Blackout),
		Threshold:   "blackout=false",
		Contributes: ev.Blackout,
		Detail:      eventDetail(ev),
	})

	// The account's own state.
	drawdownDeep := ev.DrawdownFraction.GreaterThan(policy.MaxDrawdownFraction)
	a.Reasons = append(a.Reasons, RegimeReason{
		Code:        ReasonPortfolioDrawdown,
		Observed:    ev.DrawdownFraction.StringFixed(4),
		Threshold:   policy.MaxDrawdownFraction.StringFixed(4),
		Contributes: drawdownDeep,
		Detail:      "drawdown from peak equity: the account itself becomes the risk",
	})

	// Provider stability.
	unstable := ev.ProviderReconnects > policy.MaxProviderReconnects
	a.Reasons = append(a.Reasons, RegimeReason{
		Code:        ReasonProviderInstability,
		Observed:    fmt.Sprintf("%d reconnects", ev.ProviderReconnects),
		Threshold:   fmt.Sprintf("<=%d", policy.MaxProviderReconnects),
		Contributes: unstable,
		Detail:      "each quote may look fine while the source is unreliable",
	})

	for _, r := range a.Reasons {
		if r.Contributes {
			a.Contributing++
		}
	}

	// A single reason severe enough to stand alone. Requiring two would mean a
	// feed reporting `invalid`, or a spread five times normal, was survivable
	// on its own.
	switch {
	case ev.Health.State == DataQualityInvalid || ev.Health.State == DataQualityNoData:
		a.Decisive = ReasonMarketDataDegraded
	case extremeSpread:
		a.Decisive = ReasonExtremeVolatility
	}

	if a.Decisive != "" || a.Contributing >= policy.RiskOffReasonsRequired {
		a.Regime = RegimeRiskOff
		return a
	}

	// ---- 3. Event risk, once nothing worse applies ------------------------
	if ev.Blackout {
		a.Regime = RegimeEventRisk
		return a
	}

	// ---- 4. The research plane's shape classification ---------------------
	reported, ok := ParseRegime(string(ev.Reported))
	a.Reasons = append(a.Reasons, RegimeReason{
		Code:        ReasonReported,
		Observed:    string(ev.Reported),
		Contributes: false,
		Detail:      reportedDetail(ok, reported),
	})
	// RISK_OFF reported by a model is honoured -- a model may see something
	// this classifier cannot -- but it does not get to invent a shape when the
	// value was unparseable.
	a.Regime = reported
	return a
}

func issueSuffix(issues []DataQualityIssue) string {
	if len(issues) == 0 {
		return ""
	}
	names := make([]string, 0, len(issues))
	for _, i := range issues {
		names = append(names, string(i))
	}
	sort.Strings(names)
	return " [" + strings.Join(names, ",") + "]"
}

func eventDetail(ev RegimeEvidence) string {
	if ev.Blackout && ev.EventName != "" {
		return "inside the blackout window for " + ev.EventName
	}
	if ev.Blackout {
		return "inside a high-impact blackout window"
	}
	return "no high-impact release is close"
}

func reportedDetail(ok bool, r Regime) string {
	if !ok {
		return "the research plane reported nothing recognisable, so UNKNOWN"
	}
	return r.Describe()
}
