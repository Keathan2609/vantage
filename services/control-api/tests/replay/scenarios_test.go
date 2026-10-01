package replay

import (
	"fmt"
	"strings"
	"testing"

	"github.com/vantage/control-api/internal/domain"
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

// Every scenario steps its dataset to the END, and that is not an arbitrary
// choice.
//
// The first sixty instants of any run are WARM-UP: they build indicators, the
// regime and the correlation matrix, and produce no executable intent by
// design. These scenarios used to step sixty, which was the whole dataset's
// useful half before the warm-up model existed and is now exactly the half
// that cannot trade. Eight of the nine would have asserted on an evaluation
// window of zero instants.
//
// So the step count is the dataset's own length. A scenario that wants a
// shorter window should shorten its FIXTURE, where the intent is visible.

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
			letter: "A", name: "clean trend", dataset: "trend-clean", steps: 140,
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
			letter: "B", name: "range", dataset: "range-bound", steps: 140,
			expect: func(t *testing.T, o observed) {
				// NOT "a range produced decisions". A range is the market this
				// fixture exists to show a trend strategy DECLINING on -- the
				// registry describes it as the one "on which a trend strategy
				// should not churn" -- so asserting it must trade asserts the
				// opposite of the point. What must hold is that the strategies
				// were reached and looked.
				if !assertPipelineReachedTheStrategies(t, o) {
					return
				}
				if o.Decisions == 0 {
					t.Logf("a range produced no actionable signal at all, which is "+
						"the behaviour this dataset exists to show\n%s", o.describe())
					return
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
				// That was measured when only ORDER-BEARING instants recorded a
				// decision: twelve of them, all taken at instants where a trend
				// strategy happened to fire, all labelled TRENDING. The
				// assertion was weakened to "not pinned to one label" because
				// asserting the right label would have failed on contaminated
				// evidence.
				//
				// Since aggregation, EVERY evaluated instant records a decision
				// whether or not it trades, so the sample is the whole run
				// rather than the subset that traded -- and the classifier now
				// reports RANGING across it. The assertion is therefore
				// STRENGTHENED rather than relaxed: on a range-bound dataset
				// the dominant label must be RANGING. "Not pinned" would now
				// FAIL on a correct classifier, which is the wrong direction
				// entirely.
				dominant, count := "", 0
				for regime, n := range o.Regimes {
					if n > count {
						dominant, count = regime, n
					}
				}
				if count == 0 {
					t.Errorf("no decision recorded any regime at all\n%s", o.describe())
				} else if dominant != string(domain.RegimeRanging) {
					t.Errorf("a range-bound dataset was classified mostly %s "+
						"(%v). The window a decision is judged in mixes seeded "+
						"history with replay bars early in a run, and this is "+
						"what that contamination looks like.\n%s",
						dominant, o.Regimes, o.describe())
				} else {
					t.Logf("scenario B: the classifier reported %s on %d of %d "+
						"decisions across a range-bound dataset",
						dominant, count, o.Decisions)
				}
			},
		},
		{
			letter: "C", name: "volatility shock", dataset: "volatility-shock", steps: 140,
			expect: func(t *testing.T, o observed) {
				if !assertPipelineReachedTheStrategies(t, o) {
					return
				}
				// A shock suppressing every actionable signal is a defensible
				// outcome -- it is what a risk-aware strategy set should do -- so
				// it is reported rather than failed. What would be wrong is
				// trading through it while recording an ordinary regime, and
				// that is the assertion below.
				if o.Decisions == 0 {
					t.Logf("a volatility shock produced no actionable signal, which "+
						"is a defensible response to it\n%s", o.describe())
					return
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
			letter: "H", name: "drawdown sequence", dataset: "drawdown", steps: 120,
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
			letter: "K", name: "trend reversal", dataset: "trend-reversal", steps: 140,
			expect: func(t *testing.T, o observed) {
				if o.Decisions == 0 {
					t.Errorf("a trend reversal produced no decisions\n%s", o.describe())
				}
			},
		},
		{
			letter: "L", name: "false breakout", dataset: "false-breakout", steps: 120,
			expect: func(t *testing.T, o observed) {
				if o.Decisions == 0 {
					t.Errorf("a false breakout produced no decisions\n%s", o.describe())
				}
			},
		},
		{
			letter: "M", name: "correlated opportunities", dataset: "correlated-pair", steps: 180,
			expect: func(t *testing.T, o observed) {
				if o.Decisions == 0 {
					t.Errorf("a correlated pair produced no decisions\n%s", o.describe())
				}
				// The correlation check must have RUN on every decision. Its
				// verdict depends on the book, so this asserts it was applied
				// rather than that it fired.
				// Scoped to the most recent run's orders, so this asserts the
				// check ran HERE rather than in some earlier scenario.
				// `risk_state` is written by the OMS and by nothing else, so
				// the correlation check can only appear on a decision that
				// reached the risk engine. Since aggregation, a verdict that
				// declines writes its own snapshot and places no order, so a
				// run can hold many decisions and offer the risk engine none.
				//
				// With no order the check did not run, which is a fact about
				// the consensus rather than about correlation. A skip says so;
				// asserting anyway would report "correlation is not reaching
				// the risk engine" for a run in which nothing reached it.
				placed := psqlInt(t, `SELECT count(*) FROM orders WHERE replay_run_id = (
					SELECT id FROM replay_runs ORDER BY started_at DESC LIMIT 1)`)
				if placed == 0 {
					t.Skipf("no order reached the risk engine in this run, so the "+
						"correlation check had nothing to run on\n%s", o.describe())
				}
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
			letter: "T", name: "daily and session boundary", dataset: "day-boundary", steps: 180,
			expect: func(t *testing.T, o observed) {
				if o.Decisions == 0 {
					t.Errorf("the day-boundary dataset produced no decisions\n%s", o.describe())
				}
				// Crossing a session boundary must be visible in the recorded
				// sessions, or session attribution is measuring one session and
				// reporting it as several.
				// Scoped to the RUN rather than to its orders.
				//
				// Session attribution is decision-time information and every
				// evaluated instant now records it, traded or not. Reading
				// only order-bearing decisions would describe the instants
				// that traded rather than the day the dataset spans -- and on
				// a run that places nothing it would describe nothing at all
				// while looking like a broken session clock.
				sessions := psqlInt(t, `SELECT count(DISTINCT
					d.market_data_health->>'session') FROM decision_snapshots d
					WHERE d.market_data_health ? 'session'
					  AND d.created_at >= (
						SELECT started_at FROM replay_runs
						ORDER BY started_at DESC LIMIT 1)`)
				if sessions < 2 {
					t.Errorf("a dataset spanning a day boundary recorded %d distinct "+
						"sessions\n%s", sessions, o.describe())
				}
			},
		},
	}
}

// assertPipelineReachedTheStrategies separates the two things that "no
// decisions" can mean.
//
// It can mean the pipeline never arrived -- ingestion, bars, authority,
// Autopilot, the research service -- which is a defect. Or it can mean the
// strategies looked and declined, which for a range or a shock is the DESIRED
// behaviour and is exactly what those fixtures are built to produce.
//
// Asserting "decisions > 0" cannot tell them apart, and on a range it asserts
// the opposite of the point. What distinguishes them is whether strategy
// EVALUATIONS happened: a signal recorded as no_trade is the pipeline working
// and the strategy declining; no evaluation at all is the pipeline not
// arriving.
//
// This became load-bearing when the isolation milestone added the data floor.
// Before it, a strategy could see the seeded 8730-bar history and signalled
// from the first instant; now it sees only the replay's own bars, so a dataset
// has to build that history itself before anything is actionable.
func assertPipelineReachedTheStrategies(t *testing.T, o observed) bool {
	t.Helper()
	evaluated := o.StrategyRuns - o.Skipped
	if evaluated <= 0 {
		t.Errorf("no strategy was evaluated at all: %d runs, all skipped. The "+
			"pipeline did not reach strategy evaluation, which is a different "+
			"thing from the strategies declining to act.\n%s",
			o.StrategyRuns, o.describe())
		return false
	}
	return true
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
	runScenarios(t, datasetScenarios())
}

// TestConditionScenariosThroughTheRealPipeline drives D, G, I and N.
//
// Separate from the market matrix above because each of these ARMS something
// first -- a release, a news item, a tightened ceiling -- and a reader looking
// for "what does the platform do when a signal meets a blackout" should not
// have to find it inside a list of market shapes. They share the runner, so
// the universal invariants and the single-shot guard apply identically.
func TestConditionScenariosThroughTheRealPipeline(t *testing.T) {
	runScenarios(t, conditionScenarios())
}

// runScenarios drives a set of scenarios, each as its own subtest.
func runScenarios(t *testing.T, scenarios []scenario) {
	t.Helper()
	h := newHarness(t)

	for _, sc := range scenarios {
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
