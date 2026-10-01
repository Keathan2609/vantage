package risk

import (
	"context"
	"errors"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/money"
)

// SizingPolicy names a position-sizing method.
type SizingPolicy string

const (
	// SizingFixedQuantity uses a constant lot size.
	SizingFixedQuantity SizingPolicy = "fixed_quantity"
	// SizingFixedNotional targets a constant exposure value.
	SizingFixedNotional SizingPolicy = "fixed_notional"
	// SizingPercentRisk sizes so that the stop-loss distance costs a fixed
	// fraction of equity. This is the default, and the only one that makes
	// position size a function of where the stop actually is.
	SizingPercentRisk SizingPolicy = "percent_risk"
	// SizingATRRisk sizes off a volatility-derived stop distance.
	SizingATRRisk SizingPolicy = "atr_risk"
	// SizingCappedKelly is a fractional Kelly, hard-capped. Full Kelly is
	// never offered: it maximises long-run growth only under assumptions
	// (known, stationary edge; infinite divisibility; no ruin threshold) that
	// no discretionary trading account satisfies, and its drawdowns are
	// routinely unsurvivable in practice.
	SizingCappedKelly SizingPolicy = "capped_kelly"
)

// Valid reports whether the policy is recognised.
func (p SizingPolicy) Valid() bool {
	switch p {
	case SizingFixedQuantity, SizingFixedNotional, SizingPercentRisk, SizingATRRisk, SizingCappedKelly:
		return true
	}
	return false
}

// SizingRequest is the input to a sizing calculation.
type SizingRequest struct {
	Policy     SizingPolicy
	Instrument domain.Instrument
	Account    domain.Account
	Equity     money.Amount
	Side       domain.OrderSide
	EntryPrice decimal.Decimal
	StopPrice  *decimal.Decimal
	// ATR is the average true range in price units, for volatility sizing.
	ATR decimal.Decimal
	// ATRMultiple sets the stop distance as a multiple of ATR.
	ATRMultiple decimal.Decimal
	// RiskFraction is the fraction of equity to put at risk, already clamped
	// by the account's own ceiling before it reaches here.
	RiskFraction decimal.Decimal
	// FixedQuantity and FixedNotional serve their respective policies.
	FixedQuantity decimal.Decimal
	FixedNotional money.Amount
	// KellyFraction scales the Kelly result; capped at MaxKellyFraction.
	KellyFraction  decimal.Decimal
	WinProbability decimal.Decimal
	PayoffRatio    decimal.Decimal
	// ToAccountCurrency converts a quote-currency amount into the account's
	// currency. Sizing cannot proceed without it when the two differ.
	ToAccountCurrency func(ctx context.Context, amount money.Amount) (money.Amount, error)
}

// MaxKellyFraction caps fractional Kelly at a quarter. Even that is aggressive
// for a strategy whose edge is estimated from a few hundred trades.
var MaxKellyFraction = decimal.RequireFromString("0.25")

// SizingResult is a computed position size with its reasoning.
type SizingResult struct {
	Quantity     decimal.Decimal
	Policy       SizingPolicy
	RiskAmount   money.Amount
	StopDistance decimal.Decimal
	// Explanation is shown to the user next to the proposed order.
	Explanation string
	// Feasible is false when no size satisfies the constraints. For a small
	// account this is a routine outcome, not an error: the honest answer is
	// that this trade cannot be taken safely.
	Feasible bool
	Reason   string
}

// ErrSizingUnavailable means the calculation could not be performed at all.
var ErrSizingUnavailable = errors.New("risk: position size cannot be computed")

// Size computes a position size under the requested policy.
//
// Every policy ends the same way: the result is floored onto the instrument's
// quantity step and checked against its minimum. Sizing never rounds up to
// reach a tradable size, because doing so would take more risk than the policy
// authorised -- which is precisely the failure mode that empties small accounts.
func Size(ctx context.Context, req SizingRequest) (SizingResult, error) {
	res := SizingResult{Policy: req.Policy, RiskAmount: money.Zero(req.Account.Currency)}
	spec := req.Instrument.Spec

	if req.EntryPrice.LessThanOrEqual(decimal.Zero) {
		return res, fmt.Errorf("%w: entry price must be positive", ErrSizingUnavailable)
	}

	var raw decimal.Decimal

	switch req.Policy {
	case SizingFixedQuantity:
		raw = req.FixedQuantity
		res.Explanation = fmt.Sprintf("Fixed size of %s lots.", req.FixedQuantity)

	case SizingFixedNotional:
		if req.FixedNotional.IsZero() {
			return res, fmt.Errorf("%w: fixed notional not set", ErrSizingUnavailable)
		}
		perLot := req.EntryPrice.Mul(spec.ContractSize)
		if perLot.IsZero() {
			return res, fmt.Errorf("%w: instrument contract value is zero", ErrSizingUnavailable)
		}
		raw = req.FixedNotional.Decimal().Div(perLot)
		res.Explanation = fmt.Sprintf("Targeting %s of exposure.", req.FixedNotional.String())

	case SizingPercentRisk, SizingATRRisk:
		stopDistance, err := stopDistanceFor(req)
		if err != nil {
			return res, err
		}
		res.StopDistance = stopDistance
		if stopDistance.LessThanOrEqual(decimal.Zero) {
			res.Reason = "the stop distance is zero, so no size can be risk-controlled"
			return res, nil
		}

		riskBudget := req.Equity.MulDecimal(req.RiskFraction)
		res.RiskAmount = riskBudget.RoundLedger()

		// Loss per lot, in the instrument's quote currency, converted once.
		lossPerLotQuote := money.New(stopDistance.Mul(spec.ContractSize), req.Instrument.QuoteCcy)
		lossPerLot := lossPerLotQuote
		if req.Instrument.QuoteCcy != req.Account.Currency {
			if req.ToAccountCurrency == nil {
				return res, fmt.Errorf("%w: no currency conversion available for %s -> %s",
					ErrSizingUnavailable, req.Instrument.QuoteCcy, req.Account.Currency)
			}
			converted, err := req.ToAccountCurrency(ctx, lossPerLotQuote)
			if err != nil {
				return res, fmt.Errorf("%w: %v", ErrSizingUnavailable, err)
			}
			lossPerLot = converted
		}
		if !lossPerLot.IsPositive() {
			res.Reason = "the loss implied by the stop is not measurable"
			return res, nil
		}
		raw = riskBudget.Decimal().Div(lossPerLot.Decimal())
		res.Explanation = fmt.Sprintf(
			"Risking %s (%s of equity) with a stop %s away: %s per lot at risk.",
			riskBudget.RoundLedger().String(), pct(req.RiskFraction),
			stopDistance.String(), lossPerLot.RoundLedger().String())

	case SizingCappedKelly:
		f, err := cappedKellyFraction(req.WinProbability, req.PayoffRatio, req.KellyFraction)
		if err != nil {
			return res, err
		}
		if !f.IsPositive() {
			res.Reason = "the estimated edge is not positive, so Kelly sizing recommends no position"
			return res, nil
		}
		notional := req.Equity.MulDecimal(f)
		perLot := req.EntryPrice.Mul(spec.ContractSize)
		if perLot.IsZero() {
			return res, fmt.Errorf("%w: instrument contract value is zero", ErrSizingUnavailable)
		}
		raw = notional.Decimal().Div(perLot)
		res.Explanation = fmt.Sprintf(
			"Capped Kelly at %s of equity (research use only; full Kelly is never applied).", pct(f))

	default:
		return res, fmt.Errorf("%w: unknown sizing policy %q", ErrSizingUnavailable, req.Policy)
	}

	if raw.LessThanOrEqual(decimal.Zero) {
		res.Reason = "the computed size is zero or negative"
		return res, nil
	}

	// Floor onto the venue's step, then apply the instrument's own ceiling.
	q := spec.NormaliseQuantity(raw)
	if spec.MaxQuantity.IsPositive() && q.GreaterThan(spec.MaxQuantity) {
		q = spec.NormaliseQuantity(spec.MaxQuantity)
	}
	res.Quantity = q

	if q.LessThan(spec.MinQuantity) {
		res.Feasible = false
		res.Reason = fmt.Sprintf(
			"the risk budget supports %s lots but %s trades in minimum increments of %s: no safe size exists",
			raw.StringFixed(4), req.Instrument.Symbol, spec.MinQuantity)
		return res, nil
	}

	res.Feasible = true
	return res, nil
}

// stopDistanceFor derives the stop distance for risk-based policies.
func stopDistanceFor(req SizingRequest) (decimal.Decimal, error) {
	if req.Policy == SizingATRRisk {
		if !req.ATR.IsPositive() {
			return decimal.Zero, fmt.Errorf("%w: ATR sizing requires a positive ATR", ErrSizingUnavailable)
		}
		mult := req.ATRMultiple
		if !mult.IsPositive() {
			mult = decimal.NewFromInt(2)
		}
		return req.ATR.Mul(mult), nil
	}
	if req.StopPrice == nil {
		return decimal.Zero, fmt.Errorf("%w: percent-risk sizing requires a stop price", ErrSizingUnavailable)
	}
	return req.EntryPrice.Sub(*req.StopPrice).Abs(), nil
}

// cappedKellyFraction computes f* = p - (1-p)/b, scaled and capped.
//
// The cap is not a preference. Kelly assumes the edge is known exactly; in
// trading it is estimated, usually from a small and non-stationary sample, and
// overestimating the edge makes Kelly overbet in exactly the regime where the
// estimate was wrong.
func cappedKellyFraction(p, b, scale decimal.Decimal) (decimal.Decimal, error) {
	if p.LessThanOrEqual(decimal.Zero) || p.GreaterThanOrEqual(decimal.NewFromInt(1)) {
		return decimal.Zero, fmt.Errorf("%w: win probability must be strictly between 0 and 1", ErrSizingUnavailable)
	}
	if !b.IsPositive() {
		return decimal.Zero, fmt.Errorf("%w: payoff ratio must be positive", ErrSizingUnavailable)
	}
	one := decimal.NewFromInt(1)
	f := p.Sub(one.Sub(p).Div(b))
	if f.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero, nil
	}
	if !scale.IsPositive() {
		scale = decimal.RequireFromString("0.5")
	}
	f = f.Mul(scale)
	if f.GreaterThan(MaxKellyFraction) {
		f = MaxKellyFraction
	}
	return f, nil
}
