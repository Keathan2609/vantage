package store

// Reconciliation issue persistence.
//
// Two properties drive the design here:
//
//  1. An issue is identified by the PROBLEM, not by the run that found it.
//     Reconciliation runs on a schedule, so the same divergence is detected
//     repeatedly. Inserting a row per detection would turn one problem into
//     hundreds and make the operator view useless within an hour. The
//     fingerprint plus a partial unique index on unresolved rows gives
//     "upsert the open issue, count the sightings".
//
//  2. Resolution is append-only history, not a mutated field. An issue's
//     current state is a summary; the sequence of what was decided about it is
//     the record, and a financial repair needs the record.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vantage/control-api/internal/db"
	"github.com/vantage/control-api/internal/domain"
)

// ReconciliationStore persists issues, their history and per-account state.
type ReconciliationStore struct {
	pool *db.Pool
}

// Issue is a persisted reconciliation issue.
type Issue struct {
	ID          uuid.UUID
	RunID       uuid.UUID
	AccountID   uuid.UUID
	BrokerName  string
	Type        domain.IssueType
	Severity    domain.IssueSeverity
	Status      domain.IssueStatus
	RepairClass domain.RepairClass

	OrderID           *uuid.UUID
	PositionID        *uuid.UUID
	InstrumentID      *string
	BrokerOrderID     *string
	BrokerExecutionID *string

	LocalSnapshot  json.RawMessage
	BrokerSnapshot json.RawMessage
	Evidence       json.RawMessage
	Description    string
	Fingerprint    string
	CorrelationID  string

	DetectedAt    time.Time
	LastCheckedAt time.Time
	CheckCount    int

	ResolvedAt       *time.Time
	ResolutionAction *domain.ResolutionAction
	ResolutionReason *string
	ResolvedBy       *uuid.UUID
}

// Open reports whether the issue still demands attention.
func (i Issue) Open() bool { return i.ResolvedAt == nil }

// IssueEvent is one entry in an issue's history.
type IssueEvent struct {
	ID            int64
	IssueID       uuid.UUID
	Action        string
	FromStatus    *string
	ToStatus      string
	ActorType     string
	ActorUserID   *uuid.UUID
	Reason        string
	Evidence      json.RawMessage
	CorrelationID string
	OccurredAt    time.Time
}

// Fingerprint builds the identity of a real-world problem, independent of the
// run that found it.
//
// Deliberately excludes anything that changes between detections — timestamps,
// observed quantities, the run id. Including an observed value would make
// every re-detection a new problem, which is the bug this function exists to
// prevent.
func Fingerprint(t domain.IssueType, parts ...string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(t))
	for _, p := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(p))
	}
	return fmt.Sprintf("%s:%016x", t, h.Sum64())
}

const issueColumns = `id, run_id, account_id, broker_name, issue_type, severity, status,
	repair_class, order_id, position_id, instrument_id, broker_order_id, broker_execution_id,
	local_snapshot, broker_snapshot, evidence, description, fingerprint, correlation_id,
	detected_at, last_checked_at, check_count, resolved_at, resolution_action,
	resolution_reason, resolved_by`

func scanIssue(row pgx.Row) (Issue, error) {
	var i Issue
	var issueType, severity, status, repairClass string
	var action *string
	err := row.Scan(&i.ID, &i.RunID, &i.AccountID, &i.BrokerName, &issueType, &severity,
		&status, &repairClass, &i.OrderID, &i.PositionID, &i.InstrumentID, &i.BrokerOrderID,
		&i.BrokerExecutionID, &i.LocalSnapshot, &i.BrokerSnapshot, &i.Evidence,
		&i.Description, &i.Fingerprint, &i.CorrelationID, &i.DetectedAt, &i.LastCheckedAt,
		&i.CheckCount, &i.ResolvedAt, &action, &i.ResolutionReason, &i.ResolvedBy)
	if err != nil {
		return Issue{}, mapError(err)
	}
	i.Type = domain.IssueType(issueType)
	i.Severity = domain.IssueSeverity(severity)
	i.Status = domain.IssueStatus(status)
	i.RepairClass = domain.RepairClass(repairClass)
	if action != nil {
		a := domain.ResolutionAction(*action)
		i.ResolutionAction = &a
	}
	return i, nil
}

// UpsertIssue records a detection, returning the issue and whether it is new.
//
// An existing OPEN issue with the same fingerprint is touched rather than
// duplicated: its last_checked_at and check_count advance and its severity may
// be RAISED. Severity is never lowered here, because the code that found the
// problem must not also be able to decide it stopped mattering.
func (s *ReconciliationStore) UpsertIssue(ctx context.Context, tx pgx.Tx, in Issue) (Issue, bool, error) {
	if in.Fingerprint == "" {
		return Issue{}, false, errors.New("store: a reconciliation issue needs a fingerprint")
	}
	if in.LocalSnapshot == nil {
		in.LocalSnapshot = json.RawMessage(`{}`)
	}
	if in.BrokerSnapshot == nil {
		in.BrokerSnapshot = json.RawMessage(`{}`)
	}
	if in.Evidence == nil {
		in.Evidence = json.RawMessage(`{}`)
	}

	// The DO UPDATE branch is what makes a 60-second reconciliation loop
	// produce one issue rather than 1,440 a day.
	row := tx.QueryRow(ctx, `
		INSERT INTO reconciliation_issues (
			run_id, account_id, broker_name, issue_type, severity, status, repair_class,
			order_id, position_id, instrument_id, broker_order_id, broker_execution_id,
			local_snapshot, broker_snapshot, evidence, description, fingerprint, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		ON CONFLICT (account_id, fingerprint) WHERE resolved_at IS NULL
		DO UPDATE SET
			last_checked_at = now(),
			check_count = reconciliation_issues.check_count + 1,
			-- Severity ratchets upward only.
			severity = CASE
				WHEN EXCLUDED.severity = 'critical' THEN 'critical'
				WHEN EXCLUDED.severity = 'warning'
					AND reconciliation_issues.severity = 'info' THEN 'warning'
				ELSE reconciliation_issues.severity
			END,
			-- The newest evidence replaces the oldest: an operator wants to see
			-- what the venue says NOW, not what it said when first detected.
			local_snapshot = EXCLUDED.local_snapshot,
			broker_snapshot = EXCLUDED.broker_snapshot,
			evidence = EXCLUDED.evidence,
			description = EXCLUDED.description
		RETURNING `+issueColumns+`, (check_count = 1) AS is_new`,
		in.RunID, in.AccountID, in.BrokerName, string(in.Type), string(in.Severity),
		string(in.Status), string(in.RepairClass), in.OrderID, in.PositionID,
		in.InstrumentID, in.BrokerOrderID, in.BrokerExecutionID,
		in.LocalSnapshot, in.BrokerSnapshot, in.Evidence, in.Description,
		in.Fingerprint, in.CorrelationID)

	var out Issue
	var issueType, severity, status, repairClass string
	var action *string
	var isNew bool
	err := row.Scan(&out.ID, &out.RunID, &out.AccountID, &out.BrokerName, &issueType,
		&severity, &status, &repairClass, &out.OrderID, &out.PositionID, &out.InstrumentID,
		&out.BrokerOrderID, &out.BrokerExecutionID, &out.LocalSnapshot, &out.BrokerSnapshot,
		&out.Evidence, &out.Description, &out.Fingerprint, &out.CorrelationID,
		&out.DetectedAt, &out.LastCheckedAt, &out.CheckCount, &out.ResolvedAt, &action,
		&out.ResolutionReason, &out.ResolvedBy, &isNew)
	if err != nil {
		return Issue{}, false, mapError(err)
	}
	out.Type = domain.IssueType(issueType)
	out.Severity = domain.IssueSeverity(severity)
	out.Status = domain.IssueStatus(status)
	out.RepairClass = domain.RepairClass(repairClass)
	if action != nil {
		a := domain.ResolutionAction(*action)
		out.ResolutionAction = &a
	}
	return out, isNew, nil
}

// IssueByIDTx loads one issue for update.
//
// Takes the row lock, because every resolution path reads the issue, decides
// on the strength of its status, and writes. Without the lock two operators
// clicking at once could both pass the "is it still open" check.
func (s *ReconciliationStore) IssueByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Issue, error) {
	return scanIssue(tx.QueryRow(ctx,
		`SELECT `+issueColumns+` FROM reconciliation_issues WHERE id = $1 FOR UPDATE`, id))
}

// IssueForAccount loads one issue, scoped to an account.
//
// The account is part of the WHERE clause rather than checked afterwards, so a
// forged or guessed issue id belonging to another account returns not-found
// rather than data. An authorisation check written as an `if` after the read is
// one refactor away from being dropped.
func (s *ReconciliationStore) IssueForAccount(ctx context.Context, accountID, id uuid.UUID) (Issue, error) {
	return scanIssue(s.pool.QueryRow(ctx,
		`SELECT `+issueColumns+` FROM reconciliation_issues
		 WHERE id = $1 AND account_id = $2`, id, accountID))
}

// IssueFilter narrows an issue listing.
type IssueFilter struct {
	AccountID *uuid.UUID
	OpenOnly  bool
	Severity  []domain.IssueSeverity
	Types     []domain.IssueType
	OrderID   *uuid.UUID
	Limit     int
}

// ListIssues returns issues newest-first.
func (s *ReconciliationStore) ListIssues(ctx context.Context, f IssueFilter) ([]Issue, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + issueColumns + ` FROM reconciliation_issues WHERE 1 = 1`
	args := []any{}
	add := func(clause string, v any) {
		args = append(args, v)
		query += fmt.Sprintf(clause, len(args))
	}
	if f.AccountID != nil {
		add(" AND account_id = $%d", *f.AccountID)
	}
	if f.OrderID != nil {
		add(" AND order_id = $%d", *f.OrderID)
	}
	if f.OpenOnly {
		query += " AND resolved_at IS NULL"
	}
	if len(f.Severity) > 0 {
		vals := make([]string, 0, len(f.Severity))
		for _, v := range f.Severity {
			vals = append(vals, string(v))
		}
		add(" AND severity = ANY($%d)", vals)
	}
	if len(f.Types) > 0 {
		vals := make([]string, 0, len(f.Types))
		for _, v := range f.Types {
			vals = append(vals, string(v))
		}
		add(" AND issue_type = ANY($%d)", vals)
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY detected_at DESC LIMIT $%d", len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var out []Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, mapError(rows.Err())
}

// OpenIssues returns every unresolved issue for an account.
func (s *ReconciliationStore) OpenIssues(ctx context.Context, accountID uuid.UUID) ([]Issue, error) {
	return s.ListIssues(ctx, IssueFilter{AccountID: &accountID, OpenOnly: true, Limit: 500})
}

// OpenIssuesTx returns every unresolved issue for an account, inside a
// transaction.
//
// Used by the OMS to decide whether automation may proceed, in the same
// transaction that persists the order. Reading this outside the transaction
// would make the halt timing-dependent: an order could pass the check
// microseconds before a repair wrote a critical issue and still commit.
func (s *ReconciliationStore) OpenIssuesTx(ctx context.Context, tx pgx.Tx, accountID uuid.UUID) ([]Issue, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+issueColumns+` FROM reconciliation_issues
		 WHERE account_id = $1 AND resolved_at IS NULL
		 ORDER BY detected_at DESC LIMIT 500`, accountID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, mapError(rows.Err())
}

// OpenIssuesForBroker returns unresolved issues across every account on one
// broker connection, for the BROKER_CONNECTION halt scope.
func (s *ReconciliationStore) OpenIssuesForBroker(ctx context.Context, brokerName string) ([]Issue, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+issueColumns+` FROM reconciliation_issues
		 WHERE broker_name = $1 AND resolved_at IS NULL
		 ORDER BY detected_at DESC LIMIT 500`, brokerName)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, mapError(rows.Err())
}

// SetIssueStatusTx moves an unresolved issue to a new non-terminal status.
func (s *ReconciliationStore) SetIssueStatusTx(ctx context.Context, tx pgx.Tx,
	id uuid.UUID, status domain.IssueStatus) error {

	if !status.Open() {
		return fmt.Errorf(
			"store: %s is a resolved status; use ResolveIssueTx so a reason is recorded", status)
	}
	_, err := tx.Exec(ctx,
		`UPDATE reconciliation_issues SET status = $2 WHERE id = $1 AND resolved_at IS NULL`,
		id, string(status))
	return mapError(err)
}

// ResolveIssueTx closes an issue.
//
// A reason is mandatory and enforced by a CHECK constraint as well as here: an
// issue closed with no explanation is the record an operator needs and does not
// have when the same divergence reappears next month.
func (s *ReconciliationStore) ResolveIssueTx(ctx context.Context, tx pgx.Tx, id uuid.UUID,
	status domain.IssueStatus, action domain.ResolutionAction, reason string,
	resolvedBy *uuid.UUID) error {

	if status.Open() {
		return fmt.Errorf("store: %s is not a resolved status", status)
	}
	if reason == "" {
		return errors.New("store: resolving a reconciliation issue requires a reason")
	}
	tag, err := tx.Exec(ctx, `
		UPDATE reconciliation_issues
		SET status = $2, resolved_at = now(), resolution_action = $3,
		    resolution_reason = $4, resolved_by = $5
		WHERE id = $1 AND resolved_at IS NULL`,
		id, string(status), string(action), reason, resolvedBy)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		// Either the id is wrong or someone else resolved it first. Both are
		// conflicts, not internal errors, and the caller turns this into a 409.
		return ErrAlreadyResolved
	}
	return nil
}

// ErrAlreadyResolved means the issue was closed before this attempt landed.
var ErrAlreadyResolved = errors.New("store: this reconciliation issue is already resolved")

// AppendIssueEventTx records one step in an issue's history.
func (s *ReconciliationStore) AppendIssueEventTx(ctx context.Context, tx pgx.Tx, e IssueEvent) error {
	if e.Reason == "" {
		return errors.New("store: a reconciliation issue event requires a reason")
	}
	if e.Evidence == nil {
		e.Evidence = json.RawMessage(`{}`)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO reconciliation_issue_events (issue_id, action, from_status, to_status,
			actor_type, actor_user_id, reason, evidence, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		e.IssueID, e.Action, e.FromStatus, e.ToStatus, e.ActorType, e.ActorUserID,
		e.Reason, e.Evidence, e.CorrelationID)
	return mapError(err)
}

// IssueHistory returns an issue's events oldest-first.
func (s *ReconciliationStore) IssueHistory(ctx context.Context, issueID uuid.UUID) ([]IssueEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, issue_id, action, from_status, to_status, actor_type, actor_user_id,
		       reason, evidence, correlation_id, occurred_at
		FROM reconciliation_issue_events WHERE issue_id = $1 ORDER BY occurred_at, id`, issueID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []IssueEvent
	for rows.Next() {
		var e IssueEvent
		if err := rows.Scan(&e.ID, &e.IssueID, &e.Action, &e.FromStatus, &e.ToStatus,
			&e.ActorType, &e.ActorUserID, &e.Reason, &e.Evidence, &e.CorrelationID,
			&e.OccurredAt); err != nil {
			return nil, mapError(err)
		}
		out = append(out, e)
	}
	return out, mapError(rows.Err())
}

// ---------------------------------------------------------------------------
// Per-account state
// ---------------------------------------------------------------------------

// AccountState is the scheduling and coverage record for one account.
//
// The trading verdict is NOT stored here: it is derived from open issues so it
// cannot drift out of agreement with them. What is stored is what cannot be
// derived — whether the account has ever been reconciled, and from where to
// resume polling executions.
type AccountState struct {
	AccountID           uuid.UUID
	BrokerName          string
	LastRunID           *uuid.UUID
	LastStartedAt       *time.Time
	LastSuccessAt       *time.Time
	LastStatus          *string
	ConsecutiveFailures int
	ExecutionsCursor    *time.Time
	UpdatedAt           time.Time
}

// Reconciled reports whether the account has ever completed a run.
//
// "No open issues" is true of an account nobody has ever checked, and that is
// not the same as agreement with the venue. This is the difference.
func (a AccountState) Reconciled() bool { return a.LastSuccessAt != nil }

// AccountStateFor loads an account's reconciliation state, or a zero value
// with the broker name set when the account has never been reconciled.
func (s *ReconciliationStore) AccountStateFor(ctx context.Context, accountID uuid.UUID) (AccountState, error) {
	var a AccountState
	err := s.pool.QueryRow(ctx, `
		SELECT account_id, broker_name, last_run_id, last_started_at, last_success_at,
		       last_status, consecutive_failures, executions_cursor, updated_at
		FROM reconciliation_account_state WHERE account_id = $1`, accountID).
		Scan(&a.AccountID, &a.BrokerName, &a.LastRunID, &a.LastStartedAt, &a.LastSuccessAt,
			&a.LastStatus, &a.ConsecutiveFailures, &a.ExecutionsCursor, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountState{AccountID: accountID}, nil
	}
	if err != nil {
		return AccountState{}, mapError(err)
	}
	return a, nil
}

// RecordRunStarted notes that a run began.
func (s *ReconciliationStore) RecordRunStarted(ctx context.Context, accountID uuid.UUID,
	brokerName string, runID uuid.UUID) error {

	_, err := s.pool.Exec(ctx, `
		INSERT INTO reconciliation_account_state
			(account_id, broker_name, last_run_id, last_started_at, last_status, updated_at)
		VALUES ($1, $2, $3, now(), 'running', now())
		ON CONFLICT (account_id) DO UPDATE SET
			broker_name = EXCLUDED.broker_name,
			last_run_id = EXCLUDED.last_run_id,
			last_started_at = now(),
			last_status = 'running',
			updated_at = now()`, accountID, brokerName, runID)
	return mapError(err)
}

// RecordRunFinished notes the outcome of a run.
//
// A failure increments the consecutive counter rather than resetting it, which
// is what lets alerting distinguish "the venue blipped" from "reconciliation
// has been broken for an hour".
func (s *ReconciliationStore) RecordRunFinished(ctx context.Context, accountID uuid.UUID,
	status string, succeeded bool, cursor *time.Time) error {

	_, err := s.pool.Exec(ctx, `
		UPDATE reconciliation_account_state
		SET last_status = $2,
		    last_success_at = CASE WHEN $3 THEN now() ELSE last_success_at END,
		    consecutive_failures = CASE WHEN $3 THEN 0 ELSE consecutive_failures + 1 END,
		    executions_cursor = COALESCE($4, executions_cursor),
		    updated_at = now()
		WHERE account_id = $1`, accountID, status, succeeded, cursor)
	return mapError(err)
}

// ---------------------------------------------------------------------------
// Order flags
// ---------------------------------------------------------------------------

// SetOrderReconciliationRequiredTx marks or clears an order's unknown-outcome
// flag.
//
// Separate from the status, deliberately. FAILED means "the venue-side outcome
// is unknown", which is correct but reads like a closed failure; this flag
// makes the uncertainty queryable, visible in the UI and countable in
// readiness without adding a status that would weaken the state machine for
// every ordinary execution.
func (s *TradingStore) SetOrderReconciliationRequiredTx(ctx context.Context, tx pgx.Tx,
	orderID uuid.UUID, required bool) error {

	// version is deliberately NOT bumped: this is metadata about Vantage's
	// knowledge, not a change to the order's financial content, and bumping it
	// would make a concurrent optimistic update fail for no good reason.
	_, err := tx.Exec(ctx,
		`UPDATE orders SET reconciliation_required = $2, updated_at = now() WHERE id = $1`,
		orderID, required)
	return mapError(err)
}

// OrdersRequiringReconciliation lists an account's orders whose venue-side
// outcome is unknown.
func (s *TradingStore) OrdersRequiringReconciliation(ctx context.Context,
	accountID uuid.UUID) ([]domain.Order, error) {

	rows, err := s.pool.Query(ctx,
		`SELECT `+orderColumns+`
		 FROM orders o JOIN instruments i ON i.id = o.instrument_id
		 WHERE o.account_id = $1 AND o.reconciliation_required
		 ORDER BY o.created_at DESC LIMIT 200`, accountID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var out []domain.Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, mapError(rows.Err())
}

// RepairTransitionTx applies a reconciliation repair to an order's state.
//
// # Why this is not TransitionOrderWithCodeTx with a flag
//
// It uses domain.CanRepairTransition, which permits moves the normal state
// machine forbids — ACCEPTED -> FILLED being the important one. Making that
// reachable through the ordinary transition function would mean every OMS call
// site could reach it too, and the state machine's job is to catch exactly
// that kind of mistake during normal execution.
//
// Every repair records the issue that justified it and is stamped is_repair,
// so reading the history can distinguish "the venue told us at the time" from
// "we reconstructed this afterwards from a snapshot".
func (s *TradingStore) RepairTransitionTx(ctx context.Context, tx pgx.Tx, orderID uuid.UUID,
	to domain.OrderStatus, code domain.RejectCode, reason string, issueID uuid.UUID,
	actorType string, actorUserID *uuid.UUID, evidence map[string]any) (domain.Order, error) {

	current, err := s.OrderByIDTx(ctx, tx, orderID)
	if err != nil {
		return domain.Order{}, err
	}
	if current.Status == to {
		// Idempotent. Re-running reconciliation against the same snapshot must
		// not fail, and must not append a second identical transition.
		return current, nil
	}
	if !domain.CanRepairTransition(current.Status, to) {
		return domain.Order{}, domain.ErrIllegalRepair{From: current.Status, To: to}
	}

	closedAt := "closed_at"
	if to.Terminal() {
		closedAt = "COALESCE(closed_at, now())"
	}
	submittedAt := "submitted_at"
	if to == domain.OrderSubmitted {
		submittedAt = "COALESCE(submitted_at, now())"
	}

	rejectCode := any(nil)
	rejectReason := any(nil)
	if to == domain.OrderRejected {
		if code == "" {
			code = domain.RejectInternalError
		}
		rejectCode = string(code)
		rejectReason = reason
	}

	// No optimistic version check: a repair is authoritative reconstruction of
	// what the venue did, and it holds the account row lock. Requiring a
	// version match would make recovery fail whenever anything else had
	// touched the order since the snapshot was taken, which is common.
	tag, err := tx.Exec(ctx, fmt.Sprintf(`
		UPDATE orders
		SET status = $2, updated_at = now(), version = version + 1,
		    closed_at = %s, submitted_at = %s,
		    reject_code = COALESCE(reject_code, $3),
		    reject_reason = COALESCE(NULLIF(reject_reason, ''), $4)
		WHERE id = $1`, closedAt, submittedAt),
		orderID, to, rejectCode, rejectReason)
	if err != nil {
		return domain.Order{}, mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Order{}, ErrNotFound
	}

	from := current.Status
	metadata := map[string]any{
		"repair":                  true,
		"reconciliation_issue_id": issueID.String(),
		"previous_status":         string(from),
	}
	for k, v := range evidence {
		metadata[k] = v
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return domain.Order{}, fmt.Errorf("store: encode repair metadata: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO order_state_transitions (order_id, from_status, to_status, reason,
			actor_type, actor_user_id, metadata, reconciliation_issue_id, is_repair)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,TRUE)`,
		orderID, string(from), string(to), reason, actorType, actorUserID,
		encoded, issueID); err != nil {
		return domain.Order{}, mapError(err)
	}

	return s.OrderByIDTx(ctx, tx, orderID)
}

// RepairTransitions returns the repair entries in an order's history, which is
// how the UI shows that a state was reconstructed rather than received.
func (s *TradingStore) RepairTransitions(ctx context.Context, orderID uuid.UUID) ([]OrderTransition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT from_status, to_status, reason, actor_type, occurred_at,
		       reconciliation_issue_id
		FROM order_state_transitions
		WHERE order_id = $1 AND is_repair
		ORDER BY occurred_at`, orderID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []OrderTransition
	for rows.Next() {
		var t OrderTransition
		if err := rows.Scan(&t.FromStatus, &t.ToStatus, &t.Reason, &t.ActorType,
			&t.OccurredAt, &t.ReconciliationIssueID); err != nil {
			return nil, mapError(err)
		}
		t.IsRepair = true
		out = append(out, t)
	}
	return out, mapError(rows.Err())
}
