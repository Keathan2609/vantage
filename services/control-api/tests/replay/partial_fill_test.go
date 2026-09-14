package replay

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// A partial fill, made representable, and then survived across a restart.
//
// # Why a separate instrument was needed
//
// Scenario P could not be exercised at all on the seeded account. Every order
// it produces is 0.01 lots, which on XAUUSD.m is simultaneously the minimum
// quantity AND the quantity step, so forty percent of one order is 0.004 lots
// -- not a representable quantity -- and the venue correctly declined to split
// it. The partial-fill path is the one that touches the order state machine,
// the position, the weighted average price, the fee accrual and the ledger all
// at once, so leaving it unexercised is not acceptable.
//
// TEST_XAU is the answer: a development-only synthetic instrument with a finer
// lot step, on which 0.10 lots splits into 0.04 and 0.06. Nothing about
// XAUUSD.m, the account's size or the authority's 0.10-lot ceiling was
// relaxed -- those are the assumptions under test.
//
// # Why the order is placed by hand
//
// No strategy declares TEST_XAU and none should: a synthetic instrument must
// never enter an autonomous decision or a research result. A manual order goes
// through the SAME oms.Submit and the SAME booking package as an autonomous
// one -- that is enforced structurally by TestEveryOrderPlacementGoesThrough-
// TheSameOMSMethod -- so the fill, position, ledger and state-machine
// behaviour under test is identical. What this does NOT exercise is the
// orchestrator's front end, and that is stated rather than implied.

const syntheticInstrument = "TEST_XAU"

// placeOrder submits a manual order and returns the status and the order id.
func (h *harness) placeOrder(accountID, instrument, side, quantity, stop,
	idempotencyKey string) (int, string) {

	h.t.Helper()
	var out struct {
		Order struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"order"`
		Rejection *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"rejection"`
	}
	code := h.doWithKey("POST", "/api/v1/orders", idempotencyKey, map[string]string{
		"account_id":    accountID,
		"instrument_id": instrument,
		"side":          side,
		"type":          "market",
		"quantity":      quantity,
		"stop_loss":     stop,
		"time_in_force": "gtc",
	}, &out)
	if out.Rejection != nil {
		h.t.Logf("the order was refused: %s — %s",
			out.Rejection.Code, out.Rejection.Message)
	}
	return code, out.Order.ID
}

// replayQuote reads the instrument's current bid, as the replay put it there.
func replayQuote(t *testing.T, instrument string) string {
	t.Helper()
	return psql(t, fmt.Sprintf(
		`SELECT bid::text FROM market_quotes_latest WHERE instrument_id = '%s'`,
		instrument))
}

// startSyntheticRun opens the synthetic dataset and steps past its warm-up.
func startSyntheticRun(t *testing.T, h *harness, reason string) {
	t.Helper()
	h.start("test-partial-fill", reason)
	// Past the sixty-instant warm-up, so the instrument has a settled quote
	// and the run is in the phase an operator would actually trade in. Short
	// of the dataset's eighty, so the run is still ACTIVE.
	h.step(72)

	if q := replayQuote(t, syntheticInstrument); q == "" {
		t.Fatalf("%s has no quote after the warm-up, so an order against it "+
			"would be refused for a stale feed rather than tested",
			syntheticInstrument)
	}
}

func TestAPartialFillOnTheSyntheticInstrumentBooksExactlyOnce(t *testing.T) {
	h := newHarness(t)
	t.Cleanup(func() { h.t = t; h.resetFaults(); h.stopQuietly() })

	trader := traderSession(t, h.base)
	accountID := psql(t, `SELECT id::text FROM accounts ORDER BY created_at LIMIT 1`)

	startSyntheticRun(t, h, "partial fill on the synthetic instrument")

	// 0.4 of the requested quantity: 0.10 lots becomes 0.04 now and 0.06
	// working. Armed once, so exactly one order is split and the assertions
	// below have a single subject.
	h.armFault("partial_fill", 1, map[string]any{"fraction": "0.4"})

	bid := replayQuote(t, syntheticInstrument)
	if bid == "" {
		t.Fatal("no quote to size a stop against")
	}
	stop := shiftPrice(t, bid, "-0.01") // a stop 1% below, comfortably valid

	trader.t = t
	code, orderID := trader.placeOrder(accountID, syntheticInstrument, "buy",
		"0.1000", stop, "partial-fill-"+fmt.Sprint(time.Now().UnixNano()))
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("placing the order returned %d", code)
	}
	if orderID == "" {
		t.Fatal("the order was not created, so there is nothing to split")
	}

	// 1. THE POINT: the venue actually split it.
	partial := psqlInt(t, fmt.Sprintf(`
		SELECT count(*) FROM order_state_transitions
		WHERE order_id = '%s' AND to_status = 'PARTIALLY_FILLED'`, orderID))
	if partial == 0 {
		qty := psqlRow(t, `SELECT min_quantity || ' / ' || quantity_step
			FROM instruments WHERE id = '%s'`, syntheticInstrument)
		t.Fatalf("the venue reported no partial fill for a 0.1000-lot order on "+
			"an instrument whose min/step is %s. Forty percent of 0.1000 is "+
			"0.0400, which IS representable here, so this is a defect rather "+
			"than the arithmetic limit scenario P documented.", qty)
	}

	// 2. The first execution is exactly the fraction that was forced. A split
	//    at some other size would mean the fault is approximate, and every
	//    later assertion would be measuring something unspecified.
	first := psqlRow(t, `SELECT quantity::text FROM fills
		WHERE order_id = '%s' ORDER BY created_at, id LIMIT 1`, orderID)
	if first != "0.0400000000" {
		t.Errorf("the first fill was %s lots, want 0.0400000000 (40%% of 0.1000)",
			first)
	}

	// 3. Exactness. Each of these is a place a partial fill can double-count.
	assertFillsReconcile(t, orderID)

	// 4. And the account as a whole is still sound.
	assertUniversalInvariants(t, h.observe())
}

func TestAPartialFillSurvivesARestartWithoutDuplicating(t *testing.T) {
	// The combination the brief asks for. A half-filled order is the most
	// dangerous thing to hold across a restart: the venue has executed part of
	// it, the platform owes a position for that part, and the remainder is
	// still working. Re-booking the executed part on recovery would credit the
	// account with a position it does not hold.
	h := newHarness(t)
	t.Cleanup(func() { h.t = t; h.resetFaults(); h.stopQuietly() })

	trader := traderSession(t, h.base)
	accountID := psql(t, `SELECT id::text FROM accounts ORDER BY created_at LIMIT 1`)

	startSyntheticRun(t, h, "partial fill held across a restart")
	h.armFault("partial_fill", 1, map[string]any{"fraction": "0.4"})

	bid := replayQuote(t, syntheticInstrument)
	stop := shiftPrice(t, bid, "-0.01")
	trader.t = t
	if code, orderID := trader.placeOrder(accountID, syntheticInstrument, "buy",
		"0.1000", stop, "partial-restart-"+fmt.Sprint(time.Now().UnixNano())); code >= 300 {
		t.Fatalf("placing the order returned %d", code)
	} else if orderID == "" {
		t.Fatal("the order was not created")
	}

	partialOrder := psql(t, `
		SELECT order_id::text FROM order_state_transitions
		WHERE to_status = 'PARTIALLY_FILLED' ORDER BY created_at DESC LIMIT 1`)
	if partialOrder == "" {
		t.Skip("no order reached PARTIALLY_FILLED, so there is nothing to " +
			"carry across a restart")
	}

	before := h.observe()
	beforeQty := psqlRow(t, `SELECT coalesce(sum(quantity),0)::text FROM fills
		WHERE order_id = '%s'`, partialOrder)
	beforeStatus := psqlRow(t, `SELECT status FROM orders WHERE id = '%s'`, partialOrder)
	t.Logf("before the restart: order %s is %s with %s lots filled",
		partialOrder, beforeStatus, beforeQty)

	// ---- The restart -------------------------------------------------------
	h.restartControlPlane()

	// ---- After -------------------------------------------------------------
	after := h.observe()

	// 1. Nothing was re-booked. This is the whole point.
	afterQty := psqlRow(t, `SELECT coalesce(sum(quantity),0)::text FROM fills
		WHERE order_id = '%s'`, partialOrder)
	if afterQty != beforeQty {
		t.Errorf("the filled quantity of the half-filled order changed across "+
			"the restart: %s before, %s after. A recovery that re-books an "+
			"execution credits a position the account does not hold.",
			beforeQty, afterQty)
	}
	if after.Fills != before.Fills {
		t.Errorf("fills changed across the restart: %d before, %d after",
			before.Fills, after.Fills)
	}
	if after.Ledger != before.Ledger {
		t.Errorf("ledger entries changed across the restart: %d before, %d after",
			before.Ledger, after.Ledger)
	}
	if after.StoredBalance != before.StoredBalance {
		t.Errorf("the balance changed across the restart: %s before, %s after",
			before.StoredBalance, after.StoredBalance)
	}

	// 2. The order did not silently complete or vanish. Either status is
	//    defensible -- reconciliation may legitimately close it out -- but it
	//    must be one the state machine allows from PARTIALLY_FILLED.
	afterStatus := psqlRow(t, `SELECT status FROM orders WHERE id = '%s'`, partialOrder)
	switch afterStatus {
	case "PARTIALLY_FILLED", "FILLED", "CANCELLED", "FAILED":
	default:
		t.Errorf("the half-filled order is %q after the restart, which is not a "+
			"state reachable from PARTIALLY_FILLED", afterStatus)
	}
	t.Logf("after the restart: order %s is %s with %s lots filled",
		partialOrder, afterStatus, afterQty)

	// 3. And the books still reconcile.
	assertFillsReconcile(t, partialOrder)
	assertUniversalInvariants(t, after)
}

// assertFillsReconcile checks every place a split execution can double-count.
func assertFillsReconcile(t *testing.T, orderID string) {
	t.Helper()

	// The order's filled quantity equals the sum of its fills.
	drift := psqlRow(t, `
		SELECT (o.filled_quantity - coalesce(sum(f.quantity), 0))::text
		FROM orders o LEFT JOIN fills f ON f.order_id = o.id
		WHERE o.id = '%s' GROUP BY o.filled_quantity`, orderID)
	if drift != "" && drift != "0.0000000000" && drift != "0" {
		t.Errorf("the order's filled_quantity differs from the sum of its fills "+
			"by %s: a split execution was counted more or fewer times than it "+
			"occurred", drift)
	}

	// Never more than requested. Over-filling is the failure that turns a
	// partial fill into an unasked-for position.
	over := psqlInt(t, fmt.Sprintf(
		`SELECT count(*) FROM orders WHERE id = '%s' AND filled_quantity > quantity`,
		orderID))
	if over > 0 {
		t.Error("the order is filled for more than it requested")
	}

	// The weighted average price is inside the range of its own fills. A
	// double-counted fill or a plain average over unequal sizes lands outside.
	outside := psqlInt(t, fmt.Sprintf(`
		SELECT count(*) FROM orders o
		WHERE o.id = '%s' AND o.filled_quantity > 0 AND (
			o.avg_fill_price < (SELECT min(price) FROM fills WHERE order_id = o.id) OR
			o.avg_fill_price > (SELECT max(price) FROM fills WHERE order_id = o.id))`,
		orderID))
	if outside > 0 {
		t.Error("the order's average fill price lies outside the range of its " +
			"own fills, so it is not a weighted average of them")
	}

	// Every fill was booked into the ledger exactly once.
	unbooked := psqlInt(t, fmt.Sprintf(`
		SELECT count(*) FROM fills f
		WHERE f.order_id = '%s'
		  AND NOT EXISTS (SELECT 1 FROM transactions t WHERE t.fill_id = f.id)`,
		orderID))
	if unbooked > 0 {
		t.Errorf("%d fills have no ledger entry", unbooked)
	}
	doubled := psqlInt(t, fmt.Sprintf(`
		SELECT count(*) FROM (
			SELECT t.fill_id FROM transactions t
			JOIN fills f ON f.id = t.fill_id
			WHERE f.order_id = '%s' AND t.type = 'commission'
			GROUP BY 1 HAVING count(*) > 1) d`, orderID))
	if doubled > 0 {
		t.Errorf("%d fills were charged commission more than once", doubled)
	}
}
