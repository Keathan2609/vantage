package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vantage/control-api/migrations"
)

// migrationFS is the embedded set of schema migrations.
var migrationFS fs.FS = migrations.FS

// migrationDir is the root of the embedded filesystem. io/fs paths are
// unrooted and must not carry a "./" prefix, so entries are read by bare name.
const migrationDir = "."

// Migration is one versioned schema change.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// AppliedMigration is a migration recorded as applied.
type AppliedMigration struct {
	Version   int
	Name      string
	Checksum  string
	AppliedAt time.Time
}

// LoadMigrations reads and orders the embedded migration files.
func LoadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, migrationDir)
	if err != nil {
		return nil, fmt.Errorf("db: read migrations: %w", err)
	}

	var out []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		parts := strings.SplitN(e.Name(), "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("db: migration %q must be named NNNN_name.sql", e.Name())
		}
		version, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("db: migration %q has a non-numeric version: %w", e.Name(), err)
		}
		body, err := fs.ReadFile(migrationFS, e.Name())
		if err != nil {
			return nil, fmt.Errorf("db: read %q: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  version,
			Name:     strings.TrimSuffix(parts[1], ".sql"),
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })

	for i, m := range out {
		if i > 0 && out[i-1].Version == m.Version {
			return nil, fmt.Errorf("db: duplicate migration version %d", m.Version)
		}
	}
	return out, nil
}

// Migrate applies every pending migration.
//
// Properties this implementation guarantees:
//
//   - One instance migrates at a time (advisory lock), so a rolling deploy
//     cannot run the same DDL twice.
//   - Each migration runs inside its own transaction and is recorded in the
//     same transaction, so a failure leaves neither a half-applied schema nor a
//     false record of success.
//   - Applied migrations are checksummed. Editing a migration that has already
//     run is refused rather than silently ignored, because the database and the
//     repository would otherwise disagree about what the schema is.
func Migrate(ctx context.Context, pool *Pool) ([]Migration, error) {
	migrations, err := LoadMigrations()
	if err != nil {
		return nil, err
	}

	if err := ensureMigrationTable(ctx, pool); err != nil {
		return nil, err
	}

	applied, err := AppliedMigrations(ctx, pool)
	if err != nil {
		return nil, err
	}
	appliedByVersion := make(map[int]AppliedMigration, len(applied))
	for _, a := range applied {
		appliedByVersion[a.Version] = a
	}

	var ran []Migration
	for _, m := range migrations {
		if prev, ok := appliedByVersion[m.Version]; ok {
			if prev.Checksum != m.Checksum {
				return nil, fmt.Errorf(
					"db: migration %04d_%s has changed since it was applied "+
						"(recorded %s, file %s): add a new migration instead of editing an applied one",
					m.Version, m.Name, prev.Checksum[:12], m.Checksum[:12])
			}
			continue
		}

		err := pool.InTx(ctx, func(tx pgx.Tx) error {
			ok, err := TryAdvisoryLock(ctx, tx, LockMigrations)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("db: another instance is migrating; try again shortly")
			}
			if _, err := tx.Exec(ctx, m.SQL); err != nil {
				return fmt.Errorf("db: migration %04d_%s failed: %w", m.Version, m.Name, err)
			}
			_, err = tx.Exec(ctx,
				`INSERT INTO schema_migrations (version, name, checksum, applied_at)
				 VALUES ($1, $2, $3, now())`,
				m.Version, m.Name, m.Checksum)
			if err != nil {
				return fmt.Errorf("db: record migration %04d: %w", m.Version, err)
			}
			return nil
		})
		if err != nil {
			return ran, err
		}
		ran = append(ran, m)
	}
	return ran, nil
}

func ensureMigrationTable(ctx context.Context, pool *Pool) error {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			checksum   TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("db: create schema_migrations: %w", err)
	}
	return nil
}

// AppliedMigrations lists what the database believes it has applied.
//
// This is a pure read. It deliberately does NOT create the bookkeeping table:
// the serving role has no CREATE privilege on the schema, and a read path that
// required one would defeat the least-privilege split between the migration
// role and the runtime role. An absent table means nothing has been applied.
func AppliedMigrations(ctx context.Context, pool *Pool) ([]AppliedMigration, error) {
	rows, err := pool.Query(ctx,
		`SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return nil, nil // undefined_table: the schema has never been migrated
		}
		return nil, fmt.Errorf("db: list migrations: %w", err)
	}
	defer rows.Close()

	var out []AppliedMigration
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum, &a.AppliedAt); err != nil {
			return nil, fmt.Errorf("db: scan migration: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PendingMigrations reports migrations present in the binary but not applied.
func PendingMigrations(ctx context.Context, pool *Pool) ([]Migration, error) {
	all, err := LoadMigrations()
	if err != nil {
		return nil, err
	}
	applied, err := AppliedMigrations(ctx, pool)
	if err != nil {
		return nil, err
	}
	seen := make(map[int]bool, len(applied))
	for _, a := range applied {
		seen[a.Version] = true
	}
	var pending []Migration
	for _, m := range all {
		if !seen[m.Version] {
			pending = append(pending, m)
		}
	}
	return pending, nil
}
