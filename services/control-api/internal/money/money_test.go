package money

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

func TestArithmeticRefusesCurrencyMixing(t *testing.T) {
	// An account in ZAR and an instrument quoted in USD is the normal case for
	// Vantage, so mixing them must be a hard error rather than a silent
	// coercion that quietly corrupts the ledger.
	zar := MustParse("500.00", ZAR)
	usd := MustParse("27.00", USD)

	if _, err := zar.Add(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Add across currencies: err = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := zar.Sub(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Sub across currencies: err = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := zar.Cmp(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Cmp across currencies: err = %v, want ErrCurrencyMismatch", err)
	}
}

func TestArithmeticSameCurrency(t *testing.T) {
	a := MustParse("500.00", ZAR)
	b := MustParse("125.50", ZAR)

	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if sum.StringFixed() != "625.50" {
		t.Errorf("sum = %s, want 625.50", sum.StringFixed())
	}

	diff, err := a.Sub(b)
	if err != nil {
		t.Fatalf("Sub: %v", err)
	}
	if diff.StringFixed() != "374.50" {
		t.Errorf("diff = %s, want 374.50", diff.StringFixed())
	}
}

func TestNoBinaryFloatingPointError(t *testing.T) {
	// The canonical float64 failure: 0.1 + 0.2 != 0.3. Decimal arithmetic must
	// be exact, because these values accumulate into balances.
	a := MustParse("0.1", ZAR)
	b := MustParse("0.2", ZAR)
	sum := a.MustAdd(b)
	if !sum.Decimal().Equal(decimal.RequireFromString("0.3")) {
		t.Errorf("0.1 + 0.2 = %s, want exactly 0.3", sum.Decimal())
	}

	// Repeated accumulation must not drift.
	total := Zero(ZAR)
	cent := MustParse("0.01", ZAR)
	for i := 0; i < 100; i++ {
		total = total.MustAdd(cent)
	}
	if !total.Decimal().Equal(decimal.RequireFromString("1")) {
		t.Errorf("100 x 0.01 = %s, want exactly 1", total.Decimal())
	}
}

func TestRoundLedgerUsesCurrencyScale(t *testing.T) {
	if got := MustParse("123.456", ZAR).RoundLedger().StringFixed(); got != "123.46" {
		t.Errorf("ZAR rounding = %s, want 123.46", got)
	}
	// JPY has no minor unit.
	if got := MustParse("123.6", JPY).RoundLedger().StringFixed(); got != "124" {
		t.Errorf("JPY rounding = %s, want 124", got)
	}
	// Half away from zero, in both directions.
	if got := MustParse("2.005", USD).RoundLedger().StringFixed(); got != "2.01" {
		t.Errorf("2.005 -> %s, want 2.01", got)
	}
	if got := MustParse("-2.005", USD).RoundLedger().StringFixed(); got != "-2.01" {
		t.Errorf("-2.005 -> %s, want -2.01", got)
	}
}

func TestIntermediatePrecisionIsNotLost(t *testing.T) {
	// Rounding must happen at the boundary, not mid-calculation: a third of a
	// cent per unit over many units is real money.
	a := MustParse("0.333", USD)
	scaled := a.MulDecimal(decimal.NewFromInt(3))
	if scaled.StringFixed() != "1.00" {
		t.Errorf("display = %s, want 1.00", scaled.StringFixed())
	}
	if !scaled.Decimal().Equal(decimal.RequireFromString("0.999")) {
		t.Errorf("underlying value = %s, want 0.999 retained", scaled.Decimal())
	}
}

func TestStringAlwaysCarriesCurrency(t *testing.T) {
	// No monetary value may be rendered without its currency: a bare "500" in
	// a UI or an export is exactly the ambiguity this type exists to prevent.
	if got := MustParse("500", ZAR).String(); got != "500.00 ZAR" {
		t.Errorf("String() = %q, want \"500.00 ZAR\"", got)
	}
}

func TestParseCurrency(t *testing.T) {
	if c, err := ParseCurrency("zar"); err != nil || c != ZAR {
		t.Errorf("ParseCurrency(zar) = %v, %v", c, err)
	}
	for _, bad := range []string{"", "Z", "ZARR", "Z1R", "12 "} {
		if _, err := ParseCurrency(bad); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("ParseCurrency(%q) should fail", bad)
		}
	}
}

func TestDivisionByZeroIsAnError(t *testing.T) {
	if _, err := MustParse("100", ZAR).DivDecimal(decimal.Zero); err == nil {
		t.Error("division by zero must error rather than panic or yield Inf")
	}
}

func TestZeroValueAmountCannotSilentlyJoinRealCurrency(t *testing.T) {
	// The zero value has no currency. Adding it to a real amount must fail
	// rather than being treated as "zero of whatever you have", which would
	// let an uninitialised field pass through accounting unnoticed.
	var uninitialised Amount
	if _, err := MustParse("100", ZAR).Add(uninitialised); !errors.Is(err, ErrCurrencyMismatch) {
		t.Error("uninitialised Amount must not be addable to a real currency")
	}
}
