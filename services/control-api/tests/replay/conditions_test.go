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
		restore := map[string]any{
			"max_gross_exposure":          before.Limits.MaxGrossExposure,
			"max_net_exposure":            before.Limits.MaxNetExposure,
			"max_per_instrument_exposure": before.Limits.MaxPerInstrumentExposre,
			"reason":                      "restoring the risk limits scenario I tightened",
		}
		if code := trader.do("PUT", "/api/v1/risk/limits/"+accountID, restore, nil); code != 200 {
			t.Errorf("RESTORING THE RISK LIMITS FAILED with %d. Every later suite "+
				"will now be refused for an exposure ceiling it did not set. "+
				"Restore them by hand before running anything else.", code)
		}
	})

	// Only the three fields change: the handler falls back to the stored value
	// for every field left empty, so a partial update cannot silently reset a
	// limit this scenario never meant to touch.
	//
	// Small but not zero. Zero would read as "not configured", and a positive
	// ceiling no order can satisfy tests the comparison rather than a special
	// case.
	tightened := map[string]any{
		"max_gross_exposure":          "0.01",
		"max_net_exposure":            "0.01",
		"max_per_instrument_exposure": "0.01",
		"reason":                      "scenario I: removing the account's capacity to take risk",
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
			dataset: "event-window", steps: 120, arm: armHighImpactEvents,
			expect: func(t *testing.T, o observed) {
				runID := latestRunID(t)
				blacked := psqlInt(t, decisionsInRun(runID,
					`d.event_context->>'blackout' = 'true'`))
				clear := psqlInt(t, decisionsInRun(runID,
					`d.event_context->>'blackout' = 'false'`))

				// 1. The arming must have reached a decision. Otherwise every
				//    assertion below holds over an empty set.
				if blacked == 0 {
					t.Skipf("no decision was taken inside a blackout window, so the "+
						"release was never in front of the platform; %d decisions "+
						"were taken clear of one\n%s", clear, o.describe())
				}

				// 2. THE INVARIANT. A high-impact release inside its window must
				//    stop an automated order. Not reduce it -- stop it.
				traded := psqlInt(t, decisionsInRun(runID,
					`d.event_context->>'blackout' = 'true' AND d.outcome = 'accepted'`))
				if traded > 0 {
					t.Errorf("%d of %d decisions taken inside a high-impact blackout "+
						"were ACCEPTED. A release inside its window is a refusal, "+
						"not a discount.\n%s", traded, blacked, o.describe())
				}

				// 3. And the refusal must name the blackout, so an operator
				//    reading the record learns why rather than only that.
				named := psqlInt(t, decisionsInRun(runID,
					`d.event_context->>'blackout' = 'true'
					   AND d.outcome_code = 'event_risk_blackout'`))
				if named == 0 {
					t.Errorf("%d decisions were taken inside a blackout and none "+
						"recorded outcome_code 'event_risk_blackout': the refusal "+
						"happened for some other reason, or is unattributable\n%s",
						blacked, o.describe())
				}

				// 4. The scenario is only evidence if the platform was capable of
				//    trading this market at all. Without a clear-window decision,
				//    "nothing traded" says nothing about the blackout.
				if clear == 0 {
					t.Errorf("every decision in this run was inside a blackout, so "+
						"the refusals are not attributable to the release: a "+
						"platform refusing everything would look identical\n%s",
						o.describe())
				}
				t.Logf("scenario D: %d decisions inside a blackout (%d named it), "+
					"%d clear of one", blacked, named, clear)
			},
		},
		{
			letter: "G", name: "conflicting strategy signals",
			dataset: "conflicting-signals", steps: 120,
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
				conflicts := psqlInt(t, fmt.Sprintf(`
					SELECT count(*) FROM (
						SELECT d.bar_time, d.instrument_id
						FROM decision_snapshots d
						WHERE d.id IN (SELECT o.decision_id FROM orders o
						               WHERE o.replay_run_id = '%s')
						  AND d.signal_action IN ('buy','sell')
						GROUP BY 1,2
						HAVING count(DISTINCT d.signal_action) > 1) c`, runID))
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
						"strategies disagreed. Disagreement is not a reason to "+
						"trade both ways.\n%s", opposing, conflicts, o.describe())
				}
				t.Logf("scenario G: %d instants split the strategy set, %d produced "+
					"orders on both sides", conflicts, opposing)
			},
		},
		{
			letter: "I", name: "strong signal with no risk capacity",
			dataset: "capacity-exhausted", steps: 120, arm: armNoRiskCapacity,
			expect: func(t *testing.T, o observed) {
				// 1. The signal must have reached the risk engine. If nothing was
				//    decided, the scenario measured a market that produced no
				//    opinion rather than a refusal.
				if o.Decisions == 0 {
					t.Skipf("no decision was taken, so no signal met the tightened "+
						"ceilings\n%s", o.describe())
				}

				// 2. THE INVARIANT. No capacity means no position, however good
				//    the signal looked.
				if o.Fills > 0 {
					t.Errorf("%d fills occurred with the exposure ceilings at 0.01. "+
						"A strong signal is not capacity.\n%s", o.Fills, o.describe())
				}
				if o.Accepted > 0 {
					t.Errorf("%d decisions were ACCEPTED with no exposure capacity\n%s",
						o.Accepted, o.describe())
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
				t.Logf("scenario I: %d decisions, %d exposure refusals, %d fills",
					o.Decisions, exposure, o.Fills)
			},
		},
		{
			letter: "N", name: "news alongside strategy agreement",
			dataset: "news-agreement", steps: 120, arm: armNews,
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
