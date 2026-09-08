package domain

import (
	"errors"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/money"
)

// AssetClass groups instruments that share market conventions.
type AssetClass string

const (
	AssetClassMetal     AssetClass = "metal"
	AssetClassForex     AssetClass = "forex"
	AssetClassIndex     AssetClass = "index"
	AssetClassCommodity AssetClass = "commodity"
	AssetClassCrypto    AssetClass = "crypto"
	AssetClassEquity    AssetClass = "equity"
)

// OrderTypeSet records which order types an instrument accepts. Vantage never
// emulates an order type the venue does not support: an unsupported type is
// rejected at validation rather than synthesised client-side, because a
// synthetic stop that lives only in Vantage silently disappears when Vantage
// is down.
type OrderTypeSet []OrderType

// Contains reports whether t is permitted.
func (s OrderTypeSet) Contains(t OrderType) bool {
	for _, v := range s {
		if v == t {
			return true
		}
	}
	return false
}

// Instrument is a tradable symbol together with the venue rules that govern
// order construction. Nothing in generic domain logic may assume XAUUSD: the
// gold-specific values live in seed data, not in code.
type Instrument struct {
	ID       string
	Symbol   string
	Name     string
	Class    AssetClass
	BaseCcy  money.Currency
	QuoteCcy money.Currency
	Enabled  bool
	Spec     InstrumentSpec
	// SessionCalendarID references the trading-session calendar this
	// instrument follows (see MarketClock).
	SessionCalendarID string
}

// InstrumentSpec captures the venue's arithmetic rules. Every order is
// validated against these before it can reach a broker adapter.
type InstrumentSpec struct {
	// ContractSize is the number of base units in one lot. For XAUUSD at a
	// typical retail CFD venue this is 100 (troy ounces per lot).
	ContractSize decimal.Decimal
	// PricePrecision is the number of decimal places in a quoted price.
	PricePrecision int32
	// TickSize is the minimum price increment.
	TickSize decimal.Decimal
	// QuantityPrecision is the number of decimal places in a lot size.
	QuantityPrecision int32
	// MinQuantity, MaxQuantity and QuantityStep bound order size.
	MinQuantity  decimal.Decimal
	MaxQuantity  decimal.Decimal
	QuantityStep decimal.Decimal
	// MarginRate is the fraction of notional required as margin, i.e. the
	// reciprocal of leverage. 0.005 corresponds to 1:200.
	MarginRate decimal.Decimal
	// MaxLeverage bounds the leverage any account may use on this instrument,
	// independently of the account's own leverage setting.
	MaxLeverage decimal.Decimal
	// SupportedOrderTypes lists order types the venue accepts.
	SupportedOrderTypes OrderTypeSet
	// CommissionPerLot is charged per lot per side, expressed in the QUOTE
	// currency -- the same convention as the swap rates below. The comment
	// here used to say "account currency", which contradicted both the venue
	// (it stamps commission_ccy with the quote currency) and the booking path
	// (it converts from that currency), and a reader trusting the comment
	// would double-convert.
	CommissionPerLot decimal.Decimal
	// SwapLongPerLot and SwapShortPerLot are financing charges applied per lot
	// per day held, expressed in the quote currency.
	SwapLongPerLot  decimal.Decimal
	SwapShortPerLot decimal.Decimal
}

var (
	// ErrQuantityBelowMinimum means the requested size is smaller than the
	// venue permits. For a small account this is a routine, expected outcome:
	// the correct response is NO TRADE, never rounding up into a position the
	// account cannot afford.
	ErrQuantityBelowMinimum = errors.New("quantity below instrument minimum")
	ErrQuantityAboveMaximum = errors.New("quantity above instrument maximum")
	ErrQuantityStep         = errors.New("quantity is not a multiple of the instrument step")
	ErrPriceTickSize        = errors.New("price is not a multiple of the instrument tick size")
	ErrOrderTypeUnsupported = errors.New("order type not supported for instrument")
)

// NormaliseQuantity floors a requested quantity onto the instrument's step.
// Flooring (never rounding up) guarantees the normalised size is no larger
// than the size that risk checks approved.
func (s InstrumentSpec) NormaliseQuantity(q decimal.Decimal) decimal.Decimal {
	if s.QuantityStep.IsZero() {
		return q.Truncate(s.QuantityPrecision)
	}
	steps := q.Div(s.QuantityStep).Floor()
	return steps.Mul(s.QuantityStep).Truncate(s.QuantityPrecision)
}

// ValidateQuantity enforces min/max/step. It is applied after normalisation.
func (s InstrumentSpec) ValidateQuantity(q decimal.Decimal) error {
	if q.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("%w: %s", ErrQuantityBelowMinimum, q.String())
	}
	if q.LessThan(s.MinQuantity) {
		return fmt.Errorf("%w: %s < %s", ErrQuantityBelowMinimum, q.String(), s.MinQuantity.String())
	}
	if s.MaxQuantity.IsPositive() && q.GreaterThan(s.MaxQuantity) {
		return fmt.Errorf("%w: %s > %s", ErrQuantityAboveMaximum, q.String(), s.MaxQuantity.String())
	}
	if !s.QuantityStep.IsZero() {
		rem := q.Div(s.QuantityStep)
		if !rem.Equal(rem.Floor()) {
			return fmt.Errorf("%w: %s is not a multiple of %s", ErrQuantityStep, q.String(), s.QuantityStep.String())
		}
	}
	return nil
}

// ValidatePrice enforces tick-size alignment for prices the caller supplies
// (limit, stop, take-profit levels).
func (s InstrumentSpec) ValidatePrice(p decimal.Decimal) error {
	if p.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("price must be positive, got %s", p.String())
	}
	if s.TickSize.IsZero() {
		return nil
	}
	steps := p.Div(s.TickSize)
	if !steps.Equal(steps.Round(0)) {
		return fmt.Errorf("%w: %s is not a multiple of %s", ErrPriceTickSize, p.String(), s.TickSize.String())
	}
	return nil
}

// RoundPrice snaps a price to the instrument's tick grid.
func (s InstrumentSpec) RoundPrice(p decimal.Decimal) decimal.Decimal {
	if s.TickSize.IsZero() {
		return p.Round(s.PricePrecision)
	}
	return p.Div(s.TickSize).Round(0).Mul(s.TickSize).Round(s.PricePrecision)
}

// Notional returns quantity * contract size * price, expressed in the
// instrument's QUOTE currency. Converting that into the account's currency is
// a separate, explicit step; this function never performs FX conversion.
func (i Instrument) Notional(qty, price decimal.Decimal) money.Amount {
	return money.New(qty.Mul(i.Spec.ContractSize).Mul(price), i.QuoteCcy)
}

// MarginRequired returns the margin a position of this size consumes, in the
// instrument's quote currency, before FX conversion to the account currency.
func (i Instrument) MarginRequired(qty, price decimal.Decimal) money.Amount {
	notional := i.Notional(qty, price)
	return notional.MulDecimal(i.Spec.MarginRate)
}
