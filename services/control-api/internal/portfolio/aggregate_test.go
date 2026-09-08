package portfolio

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/money"
)

const zar = money.Currency("ZAR")

func zarAmount(t *testing.T, v string) money.Amount {
	t.Helper()
	a, err := money.FromString(v, zar)
	if err != nil {
		t.Fatalf("money.FromString(%q): %v", v, err)
	}
	return a
}

// valued builds a position that was successfully marked to market.
func valued(t *testing.T, instrument string, quoteCcy money.Currency,
	side domain.OrderSide, notional, margin, unrealized string) PositionView {
	t.Helper()
	return PositionView{
		Position: domain.Position{
			InstrumentID: instrument,
			Side:         side,
			Quantity:     decimal.NewFromFloat(0.10),
		},
		Instrument:    domain.Instrument{ID: instrument, QuoteCcy: quoteCcy},
		UnrealizedPnL: zarAmount(t, unrealized),
		NotionalValue: zarAmount(t, notional),
		MarginUsed:    zarAmount(t, margin),
		Valued:        true,
	}
}

// unvaluedPosition builds a position that could not be priced. The money
// fields are left as their zero values on purpose, because that is exactly
// what valuePosition returns when a feed or an FX rate is missing -- and the
// point of the tests below is that those zeroes must not reach a total.
func unvaluedPosition(instrument string) PositionView {
	return PositionView{
		Position:      domain.Position{InstrumentID: instrument, Side: domain.SideBuy},
		Instrument:    domain.Instrument{ID: instrument, QuoteCcy: "USD"},
		Valued:        false,
		ValuationNote: "no price available for this instrument",
	}
}

// mustEqual compares exact decimal values, not formatted strings, because
// money comparisons in this codebase are never allowed to route through a
// float or a display format.
func mustEqual(t *testing.T, label string, got money.Amount, want string, why string) {
	t.Helper()
	if got.Currency() != zar {
		t.Errorf("%s has currency %q, want ZAR", label, got.Currency())
	}
	if !got.Decimal().Equal(decimal.RequireFromString(want)) {
		t.Errorf("%s = %s, want %s%s", label, got.Decimal().String(), want, why)
	}
}

func TestAnUnvaluedPositionIsCountedButNeverTreatedAsFlat(t *testing.T) {
	agg := aggregate([]PositionView{
		valued(t, "XAUUSD", "USD", domain.SideBuy, "1000.00", "50.00", "12.00"),
		unvaluedPosition("EURUSD"),
	}, zar)

	if agg.Unvalued != 1 {
		t.Fatalf("unvalued count = %d, want 1", agg.Unvalued)
	}
	// The unvalued position contributes nothing -- not a zero that would make
	// the account look flatter than it is, and not a guess.
	mustEqual(t, "gross exposure", agg.Gross, "1000",
		" -- the unvalued leg must not add anything")
	if _, ok := agg.ByInstrument["EURUSD"]; ok {
		t.Error("an unpriced instrument appeared in the exposure map; a caller " +
			"reading that map would believe the exposure was measured")
	}
	if _, ok := agg.ByCurrency["USD"]; !ok {
		t.Error("the priced leg's quote currency is missing from the currency map")
	}
}

func TestEveryFigureIsUnderstatedWhenAPositionCannotBePriced(t *testing.T) {
	// This is the reason Unvalued exists rather than being silently dropped:
	// a caller that ignores it will act on numbers it believes are complete.
	agg := aggregate([]PositionView{unvaluedPosition("XAUUSD")}, zar)

	if agg.Unvalued == 0 {
		t.Fatal("an unpriceable position was not reported as unvalued, so the " +
			"snapshot claims complete knowledge of exposure it does not have")
	}
	if !agg.Gross.IsZero() || !agg.MarginUsed.IsZero() {
		t.Errorf("unvalued position leaked into the totals: gross=%s margin=%s",
			agg.Gross.String(), agg.MarginUsed.String())
	}
}

func TestGrossAndNetDisagreeForAHedgedBook(t *testing.T) {
	// A long and an equal short is flat directionally and fully exposed in
	// gross terms. Reporting only the net would say this account is facing no
	// market at all, which is how a hedged book gets margin-called.
	agg := aggregate([]PositionView{
		valued(t, "XAUUSD", "USD", domain.SideBuy, "5000.00", "250.00", "10.00"),
		valued(t, "XAUUSD", "USD", domain.SideSell, "5000.00", "250.00", "-4.00"),
	}, zar)

	mustEqual(t, "net exposure", agg.Net, "0", "")
	mustEqual(t, "gross exposure", agg.Gross, "10000", "")
	// Per-instrument exposure is absolute for the same reason: the
	// concentration check asks how much of this instrument the account holds,
	// not which way it is leaning.
	mustEqual(t, "per-instrument exposure", agg.ByInstrument["XAUUSD"], "10000", "")
	mustEqual(t, "USD exposure", agg.ByCurrency["USD"], "0", " -- the two legs net")
	mustEqual(t, "margin used", agg.MarginUsed, "500", " -- a hedge still costs margin")
}

func TestCurrencyExposureNetsAcrossInstrumentsButGrossDoesNot(t *testing.T) {
	// Long XAUUSD is short USD; long USDJPY is long USD. Netting them is the
	// entire purpose of the currency map -- an account can be concentrated in
	// USD without holding a single position whose name contains it.
	agg := aggregate([]PositionView{
		valued(t, "XAUUSD", "USD", domain.SideBuy, "3000.00", "150.00", "0.00"),
		valued(t, "EURUSD", "USD", domain.SideSell, "1000.00", "50.00", "0.00"),
		valued(t, "USDJPY", "JPY", domain.SideBuy, "2000.00", "100.00", "0.00"),
	}, zar)

	mustEqual(t, "USD exposure", agg.ByCurrency["USD"], "2000",
		" -- 3000 long netted against 1000 short")
	mustEqual(t, "JPY exposure", agg.ByCurrency["JPY"], "2000", "")
	mustEqual(t, "gross exposure", agg.Gross, "6000", "")
	if len(agg.ByInstrument) != 3 {
		t.Errorf("instrument map has %d entries, want 3", len(agg.ByInstrument))
	}
}

func TestTwoPositionsInOneInstrumentAreSummedNotOverwritten(t *testing.T) {
	// The map is keyed by instrument, so a second position in the same
	// instrument has to accumulate. Overwriting would report the smaller of
	// the two as the account's total exposure to it.
	agg := aggregate([]PositionView{
		valued(t, "XAUUSD", "USD", domain.SideBuy, "1500.00", "75.00", "0.00"),
		valued(t, "XAUUSD", "USD", domain.SideBuy, "2500.00", "125.00", "0.00"),
	}, zar)

	mustEqual(t, "per-instrument exposure", agg.ByInstrument["XAUUSD"], "4000", "")
}

func TestAnEmptyBookAggregatesToRealZerosInTheAccountCurrency(t *testing.T) {
	agg := aggregate(nil, zar)

	// money.Amount's zero value has an empty currency deliberately, so an
	// uninitialised total cannot join a calculation as "0 ZAR". Every total
	// here has to be a real zero in the account's currency instead.
	for name, a := range map[string]money.Amount{
		"unrealized": agg.Unrealized,
		"marginUsed": agg.MarginUsed,
		"gross":      agg.Gross,
		"net":        agg.Net,
	} {
		if a.Currency() != zar {
			t.Errorf("%s has currency %q, want ZAR; an empty currency panics on "+
				"the first MustAdd against a real amount", name, a.Currency())
		}
		if !a.IsZero() {
			t.Errorf("%s = %s, want 0", name, a.String())
		}
	}
	if agg.ByInstrument == nil || agg.ByCurrency == nil {
		t.Error("exposure maps are nil; a caller writing into them would panic")
	}
}

func TestUnrealizedPnLKeepsItsSign(t *testing.T) {
	// Losses must subtract. A loser summed as an absolute value would make a
	// drawdown look like a gain.
	agg := aggregate([]PositionView{
		valued(t, "XAUUSD", "USD", domain.SideBuy, "1000.00", "50.00", "40.00"),
		valued(t, "EURUSD", "USD", domain.SideBuy, "1000.00", "50.00", "-65.50"),
	}, zar)

	mustEqual(t, "unrealized", agg.Unrealized, "-25.5", "")
}
