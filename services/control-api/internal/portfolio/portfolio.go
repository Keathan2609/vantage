// Package portfolio derives account state from the ledger and open positions.
//
// Nothing here stores a running balance. Equity, margin and exposure are
// computed from immutable facts -- the ledger's transactions and the position
// book -- every time they are asked for. That makes them reproducible and
// auditable: a balance that disagrees with the ledger is a bug that shows up
// immediately, not a slow drift nobody notices until a withdrawal.
package portfolio

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/fx"
	"github.com/vantage/control-api/internal/money"
	"github.com/vantage/control-api/internal/store"
)

// Service computes portfolio state.
type Service struct {
	store     *store.Store
	converter *fx.Converter
	clock     domain.Clock
}

// New builds the portfolio service.
func New(s *store.Store, converter *fx.Converter, clock domain.Clock) *Service {
	return &Service{store: s, converter: converter, clock: clock}
}

// PositionView is an open position valued in the account's currency.
type PositionView struct {
	Position      domain.Position
	Instrument    domain.Instrument
	CurrentPrice  decimal.Decimal
	QuoteAge      time.Duration
	UnrealizedPnL money.Amount // account currency
	NotionalValue money.Amount // account currency
	MarginUsed    money.Amount // account currency
	// Valued is false when no fresh price or FX rate was available. Such a
	// position is shown to the user as unvalued rather than as zero, because
	// zero would understate exposure at exactly the wrong moment.
	Valued        bool
	ValuationNote string
}

// Snapshot is an account's full financial state at an instant.
type Snapshot struct {
	Account   domain.Account
	State     domain.AccountSnapshot
	Positions []PositionView
	// ExposureByInstrument and ExposureByCurrency support the concentration
	// and correlated-exposure checks.
	ExposureByInstrument map[string]money.Amount
	ExposureByCurrency   map[money.Currency]money.Amount
	// UnvaluedPositions counts positions that could not be priced. Any value
	// above zero means the snapshot understates risk.
	UnvaluedPositions int
}

// Compute builds a full snapshot for an account.
func (s *Service) Compute(ctx context.Context, account domain.Account) (Snapshot, error) {
	now := s.clock.Now()
	ccy := account.Currency

	balance, err := s.store.Accounts.Balance(ctx, account.ID, ccy)
	if err != nil {
		return Snapshot{}, fmt.Errorf("portfolio: balance: %w", err)
	}
	totals, err := s.store.Accounts.Totals(ctx, account.ID, ccy)
	if err != nil {
		return Snapshot{}, fmt.Errorf("portfolio: totals: %w", err)
	}
	positions, err := s.store.Trading.OpenPositions(ctx, account.ID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("portfolio: open positions: %w", err)
	}
	pendingOrders, err := s.store.Trading.CountOpenOrders(ctx, account.ID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("portfolio: open orders: %w", err)
	}
	equityState, err := s.store.Accounts.EquityState(ctx, account.ID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("portfolio: equity state: %w", err)
	}

	snap := Snapshot{Account: account}

	// Price every position first, then aggregate. Marking to market needs the
	// store; the arithmetic over the results does not, and keeping the two
	// apart is what makes the exposure invariants testable without a database
	// (see aggregate.go).
	for _, p := range positions {
		view, err := s.valuePosition(ctx, p, ccy, now)
		if err != nil {
			return Snapshot{}, err
		}
		snap.Positions = append(snap.Positions, view)
	}

	agg := aggregate(snap.Positions, ccy)
	snap.ExposureByInstrument = agg.ByInstrument
	snap.ExposureByCurrency = agg.ByCurrency
	snap.UnvaluedPositions = agg.Unvalued

	equity := balance.MustAdd(agg.Unrealized)
	free, err := equity.Sub(agg.MarginUsed)
	if err != nil {
		return Snapshot{}, err
	}

	snap.State = domain.AccountSnapshot{
		AccountID:      account.ID,
		Currency:       ccy,
		Balance:        balance.RoundLedger(),
		Equity:         equity.RoundLedger(),
		MarginUsed:     agg.MarginUsed.RoundLedger(),
		FreeMargin:     free.RoundLedger(),
		RealizedPnL:    totals.RealizedPnL.RoundLedger(),
		UnrealizedPnL:  agg.Unrealized.RoundLedger(),
		Fees:           totals.Fees.RoundLedger(),
		Commission:     totals.Commission.RoundLedger(),
		Swap:           totals.Swap.RoundLedger(),
		GrossExposure:  agg.Gross.RoundLedger(),
		NetExposure:    agg.Net.RoundLedger(),
		OpenPositions:  len(positions),
		PendingOrders:  pendingOrders,
		PeakEquity:     equityState.PeakEquity,
		DayStartEquity: equityState.DayStartEquity,
		AsOf:           now,
		// Always true in this build. It travels with the snapshot so no
		// serialiser can omit it and let paper numbers read as real ones.
		Simulated: account.Mode != domain.ModeLive,
	}
	return snap, nil
}

// valuePosition marks one position to market in the account's currency.
func (s *Service) valuePosition(ctx context.Context, p domain.Position, accountCcy money.Currency, now time.Time) (PositionView, error) {
	view := PositionView{Position: p}

	inst, err := s.store.Market.Instrument(ctx, p.InstrumentID)
	if err != nil {
		view.ValuationNote = "instrument specification unavailable"
		return view, nil
	}
	view.Instrument = inst

	quote, _, err := s.store.Market.LatestQuote(ctx, p.InstrumentID)
	if err != nil {
		view.ValuationNote = "no price available for this instrument"
		return view, nil
	}
	view.CurrentPrice = quote.Mid()
	view.QuoteAge = now.Sub(quote.IngestedAt)

	// Value in the instrument's quote currency first, then convert once.
	unrealQuote := p.UnrealizedPnLQuote(inst, quote)
	notionalQuote := p.NotionalQuote(inst, quote)
	marginQuote := inst.MarginRequired(p.Quantity, quote.Mid())

	unreal, err := s.converter.Convert(ctx, unrealQuote, accountCcy)
	if err != nil {
		view.ValuationNote = "no " + string(inst.QuoteCcy) + "/" + string(accountCcy) + " rate available"
		return view, nil
	}
	notional, err := s.converter.Convert(ctx, notionalQuote, accountCcy)
	if err != nil {
		view.ValuationNote = "no " + string(inst.QuoteCcy) + "/" + string(accountCcy) + " rate available"
		return view, nil
	}
	margin, err := s.converter.Convert(ctx, marginQuote, accountCcy)
	if err != nil {
		view.ValuationNote = "no " + string(inst.QuoteCcy) + "/" + string(accountCcy) + " rate available"
		return view, nil
	}

	view.UnrealizedPnL = unreal.To.RoundLedger()
	view.NotionalValue = notional.To.RoundLedger()
	view.MarginUsed = margin.To.RoundLedger()
	view.Valued = true
	return view, nil
}

// RecordEquityPoint persists a snapshot's equity and advances the peak.
//
// Called after every state change that can move equity. Peak equity only ever
// rises, so the drawdown limit cannot be loosened by a losing streak.
func (s *Service) RecordEquityPoint(ctx context.Context, snap Snapshot) error {
	return s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		if err := s.store.Accounts.UpdatePeakEquityTx(ctx, tx, snap.Account.ID, snap.State.Equity); err != nil {
			return err
		}
		return s.store.Accounts.RecordEquityPoint(ctx, tx, snap.Account.ID,
			snap.State.Equity, snap.State.Balance, snap.State.UnrealizedPnL,
			snap.State.MarginUsed, snap.State.AsOf)
	})
}

// RollTradingDayIfNeeded resets the daily-loss reference point when the
// account's trading day has turned over.
//
// The boundary is the venue's rollover, not local midnight: a "daily" loss
// limit that resets at a different time from the venue's own day would let a
// strategy take two days' worth of losses inside one trading session.
func (s *Service) RollTradingDayIfNeeded(ctx context.Context, account domain.Account, dayBoundary time.Time) (bool, error) {
	st, err := s.store.Accounts.EquityState(ctx, account.ID)
	if err != nil {
		return false, err
	}
	if !st.DayStartAt.Before(dayBoundary) {
		return false, nil
	}
	snap, err := s.Compute(ctx, account)
	if err != nil {
		return false, err
	}
	err = s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		return s.store.Accounts.RollTradingDayTx(ctx, tx, account.ID, snap.State.Equity, dayBoundary)
	})
	return err == nil, err
}

// Attribution groups realised P&L by any recorded dimension, from the ledger.
//
// Two reads, deliberately. The entries are folded into buckets; the account's
// own entry count and net movement come from a SEPARATE query, and the report
// compares them. A report that folded a truncated set and reported the fold's
// own sum as the account's total would be internally consistent and wrong,
// which is the worst kind of financial number.
//
// See domain.Attribute for why this folds the ledger rather than positions.
func (s *Service) Attribution(ctx context.Context, accountID uuid.UUID,
	ccy money.Currency, dimension domain.AttributionDimension,
	limit int) (domain.AttributionReport, error) {

	entries, err := s.store.Trading.LedgerEntriesForAttribution(ctx, accountID, limit)
	if err != nil {
		return domain.AttributionReport{}, err
	}
	count, net, err := s.store.Trading.LedgerTotals(ctx, accountID, ccy)
	if err != nil {
		return domain.AttributionReport{}, err
	}
	return domain.Attribute(dimension, ccy, entries, count, net), nil
}
