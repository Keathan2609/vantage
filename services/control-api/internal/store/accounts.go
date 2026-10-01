package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/db"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/money"
)

// AccountStore persists accounts, the ledger and equity state.
type AccountStore struct{ pool *db.Pool }

const accountColumns = `id, user_id, name, mode, currency, broker_name, broker_account_ref,
	enabled, trading_enabled, leverage, created_at, updated_at, version`

func scanAccount(row pgx.Row) (domain.Account, error) {
	var a domain.Account
	var ccy string
	err := row.Scan(&a.ID, &a.UserID, &a.Name, &a.Mode, &ccy, &a.BrokerName, &a.BrokerAcctRef,
		&a.Enabled, &a.TradingEnabled, &a.Leverage, &a.CreatedAt, &a.UpdatedAt, &a.Version)
	if err != nil {
		return domain.Account{}, mapError(err)
	}
	a.Currency = money.Currency(ccy)
	return a, nil
}

// CreateAccount inserts an account.
func (s *AccountStore) CreateAccount(ctx context.Context, a domain.Account) (domain.Account, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO accounts (user_id, name, mode, currency, broker_name, broker_account_ref,
		                      enabled, trading_enabled, leverage)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING `+accountColumns,
		a.UserID, a.Name, a.Mode, string(a.Currency), a.BrokerName, a.BrokerAcctRef,
		a.Enabled, a.TradingEnabled, a.Leverage)
	return scanAccount(row)
}

// AccountForUser loads an account scoped to its owner.
//
// The owner is part of the WHERE clause rather than a check performed after
// loading. A request for someone else's account id returns ErrNotFound, and no
// code path exists that could forget the ownership predicate.
func (s *AccountStore) AccountForUser(ctx context.Context, userID, accountID uuid.UUID) (domain.Account, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE id = $1 AND user_id = $2`,
		accountID, userID)
	return scanAccount(row)
}

// AccountByIDUnscoped loads an account without an ownership predicate. It
// exists for system paths that have already established authorisation --
// reconciliation, the scheduler -- and must never be reached from a request
// handler using a caller-supplied id.
func (s *AccountStore) AccountByIDUnscoped(ctx context.Context, accountID uuid.UUID) (domain.Account, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id = $1`, accountID)
	return scanAccount(row)
}

// ListAccountsForUser returns the caller's accounts.
func (s *AccountStore) ListAccountsForUser(ctx context.Context, userID uuid.UUID) ([]domain.Account, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []domain.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, mapError(rows.Err())
}

// ListAllAccounts returns every account, for scheduler and admin use.
func (s *AccountStore) ListAllAccounts(ctx context.Context) ([]domain.Account, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+accountColumns+` FROM accounts ORDER BY created_at`)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []domain.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, mapError(rows.Err())
}

// SetTradingEnabled suspends or resumes new trading on an account, using
// optimistic concurrency so two operators cannot silently overwrite each other.
func (s *AccountStore) SetTradingEnabled(ctx context.Context, accountID uuid.UUID, enabled bool, expectedVersion int64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE accounts SET trading_enabled = $2, updated_at = now(), version = version + 1
		WHERE id = $1 AND version = $3`, accountID, enabled, expectedVersion)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleVersion
	}
	return nil
}

// ---------------------------------------------------------------------------
// Ledger
// ---------------------------------------------------------------------------

// AppendTransactionTx writes one ledger entry inside the caller's transaction.
//
// The sequence is allocated from the account's current maximum under the row
// lock the caller already holds on the account. Combined with the unique index
// on (account_id, sequence), a concurrent writer either serialises behind this
// one or fails outright; it cannot interleave and produce two entries claiming
// the same position in the ledger.
func (s *AccountStore) AppendTransactionTx(ctx context.Context, tx pgx.Tx, t domain.Transaction) (domain.Transaction, error) {
	var seq int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(sequence), 0) + 1 FROM transactions WHERE account_id = $1`,
		t.AccountID).Scan(&seq); err != nil {
		return domain.Transaction{}, mapError(err)
	}

	var out domain.Transaction
	var amount, balanceAfter decimal.Decimal
	var ccy string
	err := tx.QueryRow(ctx, `
		INSERT INTO transactions (account_id, sequence, type, amount, currency, balance_after,
		                          order_id, fill_id, position_id, description, mode)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id, account_id, sequence, type, amount, currency, balance_after,
		          order_id, fill_id, position_id, description, mode, created_at`,
		t.AccountID, seq, t.Type, t.Amount.Decimal(), string(t.Amount.Currency()),
		t.BalanceAfter.Decimal(), t.OrderID, t.FillID, t.PositionID, t.Description, t.Mode).
		Scan(&out.ID, &out.AccountID, &out.Sequence, &out.Type, &amount, &ccy, &balanceAfter,
			&out.OrderID, &out.FillID, &out.PositionID, &out.Description, &out.Mode, &out.CreatedAt)
	if err != nil {
		return domain.Transaction{}, mapError(err)
	}
	out.Amount = money.New(amount, money.Currency(ccy))
	out.BalanceAfter = money.New(balanceAfter, money.Currency(ccy))
	return out, nil
}

// LockAccountTx takes the account's exclusive row lock.
//
// # The lock order, and why it has to be declared
//
// This is the OUTERMOST lock for every transaction that writes anything
// belonging to an account. It must be taken BEFORE any other row in that
// account's graph -- before positions, before fills, before the ledger.
//
// That rule is not stylistic. It was written after a concurrency test placed
// eight orders at once on one account and six of them died with
// "deadlock detected (SQLSTATE 40P01)", surfaced to the client as HTTP 500 --
// on an order-placement call, which is the worst possible place to return an
// answer that means nothing.
//
// The cycle was subtle, because no two statements appeared to lock in
// different orders. Every table in this graph carries
// `account_id REFERENCES accounts (id)`, so INSERTing a fill silently takes a
// FOR KEY SHARE lock on the account row. The sequence was:
//
//	T1  INSERT fill        -> KEY SHARE on accounts (implicit, via the FK)
//	T1  SELECT position    -> FOR UPDATE on positions          [held]
//	T2  INSERT fill        -> KEY SHARE on accounts (granted; compatible)
//	T2  SELECT position    -> waits for T1's positions lock
//	T1  SELECT account     -> FOR UPDATE, conflicts with T2's KEY SHARE
//	                          -> waits for T2. Cycle.
//
// So the implicit lock came first and the explicit one came last, with the
// positions lock wedged between them. Taking the exclusive account lock up
// front collapses that: every writer queues on one row, in one order, and
// there is no cycle left to detect.
//
// The cost is real and accepted: writes to one account serialise. For a
// single-operator platform that is the correct trade -- a lost fill is
// unrecoverable, a queued one is merely slower.
func (s *AccountStore) LockAccountTx(ctx context.Context, tx pgx.Tx, accountID uuid.UUID) error {
	var locked uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT id FROM accounts WHERE id = $1 FOR UPDATE`, accountID).Scan(&locked); err != nil {
		return mapError(err)
	}
	return nil
}

// BalanceTx returns the account's current balance from the ledger's most
// recent entry, taking a row lock so a concurrent writer waits.
func (s *AccountStore) BalanceTx(ctx context.Context, tx pgx.Tx, accountID uuid.UUID, ccy money.Currency) (money.Amount, error) {
	// Idempotent when the caller already holds it, which it should: see
	// LockAccountTx for the lock order this is part of.
	if err := s.LockAccountTx(ctx, tx, accountID); err != nil {
		return money.Amount{}, err
	}

	var balance decimal.Decimal
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(
			(SELECT balance_after FROM transactions
			 WHERE account_id = $1 ORDER BY sequence DESC LIMIT 1), 0)`,
		accountID).Scan(&balance)
	if err != nil {
		return money.Amount{}, mapError(err)
	}
	return money.New(balance, ccy), nil
}

// Balance returns the current balance without locking, for read paths.
func (s *AccountStore) Balance(ctx context.Context, accountID uuid.UUID, ccy money.Currency) (money.Amount, error) {
	var balance decimal.Decimal
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(
			(SELECT balance_after FROM transactions
			 WHERE account_id = $1 ORDER BY sequence DESC LIMIT 1), 0)`,
		accountID).Scan(&balance)
	if err != nil {
		return money.Amount{}, mapError(err)
	}
	return money.New(balance, ccy), nil
}

// LedgerTotals aggregates realised P&L, fees, commission and swap.
type LedgerTotals struct {
	RealizedPnL money.Amount
	Commission  money.Amount
	Fees        money.Amount
	Swap        money.Amount
	Deposits    money.Amount
}

// Totals sums the ledger by category.
func (s *AccountStore) Totals(ctx context.Context, accountID uuid.UUID, ccy money.Currency) (LedgerTotals, error) {
	var realized, commission, fees, swap, deposits decimal.Decimal
	err := s.pool.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(amount) FILTER (WHERE type = 'realized_pnl'), 0),
			COALESCE(SUM(amount) FILTER (WHERE type = 'commission'), 0),
			COALESCE(SUM(amount) FILTER (WHERE type = 'fee'), 0),
			COALESCE(SUM(amount) FILTER (WHERE type = 'swap'), 0),
			COALESCE(SUM(amount) FILTER (WHERE type IN ('deposit','withdrawal')), 0)
		FROM transactions WHERE account_id = $1`, accountID).
		Scan(&realized, &commission, &fees, &swap, &deposits)
	if err != nil {
		return LedgerTotals{}, mapError(err)
	}
	return LedgerTotals{
		RealizedPnL: money.New(realized, ccy),
		Commission:  money.New(commission, ccy),
		Fees:        money.New(fees, ccy),
		Swap:        money.New(swap, ccy),
		Deposits:    money.New(deposits, ccy),
	}, nil
}

// ListTransactions returns a page of ledger entries, newest first.
func (s *AccountStore) ListTransactions(ctx context.Context, accountID uuid.UUID, ccy money.Currency, limit, offset int) ([]domain.Transaction, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, account_id, sequence, type, amount, currency, balance_after,
		       order_id, fill_id, position_id, description, mode, created_at
		FROM transactions WHERE account_id = $1
		ORDER BY sequence DESC LIMIT $2 OFFSET $3`, accountID, limit, offset)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var out []domain.Transaction
	for rows.Next() {
		var t domain.Transaction
		var amount, balanceAfter decimal.Decimal
		var rowCcy string
		if err := rows.Scan(&t.ID, &t.AccountID, &t.Sequence, &t.Type, &amount, &rowCcy, &balanceAfter,
			&t.OrderID, &t.FillID, &t.PositionID, &t.Description, &t.Mode, &t.CreatedAt); err != nil {
			return nil, mapError(err)
		}
		t.Amount = money.New(amount, money.Currency(rowCcy))
		t.BalanceAfter = money.New(balanceAfter, money.Currency(rowCcy))
		out = append(out, t)
	}
	return out, mapError(rows.Err())
}

// ---------------------------------------------------------------------------
// Equity state
// ---------------------------------------------------------------------------

// EquityState tracks the reference points that drawdown and daily-loss limits
// are measured against.
type EquityState struct {
	AccountID      uuid.UUID
	Currency       money.Currency
	PeakEquity     money.Amount
	DayStartEquity money.Amount
	DayStartAt     time.Time
	UpdatedAt      time.Time
	Version        int64
}

// InitEquityState creates the row if absent.
func (s *AccountStore) InitEquityState(ctx context.Context, accountID uuid.UUID, ccy money.Currency, equity money.Amount, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO account_equity_state (account_id, currency, peak_equity, day_start_equity, day_start_at)
		VALUES ($1,$2,$3,$3,$4)
		ON CONFLICT (account_id) DO NOTHING`,
		accountID, string(ccy), equity.Decimal(), at)
	return mapError(err)
}

// EquityStateTx loads the equity reference points inside a transaction.
func (s *AccountStore) EquityStateTx(ctx context.Context, tx pgx.Tx, accountID uuid.UUID) (EquityState, error) {
	var st EquityState
	var ccy string
	var peak, dayStart decimal.Decimal
	err := tx.QueryRow(ctx, `
		SELECT account_id, currency, peak_equity, day_start_equity, day_start_at, updated_at, version
		FROM account_equity_state WHERE account_id = $1 FOR UPDATE`, accountID).
		Scan(&st.AccountID, &ccy, &peak, &dayStart, &st.DayStartAt, &st.UpdatedAt, &st.Version)
	if err != nil {
		return EquityState{}, mapError(err)
	}
	st.Currency = money.Currency(ccy)
	st.PeakEquity = money.New(peak, st.Currency)
	st.DayStartEquity = money.New(dayStart, st.Currency)
	return st, nil
}

// EquityState loads the equity reference points for reads.
func (s *AccountStore) EquityState(ctx context.Context, accountID uuid.UUID) (EquityState, error) {
	var st EquityState
	var ccy string
	var peak, dayStart decimal.Decimal
	err := s.pool.QueryRow(ctx, `
		SELECT account_id, currency, peak_equity, day_start_equity, day_start_at, updated_at, version
		FROM account_equity_state WHERE account_id = $1`, accountID).
		Scan(&st.AccountID, &ccy, &peak, &dayStart, &st.DayStartAt, &st.UpdatedAt, &st.Version)
	if err != nil {
		return EquityState{}, mapError(err)
	}
	st.Currency = money.Currency(ccy)
	st.PeakEquity = money.New(peak, st.Currency)
	st.DayStartEquity = money.New(dayStart, st.Currency)
	return st, nil
}

// UpdatePeakEquityTx raises the high-water mark. It only ever increases: a
// drawdown limit measured against a peak that drifts downward would loosen
// exactly when the account is losing money.
func (s *AccountStore) UpdatePeakEquityTx(ctx context.Context, tx pgx.Tx, accountID uuid.UUID, equity money.Amount) error {
	_, err := tx.Exec(ctx, `
		UPDATE account_equity_state
		SET peak_equity = GREATEST(peak_equity, $2), updated_at = now(), version = version + 1
		WHERE account_id = $1`, accountID, equity.Decimal())
	return mapError(err)
}

// RollTradingDayTx resets the daily-loss reference point.
func (s *AccountStore) RollTradingDayTx(ctx context.Context, tx pgx.Tx, accountID uuid.UUID, equity money.Amount, at time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE account_equity_state
		SET day_start_equity = $2, day_start_at = $3, updated_at = now(), version = version + 1
		WHERE account_id = $1`, accountID, equity.Decimal(), at)
	return mapError(err)
}

// RecordEquityPoint appends to the equity history used by the portfolio chart.
func (s *AccountStore) RecordEquityPoint(ctx context.Context, tx pgx.Tx, accountID uuid.UUID,
	equity, balance, unrealized, marginUsed money.Amount, at time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO account_equity_history (account_id, equity, balance, unrealized_pnl, margin_used, currency, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		accountID, equity.Decimal(), balance.Decimal(), unrealized.Decimal(),
		marginUsed.Decimal(), string(equity.Currency()), at)
	return mapError(err)
}

// EquityPoint is one sample of account equity over time.
type EquityPoint struct {
	Equity        money.Amount
	Balance       money.Amount
	UnrealizedPnL money.Amount
	MarginUsed    money.Amount
	RecordedAt    time.Time
}

// EquityHistory returns recent equity samples, oldest first.
func (s *AccountStore) EquityHistory(ctx context.Context, accountID uuid.UUID, limit int) ([]EquityPoint, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT equity, balance, unrealized_pnl, margin_used, currency, recorded_at
		FROM (
			SELECT equity, balance, unrealized_pnl, margin_used, currency, recorded_at
			FROM account_equity_history
			WHERE account_id = $1
			ORDER BY recorded_at DESC
			LIMIT $2
		) recent
		ORDER BY recorded_at ASC`, accountID, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var out []EquityPoint
	for rows.Next() {
		var p EquityPoint
		var eq, bal, un, mar decimal.Decimal
		var ccy string
		if err := rows.Scan(&eq, &bal, &un, &mar, &ccy, &p.RecordedAt); err != nil {
			return nil, mapError(err)
		}
		c := money.Currency(ccy)
		p.Equity = money.New(eq, c)
		p.Balance = money.New(bal, c)
		p.UnrealizedPnL = money.New(un, c)
		p.MarginUsed = money.New(mar, c)
		out = append(out, p)
	}
	return out, mapError(rows.Err())
}

// AssertPaperMode is a defensive guard used by service code before writing any
// financial row. The database enforces the same rule; this produces a clearer
// error earlier, and documents the expectation at the call site.
func AssertPaperMode(mode domain.ExecutionMode) error {
	if mode != domain.ModePaper {
		return fmt.Errorf("refusing to persist a %s-mode financial record: this build is paper-only", mode)
	}
	return nil
}

// OwnerOf returns the user who owns an account.
//
// Used to route an account-scoped alert to the person it concerns rather than
// to everyone.
func (s *AccountStore) OwnerOf(ctx context.Context, accountID uuid.UUID) (uuid.UUID, error) {
	var userID uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT user_id FROM accounts WHERE id = $1`, accountID).Scan(&userID)
	if err != nil {
		return uuid.Nil, mapError(err)
	}
	return userID, nil
}
