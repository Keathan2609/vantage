package orchestrator

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
)

// The consensus policy is where a set of disagreeing opinions becomes one
// decision about money, and it has more interacting rules than anything else
// in the platform. It is a pure function precisely so that every one of those
// interactions can be asserted here rather than discovered in production.

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// opinion builds a counted-by-default strategy opinion.
func opinion(key, family string, action domain.SignalAction, confidence string) StrategyOpinion {
	return StrategyOpinion{
		StrategyKey: key, Family: family, Version: 1,
		Action: action, Confidence: d(confidence), Weight: decimal.NewFromInt(1),
	}
}

func baseInput(opinions ...StrategyOpinion) ConsensusInput {
	return ConsensusInput{
		Policy:   DefaultConsensusPolicy(),
		Opinions: opinions,
		Regime:   string(domain.RegimeTrending),
		// "none" rather than "" on purpose: the caller always knows the event
		// risk, and an empty string would silently mean the same as none.
		EventRisk: "none",
	}
}

func requireNoTrade(t *testing.T, v Verdict, because string) {
	t.Helper()
	if v.Action != domain.SignalNoTrade {
		t.Fatalf("action = %s, want no_trade (%s). Reason given: %s", v.Action, because, v.Reason)
	}
	if v.Traded() {
		t.Error("Traded() reported true for a no_trade verdict")
	}
	if v.Reason == "" {
		t.Error("a NO TRADE verdict carried no reason, so nobody can tell why the system did nothing")
	}
	if len(v.Rationale) == 0 {
		t.Error("a NO TRADE verdict carried no rationale")
	}
}

// ---------------------------------------------------------------------------
// The brief's worked example
// ---------------------------------------------------------------------------

func TestTheWorkedExampleProducesNoTrade(t *testing.T) {
	// Trend = BUY 0.75, Mean Reversion = HOLD 0.45, ML regime = RISK_OFF 0.82,
	// news risk = HIGH. The expected answer is NO TRADE, and it must be NO
	// TRADE for stated reasons rather than by accident.
	in := ConsensusInput{
		Policy: DefaultConsensusPolicy(),
		Opinions: []StrategyOpinion{
			opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.75"),
			opinion("rsi_mean_reversion", "mean_reversion", domain.SignalHold, "0.45"),
		},
		Model: &ModelOpinion{
			ModelKey: "regime_classifier", Version: 3,
			Regime: string(domain.RegimeRiskOff), Confidence: d("0.82"), Available: true,
		},
		Regime:    string(domain.RegimeTrending),
		EventRisk: "high",
	}

	v := Decide(in)
	requireNoTrade(t, v, "a RISK_OFF model and a high-impact release")

	// Both refusals must be recorded. Reporting only the first would leave an
	// operator fixing one problem and expecting the trade to proceed.
	joined := strings.Join(v.Vetoes, " | ")
	if !strings.Contains(joined, "RISK_OFF") {
		t.Errorf("the RISK_OFF model veto is missing from %q", joined)
	}
	if !strings.Contains(joined, "event risk") {
		t.Errorf("the event-risk veto is missing from %q", joined)
	}
	if len(v.Vetoes) < 2 {
		t.Errorf("vetoes = %d, want both the model and the event", len(v.Vetoes))
	}

	// The trend strategy's opinion is still recorded, marked as counted, so
	// the record shows there WAS an edge that was refused rather than absent.
	var sawTrend bool
	for _, c := range v.Contributions {
		if c.StrategyKey == "ma_trend_crossover" {
			sawTrend = true
			if !c.Counted {
				t.Errorf("the trend opinion was not counted: %s", c.Note)
			}
		}
	}
	if !sawTrend {
		t.Error("the trend strategy's opinion is missing from the contributions")
	}
}

func TestCompatibleConsensusTrades(t *testing.T) {
	// The other half of the same test: when the strategies agree, conditions
	// are clean and confidence is real, the policy must actually produce a
	// decision. A policy that only ever says NO TRADE is not safe, it is
	// broken.
	v := Decide(baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.78"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.71"),
		opinion("donchian_breakout", "breakout", domain.SignalBuy, "0.66"),
	))

	if v.Action != domain.SignalBuy {
		t.Fatalf("action = %s, want buy. Reason: %s", v.Action, v.Reason)
	}
	if !v.Traded() {
		t.Error("Traded() reported false for an actionable verdict")
	}
	if !v.Confidence.IsPositive() {
		t.Errorf("confidence = %s, want positive", v.Confidence)
	}
	if v.PolicyVersion != PolicyVersion {
		t.Errorf("policy version = %d, want %d", v.PolicyVersion, PolicyVersion)
	}
	if !v.SellWeight.IsZero() {
		t.Errorf("sell weight = %s with no sell opinions", v.SellWeight)
	}
}

// ---------------------------------------------------------------------------
// Disagreement is not resolved by majority
// ---------------------------------------------------------------------------

func TestABuySellSplitIsNoTradeEvenWhenOneSideHasMoreWeight(t *testing.T) {
	// Two buys against one sell is a majority for buy. It is still a
	// disagreement, and the policy refuses rather than trading the margin.
	v := Decide(baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.80"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.75"),
		opinion("rsi_mean_reversion", "mean_reversion", domain.SignalSell, "0.70"),
	))

	requireNoTrade(t, v, "two strategies disagree with a third")
	if !strings.Contains(strings.ToLower(v.Reason), "disagree") {
		t.Errorf("the reason does not name the disagreement: %s", v.Reason)
	}
	// Both weights are surfaced so a close call is visible.
	if !v.BuyWeight.IsPositive() || !v.SellWeight.IsPositive() {
		t.Errorf("weights were not reported: buy=%s sell=%s", v.BuyWeight, v.SellWeight)
	}
}

func TestATinyOpposingWeightDoesNotBlockAClearConsensus(t *testing.T) {
	// The opposite failure: refusing on any dissent at all would mean a single
	// weakly-weighted contrarian could veto everything, which is its own kind
	// of unsafe because it makes the system unpredictable.
	in := baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.85"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.82"),
		opinion("multi_timeframe_trend", "multi_timeframe", domain.SignalBuy, "0.80"),
	)
	dissent := opinion("rsi_mean_reversion", "mean_reversion", domain.SignalSell, "0.56")
	dissent.Weight = d("0.10")
	in.Opinions = append(in.Opinions, dissent)

	v := Decide(in)
	if v.Action != domain.SignalBuy {
		t.Fatalf("action = %s, want buy; a 4%% dissent should not veto. Reason: %s",
			v.Action, v.Reason)
	}
}

func TestHoldAndCloseAreAbstentionsNotAgreement(t *testing.T) {
	// A single buy plus two holds must not read as "nobody disagreed,
	// therefore consensus". The holds abstain, and one opinion is measured on
	// its own merits.
	v := Decide(baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.58"),
		opinion("rsi_mean_reversion", "mean_reversion", domain.SignalHold, "0.90"),
		opinion("bollinger_zscore_reversion", "statistical", domain.SignalClose, "0.95"),
	))

	counted := 0
	for _, c := range v.Contributions {
		if c.Counted {
			counted++
		}
		if c.Action == domain.SignalHold || c.Action == domain.SignalClose {
			if c.Counted {
				t.Errorf("%s was counted as a directional vote", c.Action)
			}
			if !strings.Contains(c.Note, "abstained") {
				t.Errorf("%s note = %q, want it recorded as an abstention", c.Action, c.Note)
			}
		}
	}
	if counted != 1 {
		t.Errorf("counted opinions = %d, want 1", counted)
	}
	// 0.58 alone: above MinConfidence, below MinNetConfidence once it is the
	// only voice. NO TRADE for a stated reason.
	requireNoTrade(t, v, "one weakly-confident opinion is not a consensus")
}

func TestNoOpinionsAtAllIsNoTrade(t *testing.T) {
	v := Decide(baseInput())
	requireNoTrade(t, v, "nothing offered an opinion")
	if len(v.Vetoes) != 0 {
		t.Errorf("an empty opinion set produced vetoes: %v", v.Vetoes)
	}
}

// ---------------------------------------------------------------------------
// Vetoes
// ---------------------------------------------------------------------------

func TestAHighImpactEventVetoesEvenAUnanimousStrongConsensus(t *testing.T) {
	in := baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.99"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.98"),
	)
	in.EventRisk = "high"

	v := Decide(in)
	requireNoTrade(t, v, "a high-impact release is inside the blackout")
	if !strings.Contains(strings.Join(v.Vetoes, " "), "event risk") {
		t.Errorf("vetoes = %v, want an event-risk veto", v.Vetoes)
	}
}

func TestMediumEventRiskRaisesTheBarWithoutVetoing(t *testing.T) {
	// A release that is close but not imminent should make the platform
	// choosier, not blind. Two behaviours in one rule, so both are asserted.
	strong := baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.95"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.93"),
	)
	strong.EventRisk = "medium"
	if v := Decide(strong); v.Action != domain.SignalBuy {
		t.Errorf("a very strong consensus was refused under medium event risk: %s", v.Reason)
	}

	marginal := baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.62"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.61"),
	)
	if v := Decide(marginal); v.Action != domain.SignalBuy {
		t.Fatalf("the control case was refused with no event risk: %s", v.Reason)
	}
	marginal.EventRisk = "medium"
	v := Decide(marginal)
	requireNoTrade(t, v, "a marginal consensus under medium event risk")
	if len(v.Vetoes) != 0 {
		t.Errorf("medium event risk produced a veto rather than a raised bar: %v", v.Vetoes)
	}
}

func TestADataBlockIsAVeto(t *testing.T) {
	in := baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.90"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.88"),
	)
	in.DataBlocks = []string{"the quote is 47s old, beyond the 10s the policy allows"}

	v := Decide(in)
	requireNoTrade(t, v, "the market data is stale")
	if !strings.Contains(strings.Join(v.Vetoes, " "), "data:") {
		t.Errorf("vetoes = %v, want the data block labelled as such", v.Vetoes)
	}
}

func TestAPortfolioBlockIsAVetoEvenForAStrongSignal(t *testing.T) {
	// The brief's case: a strong individual signal may still be NO TRADE
	// because of portfolio risk. Gold and silver are one trade twice.
	in := baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.92"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.90"),
	)
	in.PortfolioBlocks = []string{
		"XAGUSD is already held long and is highly correlated with XAUUSD",
	}

	v := Decide(in)
	requireNoTrade(t, v, "correlated exposure is already held")
	if !strings.Contains(strings.Join(v.Vetoes, " "), "portfolio:") {
		t.Errorf("vetoes = %v, want the portfolio block labelled as such", v.Vetoes)
	}
}

func TestEveryVetoIsReportedNotJustTheFirst(t *testing.T) {
	in := baseInput(opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.9"))
	in.EventRisk = "high"
	in.DataBlocks = []string{"spread is 0.9% of mid"}
	in.PortfolioBlocks = []string{"no risk budget remains today"}

	v := Decide(in)
	if len(v.Vetoes) != 3 {
		t.Fatalf("vetoes = %d (%v), want all three", len(v.Vetoes), v.Vetoes)
	}
	// The single-sentence reason has to say there are others, or an operator
	// fixes one thing and expects to trade.
	if !strings.Contains(v.Reason, "other refusal") {
		t.Errorf("reason = %q, does not mention the remaining refusals", v.Reason)
	}
}

// ---------------------------------------------------------------------------
// Model interaction
// ---------------------------------------------------------------------------

func TestAConfidentRiskOffModelVetoes(t *testing.T) {
	in := baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.88"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.85"),
	)
	in.Model = &ModelOpinion{
		ModelKey: "regime", Regime: string(domain.RegimeRiskOff),
		Confidence: d("0.70"), Available: true,
	}

	requireNoTrade(t, Decide(in), "the model reads RISK_OFF confidently")
}

func TestAnUnconfidentRiskOffModelDoesNotVeto(t *testing.T) {
	// The threshold has to work in both directions, or it is not a threshold.
	// A model that is barely leaning RISK_OFF is not evidence.
	in := baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.88"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.85"),
	)
	in.Model = &ModelOpinion{
		ModelKey: "regime", Regime: string(domain.RegimeRiskOff),
		Confidence: d("0.40"), Available: true,
	}

	if v := Decide(in); v.Action != domain.SignalBuy {
		t.Errorf("a 0.40-confidence RISK_OFF read vetoed the trade: %s", v.Reason)
	}
}

func TestAModelPointingTheOtherWayVetoesTheDirection(t *testing.T) {
	in := baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.85"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.80"),
	)
	in.Model = &ModelOpinion{
		ModelKey: "direction", Direction: domain.SignalSell,
		Confidence: d("0.90"), Available: true,
	}

	v := Decide(in)
	requireNoTrade(t, v, "the model contradicts the strategies")
	if !strings.Contains(strings.Join(v.Vetoes, " "), "against the strategies") {
		t.Errorf("vetoes = %v, want the contradiction named", v.Vetoes)
	}
}

func TestAModelAgreeingWithTheStrategiesDoesNotVeto(t *testing.T) {
	in := baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.80"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.78"),
	)
	in.Model = &ModelOpinion{
		ModelKey: "direction", Direction: domain.SignalBuy,
		Confidence: d("0.95"), Available: true,
	}

	if v := Decide(in); v.Action != domain.SignalBuy {
		t.Errorf("an agreeing model blocked the trade: %s", v.Reason)
	}
}

func TestARequiredButUnavailableModelIsAVeto(t *testing.T) {
	// The brief's failure list: missing artifact, corrupted model, wrong
	// version, feature mismatch, inference timeout, NaN. Every one of them
	// arrives here as Available=false, and the answer is NO TRADE.
	in := baseInput(
		opinion("ml_direction_filter", "machine_learning", domain.SignalBuy, "0.90"),
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.85"),
	)
	in.Model = &ModelOpinion{ModelKey: "direction", Available: false, Required: true}

	v := Decide(in)
	requireNoTrade(t, v, "a required model could not answer")
	if !strings.Contains(strings.Join(v.Vetoes, " "), "required") {
		t.Errorf("vetoes = %v, want the requirement named", v.Vetoes)
	}
}

func TestAnUnavailableOptionalModelIsRecordedButNotFatal(t *testing.T) {
	// The difference from the previous test is the whole point: a model that
	// was never load-bearing must not stop trading, but its silence must still
	// appear in the record rather than being read as assent.
	in := baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.85"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.82"),
	)
	in.Model = &ModelOpinion{ModelKey: "direction", Available: false, Required: false}

	v := Decide(in)
	if v.Action != domain.SignalBuy {
		t.Fatalf("an optional unavailable model blocked the trade: %s", v.Reason)
	}
	if !strings.Contains(strings.Join(v.Rationale, " "), "unavailable") {
		t.Errorf("rationale = %v, does not record that the model was silent", v.Rationale)
	}
}

// ---------------------------------------------------------------------------
// Regime handling
// ---------------------------------------------------------------------------

func TestAStrategyIsDiscardedInARegimeItDoesNotDeclare(t *testing.T) {
	trend := opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.90")
	trend.AllowedRegimes = []string{string(domain.RegimeTrending)}
	reversion := opinion("rsi_mean_reversion", "mean_reversion", domain.SignalBuy, "0.88")
	reversion.AllowedRegimes = []string{string(domain.RegimeRanging)}

	in := baseInput(trend, reversion)
	in.Regime = string(domain.RegimeRanging)

	v := Decide(in)
	for _, c := range v.Contributions {
		switch c.StrategyKey {
		case "ma_trend_crossover":
			if c.Counted {
				t.Error("a trend-only strategy was counted in a RANGING market")
			}
			if !strings.Contains(c.Note, "RANGING") {
				t.Errorf("the discard note does not name the regime: %q", c.Note)
			}
		case "rsi_mean_reversion":
			if !c.Counted {
				t.Errorf("the range strategy was discarded in its own regime: %s", c.Note)
			}
		}
	}
}

func TestUnknownRegimeDiscardsEveryStrategyThatRequiresAKnownOne(t *testing.T) {
	// The brief is explicit: UNKNOWN must not be coerced into another regime
	// to force trading. A strategy that declared TRENDING gets no trade here.
	trend := opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.95")
	trend.AllowedRegimes = []string{string(domain.RegimeTrending), string(domain.RegimeHighVolatility)}

	in := baseInput(trend)
	in.Regime = string(domain.RegimeUnknown)

	v := Decide(in)
	requireNoTrade(t, v, "the regime is unknown and the strategy requires a known one")
	if v.Contributions[0].Counted {
		t.Error("a regime-requiring strategy was counted in an UNKNOWN market")
	}
}

func TestAStrategyThatDeclaresNoRegimeRunsAnywhereAndSaysSo(t *testing.T) {
	// An absent declaration means the strategy never made a claim. It is
	// allowed, but trading an uncharacterised market is a choice and has to
	// appear in the rationale.
	in := baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.85"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.82"),
	)
	in.Regime = string(domain.RegimeUnknown)

	v := Decide(in)
	if v.Action != domain.SignalBuy {
		t.Fatalf("action = %s, want buy. Reason: %s", v.Action, v.Reason)
	}
	if !strings.Contains(strings.Join(v.Rationale, " "), "UNKNOWN") {
		t.Errorf("rationale = %v, does not record that the regime was unknown", v.Rationale)
	}
}

func TestAnyIsAcceptedAsARegimeDeclaration(t *testing.T) {
	o := opinion("ensemble_weighted_vote", "ensemble", domain.SignalBuy, "0.85")
	o.AllowedRegimes = []string{"any"}
	o2 := opinion("macd_momentum", "momentum", domain.SignalBuy, "0.83")
	o2.AllowedRegimes = []string{"ANY"}

	in := baseInput(o, o2)
	in.Regime = string(domain.RegimeHighVolatility)
	if v := Decide(in); v.Action != domain.SignalBuy {
		t.Errorf("a strategy declaring \"any\" was discarded: %s", v.Reason)
	}
}

func TestRegimeMatchingIsCaseInsensitive(t *testing.T) {
	// The research plane sends a string. A case difference must not silently
	// mean "this strategy is valid nowhere".
	o := opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.90")
	o.AllowedRegimes = []string{"trending"}
	o2 := opinion("macd_momentum", "momentum", domain.SignalBuy, "0.88")
	o2.AllowedRegimes = []string{"Trending"}

	in := baseInput(o, o2)
	in.Regime = "TRENDING"
	if v := Decide(in); v.Action != domain.SignalBuy {
		t.Errorf("case-different regime names did not match: %s", v.Reason)
	}
}

// ---------------------------------------------------------------------------
// Confidence and weighting
// ---------------------------------------------------------------------------

func TestLowConfidenceOpinionsAreDiscardedWithTheirNumbers(t *testing.T) {
	v := Decide(baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.30"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.20"),
	))

	requireNoTrade(t, v, "every opinion was below the confidence floor")
	for _, c := range v.Contributions {
		if c.Counted {
			t.Errorf("%s was counted at confidence %s", c.StrategyKey, c.Confidence)
		}
		if !strings.Contains(c.Note, "below") {
			t.Errorf("discard note = %q, does not state the comparison", c.Note)
		}
	}
}

func TestAZeroOrNegativeWeightExcludesAnOpinionRatherThanDefaultingToOne(t *testing.T) {
	// A caller that forgets to set the weight must not accidentally get a
	// full-weight vote. The zero value excludes.
	zero := StrategyOpinion{
		StrategyKey: "unweighted", Action: domain.SignalBuy, Confidence: d("0.99"),
	}
	negative := opinion("negative", "trend_following", domain.SignalBuy, "0.99")
	negative.Weight = d("-1")

	v := Decide(baseInput(zero, negative))
	requireNoTrade(t, v, "no opinion carried a usable weight")
	for _, c := range v.Contributions {
		if c.Counted {
			t.Errorf("%s was counted with weight %s", c.StrategyKey, c.Weight)
		}
	}
}

func TestWeightScalesInfluence(t *testing.T) {
	// A heavily-weighted sell against a lightly-weighted buy must come out
	// sell, not buy -- weight has to actually do something or the parameter is
	// decorative.
	buy := opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.80")
	buy.Weight = d("0.05")
	sell := opinion("rsi_mean_reversion", "mean_reversion", domain.SignalSell, "0.80")
	sell.Weight = d("2.0")

	v := Decide(baseInput(buy, sell))
	if v.Action != domain.SignalSell {
		t.Errorf("action = %s, want sell: the sell carried 40x the weight. Reason: %s",
			v.Action, v.Reason)
	}
}

func TestNetConfidenceIsNotSimplyTheHighestConfidence(t *testing.T) {
	// One strong opinion alongside dissent must score lower than the same
	// opinion with agreement, or "consensus" means nothing.
	alone := Decide(baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.90"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.90"),
	))
	if alone.Action != domain.SignalBuy {
		t.Fatalf("the agreeing case did not trade: %s", alone.Reason)
	}

	contested := baseInput(
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.90"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.90"),
	)
	dissent := opinion("rsi_mean_reversion", "mean_reversion", domain.SignalSell, "0.60")
	dissent.Weight = d("0.15")
	contested.Opinions = append(contested.Opinions, dissent)

	contestedVerdict := Decide(contested)
	if contestedVerdict.Action != domain.SignalBuy {
		t.Fatalf("the contested case should still trade at this dissent level: %s",
			contestedVerdict.Reason)
	}
	if !contestedVerdict.Confidence.LessThan(alone.Confidence) {
		t.Errorf("contested confidence %s is not below unanimous confidence %s",
			contestedVerdict.Confidence, alone.Confidence)
	}
}

func TestConfidenceNeverExceedsOne(t *testing.T) {
	// It is rendered as a percentage and compared against thresholds. A value
	// above 1 would be meaningless in both places.
	v := Decide(baseInput(
		opinion("a", "trend_following", domain.SignalBuy, "1.0"),
		opinion("b", "momentum", domain.SignalBuy, "1.0"),
		opinion("c", "breakout", domain.SignalBuy, "1.0"),
		opinion("d", "multi_timeframe", domain.SignalBuy, "1.0"),
	))
	if v.Confidence.GreaterThan(decimal.NewFromInt(1)) {
		t.Errorf("confidence = %s, above 1", v.Confidence)
	}
}

// ---------------------------------------------------------------------------
// Determinism and explainability
// ---------------------------------------------------------------------------

func TestTheSameInputAlwaysProducesTheSameVerdict(t *testing.T) {
	// Reproducibility is a stated requirement of this milestone: the same
	// inputs must yield the same decision, or a decision snapshot cannot be
	// replayed to explain itself.
	build := func() ConsensusInput {
		in := baseInput(
			opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.80"),
			opinion("macd_momentum", "momentum", domain.SignalBuy, "0.75"),
			opinion("rsi_mean_reversion", "mean_reversion", domain.SignalSell, "0.60"),
		)
		in.Model = &ModelOpinion{ModelKey: "m", Direction: domain.SignalBuy,
			Confidence: d("0.7"), Available: true}
		return in
	}

	first := Decide(build())
	for k := 0; k < 20; k++ {
		got := Decide(build())
		if got.Action != first.Action || !got.Confidence.Equal(first.Confidence) {
			t.Fatalf("run %d differed: %s@%s vs %s@%s",
				k, got.Action, got.Confidence, first.Action, first.Confidence)
		}
		if got.Reason != first.Reason {
			t.Fatalf("run %d gave a different reason:\n%s\n%s", k, got.Reason, first.Reason)
		}
	}
}

func TestVetoOrderingIsStableSoTheReasonDoesNotFlap(t *testing.T) {
	// The vetoes are sorted before the headline reason is built. Without that,
	// map iteration order would change which refusal an operator sees for
	// identical conditions.
	build := func() ConsensusInput {
		in := baseInput(opinion("a", "trend_following", domain.SignalBuy, "0.9"))
		in.EventRisk = "high"
		in.DataBlocks = []string{"zzz last alphabetically"}
		in.PortfolioBlocks = []string{"aaa first alphabetically"}
		return in
	}
	first := Decide(build()).Reason
	for k := 0; k < 20; k++ {
		if got := Decide(build()).Reason; got != first {
			t.Fatalf("run %d headline reason changed:\n%s\n%s", k, got, first)
		}
	}
}

func TestEveryOpinionAppearsInTheContributionsWithANote(t *testing.T) {
	// The record has to account for every input. An opinion that vanished
	// without explanation is the thing that makes a decision unexplainable.
	in := baseInput(
		opinion("counted", "trend_following", domain.SignalBuy, "0.85"),
		opinion("low_confidence", "momentum", domain.SignalBuy, "0.10"),
		opinion("abstaining", "mean_reversion", domain.SignalHold, "0.90"),
	)
	regimeBound := opinion("wrong_regime", "breakout", domain.SignalBuy, "0.90")
	regimeBound.AllowedRegimes = []string{string(domain.RegimeRanging)}
	in.Opinions = append(in.Opinions, regimeBound)

	v := Decide(in)
	if len(v.Contributions) != len(in.Opinions) {
		t.Fatalf("contributions = %d, want one per opinion (%d)",
			len(v.Contributions), len(in.Opinions))
	}
	for _, c := range v.Contributions {
		if c.Note == "" {
			t.Errorf("%s has no note", c.StrategyKey)
		}
	}
}

func TestTheVerdictAlwaysCarriesThePolicyVersion(t *testing.T) {
	// Without it, a stored decision cannot be interpreted after the rules
	// change.
	for _, in := range []ConsensusInput{
		baseInput(),
		baseInput(opinion("a", "trend_following", domain.SignalBuy, "0.9")),
		func() ConsensusInput {
			i := baseInput(opinion("a", "trend_following", domain.SignalBuy, "0.9"))
			i.EventRisk = "high"
			return i
		}(),
	} {
		if v := Decide(in); v.PolicyVersion != PolicyVersion {
			t.Errorf("policy version = %d, want %d", v.PolicyVersion, PolicyVersion)
		}
	}
}

func TestAStricterPolicyRefusesWhatTheDefaultAllows(t *testing.T) {
	// The thresholds have to be genuinely configurable, or "versioned and
	// configurable" is not true of the policy.
	opinions := []StrategyOpinion{
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.70"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.68"),
	}

	relaxed := baseInput(opinions...)
	if v := Decide(relaxed); v.Action != domain.SignalBuy {
		t.Fatalf("the default policy refused the control case: %s", v.Reason)
	}

	strict := baseInput(opinions...)
	strict.Policy.MinConfidence = d("0.90")
	if v := Decide(strict); v.Action != domain.SignalNoTrade {
		t.Errorf("a 0.90 confidence floor still traded 0.70 opinions: %s", v.Reason)
	}

	noDissent := baseInput(opinions...)
	noDissent.Policy.MaxOpposingWeightFraction = decimal.Zero
	dissent := opinion("contrarian", "mean_reversion", domain.SignalSell, "0.60")
	dissent.Weight = d("0.01")
	noDissent.Opinions = append(noDissent.Opinions, dissent)
	if v := Decide(noDissent); v.Action != domain.SignalNoTrade {
		t.Errorf("a zero-tolerance policy traded through dissent: %s", v.Reason)
	}
}

// ---------------------------------------------------------------------------
// Regime helpers
// ---------------------------------------------------------------------------

func TestParseRegimeNormalisesAndFailsClosed(t *testing.T) {
	for _, raw := range []string{"TRENDING", "trending", " Trending ", "tReNdInG"} {
		got, ok := domain.ParseRegime(raw)
		if !ok || got != domain.RegimeTrending {
			t.Errorf("ParseRegime(%q) = %s,%v want TRENDING,true", raw, got, ok)
		}
	}
	for _, raw := range []string{"", "BULLISH", "sideways", "trend"} {
		got, ok := domain.ParseRegime(raw)
		if ok {
			t.Errorf("ParseRegime(%q) reported a recognised regime", raw)
		}
		if got != domain.RegimeUnknown {
			t.Errorf("ParseRegime(%q) = %s, want UNKNOWN; an unrecognised regime is "+
				"exactly a market nobody has characterised", raw, got)
		}
	}
}

func TestOnlyCharacterisedRegimesAreTradable(t *testing.T) {
	// EVENT_RISK, RISK_OFF and UNKNOWN must all be untradable, for the three
	// different reasons documented on the type.
	for _, r := range []domain.Regime{
		domain.RegimeEventRisk, domain.RegimeRiskOff, domain.RegimeUnknown,
	} {
		if r.Tradable() {
			t.Errorf("%s reports itself tradable", r)
		}
	}
	for _, r := range []domain.Regime{
		domain.RegimeTrending, domain.RegimeRanging,
		domain.RegimeHighVolatility, domain.RegimeLowVolatility,
	} {
		if !r.Tradable() {
			t.Errorf("%s reports itself untradable", r)
		}
	}
	if domain.RegimeUnknown.Known() {
		t.Error("UNKNOWN reports itself as known")
	}
	if !domain.RegimeTrending.Known() {
		t.Error("TRENDING reports itself as unknown")
	}
}

func TestEveryRegimeDescribesItself(t *testing.T) {
	// The description is rendered next to a decision. A regime with no
	// sentence would show an operator a bare enum.
	for _, r := range domain.AllRegimes() {
		if len(r.Describe()) < 10 {
			t.Errorf("%s has no usable description: %q", r, r.Describe())
		}
	}
}
