// Package replay drives the milestone's scenario matrix through the REAL
// application pipeline.
//
// # Why these are integration tests against a running stack
//
// The claim being tested is that a market replay drives ingestion, bar
// creation, strategy evaluation, orchestration, risk, authority, Autopilot,
// the OMS, the mock venue, booking, the portfolio and the audit chain. A
// harness that called those itself would prove the harness worked, and every
// difference between it and the scheduler would be invisible until it
// mattered. So the scenarios drive the admin replay API of a running process
// and then assert on the business state that process wrote.
//
// # Why every scenario declares invariants rather than asserting PASS
//
// A replay that returns HTTP 200 for every request and reaches the wrong
// business outcome is the failure mode worth catching. "The run completed" is
// not an assertion about trading. Each scenario therefore states what it
// expects of orders, refusals, NO TRADE outcomes, the ledger and the final
// position, and fails when the platform does something defensible-looking and
// wrong.
//
// # Skipping
//
// These SKIP unless VANTAGE_REPLAY_E2E is set, because they need the
// development stack running IN REPLAY MODE:
//
//	$env:VANTAGE_MARKET_DATA_PROVIDER = "replay"   # on the API process
//	$env:VANTAGE_REPLAY_E2E = "1"
//	$env:VANTAGE_E2E_ADMIN_PASSWORD = "<printed by control-api seed>"
//	go test ./tests/replay/ -v -count=1 -timeout 30m
//
// # Paper only
//
// The stack is paper-mode by construction: no live adapter is compiled in.
// Nothing here can reach a real venue.
package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// harness is an authenticated admin session against the running stack.
type harness struct {
	t    *testing.T
	base string
	http *http.Client
	csrf string
	// password is kept so the session can be re-established after a process
	// restart, which invalidates it along with everything else in memory.
	password string
}

// signIn establishes an admin session.
//
// Separate from newHarness so a restart test can call it again: the session
// dies with the process, and every later request would otherwise fail with a
// 401 that says nothing about the restart being tested.
func (h *harness) signIn() {
	h.t.Helper()
	jar, _ := cookiejar.New(nil)
	h.http.Jar = jar
	h.csrf = ""

	var login struct {
		CSRFToken string `json:"csrf_token"`
	}
	if code := h.do("POST", "/api/v1/auth/login", map[string]string{
		"email": "admin@vantage.local", "password": h.password,
	}, &login); code != http.StatusOK {
		h.t.Fatalf("admin sign-in returned %d", code)
	}
	h.csrf = login.CSRFToken
}

func requireReplayStack(t *testing.T) string {
	t.Helper()
	if os.Getenv("VANTAGE_REPLAY_E2E") == "" {
		t.Skip("set VANTAGE_REPLAY_E2E=1 and run the stack in replay mode")
	}
	base := os.Getenv("VANTAGE_E2E_BASE_URL_API")
	if base == "" {
		base = "http://localhost:8080"
	}

	// Fail fast and loudly if the process is not in replay mode. Without this
	// every scenario would fail on a 404 from the replay routes, which reads
	// like a broken test rather than a misconfigured stack.
	resp, err := http.Get(base + "/health/ready")
	if err != nil {
		t.Fatalf("the control plane is not reachable at %s: %v", base, err)
	}
	resp.Body.Close()
	return base
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	base := requireReplayStack(t)

	password := os.Getenv("VANTAGE_E2E_ADMIN_PASSWORD")
	if password == "" {
		t.Fatal("VANTAGE_E2E_ADMIN_PASSWORD is required: replay control is admin-only")
	}
	jar, _ := cookiejar.New(nil)
	h := &harness{t: t, base: base, http: &http.Client{
		Jar: jar, Timeout: 10 * time.Minute,
	}}

	h.password = password
	h.signIn()

	// Replay is admin-only and the routes exist only in a replay process.
	if code := h.do("GET", "/api/v1/replay", nil, nil); code == http.StatusNotFound {
		t.Fatal("the replay routes are absent: run the API with " +
			"VANTAGE_MARKET_DATA_PROVIDER=replay")
	}
	return h
}

func (h *harness) do(method, path string, payload any, out any) int {
	h.t.Helper()
	var body *bytes.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			h.t.Fatalf("marshal: %v", err)
		}
		body = bytes.NewReader(raw)
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, h.base+path, body)
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin())
	if h.csrf != "" {
		req.Header.Set("X-Vantage-CSRF", h.csrf)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if derr := json.NewDecoder(resp.Body).Decode(out); derr != nil && resp.StatusCode < 300 {
			h.t.Fatalf("%s %s: decode: %v", method, path, derr)
		}
	}
	return resp.StatusCode
}

func origin() string {
	if o := os.Getenv("VANTAGE_SMOKE_WEB_ORIGIN"); o != "" {
		return o
	}
	return "http://localhost:3001"
}

// ---------------------------------------------------------------------------
// Replay control
// ---------------------------------------------------------------------------

type runView struct {
	ID        string   `json:"id"`
	DatasetID string   `json:"dataset_id"`
	State     string   `json:"state"`
	Steps     int      `json:"steps"`
	Errors    int      `json:"errors"`
	RowsTotal int      `json:"rows_total"`
	Simulated bool     `json:"simulated"`
	Warnings  []string `json:"warnings"`
}

func (h *harness) control(action string, extra map[string]any) runView {
	h.t.Helper()
	payload := map[string]any{"action": action}
	for k, v := range extra {
		payload[k] = v
	}
	var out struct {
		Run runView `json:"run"`
	}
	code := h.do("POST", "/api/v1/replay/control", payload, &out)
	if code != http.StatusOK {
		h.t.Fatalf("replay %s returned %d", action, code)
	}
	return out.Run
}

// start begins a run on a dataset and asserts nothing is quietly wrong.
//
// Any run left active by an earlier failure is stopped first. Without this one
// failing scenario makes every later one fail with a 409 that says nothing
// about the market it was meant to test -- and the 409 outlives the test
// process, so the next `go test` starts broken too.
func (h *harness) start(dataset, reason string) runView {
	h.t.Helper()
	h.do("POST", "/api/v1/replay/control", map[string]any{
		"action": "stop", "reason": "releasing any run left active before this one",
	}, nil)

	run := h.control("start", map[string]any{
		"dataset": dataset, "seed": 42, "speed": "max", "reason": reason,
	})
	if !run.Simulated {
		h.t.Fatal("a replay run did not report itself simulated")
	}
	// A preflight warning means the run cannot trade, and every invariant
	// about trading would then pass for the wrong reason. Autopilot off and an
	// expired authority are the two that bite.
	for _, w := range run.Warnings {
		if strings.Contains(w, "Autopilot") || strings.Contains(w, "authority") {
			h.t.Fatalf("this run cannot trade, so its invariants would be "+
				"meaningless: %s", w)
		}
	}
	return run
}

func (h *harness) stop(reason string) {
	h.t.Helper()
	h.control("stop", map[string]any{"reason": reason})
}

// step advances the run, in bounded requests so no single call sits for
// minutes.
func (h *harness) step(total int) runView {
	h.t.Helper()
	const perRequest = 20
	var run runView
	for done := 0; done < total; done += perRequest {
		n := perRequest
		if total-done < n {
			n = total - done
		}
		run = h.control("step", map[string]any{"steps": n})
		if run.State == "done" || run.State == "failed" || run.State == "stopped" {
			break
		}
	}
	return run
}

func (h *harness) inject(fault string, extra map[string]any) {
	h.t.Helper()
	payload := map[string]any{"fault": fault}
	for k, v := range extra {
		payload[k] = v
	}
	if code := h.do("POST", "/api/v1/replay/inject", payload, nil); code != http.StatusOK {
		h.t.Fatalf("injecting %s returned %d", fault, code)
	}
}

// ---------------------------------------------------------------------------
// Reading business state
// ---------------------------------------------------------------------------

// psql reads state directly, because the assertions are about what was
// PERSISTED. An API response is the platform's account of itself; the tables
// are the thing the account is about.
func psql(t *testing.T, sql string) string {
	t.Helper()
	cmd := exec.Command("docker", "exec", "vantage-postgres",
		"psql", "-U", "vantage_app", "-d", "vantage", "-tAc", sql)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("psql failed: %v\n%s\nSQL: %s", err, out, sql)
	}
	return strings.TrimSpace(string(out))
}

func psqlInt(t *testing.T, sql string) int {
	t.Helper()
	raw := psql(t, sql)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("psql did not return an integer for %s: %q", sql, raw)
	}
	return n
}

// observed is everything a scenario may assert on, gathered once so an
// invariant reads as a statement rather than a query.
type observed struct {
	Orders          int
	Filled          int
	Rejected        int
	PartiallyFilled int
	RejectCodes     map[string]int

	Decisions   int
	NoTrade     int
	Accepted    int
	DecisionsBy map[string]int // outcome_code -> count
	Regimes     map[string]int

	StrategyRuns int
	NoSignal     int
	Skipped      int
	SkipReasons  map[string]int

	Fills      int
	Ledger     int
	Positions  int
	OpenPos    int
	RunState   string
	RunSteps   int
	RunErrors  int
	LedgerGaps int
	// BalanceMatches is the stored balance against the ledger-derived one.
	BalanceMatches bool
	StoredBalance  string
	DerivedBalance string
}

func (h *harness) observe() observed {
	t := h.t
	t.Helper()
	o := observed{
		RejectCodes: map[string]int{}, DecisionsBy: map[string]int{},
		Regimes: map[string]int{}, SkipReasons: map[string]int{},
	}

	o.Orders = psqlInt(t, "SELECT count(*) FROM orders")
	o.Filled = psqlInt(t, "SELECT count(*) FROM orders WHERE status='FILLED'")
	o.PartiallyFilled = psqlInt(t, "SELECT count(*) FROM orders WHERE status='PARTIALLY_FILLED'")
	o.Rejected = psqlInt(t, "SELECT count(*) FROM orders WHERE status='REJECTED'")
	o.Fills = psqlInt(t, "SELECT count(*) FROM fills")
	o.Ledger = psqlInt(t, "SELECT count(*) FROM transactions")
	o.Positions = psqlInt(t, "SELECT count(*) FROM positions")
	o.OpenPos = psqlInt(t, "SELECT count(*) FROM positions WHERE status='open'")

	o.Decisions = psqlInt(t, "SELECT count(*) FROM decision_snapshots")
	o.NoTrade = psqlInt(t, "SELECT count(*) FROM decision_snapshots WHERE outcome='no_trade'")
	o.Accepted = psqlInt(t, "SELECT count(*) FROM decision_snapshots WHERE outcome='accepted'")

	for _, row := range rows(t, `SELECT coalesce(reject_code,''), count(*) FROM orders
		WHERE reject_code IS NOT NULL GROUP BY 1`) {
		o.RejectCodes[row[0]] = atoi(t, row[1])
	}
	for _, row := range rows(t, `SELECT coalesce(outcome_code,''), count(*)
		FROM decision_snapshots GROUP BY 1`) {
		o.DecisionsBy[row[0]] = atoi(t, row[1])
	}
	for _, row := range rows(t, `SELECT regime, count(*) FROM decision_snapshots GROUP BY 1`) {
		o.Regimes[row[0]] = atoi(t, row[1])
	}

	o.StrategyRuns = psqlInt(t, "SELECT count(*) FROM strategy_runs")
	o.NoSignal = psqlInt(t, "SELECT count(*) FROM strategy_runs WHERE status='no_signal'")
	o.Skipped = psqlInt(t, "SELECT count(*) FROM strategy_runs WHERE status='skipped'")
	for _, row := range rows(t, `SELECT left(coalesce(skip_reason,''),40), count(*)
		FROM strategy_runs WHERE status='skipped' GROUP BY 1`) {
		o.SkipReasons[row[0]] = atoi(t, row[1])
	}

	// The ledger must be gapless: each row's sequence is allocated in the same
	// transaction that writes the balance, so a gap means a lost write.
	o.LedgerGaps = psqlInt(t, `
		SELECT count(*) FROM (
			SELECT sequence - lag(sequence) OVER (PARTITION BY account_id ORDER BY sequence) AS d
			FROM transactions
		) g WHERE d IS NOT NULL AND d <> 1`)

	// The running balance each entry recorded must equal the sum of every
	// entry. There is no stored balance column -- the balance IS the ledger --
	// so this compares the last entry's `balance_after` against the sum of
	// `amount`.
	//
	// It is the strongest financial invariant available: it fails if any
	// booking wrote a running balance that its own entries do not explain,
	// which is what a lost or double-counted entry looks like.
	account := psql(t, "SELECT id FROM accounts ORDER BY created_at LIMIT 1")
	o.StoredBalance = psql(t, fmt.Sprintf(`
		SELECT coalesce(round(balance_after,2)::text,'0') FROM transactions
		WHERE account_id = '%s' ORDER BY sequence DESC LIMIT 1`, account))
	o.DerivedBalance = psql(t, fmt.Sprintf(`
		SELECT coalesce(round(sum(amount),2)::text,'0') FROM transactions
		WHERE account_id = '%s'`, account))
	if o.StoredBalance == "" {
		// No entries at all: consistent by definition, and a scenario that
		// traded nothing must not fail this.
		o.StoredBalance, o.DerivedBalance = "0", "0"
	}
	o.BalanceMatches = o.StoredBalance == o.DerivedBalance

	state := psql(t, "SELECT state || '|' || steps || '|' || step_errors FROM replay_runs ORDER BY started_at DESC LIMIT 1")
	if parts := strings.Split(state, "|"); len(parts) == 3 {
		o.RunState = parts[0]
		o.RunSteps = atoi(t, parts[1])
		o.RunErrors = atoi(t, parts[2])
	}
	return o
}

func rows(t *testing.T, sql string) [][]string {
	t.Helper()
	raw := psql(t, sql)
	if raw == "" {
		return nil
	}
	var out [][]string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, strings.Split(line, "|"))
	}
	return out
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		t.Fatalf("not an integer: %q", s)
	}
	return n
}

// since returns what happened BETWEEN two observations.
//
// Every count here is a running total over the whole database, and the
// scenarios share one account deliberately -- an account does not reset
// between market conditions. So an invariant about "this dataset" has to be
// asserted on the DIFFERENCE, or each scenario silently asserts on its
// predecessors' trading and the later ones pass on data they never produced.
//
// Found the hard way: three scenarios agreed on identical numbers because two
// of them had produced nothing at all, and one of those passed.
func (o observed) since(before observed) observed {
	d := observed{
		Orders:          o.Orders - before.Orders,
		Filled:          o.Filled - before.Filled,
		Rejected:        o.Rejected - before.Rejected,
		PartiallyFilled: o.PartiallyFilled - before.PartiallyFilled,
		Decisions:       o.Decisions - before.Decisions,
		NoTrade:         o.NoTrade - before.NoTrade,
		Accepted:        o.Accepted - before.Accepted,
		StrategyRuns:    o.StrategyRuns - before.StrategyRuns,
		NoSignal:        o.NoSignal - before.NoSignal,
		Skipped:         o.Skipped - before.Skipped,
		Fills:           o.Fills - before.Fills,
		Ledger:          o.Ledger - before.Ledger,
		Positions:       o.Positions - before.Positions,

		// These are STATE, not counts, so the latest value is the right one.
		OpenPos:        o.OpenPos,
		RunState:       o.RunState,
		RunSteps:       o.RunSteps,
		RunErrors:      o.RunErrors,
		LedgerGaps:     o.LedgerGaps,
		BalanceMatches: o.BalanceMatches,
		StoredBalance:  o.StoredBalance,
		DerivedBalance: o.DerivedBalance,

		RejectCodes: diffMap(o.RejectCodes, before.RejectCodes),
		DecisionsBy: diffMap(o.DecisionsBy, before.DecisionsBy),
		Regimes:     diffMap(o.Regimes, before.Regimes),
		SkipReasons: diffMap(o.SkipReasons, before.SkipReasons),
	}
	return d
}

func diffMap(after, before map[string]int) map[string]int {
	out := map[string]int{}
	for k, v := range after {
		if n := v - before[k]; n > 0 {
			out[k] = n
		}
	}
	return out
}

// describe renders the observation for a failure message. A scenario that
// fails on one invariant should print enough for the reader to see what the
// platform actually did.
func (o observed) describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "run=%s steps=%d errors=%d\n", o.RunState, o.RunSteps, o.RunErrors)
	fmt.Fprintf(&b, "strategy runs=%d (no_signal=%d skipped=%d)\n",
		o.StrategyRuns, o.NoSignal, o.Skipped)
	for k, v := range o.SkipReasons {
		fmt.Fprintf(&b, "  skip %q x%d\n", k, v)
	}
	fmt.Fprintf(&b, "decisions=%d (accepted=%d no_trade=%d)\n",
		o.Decisions, o.Accepted, o.NoTrade)
	for k, v := range o.Regimes {
		fmt.Fprintf(&b, "  regime %s x%d\n", k, v)
	}
	fmt.Fprintf(&b, "orders=%d (filled=%d partial=%d rejected=%d)\n",
		o.Orders, o.Filled, o.PartiallyFilled, o.Rejected)
	for k, v := range o.RejectCodes {
		fmt.Fprintf(&b, "  reject %s x%d\n", k, v)
	}
	fmt.Fprintf(&b, "fills=%d ledger=%d gaps=%d positions=%d open=%d\n",
		o.Fills, o.Ledger, o.LedgerGaps, o.Positions, o.OpenPos)
	fmt.Fprintf(&b, "balance stored=%s derived=%s matches=%t\n",
		o.StoredBalance, o.DerivedBalance, o.BalanceMatches)
	return b.String()
}

// ---------------------------------------------------------------------------
// Invariants every scenario shares
// ---------------------------------------------------------------------------

// assertUniversalInvariants are true of EVERY run, whatever the dataset does.
//
// They are separated from the per-scenario expectations because they are not
// predictions about a market: they are properties the platform must never
// violate, and a scenario that trades nothing must satisfy them too.
func assertUniversalInvariants(t *testing.T, o observed) {
	t.Helper()

	if o.LedgerGaps != 0 {
		t.Errorf("the ledger has %d sequence gaps: a gap means a booking wrote a "+
			"balance without its entry\n%s", o.LedgerGaps, o.describe())
	}
	if !o.BalanceMatches {
		t.Errorf("the stored balance %s does not equal the ledger-derived %s: some "+
			"booking wrote a balance its own entries do not explain\n%s",
			o.StoredBalance, o.DerivedBalance, o.describe())
	}
	if o.Fills > 0 && o.Ledger == 0 {
		t.Errorf("%d fills produced no ledger entries at all\n%s", o.Fills, o.describe())
	}
	// A filled order must have produced a position. The reverse is not true:
	// a position can be closed and gone.
	if o.Filled > 0 && o.Positions == 0 {
		t.Errorf("%d orders filled and no position was ever created\n%s",
			o.Filled, o.describe())
	}
	if o.RunErrors != 0 {
		t.Errorf("the replay recorded %d step errors\n%s", o.RunErrors, o.describe())
	}
	// Every accepted decision should have an order. A decision recorded as
	// accepted with no order means the OMS accepted and then lost it.
	if o.Accepted > o.Orders {
		t.Errorf("%d decisions were accepted but only %d orders exist\n%s",
			o.Accepted, o.Orders, o.describe())
	}
}
