package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/money"
)

// PositionStatus is the lifecycle state of a position.
type PositionStatus string

const (
	PositionOpen   PositionStatus = "open"
	PositionClosed PositionStatus = "closed"
)

// Position is net exposure in one instrument on one account.
//
// Vantage uses netting rather than hedging: one open position per instrument
// per account. Hedged (two-sided) positions are a broker-specific mode; a
// future adapter that requires them will map them explicitly rather than the
// core silently supporting both and disagreeing with the broker about net size.
type Position struct {
	ID            uuid.UUID
	AccountID     uuid.UUID
	InstrumentID  string
	Symbol        string
	Mode          ExecutionMode
	Side          OrderSide
	Quantity      decimal.Decimal
	AvgEntryPrice decimal.Decimal
	Status        PositionStatus
	StopLoss      *decimal.Decimal
	TakeProfit    *decimal.Decimal
	RealizedPnL   money.Amount
	Commission    money.Amount
	Swap          money.Amount
	StrategyID    *uuid.UUID
	OpenedAt      time.Time
	ClosedAt      *time.Time
	UpdatedAt     time.Time
	Version       int64
}

// SignedQuantity returns quantity with direction applied.
func (p Position) SignedQuantity() decimal.Decimal {
	return p.Quantity.Mul(p.Side.SignedMultiplier())
}

// UnrealizedPnLQuote returns open P&L in the instrument's QUOTE currency.
// Conversion into the account currency is an explicit downstream step.
//
// The position is valued at the price at which it could be closed: a long is
// marked at the bid it would have to hit, not the mid or the ask. Marking at
// the mid overstates equity by half a spread on every open position.
func (p Position) UnrealizedPnLQuote(inst Instrument, q Quote) money.Amount {
	if p.Quantity.IsZero() {
		return money.Zero(inst.QuoteCcy)
	}
	closePrice := q.ExecutionPrice(p.Side.Opposite())
	diff := closePrice.Sub(p.AvgEntryPrice)
	if p.Side == SideSell {
		diff = diff.Neg()
	}
	return money.New(diff.Mul(p.Quantity).Mul(inst.Spec.ContractSize), inst.QuoteCcy)
}

// NotionalQuote returns the position's notional value in the quote currency,
// marked at the current mid.
func (p Position) NotionalQuote(inst Instrument, q Quote) money.Amount {
	return inst.Notional(p.Quantity, q.Mid())
}

// ApplyFillResult describes how a fill changed a position.
type ApplyFillResult struct {
	Position      Position
	RealizedQuote money.Amount
	Closed        bool
	Reversed      bool
}

// ApplyFill folds a fill into a position and returns the new state plus any
// realised P&L, expressed in the instrument's quote currency.
//
// Three cases: increasing exposure (weighted-average entry price), reducing
// exposure (realise P&L on the closed portion, entry price unchanged), and
// flipping through zero (realise the whole old position, open the remainder at
// the fill price). Getting the flip case wrong is a classic source of phantom
// P&L, so it is handled explicitly rather than falling out of arithmetic.
func ApplyFill(pos Position, inst Instrument, f Fill) ApplyFillResult {
	quoteCcy := inst.QuoteCcy
	zero := money.Zero(quoteCcy)

	// Opening from flat.
	if pos.Quantity.IsZero() {
		pos.Side = f.Side
		pos.Quantity = f.Quantity
		pos.AvgEntryPrice = f.Price
		pos.Status = PositionOpen
		return ApplyFillResult{Position: pos, RealizedQuote: zero}
	}

	// Same direction: increase and re-average.
	if pos.Side == f.Side {
		totalQty := pos.Quantity.Add(f.Quantity)
		notional := pos.AvgEntryPrice.Mul(pos.Quantity).Add(f.Price.Mul(f.Quantity))
		pos.AvgEntryPrice = notional.Div(totalQty)
		pos.Quantity = totalQty
		pos.Status = PositionOpen
		return ApplyFillResult{Position: pos, RealizedQuote: zero}
	}

	// Opposite direction: reduce, close, or flip.
	closing := decimal.Min(pos.Quantity, f.Quantity)
	diff := f.Price.Sub(pos.AvgEntryPrice)
	if pos.Side == SideSell {
		diff = diff.Neg()
	}
	realized := money.New(diff.Mul(closing).Mul(inst.Spec.ContractSize), quoteCcy)

	remainingOld := pos.Quantity.Sub(closing)
	remainingNew := f.Quantity.Sub(closing)

	switch {
	case remainingOld.IsPositive():
		// Partial reduction; entry price is unchanged.
		pos.Quantity = remainingOld
		return ApplyFillResult{Position: pos, RealizedQuote: realized}

	case remainingNew.IsPositive():
		// Flipped through flat into the opposite direction.
		pos.Side = f.Side
		pos.Quantity = remainingNew
		pos.AvgEntryPrice = f.Price
		pos.Status = PositionOpen
		return ApplyFillResult{Position: pos, RealizedQuote: realized, Reversed: true}

	default:
		// Exactly flat.
		pos.Quantity = decimal.Zero
		pos.Status = PositionClosed
		return ApplyFillResult{Position: pos, RealizedQuote: realized, Closed: true}
	}
}
