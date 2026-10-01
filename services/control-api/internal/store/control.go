package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/db"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/money"
)

// ControlStore persists the control-plane objects that gate execution:
// trading authority, kill switches, risk limits, risk events and the audit log.
type ControlStore struct{ pool *db.Pool }

// ---------------------------------------------------------------------------
// Trading authority
// ---------------------------------------------------------------------------

const authorityColumns = `id, user_id, account_id, mode, active, automation_enabled,
	allowed_instruments, allowed_strategy_ids, allowed_order_types,
	max_order_quantity, max_order_notional, max_position_exposure, max_leverage,
	max_daily_loss, currency, valid_from, valid_until, created_by, created_at,
	updated_at, revoked_at, revocation_reason, version`

func scanAuthority(row pgx.Row) (domain.TradingAuthority, error) {
	var a domain.TradingAuthority
	var orderTypes []string
	var ccy string
	var maxNotional, maxExposure, maxDailyLoss decimal.Decimal

	err := row.Scan(&a.ID, &a.UserID, &a.AccountID, &a.Mode, &a.Active, &a.AutomationEnabled,
		&a.AllowedInstruments, &a.AllowedStrategyIDs, &orderTypes,
		&a.MaxOrderQuantity, &maxNotional, &maxExposure, &a.MaxLeverage,
		&maxDailyLoss, &ccy, &a.ValidFrom, &a.ValidUntil, &a.CreatedBy, &a.CreatedAt,
		&a.UpdatedAt, &a.RevokedAt, &a.RevocationReason, &a.Version)
	if err != nil {
		return domain.TradingAuthority{}, mapError(err)
	}
	c := money.Currency(ccy)
	a.MaxOrderNotional = money.New(maxNotional, c)
	a.MaxPositionExposure = money.New(maxExposure, c)
	a.MaxDailyLoss = money.New(maxDailyLoss, c)
	a.AllowedOrderTypes = make([]domain.OrderType, 0, len(orderTypes))
	for _, t := range orderTypes {
		a.AllowedOrderTypes = append(a.AllowedOrderTypes, domain.OrderType(t))
	}
	return a, nil
}

// CreateAuthority grants a scoped trading mandate.
func (s *ControlStore) CreateAuthority(ctx context.Context, a domain.TradingAuthority, actor uuid.UUID) (domain.TradingAuthority, error) {
	orderTypes := make([]string, 0, len(a.AllowedOrderTypes))
	for _, t := range a.AllowedOrderTypes {
		orderTypes = append(orderTypes, string(t))
	}

	var created domain.TradingAuthority
	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO trading_authorities (user_id, account_id, mode, active, automation_enabled,
				allowed_instruments, allowed_strategy_ids, allowed_order_types,
				max_order_quantity, max_order_notional, max_position_exposure, max_leverage,
				max_daily_loss, currency, valid_from, valid_until, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
			RETURNING `+authorityColumns,
			a.UserID, a.AccountID, a.Mode, a.Active, a.AutomationEnabled,
			a.AllowedInstruments, a.AllowedStrategyIDs, orderTypes,
			a.MaxOrderQuantity, a.MaxOrderNotional.Decimal(), a.MaxPositionExposure.Decimal(),
			a.MaxLeverage, a.MaxDailyLoss.Decimal(), string(a.MaxDailyLoss.Currency()),
			a.ValidFrom, a.ValidUntil, actor)
		var err error
		created, err = scanAuthority(row)
		if err != nil {
			return err
		}
		after, _ := json.Marshal(created)
		_, err = tx.Exec(ctx, `
			INSERT INTO trading_authority_history (authority_id, account_id, changed_by, action, after)
			VALUES ($1,$2,$3,'created',$4)`, created.ID, created.AccountID, actor, after)
		return mapError(err)
	})
	return created, err
}

// ActiveAuthorityForAccount loads the live mandate for an account, if any.
func (s *ControlStore) ActiveAuthorityForAccount(ctx context.Context, accountID uuid.UUID) (domain.TradingAuthority, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+authorityColumns+` FROM trading_authorities
		WHERE account_id = $1 AND active = TRUE AND revoked_at IS NULL`, accountID)
	return scanAuthority(row)
}

// AuthorityForUser loads a mandate scoped to its owner.
func (s *ControlStore) AuthorityForUser(ctx context.Context, userID, id uuid.UUID) (domain.TradingAuthority, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+authorityColumns+` FROM trading_authorities WHERE id = $1 AND user_id = $2`, id, userID)
	return scanAuthority(row)
}

// UpdateAuthority replaces a mandate's scope, recording the before/after.
func (s *ControlStore) UpdateAuthority(ctx context.Context, a domain.TradingAuthority, actor uuid.UUID, expectedVersion int64) (domain.TradingAuthority, error) {
	orderTypes := make([]string, 0, len(a.AllowedOrderTypes))
	for _, t := range a.AllowedOrderTypes {
		orderTypes = append(orderTypes, string(t))
	}

	var updated domain.TradingAuthority
	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		before, err := scanAuthority(tx.QueryRow(ctx,
			`SELECT `+authorityColumns+` FROM trading_authorities WHERE id = $1 FOR UPDATE`, a.ID))
		if err != nil {
			return err
		}
		if before.Version != expectedVersion {
			return ErrStaleVersion
		}

		row := tx.QueryRow(ctx, `
			UPDATE trading_authorities
			SET automation_enabled = $2, allowed_instruments = $3, allowed_strategy_ids = $4,
			    allowed_order_types = $5, max_order_quantity = $6, max_order_notional = $7,
			    max_position_exposure = $8, max_leverage = $9, max_daily_loss = $10,
			    valid_until = $11, active = $12, updated_at = now(), version = version + 1
			WHERE id = $1 AND version = $13
			RETURNING `+authorityColumns,
			a.ID, a.AutomationEnabled, a.AllowedInstruments, a.AllowedStrategyIDs, orderTypes,
			a.MaxOrderQuantity, a.MaxOrderNotional.Decimal(), a.MaxPositionExposure.Decimal(),
			a.MaxLeverage, a.MaxDailyLoss.Decimal(), a.ValidUntil, a.Active, expectedVersion)
		updated, err = scanAuthority(row)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(before)
		af, _ := json.Marshal(updated)
		_, err = tx.Exec(ctx, `
			INSERT INTO trading_authority_history (authority_id, account_id, changed_by, action, before, after)
			VALUES ($1,$2,$3,'updated',$4,$5)`, a.ID, a.AccountID, actor, b, af)
		return mapError(err)
	})
	return updated, err
}

// RevokeAuthority ends a mandate. Revocation is immediate and irreversible:
// re-authorising requires a new grant, so a revoked mandate cannot be quietly
// reinstated without leaving a record.
func (s *ControlStore) RevokeAuthority(ctx context.Context, id uuid.UUID, actor uuid.UUID, reason string) error {
	return s.pool.InTx(ctx, func(tx pgx.Tx) error {
		var accountID uuid.UUID
		err := tx.QueryRow(ctx, `
			UPDATE trading_authorities
			SET active = FALSE, revoked_at = now(), revocation_reason = $2,
			    automation_enabled = FALSE, updated_at = now(), version = version + 1
			WHERE id = $1 AND revoked_at IS NULL
			RETURNING account_id`, id, reason).Scan(&accountID)
		if err != nil {
			return mapError(err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO trading_authority_history (authority_id, account_id, changed_by, action, reason)
			VALUES ($1,$2,$3,'revoked',$4)`, id, accountID, actor, reason)
		return mapError(err)
	})
}

// ---------------------------------------------------------------------------
// Kill switches
// ---------------------------------------------------------------------------

// ActivateKillSwitch halts new orders within a scope. Activation is idempotent:
// re-activating an already-active switch returns the existing one rather than
// erroring, because an operator hitting the button twice in an incident must
// not see a failure.
func (s *ControlStore) ActivateKillSwitch(ctx context.Context, scope domain.KillSwitchScope, targetID *string, reason string, actor uuid.UUID) (domain.KillSwitch, error) {
	var ks domain.KillSwitch
	err := s.pool.QueryRow(ctx, `
		INSERT INTO kill_switches (scope, target_id, active, reason, activated_by)
		VALUES ($1,$2,TRUE,$3,$4)
		ON CONFLICT DO NOTHING
		RETURNING id, scope, target_id, active, reason, activated_by, activated_at,
		          deactivated_by, deactivated_at, created_at, updated_at`,
		string(scope), targetID, reason, actor).
		Scan(&ks.ID, &ks.Scope, &ks.TargetID, &ks.Active, &ks.Reason, &ks.ActivatedBy,
			&ks.ActivatedAt, &ks.DeactivatedBy, &ks.DeactivatedAt, &ks.CreatedAt, &ks.UpdatedAt)
	if err == nil {
		return ks, nil
	}
	if mapped := mapError(err); mapped != ErrNotFound {
		return domain.KillSwitch{}, mapped
	}
	// ON CONFLICT DO NOTHING returned no row: a switch is already active here.
	return s.activeKillSwitch(ctx, scope, targetID)
}

func (s *ControlStore) activeKillSwitch(ctx context.Context, scope domain.KillSwitchScope, targetID *string) (domain.KillSwitch, error) {
	var ks domain.KillSwitch
	q := `SELECT id, scope, target_id, active, reason, activated_by, activated_at,
	             deactivated_by, deactivated_at, created_at, updated_at
	      FROM kill_switches WHERE active = TRUE AND scope = $1 AND `
	var row pgx.Row
	if targetID == nil {
		row = s.pool.QueryRow(ctx, q+`target_id IS NULL`, string(scope))
	} else {
		row = s.pool.QueryRow(ctx, q+`target_id = $2`, string(scope), *targetID)
	}
	err := row.Scan(&ks.ID, &ks.Scope, &ks.TargetID, &ks.Active, &ks.Reason, &ks.ActivatedBy,
		&ks.ActivatedAt, &ks.DeactivatedBy, &ks.DeactivatedAt, &ks.CreatedAt, &ks.UpdatedAt)
	return ks, mapError(err)
}

// KillSwitchByID loads one switch, active or not.
//
// Deactivation needs it: the authorisation rule depends on the switch's SCOPE,
// and the request carries only an id. Without this the handler had nothing to
// decide on and so decided nothing, which let a trader lift a global halt an
// administrator had placed.
func (s *ControlStore) KillSwitchByID(ctx context.Context, id uuid.UUID) (domain.KillSwitch, error) {
	var ks domain.KillSwitch
	err := s.pool.QueryRow(ctx, `
		SELECT id, scope, target_id, active, reason, activated_by, activated_at,
		       deactivated_by, deactivated_at, created_at, updated_at
		FROM kill_switches WHERE id = $1`, id).
		Scan(&ks.ID, &ks.Scope, &ks.TargetID, &ks.Active, &ks.Reason, &ks.ActivatedBy,
			&ks.ActivatedAt, &ks.DeactivatedBy, &ks.DeactivatedAt, &ks.CreatedAt, &ks.UpdatedAt)
	return ks, mapError(err)
}

// DeactivateKillSwitch resumes trading within a scope.
func (s *ControlStore) DeactivateKillSwitch(ctx context.Context, id uuid.UUID, actor uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE kill_switches
		SET active = FALSE, deactivated_by = $2, deactivated_at = now(), updated_at = now()
		WHERE id = $1 AND active = TRUE`, id, actor)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// KillSwitchesFor resolves every active switch that applies to one order.
//
// Evaluated as a single query so the check cannot be partially applied: it is
// not possible for the account switch to be honoured and the global one missed
// because of an early return in application code.
func (s *ControlStore) KillSwitchesFor(ctx context.Context, userID, accountID uuid.UUID, brokerName string, strategyID *uuid.UUID) (domain.KillSwitchState, error) {
	var strategyStr *string
	if strategyID != nil {
		v := strategyID.String()
		strategyStr = &v
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, scope, target_id, active, reason, activated_by, activated_at,
		       deactivated_by, deactivated_at, created_at, updated_at
		FROM kill_switches
		WHERE active = TRUE AND (
			scope = 'global'
			OR (scope = 'user' AND target_id = $1)
			OR (scope = 'account' AND target_id = $2)
			OR (scope = 'broker' AND target_id = $3)
			OR (scope = 'strategy' AND target_id = $4)
		)
		ORDER BY CASE scope WHEN 'global' THEN 0 WHEN 'account' THEN 1
		                    WHEN 'user' THEN 2 WHEN 'broker' THEN 3 ELSE 4 END`,
		userID.String(), accountID.String(), brokerName, strategyStr)
	if err != nil {
		return domain.KillSwitchState{}, mapError(err)
	}
	defer rows.Close()

	var state domain.KillSwitchState
	for rows.Next() {
		var ks domain.KillSwitch
		if err := rows.Scan(&ks.ID, &ks.Scope, &ks.TargetID, &ks.Active, &ks.Reason,
			&ks.ActivatedBy, &ks.ActivatedAt, &ks.DeactivatedBy, &ks.DeactivatedAt,
			&ks.CreatedAt, &ks.UpdatedAt); err != nil {
			return domain.KillSwitchState{}, mapError(err)
		}
		state.Active = append(state.Active, ks)
	}
	return state, mapError(rows.Err())
}

// ListKillSwitches returns all switches, active first.
func (s *ControlStore) ListKillSwitches(ctx context.Context, activeOnly bool) ([]domain.KillSwitch, error) {
	q := `SELECT id, scope, target_id, active, reason, activated_by, activated_at,
	             deactivated_by, deactivated_at, created_at, updated_at
	      FROM kill_switches`
	if activeOnly {
		q += ` WHERE active = TRUE`
	}
	q += ` ORDER BY active DESC, activated_at DESC LIMIT 200`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []domain.KillSwitch
	for rows.Next() {
		var ks domain.KillSwitch
		if err := rows.Scan(&ks.ID, &ks.Scope, &ks.TargetID, &ks.Active, &ks.Reason,
			&ks.ActivatedBy, &ks.ActivatedAt, &ks.DeactivatedBy, &ks.DeactivatedAt,
			&ks.CreatedAt, &ks.UpdatedAt); err != nil {
			return nil, mapError(err)
		}
		out = append(out, ks)
	}
	return out, mapError(rows.Err())
}

// ---------------------------------------------------------------------------
// Risk limits
// ---------------------------------------------------------------------------

const riskColumns = `id, account_id, currency, max_order_quantity, max_order_notional,
	max_risk_per_trade_fraction, require_stop_loss, max_open_positions, max_pending_orders,
	max_gross_exposure, max_net_exposure, max_per_instrument_exposure, max_concentration_fraction,
	max_leverage, max_daily_loss, max_drawdown_fraction, max_spread_fraction, max_slippage_fraction,
	event_blackout_before_minutes, event_blackout_after_minutes, block_on_high_impact_events,
	updated_by, created_at, updated_at, version`

func scanRiskLimits(row pgx.Row) (domain.RiskLimits, error) {
	var r domain.RiskLimits
	var ccy string
	var notional, gross, net, perInst, dailyLoss decimal.Decimal
	var updatedBy *uuid.UUID

	err := row.Scan(&r.ID, &r.AccountID, &ccy, &r.MaxOrderQuantity, &notional,
		&r.MaxRiskPerTradeFraction, &r.RequireStopLoss, &r.MaxOpenPositions, &r.MaxPendingOrders,
		&gross, &net, &perInst, &r.MaxConcentrationFraction,
		&r.MaxLeverage, &dailyLoss, &r.MaxDrawdownFraction, &r.MaxSpreadFraction, &r.MaxSlippageFraction,
		&r.EventBlackoutBeforeMinutes, &r.EventBlackoutAfterMinutes, &r.BlockOnHighImpactEvents,
		&updatedBy, &r.CreatedAt, &r.UpdatedAt, &r.Version)
	if err != nil {
		return domain.RiskLimits{}, mapError(err)
	}
	c := money.Currency(ccy)
	r.Currency = c
	r.MaxOrderNotional = money.New(notional, c)
	r.MaxGrossExposure = money.New(gross, c)
	r.MaxNetExposure = money.New(net, c)
	r.MaxPerInstrumentExposure = money.New(perInst, c)
	r.MaxDailyLoss = money.New(dailyLoss, c)
	if updatedBy != nil {
		r.UpdatedBy = *updatedBy
	}
	return r, nil
}

// UpsertRiskLimits creates or replaces an account's limits.
func (s *ControlStore) UpsertRiskLimits(ctx context.Context, r domain.RiskLimits, actor uuid.UUID) (domain.RiskLimits, error) {
	var out domain.RiskLimits
	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		before, beforeErr := scanRiskLimits(tx.QueryRow(ctx,
			`SELECT `+riskColumns+` FROM risk_limits WHERE account_id = $1 FOR UPDATE`, r.AccountID))

		row := tx.QueryRow(ctx, `
			INSERT INTO risk_limits (account_id, currency, max_order_quantity, max_order_notional,
				max_risk_per_trade_fraction, require_stop_loss, max_open_positions, max_pending_orders,
				max_gross_exposure, max_net_exposure, max_per_instrument_exposure,
				max_concentration_fraction, max_leverage, max_daily_loss, max_drawdown_fraction,
				max_spread_fraction, max_slippage_fraction, event_blackout_before_minutes,
				event_blackout_after_minutes, block_on_high_impact_events, updated_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
			ON CONFLICT (account_id) DO UPDATE SET
				currency = EXCLUDED.currency,
				max_order_quantity = EXCLUDED.max_order_quantity,
				max_order_notional = EXCLUDED.max_order_notional,
				max_risk_per_trade_fraction = EXCLUDED.max_risk_per_trade_fraction,
				require_stop_loss = EXCLUDED.require_stop_loss,
				max_open_positions = EXCLUDED.max_open_positions,
				max_pending_orders = EXCLUDED.max_pending_orders,
				max_gross_exposure = EXCLUDED.max_gross_exposure,
				max_net_exposure = EXCLUDED.max_net_exposure,
				max_per_instrument_exposure = EXCLUDED.max_per_instrument_exposure,
				max_concentration_fraction = EXCLUDED.max_concentration_fraction,
				max_leverage = EXCLUDED.max_leverage,
				max_daily_loss = EXCLUDED.max_daily_loss,
				max_drawdown_fraction = EXCLUDED.max_drawdown_fraction,
				max_spread_fraction = EXCLUDED.max_spread_fraction,
				max_slippage_fraction = EXCLUDED.max_slippage_fraction,
				event_blackout_before_minutes = EXCLUDED.event_blackout_before_minutes,
				event_blackout_after_minutes = EXCLUDED.event_blackout_after_minutes,
				block_on_high_impact_events = EXCLUDED.block_on_high_impact_events,
				updated_by = EXCLUDED.updated_by,
				updated_at = now(),
				version = risk_limits.version + 1
			RETURNING `+riskColumns,
			r.AccountID, string(r.Currency), r.MaxOrderQuantity, r.MaxOrderNotional.Decimal(),
			r.MaxRiskPerTradeFraction, r.RequireStopLoss, r.MaxOpenPositions, r.MaxPendingOrders,
			r.MaxGrossExposure.Decimal(), r.MaxNetExposure.Decimal(), r.MaxPerInstrumentExposure.Decimal(),
			r.MaxConcentrationFraction, r.MaxLeverage, r.MaxDailyLoss.Decimal(), r.MaxDrawdownFraction,
			r.MaxSpreadFraction, r.MaxSlippageFraction, r.EventBlackoutBeforeMinutes,
			r.EventBlackoutAfterMinutes, r.BlockOnHighImpactEvents, actor)

		var err error
		out, err = scanRiskLimits(row)
		if err != nil {
			return err
		}

		// Every change to a risk ceiling is recorded with its previous value.
		// "Who widened the daily loss limit, and when?" must be answerable.
		beforeJSON := []byte(`null`)
		if beforeErr == nil {
			beforeJSON, _ = json.Marshal(before)
		}
		afterJSON, _ := json.Marshal(out)
		_, err = tx.Exec(ctx, `
			INSERT INTO risk_limit_history (account_id, changed_by, before, after)
			VALUES ($1,$2,$3,$4)`, r.AccountID, actor, beforeJSON, afterJSON)
		return mapError(err)
	})
	return out, err
}

// RiskLimitsForAccount loads an account's limits.
func (s *ControlStore) RiskLimitsForAccount(ctx context.Context, accountID uuid.UUID) (domain.RiskLimits, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+riskColumns+` FROM risk_limits WHERE account_id = $1`, accountID)
	return scanRiskLimits(row)
}

// RecordRiskEvent persists a risk occurrence.
func (s *ControlStore) RecordRiskEvent(ctx context.Context, q querier, e domain.RiskEvent) error {
	detail := e.Detail
	if detail == nil {
		detail = map[string]string{}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("store: marshal risk detail: %w", err)
	}
	if q == nil {
		q = s.pool
	}
	_, err = q.Exec(ctx, `
		INSERT INTO risk_events (account_id, user_id, severity, check_name, code, message,
			instrument_id, strategy_id, order_id, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		e.AccountID, e.UserID, e.Severity, string(e.Check), string(e.Code), e.Message,
		e.InstrumentID, e.StrategyID, e.OrderID, raw)
	return mapError(err)
}

// ListRiskEvents returns recent risk events for an account.
func (s *ControlStore) ListRiskEvents(ctx context.Context, accountID uuid.UUID, limit int) ([]domain.RiskEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, account_id, user_id, severity, check_name, code, message,
		       instrument_id, strategy_id, order_id, detail, created_at
		FROM risk_events WHERE account_id = $1 ORDER BY created_at DESC LIMIT $2`,
		accountID, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []domain.RiskEvent
	for rows.Next() {
		var e domain.RiskEvent
		var raw []byte
		if err := rows.Scan(&e.ID, &e.AccountID, &e.UserID, &e.Severity, &e.Check, &e.Code,
			&e.Message, &e.InstrumentID, &e.StrategyID, &e.OrderID, &raw, &e.CreatedAt); err != nil {
			return nil, mapError(err)
		}
		_ = json.Unmarshal(raw, &e.Detail)
		out = append(out, e)
	}
	return out, mapError(rows.Err())
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// AppendAudit writes one hash-chained audit event.
//
// The chain has exactly one writer at a time, enforced by an advisory lock
// taken inside the transaction. Without it two concurrent appends could read
// the same head hash and fork the chain, which would look identical to
// tampering during verification.
func (s *ControlStore) AppendAudit(ctx context.Context, q pgx.Tx, e domain.AuditEvent) (domain.AuditEvent, error) {
	if err := db.AdvisoryLock(ctx, q, db.LockAuditChain); err != nil {
		return domain.AuditEvent{}, err
	}

	var headHash string
	var headSeq int64
	if err := q.QueryRow(ctx,
		`SELECT head_hash, head_sequence FROM audit_chain_state WHERE id = TRUE FOR UPDATE`).
		Scan(&headHash, &headSeq); err != nil {
		return domain.AuditEvent{}, mapError(err)
	}

	if e.Metadata == nil {
		e.Metadata = json.RawMessage(`{}`)
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	// Truncate to microseconds BEFORE hashing.
	//
	// TIMESTAMPTZ stores microsecond precision, so a Go timestamp carrying
	// nanoseconds is silently rounded on the way in. Hashing the unrounded
	// value would produce a hash that can never be recomputed from what the
	// database returns, which makes the whole chain unverifiable — a failure
	// that looks exactly like tampering.
	e.OccurredAt = e.OccurredAt.UTC().Truncate(time.Microsecond)
	e.Sequence = headSeq + 1
	e.PrevHash = headHash
	e.Hash = e.ComputeHash(headHash)

	err := q.QueryRow(ctx, `
		INSERT INTO audit_events (sequence, occurred_at, actor_user_id, actor_type, action,
			target_type, target_id, account_id, result, request_id, correlation_id,
			ip_address, user_agent, metadata, prev_hash, hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		RETURNING id`,
		e.Sequence, e.OccurredAt, e.ActorUserID, e.ActorType, string(e.Action),
		e.TargetType, e.TargetID, e.AccountID, string(e.Result), e.RequestID, e.CorrelationID,
		e.IPAddress, e.UserAgent, e.Metadata, e.PrevHash, e.Hash).Scan(&e.ID)
	if err != nil {
		return domain.AuditEvent{}, mapError(err)
	}

	if _, err := q.Exec(ctx, `
		UPDATE audit_chain_state SET head_hash = $1, head_sequence = $2, updated_at = now()
		WHERE id = TRUE`, e.Hash, e.Sequence); err != nil {
		return domain.AuditEvent{}, mapError(err)
	}
	return e, nil
}

// AuditFilter narrows an audit listing.
type AuditFilter struct {
	AccountID *uuid.UUID
	UserID    *uuid.UUID
	Actions   []string
	Since     *time.Time
	Limit     int
	Offset    int
}

// ListAudit returns audit events, newest first.
func (s *ControlStore) ListAudit(ctx context.Context, f AuditFilter) ([]domain.AuditEvent, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args := []any{limit, f.Offset}
	q := `SELECT id, sequence, occurred_at, actor_user_id, actor_type, action, target_type,
	             target_id, account_id, result, request_id, correlation_id, host(ip_address),
	             user_agent, metadata, prev_hash, hash
	      FROM audit_events WHERE 1=1`

	if f.AccountID != nil {
		args = append(args, *f.AccountID)
		q += fmt.Sprintf(" AND account_id = $%d", len(args))
	}
	if f.UserID != nil {
		args = append(args, *f.UserID)
		q += fmt.Sprintf(" AND actor_user_id = $%d", len(args))
	}
	if len(f.Actions) > 0 {
		args = append(args, f.Actions)
		q += fmt.Sprintf(" AND action = ANY($%d)", len(args))
	}
	if f.Since != nil {
		args = append(args, *f.Since)
		q += fmt.Sprintf(" AND occurred_at >= $%d", len(args))
	}
	q += ` ORDER BY sequence DESC LIMIT $1 OFFSET $2`

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	return scanAuditRows(rows)
}

// AuditChainSlice returns events in ascending sequence for verification.
func (s *ControlStore) AuditChainSlice(ctx context.Context, fromSeq int64, limit int) ([]domain.AuditEvent, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, sequence, occurred_at, actor_user_id, actor_type, action, target_type,
		       target_id, account_id, result, request_id, correlation_id, host(ip_address),
		       user_agent, metadata, prev_hash, hash
		FROM audit_events WHERE sequence >= $1 ORDER BY sequence ASC LIMIT $2`, fromSeq, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	return scanAuditRows(rows)
}

func scanAuditRows(rows pgx.Rows) ([]domain.AuditEvent, error) {
	var out []domain.AuditEvent
	for rows.Next() {
		var e domain.AuditEvent
		var raw []byte
		if err := rows.Scan(&e.ID, &e.Sequence, &e.OccurredAt, &e.ActorUserID, &e.ActorType,
			&e.Action, &e.TargetType, &e.TargetID, &e.AccountID, &e.Result, &e.RequestID,
			&e.CorrelationID, &e.IPAddress, &e.UserAgent, &raw, &e.PrevHash, &e.Hash); err != nil {
			return nil, mapError(err)
		}
		e.Metadata = json.RawMessage(raw)
		out = append(out, e)
	}
	return out, mapError(rows.Err())
}

// AuditHead returns the chain's current head.
func (s *ControlStore) AuditHead(ctx context.Context) (string, int64, error) {
	var hash string
	var seq int64
	err := s.pool.QueryRow(ctx,
		`SELECT head_hash, head_sequence FROM audit_chain_state WHERE id = TRUE`).Scan(&hash, &seq)
	return hash, seq, mapError(err)
}

// ---------------------------------------------------------------------------
// Notifications
// ---------------------------------------------------------------------------

// Notification is a user-facing alert.
type Notification struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	AccountID *uuid.UUID
	Severity  string
	Category  string
	Title     string
	Body      string
	Metadata  json.RawMessage
	ReadAt    *time.Time
	CreatedAt time.Time
}

// CreateNotification stores an alert for a user.
func (s *ControlStore) CreateNotification(ctx context.Context, q querier, n Notification) error {
	if q == nil {
		q = s.pool
	}
	if n.Metadata == nil {
		n.Metadata = json.RawMessage(`{}`)
	}
	_, err := q.Exec(ctx, `
		INSERT INTO notifications (user_id, account_id, severity, category, title, body, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		n.UserID, n.AccountID, n.Severity, n.Category, n.Title, n.Body, n.Metadata)
	return mapError(err)
}

// ListNotifications returns a user's notifications.
func (s *ControlStore) ListNotifications(ctx context.Context, userID uuid.UUID, unreadOnly bool, limit int) ([]Notification, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT id, user_id, account_id, severity, category, title, body, metadata, read_at, created_at
	      FROM notifications WHERE user_id = $1`
	if unreadOnly {
		q += ` AND read_at IS NULL`
	}
	q += ` ORDER BY created_at DESC LIMIT $2`

	rows, err := s.pool.Query(ctx, q, userID, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		var n Notification
		var raw []byte
		if err := rows.Scan(&n.ID, &n.UserID, &n.AccountID, &n.Severity, &n.Category,
			&n.Title, &n.Body, &raw, &n.ReadAt, &n.CreatedAt); err != nil {
			return nil, mapError(err)
		}
		n.Metadata = json.RawMessage(raw)
		out = append(out, n)
	}
	return out, mapError(rows.Err())
}

// MarkNotificationRead marks one of the caller's notifications read.
func (s *ControlStore) MarkNotificationRead(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE notifications SET read_at = now() WHERE id = $1 AND user_id = $2 AND read_at IS NULL`,
		id, userID)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
