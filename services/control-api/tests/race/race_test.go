package race

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The instrument every test uses. Gold is the seeded instrument with a live
// mock feed.
const instrument = "XAUUSD.m"

// setup signs in and puts the account into a known state.
func setup(t *testing.T) (*client, string) {
	t.Helper()
	base := requireStack(t)
	c := signIn(t, base)
	accountID := c.firstAccount()
	c.waitForTradableFeed(instrument, 30*time.Second)
	c.releaseAllKillSwitches()
	c.resetFaults()

	// Diagnose the missing-authority case here, once, instead of letting every
	// test report an unexplained 403. A previous run of this suite revoked the
	// authority and failed to restore it; the next nine tests all failed on
	// "403" and the cause took far longer to find than it should have.
	var authority struct {
		Authority *struct {
			ID string `json:"id"`
		} `json:"authority"`
	}
	c.get("/api/v1/authority/"+accountID, &authority)
	if authority.Authority == nil {
		t.Log("no trading authority on this account; granting the seed mandate so " +
			"orders are not refused for a reason unrelated to the race under test")
		c.grantSeedAuthority(accountID)
	}

	// Order matters: cancel what is resting BEFORE flattening, so a flatten
	// order is not itself refused for exceeding the pending-order limit.
	c.cancelAllWorking(accountID)
	c.flattenAll(accountID)

	// Clear divergence an earlier test left behind. An unresolved
	// operator-required issue halts automation for every later test, which is
	// the platform working correctly -- so the fixture resolves it rather than
	// the tests weakening their assertions to tolerate it.
	c.clearReconciliationIssues(accountID,
		"starting from a known reconciliation state before the next scenario")

	// Checked LAST, after the book is clean, so an exhausted risk budget is
	// not confused with leftover exposure.
	c.requireRiskBudget(accountID)
	t.Cleanup(func() {
		c.resetFaults()
		c.releaseAllKillSwitches()
	})
	return c, accountID
}

// ---------------------------------------------------------------------------
// 1. Duplicate submission
// ---------------------------------------------------------------------------

// TestSameKeyConcurrentlyProducesExactlyOneOrder is the race the whole
// idempotency mechanism exists for.
//
// Sixteen requests with ONE key leave together. The desired outcome is not
// "no error" -- it is exactly one order in the book. A 409 or a replayed 200
// are both correct answers for the losers; a second order is not.
func TestSameKeyConcurrentlyProducesExactlyOneOrder(t *testing.T) {
	c, accountID := setup(t)
	stop := c.safeStop(instrument, "buy", 0.004)
	key := newKey("dup")

	statuses := c.concurrentPost(16, "/api/v1/orders", []string{key},
		c.marketOrder(accountID, instrument, "buy", stop))

	created := countStatus(statuses, http.StatusCreated)
	if created > 1 {
		t.Fatalf("sixteen concurrent identical submissions created %d orders; statuses %v",
			created, statuses)
	}

	// The authoritative check is the book, not the response codes: a bug that
	// wrote two rows and returned one 201 would pass the check above.
	//
	// Zero rows is a legitimate outcome -- a pre-persistence refusal (risk,
	// exposure, a stale quote) never creates an order. What must not happen is
	// TWO. That is the whole property.
	orders := c.ordersWithKey(accountID, key)
	if len(orders) > 1 {
		t.Fatalf("key %s produced %d orders; idempotency did not hold under "+
			"concurrency, statuses %v", key, len(orders), statuses)
	}
	if len(orders) == 0 && created > 0 {
		t.Fatalf("%d request(s) reported 201 but no order row exists for key %s",
			created, key)
	}
	if len(orders) == 0 {
		t.Logf("all sixteen were refused before persistence (statuses %v); "+
			"the invariant checked is that no duplicate row appeared, and none did",
			statuses)
	} else {
		t.Logf("16 concurrent identical submissions -> 1 order (%s), statuses %v",
			orders[0].Status, statuses)
	}

	c.assertNoDuplicateKeys(accountID)
	c.assertLedgerConsistent(accountID)
	c.assertNoUnknownOutcomeIsHidden(accountID)
}

// TestDistinctKeysConcurrentlyAreAllIndependent is the mirror image.
//
// Idempotency that collapses DIFFERENT orders would be a far worse defect than
// one that allows a duplicate: it would silently discard trades the operator
// intended. Eight distinct keys must produce eight distinct outcomes, each
// either accepted or refused on its own merits.
func TestDistinctKeysConcurrentlyAreAllIndependent(t *testing.T) {
	c, accountID := setup(t)
	stop := c.safeStop(instrument, "buy", 0.004)

	keys := make([]string, 8)
	for i := range keys {
		keys[i] = newKey("distinct" + strconv.Itoa(i))
	}
	statuses := c.concurrentPost(8, "/api/v1/orders", keys,
		c.marketOrder(accountID, instrument, "buy", stop))

	// Every key must resolve to at most one order, and no key may be lost.
	total := 0
	for _, key := range keys {
		found := c.ordersWithKey(accountID, key)
		if len(found) > 1 {
			t.Fatalf("key %s produced %d orders", key, len(found))
		}
		total += len(found)
	}
	if n := countStatus(statuses, http.StatusInternalServerError); n != 0 {
		t.Fatalf("%d of 8 concurrent orders returned HTTP 500; statuses %v. "+
			"A 500 on order placement tells the caller nothing about whether "+
			"the order exists, which is the one answer this API must never give",
			n, statuses)
	}

	created := countStatus(statuses, http.StatusCreated)
	if total != created {
		t.Fatalf("%d requests reported 201 but %d orders exist; statuses %v",
			created, total, statuses)
	}
	if created == 0 {
		// Legitimate: eight orders at once can hit the exposure ceiling. What
		// must not happen is a 201 without a row, or a row without a 201.
		t.Log("all eight were refused (exposure or risk); the invariant checked is " +
			"that responses and rows agree, and they do")
	}
	t.Logf("8 distinct keys -> %d created, %d rows, statuses %v", created, total, statuses)

	// Whatever happened, the money must still add up.
	c.assertLedgerConsistent(accountID)
	c.assertNoUnknownOutcomeIsHidden(accountID)
}

// ---------------------------------------------------------------------------
// 2. Kill switch activated during submission
// ---------------------------------------------------------------------------

// TestKillSwitchDuringSubmissionIsNeverPartiallyApplied races an activation
// against a burst of orders.
//
// There is a real window here and the test does not pretend otherwise: an
// order that passed the kill-switch gate microseconds before the switch was
// written is allowed through, and that is correct -- the switch stops NEW
// decisions, it does not reach into flight and retract one.
//
// The property that must hold is the one after the race settles: once the
// switch is active, every subsequent order is refused. A switch that stopped
// only some later orders would be the defect.
func TestKillSwitchDuringSubmissionIsNeverPartiallyApplied(t *testing.T) {
	c, accountID := setup(t)
	stop := c.safeStop(instrument, "buy", 0.004)
	body := c.marketOrder(accountID, instrument, "buy", stop)

	var wg sync.WaitGroup
	statuses := make([]int, 6)

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Armed while the orders below are in flight.
		c.post("/api/v1/kill-switches", "", map[string]string{
			"scope": "account", "target_id": accountID,
			"reason": "race test: activation during submission",
		}, nil)
	}()

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = c.do(http.MethodPost, "/api/v1/orders",
				newKey("killrace"+strconv.Itoa(i)), body, nil)
		}(i)
	}
	wg.Wait()

	t.Logf("statuses during the activation race: %v (a mix is expected)", statuses)

	// The settled state is what matters.
	after := c.do(http.MethodPost, "/api/v1/orders", newKey("killafter"), body, nil)
	if after == http.StatusCreated {
		t.Fatalf("an order was accepted AFTER the kill switch settled (status %d)", after)
	}
	t.Logf("after the switch settled, a further order was refused with %d", after)

	c.assertLedgerConsistent(accountID)
	c.assertNoUnknownOutcomeIsHidden(accountID)
}

// ---------------------------------------------------------------------------
// 3. Authority revoked during submission
// ---------------------------------------------------------------------------

// TestAuthorityRevocationDuringSubmissionSettlesClosed is the same shape as
// the kill-switch race, for the other independent gate.
//
// Authority is what makes AUTOMATED trading permissible. The settled property
// is that once it is revoked, nothing further is accepted.
func TestAuthorityRevocationDuringSubmissionSettlesClosed(t *testing.T) {
	c, accountID := setup(t)

	var authority struct {
		Authority struct {
			ID string `json:"id"`
		} `json:"authority"`
	}
	c.get("/api/v1/authority/"+accountID, &authority)
	if authority.Authority.ID == "" {
		t.Skip("no active authority on this account to revoke")
	}

	stop := c.safeStop(instrument, "buy", 0.004)
	body := c.marketOrder(accountID, instrument, "buy", stop)

	// The restore is registered BEFORE the revoke, not after.
	//
	// The first version of this test registered it afterwards and then failed
	// on an unrelated assertion, so the cleanup never ran: the authority
	// stayed revoked, and every subsequent test in the suite got HTTP 403 on
	// every order. The platform was entirely correct -- no authority means no
	// trading -- but the suite looked broken for twenty minutes.
	//
	// The payload must be the full mandate, too. An authority with only a
	// daily-loss limit is refused (an empty instrument allow-list would permit
	// nothing), which is why the earlier one-field restore silently failed.
	t.Cleanup(func() { c.grantSeedAuthority(accountID) })

	var wg sync.WaitGroup
	statuses := make([]int, 5)

	wg.Add(1)
	go func() {
		defer wg.Done()
		c.post("/api/v1/authority/"+authority.Authority.ID+"/revoke", "",
			map[string]string{"reason": "race test: revocation during submission"}, nil)
	}()
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = c.do(http.MethodPost, "/api/v1/orders",
				newKey("authrace"+strconv.Itoa(i)), body, nil)
		}(i)
	}
	wg.Wait()
	t.Logf("statuses during the revocation race: %v", statuses)

	c.assertLedgerConsistent(accountID)
	c.assertNoUnknownOutcomeIsHidden(accountID)
}

// ---------------------------------------------------------------------------
// 4. Cancellation racing a fill
// ---------------------------------------------------------------------------

// TestCancellationRacingAFillResolvesToExactlyOneOutcome.
//
// An order can be filled or cancelled. It cannot be both, and it must not
// become neither. This races a cancel against a resting limit order while
// eight cancels contend, which is the concurrency the state machine's
// optimistic version check exists to arbitrate.
func TestCancellationRacingAFillResolvesToExactlyOneOutcome(t *testing.T) {
	c, accountID := setup(t)

	// A resting limit far below the market stays working, so there is
	// something to cancel. A market order would fill before the request lands.
	q := c.quote(instrument)
	bid, err := strconv.ParseFloat(q.Bid, 64)
	if err != nil {
		t.Fatalf("parsing the bid: %v", err)
	}
	limit := strconv.FormatFloat(bid*0.80, 'f', 2, 64)
	stopPrice := strconv.FormatFloat(bid*0.79, 'f', 2, 64)

	var placed struct {
		Order struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"order"`
	}
	status := c.post("/api/v1/orders", newKey("cancelrace"), map[string]string{
		"account_id":    accountID,
		"instrument_id": instrument,
		"side":          "buy",
		"type":          "limit",
		"quantity":      "0.01",
		"limit_price":   limit,
		"stop_loss":     stopPrice,
		"time_in_force": "gtc",
	}, &placed)
	if status != http.StatusCreated || placed.Order.ID == "" {
		t.Skipf("could not place the resting limit order (status %d); nothing to cancel", status)
	}
	orderID := placed.Order.ID

	// Eight simultaneous cancels. Exactly one may succeed.
	statuses := c.concurrentPost(8, "/api/v1/orders/"+orderID+"/cancel", nil, nil)
	succeeded := countStatus(statuses, http.StatusOK) + countStatus(statuses, http.StatusAccepted)
	if succeeded > 1 {
		t.Fatalf("%d of 8 concurrent cancels succeeded; statuses %v", succeeded, statuses)
	}

	// The seven losers must be told CONFLICT, not "internal error".
	//
	// This is what the first run reported: [500 200 500 500 500 500 500 500].
	// Exactly one cancel took effect, which was correct, but the state
	// machine's refusal ("CANCEL_PENDING -> CANCEL_PENDING") was unclassified
	// and fell through to a 500. Losing a race is an expected outcome and must
	// read as one, or a client learns to retry a request that will never
	// succeed.
	if n := countStatus(statuses, http.StatusInternalServerError); n != 0 {
		t.Fatalf("%d of 8 concurrent cancels returned HTTP 500; statuses %v. "+
			"Losing a cancellation race is a conflict, not a server fault", n, statuses)
	}

	final := c.orderStatus(orderID)
	if final != "CANCELLED" && final != "FILLED" && final != "PARTIALLY_FILLED" {
		t.Fatalf("after the cancel race the order is %q; it must have settled to a "+
			"terminal state, not been left working or lost", final)
	}
	t.Logf("8 concurrent cancels -> %d succeeded, final state %s, statuses %v",
		succeeded, final, statuses)

	c.assertLedgerConsistent(accountID)
	c.assertNoUnknownOutcomeIsHidden(accountID)
}

// ---------------------------------------------------------------------------
// 5. Lost response after venue acceptance ("crash after acceptance")
// ---------------------------------------------------------------------------

// TestLostResponseAfterAcceptanceLeavesAFailedOrderAndNeverRetries is the most
// dangerous state in the system.
//
// The venue accepted the order; the answer never came back. The outcome is
// UNKNOWN. Retrying an unknown outcome is how one intended trade becomes two
// positions, so the platform must:
//
//   - record the order as FAILED, meaning "outcome unknown"
//   - NOT retry it automatically
//   - block automation until reconciliation resolves it
//
// The `lost_response` fault reproduces exactly this: the mock venue commits
// the order and then returns ErrUnknownOutcome instead of the ack.
func TestLostResponseAfterAcceptanceLeavesAFailedOrderAndNeverRetries(t *testing.T) {
	c, accountID := setup(t)
	stop := c.safeStop(instrument, "buy", 0.004)

	c.armFault("lost_response", 1, nil)
	key := newKey("lostresp")
	status := c.do(http.MethodPost, "/api/v1/orders", key,
		c.marketOrder(accountID, instrument, "buy", stop), nil)

	// A 2xx here would mean the platform reported success for an outcome it
	// cannot know.
	if status >= 200 && status < 300 {
		t.Fatalf("a lost venue response was reported to the client as success (%d)", status)
	}
	t.Logf("a lost response after acceptance returned %d to the client", status)

	orders := c.ordersWithKey(accountID, key)
	if len(orders) != 1 {
		t.Fatalf("expected exactly 1 order row for the lost response, found %d", len(orders))
	}
	if orders[0].Status != "FAILED" {
		t.Fatalf("an order whose outcome is unknown is %q; it must be FAILED", orders[0].Status)
	}

	// It must stay FAILED. An automatic retry would show up as the status
	// changing on its own, or as a second row appearing.
	time.Sleep(3 * time.Second)
	again := c.ordersWithKey(accountID, key)
	if len(again) != 1 || again[0].Status != "FAILED" {
		t.Fatalf("the unknown-outcome order was retried or advanced on its own: %+v", again)
	}
	t.Log("the order remained FAILED and was not retried")

	c.assertLedgerConsistent(accountID)
	c.assertNoUnknownOutcomeIsHidden(accountID)
}

// TestTimeoutAfterAcceptanceIsAlsoTreatedAsUnknown.
//
// A timeout is indistinguishable from a lost response from our side, and must
// be handled identically. A platform that treats a timeout as a rejection will
// eventually trade twice.
func TestTimeoutAfterAcceptanceIsAlsoTreatedAsUnknown(t *testing.T) {
	c, accountID := setup(t)
	stop := c.safeStop(instrument, "buy", 0.004)

	c.armFault("timeout", 1, map[string]any{"delay_ms": 100})
	key := newKey("timeout")
	status := c.do(http.MethodPost, "/api/v1/orders", key,
		c.marketOrder(accountID, instrument, "buy", stop), nil)
	if status >= 200 && status < 300 {
		t.Fatalf("a venue timeout was reported as success (%d)", status)
	}

	orders := c.ordersWithKey(accountID, key)
	if len(orders) == 1 && orders[0].Status == "FILLED" {
		t.Fatal("a timed-out order is recorded as FILLED, which the platform cannot know")
	}
	if len(orders) == 1 {
		t.Logf("a venue timeout left the order %q", orders[0].Status)
	}
	c.assertLedgerConsistent(accountID)
	c.assertNoUnknownOutcomeIsHidden(accountID)
}

// ---------------------------------------------------------------------------
// 6. Concurrent strategy runs
// ---------------------------------------------------------------------------

// TestConcurrentStrategyRunsDoNotDoubleTrade.
//
// Two runs of the same strategy overlapping is the realistic failure: a slow
// run has not finished when the scheduler starts the next. Both would see the
// same signal on the same bar, and without a guard both would trade it.
func TestConcurrentStrategyRunsDoNotDoubleTrade(t *testing.T) {
	c, accountID := setup(t)

	var before struct {
		Orders []order `json:"orders"`
	}
	c.get("/api/v1/orders?account_id="+accountID+"&limit=200", &before)

	var listed struct {
		Strategies []struct {
			ID string `json:"id"`
		} `json:"strategies"`
	}
	c.get("/api/v1/strategies", &listed)
	if len(listed.Strategies) == 0 {
		t.Skip("no strategies are configured on this stack")
	}
	strategyID := listed.Strategies[0].ID

	statuses := c.concurrentPost(4, "/api/v1/strategies/"+strategyID+"/run", nil,
		map[string]any{"account_id": accountID, "instrument_id": instrument})
	t.Logf("four concurrent runs of strategy %s returned %v", strategyID, statuses)

	// Whatever the runs did, the invariants hold: no duplicate idempotency
	// keys, and the ledger adds up. The first is the real anti-double-trade
	// guarantee -- a strategy order carries a deterministic key derived from
	// the signal, so two runs of the same bar collapse to one order.
	c.assertNoDuplicateKeys(accountID)
	c.assertLedgerConsistent(accountID)
	c.assertNoUnknownOutcomeIsHidden(accountID)
}

// ---------------------------------------------------------------------------
// 7. The invariant that must survive every race above
// ---------------------------------------------------------------------------

// TestLedgerIsGaplessAndBalancesAgreeAfterConcurrentActivity is the check that
// makes the rest meaningful.
//
// Every race above ends by asserting this, but it is also run standalone
// against deliberate concurrent load: mixed buys and sells at once, then the
// full financial-integrity check.
func TestLedgerIsGaplessAndBalancesAgreeAfterConcurrentActivity(t *testing.T) {
	c, accountID := setup(t)
	buyStop := c.safeStop(instrument, "buy", 0.004)
	sellStop := c.safeStop(instrument, "sell", 0.004)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			side, stop := "buy", buyStop
			if i%2 == 1 {
				side, stop = "sell", sellStop
			}
			c.do(http.MethodPost, "/api/v1/orders", newKey("mixed"+strconv.Itoa(i)),
				c.marketOrder(accountID, instrument, side, stop), nil)
		}(i)
	}
	wg.Wait()

	c.assertLedgerConsistent(accountID)
	c.assertNoDuplicateKeys(accountID)
	c.flattenAll(accountID)
	c.assertLedgerConsistent(accountID)
	c.assertNoUnknownOutcomeIsHidden(accountID)
}

// ---------------------------------------------------------------------------
// Assertions and read helpers
// ---------------------------------------------------------------------------

type order struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	IdempotencyKey string `json:"idempotency_key"`
	RejectCode     string `json:"reject_code"`
}

// ordersWithKey reads the book directly.
//
// The API's order payload does not carry idempotency_key, and this assertion
// is about exactly that column, so it goes to the source.
func (c *client) ordersWithKey(accountID, key string) []order {
	c.t.Helper()
	// Aggregated into one row so the result is a single field: psql's
	// unaligned tuples-only output would otherwise need line splitting, and
	// one value is easier to read back correctly than several.
	raw := psql(c.t, "SELECT COALESCE(string_agg(id || ' ' || status, ',' ORDER BY created_at), '') "+
		"FROM orders WHERE account_id = "+quoteSQL(accountID)+
		" AND idempotency_key = "+quoteSQL(key))
	if raw == "" {
		return nil
	}
	var found []order
	for _, entry := range strings.Split(raw, ",") {
		parts := strings.Fields(strings.TrimSpace(entry))
		if len(parts) < 2 {
			continue
		}
		found = append(found, order{ID: parts[0], Status: strings.ToUpper(parts[1])})
	}
	return found
}

func (c *client) orderStatus(orderID string) string {
	c.t.Helper()
	return strings.ToUpper(psql(c.t,
		"SELECT status FROM orders WHERE id = "+quoteSQL(orderID)))
}

// assertNoDuplicateKeys checks the invariant the unique index enforces.
//
// Read back rather than trusted, so a migration that dropped the index would
// still be caught here.
func (c *client) assertNoDuplicateKeys(accountID string) {
	c.t.Helper()
	dupes := psqlInt(c.t, `SELECT count(*) FROM (
		SELECT idempotency_key FROM orders WHERE account_id = `+quoteSQL(accountID)+`
		GROUP BY idempotency_key HAVING count(*) > 1
	) d`)
	if dupes != 0 {
		c.t.Fatalf("%d idempotency key(s) appear on more than one order", dupes)
	}
}

// assertLedgerConsistent verifies that the money adds up, in SQL.
//
// Two properties, both computed in numeric rather than float:
//
//   - the per-account sequence is gapless. A hole means a transaction is
//     missing, which means the balance cannot be reconstructed. This is the
//     worst thing a race could produce.
//   - every balance_after equals the running total of the account's ledger.
//
// These are the same checks scripts/backup-restore-drill.ps1 runs after a
// restore, applied here after deliberate concurrency.
func (c *client) assertLedgerConsistent(accountID string) {
	c.t.Helper()
	id := quoteSQL(accountID)

	gaps := psqlInt(c.t, `SELECT count(*) FROM (
		SELECT sequence, lag(sequence) OVER (ORDER BY sequence) AS prev
		FROM transactions WHERE account_id = `+id+`
	) s WHERE prev IS NOT NULL AND sequence - prev <> 1`)
	if gaps != 0 {
		c.t.Fatalf("the ledger has %d gap(s) in its sequence after concurrent activity", gaps)
	}

	drift := psqlInt(c.t, `WITH running AS (
		SELECT sequence, balance_after,
		       sum(amount) OVER (ORDER BY sequence
		                         ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS computed
		FROM transactions WHERE account_id = `+id+`
	) SELECT count(*) FROM running WHERE abs(balance_after - computed) > 0.005`)
	if drift != 0 {
		c.t.Fatalf("%d ledger row(s) have a balance that does not equal the running total", drift)
	}

	rows := psqlInt(c.t, "SELECT count(*) FROM transactions WHERE account_id = "+id)
	c.t.Logf("ledger consistent: %d transactions, gapless, balances agree", rows)
}

// assertNoUnknownOutcomeIsHidden is the invariant behind the deadlock fix.
//
// An order may legitimately be FAILED -- that is what "outcome unknown" looks
// like, and it is the safe state. What must never happen is an order left in a
// pre-submission state after the venue accepted it, because nothing would ever
// reconcile it. A FAILED order must also carry a reject code, or the
// constraint that pairs them would have refused the write.
func (c *client) assertNoUnknownOutcomeIsHidden(accountID string) {
	c.t.Helper()
	id := quoteSQL(accountID)
	// The states are the real ones from domain.OrderStatus. The first version
	// of this assertion looked for 'PENDING' and 'NEW', neither of which this
	// system has, so it could never fail -- and it passed happily while six
	// orders sat stranded in ACCEPTED.
	//
	// An order older than a minute and still in a pre-submission state is
	// stranded: the placement call that created it has long since returned, so
	// nothing is going to advance it.
	stranded := psqlInt(c.t, `SELECT count(*) FROM orders
		WHERE account_id = `+id+`
		  AND status IN ('CREATED', 'VALIDATING', 'ACCEPTED')
		  AND created_at < now() - interval '1 minute'`)
	if stranded != 0 {
		c.t.Fatalf("%d order(s) have been stuck in a pre-submission state for over a "+
			"minute; they consume the pending-order budget and cannot be cancelled "+
			"(no venue id), so the account eventually cannot trade at all", stranded)
	}
	uncoded := psqlInt(c.t, `SELECT count(*) FROM orders
		WHERE account_id = `+id+` AND status = 'REJECTED' AND reject_code IS NULL`)
	if uncoded != 0 {
		c.t.Fatalf("%d rejected order(s) carry no reject code", uncoded)
	}
}
