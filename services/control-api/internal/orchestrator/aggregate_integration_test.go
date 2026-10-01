package orchestrator

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/quant"
)

// The PRODUCTION decision chain, end to end minus the database writes.
//
// `Decide` was fully unit-tested and completely unwired for a whole milestone.
// Thirty-five passing tests said the policy was correct and none of them could
// notice that nothing called it. So these drive the real functions the
// scheduler drives -- opinionsFrom, resolveRegime, ConsensusInputFor, Decide,
// directionToRoute -- in the order and with the values the orchestrator uses,
// rather than assembling a ConsensusInput by hand and proving the policy works
// on inputs nothing produces.

// decide runs the production chain over a set of evaluations.
func decide(t *testing.T, evaluations []Outcome, model *ModelOpinion) (Verdict, domain.Regime) {
	t.Helper()
	opinions, fresh := opinionsFrom(evaluations)
	if len(fresh) == 0 {
		return Verdict{Action: domain.SignalNoTrade}, domain.RegimeUnknown
	}
	regime, _ := resolveRegime(fresh)
	return Decide(ConsensusInputFor(opinions, regime, fresh[0].routing.eventRisk, model)), regime
}

// withEventRisk marks every evaluation as sitting inside a blackout window.
func withEventRisk(evaluations []Outcome, level string) []Outcome {
	out := make([]Outcome, len(evaluations))
	for i, e := range evaluations {
		e.routing.eventRisk = level
		out[i] = e
	}
	return out
}

func TestAgreementOnOneSideRoutesThatSide(t *testing.T) {
	for _, c := range []struct {
		name   string
		action domain.SignalAction
		want   domain.OrderSide
	}{
		{"all buy", domain.SignalBuy, domain.SideBuy},
		{"all sell", domain.SignalSell, domain.SideSell},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, _ := decide(t, []Outcome{
				evalFor("trend", domain.FamilyTrendFollowing, c.action, "0.80"),
				evalFor("breakout", domain.FamilyBreakout, c.action, "0.75"),
				evalFor("momentum", domain.FamilyMomentum, c.action, "0.70"),
			}, nil)

			if v.Action != c.action {
				t.Fatalf("three strategies agreed on %s and the verdict is %s: %s",
					c.action, v.Action, v.Reason)
			}
			side, _, reducing, ok := directionToRoute(v, nil)
			if !ok || side != c.want {
				t.Fatalf("the verdict did not route: ok=%v side=%q", ok, side)
			}
			if reducing {
				t.Error("agreement on an opening side was marked as reducing")
			}
			// The stop must come from the most confident of the three.
			_, fresh := opinionsFrom([]Outcome{
				evalFor("trend", domain.FamilyTrendFollowing, c.action, "0.80"),
				evalFor("breakout", domain.FamilyBreakout, c.action, "0.75"),
			})
			leader, found := strongestFresh(fresh, v.Contributions, side)
			if !found || leader.routing.strategy.Key != "trend" {
				t.Errorf("the order would carry %q's stop, not the most confident "+
					"opinion's", leader.routing.strategy.Key)
			}
		})
	}
}

func TestAConflictProducesNoTradeRatherThanAMajorityWinner(t *testing.T) {
	// Two against one on the BUY side. A majority vote would buy; the policy
	// is explicit that "netting opposing signals into whichever side has more
	// weight is how a system ends up trading its own indecision".
	v, _ := decide(t, []Outcome{
		evalFor("trend", domain.FamilyTrendFollowing, domain.SignalBuy, "0.80"),
		evalFor("breakout", domain.FamilyBreakout, domain.SignalBuy, "0.80"),
		evalFor("reversion", domain.FamilyMeanReversion, domain.SignalSell, "0.80"),
	}, nil)

	if v.Action != domain.SignalNoTrade {
		t.Fatalf("a 2-1 split produced %s. That is a majority vote, which this "+
			"policy does not take: %s", v.Action, v.Reason)
	}
	if v.BuyWeight.LessThanOrEqual(v.SellWeight) {
		t.Fatalf("the fixture did not actually give the buy side more weight "+
			"(buy %s, sell %s), so it is not testing a majority", v.BuyWeight, v.SellWeight)
	}
	if _, _, _, ok := directionToRoute(v, nil); ok {
		t.Error("a split routed an order on a flat account")
	}
}

func TestAHighImpactEventVetoesAgreement(t *testing.T) {
	// Unanimous, confident, and inside a blackout window. A veto is a
	// statement that conditions are wrong, not a vote about direction.
	agreed := []Outcome{
		evalFor("trend", domain.FamilyTrendFollowing, domain.SignalBuy, "0.95"),
		evalFor("breakout", domain.FamilyBreakout, domain.SignalBuy, "0.95"),
	}
	if v, _ := decide(t, agreed, nil); v.Action != domain.SignalBuy {
		t.Fatalf("the control case did not trade, so the veto below proves "+
			"nothing: %s", v.Reason)
	}

	v, _ := decide(t, withEventRisk(agreed, "high"), nil)
	if v.Action != domain.SignalNoTrade {
		t.Fatalf("a high-impact release did not veto unanimous agreement: %s", v.Reason)
	}
	if len(v.Vetoes) == 0 {
		t.Error("the refusal carries no veto, so an operator cannot see it was " +
			"a condition rather than a disagreement")
	}
}

func TestAConfidentRiskOffModelVetoesThroughTheProductionChain(t *testing.T) {
	agreed := []Outcome{
		evalFor("trend", domain.FamilyTrendFollowing, domain.SignalBuy, "0.95"),
		evalFor("breakout", domain.FamilyBreakout, domain.SignalBuy, "0.95"),
	}
	v, _ := decide(t, agreed, &ModelOpinion{
		ModelKey: "regime-classifier", Version: 1,
		Regime: string(domain.RegimeRiskOff), Confidence: decimal.RequireFromString("0.90"),
		Available: true,
	})
	if v.Action != domain.SignalNoTrade {
		t.Fatalf("a RISK_OFF model at 0.90 did not veto: %s", v.Reason)
	}
}

func TestAnUnavailableModelIsNotAssentThroughTheProductionChain(t *testing.T) {
	agreed := []Outcome{
		evalFor("trend", domain.FamilyTrendFollowing, domain.SignalBuy, "0.95"),
		evalFor("breakout", domain.FamilyBreakout, domain.SignalBuy, "0.95"),
	}

	// REQUIRED and silent is a veto: the decision was defined as model-gated
	// and the gate is missing.
	v, _ := decide(t, agreed, &ModelOpinion{
		ModelKey: "regime-classifier", Available: false, Required: true,
	})
	if v.Action != domain.SignalNoTrade {
		t.Errorf("a REQUIRED model that could not answer was read as assent: %s", v.Reason)
	}

	// Not required and silent: the decision proceeds, and the absence is
	// recorded rather than passing unremarked.
	v, _ = decide(t, agreed, &ModelOpinion{
		ModelKey: "regime-classifier", Available: false, Required: false,
	})
	if v.Action != domain.SignalBuy {
		t.Errorf("an optional silent model blocked the decision: %s", v.Reason)
	}
	var mentioned bool
	for _, line := range v.Rationale {
		if len(line) > 0 && contains(line, "regime-classifier") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Error("an unavailable model contributed nothing and left no trace in " +
			"the rationale, so a broken model would be invisible")
	}
}

func TestHoldAndCloseAreAbstentionsThroughTheProductionChain(t *testing.T) {
	// One BUY, two abstentions. Counting HOLD or CLOSE as agreement would
	// manufacture consensus out of strategies that declined to express one.
	v, _ := decide(t, []Outcome{
		evalFor("trend", domain.FamilyTrendFollowing, domain.SignalBuy, "0.95"),
		evalFor("reversion", domain.FamilyMeanReversion, domain.SignalHold, "0.90"),
		evalFor("session", domain.FamilySession, domain.SignalClose, "0.90"),
	}, nil)

	counted := 0
	for _, c := range v.Contributions {
		if c.Counted {
			counted++
		}
	}
	if counted != 1 {
		t.Fatalf("%d opinions were counted; HOLD and CLOSE are abstentions and "+
			"only the BUY should count", counted)
	}
	if !v.SellWeight.IsZero() {
		t.Errorf("an abstention contributed %s of sell weight", v.SellWeight)
	}
}

func TestTheSameStrategyEvaluatedTwiceDoesNotCountTwice(t *testing.T) {
	// The scheduler collects one (strategy, version) per instrument, but a
	// duplicate in that list must not turn one opinion into two votes and
	// outweigh a genuine dissenter.
	dup := evalFor("trend", domain.FamilyTrendFollowing, domain.SignalBuy, "0.80")
	v, _ := decide(t, []Outcome{
		dup, dup,
		evalFor("reversion", domain.FamilyMeanReversion, domain.SignalSell, "0.80"),
	}, nil)

	if !v.BuyWeight.Equal(v.SellWeight) {
		t.Errorf("one strategy listed twice carries %s against the dissenter's "+
			"%s. A duplicate is one opinion, and family down-weighting is what "+
			"keeps it so.", v.BuyWeight, v.SellWeight)
	}
	if v.Action != domain.SignalNoTrade {
		t.Errorf("a duplicate outvoted a genuine dissenter: %s", v.Reason)
	}
}

func TestOneDecisionKeyPerAccountInstrumentAndBar(t *testing.T) {
	bar := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)

	// The SAME bar and instrument must produce the same key however the
	// strategies come out, because the key is the durable guard: the
	// command_idempotency table is keyed on (account_id, idempotency_key), so
	// an identical key makes a second orchestrated order a duplicate the
	// database refuses.
	a := consensusIdempotencyKey("XAUUSD.m", bar)
	b := consensusIdempotencyKey("XAUUSD.m", bar)
	if a != b {
		t.Fatalf("two evaluations of one bar minted different keys: %q and %q", a, b)
	}
	if containsAny(a, "trend", "reversion", "breakout") {
		t.Errorf("the key %q names a strategy. The strategy an order is "+
			"attributed to is whichever counted opinion was strongest, and that "+
			"can differ between two evaluations of one bar, so a key naming it "+
			"would let a second order through.", a)
	}

	// Different instruments at the same instant are different decisions.
	if other := consensusIdempotencyKey("EURUSD", bar); other == a {
		t.Error("two instruments at one instant share a key, so the second " +
			"instrument's order would be refused as a duplicate")
	}
	// Different bars on one instrument are different decisions.
	if next := consensusIdempotencyKey("XAUUSD.m", bar.Add(time.Hour)); next == a {
		t.Error("two bars share a key, so the second bar could never trade")
	}
	// And the key has to satisfy the column's length constraint.
	if len(a) < 8 || len(a) > 200 {
		t.Errorf("key %q is %d characters; command_idempotency requires 8..200", a, len(a))
	}
}

func TestSeveralInstrumentsDecideIndependently(t *testing.T) {
	// Gold's strategies agree; the other instrument's disagree. One
	// instrument's split must not silence the other, because the aggregate is
	// per (account, instrument, bar) and nothing else.
	gold, _ := decide(t, []Outcome{
		evalFor("trend", domain.FamilyTrendFollowing, domain.SignalBuy, "0.90"),
		evalFor("breakout", domain.FamilyBreakout, domain.SignalBuy, "0.85"),
	}, nil)
	euro, _ := decide(t, []Outcome{
		evalFor("trend", domain.FamilyTrendFollowing, domain.SignalBuy, "0.90"),
		evalFor("reversion", domain.FamilyMeanReversion, domain.SignalSell, "0.90"),
	}, nil)

	if gold.Action != domain.SignalBuy {
		t.Errorf("the agreeing instrument did not trade: %s", gold.Reason)
	}
	if euro.Action != domain.SignalNoTrade {
		t.Errorf("the disagreeing instrument traded: %s", euro.Reason)
	}
}

func TestTheVerdictRecordsWhatAnInvestigatorNeeds(t *testing.T) {
	// The requirement, stated as the question it has to answer: strategy A
	// said BUY, strategy B said SELL, the result was NO TRADE, and why.
	evaluations := []Outcome{
		evalFor("trend", domain.FamilyTrendFollowing, domain.SignalBuy, "0.80"),
		evalFor("reversion", domain.FamilyMeanReversion, domain.SignalSell, "0.80"),
		evalFor("quiet", domain.FamilyStatistical, domain.SignalBuy, "0.20"),
	}
	v, regime := decide(t, evaluations, nil)
	_, fresh := opinionsFrom(evaluations)

	var blob map[string]any
	if err := json.Unmarshal(marshalVerdict(v, regime, fresh, ""), &blob); err != nil {
		t.Fatalf("the verdict did not serialise: %v", err)
	}

	for _, key := range []string{
		"policy_version", "action", "confidence", "reason", "regime",
		"vetoes", "rationale", "contributions", "buy_weight", "sell_weight",
		"attribution",
	} {
		if _, ok := blob[key]; !ok {
			t.Errorf("the recorded verdict has no %q, so that part of the "+
				"reasoning is unrecoverable", key)
		}
	}

	contributions, _ := blob["contributions"].([]any)
	if len(contributions) != 3 {
		t.Fatalf("%d contributions recorded for 3 opinions; a discarded opinion "+
			"is the part an investigator most needs", len(contributions))
	}
	seen := map[string]map[string]any{}
	for _, raw := range contributions {
		c, _ := raw.(map[string]any)
		key, _ := c["strategy"].(string)
		seen[key] = c
		for _, field := range []string{"strategy", "version", "action",
			"confidence", "counted", "role", "note"} {
			if _, ok := c[field]; !ok {
				t.Errorf("contribution %q has no %q", key, field)
			}
		}
	}
	if seen["trend"]["action"] != "buy" || seen["reversion"]["action"] != "sell" {
		t.Error("the opposing actions were not both recorded")
	}
	if seen["quiet"]["counted"] != false {
		t.Error("the 0.20-confidence opinion was counted")
	}
	if note, _ := seen["quiet"]["note"].(string); note == "" {
		t.Error("the discarded opinion carries no reason, so the verdict cannot " +
			"be argued with")
	}
	if blob["action"] != "no_trade" {
		t.Errorf("the recorded outcome is %v, not no_trade", blob["action"])
	}
	if reason, _ := blob["reason"].(string); reason == "" {
		t.Error("the verdict has no explanation")
	}
}

func TestAttributionNamesOneOwnerAndNeverDoubleCounts(t *testing.T) {
	// Three agreeing strategies, one order. Exactly one of them owns the P&L;
	// crediting each with the full result would make the sum of per-strategy
	// P&L exceed the account's own, and every ranking built on it would be
	// arithmetic about money that never existed.
	evaluations := []Outcome{
		evalFor("trend", domain.FamilyTrendFollowing, domain.SignalBuy, "0.90"),
		evalFor("breakout", domain.FamilyBreakout, domain.SignalBuy, "0.85"),
		evalFor("momentum", domain.FamilyMomentum, domain.SignalBuy, "0.80"),
	}
	v, regime := decide(t, evaluations, nil)
	_, fresh := opinionsFrom(evaluations)

	var blob map[string]any
	if err := json.Unmarshal(marshalVerdict(v, regime, fresh, "trend"), &blob); err != nil {
		t.Fatalf("serialise: %v", err)
	}
	attribution, _ := blob["attribution"].(map[string]any)
	if attribution["executing_strategy"] != "trend" {
		t.Fatalf("the executing strategy is %v, not the one the order was "+
			"attributed to", attribution["executing_strategy"])
	}
	if attribution["policy"] != AttributionPolicy {
		t.Errorf("the attribution policy is not versioned on the record: %v",
			attribution["policy"])
	}

	contributing, _ := attribution["contributing"].([]any)
	if len(contributing) != 2 {
		t.Fatalf("%d contributing strategies recorded; 3 agreed and 1 executes",
			len(contributing))
	}
	for _, raw := range contributing {
		if raw == "trend" {
			t.Error("the executing strategy is also listed as contributing, so a " +
				"naive sum over both lists would count it twice")
		}
	}

	// Exactly one contribution carries the primary role.
	primaries := 0
	for _, raw := range blob["contributions"].([]any) {
		c, _ := raw.(map[string]any)
		if c["role"] == "primary" {
			primaries++
		}
	}
	if primaries != 1 {
		t.Errorf("%d contributions claim the primary role; exactly one order "+
			"was placed and exactly one strategy can own its P&L", primaries)
	}
}

// contains and containsAny keep the assertions readable without pulling in
// strings for two uses.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if contains(haystack, n) {
			return true
		}
	}
	return false
}

// evalForRegime is evalFor with a stated regime, for the regime-gate cases.
func evalForRegime(key string, family domain.StrategyFamily, action domain.SignalAction,
	confidence, regime string, declares ...string) Outcome {

	e := evalFor(key, family, action, confidence, declares...)
	e.routing.signal = quant.SignalResponse{Regime: regime}
	return e
}

func TestAStrategyIsDiscardedInARegimeItDeclaresItselfInvalidIn(t *testing.T) {
	// ValidRegimes was stored, served and read by NOTHING before the
	// orchestrator started passing it to the policy. This is the production
	// path for it.
	v, regime := decide(t, []Outcome{
		evalForRegime("reversion", domain.FamilyMeanReversion, domain.SignalSell,
			"0.90", "TRENDING", "RANGING", "LOW_VOLATILITY"),
	}, nil)

	if regime != domain.RegimeTrending {
		t.Fatalf("the regime resolved to %s, so the fixture is not testing the gate", regime)
	}
	if v.Action != domain.SignalNoTrade {
		t.Fatalf("a mean-reversion strategy traded in a TRENDING market it "+
			"declares itself invalid in: %s", v.Reason)
	}
	if len(v.Contributions) != 1 || v.Contributions[0].Counted {
		t.Error("the opinion was counted despite the regime declaration")
	}
}
