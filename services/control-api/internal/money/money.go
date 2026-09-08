// Package money provides the authoritative numeric types for Vantage.
//
// Binary floating point is never used for money, prices, quantities or any
// value that feeds the ledger. Every monetary value carries its currency, and
// arithmetic between mismatched currencies is a hard error rather than a silent
// coercion: an account denominated in ZAR trading an instrument quoted in USD
// must convert explicitly, through an FXConversionProvider, never implicitly.
//
// Rounding policy
//
//	Ledger amounts    : rounded half-away-from-zero to the currency's minor unit
//	                    (2 decimal places for ZAR/USD/EUR) only at the point of
//	                    persistence or display. Intermediate arithmetic keeps
//	                    full decimal precision.
//	Prices            : rounded to the instrument's price precision using the
//	                    instrument's tick size (see domain.InstrumentSpec).
//	Quantities        : floored to the instrument's quantity step. Rounding a
//	                    quantity up could exceed a risk limit that was checked
//	                    against the pre-rounded value, so quantity always rounds
//	                    toward zero.
package money

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// Currency is an ISO 4217 alphabetic code. It is stored uppercase.
type Currency string

const (
	ZAR Currency = "ZAR"
	USD Currency = "USD"
	EUR Currency = "EUR"
	GBP Currency = "GBP"
	JPY Currency = "JPY"
	XAU Currency = "XAU" // gold, troy ounce; an asset unit rather than fiat
)

// ErrCurrencyMismatch is returned when arithmetic is attempted across currencies.
var ErrCurrencyMismatch = errors.New("money: currency mismatch")

// ErrInvalidCurrency is returned for empty or malformed currency codes.
var ErrInvalidCurrency = errors.New("money: invalid currency")

// minorUnits maps a currency to its accounting scale. Currencies absent from
// this table default to 2, which is correct for every currency Vantage
// currently supports and conservative for the rest.
var minorUnits = map[Currency]int32{
	ZAR: 2,
	USD: 2,
	EUR: 2,
	GBP: 2,
	JPY: 0,
	XAU: 8,
}

// ParseCurrency validates and normalises a currency code.
func ParseCurrency(s string) (Currency, error) {
	c := Currency(strings.ToUpper(strings.TrimSpace(s)))
	if len(c) != 3 {
		return "", fmt.Errorf("%w: %q", ErrInvalidCurrency, s)
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return "", fmt.Errorf("%w: %q", ErrInvalidCurrency, s)
		}
	}
	return c, nil
}

// MinorUnits returns the number of decimal places used for ledger rounding.
func (c Currency) MinorUnits() int32 {
	if u, ok := minorUnits[c]; ok {
		return u
	}
	return 2
}

func (c Currency) String() string { return string(c) }

// Amount is a decimal quantity of a specific currency.
//
// The zero value is not usable for arithmetic against a real currency: it has
// an empty currency code and will report a mismatch. Construct amounts with
// New, Zero or MustParse.
type Amount struct {
	value    decimal.Decimal
	currency Currency
}

// New builds an Amount from a decimal value and currency.
func New(value decimal.Decimal, c Currency) Amount {
	return Amount{value: value, currency: c}
}

// Zero returns a zero Amount in the given currency.
func Zero(c Currency) Amount { return Amount{value: decimal.Zero, currency: c} }

// FromString parses a decimal string, e.g. "500.00".
func FromString(s string, c Currency) (Amount, error) {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return Amount{}, fmt.Errorf("money: parse %q: %w", s, err)
	}
	return Amount{value: d, currency: c}, nil
}

// MustParse is FromString for constants and tests; it panics on bad input.
func MustParse(s string, c Currency) Amount {
	a, err := FromString(s, c)
	if err != nil {
		panic(err)
	}
	return a
}

// FromInt builds an Amount from a whole number of major units.
func FromInt(n int64, c Currency) Amount {
	return Amount{value: decimal.NewFromInt(n), currency: c}
}

// Decimal exposes the underlying value. Callers must not assume a currency.
func (a Amount) Decimal() decimal.Decimal { return a.value }

// Currency returns the amount's currency.
func (a Amount) Currency() Currency { return a.currency }

// IsZero reports whether the value is exactly zero.
func (a Amount) IsZero() bool { return a.value.IsZero() }

// IsNegative reports whether the value is below zero.
func (a Amount) IsNegative() bool { return a.value.IsNegative() }

// IsPositive reports whether the value is above zero.
func (a Amount) IsPositive() bool { return a.value.IsPositive() }

// Sign returns -1, 0 or 1.
func (a Amount) Sign() int { return a.value.Sign() }

func (a Amount) sameCurrency(b Amount) error {
	if a.currency != b.currency {
		return fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, a.currency, b.currency)
	}
	return nil
}

// Add returns a+b, erroring on currency mismatch.
func (a Amount) Add(b Amount) (Amount, error) {
	if err := a.sameCurrency(b); err != nil {
		return Amount{}, err
	}
	return Amount{value: a.value.Add(b.value), currency: a.currency}, nil
}

// Sub returns a-b, erroring on currency mismatch.
func (a Amount) Sub(b Amount) (Amount, error) {
	if err := a.sameCurrency(b); err != nil {
		return Amount{}, err
	}
	return Amount{value: a.value.Sub(b.value), currency: a.currency}, nil
}

// MustAdd is Add for call sites that have already established a shared
// currency (for example summing a slice built from one account). It panics on
// mismatch, which indicates a programming error rather than bad input.
func (a Amount) MustAdd(b Amount) Amount {
	out, err := a.Add(b)
	if err != nil {
		panic(err)
	}
	return out
}

// MulDecimal scales an amount by a dimensionless factor.
func (a Amount) MulDecimal(f decimal.Decimal) Amount {
	return Amount{value: a.value.Mul(f), currency: a.currency}
}

// DivDecimal divides an amount by a dimensionless factor.
func (a Amount) DivDecimal(f decimal.Decimal) (Amount, error) {
	if f.IsZero() {
		return Amount{}, errors.New("money: division by zero")
	}
	return Amount{value: a.value.Div(f), currency: a.currency}, nil
}

// Neg returns -a.
func (a Amount) Neg() Amount {
	return Amount{value: a.value.Neg(), currency: a.currency}
}

// Abs returns |a|.
func (a Amount) Abs() Amount {
	return Amount{value: a.value.Abs(), currency: a.currency}
}

// Cmp compares two amounts of the same currency.
func (a Amount) Cmp(b Amount) (int, error) {
	if err := a.sameCurrency(b); err != nil {
		return 0, err
	}
	return a.value.Cmp(b.value), nil
}

// GreaterThan reports a>b. A currency mismatch reports false and an error.
func (a Amount) GreaterThan(b Amount) (bool, error) {
	c, err := a.Cmp(b)
	return c > 0, err
}

// LessThan reports a<b.
func (a Amount) LessThan(b Amount) (bool, error) {
	c, err := a.Cmp(b)
	return c < 0, err
}

// RoundLedger rounds to the currency's minor unit, half away from zero.
// This is applied at persistence and display boundaries, not mid-calculation.
func (a Amount) RoundLedger() Amount {
	return Amount{value: a.value.Round(a.currency.MinorUnits()), currency: a.currency}
}

// String renders the value with its currency, e.g. "500.00 ZAR".
func (a Amount) String() string {
	return fmt.Sprintf("%s %s", a.value.StringFixed(a.currency.MinorUnits()), a.currency)
}

// StringFixed renders only the numeric part at ledger scale.
func (a Amount) StringFixed() string {
	return a.value.StringFixed(a.currency.MinorUnits())
}

// Value implements driver.Valuer so amounts persist as NUMERIC.
// The currency is always stored in an adjacent column; an Amount never
// round-trips through the database without its currency.
func (a Amount) Value() (driver.Value, error) {
	return a.value.String(), nil
}

// Quantity is an instrument quantity (lots, units, ounces). It is a bare
// decimal because its unit is defined by the instrument it accompanies.
type Quantity = decimal.Decimal

// Price is an instrument price in that instrument's quote currency.
type Price = decimal.Decimal

// Zero values reused across the codebase.
var (
	DecZero = decimal.Zero
	DecOne  = decimal.NewFromInt(1)
)
