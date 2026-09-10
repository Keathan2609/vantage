package ratelimit

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// The rate limiter is one of the few components whose failures are all silent.
// A bucket that never refills locks a trader out of closing a position; a
// bucket that refills too fast removes the control entirely; a key built from
// the wrong parts lets one caller spend another's budget. None of that shows up
// as an error, which is why this package needed tests of its own rather than
// being exercised only through end-to-end suites that happen to stay under
// every limit.

// clock is a hand-driven clock. The limiter takes one precisely so its
// behaviour over time can be asserted without waiting for time to pass.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2027, 3, 2, 9, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// testRule is small enough to exhaust in a few calls and refills at a rate
// that makes the arithmetic checkable by hand: one token per second.
var testRule = Rule{
	Name: "test", Capacity: 3, RefillPerSecond: 1, Window: time.Minute,
}

func newLimiter(t *testing.T, c *clock) *MemoryLimiter {
	t.Helper()
	l := NewMemoryLimiter(c.now)
	t.Cleanup(l.Close)
	return l
}

func TestABucketStartsFullAndDrainsOnce(t *testing.T) {
	// A caller's first request must not be refused, and each request must cost
	// exactly one token. A first-request refusal would be indistinguishable
	// from a broken endpoint.
	c := newClock()
	l := newLimiter(t, c)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		d, err := l.Allow(ctx, testRule, "alice")
		if err != nil {
			t.Fatalf("Allow: %v", err)
		}
		if !d.Allowed {
			t.Fatalf("request %d refused with a full bucket", i+1)
		}
		want := float64(2 - i)
		if d.Remaining != want {
			t.Errorf("request %d: remaining = %v, want %v", i+1, d.Remaining, want)
		}
	}

	d, _ := l.Allow(ctx, testRule, "alice")
	if d.Allowed {
		t.Fatal("a fourth request was allowed against a capacity of three")
	}
	if d.RetryAfter <= 0 {
		t.Error("a refusal carried no retry-after, so a client cannot back off")
	}
}

func TestRetryAfterIsLongEnoughToActuallySucceed(t *testing.T) {
	// A retry-after that expires before a token exists sends every client back
	// to be refused again, which turns a rate limit into a retry storm. This
	// is the one number a client obeys, so it has to be honest.
	c := newClock()
	l := newLimiter(t, c)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		l.Allow(ctx, testRule, "bob")
	}
	d, _ := l.Allow(ctx, testRule, "bob")
	if d.Allowed {
		t.Fatal("the bucket was not exhausted")
	}

	c.advance(d.RetryAfter)
	after, _ := l.Allow(ctx, testRule, "bob")
	if !after.Allowed {
		t.Errorf("still refused after waiting the advertised %s", d.RetryAfter)
	}
}

func TestARefusalDoesNotCostATokenItDoesNotHave(t *testing.T) {
	// The failure this prevents is a lockout that lengthens the more a client
	// retries. If a refused request still decremented, an impatient client
	// would push its own bucket further negative and never recover -- and the
	// person most likely to retry impatiently is one trying to close a
	// position.
	c := newClock()
	l := newLimiter(t, c)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		l.Allow(ctx, testRule, "carol")
	}
	// Hammer it while empty.
	for i := 0; i < 50; i++ {
		l.Allow(ctx, testRule, "carol")
	}
	// One second of refill is one token, and it must be available.
	c.advance(time.Second)
	d, _ := l.Allow(ctx, testRule, "carol")
	if !d.Allowed {
		t.Fatal("fifty refused retries delayed the recovery: the bucket went negative")
	}
}

func TestRefillIsCappedAtCapacity(t *testing.T) {
	// An idle bucket must not accumulate credit beyond the burst allowance,
	// or a caller silent for an hour arrives with an hour's worth of requests
	// and the limit means nothing at the moment it matters most.
	c := newClock()
	l := newLimiter(t, c)
	ctx := context.Background()

	l.Allow(ctx, testRule, "dave")
	c.advance(time.Hour)

	allowed := 0
	for i := 0; i < 20; i++ {
		if d, _ := l.Allow(ctx, testRule, "dave"); d.Allowed {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("an hour idle bought %d requests, want the capacity of 3", allowed)
	}
}

func TestRefillIsProportionalToElapsedTime(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		l.Allow(ctx, testRule, "erin")
	}
	// Two seconds at one token per second.
	c.advance(2 * time.Second)

	allowed := 0
	for i := 0; i < 5; i++ {
		if d, _ := l.Allow(ctx, testRule, "erin"); d.Allowed {
			allowed++
		}
	}
	if allowed != 2 {
		t.Errorf("two seconds of refill bought %d requests, want 2", allowed)
	}
}

func TestOneCallerCannotSpendAnothersBudget(t *testing.T) {
	// The whole point of keying by identity. If buckets were shared, a single
	// noisy client would refuse everybody -- and on a trading platform that is
	// a denial of service against people with open positions.
	c := newClock()
	l := newLimiter(t, c)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		l.Allow(ctx, testRule, "noisy")
	}
	d, _ := l.Allow(ctx, testRule, "quiet")
	if !d.Allowed {
		t.Fatal("one caller exhausting its bucket refused another")
	}
	if d.Remaining != 2 {
		t.Errorf("the second caller started with %v remaining, want a full bucket", d.Remaining)
	}
}

func TestRulesDoNotShareABucket(t *testing.T) {
	// Exhausting the order-placement budget must not also refuse a cancel.
	// The rule name is part of the key precisely so a client that has spent
	// its opening budget can still get out of a position.
	c := newClock()
	l := newLimiter(t, c)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		l.Allow(ctx, RuleOrderPlace, "frank")
	}
	if d, _ := l.Allow(ctx, RuleOrderCancel, "frank"); !d.Allowed {
		t.Fatal("exhausting the place budget also refused a cancel")
	}
}

func TestCancellingIsNeverTighterThanPlacing(t *testing.T) {
	// A configuration rule, asserted rather than trusted. If cancelling were
	// scarcer than placing, a client could open more positions than it can
	// close, which is the one asymmetry a trading platform must never have.
	if RuleOrderCancel.Capacity < RuleOrderPlace.Capacity {
		t.Errorf("cancel capacity %v is below place capacity %v",
			RuleOrderCancel.Capacity, RuleOrderPlace.Capacity)
	}
	if RuleOrderCancel.RefillPerSecond < RuleOrderPlace.RefillPerSecond {
		t.Errorf("cancel refill %v is below place refill %v",
			RuleOrderCancel.RefillPerSecond, RuleOrderPlace.RefillPerSecond)
	}
}

func TestTheKillSwitchIsNeverTheScarcestControl(t *testing.T) {
	// An operator hitting the emergency stop repeatedly during an incident
	// must not be throttled out of it. Whatever else is tightened, this rule
	// has to stay at least as generous as placing an order -- otherwise the
	// platform can be filled faster than it can be stopped.
	if RuleKillSwitch.Capacity < RuleOrderPlace.Capacity {
		t.Errorf("kill-switch capacity %v is below place capacity %v",
			RuleKillSwitch.Capacity, RuleOrderPlace.Capacity)
	}
	if RuleKillSwitch.RefillPerSecond < RuleOrderPlace.RefillPerSecond {
		t.Errorf("kill-switch refill %v is below place refill %v",
			RuleKillSwitch.RefillPerSecond, RuleOrderPlace.RefillPerSecond)
	}
}

func TestEveryDefaultRuleIsUsable(t *testing.T) {
	// A rule with zero capacity refuses every request forever, and a rule with
	// zero refill refuses every request after the first burst -- both silently,
	// and both look like a broken endpoint rather than a misconfigured limit.
	for _, rule := range []Rule{
		RuleLogin, RuleMFA, RulePasswordChange, RuleOrderPlace, RuleOrderCancel,
		RuleControlChange, RuleKillSwitch, RuleRead, RuleResearch,
	} {
		if strings.TrimSpace(rule.Name) == "" {
			t.Errorf("a rule has no name, so its metrics and logs are unattributable")
		}
		if rule.Capacity < 1 {
			t.Errorf("%s: capacity %v refuses the first request", rule.Name, rule.Capacity)
		}
		if rule.RefillPerSecond <= 0 {
			t.Errorf("%s: refill %v never recovers", rule.Name, rule.RefillPerSecond)
		}
		if rule.Window <= 0 {
			t.Errorf("%s: window %v discards the bucket immediately", rule.Name, rule.Window)
		}
	}
}

func TestConcurrentCallersConsumeExactlyTheCapacity(t *testing.T) {
	// The bucket is shared mutable state under a mutex. A lost update here
	// would let more requests through than the capacity allows, which is the
	// failure a rate limit exists to prevent and the one least likely to be
	// noticed.
	c := newClock()
	l := newLimiter(t, c)
	ctx := context.Background()

	const callers = 50
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, _ := l.Allow(ctx, testRule, "shared"); d.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed != int(testRule.Capacity) {
		t.Errorf("%d of %d concurrent requests were allowed, want exactly %v",
			allowed, callers, testRule.Capacity)
	}
}

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

func TestIdentityPrefersTheUserOverTheAddress(t *testing.T) {
	// Authenticated callers are limited per user, so colleagues behind one
	// office NAT do not share a budget. Keying an authenticated request by
	// address would make one busy user refuse the rest of the team.
	got := Identity("user-1", "203.0.113.7", "")
	if got != "u:user-1" {
		t.Errorf("Identity = %q, want the user key", got)
	}
}

func TestIdentityFallsBackToTheAddressAndThenToAnon(t *testing.T) {
	if got := Identity("", "203.0.113.7", ""); got != "ip:203.0.113.7" {
		t.Errorf("Identity = %q, want the address key", got)
	}
	// Neither a user nor an address still has to produce a key. An empty
	// string would put every such caller in one bucket by accident rather
	// than on purpose, and "anon" says which.
	if got := Identity("", "", ""); got != "anon" {
		t.Errorf("Identity = %q, want anon", got)
	}
}

func TestIdentityIncludesTheSubmittedUsername(t *testing.T) {
	// Credential stuffing rotates the username from one address. Without the
	// username in the key, each attempt would look like a different caller
	// only if the address changed -- but including it means one address
	// spraying many usernames is still one bucket per username, so the
	// per-address rule catches the spray and the per-account lockout catches
	// the guessing. Both are needed; this is the half that lives here.
	got := Identity("", "203.0.113.7", "Alice@Example.COM")
	if got != "ip:203.0.113.7|alice@example.com" {
		t.Errorf("Identity = %q, want the address and the lowercased username", got)
	}
}

func TestIdentityIsCaseInsensitiveInTheUsername(t *testing.T) {
	// Otherwise "Alice@example.com" and "alice@example.com" get separate
	// budgets, and an attacker doubles their allowance by changing the case.
	a := Identity("", "203.0.113.7", "alice@example.com")
	b := Identity("", "203.0.113.7", "ALICE@EXAMPLE.COM")
	if a != b {
		t.Errorf("case changed the bucket: %q against %q", a, b)
	}
}

func TestBothLimitersSatisfyTheInterface(t *testing.T) {
	// Compile-time assertions already exist in the package; this states the
	// reason. The middleware holds a Limiter, so a process using Redis and one
	// using memory must be interchangeable -- and the Redis limiter falls back
	// to the memory one when Redis is unreachable, which only works if the two
	// agree on behaviour.
	var _ Limiter = NewMemoryLimiter(nil)
	var _ Limiter = (*RedisLimiter)(nil)
}
