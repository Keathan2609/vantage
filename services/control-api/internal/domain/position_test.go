package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/money"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// goldInstrument mirrors the seeded XAUUSD specification: 100 ounces per lot,
// quoted in USD.
func goldInstrument() Instrument {
	return Instrument{
		ID:       "XAUUSD",
		Symbol:   "XAUUSD",
		Class:    AssetClassMetal,
		BaseCcy:  money.XAU,
		QuoteCcy: money.USD,
		Enabled:  true,
		Spec: InstrumentSpec{
			ContractSize:        dec("100"),
			PricePrecision:      2,
			TickSize:            dec("0.01"),
			QuantityPrecision:   2,
			MinQuantity:         dec("0.01"),
			MaxQuantity:         dec("50"),
			QuantityStep:        dec("0.01"),
			MarginRate:          dec("0.005"),
			MaxLeverage:         dec("200"),
			SupportedOrderTypes: OrderTypeSet{OrderTypeMarket, OrderTypeLimit, OrderTypeStop},
			CommissionPerLot:    dec("0"),
		},
	}
}

func fill(side OrderSide, qty, price string) Fill {
	return Fill{
		ID:         uuid.New(),
		Side:       side,
		Quantity:   dec(qty),
		Price:      dec(price),
		ExecutedAt: time.Now().UTC(),
	}
}

func flatPosition() Position {
	return Position{
		ID:            uuid.New(),
		InstrumentID:  "XAUUSD",
		Quantity:      decimal.Zero,
		AvgEntryPrice: decimal.Zero,
		Status:        PositionClosed,
	}
}

func TestApplyFill_OpenFromFlat(t *testing.T) {
	inst := goldInstrument()
	res := ApplyFill(flatPosition(), inst, fill(SideBuy, "0.10", "2000.00"))

	if res.Position.Side != SideBuy || !res.Position.Quantity.Equal(dec("0.10")) {
		t.Fatalf("unexpected position: %+v", res.Position)
	}
	if !res.Position.AvgEntryPrice.Equal(dec("2000.00")) {
		t.Errorf("entry price = %s, want 2000.00", res.Position.AvgEntryPrice)
	}
	if !res.RealizedQuote.IsZero() {
		t.Errorf("opening a position realises nothing, got %s", res.RealizedQuote)
	}
}

func TestApplyFill_IncreaseAveragesEntry(t *testing.T) {
	inst := goldInstrument()
	pos := flatPosition()
	pos = ApplyFill(pos, inst, fill(SideBuy, "0.10", "2000.00")).Position
	res := ApplyFill(pos, inst, fill(SideBuy, "0.10", "2100.00"))

	if !res.Position.Quantity.Equal(dec("0.20")) {
		t.Errorf("quantity = %s, want 0.20", res.Position.Quantity)
	}
	if !res.Position.AvgEntryPrice.Equal(dec("2050")) {
		t.Errorf("avg entry = %s, want 2050", res.Position.AvgEntryPrice)
	}
	if !res.RealizedQuote.IsZero() {
		t.Errorf("increasing realises nothing, got %s", res.RealizedQuote)
	}
}

func TestApplyFill_PartialReductionRealisesProportionally(t *testing.T) {
	inst := goldInstrument()
	pos := ApplyFill(flatPosition(), inst, fill(SideBuy, "0.20", "2000.00")).Position

	res := ApplyFill(pos, inst, fill(SideSell, "0.05", "2010.00"))

	// 0.05 lots * 100 oz * $10 = $50 realised.
	if got := res.RealizedQuote.Decimal(); !got.Equal(dec("50")) {
		t.Errorf("realised = %s, want 50", got)
	}
	if !res.Position.Quantity.Equal(dec("0.15")) {
		t.Errorf("remaining = %s, want 0.15", res.Position.Quantity)
	}
	// Reducing must not disturb the entry price of what remains.
	if !res.Position.AvgEntryPrice.Equal(dec("2000.00")) {
		t.Errorf("entry price moved to %s on a reduction", res.Position.AvgEntryPrice)
	}
	if res.Closed || res.Reversed {
		t.Error("partial reduction should neither close nor reverse")
	}
}

func TestApplyFill_ExactCloseRealisesAndFlattens(t *testing.T) {
	inst := goldInstrument()
	pos := ApplyFill(flatPosition(), inst, fill(SideSell, "0.10", "2000.00")).Position

	// Short closed 10 lower: profit for a short.
	res := ApplyFill(pos, inst, fill(SideBuy, "0.10", "1990.00"))

	if got := res.RealizedQuote.Decimal(); !got.Equal(dec("100")) {
		t.Errorf("realised = %s, want 100", got)
	}
	if !res.Closed || res.Position.Status != PositionClosed {
		t.Error("expected the position to be closed")
	}
	if !res.Position.Quantity.IsZero() {
		t.Errorf("closed position should hold zero quantity, got %s", res.Position.Quantity)
	}
}

func TestApplyFill_FlipThroughZero(t *testing.T) {
	inst := goldInstrument()
	pos := ApplyFill(flatPosition(), inst, fill(SideBuy, "0.10", "2000.00")).Position

	// Sell 0.25: closes 0.10 long at +5, then opens 0.15 short at 2005.
	res := ApplyFill(pos, inst, fill(SideSell, "0.25", "2005.00"))

	if got := res.RealizedQuote.Decimal(); !got.Equal(dec("50")) {
		t.Errorf("realised = %s, want 50 (only the closed portion)", got)
	}
	if !res.Reversed {
		t.Error("expected the fill to be reported as a reversal")
	}
	if res.Position.Side != SideSell || !res.Position.Quantity.Equal(dec("0.15")) {
		t.Fatalf("expected 0.15 short, got %s %s", res.Position.Side, res.Position.Quantity)
	}
	// The new leg's entry price is the fill price, not a blend of the old one.
	if !res.Position.AvgEntryPrice.Equal(dec("2005.00")) {
		t.Errorf("new entry = %s, want 2005.00", res.Position.AvgEntryPrice)
	}
}

func TestUnrealizedPnL_MarksAtClosingSideNotMid(t *testing.T) {
	inst := goldInstrument()
	pos := ApplyFill(flatPosition(), inst, fill(SideBuy, "0.10", "2000.00")).Position
	q := Quote{Bid: dec("2009.00"), Ask: dec("2010.00")}

	// A long is valued at the bid it would have to hit: 9.00, not the 9.50 mid.
	got := pos.UnrealizedPnLQuote(inst, q).Decimal()
	if !got.Equal(dec("90")) {
		t.Errorf("unrealised = %s, want 90 (bid-marked)", got)
	}

	short := ApplyFill(flatPosition(), inst, fill(SideSell, "0.10", "2000.00")).Position
	// A short is valued at the ask: 2000 - 2010 = -10.00 per ounce.
	if got := short.UnrealizedPnLQuote(inst, q).Decimal(); !got.Equal(dec("-100")) {
		t.Errorf("short unrealised = %s, want -100 (ask-marked)", got)
	}
}

func TestUnrealizedPnL_QuoteCurrencyIsCarried(t *testing.T) {
	inst := goldInstrument()
	pos := ApplyFill(flatPosition(), inst, fill(SideBuy, "0.10", "2000.00")).Position
	got := pos.UnrealizedPnLQuote(inst, Quote{Bid: dec("2001"), Ask: dec("2002")})
	if got.Currency() != money.USD {
		t.Errorf("P&L currency = %s, want USD (the instrument's quote currency)", got.Currency())
	}
}

func TestNormaliseQuantity_FloorsNeverRoundsUp(t *testing.T) {
	spec := goldInstrument().Spec
	// 0.0179 must floor to 0.01, never round to 0.02: rounding up would exceed
	// the size that risk checks approved.
	if got := spec.NormaliseQuantity(dec("0.0179")); !got.Equal(dec("0.01")) {
		t.Errorf("NormaliseQuantity(0.0179) = %s, want 0.01", got)
	}
	if got := spec.NormaliseQuantity(dec("0.199")); !got.Equal(dec("0.19")) {
		t.Errorf("NormaliseQuantity(0.199) = %s, want 0.19", got)
	}
	// Below the minimum, normalisation yields zero, which validation rejects:
	// the correct outcome for a too-small account is NO TRADE.
	small := spec.NormaliseQuantity(dec("0.004"))
	if !small.IsZero() {
		t.Errorf("NormaliseQuantity(0.004) = %s, want 0", small)
	}
	if err := spec.ValidateQuantity(small); err == nil {
		t.Error("expected sub-minimum quantity to be rejected")
	}
}

func TestValidateQuantity_StepAndBounds(t *testing.T) {
	spec := goldInstrument().Spec
	if err := spec.ValidateQuantity(dec("0.10")); err != nil {
		t.Errorf("0.10 should be valid: %v", err)
	}
	if err := spec.ValidateQuantity(dec("0.005")); err == nil {
		t.Error("0.005 is below the minimum and must be rejected")
	}
	if err := spec.ValidateQuantity(dec("0.015")); err == nil {
		t.Error("0.015 violates the 0.01 step and must be rejected")
	}
	if err := spec.ValidateQuantity(dec("60")); err == nil {
		t.Error("60 exceeds the maximum and must be rejected")
	}
	if err := spec.ValidateQuantity(dec("-1")); err == nil {
		t.Error("negative quantity must be rejected")
	}
}

func TestMarginAndNotional(t *testing.T) {
	inst := goldInstrument()
	// 0.10 lots * 100 oz * 2000 = $20,000 notional.
	n := inst.Notional(dec("0.10"), dec("2000"))
	if !n.Decimal().Equal(dec("20000")) || n.Currency() != money.USD {
		t.Errorf("notional = %s, want 20000 USD", n)
	}
	// At 0.5% margin that is $100.
	m := inst.MarginRequired(dec("0.10"), dec("2000"))
	if !m.Decimal().Equal(dec("100")) {
		t.Errorf("margin = %s, want 100 USD", m)
	}
}
