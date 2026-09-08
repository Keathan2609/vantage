// Package race drives concurrency tests against a RUNNING development stack.
//
// # Why these tests are integration tests and not unit tests
//
// Every race this package is about is arbitrated by PostgreSQL, not by Go:
//
//   - duplicate submission is prevented by PRIMARY KEY (account_id,
//     idempotency_key) on command_idempotency, and by a unique index on
//     orders (account_id, idempotency_key)
//   - the order-state machine is protected by an optimistic version check in
//     an UPDATE ... WHERE version = $n
//   - the ledger is gapless because each row's sequence is allocated inside
//     the same transaction that writes the balance
//
// A unit test with a fake store proves the fake serialises the way the test
// author imagined. It cannot prove the constraint exists, that the UPDATE
// names the right columns, or that two real transactions actually conflict.
// Only two real connections contending for real rows do that.
//
// # Skipping
//
// These tests SKIP unless VANTAGE_RACE_E2E is set, because they need the
// development stack up and they place (paper) orders. They are not part of
// `go test ./...` on a clean checkout.
//
//	$env:VANTAGE_RACE_E2E = "1"
//	$env:VANTAGE_E2E_PASSWORD = "<printed by control-api seed>"
//	go test ./tests/race/ -v -count=1
//
// # Paper only
//
// The stack these run against is paper-mode by construction: live execution is
// not compiled in. Nothing here can reach a real venue, and nothing here
// should ever be pointed at one.
package race

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const defaultAPI = "http://localhost:8080"

// client is an authenticated API session.
type client struct {
	t    *testing.T
	http *http.Client
	base string
	csrf string
}

// requireStack skips the test unless the opt-in flag is set and the stack
// answers readiness. Skipping loudly with a reason beats a failure that looks
// like a defect in the platform.
func requireStack(t *testing.T) string {
	t.Helper()
	if os.Getenv("VANTAGE_RACE_E2E") == "" {
		t.Skip("set VANTAGE_RACE_E2E=1 to run the concurrency suite against a running stack")
	}
	base := os.Getenv("VANTAGE_E2E_API_BASE_URL")
	if base == "" {
		base = defaultAPI
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health/ready", nil)
	if err != nil {
		t.Fatalf("building the readiness request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("no stack at %s (%v); start it with ./scripts/dev-up.ps1", base, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Skipf("the stack at %s is not ready (%d)", base, res.StatusCode)
	}
	return base
}

// signIn authenticates and captures the CSRF token.
//
// The password is never defaulted. A test that guesses a password reports
// "sign-in is broken" when the truth is "you did not set the variable".
func signIn(t *testing.T, base string) *client {
	t.Helper()
	password := os.Getenv("VANTAGE_E2E_PASSWORD")
	if password == "" {
		t.Fatal("VANTAGE_E2E_PASSWORD is not set; it is printed by `control-api seed`")
	}
	email := os.Getenv("VANTAGE_E2E_EMAIL")
	if email == "" {
		email = "trader@vantage.local"
	}

	clearDevRateLimits(t)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	c := &client{
		t:    t,
		http: &http.Client{Jar: jar, Timeout: 30 * time.Second},
		base: base,
	}

	var body struct {
		MFARequired bool `json:"mfa_required"`
	}
	status := c.post("/api/v1/auth/login", "", map[string]string{
		"email": email, "password": password,
	}, &body)
	if status != http.StatusOK {
		t.Fatalf("sign-in as %s failed with %d", email, status)
	}
	if body.MFARequired {
		t.Skip("the development trader has MFA enabled, so this suite cannot sign in unattended")
	}

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parsing the base URL: %v", err)
	}
	for _, cookie := range jar.Cookies(u) {
		if cookie.Name == "vantage_csrf" {
			c.csrf = cookie.Value
		}
	}
	if c.csrf == "" {
		t.Fatal("no vantage_csrf cookie after a successful sign-in")
	}
	return c
}

// post sends a state-changing request. An empty idempotency key omits the
// header, which the API requires for order placement and ignores elsewhere.
func (c *client) post(path, idempotencyKey string, payload any, out any) int {
	c.t.Helper()
	return c.do(http.MethodPost, path, idempotencyKey, payload, out)
}

func (c *client) get(path string, out any) int {
	c.t.Helper()
	return c.do(http.MethodGet, path, "", nil, out)
}

func (c *client) do(method, path, idempotencyKey string, payload any, out any) int {
	c.t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			c.t.Fatalf("encoding the request body: %v", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		c.t.Fatalf("building %s %s: %v", method, path, err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" {
		req.Header.Set("X-Vantage-CSRF", c.csrf)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatalf("reading the response to %s %s: %v", method, path, err)
	}
	if out != nil && len(raw) > 0 {
		// A decode failure is not fatal: an error response has a different
		// shape from a success response, and the caller asserts on the status.
		_ = json.Unmarshal(raw, out)
	}
	return res.StatusCode
}

// concurrentPost fires n identical requests at once and returns their statuses.
//
// The gate is what makes this a race rather than a fast loop: every goroutine
// blocks until the channel closes, so the requests leave together.
func (c *client) concurrentPost(n int, path string, keys []string, payload any) []int {
	c.t.Helper()
	gate := make(chan struct{})
	statuses := make([]int, n)
	done := make(chan struct{}, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			<-gate
			key := ""
			if len(keys) == 1 {
				key = keys[0]
			} else if i < len(keys) {
				key = keys[i]
			}
			statuses[i] = c.do(http.MethodPost, path, key, payload, nil)
		}(i)
	}
	close(gate)
	for i := 0; i < n; i++ {
		<-done
	}
	return statuses
}

// countStatus counts how many statuses equal want.
func countStatus(statuses []int, want int) int {
	n := 0
	for _, s := range statuses {
		if s == want {
			n++
		}
	}
	return n
}

// newKey builds a key that is unique per run, so a rerun is not mistaken for a
// replay of the previous run's order.
func newKey(prefix string) string {
	return fmt.Sprintf("race-%s-%d", prefix, time.Now().UnixNano())
}

// psql runs one SQL statement against the development database and returns the
// single value it produced.
//
// The authoritative assertions in this suite read the DATABASE, not the API.
// An API response is a projection: it can omit a column (the order payload
// does not carry idempotency_key), and reading money back through JSON means
// parsing decimals into float64, which is the wrong tool for asserting that a
// ledger balances. SQL answers both exactly.
//
// PGOPTIONS silences NOTICEs, matching scripts/backup-restore-drill.ps1.
func psql(t *testing.T, sql string) string {
	t.Helper()
	password := os.Getenv("POSTGRES_SUPERUSER_PASSWORD")
	if password == "" {
		password = "vantage_superuser_dev_password"
	}
	cmd := exec.Command("docker", "exec",
		"-e", "PGPASSWORD="+password,
		"-e", "PGOPTIONS=-c client_min_messages=error",
		"vantage-postgres",
		"psql", "-U", "vantage_superuser", "-d", "vantage", "-q", "-tAc", sql)
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("cannot reach the development database to verify (%v); "+
			"the suite needs the vantage-postgres container", err)
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
		t.Fatalf("expected a number from %q, got %q", sql, raw)
	}
	return n
}

// quoteSQL renders a string as a SQL literal. The inputs here are
// test-generated keys and UUIDs from our own API, but doubling quotes costs
// nothing and means a stray apostrophe is a failed assertion rather than a
// syntax error.
func quoteSQL(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// clearDevRateLimits empties the development rate-limit buckets.
//
// The suite deliberately performs more control changes in a minute than a
// human would. The limits are real controls and are NOT widened to suit a
// test; the development cache is cleared instead. A no-op wherever the
// container is unreachable, in which case a genuine 429 still fails loudly.
func clearDevRateLimits(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "exec", "vantage-redis", "sh", "-c",
		"redis-cli --scan --pattern 'vantage:rl:*' | xargs -r redis-cli del > /dev/null; echo cleared")
	_ = cmd.Run()
}

// --- shared fixtures -------------------------------------------------------

type account struct {
	ID string `json:"id"`
}

func (c *client) firstAccount() string {
	c.t.Helper()
	var body struct {
		Accounts []account `json:"accounts"`
	}
	if status := c.get("/api/v1/accounts", &body); status != http.StatusOK {
		c.t.Fatalf("listing accounts returned %d", status)
	}
	if len(body.Accounts) == 0 {
		c.t.Fatal("the signed-in user owns no account")
	}
	return body.Accounts[0].ID
}

type quote struct {
	InstrumentID string `json:"instrument_id"`
	Bid          string `json:"bid"`
	Ask          string `json:"ask"`
}

func (c *client) quote(instrument string) quote {
	c.t.Helper()
	var body struct {
		Quotes []quote `json:"quotes"`
	}
	if status := c.get("/api/v1/market/quotes", &body); status != http.StatusOK {
		c.t.Fatalf("listing quotes returned %d", status)
	}
	for _, q := range body.Quotes {
		if q.InstrumentID == instrument {
			return q
		}
	}
	c.t.Fatalf("no quote for %s", instrument)
	return quote{}
}

// safeStop returns a stop price inside the account's per-trade risk budget.
//
// A hardcoded stop is a trap: gold moves, and a stop that was 0.5% away when
// the test was written is 2% away a month later. The risk engine then
// correctly refuses the order and the test reports it as a platform failure.
func (c *client) safeStop(instrument, side string, fraction float64) string {
	c.t.Helper()
	q := c.quote(instrument)
	raw := q.Bid
	if side == "sell" {
		raw = q.Ask
	}
	reference, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		c.t.Fatalf("parsing the quote %q: %v", raw, err)
	}
	// Presentational arithmetic only: this computes a REQUEST parameter, which
	// the server then parses as a decimal. No authoritative value is derived
	// from this float.
	stop := reference * (1 - fraction)
	if side == "sell" {
		stop = reference * (1 + fraction)
	}
	return strconv.FormatFloat(stop, 'f', 2, 64)
}

// marketOrder is the smallest order the seeded account can afford.
func (c *client) marketOrder(accountID, instrument, side, stop string) map[string]string {
	return map[string]string{
		"account_id":    accountID,
		"instrument_id": instrument,
		"side":          side,
		"type":          "market",
		"quantity":      "0.01",
		"stop_loss":     stop,
		"time_in_force": "gtc",
	}
}

// waitForTradableFeed blocks until the ingestor has published a fresh quote.
//
// Immediately after a reseed the newest quote is stale and the risk engine
// correctly refuses to trade on it. Waiting is how the suite stays
// deterministic without weakening the check.
func (c *client) waitForTradableFeed(instrument string, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	last := "unknown"
	for time.Now().Before(deadline) {
		var body struct {
			Health []struct {
				InstrumentID         string `json:"instrument_id"`
				State                string `json:"state"`
				TradableByAutomation bool   `json:"tradable_by_automation"`
			} `json:"health"`
		}
		c.get("/api/v1/market/health", &body)
		for _, h := range body.Health {
			if h.InstrumentID != instrument {
				continue
			}
			last = h.State
			if h.TradableByAutomation {
				return
			}
		}
		time.Sleep(time.Second)
	}
	c.t.Skipf("market data for %s never became tradable (last state: %s)", instrument, last)
}

// flattenAll closes every open position.
//
// Without this, positions accumulate across runs until the per-instrument
// exposure ceiling binds and every later order is refused — correctly, for a
// reason unrelated to the test.
func (c *client) flattenAll(accountID string) {
	c.t.Helper()
	var body struct {
		Positions []struct {
			ID string `json:"id"`
		} `json:"positions"`
	}
	c.get("/api/v1/positions?account_id="+accountID, &body)
	for _, p := range body.Positions {
		c.post("/api/v1/positions/"+p.ID+"/flatten", newKey("flatten"),
			map[string]bool{"confirm": true}, nil)
	}
}

// cancelAllWorking cancels every order still sitting at the venue.
//
// The cancel-race test parks a limit order 20% below the market so it stays
// working, and a run that fails before cancelling it leaves it there. Those
// accumulate: after a few runs the account holds more pending orders than the
// max_pending_orders limit allows, and every later order in the suite is
// refused with "8 pending orders against a limit of 5".
//
// The engine was right and the fixture was dirty. Same lesson as flattenAll.
func (c *client) cancelAllWorking(accountID string) {
	c.t.Helper()
	var body struct {
		Orders []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"orders"`
	}
	c.get("/api/v1/orders?account_id="+accountID+"&limit=200", &body)
	for _, o := range body.Orders {
		// The live states, taken from domain.OrderStatus rather than guessed
		// at. The first version of this list omitted ACCEPTED, which is the
		// state a resting limit order actually sits in, so it cancelled
		// nothing and the pending-order limit kept binding.
		switch strings.ToUpper(o.Status) {
		case "CREATED", "VALIDATING", "ACCEPTED", "SUBMITTED", "PARTIALLY_FILLED":
			c.post("/api/v1/orders/"+o.ID+"/cancel", "", nil, nil)
		}
	}
}

// releaseAllKillSwitches clears every active switch, so one test cannot poison
// the next.
func (c *client) releaseAllKillSwitches() {
	c.t.Helper()
	var body struct {
		KillSwitches []struct {
			ID     string `json:"ID"`
			Active bool   `json:"Active"`
		} `json:"kill_switches"`
	}
	c.get("/api/v1/kill-switches?active=true", &body)
	for _, sw := range body.KillSwitches {
		if !sw.Active {
			continue
		}
		c.post("/api/v1/kill-switches/"+sw.ID+"/deactivate", "", nil, nil)
	}
}

// grantSeedAuthority re-grants the mandate the seed creates.
//
// It mirrors internal/seed/seed.go rather than inventing limits, so a test
// that revokes an authority leaves the stack in the state the next test
// expects. Every field is required: the handler refuses an authority with an
// empty instrument or order-type allow-list, on the grounds that a mandate
// permitting nothing is confusing rather than safe.
func (c *client) grantSeedAuthority(accountID string) {
	c.t.Helper()
	var listed struct {
		Strategies []struct {
			ID string `json:"id"`
		} `json:"strategies"`
	}
	c.get("/api/v1/strategies", &listed)
	ids := make([]string, 0, len(listed.Strategies))
	for _, s := range listed.Strategies {
		ids = append(ids, s.ID)
	}

	status := c.post("/api/v1/authority", "", map[string]any{
		"account_id":            accountID,
		"automation_enabled":    true,
		"allowed_instruments":   []string{"XAUUSD.m", "XAUUSD"},
		"allowed_strategy_ids":  ids,
		"allowed_order_types":   []string{"market", "limit"},
		"max_order_quantity":    "0.10",
		"max_order_notional":    "2500.00",
		"max_position_exposure": "2500.00",
		"max_leverage":          "10",
		"max_daily_loss":        "15.00",
	}, nil)
	if status != http.StatusCreated && status != http.StatusOK {
		// Loud, because leaving the authority revoked makes every later test
		// in the suite fail with a 403 that has nothing to do with it.
		c.t.Errorf("could not restore the trading authority (status %d); "+
			"later tests will be refused until it is granted again", status)
	}
}

// armFault arms a deterministic venue fault. Development-only endpoint.
func (c *client) armFault(fault string, times int, extra map[string]any) {
	c.t.Helper()
	payload := map[string]any{"fault": fault, "times": times}
	for k, v := range extra {
		payload[k] = v
	}
	if status := c.post("/api/v1/dev/broker-faults", "", payload, nil); status != http.StatusOK &&
		status != http.StatusCreated && status != http.StatusAccepted {
		c.t.Skipf("could not arm the %s fault (%d); the mock venue may not be loaded", fault, status)
	}
}

func (c *client) resetFaults() {
	c.t.Helper()
	c.post("/api/v1/dev/broker-faults/reset", "", nil, nil)
}
