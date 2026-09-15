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

		// The trading authority's own ceilings, which report separately from
		// the account limits they sit beside so a refusal names the bound.
		domain.CheckAuthorityOrderQuantity,
		domain.CheckAuthorityOrderNotional,
		domain.CheckAuthorityPositionExposure,
		domain.CheckAuthorityDailyLoss,
		domain.CheckAuthorityLeverage,
	}
	for _, name := range expected {
		check(t, d, name) // fails the test if absent
	}
}

// --- reference price -------------------------------------------------------
//
// Added after the end-to-end suite exposed the defect these cover: the engine
// priced every order at the current market, so a limit order 20% below the
// market with a stop just under its limit was measured as risking a fifth of
// the account and refused for a risk it did not carry.

func TestLimitOrderRiskIsMeasuredFromTheLimitPrice(t *testing.T) {
	in := baseInput()
	// A limit far below the market, with a stop close to it. The real risk is
	// the small distance between the two.
	limit := dec("2100.00")
	stop := dec("2092.00")
	in.Intent.Type = domain.OrderTypeLimit
	in.Intent.LimitPrice = &limit
	in.Intent.StopLoss = &stop

	if got := referencePrice(in); !got.Equal(limit) {
		t.Fatalf("referencePrice = %s, want the limit price %s", got, limit)
	}

	d := evaluate(t, in)
	c := check(t, d, domain.CheckRiskPerTrade)
	if !c.Passed {
		t.Fatalf("a limit order with an 8-dollar stop must not breach a 1%% budget: %s", c.Message)
	}
	if !d.Approved {
		t.Fatalf("expected approval, got: %v", d.Failures())
	}
}

func TestStopOrderRiskIsMeasuredFromTheStopPrice(t *testing.T) {
	in := baseInput()
	trigger := dec("2700.00")
	stop := dec("2692.00")
	in.Intent.Type = domain.OrderTypeStop
	in.Intent.StopPrice = &trigger
	in.Intent.StopLoss = &stop

	if got := referencePrice(in); !got.Equal(trigger) {
		t.Fatalf("referencePrice = %s, want the stop trigger %s", got, trigger)
	}
	if c := check(t, evaluate(t, in), domain.CheckRiskPerTrade); !c.Passed {
		t.Fatalf("a stop order measured from its trigger must pass: %s", c.Message)
	}
}

func TestMarketOrderStillUsesTheExecutablePrice(t *testing.T) {
	in := baseInput() // market order, no limit or stop price
	want := in.Quote.ExecutionPrice(in.Intent.Side)
	if got := referencePrice(in); !got.Equal(want) {
		t.Fatalf("referencePrice = %s, want the executable price %s", got, want)
	}
}

func TestAMissingRequiredPriceFallsBackToTheMarket(t *testing.T) {
	// The instrument-rule gate earlier in the pipeline refuses a limit order
	// with no limit price. If one reaches the engine anyway, it must not
	// divide by zero or price the order at nothing.
	in := baseInput()
	in.Intent.Type = domain.OrderTypeLimit
	in.Intent.LimitPrice = nil

	want := in.Quote.ExecutionPrice(in.Intent.Side)
	if got := referencePrice(in); !got.Equal(want) {
		t.Fatalf("referencePrice = %s, want the market fallback %s", got, want)
	}
}

func TestLimitOrderNotionalUsesTheLimitPrice(t *testing.T) {
	// Notional, margin and exposure all follow the reference price, so a
	// far-away limit order must not be sized against the current market.
	in := baseInput()
	limit := dec("1000.00")
	stop := dec("996.00")
	in.Intent.Type = domain.OrderTypeLimit
	in.Intent.LimitPrice = &limit
	in.Intent.StopLoss = &stop

	d := evaluate(t, in)
	notional := check(t, d, domain.CheckOrderNotional)
	// At 1000 rather than ~2648, the notional is well under half.
	if !notional.Passed {
		t.Fatalf("notional check failed for a cheap limit order: %s", notional.Message)
	}
}

// ---------------------------------------------------------------------------
// Reducing orders: the exposure limits must not prevent shedding exposure
// ---------------------------------------------------------------------------

// closingPosition builds an input where the account is long `quantity` and the
// intent sells it back.
func closingPosition(quantity, sellQuantity string) Input {
	in := baseInput()
	held := dec(quantity)
	in.ExistingPosition = &domain.Position{
		AccountID:    in.Account.ID,
		InstrumentID: "XAUUSD.m",
		Side:         domain.SideBuy,
		Quantity:     held,
		Status:       domain.PositionOpen,
	}
	in.OpenPositions = 1
	// The position is worth far more than the per-order notional cap, which is
	// the situation that made closing impossible.
	exposure := zar("3866.77")
	in.Snapshot.State.GrossExposure = exposure
	in.Snapshot.State.NetExposure = exposure
	in.Snapshot.ExposureByInstrument["XAUUSD.m"] = exposure

	in.Intent.Side = domain.SideSell
	in.Intent.Quantity = dec(sellQuantity)
	// A closing order carries no protective stop: there is nothing left to
	// protect once it fills.
	in.Intent.StopLoss = nil
	return in
}

func TestAPositionLargerThanTheOrderNotionalCapCanStillBeClosed(t *testing.T) {
	// The defect this pins: a flatten of a 3866.77 ZAR position was refused
	// with "Order notional 3866.77 ZAR against a limit of 2500.00 ZAR". The
	// position had been built by several individually-permitted orders, and
	// the per-order cap then made it impossible to exit. A risk control that
	// prevents reducing risk is worse than no control, because it converts a
	// large position into a trapped one.
	d := evaluate(t, closingPosition("0.08", "0.08"))

	notional := check(t, d, domain.CheckOrderNotional)
	if !notional.Passed {
		t.Fatalf("a full close was refused by the per-order notional cap: %s", notional.Message)
	}
	for _, name := range []domain.RiskCheckName{
		domain.CheckGrossExposure,
		domain.CheckNetExposure,
		domain.CheckInstrumentExposure,
	} {
		if r := check(t, d, name); !r.Passed {
			t.Errorf("a full close was refused by %s: %s", name, r.Message)
		}
	}
}

func TestAPartialCloseIsAlsoTreatedAsReducing(t *testing.T) {
	d := evaluate(t, closingPosition("0.08", "0.03"))
	if r := check(t, d, domain.CheckOrderNotional); !r.Passed {
		t.Fatalf("a partial close was refused by the notional cap: %s", r.Message)
	}
}

func TestAnOverClosingSideFlipDoesNotSkipTheExposureChecks(t *testing.T) {
	// The other half of the same fix, and the more dangerous half.
	//
	// `reducing` used to mean only "opposite side". An account long 0.08 could
	// therefore SELL 5.00 lots and skip the gross-exposure, per-instrument and
	// concentration checks completely — 0.08 of that is a close and 4.92 is a
	// large new short position, taken with the exposure limits switched off.
	//
	// A side flip is not a reduction and must be measured like any other new
	// exposure.
	in := closingPosition("0.08", "5.00")
	d := evaluate(t, in)

	if d.Approved {
		t.Fatal("a 5.00-lot sell against a 0.08-lot long was approved; an " +
			"over-closing side flip must be measured as new exposure")
	}
	gross := check(t, d, domain.CheckGrossExposure)
	notional := check(t, d, domain.CheckOrderNotional)
	if gross.Passed && notional.Passed {
		t.Fatalf("neither the gross-exposure nor the notional check refused a "+
			"side flip far beyond every limit (gross=%s notional=%s)",
			gross.Observed, notional.Observed)
	}
}

func TestAnAddingOrderOnTheSameSideIsNeverTreatedAsReducing(t *testing.T) {
	in := closingPosition("0.08", "0.08")
	in.Intent.Side = domain.SideBuy // adding to the long, not closing it
	stop := dec("2628.00")
	in.Intent.StopLoss = &stop
	d := evaluate(t, in)

	if r := check(t, d, domain.CheckOrderNotional); r.Passed {
		t.Fatal("an order ADDING to an existing long skipped the notional cap")
	}
	if d.Approved {
		t.Fatal("an order that would push exposure to 7733 ZAR against a 2500 " +
			"ZAR limit was approved")
	}
}

// ---------------------------------------------------------------------------
// Event blackout: it must not trap an operator in exposure
// ---------------------------------------------------------------------------

// blackoutInput puts the account inside a high-impact event window.
func blackoutInput() Input {
	in := baseInput()
	in.EventBlackout = true
	in.EventName = "Non-Farm Payrolls"
	in.EventAt = testNow.Add(5 * time.Minute)
	in.Limits.BlockOnHighImpactEvents = true
	return in
}

// withLongPosition gives the account something to close.
func withLongPosition(in Input, quantity string) Input {
	in.ExistingPosition = &domain.Position{
		AccountID:    in.Account.ID,
		InstrumentID: "XAUUSD.m",
		Side:         domain.SideBuy,
		Quantity:     dec(quantity),
		Status:       domain.PositionOpen,
	}
	in.OpenPositions = 1
	return in
}

func TestAnOperatorCanCloseAPositionDuringAnEventBlackout(t *testing.T) {
	// The defect this pins.
	//
	// The blackout exempted only Source `manual`, and an operator flatten
	// carries `risk_control`. So closing a position during a high-impact
	// release was refused with `event_risk_blackout` -- during precisely the
	// window when an operator most wants out, and while the check's own
	// comment claimed that blocking a human from closing "would be worse than
	// the risk it prevents".
	in := withLongPosition(blackoutInput(), "0.03")
	in.Intent.Side = domain.SideSell
	in.Intent.Quantity = dec("0.03")
	in.Intent.StopLoss = nil
	in.Intent.Source = domain.SourceRiskControl

	d := evaluate(t, in)
	if r := check(t, d, domain.CheckEventRisk); !r.Passed {
		t.Fatalf("an operator flatten was refused during an event blackout: %s.\n"+
			"The blackout exists because spreads widen and stops slip through a "+
			"release, which is an argument FOR being able to close a position.",
			r.Message)
	}
	if !d.Approved {
		t.Fatalf("the flatten was not approved: %+v", d.FirstFailure)
	}
}

func TestAnAutomatedStrategyStillCannotOPENDuringAnEventBlackout(t *testing.T) {
	// The control must keep doing its job. The exemption is for reducing
	// exposure, not for trading through a release.
	in := blackoutInput()
	in.Intent.Source = domain.SourceStrategy

	d := evaluate(t, in)
	if r := check(t, d, domain.CheckEventRisk); r.Passed {
		t.Fatal("an automated strategy was allowed to OPEN a position during a " +
			"high-impact event blackout, which is the thing the blackout is for")
	}
	if d.Approved {
		t.Fatal("the order was approved during a blackout")
	}
}

func TestAnAutomatedReducingOrderIsAllowedDuringAnEventBlackout(t *testing.T) {
	// A strategy closing its own position is shedding exposure, and the
	// blackout's own risk argument favours letting it.
	in := withLongPosition(blackoutInput(), "0.03")
	in.Intent.Side = domain.SideSell
	in.Intent.Quantity = dec("0.02")
	in.Intent.StopLoss = nil
	in.Intent.Source = domain.SourceStrategy

	if r := check(t, evaluate(t, in), domain.CheckEventRisk); !r.Passed {
		t.Fatalf("a strategy reducing its own exposure was blocked by the event "+
			"blackout: %s", r.Message)
	}
}

func TestAnOverClosingSideFlipIsStillBlockedDuringAnEventBlackout(t *testing.T) {
	// The exemption rides on the STRICT definition of reducing. A sell of 5.00
	// against a 0.03 long is not a reduction; it opens a large short, and
	// doing that through a release is exactly what the blackout prevents.
	in := withLongPosition(blackoutInput(), "0.03")
	in.Intent.Side = domain.SideSell
	in.Intent.Quantity = dec("5.00")
	in.Intent.StopLoss = nil
	in.Intent.Source = domain.SourceStrategy

	if r := check(t, evaluate(t, in), domain.CheckEventRisk); r.Passed {
		t.Fatal("an over-closing side flip was treated as reducing and allowed " +
			"through an event blackout")
	}
}

func TestAManualOrderIsStillAllowedDuringAnEventBlackout(t *testing.T) {
	// Unchanged behaviour, asserted so the new exemption cannot be mistaken
	// for having replaced it: the operator has been told and is deciding.
	in := blackoutInput()
	in.Intent.Source = domain.SourceManual
	if r := check(t, evaluate(t, in), domain.CheckEventRisk); !r.Passed {
		t.Fatalf("a manual order was refused during an event blackout: %s", r.Message)
	}
}

// TestALossCeilingNeverPreventsClosingAPosition.
//
// The third instance of one inversion, and the reason the general rule is now
// written down in the engine: a check whose purpose is to LIMIT exposure or
// loss must never refuse an order that strictly reduces exposure.
//
// A daily-loss limit that blocks a close locks the account into the losing
// position that caused the breach, and it goes on losing. A drawdown ceiling
// that blocks a close deepens the drawdown it is measuring.
func TestALossCeilingNeverPreventsClosingAPosition(t *testing.T) {
	for _, c := range []struct {
		name  string
		set   func(*Input)
		check domain.RiskCheckName
	}{
		{
			"daily loss breached",
			func(in *Input) {
				// Realised loss past the 15 ZAR limit.
				in.Snapshot.State.RealizedPnL = zar("-20")
				in.Snapshot.State.Equity = zar("480")
				in.Snapshot.State.DayStartEquity = zar("500")
			},
			domain.CheckDailyLoss,
		},
		{
			"drawdown breached",
			func(in *Input) {
				in.Snapshot.State.PeakEquity = zar("500")
				in.Snapshot.State.Equity = zar("300")
			},
			domain.CheckDrawdown,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := withLongPosition(baseInput(), "0.03")
			c.set(&in)
			in.Intent.Side = domain.SideSell
			in.Intent.Quantity = dec("0.03")
			in.Intent.StopLoss = nil
			in.Intent.Source = domain.SourceRiskControl

			if r := check(t, evaluate(t, in), c.check); !r.Passed {
				t.Fatalf("%s refused a position-closing order: %s.\n"+
					"The account is now locked into the exposure that caused the breach.",
					c.check, r.Message)
			}
		})
	}
}

func TestALossCeilingStillBlocksOpeningAPosition(t *testing.T) {
	// The control must keep working for what it is for.
	in := baseInput()
	in.Snapshot.State.RealizedPnL = zar("-20")
	in.Snapshot.State.Equity = zar("480")
	in.Snapshot.State.DayStartEquity = zar("500")

	d := evaluate(t, in)
	if r := check(t, d, domain.CheckDailyLoss); r.Passed {
		t.Fatal("an OPENING order was allowed with the daily loss limit breached")
	}
	if d.Approved {
		t.Fatal("the order was approved past its daily loss limit")
	}
}
