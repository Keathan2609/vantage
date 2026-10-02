package orchestrator

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
)

// The consensus half of the decisive-breakout fixture.
//
// # Where these numbers come from
//
// They are not invented. They are what the five PAPER strategies actually
// produced at bar 119 of `internal/replay/fixtures/decisive_breakout.csv`,
// measured by evaluating the real strategy implementations against that
// fixture's bars, with `scanner.classify_regime` reporting TRENDING at the
// same instant:
//
//	macd_momentum               buy      0.5991   valid in TRENDING
//	donchian_breakout           buy      0.7500   valid in TRENDING
//	rsi_mean_reversion          no_trade 0.0000   RANGING only, gated out
//	ma_trend_crossover          no_trade 0.0000   valid, but had no opinion
//	bollinger_zscore_reversion  sell     0.6467   RANGING only, gated out
//
// # What this test is for
//
// The engineering report recorded ST-5 as "the set of strategies that can
// clear the floor and the set permitted to act do not intersect". The Python
// side now shows that a sufficiently decisive break clears the floor in a
// confirmed trend. This is the other half: that such opinions, fed to the real
// consensus policy with nothing loosened, produce an actionable verdict rather
// than dying one step later.
//
// Every committed fixture before this one topped out at 0.441 on
// `donchian_breakout`, so the equivalent input was never available to test
// with. That is why the claim survived as long as it did.
//
// # What it still does not prove
//
// An order. Position sizing, the eighteen risk checks, trading authority,
// Autopilot, the account's exposure ceilings and its daily-loss budget all sit
// downstream of this verdict, and a replay through the real scheduler is what
// exercises those. This asserts the decision, which is exactly where the claim
// was made and no further.
func bar119Opinions() []StrategyOpinion {
	one := decimal.NewFromInt(1)
	return []StrategyOpinion{
		{
			StrategyKey: "macd_momentum", Family: "momentum", Version: 1,
			Action: domain.SignalBuy, Confidence: d("0.5991"), Weight: one,
			AllowedRegimes: []string{"TRENDING"},
		},
		{
			StrategyKey: "donchian_breakout", Family: "breakout", Version: 1,
			Action: domain.SignalBuy, Confidence: d("0.7500"), Weight: one,
			AllowedRegimes: []string{"TRENDING", "HIGH_VOLATILITY"},
		},
		{
			StrategyKey: "rsi_mean_reversion", Family: "mean_reversion", Version: 1,
			Action: domain.SignalNoTrade, Confidence: decimal.Zero, Weight: one,
			AllowedRegimes: []string{"RANGING", "LOW_VOLATILITY"},
		},
		{
			StrategyKey: "ma_trend_crossover", Family: "trend_following", Version: 1,
			Action: domain.SignalNoTrade, Confidence: decimal.Zero, Weight: one,
			AllowedRegimes: []string{"TRENDING", "HIGH_VOLATILITY"},
		},
		{
			StrategyKey: "bollinger_zscore_reversion", Family: "statistical", Version: 1,
			Action: domain.SignalSell, Confidence: d("0.6467"), Weight: one,
			AllowedRegimes: []string{"RANGING"},
		},
	}
}

func TestTheDecisiveBreakoutFixtureProducesAnActionableVerdict(t *testing.T) {
	verdict := Decide(ConsensusInput{
		Policy:    DefaultConsensusPolicy(),
		Opinions:  bar119Opinions(),
		Regime:    string(domain.RegimeTrending),
		EventRisk: "none",
	})

	if len(verdict.Vetoes) != 0 {
		t.Fatalf("unexpected vetoes on a clean trend: %v", verdict.Vetoes)
	}
	if verdict.Action != domain.SignalBuy {
		t.Fatalf("two agreeing above-floor opinions in a regime both declare valid, "+
			"with the only opposition gated out, produced %q (confidence %s). "+
			"Contributions: %+v",
			verdict.Action, verdict.Confidence, verdict.Contributions)
	}
	if verdict.Confidence.LessThan(DefaultConsensusPolicy().MinNetConfidence) {
		t.Errorf("verdict confidence %s is below the net requirement %s",
			verdict.Confidence, DefaultConsensusPolicy().MinNetConfidence)
	}
}

// TestTheOpposingOpinionIsDiscardedByRegimeAndNotByWeight records WHY it acts.
//
// `bollinger_zscore_reversion` sells at 0.6467, which is above the floor and
// would be disqualifying opposition if it were admitted: the policy allows at
// most 20% of weight on the other side. It is not admitted, because it
// declares itself valid only in RANGING and the regime is TRENDING.
//
// That distinction is the whole reason the verdict is a buy, and it is worth
// pinning separately. If the regime gate were ever relaxed, this fixture would
// stop producing a trade and the cause would be two steps away from the
// symptom.
func TestTheOpposingOpinionIsDiscardedByTheRegimeGate(t *testing.T) {
	verdict := Decide(ConsensusInput{
		Policy:    DefaultConsensusPolicy(),
		Opinions:  bar119Opinions(),
		Regime:    string(domain.RegimeTrending),
		EventRisk: "none",
	})

	var found bool
	for _, c := range verdict.Contributions {
		if c.StrategyKey != "bollinger_zscore_reversion" {
			continue
		}
		found = true
		if c.Counted {
			t.Errorf("the opposing sell was counted; it declares RANGING only and "+
				"the regime is TRENDING. Contribution: %+v", c)
		}
	}
	if !found {
		t.Fatal("the opposing opinion is absent from the contributions. An operator " +
			"asking why it did not trade needs the discards, so every opinion has to " +
			"appear whether or not it counted")
	}

	// And the sell weight it would have carried never reached the tally.
	if !verdict.SellWeight.IsZero() {
		t.Errorf("sell weight is %s, so a gated-out opinion still moved the balance",
			verdict.SellWeight)
	}
}
