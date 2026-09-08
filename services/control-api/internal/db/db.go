// Package db owns the PostgreSQL connection pool and schema migrations.
package db

import (
	"context"
	"fmt"
	"time"

	pgxdecimal "github.com/jackc/pgx-shopspring-decimal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps pgxpool with the settings the control plane depends on.
type Pool struct {
	*pgxpool.Pool
}

// Open connects to PostgreSQL and verifies the connection.
//
// Pool sizing is deliberately bounded. An unbounded pool converts a traffic
// spike into database saturation, and a saturated database in a trading system
// means order writes queue behind dashboard reads.
func Open(ctx context.Context, url string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("db: parse config: %w", err)
	}

	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	// A connection attempt that hangs must fail rather than block a request
	// that is holding an order in a pending state.
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second

	// Every session runs with a statement timeout: a runaway query must not be
	// able to hold locks on financial tables indefinitely.
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "15000"
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "30000"
	cfg.ConnConfig.RuntimeParams["application_name"] = "vantage-control-api"
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"

	// Teach pgx to read and write NUMERIC as shopspring decimals. Without this
	// every monetary column would round-trip through a float or a string, and
	// the first of those silently loses precision.
	cfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		pgxdecimal.Register(conn.TypeMap())
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return &Pool{Pool: pool}, nil
}

// InTx runs fn inside a serializable-by-default read-committed transaction and
// commits it, rolling back on any error or panic.
//
// Every write that touches money goes through here. A partially applied fill —
// position updated, ledger not — is not a state the system can recover from by
// retrying, so the unit of work is the transaction, never the statement.
func (p *Pool) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(r)
		}
	}()

	if err := fn(tx); err != nil {
		// Use a detached context so the rollback still runs when the request
		// context is already cancelled.
		if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil && rbErr != pgx.ErrTxClosed {
			return fmt.Errorf("%w (rollback failed: %v)", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}

// InSerializableTx runs fn at SERIALIZABLE isolation. Used where a read
// informs a write that must not interleave with a concurrent identical
// operation — for example allocating the next ledger sequence.
func (p *Pool) InSerializableTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := p.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("db: begin serializable: %w", err)
	}
	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(r)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit serializable: %w", err)
	}
	return nil
}

// SessionLock is a held advisory lock. Release it when the work is done.
type SessionLock struct {
	conn    *pgxpool.Conn
	classID int32
	objID   int32
}

// Release unlocks and returns the connection to the pool.
//
// Safe to call twice, so `defer lock.Release()` alongside an early explicit
// release is not a bug.
func (l *SessionLock) Release(ctx context.Context) {
	if l == nil || l.conn == nil {
		return
	}
	// A detached context, so a cancelled request still releases the lock
	// rather than leaving it held until the connection is recycled.
	_, _ = l.conn.Exec(context.WithoutCancel(ctx),
		`SELECT pg_advisory_unlock($1, $2)`, l.classID, l.objID)
	l.conn.Release()
	l.conn = nil
}

// TryAcquireSessionLock takes a SESSION-scoped advisory lock without blocking.
//
// # Why session-scoped and not transaction-scoped
//
// A reconciliation run spans several transactions by design: it reads a venue
// snapshot with no transaction open, then repairs each issue in its own unit of
// work so one failure does not roll back the others. A transaction-scoped lock
// would therefore be released between the snapshot and the repairs, which is
// exactly the window a second run must not be allowed into.
//
// # Why an advisory lock rather than a row
//
// It is enforced by PostgreSQL across processes, so two control planes cannot
// both decide they hold it, and it is released automatically if the process
// dies — a `locked_until` column in a table would leave a stale lock that
// needs a timeout to clear, and picking that timeout means guessing how long a
// run should take.
//
// The connection is held for the lock's lifetime. That is one connection out
// of the pool per concurrently reconciling account, which is why RunAll
// reconciles accounts in sequence rather than fanning out.
func (p *Pool) TryAcquireSessionLock(ctx context.Context, class AdvisoryLockKey, object int32) (*SessionLock, bool, error) {
	conn, err := p.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("db: acquire connection for session lock: %w", err)
	}
	var acquired bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock($1, $2)`, int32(class), object).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("db: try session advisory lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return &SessionLock{conn: conn, classID: int32(class), objID: object}, true, nil
}

// AdvisoryLockKey namespaces the 64-bit advisory lock space so unrelated
// subsystems cannot collide.
type AdvisoryLockKey int64

const (
	// LockAuditChain serialises audit-chain appends so the hash chain has a
	// single writer and cannot fork.
	LockAuditChain AdvisoryLockKey = 8_100_001
	// LockMigrations serialises schema migration across instances.
	LockMigrations AdvisoryLockKey = 8_100_002
	// LockOutboxDispatch keeps one dispatcher active at a time.
	LockOutboxDispatch AdvisoryLockKey = 8_100_003
	// LockReconciliationAccount namespaces the per-account reconciliation
	// lock. Unlike the keys above it is used with the two-argument advisory
	// lock form, so this value is the class and the account's hash is the
	// object -- which keeps every account's lock distinct while remaining
	// obviously a reconciliation lock in pg_locks.
	LockReconciliationAccount AdvisoryLockKey = 8_100_004
)

// TryAdvisoryLock attempts to take a transaction-scoped advisory lock without
// blocking. The lock is released when the transaction ends.
func TryAdvisoryLock(ctx context.Context, tx pgx.Tx, key AdvisoryLockKey) (bool, error) {
	var acquired bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, int64(key)).Scan(&acquired); err != nil {
		return false, fmt.Errorf("db: try advisory lock: %w", err)
	}
	return acquired, nil
}

// AdvisoryLock takes a transaction-scoped advisory lock, waiting for it.
func AdvisoryLock(ctx context.Context, tx pgx.Tx, key AdvisoryLockKey) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(key)); err != nil {
		return fmt.Errorf("db: advisory lock: %w", err)
	}
	return nil
}
