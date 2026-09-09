package quant

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// The research service is the one dependency the control plane cannot verify
// the correctness of: it returns opinions. So the client's job is to make sure
// an opinion is well-formed before it can influence an order, and to stop
// asking a service that is failing.
//
// Every case here turns into NO TRADE upstream, because the orchestrator
// treats any error from this client as a reason not to trade. That is why a
// permissive response validator would be a trading defect rather than a
// robustness nit.

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func ptr(d decimal.Decimal) *decimal.Decimal { return &d }

// jsonServer answers every request with one status and body.
func jsonServer(t *testing.T, status int, body string) (*httptest.Server, *int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

const goodSignal = `{"action":"buy","confidence":"0.75","explanation":"trend up",
	"indicators":{"adx":"31"},"features":{}}`

// ---------------------------------------------------------------------------
// Response validation: what a malformed opinion must not be allowed to do
// ---------------------------------------------------------------------------

func TestAWellFormedSignalIsAccepted(t *testing.T) {
	srv, _ := jsonServer(t, 200, goodSignal)
	c := New(srv.URL, "token", 2*time.Second)

	resp, err := c.Signal(context.Background(), SignalRequest{StrategyKey: "x"})
	if err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if resp.Action != "buy" || !resp.Confidence.Equal(dec("0.75")) {
		t.Errorf("got action=%q confidence=%s", resp.Action, resp.Confidence)
	}
}

func TestAnUnknownActionIsRejected(t *testing.T) {
	// The orchestrator switches on this string to derive a side. An
	// unrecognised value must not reach it: "liquidate_everything" is not an
	// action, and treating an unknown as a default would pick a side.
	for _, action := range []string{"", "BUY", "long", "liquidate", "no-trade"} {
		srv, _ := jsonServer(t, 200, `{"action":"`+action+`","confidence":"0.5"}`)
		c := New(srv.URL, "", time.Second)

		_, err := c.Signal(context.Background(), SignalRequest{})
		if !errors.Is(err, ErrBadResponse) {
			t.Errorf("action %q = %v, want ErrBadResponse", action, err)
		}
	}
}

func TestEveryValidActionIsAllowedThrough(t *testing.T) {
	// hold, close and no_trade are as important as buy and sell: they are how
	// a strategy says "I looked and decided not to", which is recorded.
	for _, action := range []string{"buy", "sell", "hold", "close", "no_trade"} {
		srv, _ := jsonServer(t, 200, `{"action":"`+action+`","confidence":"0.5"}`)
		c := New(srv.URL, "", time.Second)

		if _, err := c.Signal(context.Background(), SignalRequest{}); err != nil {
			t.Errorf("action %q was rejected: %v", action, err)
		}
	}
}

func TestAConfidenceOutsideZeroToOneIsRejected(t *testing.T) {
	// Confidence feeds the consensus policy and the operator display. A value
	// of 8.2 would outvote everything; a negative one would invert a veto.
	for _, conf := range []string{"-0.01", "1.01", "8.2", "-1"} {
		srv, _ := jsonServer(t, 200, `{"action":"buy","confidence":"`+conf+`"}`)
		c := New(srv.URL, "", time.Second)

		_, err := c.Signal(context.Background(), SignalRequest{})
		if !errors.Is(err, ErrBadResponse) {
			t.Errorf("confidence %s = %v, want ErrBadResponse", conf, err)
		}
	}
	// The boundaries themselves are legitimate.
	for _, conf := range []string{"0", "1", "0.5"} {
		srv, _ := jsonServer(t, 200, `{"action":"buy","confidence":"`+conf+`"}`)
		c := New(srv.URL, "", time.Second)
		if _, err := c.Signal(context.Background(), SignalRequest{}); err != nil {
			t.Errorf("confidence %s was rejected: %v", conf, err)
		}
	}
}

func TestANonPositiveStopOrTargetIsRejected(t *testing.T) {
	// A zero or negative stop would be sized against, producing either a
	// division by zero or an enormous position. Refusing here is the only
	// place that catches a research bug before it becomes an order.
	cases := []struct {
		name string
		body string
	}{
		{"zero stop", `{"action":"buy","confidence":"0.6","suggested_stop":"0"}`},
		{"negative stop", `{"action":"buy","confidence":"0.6","suggested_stop":"-2600"}`},
		{"zero target", `{"action":"buy","confidence":"0.6","suggested_target":"0"}`},
		{"negative target", `{"action":"buy","confidence":"0.6","suggested_target":"-1"}`},
	}
	for _, c2 := range cases {
		srv, _ := jsonServer(t, 200, c2.body)
		c := New(srv.URL, "", time.Second)
		if _, err := c.Signal(context.Background(), SignalRequest{}); !errors.Is(err, ErrBadResponse) {
			t.Errorf("%s = %v, want ErrBadResponse", c2.name, err)
		}
	}
}

func TestAnAbsentStopIsNotTheSameAsAZeroStop(t *testing.T) {
	// A strategy that offers no stop is legitimate: the account's own policy
	// decides whether one is required. Only a PRESENT but invalid stop is an
	// error, which is why the field is a pointer.
	srv, _ := jsonServer(t, 200, `{"action":"buy","confidence":"0.6"}`)
	c := New(srv.URL, "", time.Second)

	resp, err := c.Signal(context.Background(), SignalRequest{})
	if err != nil {
		t.Fatalf("a signal with no stop was rejected: %v", err)
	}
	if resp.SuggestedStop != nil {
		t.Errorf("suggested stop = %v, want nil", resp.SuggestedStop)
	}
}

func TestAnImplausiblyLongExplanationIsRejected(t *testing.T) {
	// The explanation is stored on the signal and rendered to an operator. An
	// unbounded field from an upstream service is a way to fill the database
	// and break the interface.
	long := strings.Repeat("a", 2001)
	srv, _ := jsonServer(t, 200, `{"action":"buy","confidence":"0.6","explanation":"`+long+`"}`)
	c := New(srv.URL, "", time.Second)

	if _, err := c.Signal(context.Background(), SignalRequest{}); !errors.Is(err, ErrBadResponse) {
		t.Errorf("a 2001-character explanation = %v, want ErrBadResponse", err)
	}
}

func TestUnparseableJSONIsABadResponseNotASignal(t *testing.T) {
	srv, _ := jsonServer(t, 200, `{"action":"buy","confidence":`)
	c := New(srv.URL, "", time.Second)

	if _, err := c.Signal(context.Background(), SignalRequest{}); !errors.Is(err, ErrBadResponse) {
		t.Errorf("truncated JSON = %v, want ErrBadResponse", err)
	}
}

func TestValidateIsCalledOnTheResponseNotJustTheTransport(t *testing.T) {
	// A 200 with a nonsense body is the dangerous case: the transport
	// succeeded, so nothing else would object.
	srv, _ := jsonServer(t, 200, `{"action":"buy","confidence":"99"}`)
	c := New(srv.URL, "", time.Second)

	resp, err := c.Signal(context.Background(), SignalRequest{})
	if err == nil {
		t.Fatal("a 200 response with an out-of-range confidence was accepted")
	}
	if resp.Action != "" {
		t.Errorf("a rejected response still returned a usable action %q", resp.Action)
	}
}

// ---------------------------------------------------------------------------
// Transport failures
// ---------------------------------------------------------------------------

func TestAServerErrorIsUnavailableAndCountsAgainstTheBreaker(t *testing.T) {
	srv, calls := jsonServer(t, 503, `{"detail":"down"}`)
	c := New(srv.URL, "", time.Second)

	if _, err := c.Signal(context.Background(), SignalRequest{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("503 = %v, want ErrUnavailable", err)
	}
	if *calls != 1 {
		t.Errorf("calls = %d, want 1", *calls)
	}
}

func TestAClientErrorDoesNotCountAgainstTheBreaker(t *testing.T) {
	// A 4xx means the control plane sent something wrong. Tripping the breaker
	// on our own bug would stop every OTHER strategy from being evaluated,
	// which turns one bad request into a platform-wide outage.
	srv, calls := jsonServer(t, 422, `{"detail":"unknown strategy"}`)
	c := New(srv.URL, "", time.Second)

	for k := 0; k < 10; k++ {
		if _, err := c.Signal(context.Background(), SignalRequest{}); !errors.Is(err, ErrRequestFailed) {
			t.Fatalf("call %d: 422 = %v, want ErrRequestFailed", k, err)
		}
	}
	if *calls != 10 {
		t.Errorf("calls = %d after ten 4xx responses, want 10 (the breaker tripped)", *calls)
	}
}

func TestTheBreakerOpensAfterRepeatedFailuresAndStopsCalling(t *testing.T) {
	srv, calls := jsonServer(t, 500, `{}`)
	c := New(srv.URL, "", time.Second)

	// The threshold is five. The sixth call must not reach the network.
	for k := 0; k < 5; k++ {
		if _, err := c.Signal(context.Background(), SignalRequest{}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("call %d = %v, want ErrUnavailable", k, err)
		}
	}
	before := *calls

	_, err := c.Signal(context.Background(), SignalRequest{})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("the call after the threshold = %v, want ErrCircuitOpen", err)
	}
	if *calls != before {
		t.Errorf("a request was sent while the breaker was open (%d -> %d)", before, *calls)
	}
}

func TestAnOpenBreakerIsStillAFailureForTheCaller(t *testing.T) {
	// ErrCircuitOpen must not be mistaken for "no signal": both mean the
	// orchestrator cannot get an opinion and must not trade. What matters is
	// that it is an error at all, and a distinguishable one.
	srv, _ := jsonServer(t, 500, `{}`)
	c := New(srv.URL, "", time.Second)
	for k := 0; k < 5; k++ {
		_, _ = c.Signal(context.Background(), SignalRequest{})
	}

	resp, err := c.Signal(context.Background(), SignalRequest{})
	if err == nil {
		t.Fatal("an open breaker returned no error")
	}
	if resp.Action != "" {
		t.Errorf("an open breaker returned an action %q", resp.Action)
	}
}

func TestASuccessResetsTheFailureCount(t *testing.T) {
	// Four failures then a success must not leave the breaker one failure from
	// opening: a service that fails occasionally is not a service that is down.
	var mu sync.Mutex
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		shouldFail := fail
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if shouldFail {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(goodSignal))
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)

	for k := 0; k < 4; k++ {
		_, _ = c.Signal(context.Background(), SignalRequest{})
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if _, err := c.Signal(context.Background(), SignalRequest{}); err != nil {
		t.Fatalf("the recovering call failed: %v", err)
	}

	mu.Lock()
	fail = true
	mu.Unlock()
	// Four more failures must again be tolerated without opening.
	for k := 0; k < 4; k++ {
		if _, err := c.Signal(context.Background(), SignalRequest{}); errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("the breaker opened on failure %d after a success reset it", k)
		}
	}
}

func TestTheBreakerAlertsOnOpenAndOnRecoveryOnly(t *testing.T) {
	alerter := &recordingQuantAlerter{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)
	c.SetAlerter(alerter)

	for k := 0; k < 5; k++ {
		_, _ = c.Signal(context.Background(), SignalRequest{})
	}
	if got := alerter.failures(); got != 1 {
		t.Errorf("failure alerts = %d after five failures, want 1 (only the open transition)", got)
	}
}

func TestATimeoutIsUnavailableRatherThanHanging(t *testing.T) {
	// The scheduler runs strategies on an interval. A research service that
	// accepts a connection and never answers must not hold the run open.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(goodSignal))
	}))
	defer srv.Close()
	c := New(srv.URL, "", 50*time.Millisecond)

	start := time.Now()
	_, err := c.Signal(context.Background(), SignalRequest{})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a timeout = %v, want ErrUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Errorf("the call took %s; the client timeout was not applied", elapsed)
	}
}

func TestACancelledContextStopsTheCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(goodSignal))
	}))
	defer srv.Close()
	c := New(srv.URL, "", 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := c.Signal(ctx, SignalRequest{}); err == nil {
		t.Fatal("a cancelled context still produced a signal")
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	// A research service that starts redirecting is a research service that
	// has been tampered with, and following it would send the service token
	// somewhere new.
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(goodSignal))
	}))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v1/strategies/signal", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	c := New(srv.URL, "secret", time.Second)
	if _, err := c.Signal(context.Background(), SignalRequest{}); err == nil {
		t.Fatal("the client followed a redirect")
	}
}

func TestTheServiceTokenIsSentAndNothingElseIs(t *testing.T) {
	var mu sync.Mutex
	var gotToken, gotCookie, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotToken = r.Header.Get("X-Vantage-Service-Token")
		gotCookie = r.Header.Get("Cookie")
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(goodSignal))
	}))
	defer srv.Close()

	c := New(srv.URL, "service-token", time.Second)
	if _, err := c.Signal(context.Background(), SignalRequest{}); err != nil {
		t.Fatalf("Signal: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotToken != "service-token" {
		t.Errorf("service token header = %q", gotToken)
	}
	// Research must never receive an operator's session. It authenticates the
	// control plane to research and grants research nothing in return.
	if gotCookie != "" || gotAuth != "" {
		t.Errorf("the client forwarded credentials it should not: cookie=%q auth=%q",
			gotCookie, gotAuth)
	}
}

func TestNoTokenMeansNoTokenHeader(t *testing.T) {
	var mu sync.Mutex
	present := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_, present = r.Header["X-Vantage-Service-Token"]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(goodSignal))
	}))
	defer srv.Close()

	c := New(srv.URL, "", time.Second)
	if _, err := c.Signal(context.Background(), SignalRequest{}); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if present {
		t.Error("an empty token was sent as an empty header, which reads as an " +
			"authenticated request carrying no credential")
	}
}

func TestAnUnreachableServiceIsUnavailable(t *testing.T) {
	// Closed immediately, so the port is refused rather than slow.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := New(url, "", 500*time.Millisecond)
	if _, err := c.Signal(context.Background(), SignalRequest{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an unreachable service = %v, want ErrUnavailable", err)
	}
}

func TestConcurrentCallsDoNotRaceTheBreaker(t *testing.T) {
	// The scheduler evaluates strategies in sequence today, but the client is
	// shared and holds a mutex so that is not load-bearing. Meaningful under
	// -race.
	srv, _ := jsonServer(t, 500, `{}`)
	c := New(srv.URL, "", time.Second)
	c.SetAlerter(&recordingQuantAlerter{})

	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 20; k++ {
				_, _ = c.Signal(context.Background(), SignalRequest{})
			}
		}()
	}
	wg.Wait()
}

type recordingQuantAlerter struct {
	mu        sync.Mutex
	failed    int
	recovered int
}

func (r *recordingQuantAlerter) QuantFailure(_ context.Context, _ string, _ bool) {
	r.mu.Lock()
	r.failed++
	r.mu.Unlock()
}

func (r *recordingQuantAlerter) QuantRecovered(_ context.Context) {
	r.mu.Lock()
	r.recovered++
	r.mu.Unlock()
}

func (r *recordingQuantAlerter) failures() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed
}

func TestSignalResponseValidateIsUsableWithoutATransport(t *testing.T) {
	// Validate is the contract the consensus policy will depend on, so it is
	// asserted directly as well as through the client.
	ok := SignalResponse{Action: "sell", Confidence: dec("0.4"), SuggestedStop: ptr(dec("2700"))}
	if err := ok.Validate(); err != nil {
		t.Errorf("a valid response was rejected: %v", err)
	}
	bad := SignalResponse{Action: "sell", Confidence: dec("0.4"), SuggestedStop: ptr(dec("0"))}
	if err := bad.Validate(); !errors.Is(err, ErrBadResponse) {
		t.Errorf("a zero stop = %v, want ErrBadResponse", err)
	}
}
