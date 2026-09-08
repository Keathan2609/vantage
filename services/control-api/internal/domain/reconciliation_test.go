package domain

import "testing"

// The repair policy decides when software is allowed to write to an
// append-only ledger on the strength of a snapshot. These tests exist to make
// that decision hard to change by accident.

func TestEveryIssueTypeHasAnExplicitPolicy(t *testing.T) {
	// A type with no policy falls through to the fail-closed default, which is
	// safe but silent. Silence is the problem: someone added a divergence
	// classification and never decided how it should be handled, and nothing
	// told them.
	for _, tp := range AllIssueTypes() {
		if !tp.Valid() {
			t.Errorf("issue type %s has no policy entry", tp)
		}
	}
	if len(issuePolicies) != len(AllIssueTypes()) {
		t.Fatalf("issuePolicies has %d entries but AllIssueTypes lists %d; "+
			"one of them was updated without the other",
			len(issuePolicies), len(AllIssueTypes()))
	}
}

func TestEveryPolicyKeyMatchesItsType(t *testing.T) {
	// A copy-paste error here would silently apply one type's repair class to
	// another, which is the single worst mistake available in this file.
	for key, policy := range issuePolicies {
		if key != policy.Type {
			t.Errorf("policy under key %s declares Type %s", key, policy.Type)
		}
	}
}

func TestAnUnknownIssueTypeFailsClosed(t *testing.T) {
	p := PolicyFor(IssueType("SOMETHING_NOBODY_CLASSIFIED"))
	if p.Repair != RepairUnresolvableAutomatically {
		t.Errorf("an unclassified issue type is %s; it must never be automatically repairable",
			p.Repair)
	}
	if p.DefaultSeverity != SeverityCriticalIssue {
		t.Errorf("an unclassified issue type is %s severity; expected critical", p.DefaultSeverity)
	}
	if p.Halt == HaltNone {
		t.Error("an unclassified issue type does not halt anything; it must")
	}
}

// TestOnlyProvableIssuesAreAutomaticallyRepairable is the central assertion of
// this file.
//
// The automatically-safe set is small and every member is there for a stated
// reason. If a future change adds a type to it, this test fails and forces the
// author to justify the addition here rather than in a commit message.
func TestOnlyProvableIssuesAreAutomaticallyRepairable(t *testing.T) {
	expected := map[IssueType]bool{
		IssueFillMissingLocally: true,
		// OUT_OF_ORDER is harmless by construction: booking is commutative,
		// because the fill aggregate is recomputed from the fills rather than
		// incremented.
		IssueOutOfOrderExecutionReport: true,
		IssueOrderMissingAtBroker:      true,
		IssueOrderStatusMismatch:       true,
		// DUPLICATE_EXECUTION_REPORT is deliberately NOT here.
		//
		// It used to be, when the type meant "the venue replayed an execution
		// we already have" -- which is the normal steady state and is now not
		// recorded at all. The type now means "the venue re-reported the same
		// execution id with DIFFERENT content", and there is nothing safe to
		// write for that: booking the new version doubles the position, and
		// overwriting the old one edits an append-only fill.
	}

	for _, tp := range AllIssueTypes() {
		auto := PolicyFor(tp).Repair == RepairAutomaticallySafe
		if auto && !expected[tp] {
			t.Errorf("%s became AUTOMATICALLY_SAFE. Adding a type to this set means "+
				"software may now write to the ledger for it without a human. Justify it "+
				"in the policy's Rationale and add it to this test deliberately.", tp)
		}
		if !auto && expected[tp] {
			t.Errorf("%s is no longer AUTOMATICALLY_SAFE; recovery that used to be "+
				"automatic now needs an operator", tp)
		}
	}
}

func TestAmbiguousExecutionsAreNeverAutomaticallyRepaired(t *testing.T) {
	// The milestone's separate acceptance criterion: an ambiguous broker-side
	// execution must not be guessed into the ledger.
	for _, tp := range []IssueType{
		IssueExtraBrokerFill,
		IssueOrderMissingLocally,
		IssuePositionMismatch,
		IssueUnknownExecutionState,
		IssueExternalBrokerActivity,
		IssueVenueIDMismatch,
		IssueDuplicateExecutionReport,
	} {
		if PolicyFor(tp).Repair == RepairAutomaticallySafe {
			t.Errorf("%s is automatically repairable; an unattributable or externally "+
				"caused divergence must require operator review", tp)
		}
	}
}

func TestPositionAndBalanceAreNeverWrittenToMatchTheVenue(t *testing.T) {
	// Positions and balances are derived: a position is the consequence of
	// fills, a balance is the running total of transactions. An action that
	// wrote either directly would produce a value nothing explains.
	for _, tp := range []IssueType{IssuePositionMismatch, IssueBalanceMismatch} {
		for _, a := range PolicyFor(tp).AllowedActions {
			if a.MutatesFinancialState() {
				t.Errorf("%s permits %s, which writes financial state. Neither a position "+
					"nor a balance may be set to match a snapshot; the repair is to book "+
					"the executions that explain the difference", tp, a)
			}
		}
	}
}

func TestBalanceMismatchDoesNotHaltAutomationOnItsOwn(t *testing.T) {
	// In paper mode the venue simulates its own book, so a small difference is
	// expected by design. Halting on it would mean automation never runs.
	if PolicyFor(IssueBalanceMismatch).Halt != HaltNone {
		t.Error("a balance mismatch halts automation; in paper mode the two balances are " +
			"computed differently by design and this would halt permanently")
	}
}

func TestIdentifierMismatchHaltsTheWholeConnection(t *testing.T) {
	// Every other repair relies on the id mapping. While that is in doubt, no
	// account on the connection can be repaired safely.
	if got := PolicyFor(IssueVenueIDMismatch).Halt; got != HaltBrokerConnection {
		t.Errorf("VENUE_ID_MISMATCH halt scope is %s; expected BROKER_CONNECTION, because "+
			"a broken identifier mapping invalidates every repair on that connection", got)
	}
}

func TestNoIssueTypeHaltsAllTradingGlobally(t *testing.T) {
	// Minimum necessary blast radius. One uncertain execution on one account
	// must not stop unrelated accounts, and the taxonomy must not quietly
	// acquire the power to do so.
	for _, tp := range AllIssueTypes() {
		if PolicyFor(tp).Halt == HaltAll {
			t.Errorf("%s halts ALL trading. A single account's divergence must not stop "+
				"unrelated accounts; use a global kill switch for that deliberately", tp)
		}
	}
}

func TestEveryCriticalIssueHaltsSomething(t *testing.T) {
	for _, tp := range AllIssueTypes() {
		p := PolicyFor(tp)
		if p.DefaultSeverity == SeverityCriticalIssue && p.Halt == HaltNone {
			t.Errorf("%s is critical but halts nothing; a critical divergence that does "+
				"not stop automation is a label, not a control", tp)
		}
	}
}

func TestEveryPolicyExplainsItself(t *testing.T) {
	// The rationale is shown to the operator deciding what to do. An empty one
	// leaves them guessing at why the system would not decide for them.
	for _, tp := range AllIssueTypes() {
		p := PolicyFor(tp)
		if len(p.Rationale) < 80 {
			t.Errorf("%s has a rationale of %d characters; explain the classification",
				tp, len(p.Rationale))
		}
		if len(p.AllowedActions) == 0 {
			t.Errorf("%s permits no operator action at all, so it can never be closed", tp)
		}
	}
}

func TestEveryPolicyAllowsAnEscapeHatch(t *testing.T) {
	// Every issue must be closable, or an account halts forever on something
	// that was resolved outside Vantage. ACKNOWLEDGE changes no financial
	// state, so it is always available.
	for _, tp := range AllIssueTypes() {
		if !ActionAllowed(tp, ActionAcknowledge) {
			t.Errorf("%s cannot be acknowledged, so an issue resolved outside Vantage "+
				"would halt the account permanently", tp)
		}
	}
}

func TestActionsOutsideAnIssuePolicyAreRefused(t *testing.T) {
	// An operator must not be able to import a fill against a balance
	// mismatch, or mark a position mismatch as broker-rejected.
	if ActionAllowed(IssueBalanceMismatch, ActionImportBrokerFill) {
		t.Error("a balance mismatch accepts IMPORT_BROKER_FILL")
	}
	if ActionAllowed(IssuePositionMismatch, ActionMarkBrokerRejected) {
		t.Error("a position mismatch accepts MARK_BROKER_REJECTED")
	}
	if ActionAllowed(IssueDuplicateExecutionReport, ActionImportBrokerFill) {
		t.Error("a duplicate execution report accepts IMPORT_BROKER_FILL, which would " +
			"book the very execution that was correctly refused as a duplicate")
	}
}

func TestOnlyRecognisedActionsAreValid(t *testing.T) {
	if ResolutionAction("SET_ORDER_STATUS").Valid() {
		t.Fatal("SET_ORDER_STATUS is a valid action. There is deliberately no " +
			"arbitrary status-setting operation: it would be a way to write any state " +
			"into the ledger with an audit trail that merely says someone asked")
	}
	for _, a := range []ResolutionAction{
		ActionAcknowledge, ActionRecheck, ActionImportBrokerFill,
		ActionMarkBrokerRejected, ActionMarkNotExecuted, ActionLinkBrokerOrder,
		ActionResolveManually,
	} {
		if !a.Valid() {
			t.Errorf("%s is not valid", a)
		}
	}
}

func TestAcknowledgingDoesNotMutateFinancialState(t *testing.T) {
	for _, a := range []ResolutionAction{ActionAcknowledge, ActionRecheck, ActionResolveManually} {
		if a.MutatesFinancialState() {
			t.Errorf("%s claims to mutate financial state; closing a ticket is not the "+
				"same act as booking a trade and must not be conflated with it", a)
		}
	}
	for _, a := range []ResolutionAction{
		ActionImportBrokerFill, ActionMarkBrokerRejected,
		ActionMarkNotExecuted, ActionLinkBrokerOrder,
	} {
		if !a.MutatesFinancialState() {
			t.Errorf("%s writes to orders, positions or the ledger but does not declare it, "+
				"so it would not be announced or justified as a financial action", a)
		}
	}
}

// ---------------------------------------------------------------------------
// Repair transitions
// ---------------------------------------------------------------------------

func TestRepairAllowsRecoveryFromAcceptedWithoutWideningNormalExecution(t *testing.T) {
	// The case the previous audit could not resolve: a venue filled an order
	// while Vantage's persistence rolled back, leaving it ACCEPTED.
	if CanTransition(OrderAccepted, OrderFilled) {
		t.Fatal("ACCEPTED -> FILLED became a NORMAL transition. ACCEPTED means " +
			"'persisted, not yet sent'; a fill in that state during ordinary execution " +
			"is an OMS bug, and the state machine refusing it is how that bug is caught")
	}
	if !CanRepairTransition(OrderAccepted, OrderFilled) {
		t.Error("reconciliation cannot repair ACCEPTED -> FILLED, which is exactly the " +
			"state a lost response leaves behind")
	}
	if !CanRepairTransition(OrderAccepted, OrderPartiallyFilled) {
		t.Error("reconciliation cannot repair ACCEPTED -> PARTIALLY_FILLED")
	}
}

func TestRepairNeverLeavesATerminalState(t *testing.T) {
	// A FILLED order does not become CANCELLED because a snapshot disagreed.
	// That is a contradiction to investigate, not a state to overwrite.
	terminal := []OrderStatus{OrderFilled, OrderRejected, OrderCancelled, OrderExpired}
	every := []OrderStatus{
		OrderCreated, OrderValidating, OrderAccepted, OrderSubmitted,
		OrderPartiallyFilled, OrderFilled, OrderRejected, OrderCancelPending,
		OrderCancelled, OrderExpired, OrderFailed,
	}
	for _, from := range terminal {
		for _, to := range every {
			if CanRepairTransition(from, to) {
				t.Errorf("repair permits %s -> %s; a terminal state must never be "+
					"overwritten by reconciliation", from, to)
			}
		}
	}
}

func TestEveryNormalTransitionIsAlsoALegalRepair(t *testing.T) {
	// If the OMS may do it while the venue is answering, reconciliation may do
	// it while reconstructing what the venue said.
	every := []OrderStatus{
		OrderCreated, OrderValidating, OrderAccepted, OrderSubmitted,
		OrderPartiallyFilled, OrderFilled, OrderRejected, OrderCancelPending,
		OrderCancelled, OrderExpired, OrderFailed,
	}
	for _, from := range every {
		for _, to := range every {
			if CanTransition(from, to) && !CanRepairTransition(from, to) {
				t.Errorf("%s -> %s is a normal transition but not a legal repair", from, to)
			}
		}
	}
}

func TestFailedResolvesToEveryDefiniteOutcome(t *testing.T) {
	// FAILED is the unknown-outcome state. Every resolution must be reachable
	// from it, or an uncertain order could never be settled.
	for _, to := range []OrderStatus{
		OrderFilled, OrderPartiallyFilled, OrderCancelled, OrderRejected, OrderExpired,
	} {
		if !CanRepairTransition(OrderFailed, to) {
			t.Errorf("FAILED cannot be repaired to %s, so an order with an unknown "+
				"outcome could never be settled as %s", to, to)
		}
	}
}

func TestRepairCannotInventAnUnknownState(t *testing.T) {
	if CanRepairTransition(OrderStatus("MADE_UP"), OrderFilled) {
		t.Error("repair accepts an unrecognised source state")
	}
	if CanRepairTransition(OrderAccepted, OrderStatus("MADE_UP")) {
		t.Error("repair accepts an unrecognised destination state")
	}
}

// ---------------------------------------------------------------------------
// Trading state
// ---------------------------------------------------------------------------

func TestAutomationIsBlockedByHaltAndByUnproven(t *testing.T) {
	if TradingHalted.AutomationAllowed() {
		t.Error("automation is allowed while trading is halted")
	}
	if TradingReconciliationRequired.AutomationAllowed() {
		t.Error("automation is allowed while agreement with the venue is unproven. " +
			"An account that has never been reconciled is not thereby safe")
	}
	if !TradingHealthy.AutomationAllowed() {
		t.Error("automation is blocked when healthy")
	}
	if !TradingDegraded.AutomationAllowed() {
		t.Error("automation is blocked when merely degraded; a stale feed is already " +
			"refused by the risk engine and does not need a second, broader stop")
	}
}

func TestAnEmptySetOfStatesIsNotHealthy(t *testing.T) {
	// The failure mode this guards: a bug returns no per-account states and
	// the aggregate reports HEALTHY, so an operator believes everything is
	// fine because nothing reported a problem.
	if got := WorstTradingState(); got != TradingReconciliationRequired {
		t.Errorf("no reported states aggregates to %s; an account nobody reported on is "+
			"not thereby healthy", got)
	}
}

func TestWorstTradingStateWins(t *testing.T) {
	got := WorstTradingState(TradingHealthy, TradingHalted, TradingDegraded)
	if got != TradingHalted {
		t.Errorf("aggregate of healthy+halted+degraded is %s; expected TRADING_HALTED", got)
	}
	got = WorstTradingState(TradingHealthy, TradingDegraded)
	if got != TradingDegraded {
		t.Errorf("aggregate of healthy+degraded is %s; expected DEGRADED", got)
	}
	if got := WorstTradingState(TradingHealthy); got != TradingHealthy {
		t.Errorf("aggregate of a single healthy state is %s", got)
	}
}

func TestAnUnrecognisedTradingStateIsTreatedAsTheWorst(t *testing.T) {
	s := TradingState("SOMETHING_NEW")
	if s.AutomationAllowed() {
		t.Error("an unrecognised trading state permits automation")
	}
	if s.Severity() != TradingHalted.Severity() {
		t.Error("an unrecognised trading state does not sort as the most severe, so it " +
			"could be masked by a halted account in an aggregate")
	}
}

func TestOpenStatusesAreExactlyTheUnresolvedOnes(t *testing.T) {
	if !IssueOpen.Open() || !IssueOperatorActionRequired.Open() {
		t.Error("OPEN or OPERATOR_ACTION_REQUIRED does not report as open")
	}
	for _, s := range []IssueStatus{IssueAutomaticallyRepaired, IssueResolved, IssueUnresolvable} {
		if s.Open() {
			t.Errorf("%s reports as open", s)
		}
	}
}
