package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ReplayStore persists the record of every market replay.
//
// See migrations/0013_replay_runs.sql for why the record is durable rather
// than a field on an in-memory engine: two runs cannot be compared if neither
// survives the process that produced it.
type ReplayStore struct{ pool querier }

// ReplayRun is one recorded replay.
//
// Deliberately a plain struct with no dependency on internal/replay. The
// replay package sits above marketdata, which sits above this one, so a type
// imported the other way would close an import cycle. The engine hands its
// record to an application-supplied function, which maps it to this.
type ReplayRun struct {
	ID          uuid.UUID
	DatasetID   string
	DatasetHash string
	CodeSHA     string
	ConfigHash  string
	Seed        int64

	// FromTime and ToTime are dataset time.
	FromTime time.Time
	ToTime   time.Time
	// StartedAt and FinishedAt are wall time.
	StartedAt  time.Time
	FinishedAt *time.Time

	State         string
	Steps         int
	BarsProcessed int
	StepErrors    int
	Failure       string
	Warnings      []string
}

// ErrReplayRunIncomplete means a caller tried to record a run that could not
// serve as evidence.
//
// Refused rather than stored with blanks. A row missing its dataset hash or
// its code SHA cannot answer "was this the same code against the same data",
// which is the only reason the table exists -- and a half-filled row is worse
// than no row, because it looks like a record.
var ErrReplayRunIncomplete = errors.New("store: a replay run record needs a dataset, a dataset hash and a code SHA")

// RecordRun inserts or updates one run.
//
// An upsert because a run is written at least twice: once when it starts, so
// an interrupted replay still leaves a trace, and again as it progresses and
// finishes. The identity columns are written once and then rewritten with the
// same values; the counters and the state are what actually move.
func (s *ReplayStore) RecordRun(ctx context.Context, run ReplayRun) error {
	if run.ID == uuid.Nil ||
		strings.TrimSpace(run.DatasetID) == "" ||
		strings.TrimSpace(run.DatasetHash) == "" ||
		strings.TrimSpace(run.CodeSHA) == "" {
		return ErrReplayRunIncomplete
	}
	// Never nil: a NULL would violate the column, and "no warnings" and
	// "warnings not recorded" must not become the same value.
	warnings := run.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	_, err := s.pool.Exec(ctx, `
        INSERT INTO replay_runs (
            id, dataset_id, dataset_hash, code_sha, config_hash, seed,
            from_time, to_time, started_at, finished_at,
            state, steps, bars_processed, step_errors, failure, warnings)
        VALUES ($1, $2, $3, $4, $5, $6,
                $7, $8, $9, $10,
                $11, $12, $13, $14, $15, $16)
        ON CONFLICT (id) DO UPDATE SET
            finished_at    = EXCLUDED.finished_at,
            state          = EXCLUDED.state,
            steps          = EXCLUDED.steps,
            bars_processed = EXCLUDED.bars_processed,
            step_errors    = EXCLUDED.step_errors,
            failure        = EXCLUDED.failure,
            warnings       = EXCLUDED.warnings`,
		run.ID, run.DatasetID, run.DatasetHash, run.CodeSHA, run.ConfigHash, run.Seed,
		run.FromTime.UTC(), run.ToTime.UTC(), run.StartedAt.UTC(), run.FinishedAt,
		run.State, run.Steps, run.BarsProcessed, run.StepErrors, run.Failure, warnings)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// Run loads one recorded run.
func (s *ReplayStore) Run(ctx context.Context, id uuid.UUID) (ReplayRun, error) {
	rows, err := s.query(ctx, `
        SELECT id, dataset_id, dataset_hash, code_sha, config_hash, seed,
               from_time, to_time, started_at, finished_at,
               state, steps, bars_processed, step_errors, failure, warnings
        FROM replay_runs WHERE id = $1`, id)
	if err != nil {
		return ReplayRun{}, err
	}
	if len(rows) == 0 {
		return ReplayRun{}, ErrNotFound
	}
	return rows[0], nil
}

// Runs lists recorded runs, newest first, optionally for one dataset.
//
// An empty datasetID means every dataset. The filter is a parameter and never
// interpolated, so an unknown dataset name is an empty result rather than a
// query.
func (s *ReplayStore) Runs(ctx context.Context, datasetID string, limit int) ([]ReplayRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return s.query(ctx, `
        SELECT id, dataset_id, dataset_hash, code_sha, config_hash, seed,
               from_time, to_time, started_at, finished_at,
               state, steps, bars_processed, step_errors, failure, warnings
        FROM replay_runs
        WHERE ($1 = '' OR dataset_id = $1)
        ORDER BY started_at DESC, id
        LIMIT $2`, datasetID, limit)
}

func (s *ReplayStore) query(ctx context.Context, sql string, args ...any) ([]ReplayRun, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	out := []ReplayRun{}
	for rows.Next() {
		var r ReplayRun
		if serr := rows.Scan(&r.ID, &r.DatasetID, &r.DatasetHash, &r.CodeSHA,
			&r.ConfigHash, &r.Seed, &r.FromTime, &r.ToTime, &r.StartedAt,
			&r.FinishedAt, &r.State, &r.Steps, &r.BarsProcessed, &r.StepErrors,
			&r.Failure, &r.Warnings); serr != nil {
			return nil, mapError(serr)
		}
		out = append(out, r)
	}
	if rows.Err() != nil {
		return nil, mapError(rows.Err())
	}
	return out, nil
}
