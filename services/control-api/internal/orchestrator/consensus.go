package orchestrator

import (
	"fmt"
	"sort"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
)

// Consensus decides what to do when several strategies, a model and the risk
// context disagree.
//
// # Why this is a separate, pure decision
//
// Until now the orchestrator evaluated exactly one strategy and routed its
// signal. That is safe but it is not a trading system: it means the platform
// has no opinion about the case where a trend strategy says BUY at 0.75, a
// mean-reversion strategy says HOLD at 0.45, a regime model says RISK_OFF at
// 0.82 and a high-impact release is due. The correct answer there is NO TRADE,
// and it has to be produced by an explicit rule rather than by whichever
// strategy happened to be evaluated first.
//
// Everything here is a pure function of its input. No store, no clock, no
// adapter -- so every branch below is reachable in a unit test, which is the
// only way a policy with this many interacting rules can be trusted.
//
// # What this deliberately does NOT do
//
// **It does not take a majority vote.** Three strategies voting 2-1 for a buy
// is not a decision, it is a disagreement, and the platform's own risk posture
// is that an ambiguous edge is not an edge. Netting opposing signals into
// whichever side has more weight is how a system ends up trading its own
// indecision.
//
// **It does not let confidence outvote a veto.** A veto is a statement that
// conditions are wrong, not a vote about direction. A 0.99-confidence signal
// into a news blackout is a 0.99-confidence signal that must not be taken.
//
// **It does not treat HOLD or CLOSE as directional.** A strategy saying HOLD
// is declining to express a direction; counting it as agreement with whatever
// else is present would manufacture consensus out of abstention.

// PolicyVersion identifies the aggregation rules in force.
//
// Stored on every decision snapshot. When the rules change, old decisions stay
// interpretable: "why did it trade?" is answerable against the policy that
// actually ran, not against today's.
const PolicyVersion = 1

// ConsensusPolicy is the configurable half of the decision. The rules
// themselves are fixed; these are the thresholds they compare against.
type ConsensusPolicy struct {
	// MinConfidence discards an individual opinion below this level. A
	// strategy that is barely convinced should not contribute to a decision
	// about real money.
	MinConfidence decimal.Decimal
	// MinNetConfidence is required of the surviving direction after
	// disagreement is accounted for. It is deliberately higher than
	// MinConfidence: agreeing weakly is not the same as agreeing.
	MinNetConfidence decimal.Decimal
	// ModelVetoConfidence is the level at which a model's RISK_OFF or
	// contrary-direction call becomes a veto rather than an input.
	ModelVetoConfidence decimal.Decimal
	// MaxOpposingWeightFraction is how much weight may sit on the opposite
	// side before the disagreement itself is disqualifying. Zero means any
	// opposition is disqualifying.
	MaxOpposingWeightFraction decimal.Decimal
}

// # What these thresholds are compared AGAINST
//
// A raw score, not a probability, and the difference has consequences.
//
// Every strategy signal this platform receives carries `confidence_kind:
// raw_score`. The research plane averages hand-chosen 0-1 components, and
// three strategies average a real component with a hard-coded constant (0.5,
// 0.45, 0.6). The result is bounded in [0, 1], is not comparable BETWEEN
// strategies, and has never been fitted against realised outcomes.
//
// MinConfidence and MinNetConfidence read as confidence levels and are applied
// uniformly to those scores. `donchian_breakout` averages its breakout
// penetration with a constant 0.5 and reported 0.31-0.44 on the seeded set.
//
// An earlier version of this comment concluded that it therefore "cannot clear
// a 0.55 floor whatever the market does". That was wrong, and the correction
// matters because the two readings point at different fixes. The blend is a
// MEAN, so
//
//	confidence = (min(1, penetration) + 0.5) / 2
//
// which crosses 0.55 at a penetration of 0.6 ATR and saturates at 0.75.
// Measured, in services/quant/tests/test_confidence_reachability.py: a 0.74x
// ATR break scores 0.618, a 0.97x break scores 0.733, and no break however
// extreme exceeds 0.75. The 0.31-0.44 on the seeded set was a weak-breakout
// result, not a ceiling.
//
// What the constant actually costs is the TOP of the range, and that part is
// real: half the dynamic range is thrown away, so any threshold above 0.75 is
// unreachable for this strategy and a strong break is compressed towards a
// mediocre one. That is a calibration defect rather than an impossibility, and
// "impossible" would have invited deleting the strategy or lowering the floor,
// neither of which is the fix.
//
// The thresholds are NOT adjusted to compensate. Moving a number until trades
// appear is how a platform talks itself into a result, and the fix is
// calibration against outcomes, which is research and needs evidence. What has
// been done is to stop the category error being invisible: every decision now
// records the score kind it was taken against, so a reader can tell a decision
// weighed against a probability from one weighed against a ranking input.
//
// DefaultConsensusPolicy is deliberately strict.
//
// These are not tuned numbers and must not be presented as such. They are a
// starting posture for a platform that has never traded unattended, chosen so
// that the common outcome is NO TRADE. Loosening them is a decision that
// should be made against paper-forward evidence, not to make a demo trade.
func DefaultConsensusPolicy() ConsensusPolicy {
	return ConsensusPolicy{
		MinConfidence:             decimal.RequireFromString("0.55"),
		MinNetConfidence:          decimal.RequireFromString("0.60"),
		ModelVetoConfidence:       decimal.RequireFromString("0.65"),
		MaxOpposingWeightFraction: decimal.RequireFromString("0.20"),
	}
}

// StrategyOpinion is one strategy's contribution.
type StrategyOpinion struct {
	StrategyKey string
	Family      string
	Version     int
	Action      domain.SignalAction
	Confidence  decimal.Decimal
	// Weight scales the opinion. Two strategies from the same family are one
	// opinion counted twice, so a caller may down-weight a crowded family.
	// Zero or negative is treated as "excluded", never as "unweighted".
	Weight decimal.Decimal
	// AllowedRegimes is what the strategy declares it is valid in. Empty means
	// the strategy makes no claim, which is treated as "any" -- an absent
	// declaration is not a licence to override the regime check, it means the
	// strategy never made one.
	AllowedRegimes []string
}

// ModelOpinion is a statistical or ML model's read of conditions.
//
// Separate from a strategy opinion because a model that says RISK_OFF is not
// voting on direction: it is saying the market is not one to take a directional
// position in, which is a veto rather than a ballot.
type ModelOpinion struct {
	ModelKey   string
	Version    int
	Regime     string
	Direction  domain.SignalAction
	Confidence decimal.Decimal
	// Available is false when the model could not produce a prediction -- a
	// missing artifact, a feature that could not be computed, an inference
	// timeout. It is NOT treated as neutral: see the rule in Decide.
	Available bool
	// Required is true when this decision is supposed to be model-gated. A
	// required model that is unavailable is a veto.
	Required bool
}

// ConsensusInput is everything the decision sees.
type ConsensusInput struct {
	Policy    ConsensusPolicy
	Opinions  []StrategyOpinion
	Model     *ModelOpinion
	Regime    string
	EventRisk string // "none", "low", "medium", "high"
	// PortfolioBlocks carries refusals from portfolio context -- correlated
	// exposure, currency concentration, no risk budget. Each is a veto with
	// its own wording, supplied by the caller because portfolio state needs a
	// database and this function must not.
	PortfolioBlocks []string
	// DataBlocks carries refusals about the inputs themselves: stale quotes,
	// an abnormal spread, insufficient history. Also vetoes.
	DataBlocks []string
}

// Contribution records what one opinion did to the outcome, including being
// discarded and why. An operator asking "why did it not trade?" needs the
// discards more than the survivors.
type Contribution struct {
	StrategyKey string
	Family      string
	Action      domain.SignalAction
	Confidence  decimal.Decimal
	Weight      decimal.Decimal
	Counted     bool
	Note        string
}

// Verdict is the decision plus the whole reasoning.
type Verdict struct {
	Action        domain.SignalAction
	Confidence    decimal.Decimal
	PolicyVersion int
	// Reason is the single sentence shown to an operator.
	Reason string
	// Rationale is every rule that fired, in the order it fired. This is what
	// makes the decision explainable after the fact.
	Rationale []string
	// Vetoes are the absolute refusals. Non-empty means Action is no_trade
	// regardless of anything else.
	Vetoes        []string
	Contributions []Contribution
	// BuyWeight and SellWeight are the surviving directional weights, kept so
	// a close call is visible rather than hidden behind the verdict.
	BuyWeight  decimal.Decimal
	SellWeight decimal.Decimal
}

// Traded reports whether the verdict is actionable.
func (v Verdict) Traded() bool { return v.Action.Actionable() }

// Decide applies the aggregation policy.
//
// The order of the sections below is the policy. Vetoes are evaluated first
// and completely, before any opinion is weighed, so that no amount of
// confidence can reach past them.
func Decide(in ConsensusInput) Verdict {
	v := Verdict{
		Action:        domain.SignalNoTrade,
		PolicyVersion: PolicyVersion,
		Confidence:    decimal.Zero,
		BuyWeight:     decimal.Zero,
		SellWeight:    decimal.Zero,
	}

	// ---- 1. Vetoes ---------------------------------------------------------
	// Conditions that make trading wrong, whatever anyone thinks.

	for _, block := range in.DataBlocks {
		v.Vetoes = append(v.Vetoes, "data: "+block)
	}
	for _, block := range in.PortfolioBlocks {
		v.Vetoes = append(v.Vetoes, "portfolio: "+block)
	}

	switch strings.ToLower(in.EventRisk) {
	case "high":
		v.Vetoes = append(v.Vetoes,
			"event risk: a high-impact release is inside the blackout window")
	case "medium":
		// Medium is not a veto on its own, but it raises the bar. Recorded so
		// the tightening is visible in the rationale rather than implicit.
		v.Rationale = append(v.Rationale,
			"event risk is medium: the net-confidence requirement is raised")
	}

	if in.Model != nil {
		m := *in.Model
		switch {
		case m.Required && !m.Available:
			// A model that is required and cannot answer is the clearest case
			// in the whole policy: the decision was defined as model-gated and
			// the gate is missing.
			v.Vetoes = append(v.Vetoes, fmt.Sprintf(
				"model %s is required for this decision and produced no prediction",
				m.ModelKey))
		case m.Available && domain.Regime(m.Regime) == domain.RegimeRiskOff &&
			m.Confidence.GreaterThanOrEqual(in.Policy.ModelVetoConfidence):
			v.Vetoes = append(v.Vetoes, fmt.Sprintf(
				"model %s reads the market as RISK_OFF at %s confidence",
				m.ModelKey, m.Confidence.StringFixed(2)))
		case !m.Available:
			// Unavailable and not required: the decision proceeds without it,
			// but the absence is recorded. Treating a silent model as
			// agreement is how a broken model becomes invisible.
			v.Rationale = append(v.Rationale, fmt.Sprintf(
				"model %s was unavailable and contributed nothing", m.ModelKey))
		}
	}

	// ---- 2. Opinions -------------------------------------------------------
	// Each is admitted or discarded, with the reason kept either way.

	buy, sell := decimal.Zero, decimal.Zero
	var counted int
	for _, o := range in.Opinions {
		c := Contribution{
			StrategyKey: o.StrategyKey, Family: o.Family,
			Action: o.Action, Confidence: o.Confidence, Weight: o.Weight,
		}

		switch {
		case !o.Weight.IsPositive():
			c.Note = "excluded: weight is not positive"
		case !o.Action.Actionable():
			// HOLD, CLOSE and NO_TRADE are abstentions, not agreement.
			c.Note = fmt.Sprintf("abstained: action is %s", o.Action)
		case o.Confidence.LessThan(in.Policy.MinConfidence):
			c.Note = fmt.Sprintf("discarded: confidence %s is below the %s minimum",
				o.Confidence.StringFixed(2), in.Policy.MinConfidence.StringFixed(2))
		case !regimeAllows(o.AllowedRegimes, in.Regime):
			c.Note = fmt.Sprintf("discarded: declares itself valid in %s, and the regime is %s",
				strings.Join(o.AllowedRegimes, "/"), in.Regime)
		default:
			c.Counted = true
			c.Note = "counted"
			counted++
			contribution := o.Confidence.Mul(o.Weight)
			if side, ok := o.Action.Side(); ok && side == domain.SideBuy {
				buy = buy.Add(contribution)
			} else {
				sell = sell.Add(contribution)
			}
		}
		v.Contributions = append(v.Contributions, c)
	}

	v.BuyWeight, v.SellWeight = buy, sell

	// ---- 3. Resolve --------------------------------------------------------

	if len(v.Vetoes) > 0 {
		sort.Strings(v.Vetoes)
		v.Reason = "NO TRADE: " + v.Vetoes[0]
		if len(v.Vetoes) > 1 {
			v.Reason += fmt.Sprintf(" (and %d other refusal(s))", len(v.Vetoes)-1)
		}
		v.Rationale = append(v.Rationale, v.Vetoes...)
		return v
	}

	if counted == 0 {
		v.Reason = "NO TRADE: no strategy offered an actionable opinion that survived the policy"
		v.Rationale = append(v.Rationale, v.Reason)
		return v
	}

	total := buy.Add(sell)
	if !total.IsPositive() {
		v.Reason = "NO TRADE: the surviving opinions carry no weight"
		v.Rationale = append(v.Rationale, v.Reason)
		return v
	}

	winner, winning, losing := domain.SignalBuy, buy, sell
	if sell.GreaterThan(buy) {
		winner, winning, losing = domain.SignalSell, sell, buy
	}

	// Directional disagreement. Not resolved by majority: if a meaningful
	// share of the weight is on the other side, the strategies do not agree
	// and there is nothing to act on.
	opposingFraction := losing.Div(total)
	if opposingFraction.GreaterThan(in.Policy.MaxOpposingWeightFraction) {
		v.Reason = fmt.Sprintf(
			"NO TRADE: strategies disagree. %s%% of the surviving weight opposes the "+
				"%s side, above the %s%% the policy tolerates. A split is not a decision",
			opposingFraction.Mul(decimal.NewFromInt(100)).StringFixed(1),
			winner,
			in.Policy.MaxOpposingWeightFraction.Mul(decimal.NewFromInt(100)).StringFixed(1))
		v.Rationale = append(v.Rationale, v.Reason)
		return v
	}

	// A model pointing the other way at high confidence is a veto on the
	// direction, evaluated here rather than with the other vetoes because it
	// depends on which way the strategies came out.
	if in.Model != nil && in.Model.Available && in.Model.Direction.Actionable() {
		modelSide, _ := in.Model.Direction.Side()
		winnerSide, _ := winner.Side()
		if modelSide != winnerSide &&
			in.Model.Confidence.GreaterThanOrEqual(in.Policy.ModelVetoConfidence) {
			v.Vetoes = append(v.Vetoes, fmt.Sprintf(
				"model %s points %s at %s confidence, against the strategies' %s",
				in.Model.ModelKey, in.Model.Direction,
				in.Model.Confidence.StringFixed(2), winner))
			v.Reason = "NO TRADE: " + v.Vetoes[0]
			v.Rationale = append(v.Rationale, v.Reason)
			return v
		}
	}

	// Net confidence: the winning weight as a share of the total, scaled by
	// the strongest surviving conviction. Agreeing weakly is not agreeing.
	net := winning.Div(total).Mul(strongest(v.Contributions, winner))
	required := in.Policy.MinNetConfidence
	if strings.EqualFold(in.EventRisk, "medium") {
		// Halfway to the veto: a release that is close but not imminent.
		required = required.Add(decimal.RequireFromString("0.10"))
	}
	if net.LessThan(required) {
		v.Reason = fmt.Sprintf(
			"NO TRADE: net confidence %s is below the %s this policy requires",
			net.StringFixed(3), required.StringFixed(3))
		v.Rationale = append(v.Rationale, v.Reason)
		return v
	}

	if domain.Regime(in.Regime) == domain.RegimeUnknown {
		// Reachable only when every counted strategy declared no regime
		// preference, because one that did would have been discarded above.
		// Recorded because trading an uncharacterised market is a choice.
		v.Rationale = append(v.Rationale,
			"the regime is UNKNOWN and no surviving strategy declared a regime requirement")
	}

	v.Action = winner
	v.Confidence = net
	v.Reason = fmt.Sprintf(
		"%s: %d strategy opinion(s) agree at net confidence %s in a %s regime",
		strings.ToUpper(string(winner)), counted, net.StringFixed(3), in.Regime)
	v.Rationale = append(v.Rationale, v.Reason)
	return v
}

// regimeAllows reports whether a strategy declaring these regimes may run in
// the current one.
//
// An empty declaration means the strategy never made a claim, and is allowed.
// UNKNOWN is never coerced into a match: a strategy that declares TRENDING
// does not run in a market nobody can characterise, which is the whole reason
// UNKNOWN is a real regime rather than a default.
func regimeAllows(allowed []string, regime string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if strings.EqualFold(a, regime) {
			return true
		}
		if strings.EqualFold(a, "any") {
			return true
		}
	}
	return false
}

// strongest returns the highest confidence among counted opinions on the
// winning side, or zero if there are none.
func strongest(contributions []Contribution, winner domain.SignalAction) decimal.Decimal {
	want, _ := winner.Side()
	best := decimal.Zero
	for _, c := range contributions {
		if !c.Counted {
			continue
		}
		if side, ok := c.Action.Side(); ok && side == want && c.Confidence.GreaterThan(best) {
			best = c.Confidence
		}
	}
	return best
}
