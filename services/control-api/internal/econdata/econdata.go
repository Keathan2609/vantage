// Package econdata supplies the economic calendar and news through provider
// interfaces, with deterministic development implementations.
//
// The abstraction exists for the same reason the market-data one does: the
// implementation is expected to change (a licensed calendar feed, a news
// vendor) and nothing above these interfaces should know or care which one is
// in use.
//
// Two properties are deliberate:
//
//   - Every record carries its SOURCE, and the source travels all the way to
//     the interface. Fixture data that cannot be distinguished from a licensed
//     feed is worse than no data, because an operator would plan around it.
//   - Refresh is idempotent. Records are keyed by (source, external_id) and
//     upserted, so running the ingestor a hundred times produces the same
//     rows rather than a hundred copies of the calendar.
//
// No provider here scrapes a third-party site, and none is permitted to. A
// licensed feed is a new implementation of these interfaces plus credentials;
// it is not a scraper.
package econdata

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/store"
)

// Event is one scheduled macroeconomic release, as a provider reports it.
//
// Actual, Forecast and Previous are STRINGS. Releases are published in mixed
// units — "4.50%", "185K", "-73.8B" — and coercing them to numbers discards
// the unit, after which something will eventually compare payrolls of "185K"
// to the number 185.
type Event struct {
	ExternalID  string
	ScheduledAt time.Time
	Country     string
	Currency    string
	Impact      string // low | medium | high
	Name        string
	Category    string
	Actual      string
	Forecast    string
	Previous    string
	Status      string // scheduled | released | revised
	ReleasedAt  *time.Time
}

// Headline is one news item as a provider reports it.
type Headline struct {
	ExternalID         string
	Headline           string
	Summary            string
	URL                string
	PublishedAt        time.Time
	RelatedInstruments []string
	RelatedCurrencies  []string
	Sentiment          *decimal.Decimal
	SentimentModel     string
	Topics             []string
}

// CalendarProvider supplies scheduled economic releases.
type CalendarProvider interface {
	// Name identifies the provider and is stored as the source on every row.
	Name() string
	// Events returns releases scheduled within a window, in any order.
	Events(ctx context.Context, from, to time.Time) ([]Event, error)
}

// NewsProvider supplies headlines.
type NewsProvider interface {
	Name() string
	// Items returns headlines published at or after since.
	Items(ctx context.Context, since time.Time, limit int) ([]Headline, error)
}

// Ingestor pulls from both providers and records what they return.
type Ingestor struct {
	store    *store.Store
	calendar CalendarProvider
	news     NewsProvider
}

// NewIngestor builds an ingestor.
func NewIngestor(s *store.Store, calendar CalendarProvider, news NewsProvider) *Ingestor {
	return &Ingestor{store: s, calendar: calendar, news: news}
}

// CalendarProviderName and NewsProviderName report which implementations are
// in use, for the connections endpoint.
func (i *Ingestor) CalendarProviderName() string { return i.calendar.Name() }
func (i *Ingestor) NewsProviderName() string     { return i.news.Name() }

// Result summarises one refresh.
type Result struct {
	Events    int
	Headlines int
}

// Refresh fetches and upserts both feeds.
//
// The window reaches back as well as forward: released figures matter for
// point-in-time research, and a calendar that only shows the future cannot
// explain a decision made yesterday.
func (i *Ingestor) Refresh(ctx context.Context, now time.Time) (Result, error) {
	var out Result

	events, err := i.calendar.Events(ctx, now.Add(-7*24*time.Hour), now.Add(14*24*time.Hour))
	if err != nil {
		return out, fmt.Errorf("econdata: fetch calendar: %w", err)
	}
	for _, e := range events {
		row := store.EconomicEvent{
			ExternalID:  e.ExternalID,
			ScheduledAt: e.ScheduledAt,
			Country:     e.Country,
			Currency:    e.Currency,
			Impact:      e.Impact,
			EventName:   e.Name,
			Status:      e.Status,
			Source:      i.calendar.Name(),
			ReleasedAt:  e.ReleasedAt,
		}
		if e.Category != "" {
			c := e.Category
			row.Category = &c
		}
		if e.Actual != "" {
			a := e.Actual
			row.Actual = &a
		}
		if e.Forecast != "" {
			f := e.Forecast
			row.Forecast = &f
		}
		if e.Previous != "" {
			p := e.Previous
			row.Previous = &p
		}
		if err := i.store.Research.UpsertEconomicEvent(ctx, row); err != nil {
			return out, fmt.Errorf("econdata: upsert event %q: %w", e.Name, err)
		}
		out.Events++
	}

	headlines, err := i.news.Items(ctx, now.Add(-3*24*time.Hour), 100)
	if err != nil {
		return out, fmt.Errorf("econdata: fetch news: %w", err)
	}
	for _, h := range headlines {
		row := store.NewsItem{
			ExternalID:         h.ExternalID,
			Source:             i.news.Name(),
			Headline:           h.Headline,
			PublishedAt:        h.PublishedAt,
			RelatedInstruments: h.RelatedInstruments,
			RelatedCurrencies:  h.RelatedCurrencies,
			Sentiment:          h.Sentiment,
			Topics:             h.Topics,
		}
		if h.Summary != "" {
			s := h.Summary
			row.Summary = &s
		}
		if h.URL != "" {
			u := h.URL
			row.URL = &u
		}
		if h.SentimentModel != "" {
			m := h.SentimentModel
			row.SentimentModel = &m
		}
		if err := i.store.Research.UpsertNewsItem(ctx, row); err != nil {
			return out, fmt.Errorf("econdata: upsert news %q: %w", h.ExternalID, err)
		}
		out.Headlines++
	}

	return out, nil
}

// Run refreshes on an interval until the context ends.
//
// A failed refresh is logged and retried on the next tick. Stale calendar data
// is a degradation, not a reason to stop the service — and the event-risk check
// reads what is in the database, so a failed refresh leaves the last known
// calendar in force rather than silently removing the blackouts.
func (i *Ingestor) Run(ctx context.Context, every time.Duration, now func() time.Time) {
	log := logging.FromContext(ctx)

	refresh := func() {
		result, err := i.Refresh(ctx, now())
		if err != nil {
			log.Warn("calendar and news refresh failed", "error", err.Error())
			return
		}
		log.Debug("calendar and news refreshed",
			"events", result.Events, "headlines", result.Headlines)
	}

	refresh()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}
