package mock

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// These tests cover the injector's own semantics: arming, exact firing counts,
// and disarming. The behaviour of each mode against a real database is
// exercised by the smoke suite, which drives the whole OMS pipeline; what
// matters here is that a fault fires EXACTLY as many times as it was armed
// for, because a fault that fires an unpredictable number of times produces a
// test failure nobody can reproduce.

func TestInjectorIsDisabledUntilArmed(t *testing.T) {
	i := NewFaultInjector()
	if i.Enabled() {
		t.Fatal("a fresh injector must be disabled: a venue that can lie by default is a bug waiting for a deployment")
	}
	if _, ok := i.take(FaultTimeout); ok {
		t.Fatal("nothing may fire on a disabled injector")
	}
}

func TestArmFiresExactlyTheRequestedNumberOfTimes(t *testing.T) {
	i := NewFaultInjector()
	if err := i.Arm(FaultRejection, 3, FaultSpec{}); err != nil {
		t.Fatalf("Arm: %v", err)
	}

	for n := 1; n <= 3; n++ {
		if _, ok := i.take(FaultRejection); !ok {
			t.Fatalf("firing %d of 3 did not happen", n)
		}
	}
	if _, ok := i.take(FaultRejection); ok {
		t.Fatal("the fault fired a fourth time after being armed for three")
	}
	if got := i.Fired(FaultRejection); got != 3 {
		t.Fatalf("Fired = %d, want 3", got)
	}
}

func TestArmRejectsUnknownAndNonPositive(t *testing.T) {
	i := NewFaultInjector()
	if err := i.Arm(Fault("nonsense"), 1, FaultSpec{}); err == nil {
		t.Fatal("an unknown fault must be refused rather than silently ignored")
	}
	if err := i.Arm(FaultTimeout, 0, FaultSpec{}); err == nil {
		t.Fatal("arming for zero calls must be refused: it reads as armed and does nothing")
	}
}

func TestDisarmAndReset(t *testing.T) {
	i := NewFaultInjector()
	_ = i.Arm(FaultTimeout, 5, FaultSpec{})
	_ = i.Arm(FaultDisconnect, 5, FaultSpec{})

	i.Disarm(FaultTimeout)
	if _, ok := i.take(FaultTimeout); ok {
		t.Fatal("a disarmed fault still fired")
	}
	if _, ok := i.take(FaultDisconnect); !ok {
		t.Fatal("disarming one fault must not disarm another")
	}

	i.Reset()
	if i.Enabled() {
		t.Fatal("Reset must disable injection")
	}
	if len(i.Armed()) != 0 {
		t.Fatalf("Reset left %d faults armed", len(i.Armed()))
	}
	if i.Fired(FaultDisconnect) != 0 {
		t.Fatal("Reset must clear the firing counts")
	}
}

func TestPeekDoesNotConsume(t *testing.T) {
	i := NewFaultInjector()
	_ = i.Arm(FaultSlippage, 1, FaultSpec{Factor: decimal.RequireFromString("0.01")})

	for n := 0; n < 5; n++ {
		if _, ok := i.peek(FaultSlippage); !ok {
			t.Fatalf("peek %d consumed the fault", n)
		}
	}
	// The modes that MODIFY a result rather than replacing it use peek, so
	// they must apply for the whole armed window rather than once.
	if _, ok := i.take(FaultSlippage); !ok {
		t.Fatal("take after peeks should still fire")
	}
}

func TestArmedReportsRemainingCounts(t *testing.T) {
	i := NewFaultInjector()
	_ = i.Arm(FaultPartialFill, 2, FaultSpec{Fraction: decimal.RequireFromString("0.5")})
	_, _ = i.take(FaultPartialFill)

	armed := i.Armed()
	if armed[FaultPartialFill] != 1 {
		t.Fatalf("Armed reported %d remaining, want 1", armed[FaultPartialFill])
	}
}

func TestEveryFaultNameIsValidAndDistinct(t *testing.T) {
	seen := map[Fault]bool{}
	for _, f := range AllFaults {
		if !f.Valid() {
			t.Fatalf("%q is in AllFaults but Valid() rejects it", f)
		}
		if seen[f] {
			t.Fatalf("%q appears twice in AllFaults", f)
		}
		seen[f] = true
	}
	// The spec this build is audited against names twelve modes. If one is
	// added or removed, this number is a deliberate decision, not a drift.
	if len(AllFaults) != 12 {
		t.Fatalf("AllFaults has %d entries, want 12", len(AllFaults))
	}
}

func TestEveryFaultCanBeArmedAndFired(t *testing.T) {
	for _, f := range AllFaults {
		t.Run(string(f), func(t *testing.T) {
			i := NewFaultInjector()
			spec := FaultSpec{
				Delay:    time.Millisecond,
				Factor:   decimal.RequireFromString("2"),
				Fraction: decimal.RequireFromString("0.5"),
			}
			if err := i.Arm(f, 1, spec); err != nil {
				t.Fatalf("Arm(%s): %v", f, err)
			}
			if _, ok := i.take(f); !ok {
				t.Fatalf("%s was armed but did not fire", f)
			}
		})
	}
}

func TestSlippageAndSpreadRequireAFactor(t *testing.T) {
	// A mode armed without the number it needs must not silently apply a
	// zero-effect fault, because the test would then pass for the wrong
	// reason.
	b := &Broker{faults: NewFaultInjector()}

	_ = b.faults.Arm(FaultSlippage, 1, FaultSpec{})
	if _, ok := b.injectedSlippage(); ok {
		t.Fatal("slippage with a zero factor must not report as armed")
	}

	b.faults.Reset()
	_ = b.faults.Arm(FaultSpreadExpansion, 1, FaultSpec{})
	if _, ok := b.injectedSpreadMultiplier(); ok {
		t.Fatal("spread expansion with a zero multiplier must not report as armed")
	}

	b.faults.Reset()
	_ = b.faults.Arm(FaultSpreadExpansion, 1, FaultSpec{Factor: decimal.RequireFromString("3")})
	mult, ok := b.injectedSpreadMultiplier()
	if !ok || !mult.Equal(decimal.RequireFromString("3")) {
		t.Fatalf("spread multiplier = %s, %v; want 3, true", mult, ok)
	}
}

func TestPartialFillRequiresAFraction(t *testing.T) {
	b := &Broker{faults: NewFaultInjector()}
	_ = b.faults.Arm(FaultPartialFill, 1, FaultSpec{})
	if _, ok := b.injectedPartialFill(); ok {
		t.Fatal("a partial fill with no fraction must not report as armed")
	}
}
