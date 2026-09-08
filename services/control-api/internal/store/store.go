// Package store is the persistence layer for the control plane.
//
// Design rules this package follows without exception:
//
//   - Every query is parameterised. No SQL is ever assembled from user input.
//   - Every read of a user-owned resource is scoped by owner in the WHERE
//     clause. Authorisation is not a filter applied after loading a row; a row
//     the caller does not own is never loaded in the first place. This is what
//     makes broken-object-level-authorisation bugs structurally hard here.
//   - Mutations of financial objects use optimistic concurrency: the caller
//     passes the version it read, and an UPDATE that matches no row is
//     reported as a stale-version conflict rather than silently doing nothing.
//   - Money is read and written as NUMERIC and carried in Go as decimals with
//     an explicit currency.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vantage/control-api/internal/db"
)

// Store holds the database pool and the aggregate-specific repositories.
type Store struct {
	pool *db.Pool

	Users    *UserStore
	Accounts *AccountStore
	Market   *MarketStore
	Trading  *TradingStore
	Control  *ControlStore
	Research *ResearchStore
}

// New builds a Store over an open pool.
func New(pool *db.Pool) *Store {
	s := &Store{pool: pool}
	s.Users = &UserStore{pool: pool}
	s.Accounts = &AccountStore{pool: pool}
	s.Market = &MarketStore{pool: pool}
	s.Trading = &TradingStore{pool: pool}
	s.Control = &ControlStore{pool: pool}
	s.Research = &ResearchStore{pool: pool}
	return s
}

// Pool exposes the underlying pool for transactional work that spans stores.
func (s *Store) Pool() *db.Pool { return s.pool }

// Errors the service layer distinguishes.
var (
	// ErrNotFound covers both "does not exist" and "exists but is not yours".
	// The distinction is deliberately not surfaced: telling a caller that an
	// object exists but belongs to someone else is an enumeration oracle.
	ErrNotFound = errors.New("store: not found")
	// ErrStaleVersion means an optimistic-concurrency update lost the race.
	ErrStaleVersion = errors.New("store: stale object version")
	// ErrConflict covers unique-constraint violations.
	ErrConflict = errors.New("store: conflict")
	// ErrConstraint covers check-constraint violations, which indicate the
	// application tried to persist a state the schema forbids.
	ErrConstraint = errors.New("store: constraint violation")
)

// ConstraintError names the specific database constraint that rejected a write.
type ConstraintError struct {
	Constraint string
	Table      string
	Detail     string
}

func (e ConstraintError) Error() string {
	return fmt.Sprintf("store: constraint %q on %q rejected the write", e.Constraint, e.Table)
}

func (e ConstraintError) Is(target error) bool {
	return target == ErrConstraint || target == ErrConflict
}

// mapError translates pgx errors into the package's error vocabulary.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return ConstraintError{Constraint: pgErr.ConstraintName, Table: pgErr.TableName, Detail: pgErr.Detail}
		case "23514", "23503", "23502": // check, foreign key, not null
			return ConstraintError{Constraint: pgErr.ConstraintName, Table: pgErr.TableName, Detail: pgErr.Detail}
		case "23001": // restrict_violation, raised by the append-only triggers
			return ConstraintError{Constraint: "append_only", Table: pgErr.TableName, Detail: pgErr.Message}
		case "40001": // serialization_failure
			return fmt.Errorf("%w: serialisation conflict, retry", ErrStaleVersion)
		}
	}
	return err
}

// IsConstraint reports whether err was a violation of the named constraint.
func IsConstraint(err error, name string) bool {
	var ce ConstraintError
	if errors.As(err, &ce) {
		return ce.Constraint == name
	}
	return false
}

// querier is satisfied by both *pgxpool.Pool and pgx.Tx, so every repository
// method can run inside a caller's transaction or on its own.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
