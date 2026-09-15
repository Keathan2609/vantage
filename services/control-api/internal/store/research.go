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
)

// ResearchStore persists research-plane records.
//
// The control plane is the only writer to the database. The quant service
// computes and returns results; the control plane validates and stores them.
// That keeps research on a read-only database role and leaves one component
// responsible for what enters the system of record.
type ResearchStore struct{ pool *db.Pool }

// ---------------------------------------------------------------------------
// Strategies
// ---------------------------------------------------------------------------

// UpsertStrategy registers a strategy definition.
func (s *ResearchStore) UpsertStrategy(ctx context.Context, st domain.Strategy) (domain.Strategy, error) {
	var out domain.Strategy
	err := s.pool.QueryRow(ctx, `
		INSERT INTO strategies (key, name, family, description, high_risk, enabled, owner_user_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (key) DO UPDATE SET
			name = EXCLUDED.name, family = EXCLUDED.family,
			description = EXCLUDED.description, high_risk = EXCLUDED.high_risk,
			updated_at = now()
		RETURNING id, key, name, family, description, high_risk, enabled, owner_user_id, created_at, updated_at`,
		st.Key, st.Name, st.Family, st.Description, st.HighRisk, st.Enabled, st.OwnerUserID).
		Scan(&out.ID, &out.Key, &out.Name, &out.Family, &out.Description, &out.HighRisk,
			&out.Enabled, &out.OwnerUserID, &out.CreatedAt, &out.UpdatedAt)
	return out, mapError(err)
}

// ListStrategies returns strategy definitions.
func (s *ResearchStore) ListStrategies(ctx context.Context) ([]domain.Strategy, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, key, name, family, description, high_risk, enabled, owner_user_id, created_at, updated_at
		FROM strategies ORDER BY family, name`)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []domain.Strategy
	for rows.Next() {
		var st domain.Strategy
		var owner *uuid.UUID
		if err := rows.Scan(&st.ID, &st.Key, &st.Name, &st.Family, &st.Description,
			&st.HighRisk, &st.Enabled, &owner, &st.CreatedAt, &st.UpdatedAt); err != nil {
			return nil, mapError(err)
		}
		if owner != nil {
			st.OwnerUserID = *owner
		}
		out = append(out, st)
	}
	return out, mapError(rows.Err())
}

// Strategy loads one strategy by id.
func (s *ResearchStore) Strategy(ctx context.Context, id uuid.UUID) (domain.Strategy, error) {
	var st domain.Strategy
	var owner *uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT id, key, name, family, description, high_risk, enabled, owner_user_id, created_at, updated_at
		FROM strategies WHERE id = $1`, id).
		Scan(&st.ID, &st.Key, &st.Name, &st.Family, &st.Description, &st.HighRisk,
			&st.Enabled, &owner, &st.CreatedAt, &st.UpdatedAt)
	if err != nil {
		return domain.Strategy{}, mapError(err)
	}
	if owner != nil {
		st.OwnerUserID = *owner
	}
	return st, nil
}

// StrategyByKey loads one strategy by its stable key.
func (s *ResearchStore) StrategyByKey(ctx context.Context, key string) (domain.Strategy, error) {
	var st domain.Strategy
	var owner *uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT id, key, name, family, description, high_risk, enabled, owner_user_id, created_at, updated_at
		FROM strategies WHERE key = $1`, key).
		Scan(&st.ID, &st.Key, &st.Name, &st.Family, &st.Description, &st.HighRisk,
			&st.Enabled, &owner, &st.CreatedAt, &st.UpdatedAt)
	if err != nil {
		return domain.Strategy{}, mapError(err)
	}
	if owner != nil {
		st.OwnerUserID = *owner
	}
	return st, nil
}

// SetStrategyEnabled enables or disables a strategy.
//
// A high-risk research strategy cannot be enabled through this path: the
// database constraint refuses the row, and the refusal is surfaced as an
// explicit error rather than a silent no-op.
func (s *ResearchStore) SetStrategyEnabled(ctx context.Context, id uuid.UUID, enabled bool) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE strategies SET enabled = $2, updated_at = now() WHERE id = $1`, id, enabled)
	if err != nil {
		mapped := mapError(err)
		if IsConstraint(mapped, "strategies_high_risk_default_off_ck") {
			return fmt.Errorf("%w: high-risk research strategies cannot be enabled for execution", mapped)
		}
		return mapped
	}
	return nil
}

// UpsertStrategyVersion records a parameterised, code-identified version.
func (s *ResearchStore) UpsertStrategyVersion(ctx context.Context, v domain.StrategyVersion) (domain.StrategyVersion, error) {
	var out domain.StrategyVersion
	err := s.pool.QueryRow(ctx, `
		INSERT INTO strategy_versions (strategy_id, version, code_hash, git_sha, parameters,
			timeframe, instruments, lifecycle, valid_regimes, notes, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (strategy_id, version) DO UPDATE SET
			code_hash = EXCLUDED.code_hash, git_sha = EXCLUDED.git_sha,
			parameters = EXCLUDED.parameters, timeframe = EXCLUDED.timeframe,
			instruments = EXCLUDED.instruments, valid_regimes = EXCLUDED.valid_regimes,
			notes = EXCLUDED.notes
		RETURNING id, strategy_id, version, code_hash, git_sha, parameters, timeframe,
		          instruments, lifecycle, valid_regimes, notes, created_at, promoted_at, retired_at`,
		v.StrategyID, v.Version, v.CodeHash, v.GitSHA, v.Parameters, string(v.Timeframe),
		v.Instruments, string(v.Lifecycle), v.ValidRegimes, v.Notes, nullUUID(v.CreatedBy)).
		Scan(&out.ID, &out.StrategyID, &out.Version, &out.CodeHash, &out.GitSHA, &out.Parameters,
			&out.Timeframe, &out.Instruments, &out.Lifecycle, &out.ValidRegimes, &out.Notes,
			&out.CreatedAt, &out.PromotedAt, &out.RetiredAt)
	return out, mapError(err)
}

// StrategyVersion loads one version.
func (s *ResearchStore) StrategyVersion(ctx context.Context, strategyID uuid.UUID, version int) (domain.StrategyVersion, error) {
	var v domain.StrategyVersion
	err := s.pool.QueryRow(ctx, `
		SELECT id, strategy_id, version, code_hash, git_sha, parameters, timeframe,
		       instruments, lifecycle, valid_regimes, notes, created_at, promoted_at, retired_at
		FROM strategy_versions WHERE strategy_id = $1 AND version = $2`, strategyID, version).
		Scan(&v.ID, &v.StrategyID, &v.Version, &v.CodeHash, &v.GitSHA, &v.Parameters,
			&v.Timeframe, &v.Instruments, &v.Lifecycle, &v.ValidRegimes, &v.Notes,
			&v.CreatedAt, &v.PromotedAt, &v.RetiredAt)
	return v, mapError(err)
}

// LatestStrategyVersion loads the highest-numbered version.
func (s *ResearchStore) LatestStrategyVersion(ctx context.Context, strategyID uuid.UUID) (domain.StrategyVersion, error) {
	var v domain.StrategyVersion
	err := s.pool.QueryRow(ctx, `
		SELECT id, strategy_id, version, code_hash, git_sha, parameters, timeframe,
		       instruments, lifecycle, valid_regimes, notes, created_at, promoted_at, retired_at
		FROM strategy_versions WHERE strategy_id = $1 ORDER BY version DESC LIMIT 1`, strategyID).
		Scan(&v.ID, &v.StrategyID, &v.Version, &v.CodeHash, &v.GitSHA, &v.Parameters,
			&v.Timeframe, &v.Instruments, &v.Lifecycle, &v.ValidRegimes, &v.Notes,
			&v.CreatedAt, &v.PromotedAt, &v.RetiredAt)
	return v, mapError(err)
}

// ListStrategyVersions returns a strategy's versions, newest first.
func (s *ResearchStore) ListStrategyVersions(ctx context.Context, strategyID uuid.UUID) ([]domain.StrategyVersion, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, strategy_id, version, code_hash, git_sha, parameters, timeframe,
		       instruments, lifecycle, valid_regimes, notes, created_at, promoted_at, retired_at
		FROM strategy_versions WHERE strategy_id = $1 ORDER BY version DESC`, strategyID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []domain.StrategyVersion
	for rows.Next() {
		var v domain.StrategyVersion
		if err := rows.Scan(&v.ID, &v.StrategyID, &v.Version, &v.CodeHash, &v.GitSHA,
			&v.Parameters, &v.Timeframe, &v.Instruments, &v.Lifecycle, &v.ValidRegimes,
			&v.Notes, &v.CreatedAt, &v.PromotedAt, &v.RetiredAt); err != nil {
			return nil, mapError(err)
		}
		out = append(out, v)
	}
	return out, mapError(rows.Err())
}

// PromoteStrategyVersion advances a version's lifecycle with its evidence.
//
// The legality of the move is decided by domain.CanPromote before this is
// called, and the database's own ceiling constraint refuses anything past
// PAPER regardless.
func (s *ResearchStore) PromoteStrategyVersion(ctx context.Context, strategyID uuid.UUID, version int,
	to domain.StrategyLifecycle, evidence any, actor uuid.UUID, reason string) error {

	evJSON, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("store: marshal promotion evidence: %w", err)
	}
	return s.pool.InTx(ctx, func(tx pgx.Tx) error {
		var from string
		if err := tx.QueryRow(ctx, `
			UPDATE strategy_versions
			SET lifecycle = $3,
			    promoted_at = CASE WHEN $3 <> 'RETIRED' THEN now() ELSE promoted_at END,
			    retired_at = CASE WHEN $3 = 'RETIRED' THEN now() ELSE retired_at END
			WHERE strategy_id = $1 AND version = $2
			RETURNING (SELECT lifecycle FROM strategy_versions WHERE strategy_id = $1 AND version = $2)`,
			strategyID, version, string(to)).Scan(&from); err != nil {
			return mapError(err)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO strategy_lifecycle_history
				(strategy_id, version, from_state, to_state, evidence, changed_by, reason)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			strategyID, version, from, string(to), evJSON, actor, reason)
		return mapError(err)
	})
}

// ---------------------------------------------------------------------------
// Runs and signals
// ---------------------------------------------------------------------------

// CreateStrategyRunTx records a strategy evaluation.
//
// Returns ErrDuplicateCommand when a run already exists for this bar, which is
// the guard against a scheduler evaluating the same bar twice and emitting
// duplicate signals.
func (s *ResearchStore) CreateStrategyRunTx(ctx context.Context, tx pgx.Tx, r domain.StrategyRun) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO strategy_runs (strategy_id, strategy_version, account_id, instrument_id,
			timeframe, bar_time, status, skip_reason, error, duration_ms, finished_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now())
		ON CONFLICT DO NOTHING
		RETURNING id`,
		r.StrategyID, r.StrategyVersion, r.AccountID, r.InstrumentID, string(r.Timeframe),
		nullTime(r.FinishedAt), r.Status, r.SkipReason, r.Error, r.DurationMS).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// This bar was already evaluated: the per-bar guard did its job.
		return uuid.Nil, ErrDuplicateCommand
	}
	if err != nil {
		return uuid.Nil, mapError(err)
	}
	return id, nil
}

// CreateStrategyRunForBarTx records a run keyed to a specific bar time.
func (s *ResearchStore) CreateStrategyRunForBarTx(ctx context.Context, tx pgx.Tx, r domain.StrategyRun, barTime time.Time) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO strategy_runs (strategy_id, strategy_version, account_id, instrument_id,
			timeframe, bar_time, status, skip_reason, error, duration_ms, finished_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now())
		ON CONFLICT DO NOTHING
		RETURNING id`,
		r.StrategyID, r.StrategyVersion, r.AccountID, r.InstrumentID, string(r.Timeframe),
		barTime, r.Status, r.SkipReason, r.Error, r.DurationMS).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// This bar was already evaluated: the per-bar guard did its job.
		return uuid.Nil, ErrDuplicateCommand
	}
	if err != nil {
		return uuid.Nil, mapError(err)
	}
	return id, nil
}

// CreateSignalTx records a strategy's opinion.
func (s *ResearchStore) CreateSignalTx(ctx context.Context, tx pgx.Tx, sig domain.StrategySignal) (uuid.UUID, error) {
	features := sig.Features
	if features == nil {
		features = json.RawMessage(`{}`)
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO strategy_signals (run_id, strategy_id, strategy_version, account_id,
			instrument_id, timeframe, action, confidence, suggested_stop, suggested_target,
			explanation, features, bar_time)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING id`,
		sig.RunID, sig.StrategyID, sig.StrategyVersion, sig.AccountID, sig.InstrumentID,
		string(sig.Timeframe), string(sig.Action), sig.Confidence, sig.SuggestedStop,
		sig.SuggestedTarget, sig.Explanation, features, sig.BarTime).Scan(&id)
	return id, mapError(err)
}

// RecentSignals returns recent signals, newest first.
func (s *ResearchStore) RecentSignals(ctx context.Context, accountID *uuid.UUID, limit int) ([]domain.StrategySignal, error) {
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	q := `SELECT id, run_id, strategy_id, strategy_version, account_id, instrument_id, timeframe,
	             action, confidence, suggested_stop, suggested_target, explanation, features,
	             bar_time, generated_at
	      FROM strategy_signals`
	args := []any{limit}
	if accountID != nil {
		args = append(args, *accountID)
		q += ` WHERE account_id = $2 OR account_id IS NULL`
	}
	q += ` ORDER BY generated_at DESC LIMIT $1`

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []domain.StrategySignal
	for rows.Next() {
		var sig domain.StrategySignal
		var raw []byte
		if err := rows.Scan(&sig.ID, &sig.RunID, &sig.StrategyID, &sig.StrategyVersion,
			&sig.AccountID, &sig.InstrumentID, &sig.Timeframe, &sig.Action, &sig.Confidence,
			&sig.SuggestedStop, &sig.SuggestedTarget, &sig.Explanation, &raw,
			&sig.BarTime, &sig.GeneratedAt); err != nil {
			return nil, mapError(err)
		}
		sig.Features = json.RawMessage(raw)
		out = append(out, sig)
	}
	return out, mapError(rows.Err())
}

// StrategyRunSummary aggregates a strategy's recent activity for the UI.
type StrategyRunSummary struct {
	StrategyID   uuid.UUID
	Runs         int
	Signals      int
	Failures     int
	LastRunAt    *time.Time
	LastSignalAt *time.Time
}

// StrategyActivity summarises runs and signals per strategy.
func (s *ResearchStore) StrategyActivity(ctx context.Context, since time.Time) (map[uuid.UUID]StrategyRunSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.strategy_id,
		       count(*) AS runs,
		       count(*) FILTER (WHERE r.status = 'succeeded') AS signals,
		       count(*) FILTER (WHERE r.status = 'failed') AS failures,
		       max(r.started_at) AS last_run,
		       (SELECT max(generated_at) FROM strategy_signals sg WHERE sg.strategy_id = r.strategy_id) AS last_signal
		FROM strategy_runs r
		WHERE r.started_at >= $1
		GROUP BY r.strategy_id`, since)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]StrategyRunSummary{}
	for rows.Next() {
		var sum StrategyRunSummary
		if err := rows.Scan(&sum.StrategyID, &sum.Runs, &sum.Signals, &sum.Failures,
			&sum.LastRunAt, &sum.LastSignalAt); err != nil {
			return nil, mapError(err)
		}
		out[sum.StrategyID] = sum
	}
	return out, mapError(rows.Err())
}

// ---------------------------------------------------------------------------
// Decision snapshots
// ---------------------------------------------------------------------------

// CreateDecisionTx records why an intent existed and what became of it.
func (s *ResearchStore) CreateDecisionTx(ctx context.Context, tx pgx.Tx, d domain.DecisionSnapshot) (uuid.UUID, error) {
	orEmpty := func(r json.RawMessage) json.RawMessage {
		if r == nil {
			return json.RawMessage(`{}`)
		}
		return r
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO decision_snapshots (account_id, strategy_id, strategy_version, model_id,
			model_version, instrument_id, bar_time, quote, indicators, features, event_context,
			portfolio_context, risk_state, authority_state, market_data_health, signal_action,
			confidence, requested_quantity, approved_quantity, outcome, outcome_code, outcome_reason,
			regime, regime_policy_version, regime_reasons, consensus)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,
			$23,$24,$25,$26)
		RETURNING id`,
		d.AccountID, d.StrategyID, d.StrategyVersion, d.ModelID, d.ModelVersion,
		d.InstrumentID, d.BarTime, orEmpty(d.Quote), orEmpty(d.Indicators), orEmpty(d.Features),
		orEmpty(d.EventContext), orEmpty(d.PortfolioContext), orEmpty(d.RiskState),
		orEmpty(d.AuthorityState), orEmpty(d.MarketDataHealth), string(d.SignalAction),
		d.Confidence, d.RequestedQty, d.ApprovedQty, d.Outcome, d.OutcomeCode, d.OutcomeReason,
		regimeOrUnknown(d.Regime), d.RegimePolicyVersion, orEmptyList(d.RegimeReasons),
		orEmpty(d.Consensus)).
		Scan(&id)
	return id, mapError(err)
}

// regimeOrUnknown keeps the CHECK constraint satisfied.
//
// A zero-value Regime is the empty string, which is not one of the seven
// labels. UNKNOWN is the right substitute because it means exactly what an
// unset regime means: this market was not characterised.
func regimeOrUnknown(r domain.Regime) string {
	if _, ok := domain.ParseRegime(string(r)); !ok {
		return string(domain.RegimeUnknown)
	}
	return string(r)
}

// orEmptyList defaults a reasons array, which is a LIST and not an object --
// `{}` would fail to unmarshal into a slice on the way back out.
func orEmptyList(r json.RawMessage) json.RawMessage {
	if len(r) == 0 {
		return json.RawMessage(`[]`)
	}
	return r
}

// PurgeStrategyRunsInRange removes the per-bar evaluation watermark for a
// window.
//
// # Why a replay has to do this
//
// A strategy is evaluated once per completed bar and that fact is recorded in
// strategy_runs, which is what stops the scheduler double-counting one
// strategy's opinion. A replay's market-data purge does not touch it --
// strategy_runs is research history, not market data -- so a SECOND replay of
// the same dataset finds every bar already evaluated and produces nothing at
// all, and the failures read as a broken pipeline.
//
// Measured before this existed: eight of nine scenarios recorded zero strategy
// runs on a second pass.
//
// Bounded by the window the caller declares, so it removes the runs belonging
// to the dataset about to be replayed and nothing else. A replay whose dates
// overlap real research would still remove those rows, which is why the bound
// is the dataset's own span and the operation is development-only.
func (s *ResearchStore) PurgeStrategyRunsInRange(ctx context.Context,
	from, to time.Time) (int64, error) {

	tag, err := s.pool.Exec(ctx, `
		DELETE FROM strategy_runs
		WHERE bar_time IS NOT NULL AND bar_time >= $1 AND bar_time <= $2`,
		from.UTC(), to.UTC())
	if err != nil {
		return 0, mapError(err)
	}
	return tag.RowsAffected(), nil
}

// DecisionSummary is a compact view for the activity timeline.
type DecisionSummary struct {
	ID            uuid.UUID
	InstrumentID  string
	StrategyID    *uuid.UUID
	SignalAction  string
	Confidence    *decimal.Decimal
	RequestedQty  *decimal.Decimal
	ApprovedQty   *decimal.Decimal
	Outcome       string
	OutcomeCode   *string
	OutcomeReason *string
	CreatedAt     time.Time
}

// ListDecisions returns recent decisions for an account.
func (s *ResearchStore) ListDecisions(ctx context.Context, accountID uuid.UUID, limit int) ([]DecisionSummary, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, instrument_id, strategy_id, signal_action, confidence, requested_quantity,
		       approved_quantity, outcome, outcome_code, outcome_reason, created_at
		FROM decision_snapshots WHERE account_id = $1 ORDER BY created_at DESC LIMIT $2`,
		accountID, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []DecisionSummary
	for rows.Next() {
		var d DecisionSummary
		if err := rows.Scan(&d.ID, &d.InstrumentID, &d.StrategyID, &d.SignalAction,
			&d.Confidence, &d.RequestedQty, &d.ApprovedQty, &d.Outcome, &d.OutcomeCode,
			&d.OutcomeReason, &d.CreatedAt); err != nil {
			return nil, mapError(err)
		}
		out = append(out, d)
	}
	return out, mapError(rows.Err())
}

// Decision returns one decision snapshot in full.
func (s *ResearchStore) Decision(ctx context.Context, accountID, id uuid.UUID) (domain.DecisionSnapshot, error) {
	var d domain.DecisionSnapshot
	err := s.pool.QueryRow(ctx, `
		SELECT id, account_id, strategy_id, strategy_version, model_id, model_version,
		       instrument_id, bar_time, quote, indicators, features, event_context,
		       portfolio_context, risk_state, authority_state, market_data_health,
		       signal_action, confidence, requested_quantity, approved_quantity,
		       outcome, outcome_code, outcome_reason, consensus, created_at
		FROM decision_snapshots WHERE id = $1 AND account_id = $2`, id, accountID).
		Scan(&d.ID, &d.AccountID, &d.StrategyID, &d.StrategyVersion, &d.ModelID, &d.ModelVersion,
			&d.InstrumentID, &d.BarTime, &d.Quote, &d.Indicators, &d.Features, &d.EventContext,
			&d.PortfolioContext, &d.RiskState, &d.AuthorityState, &d.MarketDataHealth,
			&d.SignalAction, &d.Confidence, &d.RequestedQty, &d.ApprovedQty,
			&d.Outcome, &d.OutcomeCode, &d.OutcomeReason, &d.Consensus, &d.CreatedAt)
	return d, mapError(err)
}

// ---------------------------------------------------------------------------
// Backtests
// ---------------------------------------------------------------------------

// Backtest is a stored evaluation run.
type Backtest struct {
	ID              uuid.UUID
	StrategyID      uuid.UUID
	StrategyVersion int
	InstrumentID    string
	Timeframe       string
	DatasetHash     string
	PeriodStart     time.Time
	PeriodEnd       time.Time
	SampleKind      string
	InitialCapital  decimal.Decimal
	Currency        string
	Parameters      json.RawMessage
	CostModel       json.RawMessage
	Frictionless    bool
	Metrics         json.RawMessage
	EquityCurve     json.RawMessage
	Warnings        []string
	CodeHash        string
	Seed            *int64
	Status          string
	CreatedBy       *uuid.UUID
	StartedAt       time.Time
	FinishedAt      *time.Time
}

// BacktestTrade is one simulated round trip.
type BacktestTrade struct {
	InstrumentID string
	Side         string
	Quantity     decimal.Decimal
	EntryTime    time.Time
	EntryPrice   decimal.Decimal
	ExitTime     *time.Time
	ExitPrice    *decimal.Decimal
	GrossPnL     decimal.Decimal
	Commission   decimal.Decimal
	Slippage     decimal.Decimal
	Swap         decimal.Decimal
	NetPnL       decimal.Decimal
	MAE          *decimal.Decimal
	MFE          *decimal.Decimal
	ExitReason   *string
}

// SaveBacktest stores a run and its trades in one transaction.
func (s *ResearchStore) SaveBacktest(ctx context.Context, b Backtest, trades []BacktestTrade) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO backtests (strategy_id, strategy_version, instrument_id, timeframe,
				dataset_hash, period_start, period_end, sample_kind, initial_capital, currency,
				parameters, cost_model, frictionless, metrics, equity_curve, warnings,
				code_hash, seed, status, created_by, finished_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,now())
			RETURNING id`,
			b.StrategyID, b.StrategyVersion, b.InstrumentID, b.Timeframe, b.DatasetHash,
			b.PeriodStart, b.PeriodEnd, b.SampleKind, b.InitialCapital, b.Currency,
			b.Parameters, b.CostModel, b.Frictionless, b.Metrics, b.EquityCurve, b.Warnings,
			b.CodeHash, b.Seed, b.Status, b.CreatedBy).Scan(&id); err != nil {
			return mapError(err)
		}
		if len(trades) == 0 {
			return nil
		}
		batch := &pgx.Batch{}
		for _, t := range trades {
			batch.Queue(`
				INSERT INTO backtest_trades (backtest_id, instrument_id, side, quantity,
					entry_time, entry_price, exit_time, exit_price, gross_pnl, commission,
					slippage, swap, net_pnl, mae, mfe, exit_reason)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
				id, t.InstrumentID, t.Side, t.Quantity, t.EntryTime, t.EntryPrice,
				t.ExitTime, t.ExitPrice, t.GrossPnL, t.Commission, t.Slippage, t.Swap,
				t.NetPnL, t.MAE, t.MFE, t.ExitReason)
		}
		res := tx.SendBatch(ctx, batch)
		defer res.Close()
		for range trades {
			if _, err := res.Exec(); err != nil {
				return mapError(err)
			}
		}
		return nil
	})
	return id, err
}

const backtestColumns = `id, strategy_id, strategy_version, instrument_id, timeframe, dataset_hash,
	period_start, period_end, sample_kind, initial_capital, currency, parameters, cost_model,
	frictionless, metrics, equity_curve, warnings, code_hash, seed, status, created_by,
	started_at, finished_at`

func scanBacktest(row pgx.Row) (Backtest, error) {
	var b Backtest
	err := row.Scan(&b.ID, &b.StrategyID, &b.StrategyVersion, &b.InstrumentID, &b.Timeframe,
		&b.DatasetHash, &b.PeriodStart, &b.PeriodEnd, &b.SampleKind, &b.InitialCapital,
		&b.Currency, &b.Parameters, &b.CostModel, &b.Frictionless, &b.Metrics, &b.EquityCurve,
		&b.Warnings, &b.CodeHash, &b.Seed, &b.Status, &b.CreatedBy, &b.StartedAt, &b.FinishedAt)
	return b, mapError(err)
}

// ListBacktests returns recent runs, newest first.
func (s *ResearchStore) ListBacktests(ctx context.Context, strategyID *uuid.UUID, limit int) ([]Backtest, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT ` + backtestColumns + ` FROM backtests`
	args := []any{limit}
	if strategyID != nil {
		args = append(args, *strategyID)
		q += ` WHERE strategy_id = $2`
	}
	q += ` ORDER BY started_at DESC LIMIT $1`

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Backtest
	for rows.Next() {
		b, err := scanBacktest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, mapError(rows.Err())
}

// Backtest loads one run.
func (s *ResearchStore) Backtest(ctx context.Context, id uuid.UUID) (Backtest, error) {
	return scanBacktest(s.pool.QueryRow(ctx, `SELECT `+backtestColumns+` FROM backtests WHERE id = $1`, id))
}

// BacktestTrades returns a run's trades.
func (s *ResearchStore) BacktestTrades(ctx context.Context, id uuid.UUID, limit int) ([]BacktestTrade, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `
		SELECT instrument_id, side, quantity, entry_time, entry_price, exit_time, exit_price,
		       gross_pnl, commission, slippage, swap, net_pnl, mae, mfe, exit_reason
		FROM backtest_trades WHERE backtest_id = $1 ORDER BY entry_time LIMIT $2`, id, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []BacktestTrade
	for rows.Next() {
		var t BacktestTrade
		if err := rows.Scan(&t.InstrumentID, &t.Side, &t.Quantity, &t.EntryTime, &t.EntryPrice,
			&t.ExitTime, &t.ExitPrice, &t.GrossPnL, &t.Commission, &t.Slippage, &t.Swap,
			&t.NetPnL, &t.MAE, &t.MFE, &t.ExitReason); err != nil {
			return nil, mapError(err)
		}
		out = append(out, t)
	}
	return out, mapError(rows.Err())
}

// ---------------------------------------------------------------------------
// Machine learning
// ---------------------------------------------------------------------------

// MLDataset is a reproducible feature/label snapshot.
type MLDataset struct {
	ID              uuid.UUID
	Name            string
	InstrumentID    string
	Timeframe       string
	FeatureSet      json.RawMessage
	LabelDefinition json.RawMessage
	PeriodStart     time.Time
	PeriodEnd       time.Time
	RowCount        int
	ContentHash     string
	CreatedAt       time.Time
}

// UpsertDataset registers a dataset snapshot, keyed by content hash so the
// same data cannot be registered twice under two names.
func (s *ResearchStore) UpsertDataset(ctx context.Context, d MLDataset) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO ml_datasets (name, instrument_id, timeframe, feature_set, label_definition,
			period_start, period_end, row_count, content_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (content_hash) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`,
		d.Name, d.InstrumentID, d.Timeframe, d.FeatureSet, d.LabelDefinition,
		d.PeriodStart, d.PeriodEnd, d.RowCount, d.ContentHash).Scan(&id)
	return id, mapError(err)
}

// ListDatasets returns dataset snapshots.
func (s *ResearchStore) ListDatasets(ctx context.Context, limit int) ([]MLDataset, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, instrument_id, timeframe, feature_set, label_definition,
		       period_start, period_end, row_count, content_hash, created_at
		FROM ml_datasets ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []MLDataset
	for rows.Next() {
		var d MLDataset
		if err := rows.Scan(&d.ID, &d.Name, &d.InstrumentID, &d.Timeframe, &d.FeatureSet,
			&d.LabelDefinition, &d.PeriodStart, &d.PeriodEnd, &d.RowCount,
			&d.ContentHash, &d.CreatedAt); err != nil {
			return nil, mapError(err)
		}
		out = append(out, d)
	}
	return out, mapError(rows.Err())
}

// UpsertModel registers a model definition.
func (s *ResearchStore) UpsertModel(ctx context.Context, key, name, task, description string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO ml_models (key, name, task, description)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description
		RETURNING id`, key, name, task, description).Scan(&id)
	return id, mapError(err)
}

// MLModelVersion is a trained model with its full reproducibility record.
type MLModelVersion struct {
	ID                 uuid.UUID
	ModelID            uuid.UUID
	Version            int
	DatasetID          uuid.UUID
	Algorithm          string
	Hyperparameters    json.RawMessage
	CodeGitSHA         string
	DatasetHash        string
	FeatureDefinition  json.RawMessage
	LabelDefinition    json.RawMessage
	TrainStart         time.Time
	TrainEnd           time.Time
	ValidationStart    *time.Time
	ValidationEnd      *time.Time
	TestStart          *time.Time
	TestEnd            *time.Time
	RandomSeed         int64
	DependencyVersions json.RawMessage
	ArtifactPath       *string
	ArtifactHash       *string
	Lifecycle          string
	CreatedAt          time.Time
	PromotedAt         *time.Time
}

// SaveModelVersion stores a trained model version.
func (s *ResearchStore) SaveModelVersion(ctx context.Context, v MLModelVersion) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO ml_model_versions (model_id, version, dataset_id, algorithm, hyperparameters,
			code_git_sha, dataset_hash, feature_definition, label_definition, train_start,
			train_end, validation_start, validation_end, test_start, test_end, random_seed,
			dependency_versions, artifact_path, artifact_hash, lifecycle)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		ON CONFLICT (model_id, version) DO UPDATE SET
			hyperparameters = EXCLUDED.hyperparameters,
			artifact_path = EXCLUDED.artifact_path,
			artifact_hash = EXCLUDED.artifact_hash
		RETURNING id`,
		v.ModelID, v.Version, v.DatasetID, v.Algorithm, v.Hyperparameters, v.CodeGitSHA,
		v.DatasetHash, v.FeatureDefinition, v.LabelDefinition, v.TrainStart, v.TrainEnd,
		v.ValidationStart, v.ValidationEnd, v.TestStart, v.TestEnd, v.RandomSeed,
		v.DependencyVersions, v.ArtifactPath, v.ArtifactHash, v.Lifecycle).Scan(&id)
	return id, mapError(err)
}

// SaveEvaluation stores metrics for one data split.
func (s *ResearchStore) SaveEvaluation(ctx context.Context, modelVersionID uuid.UUID, split string, metrics json.RawMessage, start, end time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ml_evaluations (model_version_id, split, metrics, period_start, period_end)
		VALUES ($1,$2,$3,$4,$5)`, modelVersionID, split, metrics, start, end)
	return mapError(err)
}

// ModelVersionRow is a model version joined with its model metadata.
type ModelVersionRow struct {
	MLModelVersion
	ModelKey  string
	ModelName string
	Task      string
}

// ListModelVersions returns trained versions, newest first.
func (s *ResearchStore) ListModelVersions(ctx context.Context, limit int) ([]ModelVersionRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT v.id, v.model_id, v.version, v.dataset_id, v.algorithm, v.hyperparameters,
		       v.code_git_sha, v.dataset_hash, v.feature_definition, v.label_definition,
		       v.train_start, v.train_end, v.validation_start, v.validation_end,
		       v.test_start, v.test_end, v.random_seed, v.dependency_versions,
		       v.artifact_path, v.artifact_hash, v.lifecycle, v.created_at, v.promoted_at,
		       m.key, m.name, m.task
		FROM ml_model_versions v JOIN ml_models m ON m.id = v.model_id
		ORDER BY v.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []ModelVersionRow
	for rows.Next() {
		var r ModelVersionRow
		if err := rows.Scan(&r.ID, &r.ModelID, &r.Version, &r.DatasetID, &r.Algorithm,
			&r.Hyperparameters, &r.CodeGitSHA, &r.DatasetHash, &r.FeatureDefinition,
			&r.LabelDefinition, &r.TrainStart, &r.TrainEnd, &r.ValidationStart, &r.ValidationEnd,
			&r.TestStart, &r.TestEnd, &r.RandomSeed, &r.DependencyVersions, &r.ArtifactPath,
			&r.ArtifactHash, &r.Lifecycle, &r.CreatedAt, &r.PromotedAt,
			&r.ModelKey, &r.ModelName, &r.Task); err != nil {
			return nil, mapError(err)
		}
		out = append(out, r)
	}
	return out, mapError(rows.Err())
}

// EvaluationRow is one stored evaluation.
type EvaluationRow struct {
	ModelVersionID uuid.UUID
	Split          string
	Metrics        json.RawMessage
	PeriodStart    time.Time
	PeriodEnd      time.Time
}

// EvaluationsForVersion returns a version's evaluations.
func (s *ResearchStore) EvaluationsForVersion(ctx context.Context, modelVersionID uuid.UUID) ([]EvaluationRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT model_version_id, split, metrics, period_start, period_end
		FROM ml_evaluations WHERE model_version_id = $1
		ORDER BY CASE split WHEN 'train' THEN 0 WHEN 'validation' THEN 1
		                    WHEN 'test' THEN 2 ELSE 3 END`, modelVersionID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []EvaluationRow
	for rows.Next() {
		var e EvaluationRow
		if err := rows.Scan(&e.ModelVersionID, &e.Split, &e.Metrics, &e.PeriodStart, &e.PeriodEnd); err != nil {
			return nil, mapError(err)
		}
		out = append(out, e)
	}
	return out, mapError(rows.Err())
}

// ---------------------------------------------------------------------------
// Economic calendar and news
// ---------------------------------------------------------------------------

// EconomicEvent is a scheduled macroeconomic release.
type EconomicEvent struct {
	ID          uuid.UUID
	ExternalID  string
	ScheduledAt time.Time
	Country     string
	Currency    string
	Impact      string
	EventName   string
	Category    *string
	Actual      *string
	Forecast    *string
	Previous    *string
	Revised     *string
	Unit        *string
	Status      string
	Source      string
	ReleasedAt  *time.Time
}

// UpsertEconomicEvent records or updates a calendar entry.
func (s *ResearchStore) UpsertEconomicEvent(ctx context.Context, e EconomicEvent) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO economic_events (external_id, scheduled_at, country, currency, impact,
			event_name, category, actual, forecast, previous, revised, unit, status, source, released_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (source, external_id) DO UPDATE SET
			scheduled_at = EXCLUDED.scheduled_at, impact = EXCLUDED.impact,
			actual = EXCLUDED.actual, forecast = EXCLUDED.forecast,
			previous = EXCLUDED.previous, revised = EXCLUDED.revised,
			status = EXCLUDED.status, released_at = EXCLUDED.released_at`,
		e.ExternalID, e.ScheduledAt, e.Country, e.Currency, e.Impact, e.EventName,
		e.Category, e.Actual, e.Forecast, e.Previous, e.Revised, e.Unit, e.Status,
		e.Source, e.ReleasedAt)
	return mapError(err)
}

// EventFilter narrows a calendar query.
type EventFilter struct {
	From       *time.Time
	To         *time.Time
	Currencies []string
	MinImpact  string
	Limit      int
}

// ListEconomicEvents returns calendar entries in chronological order.
func (s *ResearchStore) ListEconomicEvents(ctx context.Context, f EventFilter) ([]EconomicEvent, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, external_id, scheduled_at, country, currency, impact, event_name,
	             category, actual, forecast, previous, revised, unit, status, source, released_at
	      FROM economic_events WHERE 1=1`
	args := []any{limit}
	if f.From != nil {
		args = append(args, *f.From)
		q += fmt.Sprintf(" AND scheduled_at >= $%d", len(args))
	}
	if f.To != nil {
		args = append(args, *f.To)
		q += fmt.Sprintf(" AND scheduled_at < $%d", len(args))
	}
	if len(f.Currencies) > 0 {
		args = append(args, f.Currencies)
		q += fmt.Sprintf(" AND currency = ANY($%d)", len(args))
	}
	switch f.MinImpact {
	case "high":
		q += ` AND impact = 'high'`
	case "medium":
		q += ` AND impact IN ('medium','high')`
	}
	q += ` ORDER BY scheduled_at ASC LIMIT $1`

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// UpcomingHighImpactEvents returns high-impact releases affecting an
// instrument within a window. This feeds the event-risk check in the order
// pipeline and the "next event" indicator on the dashboard.
func (s *ResearchStore) UpcomingHighImpactEvents(ctx context.Context, instrumentID string, from, to time.Time) ([]EconomicEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.external_id, e.scheduled_at, e.country, e.currency, e.impact, e.event_name,
		       e.category, e.actual, e.forecast, e.previous, e.revised, e.unit, e.status,
		       e.source, e.released_at
		FROM economic_events e
		JOIN economic_event_instrument_map m ON m.currency = e.currency
		WHERE m.instrument_id = $1
		  AND e.impact = 'high'
		  AND e.scheduled_at >= $2 AND e.scheduled_at < $3
		ORDER BY e.scheduled_at ASC`, instrumentID, from, to)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

func scanEvents(rows pgx.Rows) ([]EconomicEvent, error) {
	var out []EconomicEvent
	for rows.Next() {
		var e EconomicEvent
		if err := rows.Scan(&e.ID, &e.ExternalID, &e.ScheduledAt, &e.Country, &e.Currency,
			&e.Impact, &e.EventName, &e.Category, &e.Actual, &e.Forecast, &e.Previous,
			&e.Revised, &e.Unit, &e.Status, &e.Source, &e.ReleasedAt); err != nil {
			return nil, mapError(err)
		}
		out = append(out, e)
	}
	return out, mapError(rows.Err())
}

// MapEventCurrencyToInstrument records that a currency's releases are material
// to an instrument.
func (s *ResearchStore) MapEventCurrencyToInstrument(ctx context.Context, currency, instrumentID string, relevance decimal.Decimal) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO economic_event_instrument_map (currency, instrument_id, relevance)
		VALUES ($1,$2,$3)
		ON CONFLICT (currency, instrument_id) DO UPDATE SET relevance = EXCLUDED.relevance`,
		currency, instrumentID, relevance)
	return mapError(err)
}

// NewsItem is a headline with its provenance.
type NewsItem struct {
	ID                 uuid.UUID
	ExternalID         string
	Source             string
	Headline           string
	Summary            *string
	URL                *string
	PublishedAt        time.Time
	ReceivedAt         time.Time
	RelatedInstruments []string
	RelatedCurrencies  []string
	Sentiment          *decimal.Decimal
	SentimentModel     *string
	Topics             []string
}

// UpsertNewsItem records a headline.
func (s *ResearchStore) UpsertNewsItem(ctx context.Context, n NewsItem) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO news_items (external_id, source, headline, summary, url, published_at,
			related_instruments, related_currencies, sentiment, sentiment_model, topics)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (source, external_id) DO UPDATE SET
			headline = EXCLUDED.headline, summary = EXCLUDED.summary,
			sentiment = EXCLUDED.sentiment, topics = EXCLUDED.topics`,
		n.ExternalID, n.Source, n.Headline, n.Summary, n.URL, n.PublishedAt,
		n.RelatedInstruments, n.RelatedCurrencies, n.Sentiment, n.SentimentModel, n.Topics)
	return mapError(err)
}

// ListNews returns recent headlines.
func (s *ResearchStore) ListNews(ctx context.Context, instrumentID *string, limit int) ([]NewsItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT id, external_id, source, headline, summary, url, published_at, received_at,
	             related_instruments, related_currencies, sentiment, sentiment_model, topics
	      FROM news_items`
	args := []any{limit}
	if instrumentID != nil {
		args = append(args, *instrumentID)
		q += fmt.Sprintf(" WHERE $%d = ANY(related_instruments)", len(args))
	}
	q += ` ORDER BY published_at DESC LIMIT $1`

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []NewsItem
	for rows.Next() {
		var n NewsItem
		if err := rows.Scan(&n.ID, &n.ExternalID, &n.Source, &n.Headline, &n.Summary, &n.URL,
			&n.PublishedAt, &n.ReceivedAt, &n.RelatedInstruments, &n.RelatedCurrencies,
			&n.Sentiment, &n.SentimentModel, &n.Topics); err != nil {
			return nil, mapError(err)
		}
		out = append(out, n)
	}
	return out, mapError(rows.Err())
}

// ---------------------------------------------------------------------------
// Reconciliation
// ---------------------------------------------------------------------------

// StartReconciliationRun opens a reconciliation record.
func (s *ResearchStore) StartReconciliationRun(ctx context.Context, accountID uuid.UUID, brokerName, trigger string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO reconciliation_runs (account_id, broker_name, trigger, status)
		VALUES ($1,$2,$3,'running') RETURNING id`, accountID, brokerName, trigger).Scan(&id)
	return id, mapError(err)
}

// FinishReconciliationRun closes a reconciliation record.
func (s *ResearchStore) FinishReconciliationRun(ctx context.Context, id uuid.UUID, status string,
	ordersCompared, positionsCompared, discrepancies int, errMsg string) error {
	var e *string
	if errMsg != "" {
		e = &errMsg
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE reconciliation_runs
		SET status = $2, orders_compared = $3, positions_compared = $4,
		    discrepancies = $5, error = $6, finished_at = now()
		WHERE id = $1`, id, status, ordersCompared, positionsCompared, discrepancies, e)
	return mapError(err)
}

// Reconciliation discrepancies moved to internal/store/reconciliation.go.
//
// Migration 0010 replaced reconciliation_discrepancies with
// reconciliation_issues, which records the issue TYPE, the evidence from both
// sides, and the resolution history -- none of which the old two-string shape
// could express, and all of which a repair needs.
//
// The methods that queried the old table are deleted rather than left as
// wrappers. They compiled perfectly and failed only when called, which is the
// worst combination available: a reader assumes they work, and the 500 arrives
// on the dashboard of whoever loads it first. That is precisely what happened.

// LatestReconciliation summarises an account's most recent run.
type ReconciliationSummary struct {
	ID                uuid.UUID
	Trigger           string
	Status            string
	OrdersCompared    int
	PositionsCompared int
	Discrepancies     int
	StartedAt         time.Time
	FinishedAt        *time.Time
}

// LatestReconciliation returns the most recent run for an account.
func (s *ResearchStore) LatestReconciliation(ctx context.Context, accountID uuid.UUID) (ReconciliationSummary, error) {
	var r ReconciliationSummary
	err := s.pool.QueryRow(ctx, `
		SELECT id, trigger, status, orders_compared, positions_compared, discrepancies,
		       started_at, finished_at
		FROM reconciliation_runs WHERE account_id = $1 ORDER BY started_at DESC LIMIT 1`,
		accountID).
		Scan(&r.ID, &r.Trigger, &r.Status, &r.OrdersCompared, &r.PositionsCompared,
			&r.Discrepancies, &r.StartedAt, &r.FinishedAt)
	return r, mapError(err)
}

func nullUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
