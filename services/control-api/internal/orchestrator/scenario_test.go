package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/marketdata"
)

// The brief's ten scenarios, driven by deterministic replay series.
//
// # What these do and do not prove
//
// They exercise the DECISION layer -- regime, the consensus policy, the veto
// rules -- against price series built by internal/marketdata's replay
// generators. Every one is reproducible and none depends on the wall clock or
// on the real market being open, which is the point: the previous milestone
// lost a whole test run to the venue's 17:00 New York maintenance break, and
// "clean trend" is not a condition you can wait for.
//
// They do NOT drive the full live pipeline end to end -- ingestion, the OMS,
// the mock venue, the ledger. Wiring the replay provider through internal/app
// needs configuration plumbing that does not exist yet, and that is recorded
// as remaining work rather than implied here. What is asserted below is that
// given a known market and a known context, the decision is the right one and
// is the same every time.
//
// The scenarios share one shape: build a series, derive the inputs a real run
// would derive from it, and assert the verdict. Where a scenario is about a
// veto rather than about price, the series is flat so the price path is not
// silently doing the work.

var scenarioStart = time.Date(2026, 3, 3, 8, 0, 0, 0, time.UTC)

func scenarioSpec(count int) marketdata.SeriesSpec {
	return marketdata.SeriesSpec{
		InstrumentID: "XAUUSD",
		Timeframe:    domain.Timeframe("1h"),
		Start:        scenarioStart,
		Interval:     time.Hour,
		StartPrice:   decimal.RequireFromString("2650"),
		Count:        count,
	}
}

// trendMeasures is a deliberately simple, deterministic read of a series,
// standing in for what the research plane's classifier computes.
//
// It is NOT a reimplementation of that classifier -- duplicating it here would
// make these tests assert their own arithmetic. It computes the two things the
// scenarios turn on, drift and range expansion, so the regime a scenario
// claims to represent is measured rather than asserted.
func trendMeasures(bars []domain.Bar) (drift, expansion decimal.Decimal) {
	if len(bars) < 20 {
		return decimal.Zero, decimal.Zero
	}
	first, last := bars[0].Open, bars[len(bars)-1].Close
	drift = last.Sub(first).Div(first)

	avg := func(from, to int) decimal.Decimal {
		total := decimal.Zero
		for _, b := range bars[from:to] {
			total = total.Add(b.High.Sub(b.Low))
		}
		return total.Div(decimal.NewFromInt(int64(to - from)))
	}
	early := avg(0, len(bars)/2)
	late := avg(len(bars)/2, len(bars))
	if early.IsZero() {
		return drift, decimal.Zero
	}
	expansion = late.Div(early)
	return drift, expansion
}

// scenarioInput assembles a consensus input for a scenario.
func scenarioInput(regime domain.Regime, opinions ...StrategyOpinion) ConsensusInput {
	return ConsensusInput{
		Policy:    DefaultConsensusPolicy(),
		Opinions:  opinions,
		Regime:    string(regime),
		EventRisk: "none",
	}
}

// ---------------------------------------------------------------------------
// A. Clean trend market
// ---------------------------------------------------------------------------

func TestScenarioA_CleanTrendIsIdentifiedAndTraded(t *testing.T) {
	bars := marketdata.TrendingSeries(scenarioSpec(120), decimal.RequireFromString("0.002"))
	drift, expansion := trendMeasures(bars)

	// The fixture has to actually be a trend, or the scenario proves nothing.
	if !drift.GreaterThan(decimal.RequireFromString("0.05")) {
		t.Fatalf("scenario A series drifted only %s; it is not a trend", drift)
	}
	if expansion.GreaterThan(decimal.NewFromInt(3)) {
		t.Fatalf("scenario A series is also a volatility event (%sx range expansion), "+
			"so a passing result would not be attributable to the trend", expansion)
	}

	trend := opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.80")
	trend.AllowedRegimes = []string{string(domain.RegimeTrending)}
	momentum := opinion("macd_momentum", "momentum", domain.SignalBuy, "0.74")
	momentum.AllowedRegimes = []string{string(domain.RegimeTrending)}

	v := Decide(scenarioInput(domain.RegimeTrending, trend, momentum))
	if v.Action != domain.SignalBuy {
		t.Fatalf("action = %s, want buy in a clean trend. Reason: %s", v.Action, v.Reason)
	}
	if !strings.Contains(v.Reason, "TRENDING") {
		t.Errorf("the reason does not record the regime: %s", v.Reason)
	}
}

// ---------------------------------------------------------------------------
// B. Range market
// ---------------------------------------------------------------------------

func TestScenarioB_ATrendStrategyDoesNotOvertradeARange(t *testing.T) {
	bars := marketdata.RangingSeries(scenarioSpec(120), decimal.RequireFromString("0.004"))
	drift, _ := trendMeasures(bars)
	if drift.Abs().GreaterThan(decimal.RequireFromString("0.01")) {
		t.Fatalf("scenario B series drifted %s; it is a trend, not a range", drift)
	}

	// The trend strategy is confident, and wrong about the market it is in.
	// The regime declaration is what stops it, not its own confidence.
	trend := opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.95")
	trend.AllowedRegimes = []string{string(domain.RegimeTrending)}

	v := Decide(scenarioInput(domain.RegimeRanging, trend))
	requireNoTrade(t, v, "a trend strategy in a ranging market")
	if v.Contributions[0].Counted {
		t.Error("the trend strategy was counted in a range")
	}

	// And the strategy built for a range is not blocked by the same rule --
	// otherwise the scenario would pass because nothing can ever trade.
	reversion := opinion("rsi_mean_reversion", "mean_reversion", domain.SignalSell, "0.72")
	reversion.AllowedRegimes = []string{string(domain.RegimeRanging)}
	bollinger := opinion("bollinger_zscore_reversion", "statistical", domain.SignalSell, "0.68")
	bollinger.AllowedRegimes = []string{string(domain.RegimeRanging)}

	if v := Decide(scenarioInput(domain.RegimeRanging, reversion, bollinger)); v.Action != domain.SignalSell {
		t.Errorf("a range strategy was refused in its own regime: %s", v.Reason)
	}
}

// ---------------------------------------------------------------------------
// C. Volatility shock
// ---------------------------------------------------------------------------

func TestScenarioC_AVolatilityShockIsRecognisedAndBlocksTrendEntry(t *testing.T) {
	bars := marketdata.VolatilityShockSeries(scenarioSpec(120), 60,
		decimal.RequireFromString("0.02"))
	_, expansion := trendMeasures(bars)
	if !expansion.GreaterThan(decimal.NewFromInt(4)) {
		t.Fatalf("scenario C series expanded only %sx; there is no shock to react to",
			expansion)
	}

	// A trend strategy that declares TRENDING and HIGH_VOLATILITY separately
	// is the realistic case: the shock takes it out of the regime it works in.
	trend := opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.85")
	trend.AllowedRegimes = []string{string(domain.RegimeTrending)}

	v := Decide(scenarioInput(domain.RegimeHighVolatility, trend))
	requireNoTrade(t, v, "the regime has become HIGH_VOLATILITY")

	// The volatility strategy is the one that belongs here.
	vol := opinion("atr_volatility_regime", "volatility", domain.SignalBuy, "0.70")
	vol.AllowedRegimes = []string{string(domain.RegimeHighVolatility)}
	vol2 := opinion("donchian_breakout", "breakout", domain.SignalBuy, "0.68")
	vol2.AllowedRegimes = []string{string(domain.RegimeHighVolatility)}
	if v := Decide(scenarioInput(domain.RegimeHighVolatility, vol, vol2)); v.Action != domain.SignalBuy {
		t.Errorf("a volatility strategy was refused in HIGH_VOLATILITY: %s", v.Reason)
	}
}

// ---------------------------------------------------------------------------
// D. High-impact economic event
// ---------------------------------------------------------------------------

func TestScenarioD_AHighImpactEventStopsEverything(t *testing.T) {
	// Flat series on purpose: the price path must not be doing the work, or
	// this would not be a test of the news policy.
	bars := marketdata.FlatSeries(scenarioSpec(120))
	drift, _ := trendMeasures(bars)
	if drift.Abs().GreaterThan(decimal.RequireFromString("0.005")) {
		t.Fatalf("scenario D series is not flat (drift %s)", drift)
	}

	in := scenarioInput(domain.RegimeTrending,
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.92"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.90"),
	)
	in.EventRisk = "high"

	v := Decide(in)
	requireNoTrade(t, v, "a high-impact release is inside the blackout")
	if !strings.Contains(strings.Join(v.Vetoes, " "), "event risk") {
		t.Errorf("vetoes = %v, want the event named", v.Vetoes)
	}
}

// ---------------------------------------------------------------------------
// E. Spread spike
// ---------------------------------------------------------------------------

func TestScenarioE_ASpreadSpikeRefusesANewTrade(t *testing.T) {
	// Built through the replay provider so the spike is a real widened book
	// the platform's own health check objects to, rather than a string the
	// test hands to the policy.
	r := marketdata.NewReplayProvider()
	if err := r.SetSeries("XAUUSD", marketdata.FlatSeries(scenarioSpec(60))); err != nil {
		t.Fatalf("SetSeries: %v", err)
	}
	inst := domain.Instrument{
		ID: "XAUUSD", Symbol: "XAUUSD",
		Spec: domain.InstrumentSpec{
			TickSize: decimal.RequireFromString("0.01"), PricePrecision: 2,
		},
	}
	r.SetSpreadFraction("XAUUSD", decimal.RequireFromString("0.02"))

	bar, _ := r.CurrentBar("XAUUSD")
	q, err := r.Quote(t.Context(), inst, bar.CloseTime)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	health := domain.EvaluateQuoteHealth(q, nil, domain.DefaultDataQualityPolicy(), bar.CloseTime)
	if !health.HasIssue(domain.IssueSpreadAbnormal) {
		t.Fatalf("the widened book was not flagged abnormal: %v", health.Issues)
	}

	// A real run turns that health verdict into a data block.
	in := scenarioInput(domain.RegimeTrending,
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.88"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.85"),
	)
	in.DataBlocks = []string{"the spread is " + health.SpreadPct.StringFixed(4) +
		" of mid, beyond the policy maximum"}

	v := Decide(in)
	requireNoTrade(t, v, "the spread is abnormal")
	if !strings.Contains(strings.Join(v.Vetoes, " "), "spread") {
		t.Errorf("vetoes = %v, want the spread named", v.Vetoes)
	}
}

// ---------------------------------------------------------------------------
// F. Data outage
// ---------------------------------------------------------------------------

func TestScenarioF_ADataOutageStopsAutomation(t *testing.T) {
	r := marketdata.NewReplayProvider()
	if err := r.SetSeries("XAUUSD", marketdata.FlatSeries(scenarioSpec(60))); err != nil {
		t.Fatalf("SetSeries: %v", err)
	}
	inst := domain.Instrument{ID: "XAUUSD", Symbol: "XAUUSD"}

	r.SetOutage(true)
	if _, err := r.Quote(t.Context(), inst, scenarioStart); err == nil {
		t.Fatal("the outage produced a quote")
	}

	// A run that cannot price the instrument has no business asking a
	// strategy for an opinion, and if it has one already it must not act.
	in := scenarioInput(domain.RegimeTrending,
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.95"),
	)
	in.DataBlocks = []string{"no quote is available: the provider is unreachable"}

	requireNoTrade(t, Decide(in), "the feed is down")
}

func TestScenarioF_AReplayedStalePriceAlsoStopsAutomation(t *testing.T) {
	// The subtler half of an outage: the provider comes BACK and replays an
	// old price. The quote is newly received, so a freshness check based only
	// on arrival time would pass it.
	r := marketdata.NewReplayProvider()
	if err := r.SetSeries("XAUUSD", marketdata.FlatSeries(scenarioSpec(60))); err != nil {
		t.Fatalf("SetSeries: %v", err)
	}
	inst := domain.Instrument{
		ID: "XAUUSD", Symbol: "XAUUSD",
		Spec: domain.InstrumentSpec{TickSize: decimal.RequireFromString("0.01")},
	}

	bar, _ := r.CurrentBar("XAUUSD")
	// Received now; the venue says it was true an hour ago.
	reconnectedAt := bar.CloseTime.Add(time.Hour)
	q, err := r.Quote(t.Context(), inst, reconnectedAt)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	health := domain.EvaluateQuoteHealth(q, nil, domain.DefaultDataQualityPolicy(), reconnectedAt)
	if health.Healthy() {
		t.Fatalf("an hour-old venue price read as healthy on reconnect "+
			"(received age %s, source age %s)", health.QuoteAge, health.SourceAge)
	}
	if !health.HasIssue(domain.IssueStaleQuote) {
		t.Errorf("issues = %v, want stale_quote", health.Issues)
	}
}

// ---------------------------------------------------------------------------
// G. Conflicting strategy signals
// ---------------------------------------------------------------------------

func TestScenarioG_ConflictingSignalsProduceNoTrade(t *testing.T) {
	// The series is a range, which is exactly the market that produces
	// genuine disagreement: a trend strategy sees a breakout, a reversion
	// strategy sees an extreme.
	bars := marketdata.RangingSeries(scenarioSpec(120), decimal.RequireFromString("0.005"))
	if len(bars) == 0 {
		t.Fatal("no series")
	}

	trend := opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.76")
	reversion := opinion("rsi_mean_reversion", "mean_reversion", domain.SignalSell, "0.74")

	v := Decide(scenarioInput(domain.RegimeRanging, trend, reversion))
	requireNoTrade(t, v, "two strategies point opposite ways with similar conviction")
	if !strings.Contains(strings.ToLower(v.Reason), "disagree") {
		t.Errorf("the reason does not name the disagreement: %s", v.Reason)
	}
	// Both weights are reported, so the closeness of the call is visible.
	if !v.BuyWeight.IsPositive() || !v.SellWeight.IsPositive() {
		t.Errorf("weights not reported: buy=%s sell=%s", v.BuyWeight, v.SellWeight)
	}
}

// ---------------------------------------------------------------------------
// H. Drawdown sequence
// ---------------------------------------------------------------------------

func TestScenarioH_ADrawdownSequenceBlocksFurtherRisk(t *testing.T) {
	bars := marketdata.DrawdownSeries(scenarioSpec(80), decimal.RequireFromString("0.004"))
	drift, _ := trendMeasures(bars)
	if !drift.LessThan(decimal.RequireFromString("-0.20")) {
		t.Fatalf("scenario H series fell only %s; a long position would not have lost "+
			"enough to trip a limit", drift)
	}

	// A run in this state derives a portfolio block from the risk engine's
	// daily-loss verdict. The strategies are still confident, which is the
	// point: the refusal comes from the account, not from the signal.
	in := scenarioInput(domain.RegimeTrending,
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.86"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.83"),
	)
	in.PortfolioBlocks = []string{"the daily loss limit has been reached"}

	v := Decide(in)
	requireNoTrade(t, v, "the account has hit its daily loss limit")

	// And the rule that matters most here: a REDUCING order must still be
	// possible. This is asserted directly in internal/risk, and named here
	// because scenario H is exactly the situation in which three separate
	// checks were once found trapping the operator in the losing position.
	if !strings.Contains(strings.Join(v.Vetoes, " "), "daily loss") {
		t.Errorf("vetoes = %v, want the daily-loss block named", v.Vetoes)
	}
}

// ---------------------------------------------------------------------------
// I. Strong ML signal but risk unavailable
// ---------------------------------------------------------------------------

func TestScenarioI_AStrongModelSignalWithNoRiskBudgetIsNoTrade(t *testing.T) {
	in := scenarioInput(domain.RegimeTrending,
		opinion("ml_direction_filter", "machine_learning", domain.SignalBuy, "0.94"),
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.88"),
	)
	in.Model = &ModelOpinion{
		ModelKey: "direction", Version: 4, Direction: domain.SignalBuy,
		Confidence: decimal.RequireFromString("0.93"), Available: true,
	}
	in.PortfolioBlocks = []string{
		"no risk budget remains: the account's open risk already equals its ceiling",
	}

	v := Decide(in)
	requireNoTrade(t, v, "there is no risk budget to size against")
	// The model agreed with the strategies and was confident. That must not
	// be able to reach past a portfolio veto.
	if v.Action != domain.SignalNoTrade {
		t.Error("a confident, agreeing model overrode the portfolio refusal")
	}
}

func TestScenarioI_AnUnavailableRequiredModelIsAlsoNoTrade(t *testing.T) {
	// The brief's model-failure list -- missing artifact, corrupted file, wrong
	// version, feature mismatch, inference timeout, NaN, out-of-range
	// probability -- all arrive as Available=false, and the answer is the same.
	in := scenarioInput(domain.RegimeTrending,
		opinion("ml_direction_filter", "machine_learning", domain.SignalBuy, "0.94"),
	)
	in.Model = &ModelOpinion{ModelKey: "direction", Available: false, Required: true}

	requireNoTrade(t, Decide(in), "a required model could not answer")
}

// ---------------------------------------------------------------------------
// J. Kill switch during autonomous operation
// ---------------------------------------------------------------------------

func TestScenarioJ_AKillSwitchOrAutopilotOffStopsNewOrders(t *testing.T) {
	// Both controls arrive at the decision layer as a data or portfolio block,
	// because both are established before a strategy is consulted. The
	// enforcement that actually matters happens inside the order transaction
	// in internal/oms, which is where a switch flipped mid-run is caught.
	in := scenarioInput(domain.RegimeTrending,
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.99"),
		opinion("macd_momentum", "momentum", domain.SignalBuy, "0.97"),
	)
	in.PortfolioBlocks = []string{"a global kill switch is active"}
	requireNoTrade(t, Decide(in), "a kill switch is active")

	in2 := scenarioInput(domain.RegimeTrending,
		opinion("ma_trend_crossover", "trend_following", domain.SignalBuy, "0.99"),
	)
	in2.PortfolioBlocks = []string{"the global Autopilot switch is off"}
	requireNoTrade(t, Decide(in2), "autopilot is off")
}

// ---------------------------------------------------------------------------
// Determinism across the whole set
// ---------------------------------------------------------------------------

func TestEveryScenarioIsReproducible(t *testing.T) {
	// The property that makes the whole suite worth having. If a scenario's
	// series or verdict varied between runs, a failure could not be told from
	// a changed fixture.
	build := func() []string {
		var out []string
		for _, c := range []struct {
			name string
			in   ConsensusInput
		}{
			{"A", scenarioInput(domain.RegimeTrending,
				opinion("trend", "trend_following", domain.SignalBuy, "0.80"),
				opinion("momentum", "momentum", domain.SignalBuy, "0.74"))},
			{"G", scenarioInput(domain.RegimeRanging,
				opinion("trend", "trend_following", domain.SignalBuy, "0.76"),
				opinion("reversion", "mean_reversion", domain.SignalSell, "0.74"))},
		} {
			v := Decide(c.in)
			out = append(out, c.name+":"+string(v.Action)+":"+v.Confidence.String()+":"+v.Reason)
		}
		// The series generators too: a scenario is only reproducible if its
		// market is.
		for _, bars := range [][]domain.Bar{
			marketdata.TrendingSeries(scenarioSpec(60), decimal.RequireFromString("0.002")),
			marketdata.RangingSeries(scenarioSpec(60), decimal.RequireFromString("0.004")),
			marketdata.VolatilityShockSeries(scenarioSpec(60), 30, decimal.RequireFromString("0.02")),
			marketdata.DrawdownSeries(scenarioSpec(60), decimal.RequireFromString("0.004")),
		} {
			out = append(out, bars[len(bars)-1].Close.String())
		}
		return out
	}

	first := build()
	for k := 0; k < 10; k++ {
		got := build()
		for i := range first {
			if got[i] != first[i] {
				t.Fatalf("run %d differed at position %d:\n  %s\n  %s", k, i, got[i], first[i])
			}
		}
	}
}

func TestNoTradeIsTheCommonOutcomeAcrossTheScenarios(t *testing.T) {
	// A sanity check on the posture rather than on any single rule. Of the ten
	// scenarios, most are conditions under which trading is wrong, and a
	// policy that traded through them would be the defect.
	//
	// It is asserted as a floor, not an exact count: the point is that the
	// default posture is refusal, not that a particular number of scenarios
	// refuse.
	refusals, total := 0, 0
	for _, in := range []ConsensusInput{
		// B: trend strategy in a range.
		func() ConsensusInput {
			o := opinion("trend", "trend_following", domain.SignalBuy, "0.95")
			o.AllowedRegimes = []string{string(domain.RegimeTrending)}
			return scenarioInput(domain.RegimeRanging, o)
		}(),
		// D: high-impact event.
		func() ConsensusInput {
			i := scenarioInput(domain.RegimeTrending,
				opinion("trend", "trend_following", domain.SignalBuy, "0.92"))
			i.EventRisk = "high"
			return i
		}(),
		// E: spread spike.
		func() ConsensusInput {
			i := scenarioInput(domain.RegimeTrending,
				opinion("trend", "trend_following", domain.SignalBuy, "0.88"))
			i.DataBlocks = []string{"spread abnormal"}
			return i
		}(),
		// F: outage.
		func() ConsensusInput {
			i := scenarioInput(domain.RegimeTrending,
				opinion("trend", "trend_following", domain.SignalBuy, "0.95"))
			i.DataBlocks = []string{"no quote available"}
			return i
		}(),
		// G: conflict.
		scenarioInput(domain.RegimeRanging,
			opinion("trend", "trend_following", domain.SignalBuy, "0.76"),
			opinion("reversion", "mean_reversion", domain.SignalSell, "0.74")),
		// H: drawdown.
		func() ConsensusInput {
			i := scenarioInput(domain.RegimeTrending,
				opinion("trend", "trend_following", domain.SignalBuy, "0.86"))
			i.PortfolioBlocks = []string{"daily loss limit reached"}
			return i
		}(),
		// I: no risk budget.
		func() ConsensusInput {
			i := scenarioInput(domain.RegimeTrending,
				opinion("ml", "machine_learning", domain.SignalBuy, "0.94"))
			i.PortfolioBlocks = []string{"no risk budget"}
			return i
		}(),
		// J: kill switch.
		func() ConsensusInput {
			i := scenarioInput(domain.RegimeTrending,
				opinion("trend", "trend_following", domain.SignalBuy, "0.99"))
			i.PortfolioBlocks = []string{"kill switch active"}
			return i
		}(),
	} {
		total++
		if Decide(in).Action == domain.SignalNoTrade {
			refusals++
		}
	}
	if refusals != total {
		t.Errorf("%d of %d refusal scenarios were refused; every one of these is a "+
			"condition under which trading is wrong", refusals, total)
	}
}
