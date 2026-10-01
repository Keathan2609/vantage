// Package ratelimit implements per-operation token-bucket rate limiting.
//
// Limits are per OPERATION, not global. Logging in, changing risk limits and
// placing an order have different abuse profiles and different costs, so they
// get different budgets. A single global limit would either be too loose to
// stop credential stuffing or too tight to trade.
//
// Two backends exist. Redis is used when configured, so limits hold across
// instances. Otherwise an in-process limiter applies, which is correct for a
// single instance and is documented as such -- a multi-instance deployment
// without Redis would divide each limit by the instance count.
package ratelimit

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vantage/control-api/internal/metrics"
)

// Rule is a token-bucket configuration.
type Rule struct {
	// Name identifies the bucket in metrics and logs.
	Name string
	// Capacity is the burst allowance.
	Capacity float64
	// RefillPerSecond is the sustained rate.
	RefillPerSecond float64
	// Window is how long an idle bucket is retained.
	Window time.Duration
}

// Decision is the outcome of a limit check.
type Decision struct {
	Allowed    bool
	Remaining  float64
	RetryAfter time.Duration
	Rule       Rule
}

// Limiter checks and consumes budget.
type Limiter interface {
	// Allow consumes one token from the identity's bucket for a rule.
	Allow(ctx context.Context, rule Rule, identity string) (Decision, error)
}

// Default rules.
//
// Calibration note. These buckets are keyed by CLIENT ADDRESS for
// unauthenticated endpoints, so they are a coarse anti-spray control rather
// than the defence against guessing one account's password. That job belongs
// to the per-account lockout in internal/httpapi (five failures, then
// exponential backoff), which is both stricter and targeted.
//
// Setting the address bucket too tight punishes the wrong people: several
// colleagues behind one office NAT share a single bucket, and a handful of
// ordinary sign-ins would lock them all out. Ten attempts with a sustained
// two per minute stops credential spraying while leaving shared addresses
// usable.
//
// Order placement is more generous but still bounded: an unbounded order
// endpoint is both a financial risk and a denial-of-service amplifier pointed
// at the venue.
var (
	RuleLogin = Rule{
		Name: "auth_login", Capacity: 10, RefillPerSecond: 2.0 / 60.0, Window: 30 * time.Minute,
	}
	RuleMFA = Rule{
		Name: "auth_mfa", Capacity: 10, RefillPerSecond: 2.0 / 60.0, Window: 30 * time.Minute,
	}
	RulePasswordChange = Rule{
		Name: "auth_password_change", Capacity: 3, RefillPerSecond: 3.0 / 600.0, Window: 30 * time.Minute,
	}
	RuleOrderPlace = Rule{
		Name: "order_place", Capacity: 20, RefillPerSecond: 2, Window: 10 * time.Minute,
	}
	RuleOrderCancel = Rule{
		Name: "order_cancel", Capacity: 30, RefillPerSecond: 3, Window: 10 * time.Minute,
	}
	RuleControlChange = Rule{
		Name: "control_change", Capacity: 10, RefillPerSecond: 0.5, Window: 30 * time.Minute,
	}
	RuleKillSwitch = Rule{
		// Generous on purpose: an operator hitting the emergency stop
		// repeatedly during an incident must never be throttled out of it.
		Name: "kill_switch", Capacity: 30, RefillPerSecond: 2, Window: 10 * time.Minute,
	}
	RuleRead = Rule{
		Name: "read", Capacity: 300, RefillPerSecond: 30, Window: 5 * time.Minute,
	}
	RuleResearch = Rule{
		// Backtests and training are expensive; a small budget prevents one
		// user from occupying the quant service.
		Name: "research", Capacity: 10, RefillPerSecond: 0.2, Window: 30 * time.Minute,
	}
)

// ---------------------------------------------------------------------------
// In-process limiter
// ---------------------------------------------------------------------------

type bucket struct {
	tokens   float64
	lastFill time.Time
}

// MemoryLimiter is a single-process token bucket.
type MemoryLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	stop    chan struct{}
}

// NewMemoryLimiter builds an in-process limiter and starts its sweeper.
func NewMemoryLimiter(now func() time.Time) *MemoryLimiter {
	if now == nil {
		now = time.Now
	}
	l := &MemoryLimiter{
		buckets: map[string]*bucket{},
		now:     now,
		stop:    make(chan struct{}),
	}
	go l.sweep()
	return l
}

// Close stops the background sweeper.
func (l *MemoryLimiter) Close() { close(l.stop) }

// Allow consumes a token.
func (l *MemoryLimiter) Allow(_ context.Context, rule Rule, identity string) (Decision, error) {
	key := rule.Name + "|" + identity
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: rule.Capacity, lastFill: now}
		l.buckets[key] = b
	}

	elapsed := now.Sub(b.lastFill).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(rule.Capacity, b.tokens+elapsed*rule.RefillPerSecond)
		b.lastFill = now
	}

	if b.tokens < 1 {
		retry := time.Duration((1 - b.tokens) / rule.RefillPerSecond * float64(time.Second))
		metrics.RateLimited.WithLabelValues(rule.Name).Inc()
		return Decision{Allowed: false, Remaining: 0, RetryAfter: retry, Rule: rule}, nil
	}
	b.tokens--
	return Decision{Allowed: true, Remaining: b.tokens, Rule: rule}, nil
}

func (l *MemoryLimiter) sweep() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			now := l.now()
			l.mu.Lock()
			for k, b := range l.buckets {
				if now.Sub(b.lastFill) > time.Hour {
					delete(l.buckets, k)
				}
			}
			l.mu.Unlock()
		}
	}
}

// ---------------------------------------------------------------------------
// Redis limiter
// ---------------------------------------------------------------------------

// RedisLimiter shares limit state across instances.
type RedisLimiter struct {
	client   *redis.Client
	fallback *MemoryLimiter
	now      func() time.Time
}

// tokenBucketScript performs the whole refill-and-consume atomically.
//
// Doing this in a script rather than as read-modify-write is the difference
// between a limit and a suggestion: concurrent requests against the same key
// would otherwise each read the same token count and all be allowed.
const tokenBucketScript = `
local key = KEYS[1]
local capacity = tonumber(ARGV[1])
local refill = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

local data = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(data[1])
local ts = tonumber(data[2])

if tokens == nil then
  tokens = capacity
  ts = now
end

local elapsed = math.max(0, now - ts)
tokens = math.min(capacity, tokens + elapsed * refill)

local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end

redis.call('HMSET', key, 'tokens', tokens, 'ts', now)
redis.call('EXPIRE', key, ttl)
return {allowed, tostring(tokens)}
`

// NewRedisLimiter connects to Redis, falling back to in-process limiting if
// Redis is unreachable at call time.
func NewRedisLimiter(url string, now func() time.Time) (*RedisLimiter, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("ratelimit: parse redis url: %w", err)
	}
	opts.DialTimeout = 2 * time.Second
	opts.ReadTimeout = time.Second
	opts.WriteTimeout = time.Second
	if now == nil {
		now = time.Now
	}
	return &RedisLimiter{
		client:   redis.NewClient(opts),
		fallback: NewMemoryLimiter(now),
		now:      now,
	}, nil
}

// Close releases resources.
func (l *RedisLimiter) Close() error {
	l.fallback.Close()
	return l.client.Close()
}

// Ping verifies connectivity.
func (l *RedisLimiter) Ping(ctx context.Context) error { return l.client.Ping(ctx).Err() }

// Allow consumes a token, degrading to the in-process limiter if Redis fails.
//
// Degrading rather than failing open is the deliberate choice: losing Redis
// must not remove rate limiting from the login endpoint. Degrading rather than
// failing closed is equally deliberate: a cache outage must not lock every user
// out of a trading platform where they may need to close positions.
func (l *RedisLimiter) Allow(ctx context.Context, rule Rule, identity string) (Decision, error) {
	key := "vantage:rl:" + rule.Name + ":" + identity
	ttl := int(rule.Window.Seconds())
	if ttl < 60 {
		ttl = 60
	}

	res, err := l.client.Eval(ctx, tokenBucketScript, []string{key},
		rule.Capacity, rule.RefillPerSecond, float64(l.now().UnixNano())/1e9, ttl).Result()
	if err != nil {
		return l.fallback.Allow(ctx, rule, identity)
	}

	arr, ok := res.([]any)
	if !ok || len(arr) != 2 {
		return l.fallback.Allow(ctx, rule, identity)
	}
	allowed, _ := arr[0].(int64)
	remaining := 0.0
	if s, ok := arr[1].(string); ok {
		fmt.Sscanf(s, "%f", &remaining)
	}

	if allowed == 0 {
		retry := time.Duration((1 - remaining) / rule.RefillPerSecond * float64(time.Second))
		metrics.RateLimited.WithLabelValues(rule.Name).Inc()
		return Decision{Allowed: false, RetryAfter: retry, Rule: rule}, nil
	}
	return Decision{Allowed: true, Remaining: remaining, Rule: rule}, nil
}

// Identity builds a bucket key from the parts that identify a caller.
//
// Authenticated requests are limited per user, so one user cannot exhaust
// another's budget. Unauthenticated requests fall back to the client address,
// combined with the submitted username where there is one, so credential
// stuffing across many usernames from one address is still caught.
func Identity(userID, ip, extra string) string {
	parts := make([]string, 0, 3)
	if userID != "" {
		parts = append(parts, "u:"+userID)
	} else if ip != "" {
		parts = append(parts, "ip:"+ip)
	} else {
		parts = append(parts, "anon")
	}
	if extra != "" {
		parts = append(parts, strings.ToLower(extra))
	}
	return strings.Join(parts, "|")
}

var (
	_ Limiter = (*MemoryLimiter)(nil)
	_ Limiter = (*RedisLimiter)(nil)
)
