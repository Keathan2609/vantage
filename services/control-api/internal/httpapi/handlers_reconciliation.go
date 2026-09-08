package httpapi

// Reconciliation and operations endpoints.
//
// # Two very different privilege levels in one file
//
// The read endpoints are available to any authenticated owner of the account:
// seeing that your own account has halted is not a privileged act, and hiding
// it would leave an operator wondering why nothing trades.
//
// The RESOLVE endpoint is admin-only and is the most dangerous route in this
// API. It can book an execution into an append-only ledger. Everything about
// its shape is chosen to bound that:
//
//   - the caller chooses WHICH remedy, never WHAT the numbers are; the
//     execution comes from the issue's own stored evidence
//   - the action must be one the issue type permits
//   - a reason is mandatory and length-checked
//   - the issue is loaded scoped to the account, so a forged id from another
//     account is not-found rather than data
//   - there is no "set order status" operation at all
//
// See internal/reconcile/resolve.go for the reasoning behind each.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/reconcile"
	"github.com/vantage/control-api/internal/store"
)

// issueView is the API shape of a reconciliation issue.
//
// The evidence is passed through as raw JSON rather than re-encoded, so what
// the operator sees is what was recorded. Re-encoding would risk a summary
// diverging from the evidence it summarises, which is the one thing this
// record exists to prevent.
type issueView struct {
	ID          string `json:"id"`
	RunID       string `json:"run_id"`
	AccountID   string `json:"account_id"`
	BrokerName  string `json:"broker_name"`
	Type        string `json:"issue_type"`
	Severity    string `json:"severity"`
	Status      string `json:"status"`
	RepairClass string `json:"repair_class"`
	// AutomaticRepairAllowed restates the repair class as the single boolean
	// the UI needs, so the terminal does not re-implement the policy.
	AutomaticRepairAllowed bool     `json:"automatic_repair_allowed"`
	HaltScope              string   `json:"halt_scope"`
	Rationale              string   `json:"rationale"`
	AllowedActions         []string `json:"allowed_actions"`

	OrderID           *string `json:"order_id"`
	PositionID        *string `json:"position_id"`
	InstrumentID      *string `json:"instrument_id"`
	BrokerOrderID     *string `json:"broker_order_id"`
	BrokerExecutionID *string `json:"broker_execution_id"`

	LocalState  json.RawMessage `json:"local_state"`
	BrokerState json.RawMessage `json:"broker_state"`
	Evidence    json.RawMessage `json:"evidence"`
	Description string          `json:"description"`

	DetectedAt    string `json:"detected_at"`
	LastCheckedAt string `json:"last_checked_at"`
	CheckCount    int    `json:"check_count"`

	ResolvedAt       *string `json:"resolved_at"`
	ResolutionAction *string `json:"resolution_action"`
	ResolutionReason *string `json:"resolution_reason"`
}

func toIssueView(i store.Issue) issueView {
	policy := domain.PolicyFor(i.Type)
	actions := make([]string, 0, len(policy.AllowedActions))
	for _, a := range policy.AllowedActions {
		actions = append(actions, string(a))
	}
	v := issueView{
		ID:                     i.ID.String(),
		RunID:                  i.RunID.String(),
		AccountID:              i.AccountID.String(),
		BrokerName:             i.BrokerName,
		Type:                   string(i.Type),
		Severity:               string(i.Severity),
		Status:                 string(i.Status),
		RepairClass:            string(i.RepairClass),
		AutomaticRepairAllowed: policy.Repair == domain.RepairAutomaticallySafe,
		HaltScope:              string(policy.Halt),
		Rationale:              policy.Rationale,
		AllowedActions:         actions,
		InstrumentID:           i.InstrumentID,
		BrokerOrderID:          i.BrokerOrderID,
		BrokerExecutionID:      i.BrokerExecutionID,
		LocalState:             i.LocalSnapshot,
		BrokerState:            i.BrokerSnapshot,
		Evidence:               i.Evidence,
		Description:            i.Description,
		DetectedAt:             i.DetectedAt.UTC().Format("2006-01-02T15:04:05.000000Z"),
		LastCheckedAt:          i.LastCheckedAt.UTC().Format("2006-01-02T15:04:05.000000Z"),
		CheckCount:             i.CheckCount,
		ResolutionReason:       i.ResolutionReason,
	}
	if i.OrderID != nil {
		s := i.OrderID.String()
		v.OrderID = &s
	}
	if i.PositionID != nil {
		s := i.PositionID.String()
		v.PositionID = &s
	}
	if i.ResolvedAt != nil {
		s := i.ResolvedAt.UTC().Format("2006-01-02T15:04:05.000000Z")
		v.ResolvedAt = &s
	}
	if i.ResolutionAction != nil {
		s := string(*i.ResolutionAction)
		v.ResolutionAction = &s
	}
	return v
}

func issueViews(in []store.Issue) []issueView {
	out := make([]issueView, 0, len(in))
	for _, i := range in {
		out = append(out, toIssueView(i))
	}
	return out
}

// handleReconciliationIssues lists an account's issues.
//
// Resolved issues are returned unless the caller asks for open only. A
// divergence that keeps recurring is itself a finding, and hiding history
// would make that invisible.
func (s *Server) handleReconciliationIssues(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForOperations(w, r, "accountID")
	if !ok {
		return
	}

	filter := store.IssueFilter{AccountID: &account.ID, Limit: 200}
	if r.URL.Query().Get("open") == "true" {
		filter.OpenOnly = true
	}
	if sev := r.URL.Query().Get("severity"); sev != "" {
		for _, v := range strings.Split(sev, ",") {
			switch domain.IssueSeverity(strings.TrimSpace(v)) {
			case domain.SeverityInfoIssue, domain.SeverityWarningIssue, domain.SeverityCriticalIssue:
				filter.Severity = append(filter.Severity, domain.IssueSeverity(strings.TrimSpace(v)))
			default:
				writeError(w, r, http.StatusUnprocessableEntity, "invalid_severity",
					"severity must be one of info, warning, critical.")
				return
			}
		}
	}

	issues, err := s.store.Reconcile.ListIssues(r.Context(), filter)
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return
	}

	verdict, err := s.reconciler.Verdict(r.Context(), account)
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"account_id":               account.ID.String(),
		"trading_state":            string(verdict.State),
		"automation_allowed":       verdict.AutomationAllowed(),
		"halt_scope":               string(verdict.HaltScope),
		"reason":                   verdict.Reason,
		"open_issues":              verdict.OpenIssues,
		"critical_issues":          verdict.CriticalIssues,
		"operator_action_required": verdict.OperatorAction,
		"uncertain_orders":         verdict.UncertainOrders,
		"consecutive_failures":     verdict.ConsecutiveFailures,
		"last_success_at":          verdict.LastSuccessAt,
		"issues":                   issueViews(issues),
		"simulated":                true,
	})
}

// handleReconciliationIssue returns one issue with its full history.
func (s *Server) handleReconciliationIssue(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForOperations(w, r, "accountID")
	if !ok {
		return
	}
	issueID, ok := parseUUID(w, r, chi.URLParam(r, "issueID"), "Issue id")
	if !ok {
		return
	}

	// Scoped read: an issue id belonging to another account is not-found, not
	// forbidden. Distinguishing the two would confirm the id exists, which is
	// an enumeration oracle.
	issue, err := s.store.Reconcile.IssueForAccount(r.Context(), account.ID, issueID)
	if err != nil {
		writeStoreError(w, r, err, "No such reconciliation issue for this account.")
		return
	}
	history, err := s.store.Reconcile.IssueHistory(r.Context(), issue.ID)
	if err != nil {
		writeStoreError(w, r, err, "No such reconciliation issue for this account.")
		return
	}

	events := make([]map[string]any, 0, len(history))
	for _, e := range history {
		events = append(events, map[string]any{
			"action":      e.Action,
			"from_status": e.FromStatus,
			"to_status":   e.ToStatus,
			"actor_type":  e.ActorType,
			"reason":      e.Reason,
			"evidence":    e.Evidence,
			"occurred_at": e.OccurredAt,
		})
	}

	view := map[string]any{"issue": toIssueView(issue), "history": events}
	if issue.OrderID != nil {
		if repairs, err := s.store.Trading.RepairTransitions(r.Context(), *issue.OrderID); err == nil {
			view["order_repairs"] = repairs
		}
	}
	writeJSON(w, r, http.StatusOK, view)
}

// handleOperationsOverview is the system-wide operations view.
//
// Answers the question an operator actually has — "is anything stopped, and
// why" — in one call, rather than requiring a walk over accounts.
func (s *Server) handleOperationsOverview(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())

	verdict, err := s.reconciler.System(r.Context())
	if err != nil {
		writeStoreError(w, r, err, "Operations state unavailable.")
		return
	}

	accounts := make([]map[string]any, 0, len(verdict.Accounts))
	for _, a := range verdict.Accounts {
		// Scoped to what this user owns. An admin sees every account; a trader
		// sees their own, because an operations view is not a reason to widen
		// who can see whose balances.
		if p.User.Role != domain.RoleAdmin {
			owned, err := s.store.Accounts.AccountForUser(r.Context(), p.User.ID, a.AccountID)
			if err != nil || owned.ID != a.AccountID {
				continue
			}
		}
		accounts = append(accounts, map[string]any{
			"account_id":               a.AccountID.String(),
			"broker_name":              a.BrokerName,
			"trading_state":            string(a.State),
			"automation_allowed":       a.AutomationAllowed(),
			"halt_scope":               string(a.HaltScope),
			"reason":                   a.Reason,
			"open_issues":              a.OpenIssues,
			"critical_issues":          a.CriticalIssues,
			"operator_action_required": a.OperatorAction,
			"uncertain_orders":         a.UncertainOrders,
			"last_success_at":          a.LastSuccessAt,
			"consecutive_failures":     a.ConsecutiveFailures,
		})
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"trading_state":   string(verdict.State),
		"halted_accounts": verdict.HaltedAccounts,
		"open_issues":     verdict.OpenIssues,
		"critical_issues": verdict.CriticalIssues,
		"accounts":        accounts,
		"execution_mode":  string(s.cfg.ExecutionMode),
		"simulated":       true,
	})
}

// resolveIssueRequest is an operator's decision.
//
// Deliberately minimal. There is no status field, no quantity, no price: every
// number involved comes from the issue's own recorded evidence. A caller who
// could supply them would be writing arbitrary values into the ledger with an
// operator's authority attached, which is the thing this endpoint must not be.
type resolveIssueRequest struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
	// BrokerOrderID is used by LINK_BROKER_ORDER only.
	BrokerOrderID string `json:"broker_order_id,omitempty"`
}

// handleResolveReconciliationIssue applies an operator action.
//
// ADMIN ONLY, enforced by the router. The role check is a route-level
// constraint rather than an `if` in this function, so it cannot be lost in a
// refactor of the handler body.
func (s *Server) handleResolveReconciliationIssue(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())

	account, ok := s.accountForOperations(w, r, "accountID")
	if !ok {
		return
	}
	issueID, ok := parseUUID(w, r, chi.URLParam(r, "issueID"), "Issue id")
	if !ok {
		return
	}

	var req resolveIssueRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	action := domain.ResolutionAction(strings.TrimSpace(req.Action))
	if !action.Valid() {
		known := make([]string, 0, 7)
		for _, a := range []domain.ResolutionAction{
			domain.ActionAcknowledge, domain.ActionRecheck, domain.ActionImportBrokerFill,
			domain.ActionMarkBrokerRejected, domain.ActionMarkNotExecuted,
			domain.ActionLinkBrokerOrder, domain.ActionResolveManually,
		} {
			known = append(known, string(a))
		}
		writeError(w, r, http.StatusUnprocessableEntity, "unknown_action",
			"action must be one of: "+strings.Join(known, ", ")+
				". There is deliberately no operation that sets an order status directly.")
		return
	}

	result, err := s.reconciler.Resolve(r.Context(), account, reconcile.ResolveRequest{
		IssueID:       issueID,
		Action:        action,
		Reason:        req.Reason,
		ActorUserID:   p.User.ID,
		BrokerOrderID: req.BrokerOrderID,
		CorrelationID: logging.CorrelationID(r.Context()),
	})
	if err != nil {
		s.writeResolveError(w, r, err)
		// A refused financial action is audited too. An audit trail that
		// records only successes cannot answer "did anyone try".
		s.auditAuth(r, &p.User.ID, domain.AuditReconciliationResolve, domain.AuditFailure,
			map[string]any{
				"account_id": account.ID.String(), "issue_id": issueID.String(),
				"action": string(action), "error": err.Error(),
			})
		return
	}

	s.auditAuth(r, &p.User.ID, domain.AuditReconciliationResolve, domain.AuditSuccess,
		map[string]any{
			"account_id":    account.ID.String(),
			"issue_id":      issueID.String(),
			"action":        string(action),
			"issue_type":    string(result.Issue.Type),
			"state_changed": result.StateChanged,
			"detail":        result.Detail,
			// The operator's own words, retained verbatim. This is the record
			// that explains a ledger entry to whoever reads it later.
			"reason": req.Reason,
		})

	writeJSON(w, r, http.StatusOK, map[string]any{
		"issue":         toIssueView(result.Issue),
		"detail":        result.Detail,
		"state_changed": result.StateChanged,
	})
}

// writeResolveError maps a resolution refusal to a status a client can branch
// on.
//
// Each case is a genuinely different situation and deserves a different code:
// "you may not do that" is a permanent client error, "already resolved" means
// someone else got there first, and "a run is in progress" means try again.
func (s *Server) writeResolveError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, reconcile.ErrIssueNotFound), errors.Is(err, store.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "issue_not_found",
			"No such reconciliation issue for this account.")
	case errors.Is(err, reconcile.ErrAlreadyResolved):
		writeError(w, r, http.StatusConflict, "already_resolved",
			"This issue was resolved by someone else. Reload to see how.")
	case errors.Is(err, reconcile.ErrActionNotPermitted):
		writeError(w, r, http.StatusUnprocessableEntity, "action_not_permitted", err.Error())
	case errors.Is(err, reconcile.ErrReasonRequired):
		writeError(w, r, http.StatusUnprocessableEntity, "reason_required", err.Error())
	case errors.Is(err, reconcile.ErrNoEvidence):
		writeError(w, r, http.StatusUnprocessableEntity, "insufficient_evidence", err.Error())
	case errors.Is(err, reconcile.ErrRunInProgress):
		writeError(w, r, http.StatusConflict, "reconciliation_in_progress",
			"A reconciliation run is already in progress for this account. Try again shortly.")
	default:
		logging.FromContext(r.Context()).Error("reconciliation resolution failed",
			"error", err.Error())
		writeError(w, r, http.StatusInternalServerError, "internal_error",
			"The reconciliation action could not be applied.")
	}
}

// handleReconciliationTaxonomy publishes the repair policy.
//
// Exposed so the terminal renders the same classification the server enforces
// rather than duplicating it, and so an operator can read WHY a class of
// divergence is not repaired automatically without reading the source.
func (s *Server) handleReconciliationTaxonomy(w http.ResponseWriter, r *http.Request) {
	types := make([]map[string]any, 0, len(domain.AllIssueTypes()))
	for _, t := range domain.AllIssueTypes() {
		p := domain.PolicyFor(t)
		actions := make([]string, 0, len(p.AllowedActions))
		for _, a := range p.AllowedActions {
			actions = append(actions, string(a))
		}
		types = append(types, map[string]any{
			"issue_type":               string(t),
			"default_severity":         string(p.DefaultSeverity),
			"repair_class":             string(p.Repair),
			"automatic_repair_allowed": p.Repair == domain.RepairAutomaticallySafe,
			"halt_scope":               string(p.Halt),
			"allowed_actions":          actions,
			"rationale":                p.Rationale,
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"issue_types": types})
}

// uuidFromString is a small helper for optional identifiers in payloads.
func uuidFromString(s string) *uuid.UUID {
	id, err := uuid.Parse(strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	return &id
}
