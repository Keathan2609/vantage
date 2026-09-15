package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
)

// Aggregation: several strategies, one instrument, ONE verdict.
//
// # The defect this closes
//
// The scheduler used to call EvaluateAndRoute once per (strategy, instrument)
// with Execute:true, and each call routed its own signal into the OMS. Two
// strategies disagreeing on one bar therefore produced two opposing orders and
// both filled. Measured on the replay fixtures, not inferred: the
// conflicting-signals dataset split the strategy set at 38 instants and ALL 38
// produced filled orders on both sides of XAUUSD.m; event-window did it at 19
// of 38 and news-agreement at 31.
//
// Every individual control was working. The account was hedging itself one
// permitted order at a time, and no check is positioned to see that, because
// each order is individually reasonable -- which is exactly why the decision
// had to be made before the OMS rather than inside it.
//
// `Decide` -- a pure, versioned policy that puts vetoes before votes and never
// takes a majority vote -- existed the whole time with no production caller.
// This file is that caller.
//
// # What is NOT changed here
//
// The per-bar watermark. Every strategy still gets its own run row and its own
// signal row for the bar it was evaluated on, written by the same code as
// before. Aggregation happens after all of that: it changes what reaches the
// OMS, not what is recorded about each strategy's opinion.
//
// The single-strategy endpoint. An operator asking for one named strategy to
// be run gets that strategy's own signal routed, as before. There is no
// disagreement to aggregate over one opinion, and putting the manual path
// through a confidence threshold it never had would change an operator-facing
// control that nobody asked to change.

// StrategyRef names one strategy version to evaluate.
type StrategyRef struct {
	ID      uuid.UUID
	Version int
}

// InstrumentRunRequest asks for every applicable strategy on one instrument to
// be evaluated and aggregated into a single verdict.
type InstrumentRunRequest struct {
	Account      domain.Account
	InstrumentID string
	// Strategies is the set whose opinions form the verdict. The caller has
	// already filtered it by authority scope and lifecycle.
	Strategies []StrategyRef
	// Execute routes the verdict into the order pipeline. When false the
	// aggregation is a dry run: every strategy is still evaluated and
	// recorded, and nothing is placed.
	Execute     bool
	ActorUserID uuid.UUID
	ActorRole   domain.Role
	RequestID   string
}

// InstrumentOutcome is the aggregate result.
type InstrumentOutcome struct {
	Verdict Verdict
	// Evaluations is each strategy's own outcome, in the order evaluated.
	// Every one of them is recorded in strategy_runs regardless of what the
	// aggregate decided.
	Evaluations []Outcome
	Order       *domain.Order
	Rejection   *domain.Rejection
	Executed    bool
	// Reducing reports that the order was permitted ONLY because it reduces an
	// open position, against a verdict that was otherwise no-trade.
	Reducing bool
	// DecisionID is the no-trade snapshot written when nothing was placed.
	DecisionID *uuid.UUID
	// Abstained reports that no verdict was reached at all, because at least
	// one strategy's opinion for this bar was recorded elsewhere and the
	// aggregate would have been taken over a partial set.
	Abstained  bool
	SkipReason string
}

// EvaluateInstrument evaluates every strategy for one instrument and routes at
// most ONE order from the aggregate.
func (s *Service) EvaluateInstrument(ctx context.Context, req InstrumentRunRequest) (InstrumentOutcome, error) {
	log := logging.FromContext(ctx)
	out := InstrumentOutcome{Verdict: Verdict{Action: domain.SignalNoTrade, PolicyVersion: PolicyVersion}}

	for _, ref := range req.Strategies {
		// Execute is false for every individual evaluation, always. This is
		// the whole point: a strategy records its opinion and does not act on
		// it. The single place an order can now originate for this instrument
		// is the verdict below.
		outcome, err := s.EvaluateAndRoute(ctx, RunRequest{
			Account: req.Account, StrategyID: ref.ID, InstrumentID: req.InstrumentID,
			Version: ref.Version, Execute: false,
			ActorUserID: req.ActorUserID, ActorRole: req.ActorRole, RequestID: req.RequestID,
		})
		if err != nil {
			// One strategy failing does not silence the rest, but it does mean
			// the verdict is taken over a set that is missing an opinion. It
			// is recorded on the verdict's rationale rather than swallowed.
			log.Error("strategy evaluation failed",
				"strategy", ref.ID.String(), "instrument", req.InstrumentID, "error", err.Error())
			out.Evaluations = append(out.Evaluations, Outcome{
				Status: domain.RunFailed, Action: domain.SignalNoTrade,
				SkipReason: err.Error(),
			})
			continue
		}
		out.Evaluations = append(out.Evaluations, outcome)

		if outcome.alreadyEvaluated {
			// A partial set is not a decision. Abstain rather than aggregate
			// over whoever happened to answer: the missing opinion may be the
			// one that would have produced disagreement, and acting without it
			// is the exact failure this file exists to close.
			out.Abstained = true
			out.SkipReason = outcome.SkipReason
		}
	}

	if out.Abstained {
		log.Info("consensus abstained: an opinion for this bar was recorded elsewhere",
			"instrument", req.InstrumentID, "reason", out.SkipReason)
		return out, nil
	}

	opinions, fresh := opinionsFrom(out.Evaluations)
	if len(fresh) == 0 {
		// Nothing offered an opinion. Ordinary and common: warm-up, a closed
		// market, a feed the platform does not trust. Each strategy has
		// already recorded its own reason in strategy_runs.
		return out, nil
	}

	regime, regimeAgreed := resolveRegime(fresh)
	lead := fresh[0]

	in := ConsensusInput{
		Policy:    DefaultConsensusPolicy(),
		Opinions:  opinions,
		Regime:    string(regime),
		EventRisk: lead.routing.eventRisk,
		// No model participates in this path. A ModelOpinion with Available
		// false would add "model X was unavailable" to every rationale and
		// imply a model that was consulted and could not answer; nil says
		// truthfully that the decision is not model-gated.
		Model: nil,
	}
	verdict := Decide(in)
	if !regimeAgreed {
		verdict.Rationale = append(verdict.Rationale,
			"the strategies reported different regimes for the same bars, so the "+
				"regime was taken as UNKNOWN rather than as whichever was reported first")
	}
	out.Verdict = verdict

	if !req.Execute {
		return out, nil
	}

	// The book, needed twice: to decide whether a refused direction would have
	// REDUCED an open position, and to record portfolio state on a no-trade
	// snapshot.
	existing, err := s.openPosition(ctx, req.Account, req.InstrumentID)
	if err != nil {
		return out, err
	}

	side, quantityCap, reducing, ok := directionToRoute(verdict, existing)
	if !ok {
		id, derr := s.recordNoTrade(ctx, req, fresh, verdict, regime, existing)
		if derr != nil {
			return out, derr
		}
		out.DecisionID = id
		return out, nil
	}

	// Whose stop and target the order carries: the strongest COUNTED opinion
	// on the side being routed. That is the same opinion the policy's net
	// confidence is scaled by, and the same strategy the order is attributed
	// to, so the three cannot disagree with each other.
	leader, found := strongestFresh(fresh, verdict.Contributions, side)
	if !found {
		// Unreachable while directionToRoute only returns a side that a
		// counted opinion asked for, which is the only way it can return one.
		// Refusing here rather than picking an arbitrary strategy keeps that
		// invariant checkable instead of assumed.
		id, derr := s.recordNoTrade(ctx, req, fresh, verdict, regime, existing)
		if derr != nil {
			return out, derr
		}
		out.DecisionID = id
		return out, nil
	}

	consensus := marshalVerdict(verdict, regime, fresh)
	out.Reducing = reducing

	order, rejection, executed, err := s.place(ctx, placement{
		Account:     req.Account,
		StrategyID:  leader.routing.strategy.ID,
		ActorUserID: req.ActorUserID,
		ActorRole:   req.ActorRole,
		RequestID:   req.RequestID,
		// The leading strategy's run, so the order joins back to the
		// evaluation whose stop it carries.
		CorrelationID: leader.RunID.String(),
		rc:            leader.routing,
		side:          side,
		regime:        regime,
		maxQuantity:   quantityCap,
		consensus:     consensus,
		keyPrefix:     "consensus",
	})
	if err != nil {
		return out, err
	}
	out.Order = order
	out.Rejection = rejection
	out.Executed = executed
	if order == nil && rejection != nil {
		log.Info("consensus verdict was not sizeable",
			"instrument", req.InstrumentID, "reason", rejection.Message)
	}
	return out, nil
}

// opinionsFrom turns fresh evaluations into the policy's inputs.
//
// The weight is 1 divided by the number of FRESH opinions from the same
// family. Two trend strategies on one instrument are not two independent
// bets -- they are one read of the market counted twice -- and letting a
// crowded family outvote the rest is how a system manufactures agreement out
// of correlation. The policy exposes Weight for exactly this and names the
// caller as responsible for it.
func opinionsFrom(evaluations []Outcome) ([]StrategyOpinion, []Outcome) {
	var fresh []Outcome
	for _, e := range evaluations {
		if e.routing.valid {
			fresh = append(fresh, e)
		}
	}
	// Deterministic order. The policy is order-independent by construction,
	// but the contributions it records are not, and a decision whose stored
	// explanation reorders between two identical runs is not reproducible.
	sort.SliceStable(fresh, func(i, j int) bool {
		return fresh[i].routing.strategy.Key < fresh[j].routing.strategy.Key
	})

	perFamily := map[domain.StrategyFamily]int{}
	for _, e := range fresh {
		perFamily[e.routing.strategy.Family]++
	}

	opinions := make([]StrategyOpinion, 0, len(fresh))
	for _, e := range fresh {
		n := perFamily[e.routing.strategy.Family]
		weight := decimal.NewFromInt(1)
		if n > 1 {
			weight = weight.Div(decimal.NewFromInt(int64(n)))
		}
		opinions = append(opinions, StrategyOpinion{
			StrategyKey:    e.routing.strategy.Key,
			Family:         string(e.routing.strategy.Family),
			Version:        e.routing.version,
			Action:         e.Action,
			Confidence:     e.Confidence,
			Weight:         weight,
			AllowedRegimes: e.routing.validRegimes,
		})
	}
	return opinions, fresh
}

// resolveRegime reports the regime every fresh signal agrees on.
//
// Disagreement collapses to UNKNOWN rather than to a majority. The strategies
// saw the SAME bars, so two different regimes is a statement that the
// classification is not stable, and UNKNOWN is the label this platform already
// uses for a market nobody can characterise. It is a real answer and an
// untradable one: `regimeAllows` never coerces UNKNOWN into a match, so every
// strategy that declared a regime requirement is discarded and the usual
// outcome is NO TRADE. Picking the most common reading instead would trade a
// market the platform cannot describe.
func resolveRegime(fresh []Outcome) (domain.Regime, bool) {
	if len(fresh) == 0 {
		return domain.RegimeUnknown, true
	}
	first := fresh[0].routing.signal.MarketRegime()
	for _, e := range fresh[1:] {
		if e.routing.signal.MarketRegime() != first {
			return domain.RegimeUnknown, false
		}
	}
	return first, true
}

// directionToRoute decides what, if anything, to place.
//
// Returns the side, a quantity cap (zero for none), whether the order is
// permitted only as a reduction, and whether to place at all.
func directionToRoute(v Verdict, existing *domain.Position) (domain.OrderSide, decimal.Decimal, bool, bool) {
	if side, ok := v.Action.Side(); ok {
		return side, decimal.Zero, false, true
	}

	// ---- The reducing rescue ----------------------------------------------
	//
	// CLAUDE.md rule 8: a check that limits exposure or loss must never refuse
	// a reducing order. Aggregation is not a check, but suppressing the
	// reducing leg of a disagreement produces exactly the inversion the rule
	// exists to prevent, and it is a NEW way to produce it.
	//
	// Concretely: an account long 0.05 lots, the trend strategy says BUY and
	// the reversion strategy says SELL. Before aggregation both routed and the
	// SELL reduced the position. After it, the disagreement is a no-trade and
	// the position stays open -- so the change that was meant to stop the
	// account hedging itself would instead have stopped it UNWINDING. The same
	// applies to a veto: a high-impact release is an argument for being able to
	// close, not against it, which is why the event blackout carries the same
	// exemption one layer down.
	//
	// Deliberately narrow, on three counts. Only a COUNTED opinion qualifies:
	// one the policy discarded is one it decided not to listen to, and
	// reaching past that would be aggregation in name only. Only the direction
	// that opposes an open position qualifies. And the size is capped at the
	// position, so the result is strictly reducing and can never become a side
	// flip that opens fresh exposure in the other direction.
	if existing == nil || !existing.Quantity.IsPositive() {
		return "", decimal.Zero, false, false
	}
	var want domain.OrderSide
	switch existing.Side {
	case domain.SideBuy:
		want = domain.SideSell
	case domain.SideSell:
		want = domain.SideBuy
	default:
		return "", decimal.Zero, false, false
	}
	for _, c := range v.Contributions {
		if !c.Counted {
			continue
		}
		if side, ok := c.Action.Side(); ok && side == want {
			return want, existing.Quantity, true, true
		}
	}
	return "", decimal.Zero, false, false
}

// strongestFresh finds the evaluation behind the highest-confidence COUNTED
// opinion on one side, which is the strategy the order is attributed to and
// whose suggested stop it carries.
func strongestFresh(fresh []Outcome, contributions []Contribution,
	side domain.OrderSide) (Outcome, bool) {

	// COUNTED, not merely present. An opinion the policy discarded -- below
	// the confidence floor, or declaring itself invalid in this regime -- must
	// not end up supplying the stop for an order it was excluded from.
	counted := map[string]bool{}
	for _, c := range contributions {
		if c.Counted {
			counted[c.StrategyKey] = true
		}
	}

	best := Outcome{}
	found := false
	for _, e := range fresh {
		if !counted[e.routing.strategy.Key] {
			continue
		}
		got, ok := e.Action.Side()
		if !ok || got != side {
			continue
		}
		if !found || e.Confidence.GreaterThan(best.Confidence) {
			best, found = e, true
		}
	}
	return best, found
}

// recordNoTrade writes the decision snapshot for a verdict that placed nothing.
//
// Until this existed, "why did it not trade?" was answerable only from the
// individual strategy_runs, which record what each strategy said and cannot
// record what the policy did with the disagreement. The snapshot table already
// anticipated the row: `outcome` has always permitted 'no_trade' and the
// strategy columns have always been nullable, because a verdict is not
// attributable to one strategy.
func (s *Service) recordNoTrade(ctx context.Context, req InstrumentRunRequest, fresh []Outcome,
	v Verdict, regime domain.Regime, existing *domain.Position) (*uuid.UUID, error) {

	// Any fresh evaluation carries the shared market context -- the same
	// instrument, the same quote, the same event window. The first is used for
	// it and the whole set for the contributor list.
	lead := fresh[0]
	barTime := lead.routing.barTime

	portfolio := map[string]any{"open_position": nil}
	if existing != nil {
		portfolio["open_position"] = map[string]any{
			"side": string(existing.Side), "quantity": existing.Quantity.String(),
		}
	}

	snap := domain.DecisionSnapshot{
		AccountID:    req.Account.ID,
		InstrumentID: req.InstrumentID,
		BarTime:      &barTime,
		Quote: marshalJSON(map[string]any{
			"bid": lead.routing.quote.Bid.String(), "ask": lead.routing.quote.Ask.String(),
			"source_time": lead.routing.quote.SourceTime,
			"ingested_at": lead.routing.quote.IngestedAt,
			"provider":    lead.routing.quote.Provider,
		}),
		EventContext:     marshalJSON(map[string]any{"event_risk": lead.routing.eventRisk}),
		PortfolioContext: marshalJSON(portfolio),
		Consensus:        marshalVerdict(v, regime, fresh),
		Regime:           regime,
		SignalAction:     domain.SignalNoTrade,
		Confidence:       v.Confidence,
		Outcome:          "no_trade",
		OutcomeReason:    v.Reason,
	}

	var id uuid.UUID
	err := s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		var derr error
		id, derr = s.store.Research.CreateDecisionTx(ctx, tx, snap)
		return derr
	})
	if err != nil {
		return nil, fmt.Errorf("orchestrator: record no-trade decision: %w", err)
	}
	return &id, nil
}

// marshalVerdict serialises the verdict for the decision snapshot.
//
// The DISCARDED contributions are kept, not filtered out. "The mean-reversion
// strategy said SELL at 0.71 and was discarded because it declares itself
// valid in RANGING and the regime is TRENDING" is the answer to the question
// an operator is actually asking; a record of only the survivors makes the
// verdict unarguable.
func marshalVerdict(v Verdict, regime domain.Regime, fresh []Outcome) json.RawMessage {
	contributions := make([]map[string]any, 0, len(v.Contributions))
	for _, c := range v.Contributions {
		contributions = append(contributions, map[string]any{
			"strategy":   c.StrategyKey,
			"family":     c.Family,
			"action":     string(c.Action),
			"confidence": c.Confidence.String(),
			"weight":     c.Weight.String(),
			"counted":    c.Counted,
			"note":       c.Note,
		})
	}
	bars := map[string]string{}
	for _, e := range fresh {
		bars[e.routing.strategy.Key] = e.routing.barTime.UTC().Format(time.RFC3339)
	}
	return marshalJSON(map[string]any{
		"policy_version": v.PolicyVersion,
		"action":         string(v.Action),
		"confidence":     v.Confidence.String(),
		"reason":         v.Reason,
		"regime":         string(regime),
		"vetoes":         v.Vetoes,
		"rationale":      v.Rationale,
		"contributions":  contributions,
		"buy_weight":     v.BuyWeight.String(),
		"sell_weight":    v.SellWeight.String(),
		"bar_times":      bars,
	})
}

// openPosition reports the account's open position in one instrument, or nil.
func (s *Service) openPosition(ctx context.Context, account domain.Account,
	instrumentID string) (*domain.Position, error) {

	snapshot, err := s.portfolio.Compute(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: portfolio snapshot: %w", err)
	}
	for i := range snapshot.Positions {
		if snapshot.Positions[i].Position.InstrumentID == instrumentID {
			p := snapshot.Positions[i].Position
			return &p, nil
		}
	}
	return nil, nil
}

// marshalJSON never fails the decision for a serialisation problem: an
// unrecordable blob becomes an empty object and the decision still stands,
// because losing the explanation is better than losing the refusal.
func marshalJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
