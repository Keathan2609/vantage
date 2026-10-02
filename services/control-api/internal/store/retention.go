package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vantage/control-api/internal/db"
)

// Retention for the tables that are pure telemetry.
//
// # What may be pruned, and what may never be
//
// Ten tables carry an append-only trigger that raises on DELETE --
// transactions, fills, order_state_transitions, audit_events,
// decision_snapshots, reconciliation_issue_events and the four *_history
// tables. Those are the financial and evidentiary record and the database
// refuses to let this file touch them. They need an ARCHIVE design, which is a
// separate decision, not a cleanup job.
//
// What is pruned here is the subset with no production reader at all, or whose
// only reader is bounded to recent rows. Each window below is a separate
// judgement with its reason written next to it, rather than one global
// constant, because the consequence of being wrong differs per table.
//
// # reconciliation_runs is NOT pruned, and must not be
//
// It is the most obvious candidate for a fourth sweep and the most dangerous
// one. A row is written every five minutes whether or not anything happened,
// so it grows with uptime exactly like the three tables above, and adding it
// here looks like finishing the job.
//
// It is not. The foreign keys run:
//
//	reconciliation_issues       -> reconciliation_runs    ON DELETE CASCADE
//	reconciliation_issue_events -> reconciliation_issues  ON DELETE CASCADE
//	fills                       -> reconciliation_issues  ON DELETE SET NULL
//
// So deleting an old run deletes the ISSUES raised by that run. An unresolved
// issue is an open halt and an operator's work queue, and rule 7's corollary
// is that "not re-detected" must mean "fixed", never "no longer examined" --
// a retention sweep that removed one would release an account's halt as a side
// effect of housekeeping, which is the precise failure that corollary exists
// to prevent.
//
// The database refuses it today: the cascade reaches the append-only
// reconciliation_issue_events and the trigger aborts the transaction with
// "table reconciliation_issue_events is append-only". That was confirmed by
// running the DELETE inside a transaction and rolling back. Note that the
// trigger is a backstop, not the reason -- it catches the second-order effect,
// while the thing that makes this wrong is the first-order one.
//
// Bounding this table therefore means deciding what happens to a run's issues
// first, which is an archive design and not a line in this file.
//
// # Why batched
//
// market_quotes gains roughly 179 000 rows a day. The first run against a
// database that has been up for months would otherwise issue one DELETE over
// millions of rows, holding locks and a transaction open for as long as it
// takes. Each call removes at most maxRowsPerSweep and returns; the hourly
// cleanup job comes back an hour later and takes the next slice. Steady state
// is far below the cap.
const maxRowsPerSweep = 50_000

// PruneQuoteHistory removes raw tick history older than `before`.
//
// `market_quotes` is WRITE-ONLY in production: the tree contains exactly one
// INSERT, one replay-scoped DELETE, and no SELECT. Nothing reads it, no
// decision depends on it, and the hot path for a current price is
// `market_quotes_latest`, which this does not touch. `market_data_health` is
// written only when a quote fails validation and is read only by the replay
// cleanup.
func (s *MarketStore) PruneQuoteHistory(ctx context.Context, before time.Time) (int64, error) {
	return pruneByTimestamp(ctx, s.pool, []pruneTarget{
		{table: "market_quotes", column: "ingested_at"},
		{table: "market_data_health", column: "evaluated_at"},
	}, before)
}

// PrunePublishedOutbox removes outbox rows that were successfully published.
//
// Only rows with a published_at: an unpublished row is still owed to a
// consumer and the dispatcher selects exactly those. Deleting one would drop
// an event that was never delivered.
func (s *TradingStore) PrunePublishedOutbox(ctx context.Context, before time.Time) (int64, error) {
	var removed int64
	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, fmt.Sprintf(`
			DELETE FROM outbox WHERE ctid IN (
				SELECT ctid FROM outbox
				WHERE published_at IS NOT NULL AND published_at < $1
				LIMIT %d
			)`, maxRowsPerSweep), before)
		if err != nil {
			return mapError(err)
		}
		removed = tag.RowsAffected()
		return nil
	})
	return removed, err
}

// PruneStrategyRuns removes old evaluation records.
//
// `StrategyActivity` reads this table for a recent window only. The rows
// removed here are the ones no view asks for, and the table is the one that
// grows fastest when something is wrong: a halted account records a skip per
// strategy per instrument every 30 seconds.
//
// Deliberately NOT scoped to a status. A long-running account accumulates
// succeeded and no_signal rows at the same cadence as skipped ones, and
// keeping one class forever while pruning another would make the activity view
// a biased sample of its own history.
func (s *ResearchStore) PruneStrategyRuns(ctx context.Context, before time.Time) (int64, error) {
	return pruneByTimestamp(ctx, s.pool, []pruneTarget{
		{table: "strategy_runs", column: "started_at"},
	}, before)
}

type pruneTarget struct {
	table  string
	column string
}

// pruneByTimestamp deletes up to maxRowsPerSweep rows per target.
//
// The table and column names are compile-time literals from the callers above
// and never reach here from a request; `before` is the only value a caller
// supplies and it is bound. This is the one place in the store that formats
// an identifier into SQL, and it is formatted from a constant.
func pruneByTimestamp(ctx context.Context, pool *db.Pool, targets []pruneTarget,
	before time.Time) (int64, error) {

	var removed int64
	err := pool.InTx(ctx, func(tx pgx.Tx) error {
		removed = 0
		for _, t := range targets {
			tag, err := tx.Exec(ctx, fmt.Sprintf(`
				DELETE FROM %s WHERE ctid IN (
					SELECT ctid FROM %s WHERE %s < $1 LIMIT %d
				)`, t.table, t.table, t.column, maxRowsPerSweep), before)
			if err != nil {
				return mapError(err)
			}
			removed += tag.RowsAffected()
		}
		return nil
	})
	return removed, err
}
