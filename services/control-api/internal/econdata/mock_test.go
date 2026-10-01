package econdata

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

var one = decimal.NewFromInt(1)

var refNow = time.Date(2026, 3, 12, 14, 0, 0, 0, time.UTC)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestCalendarIsIdempotentAcrossRefreshes(t *testing.T) {
	// The schedule must not shift when the ingestor runs again a few minutes
	// later. An earlier design anchored events to "now plus an offset", which
	// pushed every release further into the future on every refresh.
	calendar := NewMockCalendar(fixedClock(refNow))
	later := NewMockCalendar(fixedClock(refNow.Add(7 * time.Minute)))

	from, to := refNow.Add(-7*24*time.Hour), refNow.Add(14*24*time.Hour)

	first, err := calendar.Events(context.Background(), from, to)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	second, err := later.Events(context.Background(), from, to)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}

	scheduled := map[string]time.Time{}
	for _, e := range first {
		scheduled[e.ExternalID] = e.ScheduledAt
	}
	for _, e := range second {
		if e.ExternalID == "seed-imminent" {
			// The deliberately-near release tracks the clock; that is its job.
			continue
		}
		was, ok := scheduled[e.ExternalID]
		if !ok {
			t.Fatalf("event %s appeared only on the second fetch", e.ExternalID)
		}
		if !was.Equal(e.ScheduledAt) {
			t.Fatalf("event %s moved between refreshes: %s then %s",
				e.ExternalID, was, e.ScheduledAt)
		}
	}
}

func TestCalendarRespectsTheRequestedWindow(t *testing.T) {
	calendar := NewMockCalendar(fixedClock(refNow))
	from, to := refNow, refNow.Add(24*time.Hour)

	events, err := calendar.Events(context.Background(), from, to)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least one event in a 24-hour window")
	}
	for _, e := range events {
		if e.ScheduledAt.Before(from) || !e.ScheduledAt.Before(to) {
			t.Fatalf("event %s at %s is outside the requested window %s..%s",
				e.ExternalID, e.ScheduledAt, from, to)
		}
	}
}

// Status is derived, so a fixture cannot claim a release happened in the
// future -- which would corrupt any point-in-time research built on it.
func TestNoEventClaimsAFutureRelease(t *testing.T) {
	calendar := NewMockCalendar(fixedClock(refNow))
	events, err := calendar.Events(context.Background(),
		refNow.Add(-7*24*time.Hour), refNow.Add(14*24*time.Hour))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	for _, e := range events {
		if e.Status == "released" {
			if e.Actual == "" {
				t.Fatalf("event %s is released but carries no figure", e.ExternalID)
			}
			if !e.ScheduledAt.Before(refNow) {
				t.Fatalf("event %s is marked released but is scheduled at %s, after now",
					e.ExternalID, e.ScheduledAt)
			}
			if e.ReleasedAt == nil {
				t.Fatalf("released event %s has no release timestamp", e.ExternalID)
			}
		}
		if e.Status == "scheduled" && e.Actual != "" {
			t.Fatalf("event %s is scheduled but already carries an actual figure", e.ExternalID)
		}
	}
}

func TestEveryEventCarriesTheFixtureSource(t *testing.T) {
	calendar := NewMockCalendar(fixedClock(refNow))
	news := NewMockNews(fixedClock(refNow))

	if calendar.Name() != SourceName {
		t.Fatalf("calendar source is %q, expected %q", calendar.Name(), SourceName)
	}
	if news.Name() != SourceName {
		t.Fatalf("news source is %q, expected %q", news.Name(), SourceName)
	}
}

func TestEventImpactsAreValid(t *testing.T) {
	calendar := NewMockCalendar(fixedClock(refNow))
	events, err := calendar.Events(context.Background(),
		refNow.Add(-7*24*time.Hour), refNow.Add(14*24*time.Hour))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	valid := map[string]bool{"low": true, "medium": true, "high": true}
	high := 0
	for _, e := range events {
		if !valid[e.Impact] {
			t.Fatalf("event %s has impact %q, which is not a recognised level",
				e.ExternalID, e.Impact)
		}
		if e.Impact == "high" {
			high++
		}
		if e.Currency == "" || e.Name == "" {
			t.Fatalf("event %s is missing a currency or a name", e.ExternalID)
		}
	}
	if high == 0 {
		t.Fatal("the fixture calendar must contain a high-impact event, " +
			"otherwise the event-risk blackout is never exercised")
	}
}

func TestNewsIsIdempotentAndWithinTheWindow(t *testing.T) {
	first := NewMockNews(fixedClock(refNow))
	second := NewMockNews(fixedClock(refNow.Add(30 * time.Second)))
	since := refNow.Add(-3 * 24 * time.Hour)

	a, err := first.Items(context.Background(), since, 100)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	b, err := second.Items(context.Background(), since, 100)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if len(a) != len(b) {
		t.Fatalf("headline count changed between refreshes: %d then %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ExternalID != b[i].ExternalID {
			t.Fatalf("headline order changed: %s then %s", a[i].ExternalID, b[i].ExternalID)
		}
		if !a[i].PublishedAt.Equal(b[i].PublishedAt) {
			t.Fatalf("headline %s moved: %s then %s",
				a[i].ExternalID, a[i].PublishedAt, b[i].PublishedAt)
		}
		if a[i].PublishedAt.Before(since) {
			t.Fatalf("headline %s is older than the requested window", a[i].ExternalID)
		}
	}
}

func TestNewsRespectsTheLimit(t *testing.T) {
	news := NewMockNews(fixedClock(refNow))
	items, err := news.Items(context.Background(), refNow.Add(-3*24*time.Hour), 2)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(items) > 2 {
		t.Fatalf("limit of 2 returned %d headlines", len(items))
	}
}

func TestNewsCarriesSentimentAsANumberWithAModelName(t *testing.T) {
	news := NewMockNews(fixedClock(refNow))
	items, err := news.Items(context.Background(), refNow.Add(-3*24*time.Hour), 100)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected headlines")
	}
	for _, item := range items {
		if item.Sentiment == nil {
			continue
		}
		// A sentiment score with no stated model is an unattributable number.
		if item.SentimentModel == "" {
			t.Fatalf("headline %s carries a sentiment score with no model name",
				item.ExternalID)
		}
		if item.Sentiment.Abs().GreaterThan(one) {
			t.Fatalf("headline %s has sentiment %s outside [-1,1]",
				item.ExternalID, item.Sentiment)
		}
	}
}

func TestCancelledContextIsHonoured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewMockCalendar(fixedClock(refNow)).Events(ctx, refNow, refNow.Add(time.Hour)); err == nil {
		t.Fatal("expected the calendar provider to honour a cancelled context")
	}
	if _, err := NewMockNews(fixedClock(refNow)).Items(ctx, refNow, 10); err == nil {
		t.Fatal("expected the news provider to honour a cancelled context")
	}
}
