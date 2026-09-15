package replay

import (
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Determinism and speed invariance, measured on the result digest.
//
// # Why a digest rather than a list of assertions
//
// "Identical signals, decisions, orders, fills, ledger, attribution and final
// state" is seven assertions that each have to be written, kept in step with
// the schema, and remembered when a column is added. A canonical digest is one
// assertion that covers all of them and fails loudly when any diverges -- and
// names WHICH section diverged, which a set of separate assertions would not.
//
// # Why the database is restored rather than reseeded
//
// Two runs must begin from BYTE-IDENTICAL state. A reseed produces a fresh
// database that is equivalent but not identical: ids differ, timestamps
// differ, and the mock venue's own books start from a different instant. Only
// a dump and restore gives the same bytes.

const digestAccountQuery = `SELECT id::text FROM accounts ORDER BY created_at LIMIT 1`

// snapshotDatabase dumps the database to a file inside the container.
//
// Inside the container rather than to the host: the dump is large, it is
// temporary, and streaming it through the test process would make a
// three-iteration determinism proof slower than the runs it is proving.
func snapshotDatabase(t *testing.T, name string) {
	t.Helper()
	cmd := exec.Command("docker", "exec", "vantage-postgres",
		"pg_dump", "-U", "vantage_owner", "-d", "vantage", "-Fc", "-f", "/tmp/"+name)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pg_dump failed: %v\n%s", err, out)
	}
}

// restoreDatabase puts the bytes back.
func restoreDatabase(t *testing.T, name string) {
	t.Helper()
	cmd := exec.Command("docker", "exec", "vantage-postgres",
		"pg_restore", "-U", "vantage_owner", "-d", "vantage", "--clean", "--if-exists",
		"/tmp/"+name)
	// pg_restore reports non-fatal notices on stderr and exits non-zero for
	// them, so the exit code alone is not a verdict. A genuinely failed
	// restore shows up immediately as a missing account.
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), "FATAL") {
		t.Fatalf("pg_restore failed: %s", out)
	}
}

// digestOf reads the account's canonical financial fingerprint.
func (h *harness) digestOf(accountID string) (string, map[string]string, map[string]int) {
	h.t.Helper()
	var out struct {
		Digest     string            `json:"digest"`
		Components map[string]string `json:"components"`
		Counts     map[string]int    `json:"counts"`
	}
	if code := h.do("GET", "/api/v1/replay/digest/"+accountID, nil, &out); code != http.StatusOK {
		h.t.Fatalf("reading the result digest returned %d", code)
	}
	return out.Digest, out.Components, out.Counts
}

func describeDigest(components map[string]string, counts map[string]int) string {
	var b strings.Builder
	for _, name := range []string{"decisions", "orders", "fills", "ledger",
		"positions", "attribution", "reconciliation"} {
		sum := components[name]
		if len(sum) > 16 {
			sum = sum[:16]
		}
		fmt.Fprintf(&b, "    %-15s %s  (%d rows)\n", name, sum, counts[name])
	}
	return b.String()
}

// runOnce plays a dataset from the restored snapshot and returns its digest.
//
// perRequest is how many instants one HTTP call may advance. It is a parameter
// rather than a constant because a PACED run sleeps inside the request -- see
// stepInBatches -- so the batch that is right at max is far too large at 1x.
func (h *harness) runOnce(t *testing.T, dataset string, steps int, speed string,
	perRequest int, snapshot, accountID string) (string, map[string]string, map[string]int) {

	t.Helper()
	restoreDatabase(t, snapshot)
	// The session survives the restore -- sessions live in the database, so it
	// does not. Sign in again.
	h.signIn()

	// The BASELINE, read from the restored snapshot before anything runs.
	//
	// The digest covers the account, not the run, and the restored account is
	// not empty. So "the digest has rows" says nothing about whether this run
	// produced any of them -- a run that did nothing at all leaves the
	// baseline's rows behind and the digests agree perfectly. The counts are
	// therefore compared against this, and the vacuity guards read the
	// difference rather than the total.
	_, _, baseline := h.digestOf(accountID)

	run := h.control("start", map[string]any{
		"dataset": dataset, "seed": 42, "speed": speed,
		"reason": "determinism iteration at speed " + speed,
	})
	// The same preflight the scenario matrix applies. Without it a run that
	// cannot trade steps happily to the end of the dataset and this suite
	// compares four empty accounts.
	h.assertRunCanTrade(run)
	if perRequest <= 0 {
		// Paced: run it in the background and poll. See advanceUntilDone for
		// why a paced run cannot be stepped through a request.
		h.advanceUntilDone(25 * time.Minute)
	} else {
		h.stepInBatches(steps, perRequest)
	}
	digest, components, counts := h.digestOf(accountID)
	h.stopQuietly()

	produced := map[string]int{}
	for name, n := range counts {
		produced[name] = n - baseline[name]
	}
	// Carried alongside the absolute counts under a distinct key, so a caller
	// asserting "this run decided something" cannot accidentally read the
	// account's opening position as evidence.
	counts["decisions_produced"] = produced["decisions"]
	counts["orders_produced"] = produced["orders"]
	counts["fills_produced"] = produced["fills"]
	return digest, components, counts
}

func TestDeterminismAcrossScenarioClasses(t *testing.T) {
	// Three runs of each dataset from byte-identical state. Three rather than
	// two because two agreeing could be luck in a system with one source of
	// randomness; a third makes that implausible.
	h := newHarness(t)
	t.Cleanup(func() { h.t = t; h.stopQuietly() })

	accountID := psql(t, digestAccountQuery)
	if accountID == "" {
		t.Fatal("no account to measure")
	}

	const snapshot = "determinism.dump"
	h.stopQuietly()
	snapshotDatabase(t, snapshot)

	for _, tc := range []struct {
		name    string
		dataset string
		steps   int
	}{
		{"A_clean_trend", "trend-clean", 140},
		{"B_range", "range-bound", 140},
		{"M_correlated", "correlated-pair", 120},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h.t = t
			var first string
			var firstComponents map[string]string
			var firstCounts map[string]int

			for i := 1; i <= 3; i++ {
				digest, components, counts := h.runOnce(
					t, tc.dataset, tc.steps, "max", 20, snapshot, accountID)

				if i == 1 {
					first, firstComponents, firstCounts = digest, components, counts
					t.Logf("run 1 digest %s\n%s", digest[:16],
						describeDigest(components, counts))
					continue
				}
				if digest != first {
					var diverged []string
					for name, sum := range components {
						if firstComponents[name] != sum {
							diverged = append(diverged, fmt.Sprintf(
								"%s (%d rows against %d)",
								name, counts[name], firstCounts[name]))
						}
					}
					t.Fatalf("run %d produced digest %s, run 1 produced %s.\n"+
						"Diverged in: %s\nrun %d:\n%srun 1:\n%s",
						i, digest[:16], first[:16], strings.Join(diverged, ", "),
						i, describeDigest(components, counts),
						describeDigest(firstComponents, firstCounts))
				}
				t.Logf("run %d digest %s — identical", i, digest[:16])
			}

			// A digest over an empty account is identical for a trivial
			// reason, so agreement here proves nothing about determinism.
			//
			// A SKIP rather than a failure: a range-bound dataset producing no
			// actionable signal is correct behaviour, not a defect. What is
			// wrong is claiming it as evidence of determinism, and a skip says
			// exactly that.
			// Measured on what the run ADDED. The digest covers the account,
			// and the restored snapshot is not empty, so a total above zero
			// would be satisfied by rows this run did not write.
			if firstCounts["decisions_produced"] <= 0 {
				t.Skipf("this dataset produced no decisions OF ITS OWN (the "+
					"account already held %d), so the digests agree vacuously "+
					"and this run is not evidence of determinism\n%s",
					firstCounts["decisions"],
					describeDigest(firstComponents, firstCounts))
			}

			// A decision is no longer proof that the financial path ran.
			//
			// It used to be. A decision snapshot was written only by the OMS,
			// so "this run produced decisions" implied an order was attempted,
			// and the orders, fills, ledger, position and balance sections of
			// the digest had something in them. Since multi-strategy
			// aggregation, a verdict that declines writes its own no-trade
			// snapshot BEFORE the OMS is reached -- so the guard above is
			// satisfied by a run that moved no money at all, and five of the
			// digest's seven sections agree because all five are empty.
			//
			// That is the same defect the guard above exists to prevent,
			// reintroduced through the back door by a change somewhere else.
			// The lesson is the one this suite keeps relearning: a vacuity
			// guard has to measure the thing it stands in for, never a proxy
			// that merely happened to imply it.
			//
			// Decision determinism IS still demonstrated above, and it is
			// worth something: three runs produced byte-identical verdicts. It
			// is not financial determinism, and only the skip can say so.
			if firstCounts["orders_produced"] <= 0 {
				t.Skipf("this dataset produced %d decisions of its own and NO "+
					"orders, so the digests agree on identical verdicts and on "+
					"five empty sections. That is evidence of decision "+
					"determinism and not of financial determinism\n%s",
					firstCounts["decisions_produced"],
					describeDigest(firstComponents, firstCounts))
			}
		})
	}
}

func TestSpeedDoesNotAlterFinancialOutput(t *testing.T) {
	// Speed changes wall-clock pacing and must change nothing else. A timing
	// dependency here would mean the platform's results depend on how fast it
	// happened to run, which is the least reproducible property a research
	// system can have.
	//
	// # Why the batch size varies with the speed
	//
	// The engine's pacing sleep happens INSIDE the step request and the router
	// gives every handler thirty seconds. Twenty paced instants therefore need
	// forty seconds and the request dies. The batch shrinks instead of the
	// deadline growing: the deadline protects every other route, and a "1x"
	// that only works when the API is made slower to kill would not be the
	// same system under test. See stepInBatches.
	//
	// # STEP is a mode, not a speed
	//
	// One instant per request is the manual control an operator uses, and it
	// crosses a request, transaction and context boundary between every pair
	// of instants. That is a genuinely different execution shape from a batch
	// of twenty, and it is the one most likely to expose state that survives
	// between instants only because nothing interrupted it.
	h := newHarness(t)
	t.Cleanup(func() { h.t = t; h.stopQuietly() })

	accountID := psql(t, digestAccountQuery)
	const snapshot = "speed.dump"
	h.stopQuietly()
	snapshotDatabase(t, snapshot)

	// The WHOLE dataset, not a prefix.
	//
	// The first sixty instants are warm-up and produce no executable intent by
	// design, so a run of eighty leaves only twenty evaluation instants -- and
	// measured, those twenty produced no actionable signal at all. Four modes
	// then agree on an account nothing touched. The cost is wall-clock time in
	// the paced modes, which is the right thing to spend it on.
	const replaySteps = 140

	type mode struct {
		name       string
		speed      string
		perRequest int
	}
	type result struct {
		mode    string
		digest  string
		counts  map[string]int
		elapsed time.Duration
	}
	var results []result

	// perRequest of zero means "advance in the background and poll", which is
	// the only way a PACED run can be driven: the pacing sleep happens inside
	// the step request, and no batch size is small enough to stay under the
	// thirty-second handler deadline once the pipeline is producing orders.
	for _, m := range []mode{
		{"STEP", "max", 1},
		{"1x", "1x", 0},
		{"10x", "10x", 0},
		{"MAX", "max", 20},
	} {
		start := time.Now()
		digest, components, counts := h.runOnce(
			t, "trend-clean", replaySteps, m.speed, m.perRequest, snapshot, accountID)
		elapsed := time.Since(start)
		results = append(results, result{m.name, digest, counts, elapsed})
		driver := fmt.Sprintf("%d instants per request", m.perRequest)
		if m.perRequest <= 0 {
			driver = "background advance, polled"
		}
		t.Logf("mode %-4s (speed %s, %s) digest %s in %s\n%s",
			m.name, m.speed, driver, digest[:16],
			elapsed.Round(time.Millisecond), describeDigest(components, counts))
	}

	for i := 1; i < len(results); i++ {
		if results[i].digest != results[0].digest {
			t.Errorf("mode %s produced digest %s and mode %s produced %s. "+
				"Replay speed must alter pacing and nothing else; a difference "+
				"here is a timing dependency in the financial path.",
				results[i].mode, results[i].digest[:16],
				results[0].mode, results[0].digest[:16])
		}
	}

	// Agreement across four modes that all did nothing is not evidence.
	//
	// Measured against what each run ADDED, not what the account holds. The
	// first version of this guard read the total, and passed on four runs that
	// each produced nothing because the research service was down: the
	// restored snapshot's single pre-existing decision satisfied it every
	// time, and four empty runs were reported as speed invariance.
	for _, r := range results {
		if r.counts["decisions_produced"] <= 0 {
			t.Errorf("mode %s produced no decision of its own (it added %d "+
				"orders and %d fills), so its digest describes the restored "+
				"snapshot rather than a replay and proves nothing about speed",
				r.mode, r.counts["orders_produced"], r.counts["fills_produced"])
		}
	}

	// And the financial half, which a decision count stopped standing in for
	// once a declining verdict began writing its own snapshot. See the longer
	// note on the same guard in TestDeterminismAcrossScenarioClasses.
	//
	// A SKIP and not a failure: four modes that each evaluated the dataset and
	// each declined to trade have demonstrated that SPEED did not change the
	// decision, which is real. They have not demonstrated anything about fill
	// prices, commission or the ledger, because none exist -- and reporting
	// that as speed invariance is the exact claim this suite was once caught
	// making.
	traded := false
	for _, r := range results {
		if r.counts["orders_produced"] > 0 {
			traded = true
			break
		}
	}
	if !traded {
		t.Skipf("no mode produced an order, so the digests agree on four sets " +
			"of identical verdicts and on empty orders, fills, ledger, " +
			"positions and balance. Speed did not change the DECISION, which " +
			"is what this run shows; it shows nothing about financial output")
	}

	// What the wall clock actually showed, reported rather than asserted.
	//
	// The pacing cap is two seconds per instant, so on this dataset's HOURLY
	// bars every finite speed is capped to the same two seconds: 3600s/1,
	// 3600s/10 and 3600s/100 all exceed the cap. 1x and 10x are therefore
	// expected to take the SAME wall time here, and the only pacing difference
	// the test can demonstrate is between a paced mode and max. That is a
	// property of the cap, not a defect -- an uncapped 1x on 1h bars would
	// take eighty hours -- but it does mean the speed NAMES overstate what
	// they control on long timeframes.
	var line strings.Builder
	for _, r := range results {
		fmt.Fprintf(&line, "%s=%s ", r.mode, r.elapsed.Round(time.Millisecond))
	}
	t.Logf("wall clock: %s", strings.TrimSpace(line.String()))
	paced, unpaced := results[1].elapsed, results[3].elapsed
	if paced <= unpaced {
		t.Errorf("the paced mode (1x, %s) was no slower than max (%s), so "+
			"pacing did not take effect and the comparison is between four "+
			"identical runs", paced.Round(time.Millisecond),
			unpaced.Round(time.Millisecond))
	}
}

func TestTheDigestExcludesNonDeterministicValues(t *testing.T) {
	// Two reads of the SAME state must agree. If the digest included a
	// generated id or a wall-clock timestamp it would differ between two reads
	// seconds apart, and every determinism failure above would be noise.
	h := newHarness(t)
	accountID := psql(t, digestAccountQuery)

	first, _, _ := h.digestOf(accountID)
	time.Sleep(1100 * time.Millisecond)
	second, _, _ := h.digestOf(accountID)

	if first != second {
		t.Errorf("two reads of unchanged state produced different digests, %s "+
			"and %s: the canonical form includes something non-deterministic",
			first[:16], second[:16])
	}
}
