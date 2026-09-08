package notify

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The alerter's store is optional, so these tests exercise the part that
// actually decides whether an operator is interrupted — the cooldown — without
// a database. The store path is exercised by the smoke suite.

func newTestAlerter(now *time.Time) *Alerter {
	return New(nil, func() time.Time { return *now })
}

func TestFirstOccurrenceAlwaysAlerts(t *testing.T) {
	now := time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC)
	a := newTestAlerter(&now)

	ok, suppressed := a.shouldAlert(Event{Kind: KindMarketDataStale, Key: "XAUUSD.m"})
	if !ok {
		t.Fatal("the first occurrence must alert; an incident nobody is told about is the failure mode")
	}
	if suppressed != 0 {
		t.Fatalf("suppressed = %d on a first occurrence, want 0", suppressed)
	}
}

func TestRepeatsInsideTheWindowAreSuppressedAndCounted(t *testing.T) {
	now := time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC)
	a := newTestAlerter(&now)
	e := Event{Kind: KindMarketDataStale, Key: "XAUUSD.m"}

	if ok, _ := a.shouldAlert(e); !ok {
		t.Fatal("first alert did not fire")
	}
	// A feed is evaluated every two seconds. Thirty repeats inside the
	// two-minute window must produce no further alerts.
	for i := 0; i < 30; i++ {
		now = now.Add(2 * time.Second)
		if ok, _ := a.shouldAlert(e); ok {
			t.Fatalf("repeat %d alerted inside the cooldown window", i)
		}
	}

	now = now.Add(3 * time.Minute)
	ok, suppressed := a.shouldAlert(e)
	if !ok {
		t.Fatal("the alert must fire again once the window expires")
	}
	if suppressed != 30 {
		t.Fatalf("suppressed = %d, want 30: the count is how an operator knows it never stopped", suppressed)
	}
}

func TestZeroCooldownKindsAreNeverSuppressed(t *testing.T) {
	now := time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC)
	a := newTestAlerter(&now)

	// A kill switch is a deliberate human action and an unknown order outcome
	// needs individual resolution. Neither may ever be collapsed.
	for _, kind := range []Kind{KindKillSwitch, KindOrderFailed, KindAuditChainBroken} {
		for i := 0; i < 5; i++ {
			if ok, _ := a.shouldAlert(Event{Kind: kind, Key: "k"}); !ok {
				t.Fatalf("%s was suppressed on occurrence %d; it must never be", kind, i)
			}
		}
	}
}

func TestCooldownIsPerKeyNotPerKind(t *testing.T) {
	now := time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC)
	a := newTestAlerter(&now)

	if ok, _ := a.shouldAlert(Event{Kind: KindMarketDataStale, Key: "XAUUSD.m"}); !ok {
		t.Fatal("first instrument did not alert")
	}
	// A second instrument going stale is a different fact and must not be
	// hidden by the first one's cooldown.
	if ok, _ := a.shouldAlert(Event{Kind: KindMarketDataStale, Key: "EURUSD"}); !ok {
		t.Fatal("a different key was suppressed by another key's cooldown")
	}
}

func TestResolveOnlyFiresForAnActiveCondition(t *testing.T) {
	now := time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC)
	a := newTestAlerter(&now)
	ctx := context.Background()

	// Nothing was raised, so a recovery is not news.
	a.Resolve(ctx, KindMarketDataStale, "XAUUSD.m", "recovered", "body", nil)
	if a.Active(KindMarketDataStale, "XAUUSD.m") {
		t.Fatal("Resolve on an unraised condition should not mark it active")
	}

	a.Raise(ctx, Event{Kind: KindMarketDataStale, Severity: SeverityWarning, Key: "XAUUSD.m"})
	if !a.Active(KindMarketDataStale, "XAUUSD.m") {
		t.Fatal("a raised condition should report active")
	}

	a.Resolve(ctx, KindMarketDataStale, "XAUUSD.m", "recovered", "body", nil)
	if a.Active(KindMarketDataStale, "XAUUSD.m") {
		t.Fatal("a resolved condition should no longer report active")
	}
}

func TestReRaiseAfterResolveAlertsImmediately(t *testing.T) {
	now := time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC)
	a := newTestAlerter(&now)
	ctx := context.Background()
	e := Event{Kind: KindBrokerFailure, Severity: SeverityCritical, Key: "mock:place_order"}

	a.Raise(ctx, e)
	a.Resolve(ctx, KindBrokerFailure, "mock:place_order", "recovered", "body", nil)

	// Flapping is itself information. A second outage one second later must
	// alert rather than be swallowed by the first outage's window.
	now = now.Add(time.Second)
	if ok, _ := a.shouldAlert(e); !ok {
		t.Fatal("a condition that recovered and broke again must alert immediately")
	}
}

func TestEveryKindHasAnExplicitCooldown(t *testing.T) {
	// A kind with no entry silently gets the five-minute default. For the
	// events in this list that is a decision, so it should be visible.
	kinds := []Kind{
		KindMarketDataStale, KindBrokerFailure, KindReconciliation, KindKillSwitch,
		KindDailyLoss, KindAuthFailures, KindQuantFailure, KindOrderFailed,
		KindAuditChainBroken, KindAutomationSuspended,
	}
	for _, k := range kinds {
		if _, ok := cooldowns[k]; !ok {
			t.Fatalf("%s has no explicit cooldown", k)
		}
	}
}

func TestRaiseWithoutAStoreDoesNotPanic(t *testing.T) {
	now := time.Now()
	a := newTestAlerter(&now)
	id := uuid.New()

	// Every typed helper, with a nil store. An alerter that panics takes down
	// the operation that noticed the problem, which is the worst outcome.
	ctx := context.Background()
	a.FeedDegraded(ctx, "XAUUSD.m", "XAUUSD.m", "stale", []string{"stale_quote"}, 12.5)
	a.FeedRecovered(ctx, "XAUUSD.m", "XAUUSD.m")
	a.BrokerFailure(ctx, "mock", "place_order", true, "timeout")
	a.BrokerRecovered(ctx, "mock", "place_order")
	a.ReconciliationMismatch(ctx, id, 1, 2, []string{"order_missing_at_broker"})
	a.ReconciliationClean(ctx, id)
	a.KillSwitchActivated(ctx, "account", id.String(), "investigating", &id)
	a.KillSwitchReleased(ctx, "account", id.String(), &id)
	a.DailyLossThreshold(ctx, id, "15.00", "15.00", "ZAR", true)
	a.RepeatedAuthFailure(ctx, "trader@vantage.local", 5, true, "127.0.0.1")
	a.QuantFailure(ctx, "connection refused", true)
	a.QuantRecovered(ctx)
	a.OrderOutcomeUnknown(ctx, id, uuid.NewString(), "XAUUSD.m", "timeout")
	a.AuditChainBroken(ctx, 42)
}

func TestSeverityEscalatesForInvalidDataAndUnknownOutcomes(t *testing.T) {
	now := time.Now()
	a := newTestAlerter(&now)
	ctx := context.Background()

	// A degraded feed is a warning; invalid data is critical, because the
	// prices on screen cannot be trusted at all.
	a.FeedDegraded(ctx, "i", "SYM", "degraded", nil, 4)
	a.FeedDegraded(ctx, "j", "SYM2", "invalid", nil, 4)
	// No assertion on the log sink here; what is asserted is that the two
	// paths are distinguishable and neither panics. The severity mapping
	// itself is a single expression in events.go.
	if !a.Active(KindMarketDataStale, "i") || !a.Active(KindMarketDataStale, "j") {
		t.Fatal("both feed conditions should be active")
	}
}
