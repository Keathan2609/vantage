package domain

import (
	"fmt"
	"time"
)

// Clock abstracts wall-clock time so every time-dependent decision is testable.
// Production uses SystemClock; tests use a fixed clock. Nothing in the trading
// path calls time.Now() directly.
type Clock interface {
	Now() time.Time
}

// SystemClock returns real UTC time.
type SystemClock struct{}

// Now returns the current instant in UTC.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FixedClock returns a constant instant, for deterministic tests.
type FixedClock struct{ T time.Time }

// Now returns the fixed instant.
func (c FixedClock) Now() time.Time { return c.T.UTC() }

// MarketStatus describes whether an instrument may trade at an instant.
type MarketStatus string

const (
	MarketOpen          MarketStatus = "open"
	MarketClosedWeekend MarketStatus = "closed_weekend"
	MarketClosedHoliday MarketStatus = "closed_holiday"
	MarketClosedBreak   MarketStatus = "closed_daily_break"
)

// Tradable reports whether orders may be sent.
func (s MarketStatus) Tradable() bool { return s == MarketOpen }

// SessionName identifies a liquidity session.
type SessionName string

const (
	SessionSydney   SessionName = "sydney"
	SessionTokyo    SessionName = "tokyo"
	SessionLondon   SessionName = "london"
	SessionNewYork  SessionName = "new_york"
	SessionOverlap  SessionName = "london_new_york_overlap"
	SessionNoActive SessionName = "none"
)

// clockTime is a wall-clock time of day in some named timezone.
type clockTime struct {
	Hour   int
	Minute int
}

func (c clockTime) minutes() int { return c.Hour*60 + c.Minute }

// SessionWindow is a recurring daily window defined in a local timezone. The
// timezone matters: London's session moves relative to UTC twice a year, and a
// system that hard-codes UTC offsets trades the wrong hours for weeks at a time.
type SessionWindow struct {
	Name     SessionName
	Location string // IANA timezone, e.g. "Europe/London"
	Start    clockTime
	End      clockTime
}

// MarketCalendar defines when an instrument's market is open.
//
// The default calendar models the retail FX/metals convention: the week opens
// Sunday evening and closes Friday evening in New York time, with a short daily
// maintenance break at the rollover. Because the boundaries are defined in New
// York local time, they shift correctly across US daylight-saving transitions.
type MarketCalendar struct {
	ID       string
	Name     string
	Location string // IANA timezone the week boundaries are expressed in

	WeekOpenDay    time.Weekday
	WeekOpenTime   clockTime
	WeekCloseDay   time.Weekday
	WeekCloseTime  clockTime
	DailyBreakFrom clockTime
	DailyBreakTo   clockTime
	HasDailyBreak  bool

	// Holidays are full closure dates expressed as YYYY-MM-DD in Location.
	Holidays map[string]string // date -> human-readable reason

	Sessions []SessionWindow
}

// ForexMetalsCalendar returns the calendar used by XAUUSD and major FX pairs.
// Holidays are seeded from configuration rather than hard-coded here, so the
// list can be maintained without a code change.
func ForexMetalsCalendar() MarketCalendar {
	return MarketCalendar{
		ID:             "fx_metals_24x5",
		Name:           "FX & Metals 24x5",
		Location:       "America/New_York",
		WeekOpenDay:    time.Sunday,
		WeekOpenTime:   clockTime{Hour: 17, Minute: 0},
		WeekCloseDay:   time.Friday,
		WeekCloseTime:  clockTime{Hour: 17, Minute: 0},
		DailyBreakFrom: clockTime{Hour: 17, Minute: 0},
		DailyBreakTo:   clockTime{Hour: 18, Minute: 0},
		HasDailyBreak:  true,
		Holidays:       map[string]string{},
		Sessions: []SessionWindow{
			{Name: SessionSydney, Location: "Australia/Sydney", Start: clockTime{7, 0}, End: clockTime{16, 0}},
			{Name: SessionTokyo, Location: "Asia/Tokyo", Start: clockTime{9, 0}, End: clockTime{18, 0}},
			{Name: SessionLondon, Location: "Europe/London", Start: clockTime{8, 0}, End: clockTime{16, 30}},
			{Name: SessionNewYork, Location: "America/New_York", Start: clockTime{8, 0}, End: clockTime{17, 0}},
		},
	}
}

// MarketClock answers session and status questions for a calendar. It resolves
// IANA timezones once at construction so per-call lookups cannot fail.
type MarketClock struct {
	cal     MarketCalendar
	loc     *time.Location
	sessLoc map[SessionName]*time.Location
}

// NewMarketClock builds a clock, loading every timezone the calendar needs.
// The binary imports time/tzdata so this works identically on Windows, where
// no system zoneinfo database exists.
func NewMarketClock(cal MarketCalendar) (*MarketClock, error) {
	loc, err := time.LoadLocation(cal.Location)
	if err != nil {
		return nil, fmt.Errorf("market clock: load %q: %w", cal.Location, err)
	}
	sessLoc := make(map[SessionName]*time.Location, len(cal.Sessions))
	for _, s := range cal.Sessions {
		l, err := time.LoadLocation(s.Location)
		if err != nil {
			return nil, fmt.Errorf("market clock: load session zone %q: %w", s.Location, err)
		}
		sessLoc[s.Name] = l
	}
	return &MarketClock{cal: cal, loc: loc, sessLoc: sessLoc}, nil
}

// Calendar exposes the underlying calendar.
func (mc *MarketClock) Calendar() MarketCalendar { return mc.cal }

// Status reports whether the market is open at instant t. The instant may be
// supplied in any zone; it is converted to the calendar's zone first.
func (mc *MarketClock) Status(t time.Time) MarketStatus {
	local := t.In(mc.loc)

	if reason, ok := mc.cal.Holidays[local.Format("2006-01-02")]; ok && reason != "" {
		return MarketClosedHoliday
	}
	if !mc.withinTradingWeek(local) {
		return MarketClosedWeekend
	}
	if mc.cal.HasDailyBreak && mc.withinDailyBreak(local) {
		return MarketClosedBreak
	}
	return MarketOpen
}

// withinTradingWeek reports whether local falls inside the Sunday-open to
// Friday-close window. Comparison is done on minutes-since-week-start so the
// wrap-around at Sunday is handled without special cases.
func (mc *MarketClock) withinTradingWeek(local time.Time) bool {
	cur := weekMinutes(local.Weekday(), local.Hour(), local.Minute())
	open := weekMinutes(mc.cal.WeekOpenDay, mc.cal.WeekOpenTime.Hour, mc.cal.WeekOpenTime.Minute)
	closeM := weekMinutes(mc.cal.WeekCloseDay, mc.cal.WeekCloseTime.Hour, mc.cal.WeekCloseTime.Minute)

	if open <= closeM {
		return cur >= open && cur < closeM
	}
	// Window wraps across the week boundary (Sunday 17:00 -> Friday 17:00).
	return cur >= open || cur < closeM
}

func (mc *MarketClock) withinDailyBreak(local time.Time) bool {
	// The break is only meaningful on days that are otherwise trading days.
	cur := local.Hour()*60 + local.Minute()
	from := mc.cal.DailyBreakFrom.minutes()
	to := mc.cal.DailyBreakTo.minutes()
	if from == to {
		return false
	}
	// Friday's 17:00 boundary is the weekly close, already handled above, and
	// Sunday's 17:00 is the weekly open, so the break only applies Mon-Thu.
	switch local.Weekday() {
	case time.Friday, time.Saturday, time.Sunday:
		return false
	}
	if from < to {
		return cur >= from && cur < to
	}
	return cur >= from || cur < to
}

func weekMinutes(d time.Weekday, hour, minute int) int {
	return int(d)*24*60 + hour*60 + minute
}

// ActiveSessions returns the liquidity sessions live at instant t. Sessions are
// evaluated in their own local timezones, so London and New York shift with
// their respective DST rules independently.
func (mc *MarketClock) ActiveSessions(t time.Time) []SessionName {
	var out []SessionName
	london, newYork := false, false
	for _, s := range mc.cal.Sessions {
		loc, ok := mc.sessLoc[s.Name]
		if !ok {
			continue
		}
		local := t.In(loc)
		cur := local.Hour()*60 + local.Minute()
		start, end := s.Start.minutes(), s.End.minutes()
		active := false
		if start <= end {
			active = cur >= start && cur < end
		} else {
			active = cur >= start || cur < end
		}
		// A session is only meaningful while the market itself is open.
		if active && mc.Status(t) == MarketOpen {
			out = append(out, s.Name)
			if s.Name == SessionLondon {
				london = true
			}
			if s.Name == SessionNewYork {
				newYork = true
			}
		}
	}
	if london && newYork {
		out = append(out, SessionOverlap)
	}
	if len(out) == 0 {
		out = append(out, SessionNoActive)
	}
	return out
}

// NextClose returns the next instant at which the market closes, searching
// forward minute by minute up to a week. It is used to warn before the weekly
// close, when holding a position through a gap carries elevated risk.
func (mc *MarketClock) NextClose(from time.Time) (time.Time, bool) {
	if mc.Status(from) != MarketOpen {
		return time.Time{}, false
	}
	cursor := from.UTC().Truncate(time.Minute)
	for i := 0; i < 7*24*60; i++ {
		cursor = cursor.Add(time.Minute)
		if mc.Status(cursor) != MarketOpen {
			return cursor, true
		}
	}
	return time.Time{}, false
}
