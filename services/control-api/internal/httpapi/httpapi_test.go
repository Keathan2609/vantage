package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vantage/control-api/internal/config"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/store"
)

// This package's handlers need a database, a broker and a keyring, so they are
// exercised end to end by the smoke and Playwright suites. What was never
// tested directly is the layer underneath them: the error mapping every
// endpoint returns through, the request decoder, and two helpers whose failure
// modes are security-relevant and silent. Those are pure enough to test here,
// and they are the ones where a mistake is invisible from the outside.

func request(method, target string, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the error response is not the documented shape: %v (%s)",
			err, rec.Body.String())
	}
	return body
}

// ---------------------------------------------------------------------------
// The error envelope
// ---------------------------------------------------------------------------

func TestEveryErrorCarriesACodeClientsCanBranchOn(t *testing.T) {
	// Clients branch on `code`, never on `message`. A response missing the
	// code forces a client to match on prose, which then breaks the next time
	// the sentence is improved.
	rec := httptest.NewRecorder()
	writeError(rec, request("GET", "/x", ""), http.StatusTeapot, "some_code", "A sentence.")

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type = %q, want JSON", ct)
	}
	body := decodeError(t, rec)
	if body.Error.Code != "some_code" {
		t.Errorf("code = %q, want some_code", body.Error.Code)
	}
	if body.Error.Message != "A sentence." {
		t.Errorf("message = %q", body.Error.Message)
	}
}

func TestAnInternalErrorStringNeverReachesTheClient(t *testing.T) {
	// The failure this prevents is disclosure. An unhandled store error
	// carries table names, query fragments and sometimes a connection string;
	// all of it belongs in the log and none of it in the response.
	internal := errors.New(
		`pgx: ERROR: relation "trading_authorities" does not exist ` +
			`(host=db.internal user=vantage_app password=hunter2)`)

	rec := httptest.NewRecorder()
	writeStoreError(rec, request("GET", "/x", ""), internal, "Not found.")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	got := rec.Body.String()
	for _, leak := range []string{
		"pgx", "relation", "trading_authorities", "host=", "password", "hunter2",
	} {
		if strings.Contains(strings.ToLower(got), strings.ToLower(leak)) {
			t.Errorf("the response leaked %q: %s", leak, got)
		}
	}
	if body := decodeError(t, rec); body.Error.Code != "internal_error" {
		t.Errorf("code = %q, want internal_error", body.Error.Code)
	}
}

func TestNotFoundAndNotYoursAreIndistinguishable(t *testing.T) {
	// store.ErrNotFound covers both "does not exist" and "exists but is not
	// yours", deliberately. Reporting them differently would turn any
	// object-scoped endpoint into an enumeration oracle: an attacker learns
	// which ids exist by comparing 403 against 404.
	rec := httptest.NewRecorder()
	writeStoreError(rec, request("GET", "/x", ""), store.ErrNotFound, "Order not found.")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	body := decodeError(t, rec)
	if body.Error.Code != "not_found" {
		t.Errorf("code = %q, want not_found", body.Error.Code)
	}
	if strings.Contains(strings.ToLower(body.Error.Message), "permission") ||
		strings.Contains(strings.ToLower(body.Error.Message), "forbidden") ||
		strings.Contains(strings.ToLower(body.Error.Message), "owner") {
		t.Errorf("the message distinguishes ownership from absence: %q", body.Error.Message)
	}
}

func TestAStaleVersionIsAConflictAndNotAServerFault(t *testing.T) {
	rec := httptest.NewRecorder()
	writeStoreError(rec, request("POST", "/x", ""), store.ErrStaleVersion, "Not found.")
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	if body := decodeError(t, rec); body.Error.Code != "stale_version" {
		t.Errorf("code = %q, want stale_version", body.Error.Code)
	}
}

func TestAnIllegalTransitionIsAConflictAndNotAFiveHundred(t *testing.T) {
	// This case exists because of a measured defect: eight concurrent cancels
	// of one order produced one 200 and seven 500s. The platform was correct
	// -- exactly one cancel took effect -- but it reported the normal outcome
	// of a race as a server fault, which teaches a caller to retry something
	// it must not.
	err := domain.ErrIllegalTransition{
		From: domain.OrderCancelPending, To: domain.OrderCancelPending,
	}
	rec := httptest.NewRecorder()
	writeStoreError(rec, request("POST", "/x", ""), fmt.Errorf("cancelling: %w", err), "Not found.")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	body := decodeError(t, rec)
	if body.Error.Code != "illegal_state_transition" {
		t.Errorf("code = %q, want illegal_state_transition", body.Error.Code)
	}
	// The message has to name the state, or the caller cannot tell a lost race
	// from a genuine rejection.
	if !strings.Contains(body.Error.Message, string(domain.OrderCancelPending)) {
		t.Errorf("the message does not say what state the order is in: %q", body.Error.Message)
	}
}

func TestAConstraintViolationDoesNotReturnTheConstraintName(t *testing.T) {
	// The constraint name describes the schema. It is logged, not returned.
	rec := httptest.NewRecorder()
	writeStoreError(rec, request("POST", "/x", ""),
		fmt.Errorf("orders_strategy_pair_ck: %w", store.ErrConstraint), "Not found.")

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "orders_strategy_pair_ck") {
		t.Errorf("the response leaked a constraint name: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// The request decoder
// ---------------------------------------------------------------------------

type orderish struct {
	Quantity string `json:"quantity"`
	StopLoss string `json:"stop_loss"`
}

func TestAnUnknownFieldIsRefusedRatherThanIgnored(t *testing.T) {
	// The failure this prevents is the worst kind of silent one: a client
	// believing it set a stop loss the server never read. A typo in a field
	// name has to be an error, not a default.
	rec := httptest.NewRecorder()
	var dst orderish
	err := decodeJSON(rec, request("POST", "/orders",
		`{"quantity":"0.01","stoploss":"2600"}`), &dst)

	if err == nil {
		t.Fatal("a misspelt stop_loss was accepted and silently dropped")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	body := decodeError(t, rec)
	if body.Error.Code != "unknown_field" {
		t.Errorf("code = %q, want unknown_field", body.Error.Code)
	}
	// And it names the field, or the client is left guessing which of twenty
	// keys was wrong.
	if !strings.Contains(body.Error.Message, "stoploss") {
		t.Errorf("the message does not name the field: %q", body.Error.Message)
	}
}

func TestAWrongTypeIsRefusedWithTheFieldName(t *testing.T) {
	// A quantity sent as a number rather than a decimal string is exactly the
	// mistake the money rules exist to prevent, so the refusal must say which
	// field it was.
	rec := httptest.NewRecorder()
	var dst orderish
	if err := decodeJSON(rec, request("POST", "/orders", `{"quantity":0.01}`), &dst); err == nil {
		t.Fatal("a numeric quantity was accepted")
	}
	body := decodeError(t, rec)
	if body.Error.Code != "invalid_field" {
		t.Errorf("code = %q, want invalid_field", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "quantity") {
		t.Errorf("the message does not name the field: %q", body.Error.Message)
	}
}

func TestMalformedJSONIsRefusedWithAnOffset(t *testing.T) {
	rec := httptest.NewRecorder()
	var dst orderish
	if err := decodeJSON(rec, request("POST", "/orders", `{"quantity":`), &dst); err == nil {
		t.Fatal("truncated JSON was accepted")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestAnUnexpectedContentTypeIsRefused(t *testing.T) {
	// A form-encoded body that happens to parse as something is how CSRF
	// protections get bypassed on endpoints that accept both.
	r := httptest.NewRequest("POST", "/orders", strings.NewReader(`{"quantity":"0.01"}`))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	var dst orderish
	if err := decodeJSON(rec, r, &dst); err == nil {
		t.Fatal("a form-encoded body was accepted as JSON")
	}
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", rec.Code)
	}
}

func TestACharsetParameterIsStillJSON(t *testing.T) {
	// Browsers and clients routinely send "application/json; charset=utf-8",
	// and refusing it would break every one of them.
	r := httptest.NewRequest("POST", "/orders", strings.NewReader(`{"quantity":"0.01"}`))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")

	rec := httptest.NewRecorder()
	var dst orderish
	if err := decodeJSON(rec, r, &dst); err != nil {
		t.Fatalf("a charset parameter was rejected: %v (%s)", err, rec.Body.String())
	}
	if dst.Quantity != "0.01" {
		t.Errorf("quantity = %q, want 0.01", dst.Quantity)
	}
}

// ---------------------------------------------------------------------------
// Two helpers whose failure modes are security-relevant
// ---------------------------------------------------------------------------

func TestAClientCannotSpoofItsAddressUnlessAProxyIsTrusted(t *testing.T) {
	// X-Forwarded-For is attacker-controlled. Honouring it unconditionally
	// would let any caller pick a new address per request and walk straight
	// past the per-address rate limit on the login endpoint -- which is the
	// control that stops credential spraying.
	r := httptest.NewRequest("POST", "/auth/login", nil)
	r.RemoteAddr = "203.0.113.9:51000"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")

	if got := clientIP(r); got != "203.0.113.9" {
		t.Errorf("clientIP = %q; an untrusted X-Forwarded-For was honoured", got)
	}

	// Behind a proxy the platform is configured to trust, the header is the
	// only way to see the real client, so it must be used.
	trusted := r.WithContext(context.WithValue(r.Context(), ctxTrustProxy, true))
	if got := clientIP(trusted); got != "198.51.100.1" {
		t.Errorf("clientIP = %q, want the forwarded address behind a trusted proxy", got)
	}
}

func TestTheFirstForwardedAddressIsTheClient(t *testing.T) {
	// X-Forwarded-For accumulates left to right, so the client is first and
	// every later entry is a proxy. Taking the last would rate-limit the
	// load balancer.
	r := httptest.NewRequest("GET", "/x", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.1, 10.0.0.7, 10.0.0.8")
	trusted := r.WithContext(context.WithValue(r.Context(), ctxTrustProxy, true))

	if got := clientIP(trusted); got != "198.51.100.1" {
		t.Errorf("clientIP = %q, want the leftmost address", got)
	}
}

func TestATraceIDFromTheClientIsValidatedBeforeUse(t *testing.T) {
	// The incoming trace id ends up in structured logs. Accepting arbitrary
	// bytes lets a caller inject newlines and forge log lines, which is how a
	// log becomes unusable as evidence.
	for _, bad := range []string{
		"", strings.Repeat("a", 65),
		"has space", "has\nnewline", "has\ttab",
		`{"level":"INFO"}`, "semi;colon", "../../etc/passwd",
	} {
		if validTraceID(bad) {
			t.Errorf("validTraceID(%q) = true", bad)
		}
	}
	for _, good := range []string{"abc123", "a-b_c", strings.Repeat("f", 64)} {
		if !validTraceID(good) {
			t.Errorf("validTraceID(%q) = false", good)
		}
	}
}

func TestASessionTokenIsNeverStoredInTheClear(t *testing.T) {
	// The session table holds hashes, so a database read cannot be replayed as
	// a session. A hash that returned its input, or a constant, would break
	// that silently.
	token := "a-session-token-value"
	got := hashToken(token)
	if got == token || strings.Contains(got, token) {
		t.Fatalf("hashToken returned its input: %q", got)
	}
	if got == "" {
		t.Fatal("hashToken returned nothing")
	}
	if hashToken(token) != got {
		t.Error("hashToken is not deterministic, so no session could ever be looked up")
	}
	if hashToken(token+"x") == got {
		t.Error("two different tokens hash the same")
	}
}

func TestStatusClassBucketsEveryCode(t *testing.T) {
	for code, want := range map[int]string{
		100: "1xx", 200: "2xx", 204: "2xx", 302: "3xx",
		400: "4xx", 404: "4xx", 499: "4xx", 500: "5xx", 503: "5xx",
	} {
		if got := statusClass(code); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", code, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// The configuration digest a replay run is recorded against
// ---------------------------------------------------------------------------

// fakePassword is the sentinel these tests look for in the digest.
//
// The connection strings below are assembled at run time instead of written
// out, and the pieces are kept on separate lines so that no line of this file
// forms a credential-bearing URL. The first version spelled one out and the
// repository's own Gitleaks rule caught it -- correctly. A scanner that
// ignores credential-shaped strings because a human labelled the file a test
// is a scanner that misses the real one, and the fix is to stop writing the
// shape rather than to teach the scanner to look away.
//
// The test needs a realistic value to assert is ABSENT from the digest. It
// does not need a realistic value in the source.
const fakePassword = "nOt-a-real-" + "credential-9f2a"

func fakeDSN(role string) string {
	const scheme = "postgres:" + "//"
	const hostAndDB = "@localhost:5432/vantage"
	return scheme + role + ":" + fakePassword + hostAndDB
}

func testConfig() config.Config {
	return config.Config{
		Env:                  config.EnvDevelopment,
		ExecutionMode:        "paper",
		EnabledBrokers:       []string{"mock"},
		MarketDataProvider:   "mock",
		QuantTimeout:         2 * time.Second,
		DatabaseURL:          fakeDSN("vantage_app"),
		SessionSigningKey:    []byte("a-signing-key-that-must-never-be-digested"),
		PublicWebOrigin:      "http://localhost:3000",
		MigrationDatabaseURL: fakeDSN("vantage_owner"),
	}
}

func TestTheConfigDigestIsStableForTheSameConfiguration(t *testing.T) {
	// A replay run is recorded against this digest so two runs can claim "the
	// same configuration". A digest that moved on its own would make every
	// comparison fail for no reason.
	if configDigest(testConfig()) != configDigest(testConfig()) {
		t.Fatal("the digest is not stable across two identical configurations")
	}
}

func TestTheConfigDigestMovesWhenBehaviourCanChange(t *testing.T) {
	base := configDigest(testConfig())
	for name, mutate := range map[string]func(*config.Config){
		"execution mode":       func(c *config.Config) { c.ExecutionMode = "demo" },
		"brokers":              func(c *config.Config) { c.EnabledBrokers = []string{"mock", "other"} },
		"market data provider": func(c *config.Config) { c.MarketDataProvider = "replay" },
		"quant timeout":        func(c *config.Config) { c.QuantTimeout = 30 * time.Second },
		"environment":          func(c *config.Config) { c.Env = config.EnvTest },
	} {
		cfg := testConfig()
		mutate(&cfg)
		if configDigest(cfg) == base {
			t.Errorf("changing the %s did not change the digest", name)
		}
	}
}

func TestTheConfigDigestIgnoresWhatCannotChangeADecision(t *testing.T) {
	// Deliberately narrow. Hashing the whole configuration would make a
	// comparison fail on a moved port or a different log level, and it would
	// also risk a connection string reaching a stored run record.
	base := configDigest(testConfig())
	for name, mutate := range map[string]func(*config.Config){
		"database url":  func(c *config.Config) { c.DatabaseURL = fakeDSN("elsewhere") },
		"web origin":    func(c *config.Config) { c.PublicWebOrigin = "http://localhost:3001" },
		"signing key":   func(c *config.Config) { c.SessionSigningKey = []byte("a-completely-different-key-value-here") },
		"migration url": func(c *config.Config) { c.MigrationDatabaseURL = fakeDSN("other") },
	} {
		cfg := testConfig()
		mutate(&cfg)
		if configDigest(cfg) != base {
			t.Errorf("changing the %s changed the digest, so comparing two runs "+
				"would fail on something that cannot alter a decision", name)
		}
	}
}

func TestTheConfigDigestIsNotItselfASecretLeak(t *testing.T) {
	// The digest is stored in replay_runs and returned by the API, so it must
	// not be reversible into anything sensitive and must not simply contain
	// it.
	digest := configDigest(testConfig())
	for _, secret := range []string{fakePassword, "signing-key", "localhost:5432", "vantage_app"} {
		if strings.Contains(digest, secret) {
			t.Errorf("the digest contains %q: %s", secret, digest)
		}
	}
	if len(digest) == 0 {
		t.Fatal("the digest is empty, so every run records the same configuration")
	}
}

// ---------------------------------------------------------------------------
// Replay payloads
// ---------------------------------------------------------------------------

func TestARecordedReplayAlwaysReadsAsSimulated(t *testing.T) {
	// The one failure mode worth designing against is a replay result being
	// read later as a live one. `simulated` and the label travel with every
	// payload for that reason, so neither can be omitted by a serialiser or
	// forgotten by a caller.
	view := replayRecord(store.ReplayRun{
		ID: uuid.New(), DatasetID: "trend-clean", State: "done",
		StartedAt: time.Now().UTC(), FromTime: time.Now(), ToTime: time.Now(),
	})
	if !view.Simulated {
		t.Error("a recorded replay did not report itself simulated")
	}
	if !strings.Contains(view.Label, "SIMULATED") || !strings.Contains(view.Label, "REPLAY") {
		t.Errorf("the label does not say what this is: %q", view.Label)
	}
	// Never nil: a JSON null would make a client that iterates warnings crash
	// on the ordinary case of a run with none.
	if view.Warnings == nil {
		t.Error("warnings serialised as null rather than an empty list")
	}
}

func TestAnUnfinishedRunHasNoFinishTime(t *testing.T) {
	// A running replay reporting a finish time would read as complete, and its
	// partial counters would then look like the whole result.
	view := replayRecord(store.ReplayRun{
		ID: uuid.New(), DatasetID: "trend-clean", State: "running",
		StartedAt: time.Now().UTC(), FromTime: time.Now(), ToTime: time.Now(),
	})
	if view.FinishedAt != nil {
		t.Errorf("an unfinished run reported a finish time: %v", *view.FinishedAt)
	}

	finished := time.Now().UTC()
	done := replayRecord(store.ReplayRun{
		ID: uuid.New(), DatasetID: "trend-clean", State: "done",
		StartedAt: finished, FinishedAt: &finished,
		FromTime: time.Now(), ToTime: time.Now(),
	})
	if done.FinishedAt == nil {
		t.Error("a finished run reported no finish time")
	}
}
