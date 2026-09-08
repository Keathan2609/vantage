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
