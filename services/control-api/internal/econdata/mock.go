package econdata

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// SourceName is the source recorded on every row these providers produce.
//
// It is "seed" rather than something that could pass for a vendor, so a
// fixture is identifiable as a fixture in the database, in the API and in the
// News table's Source column.
const SourceName = "seed"

// MockCalendar is a deterministic development calendar.
//
// Events are anchored to the current UTC day at fixed times of day rather than
// to "now plus an offset". That makes a refresh IDEMPOTENT: running the
// ingestor repeatedly leaves the schedule where it was, instead of pushing
// every release a few minutes further into the future each time.
//
// One release is deliberately placed close to the current time so the
// event-risk blackout is observable in a fresh environment without waiting for
// a real release.
type MockCalendar struct {
	now func() time.Time
}

// NewMockCalendar builds the fixture calendar provider.
//
// It takes a clock rather than reading the wall clock, so a test can place the
// schedule wherever it needs it.
func NewMockCalendar(now func() time.Time) *MockCalendar {
	return &MockCalendar{now: now}
}

// Name implements CalendarProvider.
func (c *MockCalendar) Name() string { return SourceName }

// calendarFixture is one entry in the fixture schedule.
//
// dayOffset is in days from the current UTC day; hour and minute are UTC times
// of day. Together they give a stable schedule spanning released and upcoming
// events.
type calendarFixture struct {
	dayOffset int
	hour      int
	minute    int
	country   string
	currency  string
	impact    string
	name      string
	category  string
	actual    string
	forecast  string
	previous  string
}

var calendarFixtures = []calendarFixture{
	{-1, 12, 30, "United States", "USD", "high", "Core CPI (MoM)", "inflation", "0.3%", "0.2%", "0.2%"},
	{-1, 8, 0, "Euro Area", "EUR", "medium", "Flash Manufacturing PMI", "pmi", "46.1", "46.5", "45.8"},
	{0, 12, 30, "United States", "USD", "medium", "Initial Jobless Claims", "employment", "221K", "218K", "215K"},
	{0, 14, 0, "United States", "USD", "high", "Non-Farm Payrolls", "employment", "", "185K", "227K"},
	{0, 18, 0, "United States", "USD", "high", "FOMC Interest Rate Decision", "central_bank", "", "4.50%", "4.50%"},
	{1, 13, 0, "South Africa", "ZAR", "medium", "SARB Repo Rate Decision", "central_bank", "", "7.75%", "7.75%"},
	{2, 12, 30, "United States", "USD", "high", "Core PCE Price Index (YoY)", "inflation", "", "2.8%", "2.8%"},
	{3, 12, 30, "United States", "USD", "medium", "Retail Sales (MoM)", "consumer", "", "0.4%", "0.7%"},
	{4, 12, 15, "Euro Area", "EUR", "high", "ECB Main Refinancing Rate", "central_bank", "", "3.15%", "3.15%"},
	{5, 12, 30, "United States", "USD", "low", "Trade Balance", "trade", "", "-73.8B", "-78.2B"},
	{6, 12, 30, "United States", "USD", "medium", "PPI (MoM)", "inflation", "", "0.2%", "0.4%"},
	{7, 12, 30, "United States", "USD", "high", "GDP (QoQ, Advance)", "growth", "", "2.3%", "3.1%"},
}

// Events implements CalendarProvider.
//
// Status is derived from the schedule rather than stored: an event whose time
// has passed and which has a figure is released; everything else is scheduled.
// Deriving it means a fixture set cannot drift into claiming a release
// happened in the future.
func (c *MockCalendar) Events(ctx context.Context, from, to time.Time) ([]Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	reference := c.now().UTC()
	anchor := time.Date(reference.Year(), reference.Month(), reference.Day(), 0, 0, 0, 0, time.UTC)

	// A nearby release, placed relative to the reference instant so the
	// blackout window is reachable immediately in a fresh environment.
	imminent := reference.Truncate(time.Minute).Add(20 * time.Minute)

	out := make([]Event, 0, len(calendarFixtures)+1)
	for i, f := range calendarFixtures {
		scheduled := anchor.AddDate(0, 0, f.dayOffset).
			Add(time.Duration(f.hour)*time.Hour + time.Duration(f.minute)*time.Minute)
		if scheduled.Before(from) || !scheduled.Before(to) {
			continue
		}

		event := Event{
			ExternalID:  fmt.Sprintf("seed-%03d", i),
			ScheduledAt: scheduled,
			Country:     f.country,
			Currency:    f.currency,
			Impact:      f.impact,
			Name:        f.name,
			Category:    f.category,
			Forecast:    f.forecast,
			Previous:    f.previous,
			Status:      "scheduled",
		}
		if f.actual != "" && scheduled.Before(reference) {
			event.Actual = f.actual
			event.Status = "released"
			released := scheduled
			event.ReleasedAt = &released
		}
		out = append(out, event)
	}

	if !imminent.Before(from) && imminent.Before(to) {
		out = append(out, Event{
			ExternalID:  "seed-imminent",
			ScheduledAt: imminent,
			Country:     "United States",
			Currency:    "USD",
			Impact:      "high",
			Name:        "Fed Chair Press Conference",
			Category:    "central_bank",
			Status:      "scheduled",
		})
	}

	return out, nil
}

// MockNews is a deterministic development news provider.
//
// The headlines are invented for development and attributed to the fixture
// source. Sentiment is a fixed number per item, not a model output, and
// nothing in the trading path consumes it.
type MockNews struct {
	now func() time.Time
}

// NewMockNews builds the fixture news provider.
func NewMockNews(now func() time.Time) *MockNews { return &MockNews{now: now} }

// Name implements NewsProvider.
func (n *MockNews) Name() string { return SourceName }

type newsFixture struct {
	hoursAgo    int
	headline    string
	summary     string
	instruments []string
	currencies  []string
	sentiment   string
	topics      []string
}

var newsFixtures = []newsFixture{
	{1, "Gold holds range ahead of payrolls as traders trim positions",
		"Metals traded in a narrow band with participants reluctant to carry exposure into the release.",
		[]string{"XAUUSD", "XAUUSD.m", "XAGUSD"}, []string{"USD"}, "0.05",
		[]string{"metals", "positioning"}},
	{3, "Dollar index steadies after softer inflation print",
		"The move lower in yields stalled as markets reassessed the path of policy.",
		[]string{"EURUSD", "XAUUSD"}, []string{"USD", "EUR"}, "-0.20",
		[]string{"fx", "inflation"}},
	{8, "Central bank commentary keeps rate-cut timing in question",
		"Officials repeated that decisions remain data-dependent, offering little new guidance.",
		[]string{"XAUUSD"}, []string{"USD"}, "0.00",
		[]string{"central_bank", "policy"}},
	{14, "Rand firms as commodity terms of trade improve",
		"Local assets drew support from stronger export prices.",
		[]string{"USDZAR"}, []string{"ZAR", "USD"}, "0.30",
		[]string{"emerging_markets", "fx"}},
	{22, "Silver outpaces gold as industrial demand expectations firm",
		"The gold-silver ratio compressed for a third session.",
		[]string{"XAGUSD", "XAUUSD"}, []string{"USD"}, "0.35",
		[]string{"metals", "industrial_demand"}},
	{30, "Euro area activity data continues to undershoot expectations",
		"Survey measures pointed to persistent weakness in manufacturing.",
		[]string{"EURUSD"}, []string{"EUR"}, "-0.40",
		[]string{"growth", "pmi"}},
}

// Items implements NewsProvider.
//
// Publication times are anchored to the top of the hour so repeated refreshes
// are idempotent, for the same reason the calendar is day-anchored.
func (n *MockNews) Items(ctx context.Context, since time.Time, limit int) ([]Headline, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	reference := n.now().UTC().Truncate(time.Hour)
	model := "seed-fixture"

	out := make([]Headline, 0, len(newsFixtures))
	for i, f := range newsFixtures {
		if limit > 0 && len(out) >= limit {
			break
		}
		published := reference.Add(-time.Duration(f.hoursAgo) * time.Hour)
		if published.Before(since) {
			continue
		}
		sentiment, err := decimal.NewFromString(f.sentiment)
		if err != nil {
			return nil, fmt.Errorf("econdata: fixture sentiment %q: %w", f.sentiment, err)
		}
		out = append(out, Headline{
			ExternalID:         fmt.Sprintf("seed-news-%03d", i),
			Headline:           f.headline,
			Summary:            f.summary,
			PublishedAt:        published,
			RelatedInstruments: f.instruments,
			RelatedCurrencies:  f.currencies,
			Sentiment:          &sentiment,
			SentimentModel:     model,
			Topics:             f.topics,
		})
	}
	return out, nil
}
