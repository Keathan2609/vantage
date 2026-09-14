package replay

import (
	"net/http"
	"testing"
)

// Crash TIMING: what a restart does when it lands inside one order's lifetime.
//
// # What these do, and what they honestly do not
//
// Scenario S kills the control plane at an arbitrary point in a run. That
// proves a restart duplicates nothing in general, and it says nothing about
// the two moments that actually matter:
//
//	1. the platform asked the venue to do something and never learned the
//	   answer, and then died;
//	2. the venue EXECUTED, the answer was lost, and then the platform died
//	   before it could reconcile.
//
// These are not reached by killing the process faster. They are reached by
// making the venue behave exactly as it would at that instant -- which is what
// the deterministic fault modes are for -- and then killing the process for
// real. So the venue state is produced deliberately and the restart is
// genuine.
//
// What they do NOT do is kill the process at a chosen instruction boundary
// inside the OMS transaction. That would need a fault that blocks at a named
// point, which does not exist, and the milestone records it as uncovered
// rather than implying otherwise. The gap that leaves is narrow: the order
// write and the venue call are not in one transaction, and the two cases below
// bracket the window between them.

// crashWindowSetup drives a run to the point where orders are being produced.
func crashWindowSetup(t *testing.T, h *harness, reason string) {
	t.Helper()
	h.start("trend-clean", reason)
	// Past the warm-up and into the productive part, so the fault has
	// order-producing instants left to land on.
	h.step(95)
}

func TestAnUnknownOutcomeStaysUnknownAcrossARestart(t *testing.T) {
	// Case 1. The venue timed out: NOTHING was written there, but the platform
	// cannot know that. The order is FAILED with the outcome unknown, and a
	// restart must not resolve it in either direction -- inventing a fill
	// would credit a position that does not exist, and inventing a rejection
	// would discard one that might.
	h := newHarness(t)
	t.Cleanup(func() { h.t = t; h.resetFaults(); h.stopQuietly() })

	crashWindowSetup(t, h, "crash timing: an unknown outcome carried across a restart")

	h.armFault("timeout", 1, nil)
	baseline := psqlInt(t, "SELECT count(*) FROM orders")
	if got := h.stepUntilOrders(baseline, 1, 60); got == 0 {
		t.Skip("no order was produced after the timeout fault was armed, so " +
			"there is no unknown outcome to carry")
	}

	unknown := psql(t, `
		SELECT id::text FROM orders
		WHERE status = 'FAILED' OR reconciliation_required
		ORDER BY created_at DESC LIMIT 1`)
	if unknown == "" {
		t.Skipf("no order ended with an unknown outcome\n%s", h.observe().describe())
	}
	before := h.observe()
	beforeStatus := psqlRow(t, `SELECT status FROM orders WHERE id = '%s'`, unknown)
	beforeFills := psqlInt(t, "SELECT count(*) FROM fills WHERE order_id = '"+unknown+"'")
	t.Logf("before the restart: order %s is %s with %d fills",
		unknown, beforeStatus, beforeFills)

	h.restartControlPlane()

	// 1. The order was NOT resolved by the restart. Start-up reconciliation
	//    may legitimately look at it, but it must not guess.
	after := h.observe()
	afterStatus := psqlRow(t, `SELECT status FROM orders WHERE id = '%s'`, unknown)
	afterFills := psqlInt(t, "SELECT count(*) FROM fills WHERE order_id = '"+unknown+"'")
	if afterFills != beforeFills {
		t.Errorf("the unknown-outcome order gained or lost fills across the "+
			"restart: %d before, %d after. Nothing was executed at the venue, "+
			"so a fill here is invented.", beforeFills, afterFills)
	}
	if afterStatus == "FILLED" {
		t.Errorf("an order whose outcome was UNKNOWN is %q after a restart: the "+
			"platform resolved an uncertainty it had no evidence for", afterStatus)
	}

	// 2. The books are unchanged and still sound.
	if after.Ledger != before.Ledger || after.StoredBalance != before.StoredBalance {
		t.Errorf("the ledger moved across the restart of an unknown outcome: "+
			"%d entries / %s before, %d / %s after",
			before.Ledger, before.StoredBalance, after.Ledger, after.StoredBalance)
	}
	assertUniversalInvariants(t, after)
	t.Logf("after the restart: order %s is %s with %d fills",
		unknown, afterStatus, afterFills)
}

func TestAnExecutionLostToACrashIsBookedExactlyOnceOrLeftOpen(t *testing.T) {
	// Case 2, and the dangerous one. The venue ACCEPTED and recorded the
	// order, the response was dropped, and then the process died before
	// reconciliation could settle it. The execution exists at the venue and
	// the platform has no record of it.
	//
	// Two outcomes are defensible after the restart: reconciliation imports it
	// exactly once, or it stays an open issue for an operator. What is not
	// defensible is importing it twice, inventing a position, or closing the
	// issue without re-reading the evidence.
	h := newHarness(t)
	t.Cleanup(func() { h.t = t; h.resetFaults(); h.stopQuietly() })

	crashWindowSetup(t, h, "crash timing: an execution lost to a crash")

	h.armFault("lost_response", 1, nil)
	baseline := psqlInt(t, "SELECT count(*) FROM orders")
	if got := h.stepUntilOrders(baseline, 1, 60); got == 0 {
		t.Skip("no order was produced after the lost-response fault was armed")
	}

	uncertain := psqlInt(t, `
		SELECT count(*) FROM orders
		WHERE reconciliation_required OR status = 'FAILED'`)
	if uncertain == 0 {
		t.Skipf("no order ended in an uncertain state, so the lost response was "+
			"not exercised\n%s", h.observe().describe())
	}
	before := h.observe()
	// What the VENUE holds, which is the thing the platform does not know
	// about. Read from the mock venue's own tables rather than from ours.
	venueFills := psqlInt(t, "SELECT count(*) FROM mock_venue_fills")
	t.Logf("before the restart: %d uncertain orders, %d venue executions, "+
		"%d local fills", uncertain, venueFills, before.Fills)

	h.restartControlPlane()

	// Give start-up reconciliation its chance, at the replay instant rather
	// than at wall time. The run is interrupted, so this runs through the
	// ordinary control rather than the replay one.
	accountID := psql(t, `SELECT id::text FROM accounts ORDER BY created_at LIMIT 1`)
	if code := h.do("POST", "/api/v1/reconciliation/"+accountID+"/run",
		map[string]any{}, nil); code != http.StatusOK && code != http.StatusAccepted {
		t.Logf("the manual reconciliation run returned %d; start-up "+
			"reconciliation has already run regardless", code)
	}

	after := h.observe()

	// 1. NEVER twice. This is the invariant the whole case exists for.
	dupes := psqlInt(t, `
		SELECT count(*) FROM (
			SELECT broker_name, broker_fill_id, count(*) AS n
			FROM fills GROUP BY 1, 2 HAVING count(*) > 1
		) d`)
	if dupes > 0 {
		t.Errorf("%d venue executions were imported more than once across the "+
			"restart. A recovered fill booked twice is a position the account "+
			"does not hold.", dupes)
	}

	// 2. Anything that WAS recovered went through booking.
	//
	// NOT "every fill has a ledger entry" -- that is false, and asserting it
	// reported 14 correct fills as defects. `booking` writes a transaction
	// only when realised P&L is non-zero or commission is positive, so an
	// OPENING fill on a zero-commission instrument legitimately moves no money
	// and produces no ledger row. The seeded instruments charge no commission,
	// which makes that the common case rather than an edge one.
	//
	// What IS true, and is what booking guarantees: a fill that carried
	// commission has a commission entry, and every fill belongs to an order.
	unbooked := psqlInt(t, `
		SELECT count(*) FROM fills f
		WHERE f.commission > 0
		  AND NOT EXISTS (SELECT 1 FROM transactions t
		                  WHERE t.fill_id = f.id AND t.type = 'commission')`)
	if unbooked > 0 {
		t.Errorf("%d fills carried commission and have no commission ledger "+
			"entry, so they bypassed booking", unbooked)
	}
	orphan := psqlInt(t, `
		SELECT count(*) FROM fills f
		WHERE NOT EXISTS (SELECT 1 FROM orders o WHERE o.id = f.order_id)`)
	if orphan > 0 {
		t.Errorf("%d fills belong to no order", orphan)
	}

	// 3. And if it was NOT recovered, it is still open for an operator rather
	//    than quietly forgotten. "Not re-detected" must mean "fixed", never
	//    "no longer examined".
	open := psqlInt(t, `SELECT count(*) FROM reconciliation_issues WHERE status = 'open'`)
	imported := after.Fills - before.Fills
	if imported == 0 && open == 0 && venueFills > before.Fills {
		t.Errorf("the venue holds %d executions and the platform booked %d, yet "+
			"no reconciliation issue is open. The divergence was neither "+
			"repaired nor recorded.", venueFills, after.Fills)
	}

	assertUniversalInvariants(t, after)
	t.Logf("after the restart: %d executions imported, %d issues open, "+
		"%d fills, balance %s == %s",
		imported, open, after.Fills, after.StoredBalance, after.DerivedBalance)
}
