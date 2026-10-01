// Package reconcile compares Vantage's view of an account with the venue's,
// repairs what it can prove, and holds open what it cannot.
//
// The premise: Vantage's state is a CACHE of the venue's, and caches go stale.
// Responses get lost, processes die mid-write, brokers cancel orders on their
// own, and people place trades in the broker's own terminal. Any system that
// assumes its local records match the venue will eventually size a position
// against exposure that does not exist.
//
// # The shape of a run
//
//  1. take the account's reconciliation lock, or decline to run
//  2. capture the venue's snapshot
//  3. capture Vantage's snapshot
//  4. classify the differences (pure; see classify.go)
//  5. persist each as an issue, deduplicated by fingerprint
//  6. apply the repairs the domain policy calls provable
//  7. record the run and update readiness
//
// Steps 2 and 3 happen before any repair, so a run is a pure function of two
// snapshots plus the repair policy -- which is what makes it reproducible and
// unit-testable.
//
// # Where the difference goes
//
// The VENUE WINS, but only where the correct repair is provable. Where it is
// not, the issue stays open and automated trading for the account stops.
// Trading on a position book known to be wrong is worse than not trading, and
// guessing at an ambiguous execution is worse than both.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/booking"
	"github.com/vantage/control-api/internal/broker"
	"github.com/vantage/control-api/internal/db"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/metrics"
	"github.com/vantage/control-api/internal/store"
)

// Service performs reconciliation.
type Service struct {
	store   *store.Store
	brokers *broker.Registry
	booking *booking.Service
	clock   domain.Clock
	alerter Alerter

	// balanceTolerance bounds an acceptable balance difference. Explicit
	// rather than assumed, because in paper mode the venue computes its own
	// balance and some difference is expected by design.
	balanceTolerance decimal.Decimal
}

// Alerter is the subset of internal/notify this service needs, declared here
// so reconcile does not import notify.
type Alerter interface {
	ReconciliationMismatch(ctx context.Context, accountID uuid.UUID, critical, total int, kinds []string)
	ReconciliationClean(ctx context.Context, accountID uuid.UUID)
	ReconciliationIssueRaised(ctx context.Context, accountID uuid.UUID, issueType, severity, description string)
	ReconciliationRepaired(ctx context.Context, accountID uuid.UUID, issueType, action, detail string)
	ReconciliationFailed(ctx context.Context, accountID uuid.UUID, consecutive int, cause string)
	TradingHalted(ctx context.Context, accountID uuid.UUID, scope, reason string)
	TradingResumed(ctx context.Context, accountID uuid.UUID)
}

// SetAlerter attaches an alerter after construction.
func (s *Service) SetAlerter(a Alerter) { s.alerter = a }

// New builds the reconciliation service.
func New(s *store.Store, brokers *broker.Registry, bk *booking.Service, clock domain.Clock) *Service {
	return &Service{
		store: s, brokers: brokers, booking: bk, clock: clock,
		// Two currency units. Wide enough that paper-mode rounding does not
		// raise an issue every minute, narrow enough that a missing execution
		// on a R500 account does not hide inside it.
		balanceTolerance: decimal.RequireFromString("2.00"),
	}
}

// Trigger describes why a run happened.
type Trigger string

const (
	TriggerStartup     Trigger = "startup"
	TriggerScheduled   Trigger = "scheduled"
	TriggerManual      Trigger = "manual"
	TriggerReconnect   Trigger = "reconnect"
	TriggerPostFailure Trigger = "post_failure"
)

// executionFirstRunLookback is how far back the FIRST execution fetch reaches
// on an account that has never reconciled.
//
// It applies only when there is no cursor. Once a cursor exists it anchors the
// window on its own, however old it has become -- see runLocked. Treating this
// as a floor on every run is what let a frozen cursor be overtaken, and a
// frozen cursor is the whole mechanism that stops an unresolved execution
// issue closing itself.
const executionFirstRunLookback = 7 * 24 * time.Hour

// Report summarises one run.
type Report struct {
	RunID             uuid.UUID
	AccountID         uuid.UUID
	Trigger           Trigger
	Status            string
	OrdersCompared    int
	PositionsCompared int
	ExecutionsSeen    int
	Issues            []store.Issue
	Repaired          int
	StartedAt         time.Time
	FinishedAt        time.Time
	// Skipped is true when another run held the lock. Not a failure: it is
	// overlap prevention working.
	Skipped bool
}

// Clean reports whether the two views agreed, or every difference was
// repaired.
func (r Report) Clean() bool {
	for _, i := range r.Issues {
		if i.Open() {
			return false
		}
	}
	return true
}

// CriticalCount counts unresolved issues that halt automation.
func (r Report) CriticalCount() int {
	n := 0
	for _, i := range r.Issues {
		if i.Open() && i.Severity == domain.SeverityCriticalIssue {
			n++
		}
	}
	return n
}

// ErrRunInProgress means another run holds the account's lock.
var ErrRunInProgress = errors.New("reconcile: a run is already in progress for this account")

// accountLockObject derives the advisory lock object id for an account.
//
// A 64-bit UUID hashed into 32 bits collides eventually. A collision here
// costs one account waiting for another's run -- a small, self-correcting
// serialisation, not a correctness problem -- which is the right trade for a
// lock that PostgreSQL enforces across processes.
func accountLockObject(accountID uuid.UUID) int32 {
	h := fnv.New32a()
	_, _ = h.Write(accountID[:])
	return int32(h.Sum32()) //nolint:gosec // intentional truncation; see above
}

// Run reconciles one account.
//
// Declines rather than queues when another run is in progress. A scheduled run
// that piled up behind a slow one would eventually run against a stale
// snapshot, and reconciling from stale data is how a repair gets applied twice.
func (s *Service) Run(ctx context.Context, account domain.Account, trigger Trigger) (Report, error) {
	log := logging.FromContext(ctx)
	report := Report{AccountID: account.ID, Trigger: trigger, StartedAt: s.clock.Now()}

	lock, acquired, err := s.store.Pool().TryAcquireSessionLock(
		ctx, db.LockReconciliationAccount, accountLockObject(account.ID))
	if err != nil {
		return report, fmt.Errorf("reconcile: acquire account lock: %w", err)
	}
	if !acquired {
		metrics.ReconciliationOverlapsPrevented.Inc()
		log.Info("reconciliation skipped: a run is already in progress",
			"account_id", account.ID.String(), "trigger", trigger)
		report.Skipped = true
		report.Status = "skipped"
		report.FinishedAt = s.clock.Now()
		return report, nil
	}
	defer lock.Release(ctx)

	return s.runLocked(ctx, account, trigger, report)
}

// runLocked performs a run with the account lock already held.
func (s *Service) runLocked(ctx context.Context, account domain.Account,
	trigger Trigger, report Report) (Report, error) {

	log := logging.FromContext(ctx)

	adapter, err := s.brokers.Get(account.BrokerName)
	if err != nil {
		return report, fmt.Errorf("reconcile: %w", err)
	}

	runID, err := s.store.Research.StartReconciliationRun(
		ctx, account.ID, account.BrokerName, string(trigger))
	if err != nil {
		return report, fmt.Errorf("reconcile: start run: %w", err)
	}
	report.RunID = runID
	if err := s.store.Reconcile.RecordRunStarted(ctx, account.ID, account.BrokerName, runID); err != nil {
		return report, fmt.Errorf("reconcile: record run start: %w", err)
	}

	accountRef := account.ID.String()
	if account.BrokerAcctRef != nil && *account.BrokerAcctRef != "" {
		accountRef = *account.BrokerAcctRef
	}

	// Resume execution polling from the stored cursor. On a first run there is
	// none, so a bounded lookback is used rather than the beginning of time:
	// replaying every execution ever recorded would be correct but slow, and
	// anything older than the lookback is already reflected in the position
	// book or has been an open issue for a week.
	state, err := s.store.Reconcile.AccountStateFor(ctx, account.ID)
	if err != nil {
		return report, fmt.Errorf("reconcile: load account state: %w", err)
	}
	// The seven days are a FIRST-RUN default and nothing else.
	//
	// Once a cursor exists the window is anchored on it, however old it has
	// become. The previous form only honoured the cursor while it was newer
	// than `now - 7d`, which quietly undid the freeze below: a cursor held
	// still by an unresolved execution issue eventually falls behind the
	// seven-day floor, `since` snaps forward past it, `captureBroker` stops
	// fetching the execution that is being argued about, `Classify` cannot
	// produce its fingerprint, and `closeVanishedIssues` resolves the issue
	// with "the divergence is gone: this run re-examined the same evidence and
	// did not find it".
	//
	// It had not re-examined anything. That is the precise failure the freeze
	// exists to prevent -- an unbooked venue execution closing its own issue
	// and the account releasing its own halt -- rebuilt out of a lookback
	// constant, and it would have fired seven days in.
	//
	// The cost of anchoring is a fetch window that grows while something is
	// unresolved. That is already the accepted trade below, and it is bounded
	// by an operator resolving the issue.
	since := s.clock.Now().Add(-executionFirstRunLookback)
	if state.ExecutionsCursor != nil {
		// Overlap the cursor slightly. An execution recorded with a timestamp
		// marginally before the cursor would otherwise be skipped forever, and
		// re-seeing one is free: the unique index refuses the duplicate.
		since = state.ExecutionsCursor.Add(-time.Minute)
	}

	// Local first, so the venue capture knows which client order ids to
	// resolve directly. FetchOpenOrders omits filled and cancelled orders, and
	// an order absent from that list is not thereby unknown to the venue --
	// see captureBroker.
	local, err := s.captureLocal(ctx, account)
	if err != nil {
		s.failRun(ctx, account, runID, err)
		return report, fmt.Errorf("reconcile: capture local snapshot: %w", err)
	}
	clientIDs := make([]string, 0, len(local.Orders))
	for _, o := range local.Orders {
		clientIDs = append(clientIDs, o.CommandID.String())
	}

	remote, err := s.captureBroker(ctx, adapter, accountRef, since, clientIDs)
	if err != nil {
		s.failRun(ctx, account, runID, err)
		return report, fmt.Errorf("reconcile: capture venue snapshot: %w", err)
	}

	report.OrdersCompared = len(local.Orders) + len(remote.Orders)
	report.PositionsCompared = len(local.Positions) + len(remote.Positions)
	report.ExecutionsSeen = len(remote.Executions)

	findings := Classify(local, remote, s.balanceTolerance)

	correlationID := logging.CorrelationID(ctx)
	for _, f := range findings {
		issue, repaired, err := s.persistAndRepair(ctx, account, runID, correlationID, f, local, remote)
		if err != nil {
			// One issue failing to persist or repair must not abandon the
			// others: the remaining findings may include the very execution
			// that explains this one.
			log.Error("reconciliation could not process a finding",
				"account_id", account.ID.String(), "issue_type", string(f.Type),
				"error", err.Error())
			continue
		}
		report.Issues = append(report.Issues, issue)
		if repaired {
			report.Repaired++
		}
	}

	// Close issues whose divergence is gone.
	//
	// Nothing else does this, and without it every TRANSIENT divergence halts
	// an account permanently: a snapshot taken mid-execution raises a
	// PARTIAL_FILL_MISMATCH, the next run finds the quantities agree, and the
	// original issue stays open forever because no code path ever revisits it.
	//
	// It is also how a repair applied later in the SAME run tidies up after
	// itself. Classification happens before any repair, so an order-level
	// mismatch is classified from the pre-repair snapshot and is stale the
	// moment the missing execution is imported a few findings later.
	detected := make(map[string]bool, len(findings))
	for _, f := range findings {
		detected[f.Fingerprint] = true
	}
	if err := s.closeVanishedIssues(ctx, account, runID, correlationID, detected,
		report.StartedAt); err != nil {
		log.Error("reconciliation could not close resolved issues",
			"account_id", account.ID.String(), "error", err.Error())
	}

	// Clear the unknown-outcome flag on orders no longer referenced by any
	// unresolved issue. Without this an order stays flagged after its issue is
	// resolved, and the readiness verdict never recovers.
	if err := s.refreshOrderFlags(ctx, account); err != nil {
		log.Error("reconciliation could not refresh order flags",
			"account_id", account.ID.String(), "error", err.Error())
	}

	status := "clean"
	if report.CriticalCount() > 0 || !report.Clean() {
		status = "discrepancies_found"
	}
	report.Status = status
	report.FinishedAt = s.clock.Now()

	// The execution cursor only advances when nothing execution-derived is
	// left unresolved.
	//
	// # Why the cursor must freeze
	//
	// closeVanishedIssues closes an open issue this run did not re-detect, on
	// the premise that a run examines both views completely. For executions
	// that premise holds only inside the cursor window, and it failed in
	// exactly the way that matters: the cursor moved past an unattributable
	// venue execution, the next run stopped fetching it, did not re-detect it,
	// and closed the issue with "the divergence is gone". The execution was
	// still sitting unbooked at the venue, and the account released its own
	// halt.
	//
	// Freezing rather than widening the window is deliberate. Widening back to
	// the issue's detection time looks equivalent and is not: an execution
	// detected later than the overlap would fall outside the widened window
	// too, and the issue would close itself again. A frozen cursor is still
	// the window that saw the execution in the first place, so it cannot
	// stop seeing it.
	//
	// The cost is a fetch that grows while something is unresolved, which is
	// bounded by an operator resolving it, and re-seeing a booked execution is
	// already free.
	cursor := remote.FetchedAt
	blocking, err := s.store.Reconcile.OldestUnresolvedExecutionIssue(ctx, account.ID)
	if err != nil {
		return report, fmt.Errorf("reconcile: oldest unresolved execution issue: %w", err)
	}
	cursorUpdate := &cursor
	if blocking != nil {
		cursorUpdate = nil
	}

	if err := s.store.Research.FinishReconciliationRun(ctx, runID, status,
		report.OrdersCompared, report.PositionsCompared, len(report.Issues), ""); err != nil {
		return report, err
	}
	if err := s.store.Reconcile.RecordRunFinished(ctx, account.ID, status, true,
		cursorUpdate); err != nil {
		return report, err
	}

	metrics.ReconciliationRuns.WithLabelValues(string(trigger), status).Inc()
	open, _ := s.store.Reconcile.OpenIssues(ctx, account.ID)
	metrics.UnresolvedDiscrepancies.WithLabelValues(account.ID.String()).Set(float64(len(open)))

	s.announce(ctx, account, report, open)
	return report, nil
}

// failRun records a failed run and alerts on repeated failure.
//
// A failed run leaves the account's last_success_at untouched, so readiness
// reports RECONCILIATION_REQUIRED rather than falling back to "no open issues,
// therefore healthy". A venue we cannot reach is not agreement.
func (s *Service) failRun(ctx context.Context, account domain.Account, runID uuid.UUID, cause error) {
	log := logging.FromContext(ctx)
	_ = s.store.Research.FinishReconciliationRun(ctx, runID, "failed", 0, 0, 0, cause.Error())
	_ = s.store.Reconcile.RecordRunFinished(ctx, account.ID, "failed", false, nil)
	metrics.ReconciliationRuns.WithLabelValues("any", "failed").Inc()

	state, err := s.store.Reconcile.AccountStateFor(ctx, account.ID)
	consecutive := 1
	if err == nil {
		consecutive = state.ConsecutiveFailures
	}
	log.Error("reconciliation failed",
		"account_id", account.ID.String(), "consecutive_failures", consecutive,
		"error", cause.Error())
	if s.alerter != nil {
		s.alerter.ReconciliationFailed(ctx, account.ID, consecutive, cause.Error())
	}
}

// persistAndRepair records a finding as an issue and applies its repair where
// the domain policy says the repair is provable.
//
// The issue is written BEFORE the repair, in its own transaction, so that:
//
//   - the repair can reference the issue id, which the schema requires of any
//     transition claiming to be a repair
//   - a repair that fails leaves the issue open rather than leaving no record
//     that anything was wrong
func (s *Service) persistAndRepair(ctx context.Context, account domain.Account,
	runID uuid.UUID, correlationID string, f Finding,
	local LocalSnapshot, remote BrokerSnapshot) (store.Issue, bool, error) {

	policy := domain.PolicyFor(f.Type)

	initialStatus := domain.IssueOpen
	if policy.Repair != domain.RepairAutomaticallySafe {
		initialStatus = domain.IssueOperatorActionRequired
	}

	in := store.Issue{
		RunID:          runID,
		AccountID:      account.ID,
		BrokerName:     account.BrokerName,
		Type:           f.Type,
		Severity:       f.Severity,
		Status:         initialStatus,
		RepairClass:    policy.Repair,
		OrderID:        f.OrderID,
		PositionID:     f.PositionID,
		Description:    f.Description,
		Fingerprint:    f.Fingerprint,
		CorrelationID:  correlationID,
		LocalSnapshot:  evidenceJSON(f.LocalEvidence),
		BrokerSnapshot: evidenceJSON(f.BrokerEvidence),
		Evidence:       evidenceJSON(f.Evidence),
	}
	if f.InstrumentID != "" {
		v := f.InstrumentID
		in.InstrumentID = &v
	}
	if f.BrokerOrderID != "" {
		v := f.BrokerOrderID
		in.BrokerOrderID = &v
	}
	if f.BrokerExecutionID != "" {
		v := f.BrokerExecutionID
		in.BrokerExecutionID = &v
	}

	var issue store.Issue
	var isNew bool
	err := s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		var err error
		issue, isNew, err = s.store.Reconcile.UpsertIssue(ctx, tx, in)
		if err != nil {
			return err
		}
		if !isNew {
			return nil
		}
		return s.store.Reconcile.AppendIssueEventTx(ctx, tx, store.IssueEvent{
			IssueID:       issue.ID,
			Action:        "DETECTED",
			ToStatus:      string(initialStatus),
			ActorType:     "system",
			Reason:        f.Description,
			Evidence:      evidenceJSON(f.Evidence),
			CorrelationID: correlationID,
		})
	})
	if err != nil {
		return store.Issue{}, false, fmt.Errorf("persist issue: %w", err)
	}

	if isNew {
		metrics.ReconciliationIssues.WithLabelValues(string(f.Type), string(f.Severity)).Inc()
		if s.alerter != nil {
			s.alerter.ReconciliationIssueRaised(ctx, account.ID,
				string(f.Type), string(f.Severity), f.Description)
		}
	}

	// Flag the order while the issue is open, so uncertainty is visible in the
	// UI and countable in readiness without inferring it from a status name.
	if issue.OrderID != nil && issue.Open() && policy.Halt != domain.HaltNone {
		if err := s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
			return s.store.Trading.SetOrderReconciliationRequiredTx(ctx, tx, *issue.OrderID, true)
		}); err != nil {
			return issue, false, fmt.Errorf("flag order for reconciliation: %w", err)
		}
	}

	if policy.Repair != domain.RepairAutomaticallySafe || f.Repair == nil {
		return issue, false, nil
	}

	repaired, err := s.applyRepair(ctx, account, issue, f, local, remote, correlationID, nil)
	if err != nil {
		return issue, false, err
	}
	return repaired, true, nil
}

// applyRepair executes a repair plan and closes the issue.
//
// actorUserID is nil for an automatic repair and set for an operator action.
// Everything else about the two paths is identical, deliberately: an operator
// importing a fill and reconciliation importing a fill must produce the same
// accounting, and the way to guarantee that is for them to be the same code.
func (s *Service) applyRepair(ctx context.Context, account domain.Account, issue store.Issue,
	f Finding, local LocalSnapshot, remote BrokerSnapshot, correlationID string,
	actorUserID *uuid.UUID) (store.Issue, error) {

	plan := f.Repair
	if plan == nil {
		return issue, errors.New("reconcile: no repair plan")
	}

	actorType := "system"
	actorLabel := "automatic"
	if actorUserID != nil {
		actorType = "user"
		actorLabel = "operator"
	}

	resolvedStatus := domain.IssueAutomaticallyRepaired
	if actorUserID != nil {
		resolvedStatus = domain.IssueResolved
	}

	detail := plan.Reason
	err := s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		// The account row lock, first, as everywhere that writes an account's
		// financial state. Booking touches fills, positions and the ledger,
		// and taking those locks in a different order from the OMS is what
		// produced a deadlock that silently created phantom fills.
		if err := s.store.Accounts.LockAccountTx(ctx, tx, account.ID); err != nil {
			return err
		}

		current, err := s.store.Reconcile.IssueByIDTx(ctx, tx, issue.ID)
		if err != nil {
			return err
		}
		if !current.Open() {
			// Someone resolved it first. Not an error: re-running
			// reconciliation against the same snapshot must be a no-op.
			return store.ErrAlreadyResolved
		}

		switch plan.Action {
		case domain.ActionImportBrokerFill:
			d, err := s.importFill(ctx, tx, account, current, plan)
			if err != nil {
				return err
			}
			detail = d

		case domain.ActionMarkNotExecuted, domain.ActionMarkBrokerRejected, domain.ActionRecheck:
			if plan.TargetStatus == "" {
				break
			}
			if current.OrderID == nil {
				return errors.New("a status repair needs an order")
			}
			updated, err := s.store.Trading.RepairTransitionTx(ctx, tx, *current.OrderID,
				plan.TargetStatus, plan.RejectCode, plan.Reason, current.ID,
				actorType, actorUserID, map[string]any{
					"broker_snapshot_at": remote.FetchedAt,
					"evidence":           string(current.BrokerSnapshot),
				})
			if err != nil {
				return err
			}
			detail = fmt.Sprintf("order %s set to %s", updated.ID, updated.Status)

		case domain.ActionAcknowledge:
			// Nothing to write. Used for the info-level stream observations,
			// where the correct action is to record that the defence fired.

		default:
			return fmt.Errorf("reconcile: %s is not an automatic repair", plan.Action)
		}

		// An order whose issue is resolved is no longer uncertain.
		if current.OrderID != nil {
			if err := s.store.Trading.SetOrderReconciliationRequiredTx(
				ctx, tx, *current.OrderID, false); err != nil {
				return err
			}
		}

		if err := s.store.Reconcile.ResolveIssueTx(ctx, tx, current.ID, resolvedStatus,
			plan.Action, detail, actorUserID); err != nil {
			return err
		}
		return s.store.Reconcile.AppendIssueEventTx(ctx, tx, store.IssueEvent{
			IssueID:       current.ID,
			Action:        string(plan.Action),
			FromStatus:    strPtr(string(current.Status)),
			ToStatus:      string(resolvedStatus),
			ActorType:     actorType,
			ActorUserID:   actorUserID,
			Reason:        detail,
			Evidence:      current.BrokerSnapshot,
			CorrelationID: correlationID,
		})
	})
	if errors.Is(err, store.ErrAlreadyResolved) {
		return issue, nil
	}
	if err != nil {
		return issue, fmt.Errorf("apply repair %s: %w", plan.Action, err)
	}

	metrics.ReconciliationRepairs.WithLabelValues(string(f.Type), actorLabel).Inc()
	logging.FromContext(ctx).Info("reconciliation repaired a divergence",
		"account_id", account.ID.String(), "issue_id", issue.ID.String(),
		"issue_type", string(f.Type), "action", string(plan.Action),
		"actor", actorLabel, "detail", detail)
	if s.alerter != nil {
		s.alerter.ReconciliationRepaired(ctx, account.ID,
			string(f.Type), string(plan.Action), detail)
	}

	// Re-read so the caller sees the resolved row rather than the pre-repair
	// copy, which would report the issue as still open.
	refreshed, err := s.store.Reconcile.IssueForAccount(ctx, account.ID, issue.ID)
	if err != nil {
		return issue, nil
	}
	return refreshed, nil
}

// importFill books an execution the venue reports and Vantage lacks.
//
// Goes through internal/booking, which is the single accounting path shared
// with the OMS. That is not a convenience: it is what makes the accounting
// invariants provable. An importer with its own INSERT could create a position
// with no ledger movement behind it, and nothing in the schema would object.
func (s *Service) importFill(ctx context.Context, tx pgx.Tx, account domain.Account,
	issue store.Issue, plan *RepairPlan) (string, error) {

	if plan.Execution == nil {
		return "", errors.New("no execution to import")
	}
	if issue.OrderID == nil {
		return "", errors.New("cannot import an execution with no attributed order")
	}

	order, err := s.store.Trading.OrderByIDTx(ctx, tx, *issue.OrderID)
	if err != nil {
		return "", fmt.Errorf("load order: %w", err)
	}
	instrument, err := s.store.Market.Instrument(ctx, order.InstrumentID)
	if err != nil {
		return "", fmt.Errorf("load instrument: %w", err)
	}

	issueID := issue.ID.String()
	result, err := s.booking.Apply(ctx, tx, booking.Request{
		Account:    account,
		Instrument: instrument,
		Order:      order,
		Execution:  *plan.Execution,
		Source:     booking.SourceReconciliationImport,
		IssueID:    &issueID,
		Now:        s.clock.Now(),
	})
	if err != nil {
		return "", err
	}
	if result.Duplicate {
		return fmt.Sprintf("execution %s was already booked; nothing was applied twice",
			plan.Execution.BrokerFillID), nil
	}

	// The order's state follows its fills. Recompute from the aggregate rather
	// than trusting the venue's status label, so a FILLED order always has
	// fills behind it summing to its quantity.
	refreshed, err := s.store.Trading.OrderByIDTx(ctx, tx, order.ID)
	if err != nil {
		return "", err
	}
	target := domain.OrderPartiallyFilled
	if refreshed.FilledQuantity.GreaterThanOrEqual(refreshed.Quantity) {
		target = domain.OrderFilled
	}
	if refreshed.Status != target {
		if _, err := s.store.Trading.RepairTransitionTx(ctx, tx, order.ID, target, "",
			fmt.Sprintf("reconciliation imported execution %s", plan.Execution.BrokerFillID),
			issue.ID, "system", nil, map[string]any{
				"broker_execution_id": plan.Execution.BrokerFillID,
				"filled_quantity":     refreshed.FilledQuantity.String(),
			}); err != nil {
			return "", fmt.Errorf("advance order state after import: %w", err)
		}
	}

	return fmt.Sprintf("imported execution %s (%s %s @ %s) onto order %s; filled %s of %s",
		plan.Execution.BrokerFillID, plan.Execution.Side, plan.Execution.Quantity,
		plan.Execution.Price, order.ID, refreshed.FilledQuantity, refreshed.Quantity), nil
}

// closeVanishedIssues resolves open issues that this run did not re-detect.
//
// # Why "did not re-detect" is the right test, and what it depends on
//
// A run captures both snapshots first and classifies them completely, so the
// set of fingerprints it produced IS the set of divergences that exist. An
// open issue whose fingerprint is absent from that set describes a problem
// that is no longer there.
//
// That reasoning has a precondition, and it was once violated: the run must
// actually have LOOKED at the evidence. The venue's execution list is bounded
// by the stored cursor, so when the cursor advanced past an unattributable
// execution, later runs stopped fetching it -- and "not re-detected" silently
// changed meaning from "fixed" to "no longer examined". Three unbooked
// executions were closed as resolved that way and the account released its own
// halt.
//
// The cursor now freezes while any execution-derived issue is unresolved (see
// runLocked), which restores the precondition. This function deliberately does
// not try to compensate for a narrow window itself: a closing rule that
// second-guesses its own inputs is harder to reason about than one whose
// inputs are correct.
//
// # Why only issues detected before this run started
//
// An issue raised by THIS run is obviously in the detected set, so the filter
// is belt and braces -- but it also guards the case where a repair inside this
// run created a new issue after classification finished. Closing that would
// discard a live finding.
//
// # What this does NOT do
//
// It does not touch financial state. A vanished divergence needs no repair by
// definition: the two views agree now. What it writes is the record that they
// agree, which is what releases the halt.
func (s *Service) closeVanishedIssues(ctx context.Context, account domain.Account,
	runID uuid.UUID, correlationID string, detected map[string]bool,
	runStartedAt time.Time) error {

	open, err := s.store.Reconcile.OpenIssues(ctx, account.ID)
	if err != nil {
		return err
	}

	for _, issue := range open {
		if detected[issue.Fingerprint] {
			continue
		}
		if issue.DetectedAt.After(runStartedAt) {
			continue
		}

		reason := fmt.Sprintf(
			"The divergence is gone: reconciliation run %s re-examined the same evidence "+
				"and did not find it. Seen %d time(s) before it cleared.",
			runID, issue.CheckCount)

		err := s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
			current, err := s.store.Reconcile.IssueByIDTx(ctx, tx, issue.ID)
			if err != nil {
				return err
			}
			if !current.Open() {
				return nil
			}
			if err := s.store.Reconcile.ResolveIssueTx(ctx, tx, current.ID,
				domain.IssueResolved, domain.ActionRecheck, reason, nil); err != nil {
				return err
			}
			return s.store.Reconcile.AppendIssueEventTx(ctx, tx, store.IssueEvent{
				IssueID:       current.ID,
				Action:        string(domain.ActionRecheck),
				FromStatus:    strPtr(string(current.Status)),
				ToStatus:      string(domain.IssueResolved),
				ActorType:     "system",
				Reason:        reason,
				CorrelationID: correlationID,
			})
		})
		if errors.Is(err, store.ErrAlreadyResolved) {
			continue
		}
		if err != nil {
			return err
		}
		logging.FromContext(ctx).Info("reconciliation closed a divergence that is gone",
			"account_id", account.ID.String(), "issue_id", issue.ID.String(),
			"issue_type", string(issue.Type))
	}
	return nil
}

// refreshOrderFlags clears the unknown-outcome flag on orders no longer
// referenced by an unresolved issue.
func (s *Service) refreshOrderFlags(ctx context.Context, account domain.Account) error {
	flagged, err := s.store.Trading.OrdersRequiringReconciliation(ctx, account.ID)
	if err != nil {
		return err
	}
	if len(flagged) == 0 {
		return nil
	}
	open, err := s.store.Reconcile.OpenIssues(ctx, account.ID)
	if err != nil {
		return err
	}
	referenced := map[uuid.UUID]bool{}
	for _, i := range open {
		if i.OrderID != nil {
			referenced[*i.OrderID] = true
		}
	}
	for _, o := range flagged {
		if referenced[o.ID] {
			continue
		}
		// A FAILED order with no open issue is still uncertain: nothing has
		// established what the venue did. Only a resolved order clears.
		if o.Status == domain.OrderFailed {
			continue
		}
		if err := s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
			return s.store.Trading.SetOrderReconciliationRequiredTx(ctx, tx, o.ID, false)
		}); err != nil {
			return err
		}
	}
	return nil
}

// announce emits the run-level alerts.
func (s *Service) announce(ctx context.Context, account domain.Account,
	report Report, open []store.Issue) {

	log := logging.FromContext(ctx)
	if s.alerter == nil {
		return
	}

	critical := 0
	kinds := make([]string, 0, len(open))
	seen := map[string]bool{}
	for _, i := range open {
		if i.Severity == domain.SeverityCriticalIssue {
			critical++
		}
		if !seen[string(i.Type)] {
			seen[string(i.Type)] = true
			kinds = append(kinds, string(i.Type))
		}
	}

	if len(open) > 0 {
		log.Warn("reconciliation left unresolved issues",
			"account_id", account.ID.String(), "open", len(open),
			"critical", critical, "repaired", report.Repaired, "trigger", report.Trigger)
		s.alerter.ReconciliationMismatch(ctx, account.ID, critical, len(open), kinds)
		return
	}

	log.Info("reconciliation clean",
		"account_id", account.ID.String(), "orders", report.OrdersCompared,
		"positions", report.PositionsCompared, "executions", report.ExecutionsSeen,
		"repaired", report.Repaired, "trigger", report.Trigger)
	// Only fires if a mismatch was previously raised, so a permanently healthy
	// account stays silent.
	s.alerter.ReconciliationClean(ctx, account.ID)
}

// RunAll reconciles every account.
//
// Sequential, deliberately. Each run holds a pooled connection for its
// advisory lock, so fanning out across accounts would consume the pool in
// proportion to account count and starve the request path. Reconciliation is
// not latency-critical.
func (s *Service) RunAll(ctx context.Context, trigger Trigger) ([]Report, error) {
	accounts, err := s.store.Accounts.ListAllAccounts(ctx)
	if err != nil {
		return nil, err
	}
	var reports []Report
	for _, a := range accounts {
		if ctx.Err() != nil {
			// Shutdown or timeout. Stop cleanly rather than reporting failures
			// for every remaining account.
			return reports, ctx.Err()
		}
		report, err := s.Run(ctx, a, trigger)
		if err != nil {
			// One account's failure must not stop the others being checked.
			logging.FromContext(ctx).Error("reconciliation failed",
				"account_id", a.ID.String(), "error", err.Error())
			continue
		}
		reports = append(reports, report)
	}
	return reports, nil
}

func strPtr(s string) *string { return &s }
