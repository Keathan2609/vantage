package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/db"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/money"
)

// TradingStore persists orders, fills, positions and command idempotency.
type TradingStore struct{ pool *db.Pool }

// ---------------------------------------------------------------------------
// Command idempotency
// ---------------------------------------------------------------------------

// CommandStatus is the lifecycle of a registered financial command.
type CommandStatus string

const (
	CommandInProgress CommandStatus = "in_progress"
	CommandSucceeded  CommandStatus = "succeeded"
	CommandFailed     CommandStatus = "failed"
	CommandRejected   CommandStatus = "rejected"
)

// RegisteredCommand is a previously seen command.
type RegisteredCommand struct {
	AccountID      uuid.UUID
	IdempotencyKey string
	CommandType    string
	RequestHash    string
	CommandID      uuid.UUID
	ActorUserID    uuid.UUID
	Status         CommandStatus
	ResultOrderID  *uuid.UUID
	ResultPayload  json.RawMessage
	CreatedAt      time.Time
	CompletedAt    *time.Time
}

// Errors distinguishing the two duplicate cases.
var (
	// ErrDuplicateCommand means this exact command was already registered. The
	// caller should return the stored result rather than executing again.
	ErrDuplicateCommand = errors.New("store: duplicate command")
	// ErrIdempotencyConflict means the key was reused with a DIFFERENT payload.
	// This is refused outright: guessing which payload the client meant could
	// place an order nobody asked for.
	ErrIdempotencyConflict = errors.New("store: idempotency key reused with a different payload")
	// ErrCommandInFlight means an identical command is still executing.
	ErrCommandInFlight = errors.New("store: an identical command is still in progress")
)

// RegisterCommand claims an idempotency key before any side effect occurs.
//
// This is the single choke point that makes duplicate submission impossible.
// Two concurrent requests carrying the same key contend on the same primary
// key: exactly one INSERT succeeds and proceeds, and the loser is told the
// command is already in flight rather than executing a second order.
func (s *TradingStore) RegisterCommand(ctx context.Context, tx pgx.Tx, cmd RegisteredCommand) (*RegisteredCommand, error) {
	// ON CONFLICT DO NOTHING rather than letting the unique violation raise.
	//
	// This is not a style preference. In PostgreSQL a statement error inside a
	// transaction aborts the entire transaction: every subsequent command fails
	// with 25P02 until rollback. Detecting a duplicate by catching the
	// violation would therefore poison the transaction that then needs to read
	// the existing row and return its stored result. Conflict-tolerant insert
	// keeps the transaction healthy and makes the duplicate path work.
	tag, err := tx.Exec(ctx, `
		INSERT INTO command_idempotency
			(account_id, idempotency_key, command_type, request_hash, command_id, actor_user_id, status)
		VALUES ($1,$2,$3,$4,$5,$6,'in_progress')
		ON CONFLICT (account_id, idempotency_key) DO NOTHING`,
		cmd.AccountID, cmd.IdempotencyKey, cmd.CommandType, cmd.RequestHash,
		cmd.CommandID, cmd.ActorUserID)
	if err != nil {
		return nil, mapError(err)
	}
	if tag.RowsAffected() == 1 {
		return nil, nil // freshly claimed; the caller proceeds
	}

	// The key already exists. A concurrent claim in another transaction would
	// have blocked this INSERT on the conflicting key until it committed, so by
	// the time control reaches here the existing row is visible.
	existing, loadErr := s.commandByKeyTx(ctx, tx, cmd.AccountID, cmd.IdempotencyKey)
	if loadErr != nil {
		return nil, loadErr
	}
	if existing.RequestHash != cmd.RequestHash {
		return existing, ErrIdempotencyConflict
	}
	if existing.Status == CommandInProgress {
		return existing, ErrCommandInFlight
	}
	return existing, ErrDuplicateCommand
}

func (s *TradingStore) commandByKeyTx(ctx context.Context, tx pgx.Tx, accountID uuid.UUID, key string) (*RegisteredCommand, error) {
	var c RegisteredCommand
	err := tx.QueryRow(ctx, `
		SELECT account_id, idempotency_key, command_type, request_hash, command_id,
		       status, result_order_id, result_payload, created_at, completed_at
		FROM command_idempotency WHERE account_id = $1 AND idempotency_key = $2`,
		accountID, key).
		Scan(&c.AccountID, &c.IdempotencyKey, &c.CommandType, &c.RequestHash, &c.CommandID,
			&c.Status, &c.ResultOrderID, &c.ResultPayload, &c.CreatedAt, &c.CompletedAt)
	if err != nil {
		return nil, mapError(err)
	}
	return &c, nil
}

// CommandByKey loads a registered command outside a transaction.
func (s *TradingStore) CommandByKey(ctx context.Context, accountID uuid.UUID, key string) (*RegisteredCommand, error) {
	var c RegisteredCommand
	err := s.pool.QueryRow(ctx, `
		SELECT account_id, idempotency_key, command_type, request_hash, command_id,
		       status, result_order_id, result_payload, created_at, completed_at
		FROM command_idempotency WHERE account_id = $1 AND idempotency_key = $2`,
		accountID, key).
		Scan(&c.AccountID, &c.IdempotencyKey, &c.CommandType, &c.RequestHash, &c.CommandID,
			&c.Status, &c.ResultOrderID, &c.ResultPayload, &c.CreatedAt, &c.CompletedAt)
	if err != nil {
		return nil, mapError(err)
	}
	return &c, nil
}

// CompleteCommand records a command's outcome so a retry returns the same
// answer instead of re-executing.
func (s *TradingStore) CompleteCommand(ctx context.Context, tx pgx.Tx, accountID uuid.UUID, key string,
	status CommandStatus, orderID *uuid.UUID, payload any) error {

	var raw []byte
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("store: marshal command result: %w", err)
		}
		raw = b
	}
	_, err := tx.Exec(ctx, `
		UPDATE command_idempotency
		SET status = $3, result_order_id = $4, result_payload = $5, completed_at = now()
		WHERE account_id = $1 AND idempotency_key = $2`,
		accountID, key, status, orderID, raw)
	return mapError(err)
}

// ---------------------------------------------------------------------------
// Orders
// ---------------------------------------------------------------------------

const orderColumns = `o.id, o.account_id, o.user_id, o.instrument_id, i.symbol, o.mode, o.side, o.type,
	o.time_in_force, o.status, o.quantity, o.filled_quantity, o.avg_fill_price, o.limit_price,
	o.stop_price, o.stop_loss, o.take_profit, o.source, o.strategy_id, o.strategy_version,
	o.decision_id, o.command_id, o.idempotency_key, o.broker_name, o.broker_order_id,
	o.reject_code, o.reject_reason, o.version, o.created_at, o.updated_at, o.submitted_at, o.closed_at`

func scanOrder(row pgx.Row) (domain.Order, error) {
	var o domain.Order
	err := row.Scan(&o.ID, &o.AccountID, &o.UserID, &o.InstrumentID, &o.Symbol, &o.Mode, &o.Side, &o.Type,
		&o.TimeInForce, &o.Status, &o.Quantity, &o.FilledQuantity, &o.AvgFillPrice, &o.LimitPrice,
		&o.StopPrice, &o.StopLoss, &o.TakeProfit, &o.Source, &o.StrategyID, &o.StrategyVersion,
		&o.DecisionID, &o.CommandID, &o.IdempotencyKey, &o.BrokerName, &o.BrokerOrderID,
		&o.RejectCode, &o.RejectReason, &o.Version, &o.CreatedAt, &o.UpdatedAt, &o.SubmittedAt, &o.ClosedAt)
	if err != nil {
		return domain.Order{}, mapError(err)
	}
	return o, nil
}

// CreateOrderTx inserts an order in its initial state.
func (s *TradingStore) CreateOrderTx(ctx context.Context, tx pgx.Tx, o domain.Order) (domain.Order, error) {
	if err := AssertPaperMode(o.Mode); err != nil {
		return domain.Order{}, err
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO orders (account_id, user_id, instrument_id, mode, side, type, time_in_force,
			status, quantity, filled_quantity, avg_fill_price, limit_price, stop_price,
			stop_loss, take_profit, source, strategy_id, strategy_version, decision_id,
			command_id, idempotency_key, broker_name)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,0,0,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		RETURNING id`,
		o.AccountID, o.UserID, o.InstrumentID, o.Mode, o.Side, o.Type, o.TimeInForce,
		o.Status, o.Quantity, o.LimitPrice, o.StopPrice, o.StopLoss, o.TakeProfit,
		o.Source, o.StrategyID, o.StrategyVersion, o.DecisionID, o.CommandID,
		o.IdempotencyKey, o.BrokerName).Scan(&id)
	if err != nil {
		return domain.Order{}, mapError(err)
	}
	if err := s.recordTransitionTx(ctx, tx, id, nil, o.Status, "order created", "system", nil, nil); err != nil {
		return domain.Order{}, err
	}
	return s.OrderByIDTx(ctx, tx, id)
}

// OrderByIDTx loads an order inside a transaction, taking a row lock so a
// concurrent fill handler cannot interleave with a state transition.
func (s *TradingStore) OrderByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (domain.Order, error) {
	row := tx.QueryRow(ctx, `
		SELECT `+orderColumns+`
		FROM orders o JOIN instruments i ON i.id = o.instrument_id
		WHERE o.id = $1 FOR UPDATE OF o`, id)
	return scanOrder(row)
}

// OrderForUser loads an order scoped to its owner.
func (s *TradingStore) OrderForUser(ctx context.Context, userID, orderID uuid.UUID) (domain.Order, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+orderColumns+`
		FROM orders o JOIN instruments i ON i.id = o.instrument_id
		WHERE o.id = $1 AND o.user_id = $2`, orderID, userID)
	return scanOrder(row)
}

// OrderFilter narrows an order listing.
type OrderFilter struct {
	AccountID    *uuid.UUID
	InstrumentID *string
	Status       []domain.OrderStatus
	OpenOnly     bool
	StrategyID   *uuid.UUID
	Limit        int
	Offset       int
}

// ListOrdersForUser returns the caller's orders.
func (s *TradingStore) ListOrdersForUser(ctx context.Context, userID uuid.UUID, f OrderFilter) ([]domain.Order, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args := []any{userID, limit, f.Offset}
	q := `SELECT ` + orderColumns + `
	      FROM orders o JOIN instruments i ON i.id = o.instrument_id
	      WHERE o.user_id = $1`

	if f.AccountID != nil {
		args = append(args, *f.AccountID)
		q += fmt.Sprintf(" AND o.account_id = $%d", len(args))
	}
	if f.InstrumentID != nil {
		args = append(args, *f.InstrumentID)
		q += fmt.Sprintf(" AND o.instrument_id = $%d", len(args))
	}
	if f.StrategyID != nil {
		args = append(args, *f.StrategyID)
		q += fmt.Sprintf(" AND o.strategy_id = $%d", len(args))
	}
	if f.OpenOnly {
		q += ` AND o.status IN ('CREATED','VALIDATING','ACCEPTED','SUBMITTED','PARTIALLY_FILLED','CANCEL_PENDING')`
	} else if len(f.Status) > 0 {
		statuses := make([]string, 0, len(f.Status))
		for _, st := range f.Status {
			statuses = append(statuses, string(st))
		}
		args = append(args, statuses)
		q += fmt.Sprintf(" AND o.status = ANY($%d)", len(args))
	}
	q += ` ORDER BY o.created_at DESC LIMIT $2 OFFSET $3`

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []domain.Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, mapError(rows.Err())
}

// CountOpenOrders counts orders still consuming risk budget.
func (s *TradingStore) CountOpenOrders(ctx context.Context, accountID uuid.UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM orders
		WHERE account_id = $1
		  AND status IN ('CREATED','VALIDATING','ACCEPTED','SUBMITTED','PARTIALLY_FILLED','CANCEL_PENDING')`,
		accountID).Scan(&n)
	return n, mapError(err)
}

// TransitionOrderTx moves an order to a new state.
//
// Three guards apply, and all three must pass:
//   - the state machine must permit from -> to;
//   - the caller's version must match, so a concurrent writer cannot be
//     silently overwritten;
//   - the transition is recorded in the append-only history.
func (s *TradingStore) TransitionOrderTx(ctx context.Context, tx pgx.Tx, orderID uuid.UUID,
	expectedVersion int64, to domain.OrderStatus, reason string, actorType string, actorUserID *uuid.UUID) (domain.Order, error) {

	return s.TransitionOrderWithCodeTx(ctx, tx, orderID, expectedVersion, to, "", reason, actorType, actorUserID)
}

// TransitionOrderWithCodeTx transitions an order and, when the destination is
// REJECTED, records the machine-readable code alongside the reason.
//
// This exists because `orders_reject_pair_ck` requires a rejected order to
// carry a code, and an order that says REJECTED with no reason is unusable as
// evidence. Reconciliation hit this: it moved a FAILED order the venue had no
// record of to REJECTED without a code, the constraint refused the write, and
// the whole reconciliation run failed — which permanently blocked automated
// trading for the account, in exactly the recovery path the FAILED state
// exists to serve.
func (s *TradingStore) TransitionOrderWithCodeTx(ctx context.Context, tx pgx.Tx, orderID uuid.UUID,
	expectedVersion int64, to domain.OrderStatus, code domain.RejectCode, reason string,
	actorType string, actorUserID *uuid.UUID) (domain.Order, error) {

	current, err := s.OrderByIDTx(ctx, tx, orderID)
	if err != nil {
		return domain.Order{}, err
	}
	if current.Version != expectedVersion {
		return domain.Order{}, ErrStaleVersion
	}
	if !domain.CanTransition(current.Status, to) {
		return domain.Order{}, domain.ErrIllegalTransition{From: current.Status, To: to}
	}

	closedAt := "closed_at"
	if to.Terminal() {
		closedAt = "COALESCE(closed_at, now())"
	}
	submittedAt := "submitted_at"
	if to == domain.OrderSubmitted {
		submittedAt = "COALESCE(submitted_at, now())"
	}

	// A rejected order must carry a code. COALESCE preserves the original
	// code when one is already present, so a re-transition cannot overwrite
	// the reason an order was first refused.
	rejectCode := any(nil)
	rejectReason := any(nil)
	if to == domain.OrderRejected {
		if code == "" {
			code = domain.RejectInternalError
		}
		rejectCode = string(code)
		rejectReason = reason
	}

	tag, err := tx.Exec(ctx, fmt.Sprintf(`
		UPDATE orders
		SET status = $2, updated_at = now(), version = version + 1,
		    closed_at = %s, submitted_at = %s,
		    reject_code = COALESCE(reject_code, $4),
		    reject_reason = COALESCE(NULLIF(reject_reason, ''), $5)
		WHERE id = $1 AND version = $3`, closedAt, submittedAt),
		orderID, to, expectedVersion, rejectCode, rejectReason)
	if err != nil {
		return domain.Order{}, mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Order{}, ErrStaleVersion
	}

	from := current.Status
	if err := s.recordTransitionTx(ctx, tx, orderID, &from, to, reason, actorType, actorUserID, nil); err != nil {
		return domain.Order{}, err
	}
	return s.OrderByIDTx(ctx, tx, orderID)
}

// RejectOrderTx moves an order to REJECTED with a structured reason.
func (s *TradingStore) RejectOrderTx(ctx context.Context, tx pgx.Tx, orderID uuid.UUID,
	expectedVersion int64, rej domain.Rejection, actorType string, actorUserID *uuid.UUID) (domain.Order, error) {

	current, err := s.OrderByIDTx(ctx, tx, orderID)
	if err != nil {
		return domain.Order{}, err
	}
	if !domain.CanTransition(current.Status, domain.OrderRejected) {
		return domain.Order{}, domain.ErrIllegalTransition{From: current.Status, To: domain.OrderRejected}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE orders
		SET status = 'REJECTED', reject_code = $2, reject_reason = $3,
		    closed_at = now(), updated_at = now(), version = version + 1
		WHERE id = $1 AND version = $4`,
		orderID, string(rej.Code), rej.Message, expectedVersion)
	if err != nil {
		return domain.Order{}, mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Order{}, ErrStaleVersion
	}
	from := current.Status
	meta, _ := json.Marshal(rej.Detail)
	if err := s.recordTransitionTx(ctx, tx, orderID, &from, domain.OrderRejected,
		string(rej.Code)+": "+rej.Message, actorType, actorUserID, meta); err != nil {
		return domain.Order{}, err
	}
	return s.OrderByIDTx(ctx, tx, orderID)
}

// SetBrokerOrderIDTx records the broker's identifier for an order.
func (s *TradingStore) SetBrokerOrderIDTx(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, brokerOrderID string) error {
	_, err := tx.Exec(ctx, `
		UPDATE orders SET broker_order_id = $2, updated_at = now() WHERE id = $1`,
		orderID, brokerOrderID)
	return mapError(err)
}

func (s *TradingStore) recordTransitionTx(ctx context.Context, tx pgx.Tx, orderID uuid.UUID,
	from *domain.OrderStatus, to domain.OrderStatus, reason, actorType string,
	actorUserID *uuid.UUID, metadata []byte) error {

	if metadata == nil {
		metadata = []byte(`{}`)
	}
	var fromStr *string
	if from != nil {
		v := string(*from)
		fromStr = &v
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO order_state_transitions
			(order_id, from_status, to_status, reason, actor_type, actor_user_id, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		orderID, fromStr, string(to), reason, actorType, actorUserID, metadata)
	return mapError(err)
}

// OrderTransition is one recorded state change.
type OrderTransition struct {
	FromStatus *string
	ToStatus   string
	Reason     *string
	ActorType  string
	OccurredAt time.Time
}

// OrderTransitions returns an order's history, oldest first.
func (s *TradingStore) OrderTransitions(ctx context.Context, orderID uuid.UUID) ([]OrderTransition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT from_status, to_status, reason, actor_type, occurred_at
		FROM order_state_transitions WHERE order_id = $1 ORDER BY occurred_at, id`, orderID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []OrderTransition
	for rows.Next() {
		var t OrderTransition
		if err := rows.Scan(&t.FromStatus, &t.ToStatus, &t.Reason, &t.ActorType, &t.OccurredAt); err != nil {
			return nil, mapError(err)
		}
		out = append(out, t)
	}
	return out, mapError(rows.Err())
}

// ---------------------------------------------------------------------------
// Fills
// ---------------------------------------------------------------------------

// AppendFillTx records an execution and advances the order's filled quantity.
//
// Returns ErrDuplicateCommand when the broker fill id has already been
// recorded. That is a normal occurrence, not an error condition: brokers
// replay executions after a reconnect, and the correct response is to ignore
// the repeat rather than to book the trade twice.
func (s *TradingStore) AppendFillTx(ctx context.Context, tx pgx.Tx, f domain.Fill, mode domain.ExecutionMode) (domain.Fill, error) {
	if err := AssertPaperMode(mode); err != nil {
		return domain.Fill{}, err
	}

	// Conflict-tolerant for the same reason as RegisterCommand: a replayed
	// execution must not abort the transaction that is booking the others.
	// A broker replaying fills after a reconnect is normal, not exceptional.
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO fills (order_id, account_id, instrument_id, side, quantity, price,
			commission, commission_currency, slippage, liquidity, broker_name, broker_fill_id,
			mode, executed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (broker_name, broker_fill_id) DO NOTHING
		RETURNING id`,
		f.OrderID, f.AccountID, f.InstrumentID, f.Side, f.Quantity, f.Price,
		f.Commission, f.CommissionCcy, f.Slippage, f.Liquidity, f.BrokerName, f.BrokerFillID,
		mode, f.ExecutedAt).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// No row inserted: this execution is already recorded.
		return domain.Fill{}, ErrDuplicateCommand
	}
	if err != nil {
		return domain.Fill{}, mapError(err)
	}

	// Recompute filled quantity and average price from the fills themselves
	// rather than incrementing a counter. A derived value cannot drift.
	_, err = tx.Exec(ctx, `
		UPDATE orders o
		SET filled_quantity = agg.qty,
		    avg_fill_price = agg.avg_price,
		    updated_at = now(),
		    version = version + 1
		FROM (
			SELECT COALESCE(SUM(quantity), 0) AS qty,
			       CASE WHEN COALESCE(SUM(quantity), 0) > 0
			            THEN SUM(price * quantity) / SUM(quantity)
			            ELSE 0 END AS avg_price
			FROM fills WHERE order_id = $1
		) agg
		WHERE o.id = $1`, f.OrderID)
	if err != nil {
		return domain.Fill{}, mapError(err)
	}

	f.ID = id
	f.RecordedAt = time.Now().UTC()
	return f, nil
}

// FillsForOrder returns an order's executions.
func (s *TradingStore) FillsForOrder(ctx context.Context, orderID uuid.UUID) ([]domain.Fill, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, order_id, account_id, instrument_id, side, quantity, price, commission,
		       commission_currency, slippage, liquidity, broker_name, broker_fill_id,
		       executed_at, recorded_at
		FROM fills WHERE order_id = $1 ORDER BY executed_at, id`, orderID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	return scanFills(rows)
}

// RecentFills returns an account's recent executions.
func (s *TradingStore) RecentFills(ctx context.Context, accountID uuid.UUID, limit int) ([]domain.Fill, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, order_id, account_id, instrument_id, side, quantity, price, commission,
		       commission_currency, slippage, liquidity, broker_name, broker_fill_id,
		       executed_at, recorded_at
		FROM fills WHERE account_id = $1 ORDER BY executed_at DESC LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	return scanFills(rows)
}

func scanFills(rows pgx.Rows) ([]domain.Fill, error) {
	var out []domain.Fill
	for rows.Next() {
		var f domain.Fill
		if err := rows.Scan(&f.ID, &f.OrderID, &f.AccountID, &f.InstrumentID, &f.Side,
			&f.Quantity, &f.Price, &f.Commission, &f.CommissionCcy, &f.Slippage,
			&f.Liquidity, &f.BrokerName, &f.BrokerFillID, &f.ExecutedAt, &f.RecordedAt); err != nil {
			return nil, mapError(err)
		}
		out = append(out, f)
	}
	return out, mapError(rows.Err())
}

// ---------------------------------------------------------------------------
// Positions
// ---------------------------------------------------------------------------

const positionColumns = `p.id, p.account_id, p.instrument_id, i.symbol, p.mode, p.side, p.quantity,
	p.avg_entry_price, p.status, p.stop_loss, p.take_profit, p.realized_pnl, p.commission,
	p.swap, p.currency, p.strategy_id, p.version, p.opened_at, p.updated_at, p.closed_at`

func scanPosition(row pgx.Row) (domain.Position, error) {
	var p domain.Position
	var realized, commission, swap decimal.Decimal
	var ccy string
	err := row.Scan(&p.ID, &p.AccountID, &p.InstrumentID, &p.Symbol, &p.Mode, &p.Side, &p.Quantity,
		&p.AvgEntryPrice, &p.Status, &p.StopLoss, &p.TakeProfit, &realized, &commission,
		&swap, &ccy, &p.StrategyID, &p.Version, &p.OpenedAt, &p.UpdatedAt, &p.ClosedAt)
	if err != nil {
		return domain.Position{}, mapError(err)
	}
	c := money.Currency(ccy)
	p.RealizedPnL = money.New(realized, c)
	p.Commission = money.New(commission, c)
	p.Swap = money.New(swap, c)
	return p, nil
}

// OpenPositionTx loads the open position for an instrument, locking it.
// Returns ErrNotFound when the account is flat in that instrument.
func (s *TradingStore) OpenPositionTx(ctx context.Context, tx pgx.Tx, accountID uuid.UUID, instrumentID string) (domain.Position, error) {
	row := tx.QueryRow(ctx, `
		SELECT `+positionColumns+`
		FROM positions p JOIN instruments i ON i.id = p.instrument_id
		WHERE p.account_id = $1 AND p.instrument_id = $2 AND p.status = 'open'
		FOR UPDATE OF p`, accountID, instrumentID)
	return scanPosition(row)
}

// UpsertPositionTx writes a position's new state.
func (s *TradingStore) UpsertPositionTx(ctx context.Context, tx pgx.Tx, p domain.Position) (domain.Position, error) {
	if err := AssertPaperMode(p.Mode); err != nil {
		return domain.Position{}, err
	}

	if p.ID == uuid.Nil {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO positions (account_id, instrument_id, mode, side, quantity, avg_entry_price,
				status, stop_loss, take_profit, realized_pnl, commission, swap, currency, strategy_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			RETURNING id`,
			p.AccountID, p.InstrumentID, p.Mode, p.Side, p.Quantity, p.AvgEntryPrice,
			p.Status, p.StopLoss, p.TakeProfit, p.RealizedPnL.Decimal(), p.Commission.Decimal(),
			p.Swap.Decimal(), string(p.RealizedPnL.Currency()), p.StrategyID).Scan(&id)
		if err != nil {
			return domain.Position{}, mapError(err)
		}
		return s.positionByIDTx(ctx, tx, id)
	}

	closedAt := any(nil)
	if p.Status == domain.PositionClosed {
		closedAt = time.Now().UTC()
	}
	tag, err := tx.Exec(ctx, `
		UPDATE positions
		SET side = $2, quantity = $3, avg_entry_price = $4, status = $5,
		    stop_loss = $6, take_profit = $7, realized_pnl = $8, commission = $9, swap = $10,
		    closed_at = COALESCE($11, closed_at), updated_at = now(), version = version + 1
		WHERE id = $1 AND version = $12`,
		p.ID, p.Side, p.Quantity, p.AvgEntryPrice, p.Status, p.StopLoss, p.TakeProfit,
		p.RealizedPnL.Decimal(), p.Commission.Decimal(), p.Swap.Decimal(), closedAt, p.Version)
	if err != nil {
		return domain.Position{}, mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Position{}, ErrStaleVersion
	}
	return s.positionByIDTx(ctx, tx, p.ID)
}

func (s *TradingStore) positionByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (domain.Position, error) {
	row := tx.QueryRow(ctx, `
		SELECT `+positionColumns+`
		FROM positions p JOIN instruments i ON i.id = p.instrument_id
		WHERE p.id = $1`, id)
	return scanPosition(row)
}

// OpenPositions returns an account's open positions.
func (s *TradingStore) OpenPositions(ctx context.Context, accountID uuid.UUID) ([]domain.Position, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+positionColumns+`
		FROM positions p JOIN instruments i ON i.id = p.instrument_id
		WHERE p.account_id = $1 AND p.status = 'open'
		ORDER BY p.opened_at`, accountID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []domain.Position
	for rows.Next() {
		p, err := scanPosition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, mapError(rows.Err())
}

// ClosedPositions returns an account's recent closed positions.
func (s *TradingStore) ClosedPositions(ctx context.Context, accountID uuid.UUID, limit int) ([]domain.Position, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+positionColumns+`
		FROM positions p JOIN instruments i ON i.id = p.instrument_id
		WHERE p.account_id = $1 AND p.status = 'closed'
		ORDER BY p.closed_at DESC LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []domain.Position
	for rows.Next() {
		p, err := scanPosition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, mapError(rows.Err())
}

// ---------------------------------------------------------------------------
// Outbox
// ---------------------------------------------------------------------------

// EnqueueOutboxTx writes a side effect in the same transaction as the state
// change that caused it, so the event cannot be lost if the process dies.
func (s *TradingStore) EnqueueOutboxTx(ctx context.Context, tx pgx.Tx, aggregateType, aggregateID, eventType string, payload any, correlationID string) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("store: marshal outbox payload: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox (aggregate_type, aggregate_id, event_type, payload, correlation_id)
		VALUES ($1,$2,$3,$4,$5)`, aggregateType, aggregateID, eventType, raw, correlationID)
	return mapError(err)
}

// OutboxMessage is a pending side effect.
type OutboxMessage struct {
	ID            int64
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       json.RawMessage
	CorrelationID *string
	Attempts      int
	CreatedAt     time.Time
}

// ClaimOutboxBatch takes a batch of pending messages, skipping rows another
// dispatcher already holds. SKIP LOCKED is what lets several dispatchers run
// without any of them handling the same message.
func (s *TradingStore) ClaimOutboxBatch(ctx context.Context, tx pgx.Tx, limit int) ([]OutboxMessage, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := tx.Query(ctx, `
		SELECT id, aggregate_type, aggregate_id, event_type, payload, correlation_id, attempts, created_at
		FROM outbox
		WHERE published_at IS NULL AND available_at <= now()
		ORDER BY id
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		if err := rows.Scan(&m.ID, &m.AggregateType, &m.AggregateID, &m.EventType,
			&m.Payload, &m.CorrelationID, &m.Attempts, &m.CreatedAt); err != nil {
			return nil, mapError(err)
		}
		out = append(out, m)
	}
	return out, mapError(rows.Err())
}

// MarkOutboxPublished marks a message delivered.
func (s *TradingStore) MarkOutboxPublished(ctx context.Context, tx pgx.Tx, id int64) error {
	_, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = $1`, id)
	return mapError(err)
}

// MarkOutboxFailed schedules a retry with exponential backoff.
func (s *TradingStore) MarkOutboxFailed(ctx context.Context, tx pgx.Tx, id int64, attempts int, errMsg string) error {
	backoff := time.Duration(1<<min(attempts, 10)) * time.Second
	_, err := tx.Exec(ctx, `
		UPDATE outbox
		SET attempts = attempts + 1, last_error = $2, available_at = now() + $3::interval
		WHERE id = $1`, id, errMsg, backoff.String())
	return mapError(err)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
