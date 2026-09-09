package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AutopilotStore reads and writes the global autonomous-trading switch.
//
// See migrations/0012_autopilot.sql for why this is separate from the kill
// switch: a kill switch halts every order including an operator's, while this
// halts only the autonomous pipeline and deliberately leaves manual trading
// and every read path available.
type AutopilotStore struct{ pool querier }

// AutopilotState is the current switch position with its provenance.
type AutopilotState struct {
	Enabled   bool
	Reason    string
	ChangedBy *uuid.UUID
	ChangedAt time.Time
}

// ErrAutopilotReasonRequired means a caller tried to flip the switch without
// saying why.
//
// Deliberately an error rather than a default reason. "Changed via the API" is
// not an account of a decision, and the whole value of the field is that
// somebody had to write something.
var ErrAutopilotReasonRequired = errors.New("store: changing autopilot requires a reason")

// Autopilot returns the current state.
//
// A missing row is reported as DISABLED rather than as an error. Fail closed:
// if the platform cannot establish that autonomous trading was switched on, it
// is off. The alternative -- erroring -- would leave the caller deciding, and
// the caller most likely to get it wrong is a scheduler tick that treats an
// error as "carry on".
func (s *AutopilotStore) Autopilot(ctx context.Context) (AutopilotState, error) {
	var st AutopilotState
	err := s.pool.QueryRow(ctx,
		`SELECT enabled, reason, changed_by, changed_at FROM autopilot_state WHERE id`).
		Scan(&st.Enabled, &st.Reason, &st.ChangedBy, &st.ChangedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AutopilotState{
			Enabled: false,
			Reason:  "no autopilot state is recorded, so autonomous trading is off",
		}, nil
	}
	if err != nil {
		return AutopilotState{}, mapError(err)
	}
	return st, nil
}

// AutopilotEnabledTx reads the switch inside a caller's transaction.
//
// Used by the order pipeline, for the same reason the reconciliation halt is
// read in-transaction: checking before opening the transaction leaves a window
// in which autopilot is switched off microseconds before an automated order
// commits anyway. Either the order commits before the switch, or it sees the
// switch -- decided by PostgreSQL rather than by timing.
func (s *AutopilotStore) AutopilotEnabledTx(ctx context.Context, tx pgx.Tx) (bool, error) {
	var enabled bool
	err := tx.QueryRow(ctx, `SELECT enabled FROM autopilot_state WHERE id`).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, mapError(err)
	}
	return enabled, nil
}

// SetAutopilot changes the switch and appends to its history.
//
// Both writes happen in one transaction: a state change with no history entry
// would leave a period of autonomous trading with no trace, which is the thing
// the history table exists to prevent.
//
// Idempotent by design rather than by check -- setting it to its current value
// still records an entry, because "somebody confirmed autopilot should be on,
// at this time, for this reason" is information worth keeping.
func (s *AutopilotStore) SetAutopilot(ctx context.Context, tx pgx.Tx, enabled bool,
	reason string, actor *uuid.UUID) (AutopilotState, error) {

	reason = strings.TrimSpace(reason)
	if reason == "" {
		return AutopilotState{}, ErrAutopilotReasonRequired
	}

	var st AutopilotState
	err := tx.QueryRow(ctx, `
		INSERT INTO autopilot_state (id, enabled, reason, changed_by, changed_at)
		VALUES (true, $1, $2, $3, now())
		ON CONFLICT (id) DO UPDATE
		SET enabled = EXCLUDED.enabled,
		    reason = EXCLUDED.reason,
		    changed_by = EXCLUDED.changed_by,
		    changed_at = EXCLUDED.changed_at
		RETURNING enabled, reason, changed_by, changed_at`,
		enabled, reason, actor).
		Scan(&st.Enabled, &st.Reason, &st.ChangedBy, &st.ChangedAt)
	if err != nil {
		return AutopilotState{}, mapError(err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO autopilot_state_history (enabled, reason, changed_by, changed_at)
		VALUES ($1, $2, $3, $4)`, st.Enabled, st.Reason, st.ChangedBy, st.ChangedAt); err != nil {
		return AutopilotState{}, mapError(err)
	}
	return st, nil
}

// AutopilotHistory returns the switch's history, newest first.
func (s *AutopilotStore) AutopilotHistory(ctx context.Context, limit int) ([]AutopilotState, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT enabled, reason, changed_by, changed_at
		FROM autopilot_state_history
		ORDER BY changed_at DESC, id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var out []AutopilotState
	for rows.Next() {
		var st AutopilotState
		if err := rows.Scan(&st.Enabled, &st.Reason, &st.ChangedBy, &st.ChangedAt); err != nil {
			return nil, mapError(err)
		}
		out = append(out, st)
	}
	return out, mapError(rows.Err())
}
