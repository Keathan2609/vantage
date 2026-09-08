package domain

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func buildChain(t *testing.T, n int) []AuditEvent {
	t.Helper()
	base := time.Date(2025, 7, 8, 12, 0, 0, 0, time.UTC)
	actor := uuid.New()
	events := make([]AuditEvent, 0, n)
	prev := GenesisHash
	for i := 0; i < n; i++ {
		e := AuditEvent{
			ID:            uuid.New(),
			Sequence:      int64(i + 1),
			OccurredAt:    base.Add(time.Duration(i) * time.Second),
			ActorUserID:   &actor,
			ActorType:     "user",
			Action:        AuditOrderSubmitted,
			TargetType:    "order",
			Result:        AuditSuccess,
			RequestID:     "req-" + uuid.NewString(),
			CorrelationID: "cor-1",
			Metadata:      json.RawMessage(`{"symbol":"XAUUSD"}`),
			PrevHash:      prev,
		}
		e.Hash = e.ComputeHash(prev)
		prev = e.Hash
		events = append(events, e)
	}
	return events
}

func TestVerifyChain_IntactChain(t *testing.T) {
	events := buildChain(t, 5)
	ok, at := VerifyChain(events)
	if !ok {
		t.Fatalf("intact chain reported broken at index %d", at)
	}
}

func TestVerifyChain_DetectsContentTampering(t *testing.T) {
	events := buildChain(t, 5)
	// Rewrite the outcome of an event without recomputing hashes: the classic
	// "make the blocked trade look approved" edit.
	events[2].Result = AuditFailure

	ok, at := VerifyChain(events)
	if ok {
		t.Fatal("tampered content was not detected")
	}
	if at != 2 {
		t.Errorf("break reported at %d, want 2", at)
	}
}

func TestVerifyChain_DetectsDeletion(t *testing.T) {
	events := buildChain(t, 5)
	// Remove an event and re-splice the slice, as an attacker deleting the
	// record of a blocked action would.
	spliced := append(append([]AuditEvent{}, events[:2]...), events[3:]...)

	ok, at := VerifyChain(spliced)
	if ok {
		t.Fatal("deletion was not detected")
	}
	if at != 2 {
		t.Errorf("break reported at %d, want 2", at)
	}
}

func TestVerifyChain_DetectsReordering(t *testing.T) {
	events := buildChain(t, 5)
	events[1], events[2] = events[2], events[1]
	if ok, _ := VerifyChain(events); ok {
		t.Fatal("reordering was not detected")
	}
}

func TestVerifyChain_RecomputedHashesDefeatNaiveForgery(t *testing.T) {
	// An attacker who edits an event AND recomputes that event's own hash, but
	// cannot rewrite the following rows, still leaves a broken link. This is
	// the tamper-evidence property: local edits do not survive verification.
	events := buildChain(t, 4)
	events[1].Result = AuditFailure
	events[1].Hash = events[1].ComputeHash(events[1].PrevHash)

	ok, at := VerifyChain(events)
	if ok {
		t.Fatal("forged hash chain was accepted")
	}
	if at != 2 {
		t.Errorf("break reported at %d, want 2 (the following event)", at)
	}
}

func TestCanonicalPayloadIsStable(t *testing.T) {
	e := buildChain(t, 1)[0]
	if e.CanonicalPayload() != e.CanonicalPayload() {
		t.Error("canonical payload must be deterministic")
	}
	// Changing any covered field must change the hash.
	before := e.ComputeHash(GenesisHash)
	e.Action = AuditOrderRejected
	if e.ComputeHash(GenesisHash) == before {
		t.Error("hash must cover the action field")
	}
}

func TestLifecyclePromotionIsGated(t *testing.T) {
	// One rung at a time.
	if ok, _ := CanPromote(LifecycleDraft, LifecycleResearch); !ok {
		t.Error("draft -> research should be allowed")
	}
	if ok, _ := CanPromote(LifecycleDraft, LifecyclePaper); ok {
		t.Error("skipping stages must be refused")
	}
	if ok, _ := CanPromote(LifecyclePaper, LifecycleValidated); ok {
		t.Error("moving backwards must be refused")
	}
	// This build's ceiling: PAPER. DEMO and LIVE are unreachable regardless of
	// who asks, because no adapter exists that could execute them.
	if ok, _ := CanPromote(LifecycleValidated, LifecyclePaper); !ok {
		t.Error("validated -> paper should be allowed")
	}
	if ok, reason := CanPromote(LifecyclePaper, LifecycleDemo); ok {
		t.Errorf("paper -> demo must be refused in this build (reason %q)", reason)
	}
	if ok, _ := CanPromote(LifecycleDemo, LifecycleLive); ok {
		t.Error("promotion to LIVE must be impossible in this build")
	}
	// Retirement is always available.
	if ok, _ := CanPromote(LifecyclePaper, LifecycleRetired); !ok {
		t.Error("retirement must always be allowed")
	}
}

func TestAuthorityAndKillSwitchFailClosed(t *testing.T) {
	now := time.Date(2025, 7, 8, 12, 0, 0, 0, time.UTC)

	// An authority with an empty instrument list permits nothing.
	a := TradingAuthority{Active: true, ValidFrom: now.Add(-time.Hour)}
	if ok, _ := a.Effective(now); !ok {
		t.Error("an active, in-window authority should be effective")
	}
	if a.PermitsInstrument("XAUUSD") {
		t.Error("an empty allow-list must permit nothing")
	}
	if a.PermitsOrderType(OrderTypeMarket) {
		t.Error("an empty order-type list must permit nothing")
	}

	// Expiry, revocation and inactivity each fail closed with a distinct code.
	expired := a
	until := now.Add(-time.Minute)
	expired.ValidUntil = &until
	if ok, rej := expired.Effective(now); ok || rej.Code != RejectAuthorityExpired {
		t.Errorf("expired authority: ok=%v code=%s", ok, rej.Code)
	}

	revoked := a
	revoked.RevokedAt = &now
	if ok, rej := revoked.Effective(now); ok || rej.Code != RejectAuthorityRevoked {
		t.Errorf("revoked authority: ok=%v code=%s", ok, rej.Code)
	}

	inactive := a
	inactive.Active = false
	if ok, rej := inactive.Effective(now); ok || rej.Code != RejectNoAuthority {
		t.Errorf("inactive authority: ok=%v code=%s", ok, rej.Code)
	}

	// Kill switches: a global switch is reported in preference to a narrower one.
	state := KillSwitchState{Active: []KillSwitch{
		{Scope: KillScopeStrategy, Active: true},
		{Scope: KillScopeGlobal, Active: true},
	}}
	blocked, sw := state.Blocked()
	if !blocked || sw.Scope != KillScopeGlobal {
		t.Errorf("expected the global switch to be surfaced, got %+v", sw)
	}
	if blocked, _ := (KillSwitchState{}).Blocked(); blocked {
		t.Error("no active switches must not block")
	}
}
