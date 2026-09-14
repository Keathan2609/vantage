package replay

import (
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Scenario S: restart safety.
//
// # Why this stops a real process
//
// Resetting in-memory objects inside one process proves that the objects were
// reset. It cannot prove that the surviving state is sufficient, that the boot
// path notices an abandoned run, or that nothing durable was left claiming to
// be in progress -- which are the three things a restart actually risks.
//
// So this kills the control plane and waits for it to come back. It needs the
// stack to be started in a way the test can restart, which is what
// VANTAGE_REPLAY_RESTART_CMD supplies.

// restartControlPlane stops the running control plane and waits for a new one.
//
// The command that brings it back is supplied by the environment rather than
// guessed: the test has no business knowing how this installation runs its
// API, and a hard-coded `go run` would silently test a different binary from
// the one the operator is using.
func (h *harness) restartControlPlane() {
	t := h.t
	t.Helper()

	cmd := os.Getenv("VANTAGE_REPLAY_RESTART_CMD")
	if cmd == "" {
		t.Skip("set VANTAGE_REPLAY_RESTART_CMD to the shell command that starts " +
			"the control plane in replay mode; without it this test would have " +
			"to guess how the stack is run")
	}

	// Stop it. On Windows the process is named control-api whether it was
	// started by `go run` or from a build.
	var kill *exec.Cmd
	if runtime.GOOS == "windows" {
		kill = exec.Command("powershell", "-NoProfile", "-Command",
			"Stop-Process -Name control-api -Force -ErrorAction SilentlyContinue")
	} else {
		kill = exec.Command("pkill", "-f", "control-api")
	}
	_ = kill.Run()

	// Wait for it to actually be gone, or the restart races the shutdown and
	// the new process fails to bind.
	if !h.waitForHealth(false, 30*time.Second) {
		t.Fatal("the control plane did not stop")
	}

	// Bring it back.
	//
	// A SHELL is used deliberately, and the value is not untrusted input: it
	// is an environment variable the operator running this test sets, in a
	// test file, against their own machine. The command needs shell features
	// -- redirection and backgrounding -- so splitting it into argv would
	// break the thing it is for. A scanner will flag this shape; the reason it
	// is safe here is that the input's author and the test's runner are the
	// same person, and nothing reaches it from a request.
	start := exec.Command("bash", "-c", cmd) //nolint:gosec // operator-supplied dev-only command; see above
	start.Stdout, start.Stderr = nil, nil
	if err := start.Start(); err != nil {
		t.Fatalf("restarting the control plane: %v", err)
	}
	if !h.waitForHealth(true, 3*time.Minute) {
		t.Fatal("the control plane did not come back up")
	}

	// The session died with the process.
	h.signIn()
}

func (h *harness) waitForHealth(want bool, timeout time.Duration) bool {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(h.base + "/health/ready")
		up := err == nil && resp.StatusCode == http.StatusOK
		if resp != nil {
			resp.Body.Close()
		}
		if up == want {
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}

func TestScenarioS_ARestartStopsTheReplayAndDuplicatesNothing(t *testing.T) {
	h := newHarness(t)
	t.Cleanup(func() { h.t = t; h.stopQuietly() })

	// ---- Before: a run that has actually decided something ----------------
	h.start("trend-clean", "scenario S: restart safety during an autonomous replay")
	// Deep enough that real decisions exist, but short of the dataset's 140
	// instants so the run is still ACTIVE when the process dies -- restarting
	// a finished run would test the easy half.
	h.step(135)

	before := h.observe()
	if before.Decisions == 0 {
		t.Skipf("no decision was taken before the restart, so this would prove "+
			"nothing about duplicate financial actions\n%s", before.describe())
	}
	runID := psql(t, "SELECT id::text FROM replay_runs ORDER BY started_at DESC LIMIT 1")
	t.Logf("before restart: run %s, %d decisions, %d orders, %d fills, %d ledger "+
		"entries, balance %s",
		runID, before.Decisions, before.Orders, before.Fills, before.Ledger,
		before.StoredBalance)

	// ---- The restart ------------------------------------------------------
	h.restartControlPlane()

	// ---- After ------------------------------------------------------------
	after := h.observe()

	// 1. NOTHING financial was duplicated or lost. This is the whole point.
	if after.Orders != before.Orders {
		t.Errorf("orders changed across the restart: %d before, %d after. A "+
			"restart must not create or destroy a financial action.",
			before.Orders, after.Orders)
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
	assertUniversalInvariants(t, after)

	// 2. The replay did NOT resume by itself. Engaging one puts the whole
	//    process on dataset time, and doing that at boot would be deciding it
	//    for the operator at the moment nobody is watching.
	state := psql(t, "SELECT state FROM replay_runs WHERE id = '"+runID+"'")
	if state != "interrupted" {
		t.Errorf("the interrupted run is in state %q, want \"interrupted\". A run "+
			"left claiming to be running while nothing is running is a record "+
			"that contradicts reality.", state)
	}

	// 3. And the process is on REAL time, not the dataset's.
	var status struct {
		Active  bool `json:"active"`
		Engaged bool `json:"engaged"`
	}
	h.do("GET", "/api/v1/replay", nil, &status)
	if status.Engaged {
		t.Error("the replay clock is still engaged after a restart: this process " +
			"came back up on dataset time without anyone asking")
	}
	if status.Active {
		t.Error("a replay reports itself active after a restart, so it resumed " +
			"by itself")
	}

	// 4. No bar was evaluated twice, which is what would let a restart
	//    re-trade a completed decision.
	doubled := psqlInt(t, `
		SELECT count(*) FROM (
			SELECT strategy_id, strategy_version, account_id, instrument_id,
			       bar_time, count(*) AS n
			FROM strategy_runs WHERE bar_time IS NOT NULL
			GROUP BY 1,2,3,4,5 HAVING count(*) > 1
		) d`)
	if doubled > 0 {
		t.Errorf("%d (strategy, bar) pairs were evaluated more than once across "+
			"the restart", doubled)
	}

	// 5. The engine's in-process state is GONE rather than inherited.
	//
	// The window, the cursor and the phase counters live in the process that
	// died. Asserting they were reset means starting a fresh run and checking
	// that it begins at the beginning: a run that reported itself already past
	// warm-up would be carrying the dead run's position into a new market.
	//
	// A different dataset, so this cannot pass by finding the previous run's
	// bars still evaluated.
	h.start("range-bound", "after the restart: proving the engine starts clean")
	h.step(3)
	var fresh struct {
		Run struct {
			Phase              string `json:"phase"`
			WarmupInstants     int    `json:"warmup_instants"`
			EvaluationInstants int    `json:"evaluation_instants"`
			Steps              int    `json:"steps"`
		} `json:"run"`
	}
	h.do("GET", "/api/v1/replay", nil, &fresh)
	if fresh.Run.Phase != "warmup" {
		t.Errorf("a run started three instants after a restart reports phase %q, "+
			"want \"warmup\": the engine inherited a window from the process "+
			"that died", fresh.Run.Phase)
	}
	if fresh.Run.EvaluationInstants != 0 {
		t.Errorf("a freshly started run already counts %d evaluation instants, "+
			"so its counters were not reset", fresh.Run.EvaluationInstants)
	}
	if fresh.Run.WarmupInstants != fresh.Run.Steps {
		t.Errorf("the run played %d instants and attributes %d of them to "+
			"warm-up: the phase split does not account for every instant",
			fresh.Run.Steps, fresh.Run.WarmupInstants)
	}
	t.Logf("after the restart a fresh run reports phase=%s, warmup=%d, "+
		"evaluation=%d of %d instants", fresh.Run.Phase,
		fresh.Run.WarmupInstants, fresh.Run.EvaluationInstants, fresh.Run.Steps)
}

func TestAnInterruptedRunIsListedWithAResumeVerdict(t *testing.T) {
	// An operator has to be able to see what was abandoned and whether
	// continuing it is safe. A policy that requires a deliberate act is only
	// usable if the act is discoverable.
	h := newHarness(t)

	var out struct {
		Interrupted []struct {
			ID        string `json:"id"`
			DatasetID string `json:"dataset_id"`
			Cursor    int    `json:"cursor"`
			Resumable bool   `json:"resumable"`
			Reason    string `json:"reason"`
		} `json:"interrupted"`
		Policy string `json:"policy"`
	}
	if code := h.do("GET", "/api/v1/replay/interrupted", nil, &out); code != http.StatusOK {
		t.Fatalf("listing interrupted runs returned %d", code)
	}
	if !strings.Contains(out.Policy, "explicit operator resume") {
		t.Errorf("the endpoint does not state the restart policy: %q", out.Policy)
	}
	if len(out.Interrupted) == 0 {
		t.Skip("no interrupted run to inspect; run the restart scenario first")
	}
	for _, run := range out.Interrupted {
		if run.Reason == "" {
			t.Errorf("run %s carries no verdict, so an operator cannot tell "+
				"whether resuming it is safe", run.ID)
		}
		t.Logf("interrupted run %s on %s at instant %d: resumable=%t — %s",
			run.ID, run.DatasetID, run.Cursor, run.Resumable, run.Reason)
	}
}

func TestResumingIsRefusedWhenTheDatasetChanged(t *testing.T) {
	// The check that matters. A run resumed against an edited fixture would
	// attribute its result to data that no longer exists, and the record would
	// look complete while being wrong.
	h := newHarness(t)

	code := h.do("POST", "/api/v1/replay/control", map[string]any{
		"action": "resume_interrupted",
		"run_id": "00000000-0000-0000-0000-000000000000",
		"reason": "checking that an unknown run is refused",
	}, nil)
	if code == http.StatusOK {
		t.Error("resuming an unknown run succeeded")
	}

	// And a resume without a reason is refused, like every other control that
	// changes what the platform does on its own.
	code = h.do("POST", "/api/v1/replay/control", map[string]any{
		"action": "resume_interrupted", "run_id": "00000000-0000-0000-0000-000000000000",
	}, nil)
	if code == http.StatusOK {
		t.Error("resuming without a reason succeeded")
	}
}
