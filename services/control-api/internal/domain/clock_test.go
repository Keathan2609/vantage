package domain

import (
	"testing"
	"time"

	_ "time/tzdata" // tests must resolve IANA zones on Windows too
)

func mustClock(t *testing.T) *MarketClock {
	t.Helper()
	mc, err := NewMarketClock(ForexMetalsCalendar())
	if err != nil {
		t.Fatalf("NewMarketClock: %v", err)
	}
	return mc
}

func utc(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts.UTC()
}

func TestMarketStatus_WeekBoundaries(t *testing.T) {
	mc := mustClock(t)

	cases := []struct {
		name string
		when string
		want MarketStatus
	}{
		// Sunday 17:00 New York = 21:00 UTC during EDT (summer).
		{"saturday is closed", "2025-07-05T12:00:00Z", MarketClosedWeekend},
		{"sunday before open", "2025-07-06T20:00:00Z", MarketClosedWeekend},
		{"sunday at open (EDT)", "2025-07-06T21:00:00Z", MarketOpen},
		{"monday midday", "2025-07-07T12:00:00Z", MarketOpen},
		// Friday 17:00 New York = 21:00 UTC during EDT.
		{"friday before close", "2025-07-11T20:59:00Z", MarketOpen},
		{"friday at close", "2025-07-11T21:00:00Z", MarketClosedWeekend},
		{"friday after close", "2025-07-11T23:00:00Z", MarketClosedWeekend},

		// In winter New York is EST (UTC-5), so the same 17:00 local boundary
		// falls an hour later in UTC. A system with hard-coded UTC offsets
		// trades the wrong hours for months; this is the regression guard.
		{"sunday before open (EST)", "2025-01-05T21:00:00Z", MarketClosedWeekend},
		{"sunday at open (EST)", "2025-01-05T22:00:00Z", MarketOpen},
		{"friday before close (EST)", "2025-01-10T21:59:00Z", MarketOpen},
		{"friday at close (EST)", "2025-01-10T22:00:00Z", MarketClosedWeekend},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mc.Status(utc(t, c.when)); got != c.want {
				t.Errorf("Status(%s) = %s, want %s", c.when, got, c.want)
			}
		})
	}
}

func TestMarketStatus_DailyBreak(t *testing.T) {
	mc := mustClock(t)
	// Tuesday 17:30 New York (EDT) = 21:30 UTC: inside the rollover break.
	if got := mc.Status(utc(t, "2025-07-08T21:30:00Z")); got != MarketClosedBreak {
		t.Errorf("expected daily break, got %s", got)
	}
	// Tuesday 18:30 NY = 22:30 UTC: trading resumed.
	if got := mc.Status(utc(t, "2025-07-08T22:30:00Z")); got != MarketOpen {
		t.Errorf("expected open after break, got %s", got)
	}
	// The break must not be applied on Sunday, whose 17:00 is the weekly open.
	if got := mc.Status(utc(t, "2025-07-06T21:30:00Z")); got != MarketOpen {
		t.Errorf("sunday 17:30 NY should be open, got %s", got)
	}
}

func TestMarketStatus_Holiday(t *testing.T) {
	cal := ForexMetalsCalendar()
	cal.Holidays["2025-12-25"] = "Christmas Day"
	mc, err := NewMarketClock(cal)
	if err != nil {
		t.Fatalf("NewMarketClock: %v", err)
	}
	// Thursday 2025-12-25 would otherwise be a normal trading day.
	if got := mc.Status(utc(t, "2025-12-25T15:00:00Z")); got != MarketClosedHoliday {
		t.Errorf("expected holiday closure, got %s", got)
	}
	if got := mc.Status(utc(t, "2025-12-24T15:00:00Z")); got != MarketOpen {
		t.Errorf("expected open on 24th, got %s", got)
	}
}

func TestActiveSessions_DSTIndependence(t *testing.T) {
	mc := mustClock(t)

	// 13:00 UTC in July: London is BST (14:00 local, session open) and New
	// York is EDT (09:00 local, session open) -> overlap.
	sessions := mc.ActiveSessions(utc(t, "2025-07-08T13:00:00Z"))
	if !containsSession(sessions, SessionLondon) || !containsSession(sessions, SessionNewYork) {
		t.Fatalf("expected London and New York active in July, got %v", sessions)
	}
	if !containsSession(sessions, SessionOverlap) {
		t.Errorf("expected overlap session, got %v", sessions)
	}

	// 13:00 UTC in January: London is GMT (13:00 local, still open) but New
	// York is EST (08:00 local, just open) -> also overlap, by a different
	// arithmetic path. The 07:00 UTC case separates them.
	winter := mc.ActiveSessions(utc(t, "2025-01-08T07:00:00Z"))
	if containsSession(winter, SessionNewYork) {
		t.Errorf("New York should not be active at 02:00 EST, got %v", winter)
	}
}

func TestActiveSessions_ClosedMarketHasNoSessions(t *testing.T) {
	mc := mustClock(t)
	got := mc.ActiveSessions(utc(t, "2025-07-05T12:00:00Z")) // Saturday
	if len(got) != 1 || got[0] != SessionNoActive {
		t.Errorf("expected no active sessions on Saturday, got %v", got)
	}
}

func TestNextClose(t *testing.T) {
	mc := mustClock(t)
	// From Thursday midday (EDT), the next closure is Thursday's 17:00 NY break.
	next, ok := mc.NextClose(utc(t, "2025-07-10T12:00:00Z"))
	if !ok {
		t.Fatal("expected a next close")
	}
	if want := utc(t, "2025-07-10T21:00:00Z"); !next.Equal(want) {
		t.Errorf("NextClose = %s, want %s", next, want)
	}
	// When the market is already closed there is no "next close".
	if _, ok := mc.NextClose(utc(t, "2025-07-05T12:00:00Z")); ok {
		t.Error("expected no next close while closed")
	}
}

func containsSession(list []SessionName, want SessionName) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
