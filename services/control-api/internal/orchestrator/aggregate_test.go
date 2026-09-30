package orchestrator

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/quant"
)

// The parts of aggregation that are pure, tested without a database.
//
// The wiring itself -- that the scheduler now places at most one order per
// (account, instrument, bar) -- is proved end to end by
// tests/replay/conditions_test.go scenario G, which drives the real pipeline
// over a fixture built to make the strategy set disagree. These tests cover
// the decisions that scenario cannot isolate: which opinion supplies the stop,
// what happens to a crowded family, what an unstable regime does, and the one
// case where a no-trade verdict must still let an order through.

func evalFor(key string, family domain.StrategyFamily, action domain.SignalAction,
	confidence string, regimes ...string) Outcome {

	return Outcome{
		Action:     action,
		Confidence: decimal.RequireFromString(confidence),
		RunID:      uuid.New(),
		routing: routingContext{
			valid: true,
			strategy: domain.Strategy{
				ID: uuid.New(), Key: key, Family: family, Enabled: true,
			},
			version:      1,
			validRegimes: regimes,
			barTime:      time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC),
			signal:       quant.SignalResponse{Regime: "TRENDING"},
			eventRisk:    "none",
		},
	}
}

func TestOnlyFreshEvaluationsBecomeOpinions(t *testing.T) {
	// A skip, a failure and a bar already evaluated all leave `routing.valid`
	// false. None of them is an opinion, and counting one as an abstention
	// would be indistinguishable from a strategy that looked and declined.
	evaluations := []Outcome{
		evalFor("trend", domain.FamilyTrendFollowing, domain.SignalBuy, "0.80"),
		{Status: domain.RunSkipped, Action: domain.SignalNoTrade},
		{Status: domain.RunFailed, Action: domain.SignalNoTrade},
	}
	opinions, fresh := opinionsFrom(evaluations)
	if len(opinions) != 1 || len(fresh) != 1 {
		t.Fatalf("expected exactly one opinion from one fresh evaluation, got "+
			"%d opinions and %d fresh", len(opinions), len(fresh))
	}
	if opinions[0].StrategyKey != "trend" {
		t.Errorf("the surviving opinion is %q, not the fresh one", opinions[0].StrategyKey)
	}
}

func TestACrowdedFamilyIsDownWeightedRatherThanCountedTwice(t *testing.T) {
	// Two trend strategies on one instrument are not two independent bets.
	// Left at weight 1 each they outvote a lone dissenter on weight alone,
	// which is how a system manufactures agreement out of correlation.
	evaluations := []Outcome{
		evalFor("trend_a", domain.FamilyTrendFollowing, domain.SignalBuy, "0.80"),
		evalFor("trend_b", domain.FamilyTrendFollowing, domain.SignalBuy, "0.80"),
		evalFor("reversion", domain.FamilyMeanReversion, domain.SignalSell, "0.80"),
	}
	opinions, _ := opinionsFrom(evaluations)

	byKey := map[string]decimal.Decimal{}
	for _, o := range opinions {
		byKey[o.StrategyKey] = o.Weight
	}
	half := decimal.RequireFromString("0.5")
	one := decimal.NewFromInt(1)

	for _, key := range []string{"trend_a", "trend_b"} {
		if !byKey[key].Equal(half) {
			t.Errorf("%s carries weight %s; two strategies of one family should "+
				"share a single opinion's weight", key, byKey[key])
		}
	}
	if !byKey["reversion"].Equal(one) {
		t.Errorf("the lone mean-reversion opinion carries weight %s, not 1",
			byKey["reversion"])
	}

	// And the effect: the crowded family no longer outweighs the dissenter, so
	// this is a genuine split rather than a 2-1 win.
	buy := byKey["trend_a"].Add(byKey["trend_b"])
	if !buy.Equal(byKey["reversion"]) {
		t.Errorf("buy weight %s against sell weight %s: the families are no "+
			"longer balanced and the test's premise is gone", buy, byKey["reversion"])
	}
}

func TestOpinionOrderIsDeterministic(t *testing.T) {
	// The policy is order-independent by construction; the CONTRIBUTIONS it
	// records are not, and a decision whose stored explanation reorders
	// between two identical runs is not reproducible. The replay result digest
	// is taken over stored rows.
	build := func() []Outcome {
		return []Outcome{
			evalFor("zulu", domain.FamilyVolatility, domain.SignalBuy, "0.70"),
			evalFor("alpha", domain.FamilyTrendFollowing, domain.SignalBuy, "0.70"),
			evalFor("mike", domain.FamilyMomentum, domain.SignalBuy, "0.70"),
		}
	}
	first, _ := opinionsFrom(build())
	for i := 0; i < 20; i++ {
		got, _ := opinionsFrom(build())
		for j := range got {
			if got[j].StrategyKey != first[j].StrategyKey {
				t.Fatalf("run %d ordered opinions differently: %q at position %d, "+
					"expected %q", i, got[j].StrategyKey, j, first[j].StrategyKey)
			}
		}
	}
	if first[0].StrategyKey != "alpha" {
		t.Errorf("opinions are not ordered by strategy key; first is %q", first[0].StrategyKey)
	}
}

func TestDisagreementAboutTheRegimeCollapsesToUnknown(t *testing.T) {
	// The strategies saw the SAME bars. Two different regimes is a statement
	// that the classification is not stable, and UNKNOWN is the label this
	// platform already uses for a market nobody can characterise. Taking the
	// most common reading instead would trade a market it cannot describe.
	a := evalFor("trend", domain.FamilyTrendFollowing, domain.SignalBuy, "0.80")
	b := evalFor("breakout", domain.FamilyBreakout, domain.SignalBuy, "0.80")
	b.routing.signal = quant.SignalResponse{Regime: "RANGING"}

	regime, agreed := resolveRegime([]Outcome{a, b})
	if agreed {
		t.Error("two different regimes were reported as agreement")
	}
	if regime != domain.RegimeUnknown {
		t.Errorf("regime resolved to %s, not UNKNOWN", regime)
	}

	regime, agreed = resolveRegime([]Outcome{a, a})
	if !agreed || regime != domain.RegimeTrending {
		t.Errorf("agreement on TRENDING resolved to %s (agreed=%v)", regime, agreed)
	}
}

func TestTheStopComesFromTheStrongestCountedOpinionOnTheRoutedSide(t *testing.T) {
	// An opinion the policy DISCARDED must not supply the stop for an order it
	// was excluded from. The discarded one here is the more confident, so a
	// lookup that ignored `counted` would pick it.
	weak := evalFor("counted_buy", domain.FamilyTrendFollowing, domain.SignalBuy, "0.62")
	strongButDiscarded := evalFor("discarded_buy", domain.FamilyBreakout, domain.SignalBuy, "0.95")
	wrongSide := evalFor("counted_sell", domain.FamilyMeanReversion, domain.SignalSell, "0.99")
	fresh := []Outcome{weak, strongButDiscarded, wrongSide}

	contributions := []Contribution{
		{StrategyKey: "counted_buy", Action: domain.SignalBuy, Counted: true},
		{StrategyKey: "discarded_buy", Action: domain.SignalBuy, Counted: false,
			Note: "discarded: declares itself valid in RANGING"},
		{StrategyKey: "counted_sell", Action: domain.SignalSell, Counted: true},
	}

	leader, found := strongestFresh(fresh, contributions, domain.SideBuy)
	if !found {
		t.Fatal("no leader found on the buy side")
	}
	if leader.routing.strategy.Key != "counted_buy" {
		t.Errorf("the order would carry %q's stop; a discarded opinion supplied "+
			"the stop for an order it took no part in", leader.routing.strategy.Key)
	}

	if _, found := strongestFresh(fresh, contributions, domain.SideSell); !found {
		t.Error("the counted sell opinion was not found on its own side")
	}
}

// --- the reducing rescue ---------------------------------------------------

func longPosition(quantity string) *domain.Position {
	return &domain.Position{
		InstrumentID: "XAUUSD.m", Side: domain.SideBuy,
		Quantity: decimal.RequireFromString(quantity), Status: domain.PositionOpen,
	}
}

func TestAnActionableVerdictRoutesUncapped(t *testing.T) {
	v := Verdict{Action: domain.SignalBuy}
	side, quantityCap, reducing, ok := directionToRoute(v, nil)
	if !ok || side != domain.SideBuy {
		t.Fatalf("an actionable BUY verdict did not route: ok=%v side=%q", ok, side)
	}
	if reducing {
		t.Error("an ordinary verdict was marked as reducing")
	}
	if quantityCap.IsPositive() {
		t.Errorf("an ordinary verdict was capped at %s; sizing against the "+
			"account's budget is the only bound it should have", quantityCap)
	}
}

func TestADisagreementStillLetsTheReducingLegThrough(t *testing.T) {
	// ENGINEERING_GUIDE.md rule 8, at the aggregation layer.
	//
	// The account is long. The trend strategy says BUY and the reversion
	// strategy says SELL, so the policy refuses: a split is not a decision.
	// Before aggregation both routed and the SELL reduced the position. If
	// aggregation simply suppressed both, the change meant to stop the account
	// HEDGING itself would instead have stopped it UNWINDING -- the same
	// inversion the notional cap, the event blackout and the daily-loss limit
	// each had to be rescued from.
	v := Verdict{
		Action: domain.SignalNoTrade,
		Reason: "NO TRADE: strategies disagree",
		Contributions: []Contribution{
			{StrategyKey: "trend", Action: domain.SignalBuy, Counted: true},
			{StrategyKey: "reversion", Action: domain.SignalSell, Counted: true},
		},
	}
	side, quantityCap, reducing, ok := directionToRoute(v, longPosition("0.05"))
	if !ok {
		t.Fatal("a no-trade verdict trapped an open position: the strategy asking " +
			"to reduce it was suppressed along with the one asking to add")
	}
	if side != domain.SideSell {
		t.Errorf("the rescued leg is a %s against a long position", side)
	}
	if !reducing {
		t.Error("the order was not marked as permitted only by reduction")
	}
	if !quantityCap.Equal(decimal.RequireFromString("0.05")) {
		t.Errorf("the reducing leg is capped at %s, not the position's 0.05; "+
			"an uncapped rescue is a side flip that opens fresh exposure", quantityCap)
	}
}

func TestAVetoStillLetsTheReducingLegThrough(t *testing.T) {
	// A high-impact release is an argument for being able to close, not
	// against it. The event blackout one layer down carries the same
	// exemption for the same reason.
	v := Verdict{
		Action: domain.SignalNoTrade,
		Vetoes: []string{"event risk: a high-impact release is inside the blackout window"},
		Contributions: []Contribution{
			{StrategyKey: "reversion", Action: domain.SignalSell, Counted: true},
		},
	}
	if _, _, reducing, ok := directionToRoute(v, longPosition("0.03")); !ok || !reducing {
		t.Fatal("a vetoed verdict refused to let an open position be reduced during " +
			"exactly the window an operator most wants out")
	}
}

func TestTheRescueNeverOpensExposure(t *testing.T) {
	for _, c := range []struct {
		name     string
		verdict  Verdict
		existing *domain.Position
	}{
		{
			// Flat. There is nothing to reduce, so a no-trade verdict is a
			// no-trade: the rescue must not become a back door for a refused
			// opening order.
			name: "no open position",
			verdict: Verdict{Action: domain.SignalNoTrade, Contributions: []Contribution{
				{StrategyKey: "trend", Action: domain.SignalBuy, Counted: true},
				{StrategyKey: "reversion", Action: domain.SignalSell, Counted: true},
			}},
			existing: nil,
		},
		{
			// Long, and the only counted opinion is also long. Adding to a
			// position is not reducing it.
			name: "every counted opinion is on the position's own side",
			verdict: Verdict{Action: domain.SignalNoTrade, Contributions: []Contribution{
				{StrategyKey: "trend", Action: domain.SignalBuy, Counted: true},
			}},
			existing: longPosition("0.05"),
		},
		{
			// The opposing opinion exists but the policy DISCARDED it -- below
			// the confidence floor, or invalid in this regime. Reaching past
			// the policy to act on it would be aggregation in name only.
			name: "the opposing opinion was discarded by the policy",
			verdict: Verdict{Action: domain.SignalNoTrade, Contributions: []Contribution{
				{StrategyKey: "trend", Action: domain.SignalBuy, Counted: true},
				{StrategyKey: "reversion", Action: domain.SignalSell, Counted: false,
					Note: "discarded: confidence 0.31 is below the 0.55 minimum"},
			}},
			existing: longPosition("0.05"),
		},
		{
			// An abstention is not a request to close. CLOSE and HOLD are
			// declining to express a direction, and the policy treats them as
			// abstentions everywhere else too.
			name: "the only opposing opinion abstained",
			verdict: Verdict{Action: domain.SignalNoTrade, Contributions: []Contribution{
				{StrategyKey: "reversion", Action: domain.SignalClose, Counted: true},
			}},
			existing: longPosition("0.05"),
		},
		{
			// A position of zero quantity is not a position.
			name: "the position is empty",
			verdict: Verdict{Action: domain.SignalNoTrade, Contributions: []Contribution{
				{StrategyKey: "reversion", Action: domain.SignalSell, Counted: true},
			}},
			existing: longPosition("0"),
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if side, _, _, ok := directionToRoute(c.verdict, c.existing); ok {
				t.Fatalf("a no-trade verdict placed a %s order anyway; the reducing "+
					"exemption has become a way to open exposure", side)
			}
		})
	}
}

func TestAShortPositionIsReducedByABuy(t *testing.T) {
	short := &domain.Position{
		InstrumentID: "XAUUSD.m", Side: domain.SideSell,
		Quantity: decimal.RequireFromString("0.04"), Status: domain.PositionOpen,
	}
	v := Verdict{Action: domain.SignalNoTrade, Contributions: []Contribution{
		{StrategyKey: "trend", Action: domain.SignalBuy, Counted: true},
		{StrategyKey: "reversion", Action: domain.SignalSell, Counted: true},
	}}
	side, quantityCap, reducing, ok := directionToRoute(v, short)
	if !ok || side != domain.SideBuy || !reducing {
		t.Fatalf("a short position was not reducible by a counted BUY: "+
			"ok=%v side=%q reducing=%v", ok, side, reducing)
	}
	if !quantityCap.Equal(decimal.RequireFromString("0.04")) {
		t.Errorf("capped at %s rather than the short's 0.04", quantityCap)
	}
}
