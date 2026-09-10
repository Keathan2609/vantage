package domain

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// RISK_OFF was previously a label nothing could produce. These tests are about
// two things: that it can now be inferred from evidence, and that every
// verdict carries enough of that evidence to be argued with. A protective
// state an operator cannot interrogate is a state they will eventually switch
// off.

// healthyEvidence is a market in which nothing is wrong. Every test starts
// here and breaks exactly one thing, so a verdict can be attributed to that
// one thing.
func healthyEvidence() RegimeEvidence {
	return RegimeEvidence{
		Reported: RegimeTrending,
		Health: MarketDataHealth{
			InstrumentID: "XAUUSD.m",
			State:        DataQualityOK,
			SpreadPct:    dec("0.0002"),
		},
		SpreadBaseline:     dec("0.0002"),
		Blackout:           false,
		DrawdownFraction:   dec("0.01"),
		ProviderReconnects: 0,
		HasQuote:           true,
		BarsAvailable:      200,
	}
}

func reasonFor(a RegimeAssessment, code RegimeReasonCode) (RegimeReason, bool) {
	for _, r := range a.Reasons {
		if r.Code == code {
			return r, true
		}
	}
	return RegimeReason{}, false
}

func TestAHealthyMarketKeepsTheReportedRegime(t *testing.T) {
	// The classifier must not invent RISK_OFF. If it fired on an ordinary
	// market the state would be worthless, and the first thing anybody did
	// would be to disable it.
	a := InferRegime(healthyEvidence(), DefaultRegimePolicy())
	if a.Regime != RegimeTrending {
		t.Fatalf("regime = %s, want TRENDING; reasons:\n%s",
			a.Regime, strings.Join(a.Explain(), "\n"))
	}
	if a.Contributing != 0 {
		t.Errorf("%d reasons contributed on a healthy market", a.Contributing)
	}
}

func TestTooLittleHistoryIsUnknownAndNotRiskOff(t *testing.T) {
	// UNKNOWN and RISK_OFF are different answers. A freshly seeded database
	// with four bars has not characterised the market; it is not in a crisis.
	// Conflating them would make every restart look like one.
	ev := healthyEvidence()
	ev.BarsAvailable = 4
	a := InferRegime(ev, DefaultRegimePolicy())

	if a.Regime != RegimeUnknown {
		t.Fatalf("regime = %s, want UNKNOWN", a.Regime)
	}
	if a.Contributing != 0 {
		t.Errorf("insufficient evidence contributed %d reasons towards RISK_OFF", a.Contributing)
	}
	r, ok := reasonFor(a, ReasonInsufficientEvidence)
	if !ok {
		t.Fatal("UNKNOWN was returned with no reason")
	}
	if !strings.Contains(r.Observed, "bars=4") {
		t.Errorf("the reason does not say what was observed: %q", r.Observed)
	}
}

func TestNoQuoteIsUnknown(t *testing.T) {
	ev := healthyEvidence()
	ev.HasQuote = false
	if got := InferRegime(ev, DefaultRegimePolicy()).Regime; got != RegimeUnknown {
		t.Errorf("regime = %s, want UNKNOWN with no quote", got)
	}
}

func TestOneOrdinaryReasonIsNotEnoughForRiskOff(t *testing.T) {
	// A single degraded reading is a bad bar, not a broken market. Requiring
	// two ordinary reasons is what stops RISK_OFF from being the normal state.
	ev := healthyEvidence()
	ev.Health.State = DataQualityDegraded
	ev.Health.Issues = []DataQualityIssue{IssueExcessiveLatency}

	a := InferRegime(ev, DefaultRegimePolicy())
	if a.Regime == RegimeRiskOff {
		t.Fatalf("one degraded reading produced RISK_OFF; reasons:\n%s",
			strings.Join(a.Explain(), "\n"))
	}
	if a.Contributing != 1 {
		t.Errorf("contributing = %d, want 1", a.Contributing)
	}
}

func TestTwoOrdinaryReasonsInferRiskOff(t *testing.T) {
	// The designed path: no single catastrophe, but the conditions for holding
	// a directional view have gone.
	ev := healthyEvidence()
	ev.Health.State = DataQualityDegraded
	ev.Health.Issues = []DataQualityIssue{IssueExcessiveLatency}
	ev.DrawdownFraction = dec("0.14")

	a := InferRegime(ev, DefaultRegimePolicy())
	if a.Regime != RegimeRiskOff {
		t.Fatalf("regime = %s, want RISK_OFF; reasons:\n%s",
			a.Regime, strings.Join(a.Explain(), "\n"))
	}
	if a.Contributing < 2 {
		t.Errorf("contributing = %d, want at least 2", a.Contributing)
	}
	// And it must be attributable to the two things that were actually wrong.
	health, _ := reasonFor(a, ReasonMarketDataDegraded)
	dd, _ := reasonFor(a, ReasonPortfolioDrawdown)
	if !health.Contributes || !dd.Contributes {
		t.Errorf("the wrong reasons contributed:\n%s", strings.Join(a.Explain(), "\n"))
	}
	if dd.Observed != "0.1400" || dd.Threshold != "0.1000" {
		t.Errorf("drawdown reason = observed %q threshold %q", dd.Observed, dd.Threshold)
	}
}

func TestAnInvalidFeedIsDecisiveOnItsOwn(t *testing.T) {
	// Requiring a second reason would mean a feed reporting `invalid` was
	// survivable alone. It is not: an invalid feed means the prices a decision
	// would use are known to be wrong.
	for _, state := range []DataQualityState{DataQualityInvalid, DataQualityNoData} {
		ev := healthyEvidence()
		ev.Health.State = state

		a := InferRegime(ev, DefaultRegimePolicy())
		if a.Regime != RegimeRiskOff {
			t.Errorf("%s: regime = %s, want RISK_OFF", state, a.Regime)
		}
		if a.Decisive != ReasonMarketDataDegraded {
			t.Errorf("%s: decisive = %q, want market_data_degraded", state, a.Decisive)
		}
	}
}

func TestASpreadFarOutsideTheInstrumentsNormalIsDecisive(t *testing.T) {
	// Absolute thresholds cannot do this job alone: 0.4% is ordinary for one
	// instrument and a liquidity hole for another. The multiple is what
	// catches a venue problem on a normally tight book.
	ev := healthyEvidence()
	ev.SpreadBaseline = dec("0.0002")
	ev.Health.SpreadPct = dec("0.0020") // ten times normal, still under 0.5%

	a := InferRegime(ev, DefaultRegimePolicy())
	if a.Regime != RegimeRiskOff {
		t.Fatalf("regime = %s, want RISK_OFF; reasons:\n%s",
			a.Regime, strings.Join(a.Explain(), "\n"))
	}
	if a.Decisive != ReasonExtremeVolatility {
		t.Errorf("decisive = %q, want extreme_volatility", a.Decisive)
	}
	// The absolute spread check must NOT have fired, or the test proves
	// nothing about the multiple.
	abs, _ := reasonFor(a, ReasonAbnormalSpread)
	if abs.Contributes {
		t.Error("the absolute spread threshold fired; this case is about the multiple")
	}
}

func TestAMissingSpreadBaselineIsRecordedRatherThanAssumed(t *testing.T) {
	// Without a normal there is no multiple. The check has to say it could not
	// run, or a reader assumes it passed.
	ev := healthyEvidence()
	ev.SpreadBaseline = decimal.Zero
	a := InferRegime(ev, DefaultRegimePolicy())

	r, ok := reasonFor(a, ReasonExtremeVolatility)
	if !ok {
		t.Fatal("the volatility check vanished when its baseline was missing")
	}
	if r.Contributes {
		t.Error("an unmeasurable check contributed to the verdict")
	}
	if !strings.Contains(r.Observed, "no expected spread") {
		t.Errorf("the reason does not say the check could not run: %q", r.Observed)
	}
}

func TestABlackoutAloneIsEventRiskAndNotRiskOff(t *testing.T) {
	// EVENT_RISK is temporary and specific: the release resolves it. Reporting
	// it as RISK_OFF would conflate "wait twenty minutes" with "the market has
	// broken down", and the two call for different responses.
	ev := healthyEvidence()
	ev.Blackout = true
	ev.EventName = "US Non-Farm Payrolls"

	a := InferRegime(ev, DefaultRegimePolicy())
	if a.Regime != RegimeEventRisk {
		t.Fatalf("regime = %s, want EVENT_RISK", a.Regime)
	}
	r, _ := reasonFor(a, ReasonEventRisk)
	if !strings.Contains(r.Detail, "Non-Farm Payrolls") {
		t.Errorf("the reason does not name the event: %q", r.Detail)
	}
}

func TestABlackoutDuringAnOutageIsRiskOffNotMerelyEventRisk(t *testing.T) {
	// The ordering that matters. A release landing while the feed is broken is
	// not "an event"; it is the worst combination available, and reporting the
	// milder label would licence exactly the wrong response.
	ev := healthyEvidence()
	ev.Blackout = true
	ev.Health.State = DataQualityStale
	ev.Health.Issues = []DataQualityIssue{IssueStaleQuote}

	a := InferRegime(ev, DefaultRegimePolicy())
	if a.Regime != RegimeRiskOff {
		t.Fatalf("regime = %s, want RISK_OFF; reasons:\n%s",
			a.Regime, strings.Join(a.Explain(), "\n"))
	}
}

func TestProviderInstabilityContributes(t *testing.T) {
	// A feed that keeps reconnecting serves quotes that each look fine. The
	// instability is only visible across observations, which is why it is
	// evidence in its own right.
	ev := healthyEvidence()
	ev.ProviderReconnects = 9
	ev.DrawdownFraction = dec("0.13")

	a := InferRegime(ev, DefaultRegimePolicy())
	if a.Regime != RegimeRiskOff {
		t.Fatalf("regime = %s, want RISK_OFF", a.Regime)
	}
	r, _ := reasonFor(a, ReasonProviderInstability)
	if !r.Contributes || !strings.Contains(r.Observed, "9 reconnects") {
		t.Errorf("provider reason = %+v", r)
	}
}

func TestEveryVerdictRecordsEveryCheckIncludingThePassingOnes(t *testing.T) {
	// A verdict listing only its triggers is unfalsifiable: a reader cannot
	// tell a check that passed from one that never ran. "Spread was fine" is
	// evidence.
	a := InferRegime(healthyEvidence(), DefaultRegimePolicy())
	for _, want := range []RegimeReasonCode{
		ReasonMarketDataDegraded, ReasonAbnormalSpread, ReasonExtremeVolatility,
		ReasonEventRisk, ReasonPortfolioDrawdown, ReasonProviderInstability,
	} {
		if _, ok := reasonFor(a, want); !ok {
			t.Errorf("check %q was not recorded", want)
		}
	}
	// And each line renders with its numbers.
	for _, line := range a.Explain() {
		if !strings.Contains(line, "observed") {
			t.Errorf("an explanation line carries no observation: %q", line)
		}
	}
}

func TestTheVerdictCarriesThePolicyVersion(t *testing.T) {
	// A decision taken under one threshold set cannot be compared with one
	// taken under another unless both say which they used.
	a := InferRegime(healthyEvidence(), DefaultRegimePolicy())
	if a.PolicyVersion != "regime-v1" {
		t.Errorf("policy version = %q", a.PolicyVersion)
	}
}

func TestAnUnrecognisedReportedRegimeBecomesUnknown(t *testing.T) {
	// Fail closed. A typo from the research plane must not become a tradable
	// label, and it must not become RISK_OFF either -- it is an absence of
	// information, not evidence of a crisis.
	ev := healthyEvidence()
	ev.Reported = Regime("Trendinggg")

	a := InferRegime(ev, DefaultRegimePolicy())
	if a.Regime != RegimeUnknown {
		t.Fatalf("regime = %s, want UNKNOWN", a.Regime)
	}
	r, _ := reasonFor(a, ReasonReported)
	if !strings.Contains(r.Detail, "nothing recognisable") {
		t.Errorf("the reason does not explain the fallback: %q", r.Detail)
	}
}

func TestAModelReportedRiskOffIsHonoured(t *testing.T) {
	// The classifier's own evidence is not the only route in. A model may see
	// a cross-asset flight this instrument's spread does not show, and the
	// point of keeping RISK_OFF reachable from a model is that this classifier
	// is not the whole truth.
	ev := healthyEvidence()
	ev.Reported = RegimeRiskOff

	if got := InferRegime(ev, DefaultRegimePolicy()).Regime; got != RegimeRiskOff {
		t.Errorf("regime = %s, want the reported RISK_OFF to survive", got)
	}
}

func TestRiskOffIsNotTradable(t *testing.T) {
	// The inference is only useful if the label already refuses. This pins the
	// connection between the two.
	if RegimeRiskOff.Tradable() {
		t.Error("RISK_OFF is tradable, so inferring it changes nothing")
	}
	if RegimeUnknown.Tradable() {
		t.Error("UNKNOWN is tradable")
	}
	if !RegimeTrending.Tradable() {
		t.Error("TRENDING is not tradable, which would refuse everything")
	}
}
