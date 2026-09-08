package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vantage/control-api/internal/broker"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/store"
)

// Operator resolution of reconciliation issues.
//
// # What is deliberately absent
//
// There is no "set this order's status" operation. An endpoint accepting an
// arbitrary target state would let anyone with the admin role write any number
// into an append-only ledger, with an audit trail whose only content is that
// somebody asked for it. That is not a control; it is a control-shaped hole.
//
// Instead each action is a named, bounded operation whose effect is derived
// from the issue's own evidence. IMPORT_BROKER_FILL books the execution the
// venue reported — not an execution the caller supplies. MARK_NOT_EXECUTED
// closes out an order the venue does not have. The caller chooses WHICH
// remedy, never WHAT the resulting numbers are.

// ResolveRequest is an operator's decision about one issue.
type ResolveRequest struct {
	IssueID uuid.UUID
	Action  domain.ResolutionAction
	// Reason is mandatory. An issue closed with no explanation is exactly the
	// record an operator needs, and does not have, when the same divergence
	// reappears next month.
	Reason      string
	ActorUserID uuid.UUID
	// BrokerOrderID is required by LINK_BROKER_ORDER and ignored otherwise.
	BrokerOrderID string
	CorrelationID string
}

// ResolveResult reports what an operator action did.
type ResolveResult struct {
	Issue store.Issue
	// Detail is the human-readable account of the effect, and is what gets
	// stored as the resolution reason alongside the operator's own words.
	Detail string
	// StateChanged is true when financial state was written, as opposed to the
	// issue merely being closed.
	StateChanged bool
}

// Resolution refusals. Each maps to a distinct HTTP status, so a caller can
// tell "you may not do that" from "that is no longer possible".
var (
	ErrIssueNotFound      = errors.New("reconcile: no such reconciliation issue for this account")
	ErrActionNotPermitted = errors.New("reconcile: that action is not permitted for this issue type")
	ErrReasonRequired     = errors.New("reconcile: resolving an issue requires a reason")
	ErrAlreadyResolved    = store.ErrAlreadyResolved
	ErrNoEvidence         = errors.New("reconcile: the issue no longer has the evidence this action needs")
)

// minReasonLength forces something more than a keystroke.
//
// Not arbitrary: the reason is the only durable record of WHY a financial
// repair was applied, and "ok" is not that record. Short enough not to be an
// obstacle during an incident.
const minReasonLength = 10

// Resolve applies an operator action to an issue.
//
// Authorisation is the caller's responsibility and is enforced at the HTTP
// layer, where the admin role is checked. This function assumes the actor is
// permitted and concerns itself with whether the ACTION is permitted, which is
// a different question with a different answer per issue type.
func (s *Service) Resolve(ctx context.Context, account domain.Account,
	req ResolveRequest) (ResolveResult, error) {

	if !req.Action.Valid() {
		return ResolveResult{}, fmt.Errorf("%w: %q is not a recognised action",
			ErrActionNotPermitted, req.Action)
	}
	reason := strings.TrimSpace(req.Reason)
	if len(reason) < minReasonLength {
		return ResolveResult{}, fmt.Errorf(
			"%w: at least %d characters, because this is the only durable record of why "+
				"the repair was applied", ErrReasonRequired, minReasonLength)
	}

	// Scoped read. The account is part of the query rather than checked after
	// it, so an issue id belonging to another account returns not-found rather
	// than data — and an authorisation check written as a later `if` is one
	// refactor away from being dropped.
	issue, err := s.store.Reconcile.IssueForAccount(ctx, account.ID, req.IssueID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ResolveResult{}, ErrIssueNotFound
		}
		return ResolveResult{}, err
	}
	if !issue.Open() {
		return ResolveResult{}, ErrAlreadyResolved
	}
	if !domain.ActionAllowed(issue.Type, req.Action) {
		return ResolveResult{}, fmt.Errorf("%w: %s does not accept %s (permitted: %v)",
			ErrActionNotPermitted, issue.Type, req.Action,
			domain.PolicyFor(issue.Type).AllowedActions)
	}

	switch req.Action {
	case domain.ActionRecheck:
		return s.resolveByRecheck(ctx, account, issue, reason, req)
	case domain.ActionAcknowledge, domain.ActionResolveManually:
		return s.closeWithoutWriting(ctx, account, issue, reason, req)
	case domain.ActionImportBrokerFill:
		return s.resolveByImport(ctx, account, issue, reason, req)
	case domain.ActionMarkBrokerRejected:
		return s.resolveByStatus(ctx, account, issue, domain.OrderRejected,
			domain.RejectBrokerRejected, reason, req)
	case domain.ActionMarkNotExecuted:
		return s.resolveByStatus(ctx, account, issue, domain.OrderRejected,
			domain.RejectReconciledAbsent, reason, req)
	case domain.ActionLinkBrokerOrder:
		return s.resolveByLink(ctx, account, issue, reason, req)
	}
	return ResolveResult{}, fmt.Errorf("%w: %s", ErrActionNotPermitted, req.Action)
}

// closeWithoutWriting handles ACKNOWLEDGE and RESOLVE_MANUALLY.
//
// Neither writes financial state, and that is the whole point of having them:
// an operator recording a judgement is not the same act as an operator booking
// a trade, and conflating the two would mean every "I have looked at this"
// carried the authority to move money.
func (s *Service) closeWithoutWriting(ctx context.Context, account domain.Account,
	issue store.Issue, reason string, req ResolveRequest) (ResolveResult, error) {

	status := domain.IssueResolved
	detail := reason
	if req.Action == domain.ActionAcknowledge {
		detail = "acknowledged by an operator: " + reason
	} else {
		detail = "resolved outside Vantage by an operator: " + reason
	}

	err := s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		current, err := s.store.Reconcile.IssueByIDTx(ctx, tx, issue.ID)
		if err != nil {
			return err
		}
		if !current.Open() {
			return ErrAlreadyResolved
		}
		// The order's uncertainty flag is cleared, because the operator has
		// stated the matter is settled. This is the one place a human's word
		// substitutes for evidence, which is why it is recorded as such.
		if current.OrderID != nil {
			if err := s.store.Trading.SetOrderReconciliationRequiredTx(
				ctx, tx, *current.OrderID, false); err != nil {
				return err
			}
		}
		if err := s.store.Reconcile.ResolveIssueTx(ctx, tx, current.ID, status,
			req.Action, detail, &req.ActorUserID); err != nil {
			return err
		}
		return s.store.Reconcile.AppendIssueEventTx(ctx, tx, store.IssueEvent{
			IssueID:       current.ID,
			Action:        string(req.Action),
			FromStatus:    strPtr(string(current.Status)),
			ToStatus:      string(status),
			ActorType:     "user",
			ActorUserID:   &req.ActorUserID,
			Reason:        detail,
			CorrelationID: req.CorrelationID,
		})
	})
	if err != nil {
		return ResolveResult{}, err
	}
	s.logOperatorAction(ctx, account, issue, req, detail, false)
	refreshed, _ := s.store.Reconcile.IssueForAccount(ctx, account.ID, issue.ID)
	return ResolveResult{Issue: refreshed, Detail: detail}, nil
}

// resolveByRecheck re-reads the venue and closes the issue only if the
// divergence is genuinely gone.
//
// The common and correct case for a snapshot taken mid-execution. It does not
// close the issue on the operator's say-so: it re-runs detection and lets the
// evidence decide, which is why it is safe to expose.
func (s *Service) resolveByRecheck(ctx context.Context, account domain.Account,
	issue store.Issue, reason string, req ResolveRequest) (ResolveResult, error) {

	report, err := s.Run(ctx, account, TriggerManual)
	if err != nil {
		return ResolveResult{}, fmt.Errorf("re-check: %w", err)
	}
	if report.Skipped {
		return ResolveResult{}, fmt.Errorf(
			"%w: a reconciliation run is already in progress; try again in a moment",
			ErrRunInProgress)
	}

	refreshed, err := s.store.Reconcile.IssueForAccount(ctx, account.ID, issue.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ResolveResult{}, ErrIssueNotFound
		}
		return ResolveResult{}, err
	}

	if !refreshed.Open() {
		// The re-run resolved it, either by repairing it or by finding the
		// divergence gone.
		s.logOperatorAction(ctx, account, issue, req,
			"re-check resolved the divergence", false)
		return ResolveResult{
			Issue:  refreshed,
			Detail: "the divergence is gone: a fresh venue snapshot agrees with Vantage",
		}, nil
	}

	// Still diverging. The issue is NOT closed — that is the honest outcome,
	// and closing it because someone asked for a re-check would be exactly the
	// silent guess this subsystem exists to prevent.
	detail := fmt.Sprintf(
		"re-checked against a fresh venue snapshot and the divergence persists "+
			"(seen %d times). Operator note: %s", refreshed.CheckCount, reason)
	err = s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		return s.store.Reconcile.AppendIssueEventTx(ctx, tx, store.IssueEvent{
			IssueID:       refreshed.ID,
			Action:        string(domain.ActionRecheck),
			FromStatus:    strPtr(string(refreshed.Status)),
			ToStatus:      string(refreshed.Status),
			ActorType:     "user",
			ActorUserID:   &req.ActorUserID,
			Reason:        detail,
			CorrelationID: req.CorrelationID,
		})
	})
	if err != nil {
		return ResolveResult{}, err
	}
	s.logOperatorAction(ctx, account, issue, req, detail, false)
	return ResolveResult{Issue: refreshed, Detail: detail}, nil
}

// resolveByImport books the execution the issue's evidence describes.
//
// The execution is reconstructed from the issue's stored broker snapshot, NOT
// supplied by the caller. That distinction is the whole safety property: a
// caller who could name the quantity and price would be writing arbitrary
// numbers into the ledger with an operator's authority attached.
func (s *Service) resolveByImport(ctx context.Context, account domain.Account,
	issue store.Issue, reason string, req ResolveRequest) (ResolveResult, error) {

	if issue.OrderID == nil {
		return ResolveResult{}, fmt.Errorf(
			"%w: this issue has no attributed Vantage order, so there is nothing to book "+
				"the execution against. LINK_BROKER_ORDER first", ErrNoEvidence)
	}
	exec, err := executionFromEvidence(issue)
	if err != nil {
		return ResolveResult{}, err
	}

	f := Finding{
		Type:              issue.Type,
		Severity:          issue.Severity,
		OrderID:           issue.OrderID,
		BrokerExecutionID: exec.BrokerFillID,
		Repair: &RepairPlan{
			Action:    domain.ActionImportBrokerFill,
			Execution: &exec,
			Reason:    "imported by an operator: " + reason,
		},
	}

	updated, err := s.applyRepair(ctx, account, issue, f,
		LocalSnapshot{}, BrokerSnapshot{FetchedAt: issue.LastCheckedAt},
		req.CorrelationID, &req.ActorUserID)
	if err != nil {
		return ResolveResult{}, err
	}
	detail := "operator imported the venue execution: " + reason
	s.logOperatorAction(ctx, account, issue, req, detail, true)
	return ResolveResult{Issue: updated, Detail: detail, StateChanged: true}, nil
}

// resolveByStatus applies a definite outcome an operator has established.
func (s *Service) resolveByStatus(ctx context.Context, account domain.Account,
	issue store.Issue, target domain.OrderStatus, code domain.RejectCode,
	reason string, req ResolveRequest) (ResolveResult, error) {

	if issue.OrderID == nil {
		return ResolveResult{}, fmt.Errorf(
			"%w: this issue concerns no Vantage order, so there is no order state to set",
			ErrNoEvidence)
	}

	f := Finding{
		Type:     issue.Type,
		Severity: issue.Severity,
		OrderID:  issue.OrderID,
		Repair: &RepairPlan{
			Action:       req.Action,
			TargetStatus: target,
			RejectCode:   code,
			Reason:       "established by an operator: " + reason,
		},
	}

	updated, err := s.applyRepair(ctx, account, issue, f,
		LocalSnapshot{}, BrokerSnapshot{FetchedAt: issue.LastCheckedAt},
		req.CorrelationID, &req.ActorUserID)
	if err != nil {
		return ResolveResult{}, err
	}
	detail := fmt.Sprintf("operator set order %s to %s: %s", *issue.OrderID, target, reason)
	s.logOperatorAction(ctx, account, issue, req, detail, true)
	return ResolveResult{Issue: updated, Detail: detail, StateChanged: true}, nil
}

// resolveByLink attaches a venue order id to a Vantage order.
//
// The judgement only a person can make: "this venue order and that Vantage
// order are the same order". Vantage cannot prove it — that is precisely why
// the issue exists — so the operator asserts it and the assertion is recorded
// as an assertion.
//
// The link is refused if the id already belongs to a different Vantage order.
// Two orders claiming one venue execution is the ambiguity this whole
// subsystem is built to avoid creating.
func (s *Service) resolveByLink(ctx context.Context, account domain.Account,
	issue store.Issue, reason string, req ResolveRequest) (ResolveResult, error) {

	brokerOrderID := strings.TrimSpace(req.BrokerOrderID)
	if brokerOrderID == "" && issue.BrokerOrderID != nil {
		brokerOrderID = *issue.BrokerOrderID
	}
	if brokerOrderID == "" {
		return ResolveResult{}, fmt.Errorf(
			"%w: LINK_BROKER_ORDER needs a venue order id", ErrNoEvidence)
	}
	if issue.OrderID == nil {
		return ResolveResult{}, fmt.Errorf(
			"%w: this issue names no Vantage order to link to. Vantage will not create one: "+
				"an order it did not place has no risk decision, authority check or intent, "+
				"and manufacturing them would put a fabricated record in the audit trail",
			ErrNoEvidence)
	}

	detail := fmt.Sprintf("operator linked venue order %s to Vantage order %s: %s",
		brokerOrderID, *issue.OrderID, reason)

	err := s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		if err := s.store.Accounts.LockAccountTx(ctx, tx, account.ID); err != nil {
			return err
		}
		current, err := s.store.Reconcile.IssueByIDTx(ctx, tx, issue.ID)
		if err != nil {
			return err
		}
		if !current.Open() {
			return ErrAlreadyResolved
		}

		owner, err := s.store.Trading.OrderByBrokerOrderIDTx(ctx, tx, account.ID, brokerOrderID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil && owner.ID != *current.OrderID {
			return fmt.Errorf(
				"%w: venue order %s is already linked to Vantage order %s; two orders "+
					"claiming one venue execution is the ambiguity this subsystem exists "+
					"to prevent", ErrActionNotPermitted, brokerOrderID, owner.ID)
		}

		if err := s.store.Trading.SetBrokerOrderIDTx(ctx, tx, *current.OrderID, brokerOrderID); err != nil {
			return err
		}
		if err := s.store.Reconcile.ResolveIssueTx(ctx, tx, current.ID, domain.IssueResolved,
			domain.ActionLinkBrokerOrder, detail, &req.ActorUserID); err != nil {
			return err
		}
		return s.store.Reconcile.AppendIssueEventTx(ctx, tx, store.IssueEvent{
			IssueID:     current.ID,
			Action:      string(domain.ActionLinkBrokerOrder),
			FromStatus:  strPtr(string(current.Status)),
			ToStatus:    string(domain.IssueResolved),
			ActorType:   "user",
			ActorUserID: &req.ActorUserID,
			Reason:      detail,
			Evidence: evidenceJSON(map[string]any{
				"broker_order_id":      brokerOrderID,
				"asserted_by_operator": true,
			}),
			CorrelationID: req.CorrelationID,
		})
	})
	if err != nil {
		return ResolveResult{}, err
	}
	s.logOperatorAction(ctx, account, issue, req, detail, true)
	refreshed, _ := s.store.Reconcile.IssueForAccount(ctx, account.ID, issue.ID)
	return ResolveResult{Issue: refreshed, Detail: detail, StateChanged: true}, nil
}

// executionFromEvidence rebuilds the venue execution an issue recorded.
//
// Reads the issue's own stored broker snapshot. If the evidence is not there —
// because the issue is not about a specific execution — the action is refused
// rather than the caller being asked to supply the numbers.
func executionFromEvidence(issue store.Issue) (broker.ExecutionReport, error) {
	var raw struct {
		BrokerExecutionID string `json:"broker_execution_id"`
		BrokerOrderID     string `json:"broker_order_id"`
		ClientOrderID     string `json:"client_order_id"`
		Symbol            string `json:"symbol"`
		Side              string `json:"side"`
		Quantity          string `json:"quantity"`
		Price             string `json:"price"`
		Commission        string `json:"commission"`
		CommissionCcy     string `json:"commission_currency"`
		ExecutedAt        string `json:"executed_at"`
	}
	if err := jsonUnmarshal(issue.BrokerSnapshot, &raw); err != nil {
		return broker.ExecutionReport{}, fmt.Errorf(
			"%w: the stored venue evidence could not be read: %v", ErrNoEvidence, err)
	}
	if raw.BrokerExecutionID == "" || raw.Quantity == "" || raw.Price == "" {
		return broker.ExecutionReport{}, fmt.Errorf(
			"%w: the stored evidence does not describe a specific execution, so there is "+
				"nothing to import. Re-check the issue to refresh its evidence", ErrNoEvidence)
	}

	quantity, err := decimalFromString(raw.Quantity)
	if err != nil {
		return broker.ExecutionReport{}, fmt.Errorf("%w: quantity %q: %v", ErrNoEvidence, raw.Quantity, err)
	}
	price, err := decimalFromString(raw.Price)
	if err != nil {
		return broker.ExecutionReport{}, fmt.Errorf("%w: price %q: %v", ErrNoEvidence, raw.Price, err)
	}
	commission, err := decimalFromString(raw.Commission)
	if err != nil {
		commission = zeroDecimal()
	}
	executedAt, err := timeFromString(raw.ExecutedAt)
	if err != nil {
		return broker.ExecutionReport{}, fmt.Errorf(
			"%w: execution timestamp %q: %v", ErrNoEvidence, raw.ExecutedAt, err)
	}

	ccy := raw.CommissionCcy
	if ccy == "" {
		ccy = "USD"
	}
	return broker.ExecutionReport{
		BrokerFillID:  raw.BrokerExecutionID,
		BrokerOrderID: raw.BrokerOrderID,
		ClientOrderID: raw.ClientOrderID,
		Symbol:        raw.Symbol,
		Side:          domain.OrderSide(raw.Side),
		Quantity:      quantity,
		Price:         price,
		Commission:    commission,
		CommissionCcy: ccy,
		ExecutedAt:    executedAt,
	}, nil
}

// logOperatorAction records an operator decision in the structured log.
//
// Every reconciliation action is already persisted as an issue event; this is
// the operational copy, at a level that matches whether money moved.
func (s *Service) logOperatorAction(ctx context.Context, account domain.Account,
	issue store.Issue, req ResolveRequest, detail string, stateChanged bool) {

	log := logging.FromContext(ctx).With(
		"account_id", account.ID.String(),
		"issue_id", issue.ID.String(),
		"issue_type", string(issue.Type),
		"action", string(req.Action),
		"actor_user_id", req.ActorUserID.String(),
		"state_changed", stateChanged,
		"detail", detail,
	)
	if stateChanged {
		log.Warn("operator applied a reconciliation repair that wrote financial state")
		return
	}
	log.Info("operator resolved a reconciliation issue without writing financial state")
}
