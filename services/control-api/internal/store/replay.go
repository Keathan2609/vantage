package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
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

	// The declared window. Dataset time, and nil when a run declared none --
	// which a run recorded before the column existed genuinely did.
	WarmupStart        *time.Time
	EvaluationStart    *time.Time
	EvaluationEnd      *time.Time
	AllowWarmupTrading bool

	// The account as it stood when the run began. A run that started with an
	// open position and a drawn-down balance behaves differently from one that
	// started flat, and neither the dataset hash nor the code SHA records it.
	StartingBalance   *decimal.Decimal
	StartingCurrency  string
	StartingPositions int
	// StartingStateCapture distinguishes "the account was flat" from "nobody
	// could read the account". StartingPositions is an int, so both recorded 0
	// and a research run could not tell which it had.
	StartingStateCapture string
	StartingStateError   string

	// What was in force. Digests rather than documents: enough to detect that
	// a comparison between two runs is invalid, which is the question they
	// exist to answer.
	RiskConfigHash      string
	AuthorityConfigHash string
	CorrelationPolicy   string
	RegimePolicy        string
	// StrategyVersions and ModelVersions are JSON arrays. Readable directly
	// because "which strategies ran" is asked of the record itself, and a hash
	// would send the reader elsewhere to find out.
	StrategyVersions []byte
	ModelVersions    []byte
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
            state, steps, bars_processed, step_errors, failure, warnings,
            warmup_start, evaluation_start, evaluation_end, allow_warmup_trading,
            starting_balance, starting_currency, starting_positions,
            starting_state_capture, starting_state_error,
            risk_config_hash, authority_config_hash, correlation_policy,
            regime_policy, strategy_versions, model_versions)
        VALUES ($1, $2, $3, $4, $5, $6,
                $7, $8, $9, $10,
                $11, $12, $13, $14, $15, $16,
                $17, $18, $19, $20,
                $21, $22, $23,
                $24, $25,
                $26, $27, $28,
                $29, $30, $31)
        ON CONFLICT (id) DO UPDATE SET
            finished_at    = EXCLUDED.finished_at,
            state          = EXCLUDED.state,
            steps          = EXCLUDED.steps,
            bars_processed = EXCLUDED.bars_processed,
            step_errors    = EXCLUDED.step_errors,
            failure        = EXCLUDED.failure,
            warnings       = EXCLUDED.warnings,
            -- The declared inputs are immutable within a run, so rewriting
            -- them with the same values is harmless -- and it makes the
            -- record self-healing if the first write could not gather them.
            warmup_start          = EXCLUDED.warmup_start,
            evaluation_start      = EXCLUDED.evaluation_start,
            evaluation_end        = EXCLUDED.evaluation_end,
            allow_warmup_trading  = EXCLUDED.allow_warmup_trading,
            starting_balance      = EXCLUDED.starting_balance,
            starting_currency     = EXCLUDED.starting_currency,
            starting_positions    = EXCLUDED.starting_positions,
            starting_state_capture = EXCLUDED.starting_state_capture,
            starting_state_error   = EXCLUDED.starting_state_error,
            risk_config_hash      = EXCLUDED.risk_config_hash,
            authority_config_hash = EXCLUDED.authority_config_hash,
            correlation_policy    = EXCLUDED.correlation_policy,
            regime_policy         = EXCLUDED.regime_policy,
            strategy_versions     = EXCLUDED.strategy_versions,
            model_versions        = EXCLUDED.model_versions`,
		run.ID, run.DatasetID, run.DatasetHash, run.CodeSHA, run.ConfigHash, run.Seed,
		run.FromTime.UTC(), run.ToTime.UTC(), run.StartedAt.UTC(), run.FinishedAt,
		run.State, run.Steps, run.BarsProcessed, run.StepErrors, run.Failure, warnings,
		run.WarmupStart, run.EvaluationStart, run.EvaluationEnd, run.AllowWarmupTrading,
		run.StartingBalance, run.StartingCurrency, run.StartingPositions,
		captureOrNotAttempted(run.StartingStateCapture), run.StartingStateError,
		run.RiskConfigHash, run.AuthorityConfigHash, run.CorrelationPolicy,
		run.RegimePolicy, jsonOrEmptyArray(run.StrategyVersions),
		jsonOrEmptyArray(run.ModelVersions))
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
               state, steps, bars_processed, step_errors, failure, warnings,
               warmup_start, evaluation_start, evaluation_end, allow_warmup_trading,
               starting_balance, starting_currency, starting_positions,
               starting_state_capture, starting_state_error,
               risk_config_hash, authority_config_hash, correlation_policy,
               regime_policy, strategy_versions, model_versions
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
               state, steps, bars_processed, step_errors, failure, warnings,
               warmup_start, evaluation_start, evaluation_end, allow_warmup_trading,
               starting_balance, starting_currency, starting_positions,
               starting_state_capture, starting_state_error,
               risk_config_hash, authority_config_hash, correlation_policy,
               regime_policy, strategy_versions, model_versions
        FROM replay_runs
        WHERE ($1 = '' OR dataset_id = $1)
        ORDER BY started_at DESC, id
        LIMIT $2`, datasetID, limit)
}

// MarkInterruptedRuns closes the books on runs whose process died.
//
// Called once at boot. A row that claims to be running while nothing is
// running is worse than no row: it is a record that contradicts reality, and
// the next person to read it will believe it.
//
// The reason is recorded in `failure` rather than in a log, because a log
// rotates and this fact belongs to the run.
func (s *ReplayStore) MarkInterruptedRuns(ctx context.Context, reason string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE replay_runs
		SET state = 'interrupted',
		    failure = CASE WHEN failure = '' THEN $1 ELSE failure END,
		    finished_at = COALESCE(finished_at, now())
		WHERE state IN ('running', 'paused', 'idle')`, reason)
	if err != nil {
		return 0, mapError(err)
	}
	return tag.RowsAffected(), nil
}

// InterruptedRuns lists runs that stopped without finishing, newest first.
//
// Bounded, and ordered so the most recent interruption -- the one an operator
// is most likely to be asking about -- is first.
func (s *ReplayStore) InterruptedRuns(ctx context.Context, limit int) ([]ReplayRun, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	return s.query(ctx, `
        SELECT id, dataset_id, dataset_hash, code_sha, config_hash, seed,
               from_time, to_time, started_at, finished_at,
               state, steps, bars_processed, step_errors, failure, warnings,
               warmup_start, evaluation_start, evaluation_end, allow_warmup_trading,
               starting_balance, starting_currency, starting_positions,
               starting_state_capture, starting_state_error,
               risk_config_hash, authority_config_hash, correlation_policy,
               regime_policy, strategy_versions, model_versions
        FROM replay_runs
        WHERE state = 'interrupted'
        ORDER BY started_at DESC
        LIMIT $1`, limit)
}

// jsonOrEmptyArray keeps the jsonb columns valid. A nil slice would be a NULL
// into a NOT NULL column, and `{}` would fail to unmarshal into a list.
func jsonOrEmptyArray(b []byte) []byte {
	if len(b) == 0 {
		return []byte(`[]`)
	}
	return b
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
			&r.Failure, &r.Warnings,
			&r.WarmupStart, &r.EvaluationStart, &r.EvaluationEnd, &r.AllowWarmupTrading,
			&r.StartingBalance, &r.StartingCurrency, &r.StartingPositions,
			&r.StartingStateCapture, &r.StartingStateError,
			&r.RiskConfigHash, &r.AuthorityConfigHash, &r.CorrelationPolicy,
			&r.RegimePolicy, &r.StrategyVersions, &r.ModelVersions); serr != nil {
			return nil, mapError(serr)
		}
		out = append(out, r)
	}
	if rows.Err() != nil {
		return nil, mapError(rows.Err())
	}
	return out, nil
}

// captureOrNotAttempted keeps the CHECK constraint satisfied.
//
// An empty status is what a caller that never set one produces, and the column
// permits only the three named states. NOT_ATTEMPTED is the right substitute
// because it means exactly what an unset status means: nobody looked.
func captureOrNotAttempted(status string) string {
	switch status {
	case "CAPTURED", "CAPTURE_FAILED", "NOT_ATTEMPTED":
		return status
	default:
		return "NOT_ATTEMPTED"
	}
}
