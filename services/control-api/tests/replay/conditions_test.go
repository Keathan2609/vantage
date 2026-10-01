package replay

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
	"testing"
	"time"
)

// Scenarios D, G, I and N: a tradable market meeting a CONDITION.
//
// These four were the remainder of the matrix, and they differ in kind from
// A-C, E, H and K-T. Those are market shapes a dataset can express. These are
// conditions applied to an ordinary market -- a high-impact release, two
// strategies disagreeing, an exhausted risk budget, published news -- where
// what is under test is what the platform does when a tradable signal meets
// one of them.
//
// # Why each has its own dataset
//
// The market is the control and the condition is the variable, so D, I and N
// carry the same trend shape as scenario A. They are still separate datasets
// because a strategy is evaluated once per completed bar permanently, and two
// scenarios sharing a bar range would leave the second nothing to evaluate.
//
// # Why the arming is SQL rather than a fault injection
//
// An economic release and a news item are DATA, not faults. The calendar and
// news providers write exactly these rows, and the replay step does not
// re-ingest them, so writing the row is what "the provider ingested it" means
// here. The behaviour under test is downstream of the row either way, and
// adding an injection endpoint would put test-only surface into the running
// process for no gain in fidelity.

// replayWindow reads the dataset's own time span from the running engine.
//
// From the engine rather than from the test, so arming cannot silently drift
// away from the fixture it is arming: a regenerated dataset moves these times
// and the conditions move with them.
func (h *harness) replayWindow() (from, to string) {
	h.t.Helper()
	var status struct {
		Run struct {
			FromTime string `json:"from_time"`
			ToTime   string `json:"to_time"`
		} `json:"run"`
	}
	h.do("GET", "/api/v1/replay", nil, &status)
	if status.Run.FromTime == "" || status.Run.ToTime == "" {
		h.t.Fatal("no active replay run to read a window from")
	}
	return status.Run.FromTime, status.Run.ToTime
}

// armHighImpactEvents schedules a USD release on every second hour of the run.
//
// Every SECOND hour, not every hour, and this is the whole design of the
// scenario. The blackout window is 15 minutes before and 10 after, and the
// dataset is hourly, so an event on the even hours puts roughly half the
// instants inside a window and half outside. Without the half that is outside,
// "nothing traded" would be indistinguishable from a platform that refuses
// everything, and the scenario would pass for the wrong reason.
//
// USD because economic_event_instrument_map maps USD to XAUUSD.m: an event in
// a currency the instrument is not mapped to is correctly invisible, and would
// arm nothing while looking armed.
func armHighImpactEvents(h *harness) {
	t := h.t
	t.Helper()
	from, to := h.replayWindow()

	// The clock at an instant is the bar's CLOSE, not its open, and the
	// blackout is measured against the clock. Scheduling on the hour therefore
	// lands inside the window of the instant that closes on that hour.
	psql(t, fmt.Sprintf(`
		INSERT INTO economic_events
			(external_id, scheduled_at, country, currency, impact, event_name,
			 category, status, source)
		SELECT 'replay-d-' || to_char(g, 'YYYYMMDDHH24'), g, 'US', 'USD', 'high',
		       'Replay scenario D high-impact release', 'employment', 'scheduled',
		       'replay-fixture'
		FROM generate_series(
			date_trunc('hour', timestamptz '%s'),
			timestamptz '%s',
			interval '2 hours') g
		ON CONFLICT (source, external_id) DO NOTHING`, from, to))

	armed := psqlInt(t, fmt.Sprintf(`SELECT count(*) FROM economic_events
		WHERE source = 'replay-fixture' AND impact = 'high'
		  AND scheduled_at >= timestamptz '%s' AND scheduled_at <= timestamptz '%s'`,
		from, to))
	if armed == 0 {
		t.Fatal("no economic event was armed, so this scenario would test nothing")
	}
	t.Logf("armed %d high-impact USD releases across the dataset window", armed)

	// Removed afterwards so a later suite is not silently run under a blackout
	// it did not ask for. Registered here, immediately after the insert, so a
	// failure below still cleans up.
	t.Cleanup(func() {
		psql(t, "DELETE FROM economic_events WHERE source = 'replay-fixture'")
	})
}

// armNews publishes news items across the run's window.
func armNews(h *harness) {
	t := h.t
	t.Helper()
	from, to := h.replayWindow()

	psql(t, fmt.Sprintf(`
		INSERT INTO news_items
			(external_id, source, headline, summary, published_at,
			 related_instruments, related_currencies, topics)
		SELECT 'replay-n-' || to_char(g, 'YYYYMMDDHH24'), 'replay-fixture',
		       'Replay scenario N development headline',
		       'A development fixture. Not a real story and not a real source.',
		       g, ARRAY['XAUUSD.m'], ARRAY['USD'], ARRAY['metals']
		FROM generate_series(
			date_trunc('hour', timestamptz '%s'),
			timestamptz '%s',
			interval '3 hours') g
		ON CONFLICT (source, external_id) DO NOTHING`, from, to))

	armed := psqlInt(t, fmt.Sprintf(`SELECT count(*) FROM news_items
		WHERE source = 'replay-fixture'
		  AND published_at >= timestamptz '%s' AND published_at <= timestamptz '%s'`,
		from, to))
	if armed == 0 {
		t.Fatal("no news item was armed, so this scenario would test nothing")
	}
	t.Logf("published %d news items across the dataset window", armed)

	t.Cleanup(func() {
		psql(t, "DELETE FROM news_items WHERE source = 'replay-fixture'")
	})
}

// armNoRiskCapacity tightens the account's exposure ceilings to nothing.
//
// # Why a limit change rather than a spent budget
//
// Trading the account down until it has no budget left would take a different
// market, would change the ledger, and would leave the next suite with a spent
// fixture. Tightening a ceiling is a real operator action through the real
// endpoint, it is reversible, and it isolates the property under test: a
// signal arrives with nowhere to put it.
//
// The restore is registered BEFORE the change, because a failure anywhere
// after this point would otherwise leave every later suite running against an
// account that cannot open a position -- refused correctly, for a reason that
// looks nothing like its cause.
func armNoRiskCapacity(h *harness) {
	t := h.t
	t.Helper()

	// The TRADER, not this suite's admin.
	//
	// `accountForRequest` scopes an account through ownership and an admin
	// owns no trading account, so an admin PUT to this route answers "Account
	// not found" -- the same trap that once made the reconciliation endpoints
	// unreachable by the only role allowed to use them. The limits belong to
	// the account's owner, so the owner changes them.
	trader := traderSession(t, h.base)
	accountID := psql(t, `SELECT id::text FROM accounts ORDER BY created_at LIMIT 1`)

	var before struct {
		Limits struct {
			MaxGrossExposure        string `json:"max_gross_exposure"`
			MaxNetExposure          string `json:"max_net_exposure"`
			MaxPerInstrumentExposre string `json:"max_per_instrument_exposure"`
			Version                 int64  `json:"version"`
		} `json:"limits"`
	}
	if code := trader.do("GET", "/api/v1/risk/limits/"+accountID, nil, &before); code != 200 {
		t.Fatalf("reading the risk limits returned %d", code)
	}
	if before.Limits.MaxGrossExposure == "" {
		t.Fatal("the risk limits response carried no exposure ceilings to restore")
	}

	// Registered BEFORE the change. A failure anywhere after this point would
	// otherwise leave every later suite running against an account that cannot
	// open a position -- refused correctly, for a reason that looks nothing
	// like its cause.
	t.Cleanup(func() {
		trader.t = t
		// Only the three fields this scenario changed. The request decoder
		// rejects unknown fields, so there is no "reason" key here -- and
		// every field left out now keeps its stored value, which is the whole
		// point of the pointer fields on the request.
		restore := map[string]any{
			"max_gross_exposure":          before.Limits.MaxGrossExposure,
			"max_net_exposure":            before.Limits.MaxNetExposure,
			"max_per_instrument_exposure": before.Limits.MaxPerInstrumentExposre,
		}
		if code := trader.do("PUT", "/api/v1/risk/limits/"+accountID, restore, nil); code != 200 {
			t.Errorf("RESTORING THE RISK LIMITS FAILED with %d. Every later suite "+
				"will now be refused for an exposure ceiling it did not set. "+
				"Restore them by hand before running anything else.", code)
		}
	})

	// Only the three fields change.
	//
	// That is safe only because the request's booleans and blackout minutes
	// are POINTERS: as plain values they decoded to false and zero, and this
	// very call silently turned off require_stop_loss and
	// block_on_high_impact_events and zeroed both blackout windows -- which
	// would have quietly destroyed scenario D's premise had D run after this.
	// Found by this scenario; fixed in handlers_control.go.
	//
	// Small but not zero. Zero would read as "not configured", and a positive
	// ceiling no order can satisfy tests the comparison rather than a special
	// case.
	tightened := map[string]any{
		"max_gross_exposure":          "0.01",
		"max_net_exposure":            "0.01",
		"max_per_instrument_exposure": "0.01",
	}
	if code := trader.do("PUT", "/api/v1/risk/limits/"+accountID, tightened, nil); code != 200 {
		t.Fatalf("tightening the risk limits returned %d", code)
	}
	t.Log("exposure ceilings tightened to 0.01: a signal now has nowhere to go")
}

// traderSession signs in as the seeded trader.
//
// A second session rather than a role change: the suite's own session is the
// admin that drives replay, and swapping its role mid-run would make every
// later assertion depend on which identity happened to be current.
func traderSession(t *testing.T, base string) *harness {
	t.Helper()
	password := os.Getenv("VANTAGE_E2E_PASSWORD")
	if password == "" {
		t.Skip("set VANTAGE_E2E_PASSWORD to the seeded trader's password: the " +
			"account's risk limits belong to its owner, and an admin owns no " +
			"trading account")
	}
	jar, _ := cookiejar.New(nil)
	trader := &harness{t: t, base: base, password: password,
		http: &http.Client{Jar: jar, Timeout: 2 * time.Minute}}

	var login struct {
		CSRFToken string `json:"csrf_token"`
	}
	if code := trader.do("POST", "/api/v1/auth/login", map[string]string{
		"email": "trader@vantage.local", "password": password,
	}, &login); code != http.StatusOK {
		t.Fatalf("trader sign-in returned %d", code)
	}
	trader.csrf = login.CSRFToken
	return trader
}

// conditionScenarios are the four that a condition rather than a market shape
// distinguishes.
func conditionScenarios() []scenario {
	return []scenario{
		{
			letter: "D", name: "high impact economic event",
			dataset: "event-window", steps: 180, arm: armHighImpactEvents,
			expect: func(t *testing.T, o observed) {
				runID := latestRunID(t)
				blacked := psqlInt(t, decisionsInRun(runID,
					`d.event_context->>'blackout' = 'true'`))
				clear := psqlInt(t, decisionsInRun(runID,
					`d.event_context->>'blackout' = 'false'`))

				// 1. The arming must have reached a decision that produced an
				//    ORDER. Otherwise every assertion below holds over an empty
				//    set.
				//
				// decisionsInRun scopes through orders, because a blackout can
				// only be shown to REFUSE something that was offered to the risk
				// engine. Since aggregation, a verdict that declines writes a
				// no-trade snapshot and places nothing, so a run can hold many
				// decisions and still offer the blackout nothing to act on. The
				// skip says which of those two happened rather than reporting
				// "no decision was taken", which would be false.
				if blacked == 0 {
					total := psqlInt(t, fmt.Sprintf(`
						SELECT count(*) FROM decision_snapshots
						WHERE created_at >= (SELECT started_at FROM replay_runs WHERE id = '%s')`,
						runID))
					t.Skipf("no order-bearing decision was taken inside a blackout "+
						"window, so the release was never in front of the risk "+
						"engine: %d decisions clear of one, %d decisions in the run "+
						"overall. What the verdicts recorded:\n%s\n%s",
						clear, total, verdictReasons(t, runID), o.describe())
				}

				// 2. THE INVARIANT, and it has an exemption that is NOT a
				//    loophole.
				//
				// A release inside its window must stop an OPENING order. It
				// must not stop a REDUCING one: refusing a flatten during a
				// high-impact release traps the position in exactly the
				// conditions the blackout exists to avoid, and "a check that
				// limits exposure or loss must never refuse a reducing order"
				// is the rule three separate defects have already taught this
				// repository.
				//
				// Measured on this dataset: 8 decisions were accepted inside a
				// blackout, every one of them against an open position, and
				// the engine recorded event_risk as PASSED on each -- the
				// exemption firing, not the check failing to bind.
				//
				// So the assertion is made twice over. First: no decision
				// whose event_risk check FAILED may be accepted. A failing
				// check that does not refuse is decoration.
				failedAndTraded := psqlInt(t, decisionsInRun(runID,
					`d.outcome = 'accepted' AND EXISTS (
						SELECT 1 FROM jsonb_array_elements(d.risk_state->'checks') c
						WHERE c->>'Name' = 'event_risk' AND c->>'Passed' = 'false')`))
				if failedAndTraded > 0 {
					t.Errorf("%d decisions were ACCEPTED after the event_risk check "+
						"failed on them\n%s", failedAndTraded, o.describe())
				}

				// Second, and independently of the engine's own verdict:
				// anything accepted inside a blackout must have had a position
				// to REDUCE. With no open position there is nothing to reduce,
				// the exemption cannot apply, and an acceptance is a defect.
				nothingToReduce := psqlInt(t, decisionsInRun(runID,
					`d.event_context->>'blackout' = 'true' AND d.outcome = 'accepted'
					   AND coalesce((d.portfolio_context->>'open_positions')::int, 0) = 0`))
				if nothingToReduce > 0 {
					t.Errorf("%d decisions were ACCEPTED inside a high-impact "+
						"blackout with NO open position, so none of them can be a "+
						"reducing order. A release inside its window is a refusal, "+
						"not a discount.\n%s", nothingToReduce, o.describe())
				}

				// 3. And the check must have BOUND at least once, or the
				//    exemption swallowed everything and nothing was refused.
				named := psqlInt(t, decisionsInRun(runID,
					`d.event_context->>'blackout' = 'true'
					   AND d.outcome_code = 'event_risk_blackout'`))
				if named == 0 {
					t.Errorf("%d decisions were taken inside a blackout and none "+
						"recorded outcome_code 'event_risk_blackout': either the "+
						"blackout refused nothing at all, or the refusal is "+
						"unattributable\n%s", blacked, o.describe())
				}
				exempt := psqlInt(t, decisionsInRun(runID,
					`d.event_context->>'blackout' = 'true' AND d.outcome = 'accepted'`))
				t.Logf("scenario D: %d blacked-out decisions, %d refused naming the "+
					"release, %d accepted as reducing orders against an open position",
					blacked, named, exempt)

				// 4. The scenario is only evidence if the platform was capable of
				//    trading this market at all. Without a clear-window decision,
				//    "nothing traded" says nothing about the blackout.
				if clear == 0 {
					t.Errorf("every decision in this run was inside a blackout, so "+
						"the refusals are not attributable to the release: a "+
						"platform refusing everything would look identical\n%s",
						o.describe())
				}
				t.Logf("scenario D: %d decisions inside a blackout, %d clear of one",
					blacked, clear)
			},
		},
		{
			letter: "G", name: "conflicting strategy signals",
			dataset: "conflicting-signals", steps: 180,
			expect: func(t *testing.T, o observed) {
				runID := latestRunID(t)
				if o.Decisions == 0 {
					t.Skipf("no decision was taken, so no two strategies could "+
						"disagree\n%s", o.describe())
				}

				// Did the market actually split the strategy set? Two strategies
				// producing opposite ACTIONS on the same bar is the condition
				// this scenario exists to test, and a fixture cannot guarantee
				// it -- so it is measured, and its absence is a skip rather than
				// a pass.
				//
				// # Why this reads the consensus and not only the orders
				//
				// It used to count (bar, instrument) pairs among the decisions
				// that ORDERS point to. That was the only evidence a split left
				// behind while every strategy routed its own signal: two opposing
				// orders, two decisions. Aggregation deliberately stops producing
				// that evidence -- one verdict per instant, at most one order --
				// so a guard that reads only it can never fire again, and this
				// scenario would skip for ever while looking correct.
				//
				// The split now lives in the verdict's own contributions, which
				// record every opinion INCLUDING the ones the policy discarded.
				// Both sources are counted: a split is a split whether the
				// platform acted on it or not.
				conflicts := psqlInt(t, fmt.Sprintf(`
					SELECT (
						SELECT count(*) FROM (
							SELECT d.bar_time, d.instrument_id
							FROM decision_snapshots d,
							     LATERAL jsonb_array_elements(
							         coalesce(d.consensus->'contributions', '[]'::jsonb)) c
							WHERE d.created_at >= (SELECT started_at FROM replay_runs WHERE id = '%s')
							  AND d.bar_time IS NOT NULL
							  AND c->>'action' IN ('buy','sell')
							GROUP BY 1, 2
							HAVING count(DISTINCT c->>'action') > 1) a
					) + (
						SELECT count(*) FROM (
							SELECT d.bar_time, d.instrument_id
							FROM decision_snapshots d
							WHERE d.id IN (SELECT o.decision_id FROM orders o
							               WHERE o.replay_run_id = '%s')
							  AND d.bar_time IS NOT NULL
							  AND d.signal_action IN ('buy','sell')
							GROUP BY 1, 2
							HAVING count(DISTINCT d.signal_action) > 1) b
					)`, runID, runID))
				if conflicts == 0 {
					t.Skipf("no instant produced opposing signals from different "+
						"strategies, so this dataset did not split the strategy "+
						"set and the invariant below was never exercised\n%s",
						o.describe())
				}

				// THE INVARIANT, and it does not presuppose any particular
				// aggregation policy: whatever the platform decides, it must not
				// hold both sides of one instrument at one instant. Two opposing
				// orders filled together pay the spread twice to carry no net
				// position, which is indefensible under any policy.
				opposing := psqlInt(t, fmt.Sprintf(`
					SELECT count(*) FROM (
						SELECT d.bar_time, o.instrument_id
						FROM orders o
						JOIN decision_snapshots d ON d.id = o.decision_id
						WHERE o.replay_run_id = '%s'
						  AND o.status NOT IN ('REJECTED','CANCELLED','EXPIRED','FAILED')
						GROUP BY 1,2
						HAVING count(DISTINCT o.side) > 1) x`, runID))
				if opposing > 0 {
					t.Errorf("%d (instant, instrument) pairs carry orders on BOTH "+
						"sides that were not rejected, across %d instants where "+
						"strategies disagreed.\n\n"+
						"orchestrator.Decide IS wired in now: the scheduler groups "+
						"by instrument and calls EvaluateInstrument, which "+
						"aggregates every strategy's opinion into ONE verdict and "+
						"places at most one order. Two opposing orders at one "+
						"instant therefore means something bypasses it -- check "+
						"that nothing calls EvaluateAndRoute with Execute:true "+
						"outside the operator endpoint. internal/arch guards "+
						"exactly that.\n%s",
						opposing, conflicts, o.describe())
				}

				// How the platform ACTED on the disagreement, measured rather
				// than assumed. Zero orders satisfies the invariant above and is
				// NOT by itself evidence that the platform can still trade, so
				// the verdicts' own reasons are printed when that is the case.
				orders := psqlInt(t, fmt.Sprintf(
					`SELECT count(*) FROM orders WHERE replay_run_id = '%s'`, runID))
				t.Logf("scenario G: %d instants split the strategy set, %d produced "+
					"orders on both sides, %d orders in total", conflicts, opposing, orders)
				if orders == 0 {
					t.Logf("scenario G: the invariant holds with NO orders placed, "+
						"which is a weaker demonstration than one with orders. "+
						"What the verdicts themselves recorded:\n%s",
						verdictReasons(t, runID))
				}
			},
		},
		{
			letter: "I", name: "strong signal with no risk capacity",
			dataset: "capacity-exhausted", steps: 180, arm: armNoRiskCapacity,
			expect: func(t *testing.T, o observed) {
				// 1. The signal must have reached the risk engine. If nothing was
				//    decided, the scenario measured a market that produced no
				//    opinion rather than a refusal.
				runID := latestRunID(t)
				if o.Decisions == 0 {
					t.Skipf("no decision was taken, so no signal met the tightened "+
						"ceilings\n%s", o.describe())
				}

				// An exposure ceiling can only refuse an order that was SIZED and
				// offered to the risk engine. A verdict that declines before the
				// OMS never reaches one, so with no order in this run the
				// tightening was not exercised and anything asserted below would
				// be measuring the consensus rather than the ceiling.
				//
				// This guard is new for the same reason scenario G's is: the
				// evidence it reads -- a refusal code on an order -- stops being
				// produced once aggregation decides before the pipeline. Without
				// it the scenario FAILS with "no order was refused for an exposure
				// ceiling", which is true and is not this scenario's finding.
				placed := psqlInt(t, fmt.Sprintf(
					`SELECT count(*) FROM orders WHERE replay_run_id = '%s'`, runID))
				if placed == 0 {
					t.Skipf("no order reached the risk engine in this run, so the "+
						"tightened ceilings refused nothing and this scenario "+
						"measured the consensus rather than the ceiling. What the "+
						"verdicts recorded:\n%s\n%s",
						verdictReasons(t, runID), o.describe())
				}

				// 2. THE INVARIANT, stated as what "no capacity" actually
				//    guarantees: exposure must not GROW.
				//
				// Not "nothing trades". A reducing order is exempt from every
				// exposure ceiling by design, and must be -- refusing a flatten
				// because the ceiling is tight traps the position, and that is
				// the third place that rule has had to be written down here.
				//
				// Measured on this dataset: 2 orders were accepted with
				// max_gross_exposure at 0.01 ZAR against an observed 1660.88
				// ZAR, and both were BUYs taken while net exposure was -553.58
				// -- opposite side, so reducing. The check recorded itself
				// PASSED, which is the exemption firing rather than the ceiling
				// failing to bind.
				//
				// So the assertion is that every accepted order was on the
				// opposite side to the net exposure it saw. Anything else added
				// to exposure the account had no room for.
				// split_part, because net_exposure is stored as MONEY -- "0.00
				// ZAR", not a bare number -- exactly as this repository insists
				// money be carried. Casting the whole string to numeric fails
				// outright, which is how the first version of this query was
				// caught: it errored rather than answering wrongly, which is the
				// better of the two failures.
				grew := psqlInt(t, decisionsInRun(runID, `
					d.outcome = 'accepted' AND EXISTS (
						SELECT 1 FROM orders o2 WHERE o2.decision_id = d.id AND (
							(o2.side = 'buy' AND coalesce(nullif(
								split_part(d.portfolio_context->>'net_exposure', ' ', 1),
								'')::numeric, 0) >= 0) OR
							(o2.side = 'sell' AND coalesce(nullif(
								split_part(d.portfolio_context->>'net_exposure', ' ', 1),
								'')::numeric, 0) <= 0)))`))
				if grew > 0 {
					t.Errorf("%d orders were ACCEPTED with the exposure ceilings at "+
						"0.01 and were NOT reducing -- each added to exposure the "+
						"account had no room for. A strong signal is not "+
						"capacity.\n%s", grew, o.describe())
				}

				// 3. And the refusal must be the exposure family, not some
				//    unrelated check that happened to fire first. Otherwise this
				//    passes whenever anything at all goes wrong.
				exposure := o.RejectCodes["exposure_limit_breached"] +
					o.RejectCodes["gross_exposure_limit"] +
					o.RejectCodes["net_exposure_limit"] +
					o.RejectCodes["instrument_exposure_limit"] +
					o.RejectCodes["concentration_limit"]
				if exposure == 0 {
					t.Errorf("no order was refused for an exposure ceiling, so the "+
						"tightening is not what stopped this run. Refusal codes "+
						"seen: %v\n%s", o.RejectCodes, o.describe())
				}
				accepted := psqlInt(t, decisionsInRun(runID, `d.outcome = 'accepted'`))
				t.Logf("scenario I: %d decisions, %d exposure refusals, %d accepted "+
					"-- and every accepted one was reducing, or the check above "+
					"would have failed", o.Decisions, exposure, accepted)
			},
		},
		{
			letter: "N", name: "news alongside strategy agreement",
			dataset: "news-agreement", steps: 180, arm: armNews,
			expect: func(t *testing.T, o observed) {
				if o.Decisions == 0 {
					t.Skipf("no decision was taken, so agreement was never "+
						"reached\n%s", o.describe())
				}

				// The news must be present and dated inside the run, or the
				// arming missed the window entirely.
				visible := psqlInt(t, `SELECT count(*) FROM news_items
					WHERE source = 'replay-fixture'`)
				if visible == 0 {
					t.Fatal("the armed news vanished before the assertions ran")
				}

				// THE INVARIANT that can honestly be asserted here.
				//
				// News is ingested, stored and exposed, but it is NOT an input to
				// a decision: nothing in the orchestrator, the OMS or the risk
				// engine reads news_items. So this scenario cannot assert that
				// news changed an outcome, and asserting it did would be false.
				// What it CAN assert is that publishing news did not corrupt the
				// decision path -- the run stayed internally consistent and
				// nothing traded that the record cannot explain.
				//
				// The gap itself is a finding, recorded rather than papered over.
				unexplained := psqlInt(t, fmt.Sprintf(`
					SELECT count(*) FROM decision_snapshots d
					WHERE d.id IN (SELECT o.decision_id FROM orders o
					               WHERE o.replay_run_id = '%s')
					  AND d.outcome <> 'accepted'
					  AND coalesce(d.outcome_code, '') = ''
					  AND coalesce(d.outcome_reason, '') = ''`, latestRunID(t)))
				if unexplained > 0 {
					t.Errorf("%d decisions were refused with neither a code nor a "+
						"reason: an operator cannot learn why\n%s",
						unexplained, o.describe())
				}
				t.Logf("scenario N: %d decisions with %d news items published "+
					"across the window. NOTE: news is stored and exposed but is "+
					"not read by the orchestrator, the OMS or the risk engine, so "+
					"this asserts consistency, not that news altered a decision.",
					o.Decisions, visible)
			},
		},
	}
}

// latestRunID is the replay run this scenario just drove.
func latestRunID(t *testing.T) string {
	t.Helper()
	return psql(t, "SELECT id::text FROM replay_runs ORDER BY started_at DESC LIMIT 1")
}

// decisionsInRun counts decisions of one run matching a predicate.
//
// Scoped to the run's own orders so a count cannot pick up a previous
// scenario's decisions, which share the account.
func decisionsInRun(runID, predicate string) string {
	return fmt.Sprintf(`
		SELECT count(*) FROM decision_snapshots d
		WHERE d.id IN (SELECT o.decision_id FROM orders o
		               WHERE o.replay_run_id = '%s')
		  AND %s`, runID, predicate)
}

// verdictReasons summarises why each consensus verdict declined to trade.
//
// The point of recording the verdict on the decision snapshot is that "why did
// it not trade?" has an answer in stored evidence rather than in a log that may
// have rotated. This reads that evidence back, so a run which placed nothing
// says WHY instead of leaving a reader to guess between "the policy refused"
// and "the pipeline never ran" -- the two that look identical from outside, and
// the confusion that made four earlier suites pass while proving nothing.
func verdictReasons(t *testing.T, runID string) string {
	t.Helper()
	reasons := psql(t, fmt.Sprintf(`
		SELECT coalesce(string_agg(line, chr(10)), '') FROM (
			SELECT '          ' || count(*) || ' x ' || left(outcome_reason, 96) AS line
			FROM decision_snapshots
			WHERE created_at >= (SELECT started_at FROM replay_runs WHERE id = '%s')
			  AND outcome = 'no_trade'
			GROUP BY left(outcome_reason, 96)
			ORDER BY count(*) DESC
			LIMIT 6) r`, runID))

	// Every ACTIONABLE opinion the policy discarded, and the reason it gave.
	// This is the half an operator actually needs: a verdict listing only its
	// survivors cannot be argued with.
	discards := psql(t, fmt.Sprintf(`
		SELECT coalesce(string_agg(line, chr(10)), '') FROM (
			SELECT '          ' || count(*) || ' x ' || strategy || ' ' || action ||
			       ' at ' || confidence || ' -- ' || note AS line
			FROM (
				SELECT c->>'strategy' AS strategy, c->>'action' AS action,
				       c->>'confidence' AS confidence, left(c->>'note', 72) AS note
				FROM decision_snapshots d,
				     LATERAL jsonb_array_elements(
				         coalesce(d.consensus->'contributions', '[]'::jsonb)) c
				WHERE d.created_at >= (SELECT started_at FROM replay_runs WHERE id = '%s')
				  AND c->>'action' IN ('buy','sell')
				  AND c->>'counted' = 'false') x
			GROUP BY strategy, action, confidence, note
			ORDER BY count(*) DESC
			LIMIT 8) r`, runID))

	out := "        verdicts:\n" + reasons
	if discards != "" {
		out += "\n        actionable opinions the policy discarded:\n" + discards
	}
	return out
}
