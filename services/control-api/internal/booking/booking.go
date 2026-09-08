// Package booking is the single accounting path by which any execution
// becomes a fill, a position and a ledger entry.
//
// # Why this is its own package
//
// An execution can reach Vantage two ways: returned by a PlaceOrder call, or
// discovered later by reconciliation after a response was lost or a
// transaction rolled back. Those two paths must produce byte-identical
// accounting, and "must" is not a strong enough guarantee when the code exists
// twice.
//
// So it exists once, here, and both callers go through it. An architecture
// test asserts that this package is the only place that appends a fill, which
// makes the guarantee structural rather than aspirational. A reconciliation
// importer that quietly skipped the ledger write would be a way to create a
// position with no money movement behind it, and the position/ledger
// invariants would then be unprovable.
//
// # What this package does NOT decide
//
// Whether an execution SHOULD be booked. Attribution — does this execution
// belong to this order — is a question about evidence and is answered before
// anything gets here, by the reconciliation classifier or by the OMS holding
// the venue's direct response. This package validates that what it was handed
// is internally consistent, and refuses it otherwise.
package booking

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/broker"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/fx"
	"github.com/vantage/control-api/internal/metrics"
	"github.com/vantage/control-api/internal/money"
	"github.com/vantage/control-api/internal/store"
)

// Source records how an execution reached Vantage.
//
// Stored on the fill. The accounting is identical either way — that is the
// point — but "we were told at the time" and "we discovered this afterwards"
// are different facts about the same number, and the ledger should be able to
// answer which one applies.
type Source string

const (
	// SourceExecutionResponse: the venue returned it from PlaceOrder.
	SourceExecutionResponse Source = "execution_response"
	// SourceReconciliationImport: reconciliation found it and proved it
	// belonged to a known order.
	SourceReconciliationImport Source = "reconciliation_import"
	// SourceExecutionPoll: it arrived from PollExecutions during ordinary
	// operation rather than during a reconciliation run.
	SourceExecutionPoll Source = "execution_poll"
)

// Valid reports whether the source is one the schema accepts.
func (s Source) Valid() bool {
	switch s {
	case SourceExecutionResponse, SourceReconciliationImport, SourceExecutionPoll:
		return true
	}
	return false
}

// Service books executions.
type Service struct {
	store     *store.Store
	converter *fx.Converter
	mode      domain.ExecutionMode
}

// New builds the booking service.
//
// Refuses any mode but paper, for the same reason the OMS does: a booking
// service that would happily write live fills is one configuration mistake
// away from doing so.
func New(s *store.Store, converter *fx.Converter, mode domain.ExecutionMode) (*Service, error) {
	if mode != domain.ModePaper {
		return nil, fmt.Errorf(
			"booking: refusing to construct in %s mode: this build is paper-only", mode)
	}
	return &Service{store: s, converter: converter, mode: mode}, nil
}

// Request is one execution to book, with everything needed to validate it.
type Request struct {
	Account    domain.Account
	Instrument domain.Instrument
	Order      domain.Order
	Execution  broker.ExecutionReport
	Source     Source
	// IssueID links an imported fill to the reconciliation issue that
	// justified importing it. Required when Source is a reconciliation import:
	// a fill that appeared during recovery with nothing to point at is not
	// evidence of anything.
	IssueID *string
	Now     time.Time
}

// Result reports what booking did.
type Result struct {
	Fill domain.Fill
	// Applied is false when the execution was already recorded. This is the
	// normal answer to a venue replaying executions after a reconnect, and to
	// reconciliation re-examining a snapshot it has already processed. It is
	// not an error.
	Applied bool
	// Duplicate distinguishes "already booked" from "nothing to do". Callers
	// that need to record a DUPLICATE_EXECUTION_REPORT issue use it.
	Duplicate bool
}

// Validation failures. These are refusals, not internal errors: something
// asked to book an execution that does not add up, and the answer is no.
var (
	ErrNoBrokerExecutionID = errors.New("booking: execution carries no broker execution id")
	ErrInstrumentMismatch  = errors.New("booking: execution instrument does not match the order")
	ErrAccountMismatch     = errors.New("booking: order does not belong to the given account")
	ErrSideMismatch        = errors.New("booking: execution side does not match the order")
	ErrQuantityNotPositive = errors.New("booking: execution quantity is not positive")
	ErrPriceNotPositive    = errors.New("booking: execution price is not positive")
	ErrCommissionNegative  = errors.New("booking: execution commission is negative")
	ErrOverfill            = errors.New("booking: execution would fill more than the order quantity")
	ErrMissingIssue        = errors.New("booking: a reconciliation import must name the issue that justified it")
	ErrBadSource           = errors.New("booking: unrecognised ingest source")
)

// Validate checks that a request is internally consistent.
//
// # Why this exists separately from the database constraints
//
// The schema already refuses a non-positive quantity, a bad currency and a
// duplicate execution id. Those constraints are the last line and they stay.
// But a constraint violation arrives as an aborted transaction with a
// constraint name, which tells an operator nothing about which of several
// executions was wrong or why — and during a reconciliation run that aborts
// the repair of every other issue in the same transaction.
//
// Checking first turns "the transaction failed" into "this execution claims to
// be a buy against a sell order", which is the difference between a defect
// report and a mystery.
func (r Request) Validate() error {
	if !r.Source.Valid() {
		return fmt.Errorf("%w: %q", ErrBadSource, r.Source)
	}
	if r.Source == SourceReconciliationImport && (r.IssueID == nil || *r.IssueID == "") {
		return ErrMissingIssue
	}
	if r.Execution.BrokerFillID == "" {
		// Without this, deduplication is impossible: the unique index has
		// nothing to key on and every replay becomes a second position.
		return ErrNoBrokerExecutionID
	}
	if r.Order.AccountID != r.Account.ID {
		return fmt.Errorf("%w: order belongs to %s, booking against %s",
			ErrAccountMismatch, r.Order.AccountID, r.Account.ID)
	}
	if r.Order.InstrumentID != r.Instrument.ID {
		return fmt.Errorf("%w: order is on %s, instrument given is %s",
			ErrInstrumentMismatch, r.Order.InstrumentID, r.Instrument.ID)
	}
	if r.Execution.Side != r.Order.Side {
		// A venue reporting the opposite side for a known order id is a
		// mapping error, and booking it would move the position the wrong way.
		return fmt.Errorf("%w: order is %s, execution is %s",
			ErrSideMismatch, r.Order.Side, r.Execution.Side)
	}
	if !r.Execution.Quantity.IsPositive() {
		return fmt.Errorf("%w: %s", ErrQuantityNotPositive, r.Execution.Quantity)
	}
	if !r.Execution.Price.IsPositive() {
		return fmt.Errorf("%w: %s", ErrPriceNotPositive, r.Execution.Price)
	}
	if r.Execution.Commission.IsNegative() {
		return fmt.Errorf("%w: %s", ErrCommissionNegative, r.Execution.Commission)
	}
	return nil
}

// Apply books one execution inside the caller's transaction.
//
// The caller owns the transaction because booking is never the whole unit of
// work: the OMS books a fill alongside an order transition and an audit event,
// and reconciliation books one alongside an issue resolution. Splitting them
// into separate transactions would allow a fill with no order transition,
// which is a position nothing explains.
//
// The caller must already hold the account's row lock. See
// store.LockAccountTx: this function touches fills, positions and the ledger,
// and taking those locks in a different order from the OMS is what produced a
// deadlock that silently created phantom fills.
func (s *Service) Apply(ctx context.Context, tx pgx.Tx, req Request) (Result, error) {
	if err := req.Validate(); err != nil {
		return Result{}, err
	}

	// The overfill check needs the order's current filled quantity, which the
	// caller's copy may predate. Read it inside the transaction.
	current, err := s.store.Trading.OrderByIDTx(ctx, tx, req.Order.ID)
	if err != nil {
		return Result{}, fmt.Errorf("booking: reload order: %w", err)
	}
	projected := current.FilledQuantity.Add(req.Execution.Quantity)
	if projected.GreaterThan(current.Quantity) {
		// Refused rather than clamped. A venue reporting more filled than was
		// ordered means an execution has been attributed to the wrong order,
		// and booking part of it would leave the ledger describing a trade
		// nobody placed. This is a reconciliation issue for an operator, not
		// something to round away.
		//
		// Note this is checked BEFORE the duplicate check catches a replay,
		// because a replayed execution does not increase the total: the unique
		// index refuses the insert and the aggregate is recomputed from the
		// fills, so a genuine replay never reaches this branch with a real
		// overfill.
		alreadyBooked, err := s.store.Trading.FillExistsTx(ctx, tx,
			req.Account.BrokerName, req.Execution.BrokerFillID)
		if err != nil {
			return Result{}, fmt.Errorf("booking: check for a replayed execution: %w", err)
		}
		if !alreadyBooked {
			return Result{}, fmt.Errorf(
				"%w: order %s is for %s and already has %s filled; this execution adds %s",
				ErrOverfill, current.ID, current.Quantity, current.FilledQuantity,
				req.Execution.Quantity)
		}
	}

	fill := domain.Fill{
		OrderID:       current.ID,
		AccountID:     req.Account.ID,
		InstrumentID:  req.Instrument.ID,
		Side:          req.Execution.Side,
		Quantity:      req.Execution.Quantity,
		Price:         req.Execution.Price,
		Commission:    req.Execution.Commission,
		CommissionCcy: req.Execution.CommissionCcy,
		BrokerFillID:  req.Execution.BrokerFillID,
		BrokerName:    req.Account.BrokerName,
		Liquidity:     req.Execution.Liquidity,
		ExecutedAt:    req.Execution.ExecutedAt,
		IngestSource:  string(req.Source),
		IssueID:       req.IssueID,
	}

	stored, err := s.store.Trading.AppendFillTx(ctx, tx, fill, s.mode)
	if errors.Is(err, store.ErrDuplicateCommand) {
		// Already recorded. The unique index on (broker_name, broker_fill_id)
		// is what makes importing the same execution twice impossible, and
		// this is that defence firing.
		metrics.DuplicateExecutionsSuppressed.Inc()
		return Result{Applied: false, Duplicate: true}, nil
	}
	if err != nil {
		return Result{}, err
	}

	// Position: load, apply, persist.
	position, perr := s.store.Trading.OpenPositionTx(ctx, tx, req.Account.ID, req.Instrument.ID)
	if errors.Is(perr, store.ErrNotFound) {
		position = domain.Position{
			AccountID:    req.Account.ID,
			InstrumentID: req.Instrument.ID,
			Mode:         s.mode,
			Quantity:     decimal.Zero,
			Status:       domain.PositionClosed,
			RealizedPnL:  money.Zero(req.Instrument.QuoteCcy),
			Commission:   money.Zero(req.Instrument.QuoteCcy),
			Swap:         money.Zero(req.Instrument.QuoteCcy),
			StrategyID:   current.StrategyID,
		}
	} else if perr != nil {
		return Result{}, perr
	}

	applied := domain.ApplyFill(position, req.Instrument, stored)
	updated := applied.Position
	updated.Mode = s.mode
	updated.RealizedPnL = position.RealizedPnL.MustAdd(applied.RealizedQuote)
	updated.Commission = position.Commission.MustAdd(
		money.New(stored.Commission, req.Instrument.QuoteCcy))
	if updated.StrategyID == nil {
		updated.StrategyID = current.StrategyID
	}

	savedPosition, err := s.store.Trading.UpsertPositionTx(ctx, tx, updated)
	if err != nil {
		return Result{}, err
	}

	// Ledger: realised P&L and commission, converted into the account's
	// currency. A conversion failure aborts the whole transaction rather than
	// booking a number whose currency nobody can name.
	balance, err := s.store.Accounts.BalanceTx(ctx, tx, req.Account.ID, req.Account.Currency)
	if err != nil {
		return Result{}, err
	}

	description := func(what string) string {
		if req.Source == SourceReconciliationImport {
			// The ledger line says so. An operator reading a statement should
			// not have to join to another table to discover that an entry was
			// reconstructed after a divergence.
			return fmt.Sprintf("%s on %s (recovered by reconciliation)",
				what, req.Instrument.Symbol)
		}
		return fmt.Sprintf("%s on %s", what, req.Instrument.Symbol)
	}

	if !applied.RealizedQuote.IsZero() {
		conv, cerr := s.converter.Convert(ctx, applied.RealizedQuote, req.Account.Currency)
		if cerr != nil {
			return Result{}, fmt.Errorf(
				"booking: cannot book realised P&L: no %s/%s rate: %w",
				applied.RealizedQuote.Currency(), req.Account.Currency, cerr)
		}
		amount := conv.To.RoundLedger()
		balance = balance.MustAdd(amount)
		if _, err := s.store.Accounts.AppendTransactionTx(ctx, tx, domain.Transaction{
			AccountID: req.Account.ID, Type: domain.TxRealizedPnL, Amount: amount,
			BalanceAfter: balance.RoundLedger(), OrderID: &current.ID, FillID: &stored.ID,
			PositionID: &savedPosition.ID, Mode: s.mode,
			Description: description("Realised P&L"),
		}); err != nil {
			return Result{}, err
		}
	}

	if stored.Commission.IsPositive() {
		commissionQuote := money.New(stored.Commission.Neg(), money.Currency(stored.CommissionCcy))
		conv, cerr := s.converter.Convert(ctx, commissionQuote, req.Account.Currency)
		if cerr != nil {
			return Result{}, fmt.Errorf("booking: cannot book commission: %w", cerr)
		}
		amount := conv.To.RoundLedger()
		balance = balance.MustAdd(amount)
		if _, err := s.store.Accounts.AppendTransactionTx(ctx, tx, domain.Transaction{
			AccountID: req.Account.ID, Type: domain.TxCommission, Amount: amount,
			BalanceAfter: balance.RoundLedger(), OrderID: &current.ID, FillID: &stored.ID,
			Mode: s.mode, Description: description("Commission"),
		}); err != nil {
			return Result{}, err
		}
	}

	event := "order.filled"
	if req.Source == SourceReconciliationImport {
		event = "order.filled.recovered"
	}
	if err := s.store.Trading.EnqueueOutboxTx(ctx, tx, "fill", stored.ID.String(), event,
		map[string]any{
			"order_id": current.ID, "account_id": req.Account.ID,
			"symbol": req.Instrument.Symbol, "quantity": stored.Quantity.String(),
			"price": stored.Price.String(), "ingest_source": string(req.Source),
		}, ""); err != nil {
		return Result{}, err
	}

	metrics.FillsRecorded.WithLabelValues(req.Instrument.Symbol, string(stored.Side)).Inc()
	if req.Source == SourceReconciliationImport {
		metrics.RecoveredFills.WithLabelValues(req.Instrument.Symbol).Inc()
	}
	return Result{Fill: stored, Applied: true}, nil
}
