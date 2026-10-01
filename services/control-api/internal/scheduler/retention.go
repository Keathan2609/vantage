package scheduler

import (
	"context"
	"time"
)

// Retention windows.
//
// Separate constants rather than one number, because the consequence of being
// wrong differs per table and so should the judgement.
const (
	// quoteHistoryRetention bounds the raw tick log.
	//
	// market_quotes is write-only in production — one INSERT, no SELECT — and
	// gains roughly 179 000 rows a day at the 2-second ingest interval across
	// six instruments, about 27 MB a day and 10 GB a year. It is kept at all
	// because it is the record of what the platform actually saw, which is
	// worth having when reconstructing a decision; a month is long enough for
	// that and nothing reads further back.
	quoteHistoryRetention = 30 * 24 * time.Hour

	// strategyRunRetention bounds the evaluation log.
	//
	// Shorter than the quote log because it grows fastest exactly when
	// something is wrong: a halted account records a skip per strategy per
	// instrument every 30 seconds, roughly 23 900 rows a day. The activity
	// view reads a recent window only.
	strategyRunRetention = 14 * 24 * time.Hour

	// outboxRetention bounds PUBLISHED events only.
	//
	// An unpublished row is still owed to a consumer and is never touched.
	// A week is far beyond any retry schedule — the backoff caps at about 17
	// minutes — and leaves a published event visible long enough to answer
	// "was this dispatched?".
	outboxRetention = 7 * 24 * time.Hour
)

// pruneTelemetry trims the tables that grow with uptime rather than with
// activity.
//
// Failures are logged and swallowed on purpose. This runs inside the hourly
// cleanup lease alongside the daily roll and the account metrics, and a
// retention sweep that could not delete is a housekeeping problem — returning
// an error from here would abandon the roll, which is not housekeeping. The
// next hour tries again.
//
// The append-only tables are absent by design. Ten of them carry a trigger
// that raises on DELETE, and they are the financial and evidentiary record:
// transactions, fills, order_state_transitions, audit_events,
// decision_snapshots, reconciliation_issue_events and the four *_history
// tables. They need an archive design, which is a separate decision rather
// than a line in this function.
func (s *Scheduler) pruneTelemetry(ctx context.Context) {
	now := s.deps.Clock.Now()

	for _, sweep := range []struct {
		what string
		run  func() (int64, error)
	}{
		{"quote history", func() (int64, error) {
			return s.deps.Store.Market.PruneQuoteHistory(ctx, now.Add(-quoteHistoryRetention))
		}},
		{"strategy runs", func() (int64, error) {
			return s.deps.Store.Research.PruneStrategyRuns(ctx, now.Add(-strategyRunRetention))
		}},
		{"published outbox events", func() (int64, error) {
			return s.deps.Store.Trading.PrunePublishedOutbox(ctx, now.Add(-outboxRetention))
		}},
	} {
		removed, err := sweep.run()
		if err != nil {
			s.deps.Log.Error("retention sweep failed", "what", sweep.what, "error", err.Error())
			continue
		}
		if removed > 0 {
			// Logged at INFO with the count, so a sweep that is hitting its
			// per-run cap every hour — meaning the backlog is not shrinking —
			// is visible rather than silent.
			s.deps.Log.Info("pruned", "what", sweep.what, "rows", removed)
		}
	}
}
