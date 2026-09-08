package risk

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/fx"
	"github.com/vantage/control-api/internal/money"
	"github.com/vantage/control-api/internal/portfolio"
)

// The engine is a pure function, so these tests need no database, no venue and
// no clock. That is the whole point of keeping it pure: the checks that decide
// whether money may move are exhaustively testable, including at the
// boundaries where an off-by-one would silently permit an over-sized order.

// fixedRateSource returns one rate, or none, without touching a database.
type fixedRateSource struct {
	rate  decimal.Decimal
	empty bool
}

func (s fixedRateSource) LatestFXRate(_ context.Context, base, quote money.Currency) (fx.Rate, error) {
	if s.empty {
		return fx.Rate{}, fx.ErrNoRate
	}
	return fx.Rate{
		Base: base, Quote: quote, Rate: s.rate,
		SourceTime: testNow, IngestedAt: testNow, Provider: "test",
	}, nil
}

var testNow = time.Date(2026, 3, 12, 14, 0, 0, 0, time.UTC)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func zar(s string) money.Amount { return money.New(dec(s), money.ZAR) }

// newEngine builds an engine whose USD/ZAR rate is fixed at 18.25.
func newEngine(source fx.RateSource) *Engine {
	return New(fx.NewConverter(source, 24*time.Hour, func() time.Time { return testNow }))
}

// microGold is the one-ounce contract, which is what makes a R500 account able
// to trade gold at all.
func microGold() domain.Instrument {
	return domain.Instrument{
		ID: "XAUUSD.m", Symbol: "XAUUSD.m", Class: domain.AssetClassMetal,
		BaseCcy: money.XAU, QuoteCcy: money.USD, Enabled: true,
		Spec: domain.InstrumentSpec{
			ContractSize: dec("1"), PricePrecision: 2, TickSize: dec("0.01"),
			QuantityPrecision: 2, MinQuantity: dec("0.01"), MaxQuantity: dec("100"),
			QuantityStep: dec("0.01"), MarginRate: dec("0.005"), MaxLeverage: dec("200"),
			CommissionPerLot: dec("0.10"),
		},
	}
}

func healthyLimits() domain.RiskLimits {
	return domain.RiskLimits{
		AccountID: uuid.New(), Currency: money.ZAR,
		MaxOrderQuantity:         dec("1"),
		MaxOrderNotional:         zar("2500"),
		MaxRiskPerTradeFraction:  dec("0.01"),
		RequireStopLoss:          true,
		MaxOpenPositions:         3,
		MaxPendingOrders:         5,
		MaxGrossExposure:         zar("2500"),
		MaxNetExposure:           zar("2500"),
		MaxPerInstrumentExposure: zar("2500"),
		MaxConcentrationFraction: dec("0.6"),
		MaxLeverage:              dec("10"),
		MaxDailyLoss:             zar("15"),
		MaxDrawdownFraction:      dec("0.15"),
		MaxSpreadFraction:        dec("0.001"),
		MaxSlippageFraction:      dec("0.001"),

		EventBlackoutBeforeMinutes: 15,
		EventBlackoutAfterMinutes:  10,
		BlockOnHighImpactEvents:    true,
	}
}

// baseInput is a healthy account placing a modest, stopped buy on micro gold.
func baseInput() Input {
	accountID := uuid.New()
	// 20 dollars of stop distance on a one-ounce contract at 0.01 lots risks
	// $0.20, about R3.65, which is 0.73% of a R500 account and inside the 1%
	// per-trade limit. The arithmetic is spelled out because a fixture that
	// quietly breaches the limit it is testing is worse than no fixture.
	stop := dec("2628.00")

	state := domain.AccountSnapshot{
		AccountID: accountID, Currency: money.ZAR,
		Balance: zar("500"), Equity: zar("500"),
		MarginUsed: zar("0"), FreeMargin: zar("500"),
		RealizedPnL: zar("0"), UnrealizedPnL: zar("0"),
		Fees: zar("0"), Commission: zar("0"), Swap: zar("0"),
		GrossExposure: zar("0"), NetExposure: zar("0"),
		PeakEquity: zar("500"), DayStartEquity: zar("500"),
		AsOf: testNow, Simulated: true,
	}

	return Input{
		Intent: domain.OrderIntent{
			ID: uuid.New(), AccountID: accountID, InstrumentID: "XAUUSD.m",
			Side: domain.SideBuy, Type: domain.OrderTypeMarket, Quantity: dec("0.01"),
			StopLoss: &stop, TimeInForce: domain.TIFGoodTilCancelled,
			Source: domain.SourceManual, CreatedAt: testNow,
		},
		Account: domain.Account{
			ID: accountID, Mode: domain.ModePaper, Currency: money.ZAR,
			Enabled: true, TradingEnabled: true, Leverage: dec("10"),
		},
		Snapshot: portfolio.Snapshot{
			Account:              domain.Account{ID: accountID, Currency: money.ZAR},
			State:                state,
			ExposureByInstrument: map[string]money.Amount{},
			ExposureByCurrency:   map[money.Currency]money.Amount{},
		},
		Limits: healthyLimits(),
		// An authority is always present in the running pipeline. Leaving it at
		// its zero value would make min(limit, authority) zero and refuse
		// everything — fail-closed, and correct, but not what this fixture is
		// exercising.
		Authority: domain.TradingAuthority{
			AccountID: accountID, Mode: domain.ModePaper,
			Active: true, AutomationEnabled: true,
			AllowedInstruments:  []string{"XAUUSD.m"},
			AllowedOrderTypes:   []domain.OrderType{domain.OrderTypeMarket},
			MaxOrderQuantity:    dec("1"),
			MaxOrderNotional:    zar("2500"),
			MaxPositionExposure: zar("2500"),
			MaxLeverage:         dec("10"),
			MaxDailyLoss:        zar("15"),
			ValidFrom:           testNow.Add(-time.Hour),
		},
		Instrument: microGold(),
		Quote: domain.Quote{
			InstrumentID: "XAUUSD.m", Symbol: "XAUUSD.m",
			Bid: dec("2647.95"), Ask: dec("2648.27"),
			SourceTime: testNow, IngestedAt: testNow, Provider: "test",
		},
		Health: domain.MarketDataHealth{
			InstrumentID: "XAUUSD.m", State: domain.DataQualityOK,
			LastQuoteAt: testNow, EvaluatedAt: testNow, Provider: "test",
		},
		Market: domain.MarketOpen,
		Now:    testNow,
	}
}

// check finds one check result by name.
func check(t *testing.T, d domain.RiskDecision, name domain.RiskCheckName) domain.RiskCheckResult {
	t.Helper()
	for _, c := range d.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q was not evaluated; the engine ran %d checks", name, len(d.Checks))
	return domain.RiskCheckResult{}
}

func evaluate(t *testing.T, in Input) domain.RiskDecision {
	t.Helper()
	engine := newEngine(fixedRateSource{rate: dec("18.25")})
	d, err := engine.Evaluate(context.Background(), in)
	if err != nil {
		t.Fatalf("Evaluate returned an error: %v", err)
	}
	return d
}

func TestHealthyOrderIsApproved(t *testing.T) {
	d := evaluate(t, baseInput())
	if !d.Approved {
		t.Fatalf("expected approval, got refusal: %v", d.Failures())
	}
	if d.ApprovedQuantity.IsZero() {
		t.Fatal("approved decision has a zero quantity")
	}
}

// The first trade on an empty account is by definition 100% concentrated. The
// check must not fire there, or no account can ever open a position.
func TestConcentrationDoesNotBlockTheFirstTrade(t *testing.T) {
	d := evaluate(t, baseInput())
	if c := check(t, d, domain.CheckConcentration); !c.Passed {
		t.Fatalf("concentration blocked the first trade: %s", c.Message)
	}
}

func TestConcentrationAppliesOnceOtherInstrumentsHoldExposure(t *testing.T) {
	in := baseInput()
	in.Snapshot.ExposureByInstrument = map[string]money.Amount{"EURUSD": zar("100")}
	in.Snapshot.State.GrossExposure = zar("100")
	in.Limits.MaxConcentrationFraction = dec("0.10")

	d := evaluate(t, in)
	if c := check(t, d, domain.CheckConcentration); c.Passed {
		t.Fatal("expected the concentration check to bind with another instrument held")
	}
}

// Every check must run, so an operator who fixes one breach is not surprised
// by the next three.
func TestAllFailuresAreReportedNotJustTheFirst(t *testing.T) {
	in := baseInput()
	in.Intent.Quantity = dec("50")         // over the quantity limit
	in.Limits.MaxOrderNotional = zar("10") // and over the notional limit
	in.Account.TradingEnabled = false      // and the account is suspended

	d := evaluate(t, in)
	if d.Approved {
		t.Fatal("expected refusal")
	}
	if len(d.Failures()) < 3 {
		t.Fatalf("expected at least three failures, got %d: %v", len(d.Failures()), d.Failures())
	}
	if d.FirstFailure == nil {
		t.Fatal("FirstFailure should be set when checks fail")
	}
}

func TestMissingStopLossIsRefusedWhenRequired(t *testing.T) {
	in := baseInput()
	in.Intent.StopLoss = nil

	d := evaluate(t, in)
	if d.Approved {
		t.Fatal("expected refusal without a stop loss")
	}
	if c := check(t, d, domain.CheckStopLossRequired); c.Passed {
		t.Fatal("the stop-loss check should have failed")
	}
}

func TestStopLossNotRequiredWhenPolicyAllowsIt(t *testing.T) {
	in := baseInput()
	in.Intent.StopLoss = nil
	in.Limits.RequireStopLoss = false

	d := evaluate(t, in)
	if c := check(t, d, domain.CheckStopLossRequired); !c.Passed {
		t.Fatalf("stop loss should not be required: %s", c.Message)
	}
}

// Risk may only reduce. Whatever the limits say, the approved quantity can
// never exceed what was asked for.
func TestApprovedQuantityNeverExceedsTheRequest(t *testing.T) {
	in := baseInput()
	in.Intent.Quantity = dec("0.01")
	// Deliberately generous limits: nothing here should scale the order up.
	in.Limits.MaxRiskPerTradeFraction = dec("0.10")
	in.Limits.MaxOrderQuantity = dec("100")
	in.Limits.MaxOrderNotional = zar("1000000")
	in.Limits.MaxGrossExposure = zar("1000000")
	in.Limits.MaxNetExposure = zar("1000000")
	in.Limits.MaxPerInstrumentExposure = zar("1000000")
	in.Snapshot.State.Equity = zar("1000000")
	in.Snapshot.State.FreeMargin = zar("1000000")
	in.Snapshot.State.PeakEquity = zar("1000000")
	in.Snapshot.State.DayStartEquity = zar("1000000")

	d := evaluate(t, in)
	if d.ApprovedQuantity.GreaterThan(in.Intent.Quantity) {
		t.Fatalf("risk increased the order: requested %s, approved %s",
			in.Intent.Quantity, d.ApprovedQuantity)
	}
}

// An account past its daily loss limit gets no trade, not a smaller one.
func TestDailyLossCeilingRefusesRatherThanScalingDown(t *testing.T) {
	in := baseInput()
	in.Snapshot.State.Equity = zar("480")
	in.Snapshot.State.DayStartEquity = zar("500")
	in.Limits.MaxDailyLoss = zar("15")

	d := evaluate(t, in)
	if d.Approved {
		t.Fatal("expected refusal past the daily loss ceiling")
	}
	c := check(t, d, domain.CheckDailyLoss)
	if c.Passed {
		t.Fatal("the daily-loss check should have failed")
	}
	if c.Code != domain.RejectDailyLoss {
		t.Fatalf("expected %s, got %s", domain.RejectDailyLoss, c.Code)
	}
}

func TestDrawdownCeilingRefuses(t *testing.T) {
	in := baseInput()
	in.Snapshot.State.PeakEquity = zar("500")
	in.Snapshot.State.Equity = zar("400") // 20% drawdown against a 15% ceiling
	in.Snapshot.State.DayStartEquity = zar("400")

	d := evaluate(t, in)
	if c := check(t, d, domain.CheckDrawdown); c.Passed {
		t.Fatal("the drawdown check should have failed at 20% against a 15% limit")
	}
	if d.Approved {
		t.Fatal("expected refusal past the drawdown ceiling")
	}
}

func TestWideSpreadIsRefused(t *testing.T) {
	in := baseInput()
	in.Quote.Bid = dec("2600.00")
	in.Quote.Ask = dec("2620.00") // ~0.77% spread against a 0.1% limit

	d := evaluate(t, in)
	if c := check(t, d, domain.CheckSpread); c.Passed {
		t.Fatal("the spread check should have failed")
	}
}

func TestClosedMarketIsRefused(t *testing.T) {
	in := baseInput()
	in.Market = domain.MarketClosedWeekend

	d := evaluate(t, in)
	if c := check(t, d, "market_open"); c.Passed {
		t.Fatal("the market-open check should have failed on a weekend")
	}
}

// A degraded feed stops automation but not a human, who can see the warning.
func TestDegradedFeedBlocksAutomationButNotAManualOrder(t *testing.T) {
	in := baseInput()
	in.Health.State = domain.DataQualityDegraded

	manual := evaluate(t, in)
	if c := check(t, manual, "market_data_health"); !c.Passed {
		t.Fatalf("a manual order should proceed on a degraded feed: %s", c.Message)
	}

	in.Intent.Source = domain.SourceStrategy
	automated := evaluate(t, in)
	if c := check(t, automated, "market_data_health"); c.Passed {
		t.Fatal("an automated order must be refused on a degraded feed")
	}
}

func TestStaleFeedBlocksEvenAManualOrder(t *testing.T) {
	in := baseInput()
	in.Health.State = domain.DataQualityStale

	d := evaluate(t, in)
	c := check(t, d, "market_data_health")
	if c.Passed {
		t.Fatal("a stale feed must refuse a manual order too")
	}
	if c.Code != domain.RejectMarketDataStale {
		t.Fatalf("expected %s, got %s", domain.RejectMarketDataStale, c.Code)
	}
}

// The blackout binds automation. A manual order passes with the event shown,
// because refusing to let a person act before a release is worse than the risk.
func TestEventBlackoutBindsAutomationOnly(t *testing.T) {
	in := baseInput()
	in.EventBlackout = true
	in.EventName = "Non-Farm Payrolls"
	in.EventAt = testNow.Add(5 * time.Minute)

	manual := evaluate(t, in)
	if c := check(t, manual, domain.CheckEventRisk); !c.Passed {
		t.Fatalf("a manual order should pass an event blackout: %s", c.Message)
	}

	in.Intent.Source = domain.SourceStrategy
	automated := evaluate(t, in)
	c := check(t, automated, domain.CheckEventRisk)
	if c.Passed {
		t.Fatal("an automated order must be refused inside a blackout")
	}
	if c.Code != domain.RejectEventRisk {
		t.Fatalf("expected %s, got %s", domain.RejectEventRisk, c.Code)
	}
}

func TestEventBlackoutCanBeDisabledByPolicy(t *testing.T) {
	in := baseInput()
	in.EventBlackout = true
	in.Intent.Source = domain.SourceStrategy
	in.Limits.BlockOnHighImpactEvents = false

	d := evaluate(t, in)
	if c := check(t, d, domain.CheckEventRisk); !c.Passed {
		t.Fatal("with the policy off, the event check should not bind")
	}
}

func TestDisabledInstrumentIsRefused(t *testing.T) {
	in := baseInput()
	in.Instrument.Enabled = false

	d := evaluate(t, in)
	if c := check(t, d, domain.CheckInstrumentEnabled); c.Passed {
		t.Fatal("a disabled instrument must be refused")
	}
}

func TestSuspendedAccountIsRefused(t *testing.T) {
	in := baseInput()
	in.Account.TradingEnabled = false

	d := evaluate(t, in)
	if c := check(t, d, domain.CheckAccountTrading); c.Passed {
		t.Fatal("a suspended account must be refused")
	}
}

func TestPositionAndOrderCountCeilings(t *testing.T) {
	in := baseInput()
	in.OpenPositions = 3
	in.PendingOrders = 5
	in.Snapshot.State.OpenPositions = 3
	in.Snapshot.State.PendingOrders = 5

	d := evaluate(t, in)
	if c := check(t, d, domain.CheckOpenPositions); c.Passed {
		t.Fatal("expected the open-position ceiling to bind at the limit")
	}
	if c := check(t, d, domain.CheckPendingOrders); c.Passed {
		t.Fatal("expected the pending-order ceiling to bind at the limit")
	}
}

// A quantity below the instrument's minimum is not tradable, and rounding it
// up would breach the risk budget the size came from.
func TestSubMinimumQuantityIsRefused(t *testing.T) {
	in := baseInput()
	in.Intent.Quantity = dec("0.001")

	d := evaluate(t, in)
	if d.Approved {
		t.Fatal("expected refusal below the instrument minimum")
	}
}

// Without an FX rate the order's notional cannot be expressed in the account's
// currency, so its risk cannot be checked. The engine must refuse, not guess.
func TestMissingFXRateRefusesRatherThanAssumingParity(t *testing.T) {
	engine := newEngine(fixedRateSource{empty: true})
	d, err := engine.Evaluate(context.Background(), baseInput())

	if err == nil && d.Approved {
		t.Fatal("an order that cannot be converted into the account currency must not be approved")
	}
	if err == nil {
		// A refusal carried in the decision is equally acceptable, as long as
		// it is a refusal.
		if len(d.Failures()) == 0 {
			t.Fatal("expected either an error or a recorded failure with no usable FX rate")
		}
	}
}

// Determinism: the same input twice must produce the same verdict, or a stored
// decision snapshot cannot be replayed to defend a refusal.
func TestEvaluationIsDeterministic(t *testing.T) {
	in := baseInput()
	first := evaluate(t, in)
	second := evaluate(t, in)

	if first.Approved != second.Approved {
		t.Fatal("the same input produced different approvals")
	}
	if !first.ApprovedQuantity.Equal(second.ApprovedQuantity) {
		t.Fatalf("the same input produced different sizes: %s then %s",
			first.ApprovedQuantity, second.ApprovedQuantity)
	}
	if len(first.Checks) != len(second.Checks) {
		t.Fatalf("the same input ran a different number of checks: %d then %d",
			len(first.Checks), len(second.Checks))
	}
	for i := range first.Checks {
		if first.Checks[i].Name != second.Checks[i].Name ||
			first.Checks[i].Passed != second.Checks[i].Passed {
			t.Fatalf("check %d differed between runs: %+v vs %+v",
				i, first.Checks[i], second.Checks[i])
		}
	}
}

// Every check the engine claims to run must appear in the result, because the
// UI and the snapshot both present the list as complete.
func TestEveryDocumentedCheckIsEvaluated(t *testing.T) {
	d := evaluate(t, baseInput())

	expected := []domain.RiskCheckName{
		domain.CheckAccountTrading,
		domain.CheckInstrumentEnabled,
		"market_data_health",
		"market_open",
		domain.CheckSpread,
		domain.CheckEventRisk,
		domain.CheckDailyLoss,
		domain.CheckDrawdown,
		domain.CheckOrderQuantity,
		domain.CheckOrderNotional,
		domain.CheckStopLossRequired,
		domain.CheckOpenPositions,
		domain.CheckPendingOrders,
		domain.CheckGrossExposure,
		domain.CheckNetExposure,
		domain.CheckInstrumentExposure,
		domain.CheckConcentration,
		domain.CheckLeverage,
		domain.CheckMargin,
	}
	for _, name := range expected {
		check(t, d, name) // fails the test if absent
	}
}
