package domain

import "fmt"

// Reconciliation taxonomy and repair policy.
//
// # Why the classification lives here and not in the reconcile package
//
// Whether a divergence may be repaired without a human is the single most
// consequential rule in this system. It decides when software is allowed to
// write to the ledger on the strength of a snapshot it fetched from a venue.
//
// It is therefore a pure function of the issue type, in a package with no I/O,
// unit-tested exhaustively, and impossible to reach through a code path that
// has a database handle in scope. A rule that can be nudged by "well, we
// already have the transaction open" is not a rule.
//
// # The governing principle
//
// Repair automatically only where the correct answer is PROVABLE from the
// evidence. Not likely, not usually -- provable. Everything else waits for an
// operator, and an operator who is shown the evidence rather than a summary of
// it.
//
// The asymmetry is deliberate. An unresolved issue costs a halted account,
// which is recoverable. A wrong automatic repair writes a number into an
// append-only ledger, which is not.

// IssueType classifies a divergence between Vantage and a venue.
//
// The types are deliberately narrow. An earlier version recorded
// "fill_quantity_mismatch" for both "the venue has an execution we can prove
// belongs to order X" and "the venue has an execution we cannot attribute to
// anything" -- one of which is safely repairable and one of which must never be
// touched automatically. Collapsing them meant neither could be handled.
type IssueType string

const (
	// IssueOrderMissingLocally: the venue is working or has worked an order
	// Vantage has no record of. In a future multi-venue world this is also
	// what a trade placed in the broker's own terminal looks like.
	IssueOrderMissingLocally IssueType = "ORDER_MISSING_LOCALLY"

	// IssueOrderMissingAtBroker: Vantage believes an order is live, and a
	// direct lookup by client id says the venue has never heard of it.
	IssueOrderMissingAtBroker IssueType = "ORDER_MISSING_AT_BROKER"

	// IssueFillMissingLocally: the venue reports an execution, attributable to
	// a known Vantage order, that Vantage has not booked. This is the state a
	// lost response or a rolled-back persistence leaves behind, and it is the
	// central repairable case.
	IssueFillMissingLocally IssueType = "FILL_MISSING_LOCALLY"

	// IssueExtraBrokerFill: the venue reports an execution that cannot be
	// attributed to any single Vantage order. Never repaired automatically.
	IssueExtraBrokerFill IssueType = "EXTRA_BROKER_FILL"

	// IssuePartialFillMismatch: filled quantities disagree and the difference
	// is not explained by an attributable missing execution.
	IssuePartialFillMismatch IssueType = "PARTIAL_FILL_MISMATCH"

	// IssueOrderStatusMismatch: both sides know the order and disagree about
	// its state.
	IssueOrderStatusMismatch IssueType = "ORDER_STATUS_MISMATCH"

	// IssuePositionMismatch: the position books disagree in size or direction,
	// or one side holds a position the other does not.
	IssuePositionMismatch IssueType = "POSITION_MISMATCH"

	// IssueBalanceMismatch: the venue's balance and the ledger-derived balance
	// differ by more than the configured tolerance.
	IssueBalanceMismatch IssueType = "BALANCE_MISMATCH"

	// IssueVenueIDMismatch: Vantage holds a broker order id the venue denies,
	// or the venue reports a different id for the same client order id. This
	// is more alarming than a missing order: it means an identifier mapping is
	// wrong, so any repair based on that mapping would be wrong too.
	IssueVenueIDMismatch IssueType = "VENUE_ID_MISMATCH"

	// IssueUnknownExecutionState: Vantage cannot determine whether an order
	// executed. The order is flagged, automation halts, and nothing is
	// guessed.
	IssueUnknownExecutionState IssueType = "UNKNOWN_EXECUTION_STATE"

	// IssueDuplicateExecutionReport: the venue re-reported an execution id
	// Vantage has already booked, WITH DIFFERENT CONTENT.
	//
	// A plain replay is not this. Re-reporting an execution unchanged is the
	// expected steady state -- the execution cursor overlaps deliberately, so
	// every poll re-sees recent executions -- and raising an issue for it
	// produced dozens an hour that said nothing. A replay that CONTRADICTS
	// what was booked is different in kind: one of the two records is wrong
	// about a trade that happened.
	IssueDuplicateExecutionReport IssueType = "DUPLICATE_EXECUTION_REPORT"

	// IssueOutOfOrderExecutionReport: an execution arrived with a timestamp
	// earlier than one already processed. Booking is commutative -- the fill
	// aggregate is recomputed from the fills themselves -- so this is recorded
	// rather than treated as an error.
	IssueOutOfOrderExecutionReport IssueType = "OUT_OF_ORDER_EXECUTION_REPORT"

	// IssueExternalBrokerActivity: state at the venue that Vantage did not
	// cause. Vantage must never adopt this silently as though it had.
	IssueExternalBrokerActivity IssueType = "EXTERNAL_BROKER_ACTIVITY"
)

// IssueSeverity grades how badly a divergence compromises trading.
type IssueSeverity string

const (
	SeverityInfoIssue     IssueSeverity = "info"
	SeverityWarningIssue  IssueSeverity = "warning"
	SeverityCriticalIssue IssueSeverity = "critical"
)

// IssueStatus is where an issue sits in its lifecycle.
type IssueStatus string

const (
	// IssueOpen is a freshly detected issue that has not yet been classified
	// into a terminal outcome by this run.
	IssueOpen IssueStatus = "OPEN"
	// IssueAutomaticallyRepaired means reconciliation corrected Vantage's
	// records itself, on provable evidence.
	IssueAutomaticallyRepaired IssueStatus = "AUTOMATICALLY_REPAIRED"
	// IssueOperatorActionRequired means a human must decide.
	IssueOperatorActionRequired IssueStatus = "OPERATOR_ACTION_REQUIRED"
	// IssueResolved means an operator closed it.
	IssueResolved IssueStatus = "RESOLVED"
	// IssueUnresolvable means no action available to this system can fix it --
	// typically because the truth is outside Vantage entirely.
	IssueUnresolvable IssueStatus = "UNRESOLVABLE"
)

// Open reports whether the issue still demands attention.
func (s IssueStatus) Open() bool {
	return s == IssueOpen || s == IssueOperatorActionRequired
}

// RepairClass says who is permitted to fix an issue of this type.
type RepairClass string

const (
	// RepairAutomaticallySafe: the correct repair is provable from the
	// evidence and reconciliation may apply it without a human.
	RepairAutomaticallySafe RepairClass = "AUTOMATICALLY_SAFE"
	// RepairOperatorReviewRequired: a repair exists but choosing it needs
	// judgement no rule can supply.
	RepairOperatorReviewRequired RepairClass = "OPERATOR_REVIEW_REQUIRED"
	// RepairUnresolvableAutomatically: nothing this system can do resolves it;
	// the truth lives outside Vantage.
	RepairUnresolvableAutomatically RepairClass = "UNRESOLVABLE_AUTOMATICALLY"
)

// HaltScope is the blast radius an unresolved issue imposes on automation.
//
// Minimum necessary, always. An uncertain gold execution on one account is a
// reason to stop that account trading automatically; it is not a reason to
// stop an unrelated account, and in the multi-user architecture it must not
// become one.
type HaltScope string

const (
	// HaltNone: the issue is informational and does not stop anything.
	HaltNone HaltScope = "NONE"
	// HaltAccount: automated trading stops for this account only. Manual
	// trading and all read access continue.
	HaltAccount HaltScope = "ACCOUNT"
	// HaltBrokerConnection: every account on this broker connection stops.
	// Reserved for evidence that the connection itself is misreporting -- an
	// identifier mapping that does not hold, or a venue contradicting itself --
	// because in that state no account's data on that connection is
	// trustworthy.
	HaltBrokerConnection HaltScope = "BROKER_CONNECTION"
	// HaltAll: all automated trading stops. Deliberately unused by the
	// taxonomy below; it exists so the operator endpoint and the readiness
	// model have a term for a global stop, which today is expressed by a
	// global kill switch.
	HaltAll HaltScope = "ALL"
)

// ResolutionAction is an operation that can close an issue.
//
// Only actions that can be made safe are represented. There is deliberately no
// "set order status to X": an endpoint that accepts an arbitrary target state
// is a way to write any number into the ledger with an audit trail that says
// an operator asked for it, which is not a control.
type ResolutionAction string

const (
	// ActionAcknowledge records that a human has seen the issue and judged it
	// benign. It changes no financial state.
	ActionAcknowledge ResolutionAction = "ACKNOWLEDGE"
	// ActionRecheck re-runs detection for this issue's subject. It may close
	// the issue if the divergence is gone.
	ActionRecheck ResolutionAction = "RECHECK"
	// ActionImportBrokerFill books an execution the venue reports and Vantage
	// lacks, through the ordinary accounting path.
	ActionImportBrokerFill ResolutionAction = "IMPORT_BROKER_FILL"
	// ActionMarkBrokerRejected records that the venue definitively refused the
	// order.
	ActionMarkBrokerRejected ResolutionAction = "MARK_BROKER_REJECTED"
	// ActionMarkNotExecuted records that the order never reached the market.
	ActionMarkNotExecuted ResolutionAction = "MARK_NOT_EXECUTED"
	// ActionLinkBrokerOrder attaches a venue order id to a Vantage order whose
	// acknowledgement was lost.
	ActionLinkBrokerOrder ResolutionAction = "LINK_BROKER_ORDER"
	// ActionResolveManually closes the issue on the operator's authority,
	// recording that the repair happened outside Vantage. It changes no
	// financial state here, precisely so it cannot be used as a back door.
	ActionResolveManually ResolutionAction = "RESOLVE_MANUALLY"
)

// Valid reports whether the action is recognised.
func (a ResolutionAction) Valid() bool {
	switch a {
	case ActionAcknowledge, ActionRecheck, ActionImportBrokerFill,
		ActionMarkBrokerRejected, ActionMarkNotExecuted, ActionLinkBrokerOrder,
		ActionResolveManually:
		return true
	}
	return false
}

// MutatesFinancialState reports whether the action writes to orders,
// positions or the ledger.
//
// Used to decide what an action must justify and how loudly it is announced.
// ACKNOWLEDGE and RESOLVE_MANUALLY deliberately do not: an operator closing a
// ticket is not the same act as an operator booking a trade.
func (a ResolutionAction) MutatesFinancialState() bool {
	switch a {
	case ActionImportBrokerFill, ActionMarkBrokerRejected, ActionMarkNotExecuted,
		ActionLinkBrokerOrder:
		return true
	}
	return false
}

// IssuePolicy is the complete rule for one issue type.
type IssuePolicy struct {
	Type IssueType
	// DefaultSeverity applies unless the detector has specific evidence to
	// raise it. A detector may escalate; it may never de-escalate, because
	// that would let the code that found the problem also decide it does not
	// matter.
	DefaultSeverity IssueSeverity
	Repair          RepairClass
	Halt            HaltScope
	// AllowedActions are the operator actions this issue type accepts. An
	// action outside this set is refused, so an operator cannot import a fill
	// against a balance mismatch.
	AllowedActions []ResolutionAction
	// Rationale explains the classification. It is surfaced to the operator
	// alongside the issue, because a person deciding what to do needs to know
	// why the system would not decide for them.
	Rationale string
}

// issuePolicies is the authoritative repair policy.
//
// Every issue type appears exactly once, and a test asserts that -- an issue
// type with no policy would otherwise fall through to a default, and a default
// here is a decision nobody made.
var issuePolicies = map[IssueType]IssuePolicy{
	IssueFillMissingLocally: {
		Type:            IssueFillMissingLocally,
		DefaultSeverity: SeverityWarningIssue,
		Repair:          RepairAutomaticallySafe,
		Halt:            HaltAccount,
		AllowedActions:  []ResolutionAction{ActionImportBrokerFill, ActionRecheck, ActionAcknowledge, ActionResolveManually},
		Rationale: "The venue reports an execution carrying a broker order id that maps to " +
			"exactly one Vantage order, and the execution's instrument, side and account " +
			"agree with that order. The correct repair is to book it, and booking is " +
			"idempotent on the venue's execution id, so applying it twice is impossible.",
	},
	IssueDuplicateExecutionReport: {
		Type:            IssueDuplicateExecutionReport,
		DefaultSeverity: SeverityCriticalIssue,
		Repair:          RepairOperatorReviewRequired,
		Halt:            HaltAccount,
		AllowedActions:  []ResolutionAction{ActionRecheck, ActionAcknowledge, ActionResolveManually},
		Rationale: "This is raised only when the venue re-reports an execution id with a " +
			"DIFFERENT quantity, price or side from the one Vantage booked, not for an " +
			"ordinary replay, which is normal and silent. The unique index on " +
			"(broker_name, broker_fill_id) refused the second copy, so nothing was written " +
			"twice; but one of the two records is now wrong about a trade that happened, " +
			"and deduplication cannot say which. There is nothing safe to write: booking " +
			"the new version would double the position, and overwriting the old one would " +
			"edit an append-only fill.",
	},
	IssueOutOfOrderExecutionReport: {
		Type:            IssueOutOfOrderExecutionReport,
		DefaultSeverity: SeverityInfoIssue,
		Repair:          RepairAutomaticallySafe,
		Halt:            HaltNone,
		AllowedActions:  []ResolutionAction{ActionAcknowledge},
		Rationale: "Booking is commutative: filled quantity and average price are recomputed " +
			"from the fills themselves rather than incremented, so arrival order cannot " +
			"change the result. Recorded because a venue that reorders executions is worth " +
			"knowing about even when it is harmless. Expected to be rare, unlike a plain " +
			"replay, which is the normal steady state and is not recorded at all.",
	},
	IssueOrderMissingAtBroker: {
		Type:            IssueOrderMissingAtBroker,
		DefaultSeverity: SeverityWarningIssue,
		Repair:          RepairAutomaticallySafe,
		Halt:            HaltAccount,
		AllowedActions:  []ResolutionAction{ActionMarkNotExecuted, ActionRecheck, ActionAcknowledge, ActionResolveManually},
		Rationale: "Automatically safe ONLY on the strength of a direct lookup by client id " +
			"returning not-found, and only for an order holding no venue identifier. That " +
			"combination proves the order never reached the market. An order that DOES hold " +
			"a venue id the venue denies is a VENUE_ID_MISMATCH instead, and is not " +
			"repaired automatically.",
	},
	IssueOrderStatusMismatch: {
		Type:            IssueOrderStatusMismatch,
		DefaultSeverity: SeverityWarningIssue,
		Repair:          RepairAutomaticallySafe,
		Halt:            HaltAccount,
		AllowedActions:  []ResolutionAction{ActionRecheck, ActionAcknowledge, ActionResolveManually},
		Rationale: "The venue is authoritative about its own order states. Adopting a venue " +
			"status is safe where the repair state machine permits the move AND the filled " +
			"quantities already agree. A status difference accompanied by a quantity " +
			"difference is a missing execution, which is a different issue and must be " +
			"repaired by booking the execution rather than by relabelling the order.",
	},
	IssueOrderMissingLocally: {
		Type:            IssueOrderMissingLocally,
		DefaultSeverity: SeverityCriticalIssue,
		Repair:          RepairOperatorReviewRequired,
		Halt:            HaltAccount,
		AllowedActions:  []ResolutionAction{ActionLinkBrokerOrder, ActionAcknowledge, ActionResolveManually},
		Rationale: "Vantage cannot invent the decision record for an order it did not place. " +
			"There is no risk decision, no authority check and no intent to attach it to, " +
			"and manufacturing them would put a fabricated audit trail in the ledger. An " +
			"operator may LINK it to a Vantage order whose acknowledgement was lost, which " +
			"is a judgement about identity that only a person can make.",
	},
	IssueExtraBrokerFill: {
		Type:            IssueExtraBrokerFill,
		DefaultSeverity: SeverityCriticalIssue,
		Repair:          RepairOperatorReviewRequired,
		Halt:            HaltAccount,
		AllowedActions:  []ResolutionAction{ActionImportBrokerFill, ActionAcknowledge, ActionResolveManually},
		Rationale: "An execution that cannot be attributed to exactly one Vantage order is " +
			"the case where automatic repair is most tempting and most dangerous: guessing " +
			"the parent order writes a real position and a real P&L against the wrong " +
			"order. An operator may import it once they have established which order it " +
			"belongs to.",
	},
	IssuePartialFillMismatch: {
		Type:            IssuePartialFillMismatch,
		DefaultSeverity: SeverityCriticalIssue,
		Repair:          RepairOperatorReviewRequired,
		Halt:            HaltAccount,
		AllowedActions:  []ResolutionAction{ActionRecheck, ActionAcknowledge, ActionResolveManually},
		Rationale: "Quantities disagree and no individual execution explains the difference. " +
			"The venue is telling us a total we cannot decompose, so there is nothing " +
			"specific to book. Re-checking often resolves it, because the usual cause is a " +
			"snapshot taken mid-execution.",
	},
	IssuePositionMismatch: {
		Type:            IssuePositionMismatch,
		DefaultSeverity: SeverityCriticalIssue,
		Repair:          RepairOperatorReviewRequired,
		Halt:            HaltAccount,
		AllowedActions:  []ResolutionAction{ActionRecheck, ActionAcknowledge, ActionResolveManually},
		Rationale: "A position is the consequence of executions, never an input. Writing a " +
			"position to match the venue would leave a position no sequence of fills " +
			"explains, breaking the invariant that position quantity equals fill-derived " +
			"quantity. The repair is to find and book the missing executions, which is a " +
			"FILL_MISSING_LOCALLY issue if they can be attributed.",
	},
	IssueBalanceMismatch: {
		Type:            IssueBalanceMismatch,
		DefaultSeverity: SeverityWarningIssue,
		Repair:          RepairOperatorReviewRequired,
		Halt:            HaltNone,
		AllowedActions:  []ResolutionAction{ActionRecheck, ActionAcknowledge, ActionResolveManually},
		Rationale: "The ledger is append-only and gapless by construction, so there is no " +
			"legitimate way to 'correct' a balance: a balance is the running total of its " +
			"transactions. A difference means either an unbooked execution or a venue-side " +
			"adjustment, and both are found by investigation rather than by writing. Does " +
			"not halt automation on its own: in paper mode the venue simulates its own book " +
			"and a small difference is expected by design.",
	},
	IssueVenueIDMismatch: {
		Type:            IssueVenueIDMismatch,
		DefaultSeverity: SeverityCriticalIssue,
		Repair:          RepairUnresolvableAutomatically,
		Halt:            HaltBrokerConnection,
		AllowedActions:  []ResolutionAction{ActionLinkBrokerOrder, ActionRecheck, ActionAcknowledge, ActionResolveManually},
		Rationale: "Vantage holds a venue identifier the venue does not recognise, or the " +
			"venue reports a different one for the same client order id. Every other repair " +
			"in this system relies on that mapping being sound, so while it is in doubt no " +
			"repair on this connection can be trusted, which is why the halt scope is the " +
			"connection rather than the account.",
	},
	IssueUnknownExecutionState: {
		Type:            IssueUnknownExecutionState,
		DefaultSeverity: SeverityCriticalIssue,
		Repair:          RepairOperatorReviewRequired,
		Halt:            HaltAccount,
		AllowedActions: []ResolutionAction{ActionRecheck, ActionImportBrokerFill,
			ActionMarkBrokerRejected, ActionMarkNotExecuted, ActionAcknowledge, ActionResolveManually},
		Rationale: "Vantage does not know whether this order executed. This state must be " +
			"held open rather than collapsed into a tidy one: recording it as rejected " +
			"would free the risk budget for a position that may exist, and recording it as " +
			"filled would invent one that may not. RECHECK is the first action, because a " +
			"venue that was unreachable is often reachable a minute later.",
	},
	IssueExternalBrokerActivity: {
		Type:            IssueExternalBrokerActivity,
		DefaultSeverity: SeverityCriticalIssue,
		Repair:          RepairUnresolvableAutomatically,
		Halt:            HaltAccount,
		AllowedActions:  []ResolutionAction{ActionAcknowledge, ActionResolveManually},
		Rationale: "State at the venue that Vantage did not cause: a position opened, an " +
			"order cancelled or a stop moved in the broker's own terminal. Vantage must not " +
			"adopt it as though it had originated it, because every downstream record " +
			"(decision snapshot, risk check, authority) would then be a fabrication. It is " +
			"reported and acknowledged, and the position book is treated as the venue " +
			"reports it for risk purposes without claiming Vantage placed the trade.",
	},
}

// PolicyFor returns the repair policy for an issue type.
//
// An unknown type is fail-closed: critical, unresolvable, and halting. A new
// issue type that someone forgot to classify must not default to "safe to
// repair automatically".
func PolicyFor(t IssueType) IssuePolicy {
	if p, ok := issuePolicies[t]; ok {
		return p
	}
	return IssuePolicy{
		Type:            t,
		DefaultSeverity: SeverityCriticalIssue,
		Repair:          RepairUnresolvableAutomatically,
		Halt:            HaltAccount,
		AllowedActions:  []ResolutionAction{ActionAcknowledge, ActionResolveManually},
		Rationale: "Unclassified issue type. Treated as critical and unresolvable because an " +
			"unrecognised divergence is the one least safe to act on automatically.",
	}
}

// AllIssueTypes lists every classified type, for tests and for the API's
// self-description.
func AllIssueTypes() []IssueType {
	return []IssueType{
		IssueOrderMissingLocally,
		IssueOrderMissingAtBroker,
		IssueFillMissingLocally,
		IssueExtraBrokerFill,
		IssuePartialFillMismatch,
		IssueOrderStatusMismatch,
		IssuePositionMismatch,
		IssueBalanceMismatch,
		IssueVenueIDMismatch,
		IssueUnknownExecutionState,
		IssueDuplicateExecutionReport,
		IssueOutOfOrderExecutionReport,
		IssueExternalBrokerActivity,
	}
}

// Valid reports whether the issue type is classified.
func (t IssueType) Valid() bool {
	_, ok := issuePolicies[t]
	return ok
}

// ActionAllowed reports whether an operator action is permitted for a type.
func ActionAllowed(t IssueType, a ResolutionAction) bool {
	for _, allowed := range PolicyFor(t).AllowedActions {
		if allowed == a {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Repair transitions
// ---------------------------------------------------------------------------

// repairTransitions is a SECOND state machine, used only by reconciliation.
//
// # Why not simply widen the normal table
//
// The obvious shortcut is to add ACCEPTED -> FILLED to legalTransitions and be
// done. That would be a mistake. ACCEPTED means "persisted, not yet sent"; a
// fill arriving in that state during normal execution is a bug in the OMS, and
// the state machine refusing it is how that bug gets caught. Widening the
// table to accommodate recovery would remove the check that makes normal
// execution safe, in order to describe an exceptional path.
//
// So recovery gets its own table. A transition here is reachable only through
// the reconciliation repair path, which requires an issue id, evidence and a
// classification of AUTOMATICALLY_SAFE or an explicit operator action -- and
// every such transition is stamped is_repair in order_state_transitions, so
// the history distinguishes "the venue told us at the time" from "we
// reconstructed this afterwards".
//
// What is still forbidden: leaving a terminal state. A FILLED order does not
// become CANCELLED because a snapshot disagreed; that is a contradiction to
// investigate, not a state to overwrite.
var repairTransitions = map[OrderStatus]map[OrderStatus]bool{
	// An order persisted but whose submission outcome was never recorded. The
	// venue may have taken it and filled it.
	OrderAccepted: {
		OrderSubmitted:       true,
		OrderPartiallyFilled: true,
		OrderFilled:          true,
		OrderCancelled:       true,
		OrderRejected:        true,
		OrderExpired:         true,
		OrderFailed:          true,
	},
	OrderCreated: {
		OrderRejected: true,
		OrderFailed:   true,
	},
	OrderValidating: {
		OrderRejected: true,
		OrderFailed:   true,
	},
	OrderSubmitted: {
		OrderPartiallyFilled: true,
		OrderFilled:          true,
		OrderCancelled:       true,
		OrderRejected:        true,
		OrderExpired:         true,
		OrderFailed:          true,
	},
	OrderPartiallyFilled: {
		OrderPartiallyFilled: true,
		OrderFilled:          true,
		OrderCancelled:       true,
		OrderExpired:         true,
		OrderFailed:          true,
	},
	OrderCancelPending: {
		OrderPartiallyFilled: true,
		OrderFilled:          true,
		OrderCancelled:       true,
		OrderFailed:          true,
	},
	// FAILED is the unknown-outcome state, so every resolution is reachable
	// from it. That is the whole reason FAILED is not terminal.
	OrderFailed: {
		OrderPartiallyFilled: true,
		OrderFilled:          true,
		OrderCancelled:       true,
		OrderRejected:        true,
		OrderExpired:         true,
	},
	// Terminal states stay terminal, even under repair.
	OrderFilled:    {},
	OrderRejected:  {},
	OrderCancelled: {},
	OrderExpired:   {},
}

// CanRepairTransition reports whether reconciliation may move from -> to.
//
// Every normal transition is also a legal repair: if the OMS may do it while
// the venue is answering, reconciliation may do it while reconstructing what
// the venue said. The converse does not hold.
func CanRepairTransition(from, to OrderStatus) bool {
	if CanTransition(from, to) {
		return true
	}
	allowed, ok := repairTransitions[from]
	if !ok {
		return false
	}
	return allowed[to]
}

// ErrIllegalRepair is returned when reconciliation is asked for a transition
// even the repair table forbids.
type ErrIllegalRepair struct {
	From OrderStatus
	To   OrderStatus
}

func (e ErrIllegalRepair) Error() string {
	return fmt.Sprintf(
		"illegal reconciliation repair %s -> %s: a terminal order state is never overwritten",
		e.From, e.To)
}

// ---------------------------------------------------------------------------
// Trading readiness
// ---------------------------------------------------------------------------

// TradingState is the answer to "may automation run right now".
//
// Reported separately from process health. An HTTP server and a database that
// are both alive say nothing about whether this system's picture of the market
// is trustworthy, and reporting "healthy" on that basis is how an operator
// comes to believe a halted account is trading.
type TradingState string

const (
	// TradingHealthy: reconciled recently, no unresolved issues.
	TradingHealthy TradingState = "HEALTHY"
	// TradingDegraded: something is wrong but not in a way that compromises
	// the position book -- a stale feed, a warning-level divergence.
	TradingDegraded TradingState = "DEGRADED"
	// TradingReconciliationRequired: the account has never been reconciled, or
	// its last run failed, so agreement with the venue is unproven. Distinct
	// from HALTED: nothing is known to be wrong, but nothing is known to be
	// right either, and that is not a state to trade automatically from.
	TradingReconciliationRequired TradingState = "RECONCILIATION_REQUIRED"
	// TradingHalted: an unresolved critical divergence. Automation stops.
	TradingHalted TradingState = "TRADING_HALTED"
)

// AutomationAllowed reports whether automated trading may proceed.
//
// Manual trading is deliberately not gated here. An operator can see the
// warning and decide; an algorithm cannot, and must not act on a position book
// known or merely presumed to be wrong.
func (s TradingState) AutomationAllowed() bool {
	return s == TradingHealthy || s == TradingDegraded
}

// Severity orders the states so the worst of several can be chosen.
func (s TradingState) Severity() int {
	switch s {
	case TradingHealthy:
		return 0
	case TradingDegraded:
		return 1
	case TradingReconciliationRequired:
		return 2
	case TradingHalted:
		return 3
	}
	return 3
}

// WorstTradingState returns the most severe of the given states, defaulting to
// RECONCILIATION_REQUIRED for an empty set -- an account nobody reported on is
// not thereby healthy.
func WorstTradingState(states ...TradingState) TradingState {
	if len(states) == 0 {
		return TradingReconciliationRequired
	}
	worst := states[0]
	for _, s := range states[1:] {
		if s.Severity() > worst.Severity() {
			worst = s
		}
	}
	return worst
}
