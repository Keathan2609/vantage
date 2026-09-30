package orchestrator

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/quant"
)

// A strategy that was never given enough history has not formed an opinion.
//
// Before this, it had: the research plane answered `no_trade` at confidence 0
// because the bars were short, the orchestrator could not tell that apart from
// a strategy that had looked and declined, and it recorded the refusal as an
// abstention and fed it to the consensus. Over the replay fixtures that was
// 69% of every signal on `trend_clean` and 92% on `drawdown`.

func TestAnAnswerCutShortByHistoryIsNotAnOpinion(t *testing.T) {
	refused, reason := historyRefusal(quant.SignalResponse{
		Action:              "no_trade",
		Confidence:          decimal.Zero,
		InsufficientHistory: true,
		RequiredBars:        120,
		Explanation:         "MACD Momentum requires 120 bars and received 63",
	}, 63)

	if !refused {
		t.Fatal("a response marked insufficient_history was treated as an opinion")
	}
	// The numbers belong in the recorded reason: an operator reading
	// strategy_runs must be able to see WHY without the research service.
	for _, want := range []string{"120", "63"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the skip reason does not say %q: %s", want, reason)
		}
	}
}

func TestAStrategyThatLookedAndDeclinedKeepsItsOpinion(t *testing.T) {
	// The opposite defect, and the more dangerous one: silencing real
	// abstentions would let a single agreeing strategy carry a decision that
	// the others had actively declined to join.
	refused, _ := historyRefusal(quant.SignalResponse{
		Action:              "no_trade",
		Confidence:          decimal.Zero,
		InsufficientHistory: false,
		RequiredBars:        120,
		Explanation:         "no crossover on the last bar; the fast average is above the slow one",
	}, 300)
	if refused {
		t.Fatal("a genuine abstention was discarded as a history refusal")
	}
}

func TestAnOlderResearchBuildIsReadAsTheWeakerClaim(t *testing.T) {
	// A research service that predates the field sends neither, which unmarshals
	// to the zero value. That must mean "it did not say", not "it was starved":
	// reading silence as a refusal would drop every opinion from an older build.
	refused, _ := historyRefusal(quant.SignalResponse{
		Action: "buy", Confidence: decimal.RequireFromString("0.72"),
	}, 300)
	if refused {
		t.Fatal("a response from a build that does not send the field was discarded")
	}
}

func TestAHistoryRefusalNeverReachesTheConsensus(t *testing.T) {
	// The reason the check sits before `valid` is set. A skipped evaluation
	// must contribute no opinion at all -- not a no_trade one -- because the
	// policy weights each opinion by 1/(strategies in its family), so a phantom
	// abstention halves the weight of the strategy that did look.
	evaluations := []Outcome{
		{Status: domain.RunSkipped, Action: domain.SignalNoTrade,
			SkipReason: "insufficient history: the strategy requires 120 completed bars"},
		evalFor("trend", domain.FamilyTrendFollowing, domain.SignalBuy, "0.80"),
	}

	opinions, fresh := opinionsFrom(evaluations)
	if len(opinions) != 1 || len(fresh) != 1 {
		t.Fatalf("a skipped evaluation became an opinion: %d opinions, %d fresh",
			len(opinions), len(fresh))
	}
	if !opinions[0].Weight.Equal(decimal.NewFromInt(1)) {
		t.Errorf("the surviving strategy's weight was diluted by a phantom "+
			"family member: got %s, want 1", opinions[0].Weight)
	}
}
