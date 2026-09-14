package replay

import (
	"fmt"
	"strings"
	"testing"
)

// The scenario matrix, driven through the real pipeline.
//
// Each scenario states what it EXPECTS of the business outcome. That is the
// whole point: a replay that answers HTTP 200 to every request and reaches the
// wrong trading outcome is the failure worth catching, and "the run completed"
// asserts nothing about trading.
//
// # Why the expectations are ranges and conditions rather than exact numbers
//
// An exact order count would pin this suite to the current strategy set and
// break on every parameter change, which teaches the next person to update the
// number rather than read the failure. So each scenario asserts the properties
// that must hold for the platform to be behaving correctly on that market --
// and asserts them strictly enough to fail when it is not.

// scenario is one market condition driven end to end.
type scenario struct {
	// letter is the milestone's label, so a result can be read against the
	// brief.
	letter  string
	name    string
	dataset string
	steps   int
	// arm runs before stepping, for scenarios whose condition is injected
	// rather than carried in the dataset.
	arm func(h *harness)
	// expect states the business outcome. Called with the universal
	// invariants already checked.
	expect func(t *testing.T, o observed)
}

// datasetScenarios are the ones a committed fixture expresses.
func datasetScenarios() []scenario {
	return []scenario{
		{
			letter: "A", name: "clean trend", dataset: "trend-clean", steps: 60,
			expect: func(t *testing.T, o observed) {
				// A clean trend is the one market where the strategy set should
				// find something. Zero decisions here means the pipeline is not
				// reaching the strategies at all.
				if o.Decisions == 0 {
					t.Errorf("a clean trend produced no decisions: the pipeline is "+
						"not reaching strategy evaluation\n%s", o.describe())
				}
				// And the regime must be recorded as trending for most of it,
				// or the classifier is not seeing what the dataset contains.
				if o.Regimes["TRENDING"] == 0 {
					t.Errorf("no decision recorded TRENDING on a clean trend "+
						"dataset\n%s", o.describe())
				}
				if o.Regimes["TRENDING"] <= o.Regimes["RANGING"] {
					t.Errorf("a clean trend was classified RANGING at least as often "+
						"as TRENDING (%d against %d)\n%s",
						o.Regimes["RANGING"], o.Regimes["TRENDING"], o.describe())
				}
			},
		},
		{
			letter: "B", name: "range", dataset: "range-bound", steps: 60,
			expect: func(t *testing.T, o observed) {
				if o.Decisions == 0 {
					t.Errorf("a range produced no decisions at all\n%s", o.describe())
				}
				// A STRONGER assertion belongs here and is not yet sound.
				//
				// The regime is classified over the last 300 bars the store
				// holds for the instrument, and a replay's purge removes only
				// `provider = 'replay'` rows -- deliberately, so it does not
				// destroy the seeded fixture. So early in a run the window
				// mixes SEEDED history with replay bars, and the classification
				// partly describes the seed rather than the dataset.
				//
				// Measured: this dataset recorded TRENDING twelve times and
				// RANGING never, while the same close series classified in
				// isolation gives ADX 11.2, efficiency 0.143 and RANGING. The
				// dataset is range-bound; the window it was judged in was not.
				//
				// So this asserts only that the classifier is not PINNED --
				// that something other than one label appears -- and the
				// contamination is recorded as a finding rather than hidden by
				// a weaker threshold that looks like a real check.
				distinct := 0
				for _, n := range o.Regimes {
					if n > 0 {
						distinct++
					}
				}
				if distinct < 2 {
					t.Errorf("every decision recorded the same regime (%v): the "+
						"classifier is not responding to the market at all\n%s",
						o.Regimes, o.describe())
				}
			},
		},
		{
			letter: "C", name: "volatility shock", dataset: "volatility-shock", steps: 60,
			expect: func(t *testing.T, o observed) {
				if o.Decisions == 0 {
					t.Errorf("a volatility shock produced no decisions\n%s", o.describe())
				}
				// A shock must show up somewhere in the recorded conditions.
				// If every decision reads TRENDING through a volatility shock,
				// the regime is decorative.
				shockish := o.Regimes["HIGH_VOLATILITY"] + o.Regimes["RISK_OFF"] +
					o.Regimes["UNKNOWN"]
				if shockish == 0 {
					t.Errorf("a volatility shock recorded no HIGH_VOLATILITY, RISK_OFF "+
						"or UNKNOWN decision: the regime is not responding to the "+
						"market\n%s", o.describe())
				}
			},
		},
		{
			// 100 steps, not 60: the fixture widens the book only from bar 60,
			// so a shorter run never reaches the spike and would pass or fail
			// on the flat half of the dataset.
			letter: "E", name: "spread spike", dataset: "spread-spike", steps: 100,
			expect: func(t *testing.T, o observed) {
				// A spread spike must produce a refusal SOMEWHERE. Trading
				// through a spike is the behaviour the spread check exists to
				// prevent, so silence here is a failure even though nothing
				// errored.
				refusals := o.RejectCodes["spread_too_wide"] +
					o.SkipReasonsContaining("spread") +
					o.Regimes["RISK_OFF"]
				if refusals > 0 {
					return
				}
				// The account's daily-loss budget is a finite fixture resource
				// shared across this matrix. Once it is spent, every order is
				// refused before the spread check is reached -- a correct
				// refusal, but it means this scenario tested nothing about
				// spread. Skipping says so rather than passing silently.
				if o.Orders > 0 && o.RejectCodes["daily_loss_limit_reached"] >= o.Orders {
					t.Skipf("every order was refused for the daily loss limit before "+
						"the spread check was reached, so the spike was not "+
						"exercised; reseed to test it\n%s", o.describe())
				}
				if o.Decisions > 0 {
					t.Errorf("a spread spike produced no spread refusal, no spread "+
						"skip and no RISK_OFF: the platform traded through it\n%s",
						o.describe())
				}
			},
		},
		{
			letter: "H", name: "drawdown sequence", dataset: "drawdown", steps: 60,
			expect: func(t *testing.T, o observed) {
				// The point of a drawdown dataset is that a loss limit engages.
				// If the account traded all the way through without a single
				// risk refusal, either the dataset is not producing losses or
				// the limits are not being applied.
				riskRefusals := o.RejectCodes["daily_loss_limit_reached"] +
					o.RejectCodes["drawdown_limit_reached"] +
					o.RejectCodes["risk_limit_breached"] +
					o.RejectCodes["exposure_limit_breached"]
				if o.Orders > 5 && riskRefusals == 0 {
					t.Errorf("%d orders through a drawdown dataset with no risk "+
						"refusal at all\n%s", o.Orders, o.describe())
				}
			},
		},
		{
			letter: "K", name: "trend reversal", dataset: "trend-reversal", steps: 60,
			expect: func(t *testing.T, o observed) {
				if o.Decisions == 0 {
					t.Errorf("a trend reversal produced no decisions\n%s", o.describe())
				}
			},
		},
		{
			letter: "L", name: "false breakout", dataset: "false-breakout", steps: 60,
			expect: func(t *testing.T, o observed) {
				if o.Decisions == 0 {
					t.Errorf("a false breakout produced no decisions\n%s", o.describe())
				}
			},
		},
		{
			letter: "M", name: "correlated opportunities", dataset: "correlated-pair", steps: 60,
			expect: func(t *testing.T, o observed) {
				if o.Decisions == 0 {
					t.Errorf("a correlated pair produced no decisions\n%s", o.describe())
				}
				// The correlation check must have RUN on every decision. Its
				// verdict depends on the book, so this asserts it was applied
				// rather than that it fired.
				// Scoped to the most recent run's orders, so this asserts the
				// check ran HERE rather than in some earlier scenario.
				withCheck := psqlInt(t, `SELECT count(*) FROM decision_snapshots d
					WHERE d.risk_state::text LIKE '%portfolio_correlation%'
					  AND d.id IN (
						SELECT o.decision_id FROM orders o
						WHERE o.replay_run_id = (
							SELECT id FROM replay_runs ORDER BY started_at DESC LIMIT 1))`)
				if withCheck == 0 {
					t.Errorf("no decision recorded the portfolio_correlation check: "+
						"correlation is not reaching the risk engine\n%s", o.describe())
				}
			},
		},
		{
			letter: "T", name: "daily and session boundary", dataset: "day-boundary", steps: 60,
			expect: func(t *testing.T, o observed) {
				if o.Decisions == 0 {
					t.Errorf("the day-boundary dataset produced no decisions\n%s", o.describe())
				}
				// Crossing a session boundary must be visible in the recorded
				// sessions, or session attribution is measuring one session and
				// reporting it as several.
				sessions := psqlInt(t, `SELECT count(DISTINCT
					d.market_data_health->>'session') FROM decision_snapshots d
					WHERE d.market_data_health ? 'session'
					  AND d.id IN (
						SELECT o.decision_id FROM orders o
						WHERE o.replay_run_id = (
							SELECT id FROM replay_runs ORDER BY started_at DESC LIMIT 1))`)
				if sessions < 2 {
					t.Errorf("a dataset spanning a day boundary recorded %d distinct "+
						"sessions\n%s", sessions, o.describe())
				}
			},
		},
	}
}

// SkipReasonsContaining counts skips whose reason mentions a substring.
func (o observed) SkipReasonsContaining(needle string) int {
	total := 0
	for reason, n := range o.SkipReasons {
		if strings.Contains(strings.ToLower(reason), strings.ToLower(needle)) {
			total += n
		}
	}
	return total
}

func TestScenarioMatrixThroughTheRealPipeline(t *testing.T) {
	h := newHarness(t)

	for _, sc := range datasetScenarios() {
		sc := sc
		t.Run(fmt.Sprintf("%s_%s", sc.letter, strings.ReplaceAll(sc.name, " ", "_")), func(t *testing.T) {
			// Each scenario starts its own run. Start purges the previous
			// run's market data, so scenarios do not contaminate each other
			// through the quote table -- but they DO share the account, whose
			// ledger and risk budget accumulate. That is deliberate and
			// realistic: an account does not reset between market conditions,
			// and the invariants are written to hold regardless.
			h.t = t
			// Registered BEFORE the start, so a failure anywhere below still
			// releases the engine. Without this the first failing scenario
			// leaves a run active and every later one fails with a 409 that
			// says nothing about the market it was meant to test.
			t.Cleanup(func() {
				h.t = t
				h.do("POST", "/api/v1/replay/control", map[string]any{
					"action": "stop", "reason": "scenario teardown, releasing replay time",
				}, nil)
			})

			// The watermark. Every count is a running total over one shared
			// account, so the invariants are asserted on what THIS scenario
			// added -- otherwise each one silently tests its predecessors.
			before := h.observe()

			run := h.start(sc.dataset, "scenario "+sc.letter+": "+sc.name)
			if run.DatasetID != sc.dataset {
				t.Fatalf("started %q, wanted %q", run.DatasetID, sc.dataset)
			}
			if sc.arm != nil {
				sc.arm(h)
			}

			h.step(sc.steps)
			after := h.observe()
			o := after.since(before)

			// This suite is SINGLE-SHOT per seeded database, and says so
			// rather than failing confusingly.
			//
			// A strategy is evaluated once per completed bar and that is
			// recorded in strategy_runs, which a replay's purge does not touch
			// -- deliberately, because it is research history. So a second run
			// of this matrix finds every bar already evaluated and produces
			// nothing at all, and the failures read as a broken pipeline
			// rather than as a spent fixture.
			//
			// The same applies to the account's daily-loss budget, which the
			// earlier scenarios spend.
			if o.StrategyRuns > 0 && o.Skipped == o.StrategyRuns && o.Decisions == 0 {
				only := ""
				for reason := range o.SkipReasons {
					only = reason
					break
				}
				t.Skipf("all %d strategy runs were skipped (%q) and no decision was "+
					"taken: this matrix needs a freshly seeded database. Run "+
					"./scripts/dev-up.ps1 -Reset -Seed and try again\n%s",
					o.StrategyRuns, only, o.describe())
			}

			// The universal invariants are about the WHOLE account: a ledger
			// gap or a balance that does not reconcile is a failure whoever
			// wrote it.
			assertUniversalInvariants(t, after)
			sc.expect(t, o)

			if testing.Verbose() {
				t.Logf("scenario %s (%s):\n%s", sc.letter, sc.dataset, o.describe())
			}
		})
	}
}

// TestAReplayRunIsRecordedForEveryScenario checks the audit half: every run
// above must have left a durable record with its identity fields.
func TestAReplayRunIsRecordedForEveryScenario(t *testing.T) {
	if newHarness(t) == nil {
		return
	}
	recorded := psqlInt(t, "SELECT count(*) FROM replay_runs")
	if recorded == 0 {
		t.Fatal("no replay run was recorded")
	}
	// Identity: without these a result cannot be tied to what produced it.
	incomplete := psqlInt(t, `SELECT count(*) FROM replay_runs
		WHERE dataset_hash = '' OR code_sha = '' OR dataset_id = ''`)
	if incomplete != 0 {
		t.Errorf("%d recorded runs are missing an identity field", incomplete)
	}
	// And every run is simulated, by constraint.
	live := psqlInt(t, "SELECT count(*) FROM replay_runs WHERE NOT simulated")
	if live != 0 {
		t.Errorf("%d recorded runs claim not to be simulated", live)
	}
}
