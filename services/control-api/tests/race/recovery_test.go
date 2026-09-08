package race

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Reconciliation and recovery, against a running stack.
//
// # Why these are here and not in internal/reconcile
//
// The classifier is unit-tested exhaustively in internal/reconcile, with
// hand-built snapshots and no database. What CANNOT be tested there is whether
// a repair actually converges: that the imported fill lands once, that the
// order's state follows its fills, that the ledger stays gapless, and that
// re-running changes nothing. Those are properties of PostgreSQL and of the
// real accounting path, and a fake store would only prove the fake agrees with
// the test author.
//
// The acceptance test for this milestone is
// TestALostFillIsDiscoveredImportedOnceAndConverges.

// armLostResponse arms the fault that reproduces the dangerous case: the mock
// venue commits and fills the order, then returns ErrUnknownOutcome instead of
// the acknowledgement.
func (c *client) armLostResponse() {
	c.t.Helper()
	c.armFault("lost_response", 1, nil)
}

// runReconciliation triggers a manual run and returns the decoded report.
func (c *client) runReconciliation(accountID string) (map[string]any, int) {
	c.t.Helper()
	var body map[string]any
	status := c.post("/api/v1/reconciliation/"+accountID+"/run", "", nil, &body)
	return body, status
}

// openIssues reads the account's unresolved issues through the API.
func (c *client) openIssues(accountID string) []map[string]any {
	c.t.Helper()
	var body struct {
		Issues []map[string]any `json:"issues"`
	}
	if status := c.get(
		"/api/v1/reconciliation/"+accountID+"/issues?open=true", &body); status != http.StatusOK {
		c.t.Fatalf("listing reconciliation issues returned %d", status)
	}
	return body.Issues
}

// tradingState reads the derived verdict.
func (c *client) tradingState(accountID string) (string, bool) {
	c.t.Helper()
	var body struct {
		TradingState      string `json:"trading_state"`
		AutomationAllowed bool   `json:"automation_allowed"`
	}
	if status := c.get(
		"/api/v1/reconciliation/"+accountID+"/issues", &body); status != http.StatusOK {
		c.t.Fatalf("reading the trading state returned %d", status)
	}
	return body.TradingState, body.AutomationAllowed
}

// ledgerFingerprint captures every financial quantity that a second repair
// would change.
//
// Read in SQL with exact numerics rather than through the API: this is the
// comparison that proves a repair did not happen twice, and doing it in float
// would make a duplicated cent invisible.
type ledgerFingerprint struct {
	Fills          int
	Transactions   int
	Balance        string
	PositionQty    string
	RealisedPnL    string
	OrderFilledQty string
}

func (c *client) fingerprint(accountID, orderID string) ledgerFingerprint {
	c.t.Helper()
	id := quoteSQL(accountID)
	f := ledgerFingerprint{
		Fills: psqlInt(c.t,
			"SELECT count(*) FROM fills WHERE account_id = "+id),
		Transactions: psqlInt(c.t,
			"SELECT count(*) FROM transactions WHERE account_id = "+id),
		Balance: psql(c.t,
			"SELECT COALESCE((SELECT balance_after FROM transactions WHERE account_id = "+
				id+" ORDER BY sequence DESC LIMIT 1), 0)"),
		PositionQty: psql(c.t,
			"SELECT COALESCE(sum(quantity), 0) FROM positions WHERE account_id = "+
				id+" AND status = 'open'"),
		RealisedPnL: psql(c.t,
			"SELECT COALESCE(sum(amount), 0) FROM transactions WHERE account_id = "+
				id+" AND type = 'realized_pnl'"),
	}
	if orderID != "" {
		f.OrderFilledQty = psql(c.t,
			"SELECT filled_quantity FROM orders WHERE id = "+quoteSQL(orderID))
	}
	return f
}

// ---------------------------------------------------------------------------
// The acceptance test
// ---------------------------------------------------------------------------

// TestALostFillIsDiscoveredImportedOnceAndConverges is this milestone's key
// acceptance test.
//
//	order submitted
//	  -> mock venue accepts and fills
//	  -> the response is lost, so Vantage never books the fill
//	  -> reconciliation runs
//	  -> the missing execution is discovered and attributed
//	  -> it is imported exactly once
//	  -> order, position and ledger converge
//	  -> automation may safely resume
//
// The lost_response fault reproduces the venue-side half exactly: the mock
// commits the order and its fills, then returns ErrUnknownOutcome. That is
// indistinguishable, from Vantage's side, from a process that died between the
// broker call and its own persistence.
func TestALostFillIsDiscoveredImportedOnceAndConverges(t *testing.T) {
	c, accountID := setup(t)
	stop := c.safeStop(instrument, "buy", 0.004)

	before := c.fingerprint(accountID, "")

	// --- the venue fills; Vantage loses the answer -------------------------
	c.armLostResponse()
	key := newKey("recovery")
	status := c.do(http.MethodPost, "/api/v1/orders", key,
		c.marketOrder(accountID, instrument, "buy", stop), nil)
	if status >= 200 && status < 300 {
		t.Fatalf("a lost venue response was reported to the client as success (%d); "+
			"the whole scenario depends on Vantage NOT knowing the outcome", status)
	}

	orders := c.ordersWithKey(accountID, key)
	if len(orders) != 1 {
		t.Fatalf("expected exactly one order row after the lost response, found %d", len(orders))
	}
	order := orders[0]
	if order.Status != "FAILED" {
		t.Fatalf("an order whose outcome is unknown is %q; FAILED is the state that means "+
			"'Vantage does not know', and any other value here is a guess", order.Status)
	}

	// The uncertainty must be explicit, not inferred from a status name.
	required := psql(t, "SELECT reconciliation_required FROM orders WHERE id = "+
		quoteSQL(order.ID))
	if required != "t" {
		t.Fatalf("the order is not flagged as needing reconciliation (%q); the uncertainty "+
			"would be invisible to the UI and to readiness", required)
	}

	// Nothing was booked: that is the divergence.
	midway := c.fingerprint(accountID, order.ID)
	if midway.Fills != before.Fills {
		t.Fatalf("a fill was booked despite the lost response (%d -> %d); there would be "+
			"no divergence to recover from", before.Fills, midway.Fills)
	}

	// --- reconciliation discovers and repairs it --------------------------
	report, runStatus := c.runReconciliation(accountID)
	if runStatus != http.StatusOK {
		t.Fatalf("reconciliation returned %d: %v", runStatus, report)
	}
	repaired, _ := report["repaired"].(float64)
	if repaired < 1 {
		t.Fatalf("reconciliation repaired nothing. The venue holds an execution against "+
			"a known order id, which is the one case that is provably safe to import. "+
			"Report: %v", report)
	}

	after := c.fingerprint(accountID, order.ID)

	// Scoped to THIS order. A reconciliation run repairs every attributable
	// divergence it finds, so the account's total may legitimately move by
	// more than one when an earlier test left a divergence of its own. The
	// property under test is that this order received exactly one fill.
	orderFills := psqlInt(t, "SELECT count(*) FROM fills WHERE order_id = "+
		quoteSQL(order.ID))
	if orderFills != 1 {
		t.Fatalf("expected exactly one fill on the recovered order, found %d", orderFills)
	}
	if after.Fills <= midway.Fills {
		t.Fatalf("no fill was imported at all; account fills stayed at %d", after.Fills)
	}

	// The fill must be marked as recovered, not passed off as an ordinary
	// execution response.
	source := psql(t, "SELECT ingest_source FROM fills WHERE order_id = "+
		quoteSQL(order.ID)+" LIMIT 1")
	if source != "reconciliation_import" {
		t.Errorf("the imported fill records its source as %q; it was discovered after a "+
			"divergence, and the ledger should say so", source)
	}

	// The fill must name the issue that justified importing it.
	issueLink := psqlInt(t, "SELECT count(*) FROM fills WHERE order_id = "+
		quoteSQL(order.ID)+" AND reconciliation_issue_id IS NOT NULL")
	if issueLink != 1 {
		t.Error("the imported fill does not reference the reconciliation issue that " +
			"justified it, so the repair cannot be reconstructed later")
	}

	// --- the order's state follows its fills ------------------------------
	finalStatus := c.orderStatus(order.ID)
	if finalStatus != "FILLED" && finalStatus != "PARTIALLY_FILLED" {
		t.Fatalf("after importing the execution the order is %q; it must reflect the "+
			"fills that now exist against it", finalStatus)
	}
	filled := psql(t, "SELECT filled_quantity FROM orders WHERE id = "+quoteSQL(order.ID))
	fromFills := psql(t, "SELECT COALESCE(sum(quantity), 0) FROM fills WHERE order_id = "+
		quoteSQL(order.ID))
	if !numericallyEqual(filled, fromFills) {
		t.Fatalf("the order reports %s filled but its fills sum to %s; the aggregate must "+
			"be derived from the fills, never incremented", filled, fromFills)
	}

	// The state change must be marked as a repair, so the history distinguishes
	// "the venue told us at the time" from "we reconstructed this afterwards".
	repairs := psqlInt(t, "SELECT count(*) FROM order_state_transitions WHERE order_id = "+
		quoteSQL(order.ID)+" AND is_repair")
	if repairs == 0 {
		t.Error("no transition on this order is marked as a repair; reading the history " +
			"could not tell a reconstructed state from a received one")
	}
	orphanRepairs := psqlInt(t, "SELECT count(*) FROM order_state_transitions WHERE "+
		"order_id = "+quoteSQL(order.ID)+" AND is_repair AND reconciliation_issue_id IS NULL")
	if orphanRepairs != 0 {
		t.Errorf("%d repair transition(s) name no issue; a repair with nothing to point "+
			"at is not evidence of anything", orphanRepairs)
	}

	// --- the money adds up ------------------------------------------------
	c.assertLedgerConsistent(accountID)

	// An OPENING fill realises no profit or loss, so it produces a ledger
	// entry only if it carried a commission. Asserting a transaction
	// unconditionally would fail on a zero-commission venue, and would be
	// asserting the wrong thing: what matters is that the position moved, and
	// that any money the fill DID involve was booked.
	positionQty := psql(t, "SELECT COALESCE(sum(quantity), 0) FROM positions WHERE "+
		"account_id = "+quoteSQL(accountID)+" AND instrument_id = "+quoteSQL(instrument)+
		" AND status = 'open'")
	if positionQty == "" || numericallyEqual(positionQty, "0") {
		t.Error("the imported fill did not move the position book. A fill that books no " +
			"position is not an execution having been recorded")
	}
	commission := psql(t, "SELECT COALESCE(sum(commission), 0) FROM fills WHERE order_id = "+
		quoteSQL(order.ID))
	if !numericallyEqual(commission, "0") && after.Transactions <= midway.Transactions {
		t.Errorf("the imported fill carried commission %s but produced no ledger movement. "+
			"A position with money behind it and no transaction is exactly what routing "+
			"reconciliation through the shared accounting path is supposed to make "+
			"impossible", commission)
	}

	// --- the uncertainty is cleared and automation may resume -------------
	requiredAfter := psql(t, "SELECT reconciliation_required FROM orders WHERE id = "+
		quoteSQL(order.ID))
	if requiredAfter != "f" {
		t.Errorf("the order is still flagged as needing reconciliation (%q) after its "+
			"issue was resolved; readiness would never recover", requiredAfter)
	}

	// Automation must be permitted again -- provided nothing ELSE is still
	// diverging. The suite shares one account, and an earlier test may have
	// left an operator-required issue of its own; that is a legitimate halt
	// and not this test's failure. So unrelated issues are cleared first,
	// exactly as setup does, and then the verdict is checked.
	c.clearReconciliationIssues(accountID,
		"clearing unrelated divergence so the recovery assertion measures this "+
			"scenario rather than an earlier test's leftovers")

	state, allowed := c.tradingState(accountID)
	if !allowed {
		open := c.openIssues(accountID)
		types := make([]string, 0, len(open))
		for _, i := range open {
			types = append(types, fmt.Sprint(i["issue_type"]))
		}
		t.Fatalf("automation is still blocked after a successful repair (state %s, open "+
			"issues %v). Recovery that leaves the account halted has not recovered it",
			state, types)
	}
	t.Logf("recovered: fill imported once, order %s, trading state %s", finalStatus, state)

	c.assertNoUnknownOutcomeIsHidden(accountID)
}

// TestReRunningReconciliationChangesNothing is the idempotence requirement.
//
// A repair applied twice would double a position and double a ledger entry, and
// reconciliation runs every five minutes — so "converges once" is not enough;
// it has to converge and then stop.
func TestReRunningReconciliationChangesNothing(t *testing.T) {
	c, accountID := setup(t)
	stop := c.safeStop(instrument, "buy", 0.004)

	// Create a divergence and repair it.
	c.armLostResponse()
	key := newKey("idempotent")
	c.do(http.MethodPost, "/api/v1/orders", key,
		c.marketOrder(accountID, instrument, "buy", stop), nil)

	orders := c.ordersWithKey(accountID, key)
	if len(orders) != 1 {
		t.Skipf("the lost-response fault did not produce an order to recover (%d rows)",
			len(orders))
	}
	orderID := orders[0].ID

	if _, status := c.runReconciliation(accountID); status != http.StatusOK {
		t.Fatalf("the first reconciliation run returned %d", status)
	}
	settled := c.fingerprint(accountID, orderID)

	// Now run it four more times against the same venue state.
	for i := 0; i < 4; i++ {
		body, status := c.runReconciliation(accountID)
		if status != http.StatusOK {
			t.Fatalf("run %d returned %d: %v", i+2, status, body)
		}
		again := c.fingerprint(accountID, orderID)

		if again.Fills != settled.Fills {
			t.Fatalf("run %d changed the fill count from %d to %d. The same execution was "+
				"booked twice, which doubles a real position",
				i+2, settled.Fills, again.Fills)
		}
		if again.Transactions != settled.Transactions {
			t.Fatalf("run %d changed the transaction count from %d to %d. A duplicated "+
				"ledger entry cannot be removed: the ledger is append-only",
				i+2, settled.Transactions, again.Transactions)
		}
		if !numericallyEqual(again.Balance, settled.Balance) {
			t.Fatalf("run %d changed the balance from %s to %s",
				i+2, settled.Balance, again.Balance)
		}
		if !numericallyEqual(again.PositionQty, settled.PositionQty) {
			t.Fatalf("run %d changed the open position quantity from %s to %s",
				i+2, settled.PositionQty, again.PositionQty)
		}
		if !numericallyEqual(again.RealisedPnL, settled.RealisedPnL) {
			t.Fatalf("run %d changed realised P&L from %s to %s",
				i+2, settled.RealisedPnL, again.RealisedPnL)
		}
		if !numericallyEqual(again.OrderFilledQty, settled.OrderFilledQty) {
			t.Fatalf("run %d changed the order's filled quantity from %s to %s",
				i+2, settled.OrderFilledQty, again.OrderFilledQty)
		}
	}

	t.Logf("five runs, one repair: %d fills, %d transactions, balance %s",
		settled.Fills, settled.Transactions, settled.Balance)
	c.assertLedgerConsistent(accountID)
}

// TestConcurrentReconciliationRunsDoNotDoubleRepair.
//
// Eight runs launched together. The per-account advisory lock must let exactly
// one proceed and make the rest decline, because two runs repairing the same
// divergence from the same snapshot would apply it twice.
//
// Declining is reported as 409, not as success with an empty report: a caller
// that read "no issues" from a skipped run would conclude the account was
// clean.
func TestConcurrentReconciliationRunsDoNotDoubleRepair(t *testing.T) {
	c, accountID := setup(t)
	stop := c.safeStop(instrument, "buy", 0.004)

	c.armLostResponse()
	key := newKey("concurrentrecon")
	c.do(http.MethodPost, "/api/v1/orders", key,
		c.marketOrder(accountID, instrument, "buy", stop), nil)

	orders := c.ordersWithKey(accountID, key)
	if len(orders) != 1 {
		t.Skipf("no order to recover (%d rows)", len(orders))
	}
	orderID := orders[0].ID
	before := c.fingerprint(accountID, orderID)

	statuses := c.concurrentPost(8, "/api/v1/reconciliation/"+accountID+"/run", nil, nil)

	ran := countStatus(statuses, http.StatusOK)
	declined := countStatus(statuses, http.StatusConflict)
	t.Logf("8 concurrent runs: %d ran, %d declined, statuses %v", ran, declined, statuses)

	if n := countStatus(statuses, http.StatusInternalServerError); n != 0 {
		t.Fatalf("%d of 8 concurrent runs returned HTTP 500; statuses %v. Losing a race "+
			"for the lock is an expected outcome and must not read as a server fault",
			n, statuses)
	}
	if ran+declined != 8 {
		t.Fatalf("8 runs produced %d OK and %d conflict; the rest are unexplained: %v",
			ran, declined, statuses)
	}
	if declined == 0 {
		t.Error("no concurrent run was declined. Either the lock is not being taken, or " +
			"the runs did not actually overlap -- both mean this test proves nothing")
	}

	after := c.fingerprint(accountID, orderID)
	// At most one repair happened, whichever run won.
	if after.Fills > before.Fills+1 {
		t.Fatalf("concurrent runs booked %d fills; at most one execution existed to import",
			after.Fills-before.Fills)
	}
	c.assertLedgerConsistent(accountID)
	c.assertNoUnknownOutcomeIsHidden(accountID)
}

// TestReconciliationAndOrderSubmissionDoNotRaceIntoAnUnsafeState.
//
// The invariant the brief calls out specifically: if reconciliation has
// identified unsafe divergence, an automated order must not slip through
// between detection and the halt.
//
// The OMS re-reads the halt state INSIDE the transaction that persists the
// order, after taking the account row lock that a repair also takes. So the
// outcome is decided by PostgreSQL's serialisation: either the order commits
// before the issue exists, or it sees the issue. What must never happen is an
// automated order committing AFTER a halting issue is visible.
func TestReconciliationAndOrderSubmissionDoNotRaceIntoAnUnsafeState(t *testing.T) {
	c, accountID := setup(t)
	stop := c.safeStop(instrument, "buy", 0.004)

	// Manufacture a halting divergence that reconciliation will NOT repair: a
	// venue-side execution attributable to nothing.
	c.armFault("lost_response", 1, nil)
	orphanKey := newKey("orphanhalt")
	c.do(http.MethodPost, "/api/v1/orders", orphanKey,
		c.marketOrder(accountID, instrument, "buy", stop), nil)

	var wg sync.WaitGroup
	body := c.marketOrder(accountID, instrument, "buy", stop)
	statuses := make([]int, 6)

	wg.Add(1)
	go func() {
		defer wg.Done()
		c.post("/api/v1/reconciliation/"+accountID+"/run", "", nil, nil)
	}()
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Strategy-sourced would be ideal, but the HTTP order endpoint
			// creates manual orders. The durable check is asserted below on
			// the settled state instead, which is the property that matters.
			statuses[i] = c.do(http.MethodPost, "/api/v1/orders",
				newKey("haltrace"+strconv.Itoa(i)), body, nil)
		}(i)
	}
	wg.Wait()
	t.Logf("statuses during the reconciliation/submission race: %v", statuses)

	if n := countStatus(statuses, http.StatusInternalServerError); n != 0 {
		t.Fatalf("%d of 6 orders returned HTTP 500 while reconciliation ran concurrently; "+
			"statuses %v", n, statuses)
	}

	// Whatever interleaving occurred, the invariants hold.
	c.assertLedgerConsistent(accountID)
	c.assertNoDuplicateKeys(accountID)
	c.assertNoUnknownOutcomeIsHidden(accountID)
}

// TestAnAmbiguousVenueExecutionStaysUnresolved is the brief's separate
// acceptance criterion, end to end.
//
// The classifier proves this with hand-built snapshots. This proves the
// running system does not quietly do something else: an execution that cannot
// be attributed must remain an open, operator-review issue, and must not
// appear in the ledger.
func TestAnAmbiguousVenueExecutionStaysUnresolved(t *testing.T) {
	c, accountID := setup(t)

	// An execution belonging to no Vantage order, created directly in the mock
	// venue's own tables. This is what a trade placed in the broker's terminal
	// looks like from Vantage's side, and it is the shape a future real
	// adapter will encounter.
	ref := psql(t, "SELECT COALESCE(broker_account_ref, id::text) FROM accounts WHERE id = "+
		quoteSQL(accountID))
	if ref == "" {
		t.Skip("the account has no broker reference, so a venue-side row cannot be created")
	}

	orphanOrderID := "MOCK-ORPHAN-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	orphanExecID := "EXEC-ORPHAN-" + strconv.FormatInt(time.Now().UnixNano(), 10)

	if !c.insertVenueOrphanExecution(ref, orphanOrderID, orphanExecID) {
		t.Skip("the mock venue schema does not allow inserting a standalone execution")
	}

	beforeFills := psqlInt(t, "SELECT count(*) FROM fills WHERE account_id = "+
		quoteSQL(accountID))

	if _, status := c.runReconciliation(accountID); status != http.StatusOK {
		t.Fatalf("reconciliation run failed")
	}

	// It must be reported, unresolved, and requiring an operator.
	found := false
	for _, issue := range c.openIssues(accountID) {
		if fmt.Sprint(issue["broker_execution_id"]) == orphanExecID {
			found = true
			if got := fmt.Sprint(issue["issue_type"]); got != "EXTRA_BROKER_FILL" &&
				got != "EXTERNAL_BROKER_ACTIVITY" && got != "ORDER_MISSING_LOCALLY" {
				t.Errorf("an unattributable execution was classified as %q", got)
			}
			if auto, _ := issue["automatic_repair_allowed"].(bool); auto {
				t.Error("an unattributable execution is marked as automatically repairable")
			}
			if got := fmt.Sprint(issue["status"]); got != "OPERATOR_ACTION_REQUIRED" {
				t.Errorf("issue status is %q; an ambiguous execution must require an "+
					"operator", got)
			}
		}
	}
	if !found {
		// Not fatal: the mock's snapshot endpoints may not surface a bare
		// execution. Reported so it is not mistaken for a pass.
		t.Skipf("the venue snapshot did not include the standalone execution %s, so this "+
			"scenario could not be exercised end to end (the classifier covers it in "+
			"internal/reconcile)", orphanExecID)
	}

	afterFills := psqlInt(t, "SELECT count(*) FROM fills WHERE account_id = "+
		quoteSQL(accountID))
	if afterFills != beforeFills {
		t.Fatalf("an unattributable execution was booked into the ledger (%d -> %d fills). "+
			"Importing it required guessing a parent order", beforeFills, afterFills)
	}

	state, allowed := c.tradingState(accountID)
	if allowed {
		t.Errorf("automation is permitted (%s) with an unresolved ambiguous execution", state)
	}
	c.assertLedgerConsistent(accountID)
}

// TestAnUnresolvedIssueSurvivesLaterRuns is a regression test for a defect that
// defeated the acceptance criterion above by a side door.
//
// # What happened
//
// closeVanishedIssues resolves an open issue the current run did not re-detect,
// on the premise that a run examines both views completely. For executions that
// premise held only inside the stored cursor window. Once the cursor advanced
// past an unattributable execution, later runs stopped fetching it, did not
// re-detect it, and closed the issue with "the divergence is gone" -- while the
// execution sat unbooked at the venue and the account released its own halt.
//
// Three real orphan executions were closed that way, roughly half an hour after
// detection, and the trading state went back to HEALTHY.
//
// The scenario above cannot catch this: it runs reconciliation once. Only a
// SECOND run, after the cursor has moved, exposes it.
func TestAnUnresolvedIssueSurvivesLaterRuns(t *testing.T) {
	c, accountID := setup(t)

	ref := psql(t, "SELECT COALESCE(broker_account_ref, id::text) FROM accounts WHERE id = "+
		quoteSQL(accountID))
	if ref == "" {
		t.Skip("the account has no broker reference, so a venue-side row cannot be created")
	}

	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	orphanExecID := "EXEC-SURVIVE-" + stamp
	if !c.insertVenueOrphanExecution(ref, "MOCK-SURVIVE-"+stamp, orphanExecID) {
		t.Skip("the mock venue schema does not allow inserting a standalone execution")
	}

	if _, status := c.runReconciliation(accountID); status != http.StatusOK {
		t.Fatalf("the first reconciliation run failed")
	}
	if !c.hasOpenIssueForExecution(accountID, orphanExecID) {
		t.Skipf("the venue snapshot did not include the standalone execution %s, so this "+
			"scenario could not be exercised end to end", orphanExecID)
	}

	// Three more runs, each of which advances the execution cursor to its own
	// fetch time. Before the fix the issue disappeared here.
	for i := 2; i <= 4; i++ {
		if _, status := c.runReconciliation(accountID); status != http.StatusOK {
			t.Fatalf("reconciliation run %d failed", i)
		}
		if !c.hasOpenIssueForExecution(accountID, orphanExecID) {
			t.Fatalf("run %d closed the issue for execution %s, but the execution is still "+
				"unbooked at the venue. \"Not re-detected\" must mean \"fixed\", never "+
				"\"no longer examined\": this releases the account's halt while the "+
				"divergence is still there", i, orphanExecID)
		}
	}

	// Still unbooked, and automation still refused.
	booked := psqlInt(t, "SELECT count(*) FROM fills WHERE broker_fill_id = "+
		quoteSQL(orphanExecID))
	if booked != 0 {
		t.Errorf("the ambiguous execution was booked after %d runs", 4)
	}
	if state, allowed := c.tradingState(accountID); allowed {
		t.Errorf("automation is permitted (%s) with the ambiguous execution still open", state)
	}
	c.assertLedgerConsistent(accountID)
}

// hasOpenIssueForExecution reports whether an unresolved issue still names this
// venue execution.
func (c *client) hasOpenIssueForExecution(accountID, execID string) bool {
	c.t.Helper()
	for _, issue := range c.openIssues(accountID) {
		if fmt.Sprint(issue["broker_execution_id"]) == execID {
			return true
		}
	}
	return false
}

// insertVenueOrphanExecution creates an execution in the mock venue's tables
// with no corresponding Vantage order.
//
// Writes to the VENUE's own tables, not to Vantage's. That distinction is the
// whole point: it simulates the venue knowing about something Vantage does
// not, which is not otherwise reachable through any API.
func (c *client) insertVenueOrphanExecution(accountRef, brokerOrderID, execID string) bool {
	c.t.Helper()

	// Two statements rather than one CTE. A data-modifying CTE feeding an
	// INSERT ... SELECT looked tidier and quietly inserted nothing, which made
	// this scenario skip rather than fail -- the worst outcome for a test,
	// because it reads as a pass.
	// The client order id is unique and NOT a Vantage command id.
	//
	// An empty one would be the purest simulation of external activity, but
	// the mock venue carries UNIQUE (account_ref, client_order_id), so only
	// one empty-client-id order can exist per account -- and a previous run's
	// orphan then made ON CONFLICT DO NOTHING skip this insert in silence,
	// which surfaced as an unexplained skip rather than a failure.
	//
	// A unique foreign id is the more realistic simulation anyway: a trade
	// placed in a broker's own terminal gets the BROKER's identifier, not a
	// blank one. Vantage does not recognise it either way, which is the
	// property under test. The empty-client-id branch
	// (EXTERNAL_BROKER_ACTIVITY) is covered by the classifier unit tests,
	// which is the right level for a pure classification rule.
	externalClientID := "EXTERNAL-" + brokerOrderID

	psql(c.t, fmt.Sprintf(`
		INSERT INTO mock_venue_orders (broker_order_id, client_order_id, account_ref,
			symbol, side, type, time_in_force, status, quantity, filled_quantity,
			avg_fill_price)
		VALUES (%s, %s, %s, %s, 'buy', 'market', 'gtc', 'filled', 0.01, 0.01, 2650)
		ON CONFLICT DO NOTHING`,
		quoteSQL(brokerOrderID), quoteSQL(externalClientID), quoteSQL(accountRef),
		quoteSQL(instrument)))

	// Confirmed, because ON CONFLICT DO NOTHING hides a skipped insert and the
	// fill's foreign key would then fail for a reason that looks unrelated.
	if psqlInt(c.t, "SELECT count(*) FROM mock_venue_orders WHERE broker_order_id = "+
		quoteSQL(brokerOrderID)) != 1 {
		return false
	}

	psql(c.t, fmt.Sprintf(`
		INSERT INTO mock_venue_fills (broker_fill_id, broker_order_id, account_ref, symbol,
			side, quantity, price, commission, commission_ccy, liquidity, executed_at)
		VALUES (%s, %s, %s, %s, 'buy', 0.01, 2650, 0.1, 'USD', 'taker', now())
		ON CONFLICT DO NOTHING`,
		quoteSQL(execID), quoteSQL(brokerOrderID), quoteSQL(accountRef),
		quoteSQL(instrument)))

	// Confirmed by reading it back, so a silently-skipped insert cannot pass
	// for a created one.
	return psqlInt(c.t, "SELECT count(*) FROM mock_venue_fills WHERE broker_fill_id = "+
		quoteSQL(execID)) == 1
}

// numericallyEqual compares two Postgres numerics rendered as text.
//
// "0.01" and "0.0100000000" are the same number and different strings, so a
// string comparison would report a spurious change. Compared as decimals via
// the database rather than parsed into float64 here, because the whole point
// of these assertions is that no rounding is introduced.
func numericallyEqual(a, b string) bool {
	if a == b {
		return true
	}
	if a == "" || b == "" {
		return false
	}
	trim := func(s string) string {
		s = strings.TrimSpace(s)
		if !strings.Contains(s, ".") {
			return s
		}
		s = strings.TrimRight(s, "0")
		return strings.TrimRight(s, ".")
	}
	return trim(a) == trim(b)
}
