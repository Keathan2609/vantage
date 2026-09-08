// Package risk enforces the account's hard ceilings.
//
// The engine is a PURE FUNCTION of its inputs. Given the same intent, account
// state, limits and market conditions it returns the same decision, every
// time, with the same explanation. That property is what makes a refusal
// arguable after the fact: the inputs are captured in the decision snapshot,
// so the verdict can be recomputed and challenged.
//
// Two design rules matter more than any individual check:
//
//  1. Risk may only REDUCE. The engine can shrink a requested quantity or
//     refuse it outright; there is no path by which it can approve more than
//     was asked for.
//  2. Every check runs. The engine does not stop at the first failure, because
//     an operator fixing one breached limit needs to know about the other
//     three before they try again.
//
// Strategy code cannot reach this package: it has no reference to the engine,
// and the limits it enforces are loaded from the database by the order
// pipeline, not passed in by the caller who wants to trade.
package risk

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/fx"
	"github.com/vantage/control-api/internal/money"
	"github.com/vantage/control-api/internal/portfolio"
)

// Engine evaluates pre-trade risk.
type Engine struct {
	converter *fx.Converter
}

// New builds a risk engine.
func New(converter *fx.Converter) *Engine { return &Engine{converter: converter} }

// Input is everything a risk decision depends on. It is passed explicitly
// rather than fetched inside the engine so the same struct can be serialised
// into the decision snapshot and replayed.
type Input struct {
	Intent     domain.OrderIntent
	Account    domain.Account
	Snapshot   portfolio.Snapshot
	Limits     domain.RiskLimits
	Authority  domain.TradingAuthority
	Instrument domain.Instrument
	Quote      domain.Quote
	Health     domain.MarketDataHealth
	Market     domain.MarketStatus
	// EventBlackout reports an imminent or just-passed high-impact release.
	EventBlackout    bool
	EventName        string
	EventAt          time.Time
	OpenPositions    int
	PendingOrders    int
	ExistingPosition *domain.Position
	Now              time.Time
}

// Evaluate runs every check and returns a decision.
//
// The approved quantity starts at the requested quantity and is only ever
// reduced. A decision is approved only when every check passed AND a non-zero
// quantity survives sizing.
func (e *Engine) Evaluate(ctx context.Context, in Input) (domain.RiskDecision, error) {
	d := domain.RiskDecision{
		EvaluatedAt:      in.Now,
		ApprovedQuantity: in.Intent.Quantity,
	}

	add := func(r domain.RiskCheckResult) {
		d.Checks = append(d.Checks, r)
		if !r.Passed && d.FirstFailure == nil {
			cp := r
			d.FirstFailure = &cp
		}
	}

	// --- Account and instrument gates ------------------------------------
	add(boolCheck(domain.CheckAccountTrading, in.Account.TradingEnabled && in.Account.Enabled,
		"Trading is suspended on this account.", domain.RejectForbidden,
		"trading_enabled", fmt.Sprint(in.Account.TradingEnabled)))

	add(boolCheck(domain.CheckInstrumentEnabled, in.Instrument.Enabled,
		fmt.Sprintf("%s is not enabled for trading.", in.Instrument.Symbol),
		domain.RejectInstrumentDisabled, "instrument", in.Instrument.Symbol))

	// --- Market conditions ------------------------------------------------
	// Automated orders require a healthy feed. A manual order may proceed on a
	// degraded feed, because a human looking at the screen can see the warning
	// and decide; an algorithm cannot.
	feedOK := in.Health.Healthy()
	if in.Intent.Source == domain.SourceManual && in.Health.State == domain.DataQualityDegraded {
		feedOK = true
	}
	code := domain.RejectMarketDataStale
	if in.Health.State == domain.DataQualityInvalid {
		code = domain.RejectMarketDataInvalid
	}
	add(domain.RiskCheckResult{
		Name:     "market_data_health",
		Passed:   feedOK,
		Limit:    "ok",
		Observed: string(in.Health.State),
		Message: fmt.Sprintf("Market data for %s is %s (%v).",
			in.Instrument.Symbol, in.Health.State, in.Health.Issues),
		Code: code,
	})

	add(domain.RiskCheckResult{
		Name:     "market_open",
		Passed:   in.Market.Tradable(),
		Limit:    string(domain.MarketOpen),
		Observed: string(in.Market),
		Message:  fmt.Sprintf("The market for %s is %s.", in.Instrument.Symbol, in.Market),
		Code:     domain.RejectMarketClosed,
	})

	spreadOK := !in.Limits.MaxSpreadFraction.IsPositive() ||
		in.Quote.SpreadFraction().LessThanOrEqual(in.Limits.MaxSpreadFraction)
	add(domain.RiskCheckResult{
		Name:     domain.CheckSpread,
		Passed:   spreadOK,
		Limit:    pct(in.Limits.MaxSpreadFraction),
		Observed: pct(in.Quote.SpreadFraction()),
		Message: fmt.Sprintf("Spread on %s is %s, limit %s.",
			in.Instrument.Symbol, pct(in.Quote.SpreadFraction()), pct(in.Limits.MaxSpreadFraction)),
		Code: domain.RejectSpreadTooWide,
	})

	// reducing means STRICTLY reducing: the order is on the opposite side of an
	// existing position AND is no larger than that position.
	//
	// Both halves matter, and the second was missing.
	//
	// Why "opposite side" alone is not enough: an account long 0.08 lots could
	// send a SELL of 5.00 lots, which is not a reduction at all — it closes
	// 0.08 and opens 4.92 in the other direction. Treating that as reducing
	// let it skip the gross-exposure, per-instrument and concentration checks
	// entirely, which is a hole in exactly the control that is supposed to cap
	// exposure.
	//
	// Why the concept is needed at all: an order that genuinely reduces
	// exposure must never be refused BY an exposure limit. Otherwise a
	// position built up by several individually-permitted orders can grow past
	// the per-order cap and then become impossible to close — the limit that
	// exists to contain risk would prevent shedding it, and the bigger the
	// position, the harder the exit. A flatten of a 3866 ZAR position was
	// refused for "Order notional 3866.77 against a limit of 2500.00" before
	// this was applied consistently.
	reducing := in.ExistingPosition != nil &&
		in.ExistingPosition.Side != in.Intent.Side &&
		in.Intent.Quantity.LessThanOrEqual(in.ExistingPosition.Quantity)

	// --- Event risk --------------------------------------------------------
	// A blackout applies to orders that OPEN or INCREASE exposure, and only
	// to automated ones. Two exemptions, each for its own reason:
	//
	//   * A MANUAL order goes through with the event surfaced in the UI. The
	//     operator has been told and is deciding anyway.
	//
	//   * A REDUCING order goes through whatever its source. The blackout
	//     exists because spreads widen and stops slip through a release --
	//     which is an argument FOR being able to close a position, not
	//     against it. Refusing to let exposure be shed during the riskiest
	//     window of the day is the same inversion as refusing a flatten for
	//     breaching an exposure limit.
	//
	// The second exemption was missing, and the comment above this check
	// already claimed it: "blocking a human from closing a position before a
	// release would be worse than the risk it prevents". It was not true. An
	// operator flatten carries Source `risk_control`, not `manual`, so
	// closing a position during a blackout was refused with
	// `event_risk_blackout` -- during precisely the window when an operator
	// most wants out.
	eventOK := true
	if in.EventBlackout && in.Limits.BlockOnHighImpactEvents &&
		in.Intent.Source != domain.SourceManual && !reducing {
		eventOK = false
	}
	add(domain.RiskCheckResult{
		Name:     domain.CheckEventRisk,
		Passed:   eventOK,
		Limit:    fmt.Sprintf("%dm before / %dm after high-impact events", in.Limits.EventBlackoutBeforeMinutes, in.Limits.EventBlackoutAfterMinutes),
		Observed: eventObserved(in),
		Message:  eventMessage(in),
		Code:     domain.RejectEventRisk,
	})

	// --- Loss ceilings -----------------------------------------------------
	// Checked before sizing: an account past its daily loss limit does not get
	// a smaller trade, it gets no trade.
	//
	// A REDUCING order is exempt, and this is the third place that exemption
	// had to be added -- which is the point worth recording.
	//
	// A loss limit exists to stop losses growing. Refusing a close because the
	// limit is already breached locks the account into whatever it holds at
	// exactly the moment an operator most needs out, and the position that
	// caused the breach goes on losing. The control would be producing the
	// outcome it exists to prevent.
	//
	// The same inversion appeared in the per-order notional cap, and again in
	// the event blackout. The general rule, stated once here: a check whose
	// purpose is to LIMIT exposure or loss must never refuse an order that
	// strictly reduces exposure. Checks about whether trading is possible at
	// all -- account enabled, instrument tradable, market open, feed healthy,
	// kill switch, authority -- still apply, because without them there is no
	// price to close at.
	dayPnL := in.Snapshot.State.DayPnL()
	dailyLossBreached := dayPnL.IsNegative() &&
		dayPnL.Abs().Decimal().GreaterThanOrEqual(in.Limits.MaxDailyLoss.Decimal())
	add(domain.RiskCheckResult{
		Name:     domain.CheckDailyLoss,
		Passed:   reducing || !dailyLossBreached,
		Limit:    in.Limits.MaxDailyLoss.String(),
		Observed: dayPnL.String(),
		Message: fmt.Sprintf("Daily loss %s against a limit of %s.",
			dayPnL.Abs().String(), in.Limits.MaxDailyLoss.String()),
		Code: domain.RejectDailyLoss,
	})

	dd := in.Snapshot.State.DrawdownFraction()
	ddBreached := dd.GreaterThanOrEqual(in.Limits.MaxDrawdownFraction)
	add(domain.RiskCheckResult{
		Name: domain.CheckDrawdown,
		// Reducing is exempt for the same reason as the daily loss limit
		// above: a drawdown ceiling that prevents closing a position deepens
		// the drawdown it is measuring.
		Passed:   reducing || !ddBreached,
		Limit:    pct(in.Limits.MaxDrawdownFraction),
		Observed: pct(dd),
		Message: fmt.Sprintf("Drawdown from peak equity is %s against a limit of %s.",
			pct(dd), pct(in.Limits.MaxDrawdownFraction)),
		Code: domain.RejectDrawdown,
	})

	// --- Position and order counts ----------------------------------------
	positionsOK := reducing || in.OpenPositions < in.Limits.MaxOpenPositions ||
		(in.ExistingPosition != nil && in.ExistingPosition.Side == in.Intent.Side)
	add(domain.RiskCheckResult{
		Name:     domain.CheckOpenPositions,
		Passed:   positionsOK,
		Limit:    fmt.Sprint(in.Limits.MaxOpenPositions),
		Observed: fmt.Sprint(in.OpenPositions),
		Message: fmt.Sprintf("%d open positions against a limit of %d.",
			in.OpenPositions, in.Limits.MaxOpenPositions),
		Code: domain.RejectMaxPositions,
	})

	add(domain.RiskCheckResult{
		Name:     domain.CheckPendingOrders,
		Passed:   in.PendingOrders < in.Limits.MaxPendingOrders,
		Limit:    fmt.Sprint(in.Limits.MaxPendingOrders),
		Observed: fmt.Sprint(in.PendingOrders),
		Message: fmt.Sprintf("%d pending orders against a limit of %d.",
			in.PendingOrders, in.Limits.MaxPendingOrders),
		Code: domain.RejectMaxPendingOrders,
	})

	// --- Order size --------------------------------------------------------
	add(domain.RiskCheckResult{
		Name:     domain.CheckOrderQuantity,
		Passed:   in.Intent.Quantity.LessThanOrEqual(in.Limits.MaxOrderQuantity),
		Limit:    in.Limits.MaxOrderQuantity.String(),
		Observed: in.Intent.Quantity.String(),
		Message: fmt.Sprintf("Order quantity %s against a limit of %s.",
			in.Intent.Quantity, in.Limits.MaxOrderQuantity),
		Code: domain.RejectRiskLimit,
	})

	execPrice := referencePrice(in)
	notionalQuote := in.Instrument.Notional(in.Intent.Quantity, execPrice)
	notionalAcct, convErr := e.toAccount(ctx, notionalQuote, in.Account.Currency)
	if convErr != nil {
		// Without a rate the order's size cannot be expressed in the account's
		// own currency, so no limit denominated in that currency can be
		// checked. That is a refusal, not a warning.
		add(failedCheck(domain.CheckOrderNotional, in.Limits.MaxOrderNotional.String(), "unknown",
			fmt.Sprintf("Cannot value a %s order in %s: %v",
				in.Instrument.QuoteCcy, in.Account.Currency, convErr),
			domain.RejectInternalError))
		d.Approved = false
		return d, nil
	}

	add(domain.RiskCheckResult{
		Name: domain.CheckOrderNotional,
		// A closing order's notional is the size of the position already held,
		// not new exposure being taken on. See the note on `reducing`.
		Passed: reducing ||
			notionalAcct.Decimal().LessThanOrEqual(in.Limits.MaxOrderNotional.Decimal()),
		Limit:    in.Limits.MaxOrderNotional.String(),
		Observed: notionalAcct.String(),
		Message: fmt.Sprintf("Order notional %s against a limit of %s.",
			notionalAcct.String(), in.Limits.MaxOrderNotional.String()),
		Code: domain.RejectExposureLimit,
	})

	// --- Margin ------------------------------------------------------------
	marginQuote := in.Instrument.MarginRequired(in.Intent.Quantity, execPrice)
	marginAcct, err := e.toAccount(ctx, marginQuote, in.Account.Currency)
	if err != nil {
		add(failedCheck(domain.CheckMargin, "n/a", "unknown",
			fmt.Sprintf("Cannot value required margin in %s: %v", in.Account.Currency, err),
			domain.RejectInternalError))
		d.Approved = false
		return d, nil
	}
	marginOK := reducing || marginAcct.Decimal().LessThanOrEqual(in.Snapshot.State.FreeMargin.Decimal())
	add(domain.RiskCheckResult{
		Name:     domain.CheckMargin,
		Passed:   marginOK,
		Limit:    in.Snapshot.State.FreeMargin.String(),
		Observed: marginAcct.String(),
		Message: fmt.Sprintf("Requires %s margin; %s free.",
			marginAcct.String(), in.Snapshot.State.FreeMargin.String()),
		Code: domain.RejectInsufficientMargin,
	})

	// --- Exposure ----------------------------------------------------------
	projectedGross := in.Snapshot.State.GrossExposure.MustAdd(notionalAcct)
	add(domain.RiskCheckResult{
		Name:     domain.CheckGrossExposure,
		Passed:   reducing || projectedGross.Decimal().LessThanOrEqual(in.Limits.MaxGrossExposure.Decimal()),
		Limit:    in.Limits.MaxGrossExposure.String(),
		Observed: projectedGross.String(),
		Message: fmt.Sprintf("Gross exposure would be %s against a limit of %s.",
			projectedGross.String(), in.Limits.MaxGrossExposure.String()),
		Code: domain.RejectExposureLimit,
	})

	signedNotional := notionalAcct.MulDecimal(in.Intent.Side.SignedMultiplier())
	projectedNet := in.Snapshot.State.NetExposure.MustAdd(signedNotional)
	add(domain.RiskCheckResult{
		Name: domain.CheckNetExposure,
		// A strictly reducing order moves net exposure toward zero, so it can
		// only improve this measure. It is safe to skip precisely BECAUSE
		// `reducing` excludes an over-closing side flip.
		Passed: reducing ||
			projectedNet.Abs().Decimal().LessThanOrEqual(in.Limits.MaxNetExposure.Decimal()),
		Limit:    in.Limits.MaxNetExposure.String(),
		Observed: projectedNet.String(),
		Message: fmt.Sprintf("Net exposure would be %s against a limit of %s.",
			projectedNet.String(), in.Limits.MaxNetExposure.String()),
		Code: domain.RejectExposureLimit,
	})

	instExposure := in.Snapshot.ExposureByInstrument[in.Intent.InstrumentID]
	if instExposure.Currency() == "" {
		instExposure = money.Zero(in.Account.Currency)
	}
	projectedInst := instExposure.MustAdd(notionalAcct)
	add(domain.RiskCheckResult{
		Name:     domain.CheckInstrumentExposure,
		Passed:   reducing || projectedInst.Decimal().LessThanOrEqual(in.Limits.MaxPerInstrumentExposure.Decimal()),
		Limit:    in.Limits.MaxPerInstrumentExposure.String(),
		Observed: projectedInst.String(),
		Message: fmt.Sprintf("Exposure to %s would be %s against a limit of %s.",
			in.Instrument.Symbol, projectedInst.String(), in.Limits.MaxPerInstrumentExposure.String()),
		Code: domain.RejectExposureLimit,
	})

	// Concentration: how much of total exposure sits in this one instrument.
	//
	// The check only means anything once the portfolio holds more than one
	// instrument. A single position is, by arithmetic, 100% of gross exposure,
	// so applying a 60% cap to a flat account would refuse every first trade
	// forever — the limit would read as a sensible diversification rule while
	// actually being a permanent halt. It is therefore evaluated only when the
	// account will hold exposure in at least one OTHER instrument.
	distinctInstruments := 0
	for id, amount := range in.Snapshot.ExposureByInstrument {
		if id != in.Intent.InstrumentID && amount.IsPositive() {
			distinctInstruments++
		}
	}
	concentration := decimal.Zero
	if projectedGross.IsPositive() {
		concentration = projectedInst.Decimal().Div(projectedGross.Decimal())
	}
	concentrationApplies := distinctInstruments > 0
	concentrationPassed := !concentrationApplies || reducing ||
		concentration.LessThanOrEqual(in.Limits.MaxConcentrationFraction)
	concentrationLimit := pct(in.Limits.MaxConcentrationFraction)
	if !concentrationApplies {
		concentrationLimit = "not applicable to a single-instrument portfolio"
	}
	add(domain.RiskCheckResult{
		Name:     domain.CheckConcentration,
		Passed:   concentrationPassed,
		Limit:    concentrationLimit,
		Observed: pct(concentration),
		Message: fmt.Sprintf("%s would be %s of gross exposure across %d other instrument(s), limit %s.",
			in.Instrument.Symbol, pct(concentration), distinctInstruments,
			pct(in.Limits.MaxConcentrationFraction)),
		Code: domain.RejectConcentration,
	})

	// --- Leverage ----------------------------------------------------------
	leverage := decimal.Zero
	if in.Snapshot.State.Equity.IsPositive() {
		leverage = projectedGross.Decimal().Div(in.Snapshot.State.Equity.Decimal())
	}
	effectiveMaxLeverage := decimal.Min(in.Limits.MaxLeverage, in.Authority.MaxLeverage)
	if in.Instrument.Spec.MaxLeverage.IsPositive() {
		effectiveMaxLeverage = decimal.Min(effectiveMaxLeverage, in.Instrument.Spec.MaxLeverage)
	}
	add(domain.RiskCheckResult{
		Name:     domain.CheckLeverage,
		Passed:   reducing || leverage.LessThanOrEqual(effectiveMaxLeverage),
		Limit:    effectiveMaxLeverage.StringFixed(2) + "x",
		Observed: leverage.StringFixed(2) + "x",
		Message: fmt.Sprintf("Effective leverage would be %sx against a limit of %sx.",
			leverage.StringFixed(2), effectiveMaxLeverage.StringFixed(2)),
		Code: domain.RejectLeverageLimit,
	})

	// --- Stop loss and per-trade risk --------------------------------------
	hasStop := in.Intent.StopLoss != nil
	add(domain.RiskCheckResult{
		Name:     domain.CheckStopLossRequired,
		Passed:   !in.Limits.RequireStopLoss || hasStop || reducing,
		Limit:    fmt.Sprintf("stop loss required: %v", in.Limits.RequireStopLoss),
		Observed: fmt.Sprintf("stop loss provided: %v", hasStop),
		Message:  "This account requires every position-opening order to carry a stop loss.",
		Code:     domain.RejectRiskLimit,
	})

	if hasStop && !reducing {
		riskCheck, err := e.evaluatePerTradeRisk(ctx, in, execPrice)
		if err != nil {
			add(failedCheck(domain.CheckRiskPerTrade, "n/a", "unknown", err.Error(), domain.RejectInternalError))
		} else {
			add(riskCheck)
		}
	}

	// --- Verdict -----------------------------------------------------------
	d.Approved = len(d.Failures()) == 0

	// Normalise the approved quantity onto the venue's step. Flooring can take
	// it to zero, which is a legitimate NO TRADE for a small account rather
	// than an error to be worked around.
	d.ApprovedQuantity = in.Instrument.Spec.NormaliseQuantity(d.ApprovedQuantity)
	if d.Approved && d.ApprovedQuantity.LessThan(in.Instrument.Spec.MinQuantity) {
		d.Approved = false
		d.Checks = append(d.Checks, domain.RiskCheckResult{
			Name:     "sizeable_quantity",
			Passed:   false,
			Limit:    in.Instrument.Spec.MinQuantity.String(),
			Observed: d.ApprovedQuantity.String(),
			Message: fmt.Sprintf(
				"No safe position size is possible: the smallest tradable size in %s (%s) exceeds this account's risk budget.",
				in.Instrument.Symbol, in.Instrument.Spec.MinQuantity),
			Code: domain.RejectQuantityInvalid,
		})
		last := d.Checks[len(d.Checks)-1]
		if d.FirstFailure == nil {
			d.FirstFailure = &last
		}
	}
	return d, nil
}

// evaluatePerTradeRisk bounds the loss implied by the order's stop distance.
//
// This is the check that matters most for a small account: it converts "how
// far away is my stop?" into "how much of my equity am I risking?", and
// refuses anything above the configured fraction.
func (e *Engine) evaluatePerTradeRisk(ctx context.Context, in Input, execPrice decimal.Decimal) (domain.RiskCheckResult, error) {
	stop := *in.Intent.StopLoss

	// A stop on the wrong side of the market is not a stop.
	if in.Intent.Side == domain.SideBuy && stop.GreaterThanOrEqual(execPrice) {
		return domain.RiskCheckResult{
			Name: domain.CheckRiskPerTrade, Passed: false,
			Limit: "stop below entry", Observed: stop.String(),
			Message: fmt.Sprintf("A buy order's stop loss (%s) must be below the entry price (%s).",
				stop, execPrice),
			Code: domain.RejectPriceInvalid,
		}, nil
	}
	if in.Intent.Side == domain.SideSell && stop.LessThanOrEqual(execPrice) {
		return domain.RiskCheckResult{
			Name: domain.CheckRiskPerTrade, Passed: false,
			Limit: "stop above entry", Observed: stop.String(),
			Message: fmt.Sprintf("A sell order's stop loss (%s) must be above the entry price (%s).",
				stop, execPrice),
			Code: domain.RejectPriceInvalid,
		}, nil
	}

	distance := execPrice.Sub(stop).Abs()
	lossQuote := money.New(distance.Mul(in.Intent.Quantity).Mul(in.Instrument.Spec.ContractSize), in.Instrument.QuoteCcy)
	lossAcct, err := e.toAccount(ctx, lossQuote, in.Account.Currency)
	if err != nil {
		return domain.RiskCheckResult{}, fmt.Errorf("cannot value the stop-loss distance in %s: %w", in.Account.Currency, err)
	}

	equity := in.Snapshot.State.Equity
	riskFraction := decimal.Zero
	if equity.IsPositive() {
		riskFraction = lossAcct.Decimal().Div(equity.Decimal())
	}

	return domain.RiskCheckResult{
		Name:     domain.CheckRiskPerTrade,
		Passed:   riskFraction.LessThanOrEqual(in.Limits.MaxRiskPerTradeFraction),
		Limit:    pct(in.Limits.MaxRiskPerTradeFraction) + " of equity",
		Observed: fmt.Sprintf("%s (%s)", lossAcct.String(), pct(riskFraction)),
		Message: fmt.Sprintf("If the stop is hit this loses %s, which is %s of equity; the limit is %s.",
			lossAcct.String(), pct(riskFraction), pct(in.Limits.MaxRiskPerTradeFraction)),
		Code: domain.RejectRiskLimit,
	}, nil
}

func (e *Engine) toAccount(ctx context.Context, amount money.Amount, target money.Currency) (money.Amount, error) {
	conv, err := e.converter.Convert(ctx, amount, target)
	if err != nil {
		return money.Amount{}, err
	}
	return conv.To, nil
}

// referencePrice is the price this order would actually transact at, which is
// what every size, margin and risk figure must be measured against.
//
// Using the current market price for every order type is wrong, and wrong in a
// way that is easy to miss: a limit order 20% below the market with a stop
// just under its limit has a tiny real risk, but measuring the stop distance
// from the MARKET price makes it look like a fifth of the account. The order
// is then refused for a risk it does not carry.
//
// The reference is therefore the worst price the order could fill at:
//
//	market      the current executable price, crossing the spread
//	limit       the limit price — the order cannot fill worse than this
//	stop        the stop price — where the order becomes a market order
//	stop_limit  the limit price — the worst fill the order permits
//
// Where the price the type requires is missing, the market price is used, and
// the instrument-rule gate earlier in the pipeline has already refused an
// order whose required price is absent.
func referencePrice(in Input) decimal.Decimal {
	market := in.Quote.ExecutionPrice(in.Intent.Side)

	switch in.Intent.Type {
	case domain.OrderTypeLimit, domain.OrderTypeStopLimit:
		if in.Intent.LimitPrice != nil && in.Intent.LimitPrice.IsPositive() {
			return *in.Intent.LimitPrice
		}
	case domain.OrderTypeStop:
		if in.Intent.StopPrice != nil && in.Intent.StopPrice.IsPositive() {
			return *in.Intent.StopPrice
		}
	}
	return market
}

func boolCheck(name domain.RiskCheckName, passed bool, msg string, code domain.RejectCode, kv ...string) domain.RiskCheckResult {
	r := domain.RiskCheckResult{Name: name, Passed: passed, Message: msg, Code: code}
	if len(kv) >= 2 {
		r.Limit, r.Observed = kv[0], kv[1]
	}
	return r
}

func failedCheck(name domain.RiskCheckName, limit, observed, msg string, code domain.RejectCode) domain.RiskCheckResult {
	return domain.RiskCheckResult{
		Name: name, Passed: false, Limit: limit, Observed: observed, Message: msg, Code: code,
	}
}

func pct(d decimal.Decimal) string {
	return d.Mul(decimal.NewFromInt(100)).StringFixed(2) + "%"
}

func eventObserved(in Input) string {
	if !in.EventBlackout {
		return "no high-impact event nearby"
	}
	return fmt.Sprintf("%s at %s", in.EventName, in.EventAt.UTC().Format("15:04 MST"))
}

func eventMessage(in Input) string {
	if !in.EventBlackout {
		return "No high-impact economic release is inside the blackout window."
	}
	return fmt.Sprintf(
		"Automated trading is paused around %s (%s): spreads widen and stops slip through high-impact releases.",
		in.EventName, in.EventAt.UTC().Format("15:04 MST"))
}
