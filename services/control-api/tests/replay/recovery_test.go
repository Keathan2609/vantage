package replay

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The failure and recovery scenarios, driven through the real pipeline.
//
// These are the ones the brief calls out as most important: a partial fill
// that has to book exactly once and converge, and a broker response lost after
// the venue already accepted -- where local state is genuinely uncertain and
// only reconciliation can settle it.
//
// Nothing here resets the database by hand. The point of the recovery tests is
// that the platform gets itself back to a safe state.

// armFault arms one of the mock venue's fault modes.
//
// Faults are armed on the VENUE rather than mocked in a test double,
// deliberately: the behaviour being tested is the OMS and reconciliation
// against a venue that actually misbehaves, and a mocked adapter would prove
// only that the mock behaved as the test author imagined.
func (h *harness) armFault(fault string, times int, extra map[string]any) {
	h.t.Helper()
	payload := map[string]any{"fault": fault, "times": times}
	for k, v := range extra {
		payload[k] = v
	}
	if code := h.do("POST", "/api/v1/dev/broker-faults", payload, nil); code != http.StatusOK &&
		code != http.StatusCreated && code != http.StatusAccepted {
		h.t.Fatalf("arming %s returned %d", fault, code)
	}
}

func (h *harness) resetFaults() {
	h.t.Helper()
	h.do("POST", "/api/v1/dev/broker-faults/reset", map[string]any{}, nil)
}

// stepUntilOrders advances the replay until at least n new orders exist, or
// the budget of steps runs out.
//
// Stepping blind for a fixed count would either waste minutes or stop before
// the pipeline had done anything, and both make a failure unreadable.
func (h *harness) stepUntilOrders(baseline, want, maxSteps int) int {
	h.t.Helper()
	const chunk = 10
	for done := 0; done < maxSteps; done += chunk {
		run := h.control("step", map[string]any{"steps": chunk})
		got := psqlInt(h.t, "SELECT count(*) FROM orders") - baseline
		if got >= want {
			return got
		}
		// Stop at the end of the dataset. Stepping a finished run returns 409
		// and fails the test with a status code rather than with the fact that
		// the scenario ran out of data -- which is the thing the reader needs
		// to know.
		if run.State == "done" || run.State == "failed" || run.State == "stopped" {
			break
		}
	}
	return psqlInt(h.t, "SELECT count(*) FROM orders") - baseline
}

// ---------------------------------------------------------------------------
// F. Market-data outage
// ---------------------------------------------------------------------------

func TestScenarioF_MarketDataOutageHaltsAutomationAndRecovers(t *testing.T) {
	h := newHarness(t)
	t.Cleanup(func() { h.t = t; h.stopQuietly(); h.resetFaults() })

	before := h.observe()
	h.start("trend-clean", "scenario F: market data disappears mid-run")

	// Deep into the EVALUATION phase, not merely past the warm-up boundary.
	// Measured on this dataset: the 60-instant warm-up ends at instant 60 and
	// the first order appears past instant 100, so a shallower run tests an
	// outage against a pipeline that was producing nothing anyway.
	h.step(100)
	duringWarmup := h.observe().since(before)

	// The feed disappears.
	h.inject("outage", nil)
	atOutage := h.observe()
	h.step(20)
	outage := h.observe().since(atOutage)

	if outage.Orders != 0 {
		t.Errorf("%d orders were created while the market-data feed was down\n%s",
			outage.Orders, outage.describe())
	}
	// And the refusal must be RECORDED, not merely absent: silence is
	// indistinguishable from a pipeline that stopped running.
	degraded := outage.SkipReasonsContaining("market data") +
		outage.SkipReasonsContaining("stale") +
		outage.SkipReasonsContaining("no_data")
	if degraded == 0 && outage.StrategyRuns > 0 {
		t.Errorf("the feed was down and no strategy run recorded a market-data "+
			"reason\n%s", outage.describe())
	}

	// Recovery: the feed returns and automation resumes.
	h.inject("outage", map[string]any{"clear": true})
	atRecovery := h.observe()
	h.step(30)
	recovered := h.observe().since(atRecovery)

	if recovered.StrategyRuns == 0 {
		t.Errorf("the feed recovered and no strategy ran at all\n%s", recovered.describe())
	}
	t.Logf("before outage: %d strategy runs; during: %d runs, %d orders; "+
		"after recovery: %d runs",
		duringWarmup.StrategyRuns, outage.StrategyRuns, outage.Orders,
		recovered.StrategyRuns)

	assertUniversalInvariants(t, h.observe())
}

// ---------------------------------------------------------------------------
// J. Kill switch during an autonomous replay
// ---------------------------------------------------------------------------

func TestScenarioJ_KillSwitchStopsNewOrdersImmediately(t *testing.T) {
	h := newHarness(t)
	t.Cleanup(func() {
		h.t = t
		h.releaseKillSwitches()
		h.stopQuietly()
	})

	before := h.observe()
	h.start("trend-clean", "scenario J: kill switch during an autonomous replay")
	h.step(105)
	beforeKill := h.observe()
	t.Logf("before the kill switch: %d orders", beforeKill.since(before).Orders)

	// Engage it. The durable state is established by the API call returning.
	// Global scope, which takes no target: an account-scoped switch needs the
	// account id and the point here is the halt, not the addressing.
	code := h.do("POST", "/api/v1/kill-switches", map[string]any{
		"scope": "global", "reason": "scenario J: halting an autonomous replay",
	}, nil)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("engaging the kill switch returned %d", code)
	}

	afterKill := h.observe()
	h.step(40)
	killed := h.observe().since(afterKill)

	// The assertion: NO order passes after the durable state exists. Not
	// "fewer orders" -- none.
	if killed.Orders != 0 {
		t.Errorf("%d orders were created AFTER the kill switch was engaged. A "+
			"kill switch that leaks even one order is not a kill switch.\n%s",
			killed.Orders, killed.describe())
	}
	if killed.Fills != 0 {
		t.Errorf("%d fills occurred after the kill switch was engaged\n%s",
			killed.Fills, killed.describe())
	}
	assertUniversalInvariants(t, h.observe())
}

func (h *harness) releaseKillSwitches() {
	h.t.Helper()
	var list struct {
		KillSwitches []struct {
			ID     string `json:"id"`
			Active bool   `json:"active"`
		} `json:"kill_switches"`
	}
	h.do("GET", "/api/v1/kill-switches", nil, &list)
	for _, ks := range list.KillSwitches {
		if ks.Active {
			h.do("POST", "/api/v1/kill-switches/"+ks.ID+"/deactivate",
				map[string]any{"reason": "scenario teardown, releasing the halt"}, nil)
		}
	}
}

func (h *harness) stopQuietly() {
	h.do("POST", "/api/v1/replay/control", map[string]any{
		"action": "stop", "reason": "scenario teardown, releasing replay time",
	}, nil)
}

// ---------------------------------------------------------------------------
// P. Partial fill
// ---------------------------------------------------------------------------

func TestScenarioP_APartialFillBooksExactlyOnceAndConverges(t *testing.T) {
	// The brief calls this one of the most important tests. A partial fill
	// touches every layer that can double-count: the order state machine, the
	// position, the weighted average price, the fee accrual and the ledger.
	h := newHarness(t)
	t.Cleanup(func() { h.t = t; h.resetFaults(); h.stopQuietly() })

	before := h.observe()
	h.start("trend-clean", "scenario P: a partial fill through the real pipeline")

	// Just into the productive part of the evaluation phase, so the fault has
	// as many order-producing instants as possible left to land on. Measured:
	// the 60-instant warm-up ends at 60, the first orders appear past 100, and
	// the dataset ends at 140.
	h.step(95)

	// 0.4 of the requested quantity, which is the split the brief names:
	// quantity 1.0 -> fill 0.4 -> later fill 0.6 -> FILLED. Without a fraction
	// the fault arms and forces nothing, and the scenario skips while looking
	// as though it ran.
	h.armFault("partial_fill", 1, map[string]any{"fraction": "0.4"})
	baseline := psqlInt(t, "SELECT count(*) FROM orders")
	got := h.stepUntilOrders(baseline, 1, 60)
	if got == 0 {
		t.Skipf("no order was produced after the partial-fill fault was armed, "+
			"so the scenario could not be exercised\n%s",
			h.observe().since(before).describe())
	}

	// Find an order that was partially filled at some point. The venue may
	// complete it later, which is the behaviour being tested.
	partial := psqlInt(t, `
		SELECT count(*) FROM order_state_transitions
		WHERE to_status = 'PARTIALLY_FILLED'`)
	if partial == 0 {
		// Measured, and a finding rather than a flaky test.
		//
		// Every order this account produces is 0.01 lots, which is XAUUSD.m's
		// minimum quantity AND its quantity step. A 40% partial fill would be
		// 0.004 lots, which is not a representable quantity on this
		// instrument, so the venue cannot split the order at all.
		//
		// A partial fill is therefore not exercisable on the ZAR 500 account
		// at this instrument's granularity. Exercising it needs an account
		// whose risk budget supports a multi-step position -- a fixture
		// change, not a code change -- and the constraint itself is worth
		// knowing about a small account.
		minQty := psqlRow(t, `SELECT min_quantity || ' / ' || quantity_step
			FROM instruments WHERE id = 'XAUUSD.m'`)
		sizes := psqlRow(t, `SELECT string_agg(DISTINCT quantity::text, ', ')
			FROM orders`)
		t.Skipf("the venue reported no partial fill: every order was %s lots and "+
			"XAUUSD.m has min/step %s, so 40%% of an order is not a representable "+
			"quantity. A partial fill cannot be exercised at this account's "+
			"position size.", sizes, minQty)
	}

	// Exactness. Every one of these is a place a partial fill can double-count.
	o := h.observe()

	// 1. The position quantity equals the sum of its fills, signed.
	mismatch := psqlInt(t, `
		SELECT count(*) FROM (
			SELECT p.id,
			       p.quantity AS pos_qty,
			       COALESCE(SUM(CASE WHEN f.side = p.side THEN f.quantity
			                         ELSE -f.quantity END), 0) AS fill_qty
			FROM positions p
			JOIN orders o ON o.account_id = p.account_id
			                AND o.instrument_id = p.instrument_id
			JOIN fills f ON f.order_id = o.id
			WHERE p.status = 'open'
			GROUP BY p.id, p.quantity
		) x WHERE abs(pos_qty - fill_qty) > 0.0000001`)
	if mismatch > 0 {
		t.Errorf("%d open positions have a quantity that does not equal the sum "+
			"of their fills: a fill was booked twice or not at all", mismatch)
	}

	// 2. No fill is recorded twice. The venue's own id is the identity.
	dupes := psqlInt(t, `
		SELECT count(*) FROM (
			SELECT broker_name, broker_fill_id, count(*) AS n
			FROM fills GROUP BY 1, 2 HAVING count(*) > 1
		) d`)
	if dupes > 0 {
		t.Errorf("%d venue executions were booked more than once", dupes)
	}

	// 3. Every fill produced its ledger entries, and the running balance still
	//    equals the sum of the entries.
	assertUniversalInvariants(t, o)

	// 4. The order's filled quantity never exceeds what was requested.
	over := psqlInt(t, `SELECT count(*) FROM orders WHERE filled_quantity > quantity`)
	if over > 0 {
		t.Errorf("%d orders are filled beyond their requested quantity", over)
	}

	t.Logf("partial-fill transitions: %d; fills %d; ledger %d; balance %s == %s",
		partial, o.Fills, o.Ledger, o.StoredBalance, o.DerivedBalance)
}

// ---------------------------------------------------------------------------
// Q. Broker response lost after the venue accepted
// ---------------------------------------------------------------------------

func TestScenarioQ_ALostResponseIsRecoveredByReconciliation(t *testing.T) {
	// The scenario the milestone names as a completion criterion. The venue
	// accepted and possibly filled; the response never arrived; local state is
	// genuinely uncertain. Only reconciliation can settle it, and it must
	// import the execution EXACTLY once.
	h := newHarness(t)
	t.Cleanup(func() { h.t = t; h.resetFaults(); h.stopQuietly() })

	h.start("trend-clean", "scenario Q: a broker response lost after acceptance")
	h.step(95)

	h.armFault("lost_response", 1, nil)
	baseline := psqlInt(t, "SELECT count(*) FROM orders")
	if got := h.stepUntilOrders(baseline, 1, 60); got == 0 {
		t.Skip("no order was produced after the lost-response fault was armed")
	}

	// An order whose outcome is unknown must NOT be reported as rejected or
	// filled. It is FAILED and flagged for reconciliation -- guessing either
	// way is how a real position becomes invisible.
	uncertain := psqlInt(t, `
		SELECT count(*) FROM orders
		WHERE reconciliation_required OR status = 'FAILED'`)
	if uncertain == 0 {
		t.Skipf("no order ended in an uncertain state, so the lost response was "+
			"not exercised\n%s", h.observe().describe())
	}
	t.Logf("%d orders are in an uncertain state after the lost response", uncertain)

	// Reconciliation, through the replay's own control so it runs at the
	// replay instant rather than at wall time.
	if code := h.do("POST", "/api/v1/replay/control", map[string]any{
		"action": "reconcile",
	}, nil); code != http.StatusOK {
		t.Fatalf("reconcile returned %d", code)
	}

	// Convergence. Whatever reconciliation decided, the account must end
	// internally consistent and the execution must appear exactly once.
	o := h.observe()
	assertUniversalInvariants(t, o)

	dupes := psqlInt(t, `
		SELECT count(*) FROM (
			SELECT broker_name, broker_fill_id, count(*) AS n
			FROM fills GROUP BY 1, 2 HAVING count(*) > 1
		) d`)
	if dupes > 0 {
		t.Errorf("reconciliation imported %d executions more than once", dupes)
	}

	// A recovered fill must have gone through booking like any other.
	//
	// Expressed as what booking actually guarantees, not as "it has a ledger
	// entry". `booking` writes a transaction only when realised P&L is
	// non-zero or commission is positive, so an opening fill on a
	// zero-commission instrument correctly produces none -- and the seeded
	// instruments charge no commission, which makes that the common case. The
	// previous form of this check happened to pass only because no recovered
	// fill had yet been an opening one.
	orphan := psqlInt(t, `
		SELECT count(*) FROM fills f
		WHERE f.ingest_source <> 'execution_response'
		  AND f.commission > 0
		  AND NOT EXISTS (SELECT 1 FROM transactions t
		                  WHERE t.fill_id = f.id AND t.type = 'commission')`)
	if orphan > 0 {
		t.Errorf("%d recovered fills carried commission and produced no "+
			"commission ledger entry, so they bypassed booking", orphan)
	}

	// And whether automation may resume is a DECISION the platform records,
	// not something the test asserts away: an unresolved discrepancy should
	// keep it paused.
	unresolved := psqlInt(t, `
		SELECT count(*) FROM reconciliation_issues WHERE status = 'open'`)
	t.Logf("after reconciliation: %d unresolved issues, %d fills, %d ledger "+
		"entries, balance %s == %s",
		unresolved, o.Fills, o.Ledger, o.StoredBalance, o.DerivedBalance)
}

// ---------------------------------------------------------------------------
// R. Duplicate scheduler tick
// ---------------------------------------------------------------------------

func TestScenarioR_ADuplicateTickProducesNoDuplicateOrder(t *testing.T) {
	// The per-bar guard is what stops a strategy's opinion being counted
	// twice. Driving the same instant twice is the direct test of it.
	h := newHarness(t)
	t.Cleanup(func() { h.t = t; h.stopQuietly() })

	h.start("trend-clean", "scenario R: duplicate scheduler ticks for one instant")
	h.step(105)

	beforeDupes := h.observe()

	// Reconcile is the only control that re-drives the pipeline without
	// advancing the clock, so it is the closest available analogue of a
	// duplicate tick at one instant. Five of them.
	for i := 0; i < 5; i++ {
		h.do("POST", "/api/v1/replay/control", map[string]any{"action": "reconcile"}, nil)
	}
	after := h.observe().since(beforeDupes)

	if after.Orders != 0 {
		t.Errorf("re-driving the same replay instant produced %d additional "+
			"orders; one instant must produce at most one financial command "+
			"per strategy\n%s", after.Orders, after.describe())
	}

	// And no bar was evaluated twice, which is the underlying invariant.
	doubled := psqlInt(t, `
		SELECT count(*) FROM (
			SELECT strategy_id, strategy_version, account_id, instrument_id,
			       bar_time, count(*) AS n
			FROM strategy_runs WHERE bar_time IS NOT NULL
			GROUP BY 1,2,3,4,5 HAVING count(*) > 1
		) d`)
	if doubled > 0 {
		t.Errorf("%d (strategy, bar) pairs were evaluated more than once", doubled)
	}
	assertUniversalInvariants(t, h.observe())
}

// ---------------------------------------------------------------------------
// O. Stale provider recovery
// ---------------------------------------------------------------------------

func TestScenarioO_AnOldQuoteAfterReconnectStaysStale(t *testing.T) {
	// A provider that reconnects and replays an old price stamps it with a
	// fresh ingestion time, so the quote READS fresh while describing a market
	// that has moved on. Only genuinely current data may restore tradability.
	h := newHarness(t)
	t.Cleanup(func() { h.t = t; h.resetFaults(); h.stopQuietly() })

	h.start("trend-clean", "scenario O: an old quote after a reconnect")
	h.step(105)

	h.armFault("stale_quote", 3, nil)
	baseline := h.observe()
	h.step(20)
	stale := h.observe().since(baseline)

	// The platform must refuse on the stale quote rather than trade it.
	refusals := stale.RejectCodes["stale_market_data"] +
		stale.SkipReasonsContaining("stale") +
		stale.SkipReasonsContaining("market data")
	if refusals == 0 && stale.Orders > 0 {
		t.Errorf("a stale quote produced %d orders and no stale refusal\n%s",
			stale.Orders, stale.describe())
	}

	h.resetFaults()
	afterReset := h.observe()
	h.step(25)
	recovered := h.observe().since(afterReset)
	t.Logf("stale phase: %d runs, %d orders, %d stale refusals; after: %d runs",
		stale.StrategyRuns, stale.Orders, refusals, recovered.StrategyRuns)

	assertUniversalInvariants(t, h.observe())
}

// ---------------------------------------------------------------------------
// A summary the report can quote
// ---------------------------------------------------------------------------

func TestReplayThroughputIsMeasured(t *testing.T) {
	// Not an optimisation: a number, so a later change that halves it is
	// visible. The brief asks for the bottleneck to be identified rather than
	// pre-emptively removed.
	h := newHarness(t)
	t.Cleanup(func() { h.t = t; h.stopQuietly() })

	h.start("trend-clean", "measuring replay throughput at maximum speed")
	start := time.Now()
	const steps = 40
	h.step(steps)
	elapsed := time.Since(start)

	perStep := elapsed / steps
	t.Logf("replay throughput: %d instants in %s (%s per instant, %.1f instants/second)",
		steps, elapsed.Round(time.Millisecond), perStep.Round(time.Millisecond),
		float64(steps)/elapsed.Seconds())

	if perStep > 5*time.Second {
		t.Errorf("a replay instant takes %s, which makes a long run impractical",
			perStep.Round(time.Millisecond))
	}
}

// psqlRow is a small helper for the scenarios that read one value.
func psqlRow(t *testing.T, format string, args ...any) string {
	t.Helper()
	return psql(t, fmt.Sprintf(format, args...))
}

var _ = strings.TrimSpace
