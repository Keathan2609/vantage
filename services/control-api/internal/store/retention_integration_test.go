// Retention, executed against a real database rather than reasoned about.
//
// # Why this suite exists
//
// The three prune functions are the only thing standing between a long-running
// deployment and a database that grows until the disk fills. They were written,
// wired into the hourly cleanup lease, and never executed by a test. Their
// failures are logged and swallowed on purpose -- a housekeeping sweep must not
// abandon the daily roll it shares a lease with -- which means a prune that
// deleted nothing, or deleted the wrong thing, would say so once an hour in a
// log nobody reads and otherwise look exactly like a prune that worked.
//
// Two properties here have real consequences and neither is obvious from
// reading the SQL:
//
//   - An UNPUBLISHED outbox row is still owed to a consumer. Pruning one drops
//     an event that was never delivered, and nothing downstream would ever ask
//     for it again. The sweep must spare it however old it is.
//   - Pruning strategy_runs must take strategy_signals with it. The foreign key
//     says ON DELETE CASCADE, so it does; if someone ever changes that to
//     RESTRICT the sweep starts failing with a foreign-key violation that is
//     caught, logged and swallowed, and the table quietly grows forever.
//
// These run as the APP role, which is the role the scheduler uses. That also
// makes this the only test that would notice if the role lost DELETE on one of
// these tables: the prune would fail with a permission error, in production,
// into a swallowed log line.
//
//	$env:VANTAGE_STORE_E2E     = "1"
//	$env:VANTAGE_DATABASE_URL  = "postgres://..."
package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vantage/control-api/internal/db"
)

// The fixtures live in the distant past, and the cutoff sits between them.
//
// Deliberately decades before any real row. A prune is not scoped to this
// test's own rows -- it deletes everything older than the cutoff it is given --
// so a cutoff anywhere near the present would delete the development
// database's real history as a side effect of running the tests. At this
// distance the only rows on either side of the line are the ones below.
var (
	retentionAncient = time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	retentionCutoff  = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	retentionRecent  = time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)
)

// retentionMarker tags every row this suite creates so cleanup can find them
// and no assertion can be satisfied by a row somebody else wrote.
const retentionMarker = "retention-integration-test"

func retentionStore(t *testing.T) (context.Context, *Store, *db.Pool) {
	t.Helper()
	if os.Getenv("VANTAGE_STORE_E2E") != "1" {
		t.Skip("set VANTAGE_STORE_E2E=1 and VANTAGE_DATABASE_URL to run retention tests")
	}
	url := os.Getenv("VANTAGE_DATABASE_URL")
	if url == "" {
		t.Skip("VANTAGE_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Skipf("no database at the configured URL (%v); start it with ./scripts/dev-up.ps1", err)
	}
	t.Cleanup(pool.Close)
	return ctx, New(pool), pool
}

// anInstrument returns a seeded instrument id, because market_quotes and
// strategy_runs both have a foreign key to one. A test that invented its own
// would not be exercising the platform's universe.
func anInstrument(ctx context.Context, t *testing.T, pool *db.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `SELECT id FROM instruments ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Skipf("no instruments; seed the database first (%v)", err)
	}
	return id
}

func TestPruningQuoteHistoryRemovesOldRowsAndKeepsRecentOnes(t *testing.T) {
	ctx, s, pool := retentionStore(t)
	instrument := anInstrument(ctx, t, pool)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM market_quotes WHERE provider = $1`, retentionMarker)
	})

	for _, at := range []time.Time{retentionAncient, retentionRecent} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO market_quotes (instrument_id, bid, ask, source_time, ingested_at, provider)
			VALUES ($1, 1, 2, $2, $2, $3)`, instrument, at, retentionMarker); err != nil {
			t.Fatalf("insert quote at %s: %v", at, err)
		}
	}

	if _, err := s.Market.PruneQuoteHistory(ctx, retentionCutoff); err != nil {
		t.Fatalf("PruneQuoteHistory: %v", err)
	}

	var ancient, recent int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE ingested_at = $2),
		       count(*) FILTER (WHERE ingested_at = $3)
		FROM market_quotes WHERE provider = $1`,
		retentionMarker, retentionAncient, retentionRecent).Scan(&ancient, &recent); err != nil {
		t.Fatalf("count quotes: %v", err)
	}
	if ancient != 0 {
		t.Errorf("a quote older than the cutoff survived the sweep (%d rows)", ancient)
	}
	if recent != 1 {
		t.Errorf("a quote newer than the cutoff was deleted: got %d rows, want 1", recent)
	}
}

// TestPruningTheOutboxSparesUndeliveredEvents is the one with consequences.
//
// An unpublished row is an event the platform still owes somebody. It has no
// published_at precisely because nothing has consumed it yet, and the
// dispatcher selects exactly those rows. Age is not evidence of delivery, so
// an old unpublished row must survive a sweep that removes its published
// neighbour of the same age.
func TestPruningTheOutboxSparesUndeliveredEvents(t *testing.T) {
	ctx, s, pool := retentionStore(t)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM outbox WHERE aggregate_type = $1`, retentionMarker)
	})

	// Three rows, all with the same created_at. The only difference between the
	// first two is whether they were ever published.
	rows := []struct {
		name      string
		published any
	}{
		{"old and published", retentionAncient},
		{"old and NEVER published", nil},
	}
	for _, r := range rows {
		if _, err := pool.Exec(ctx, `
			INSERT INTO outbox (aggregate_type, aggregate_id, event_type, payload,
			                    created_at, available_at, published_at)
			VALUES ($1, $2, 'test.event', '{}'::jsonb, $3, $3, $4)`,
			retentionMarker, r.name, retentionAncient, r.published); err != nil {
			t.Fatalf("insert outbox row %q: %v", r.name, err)
		}
	}
	// A published row on the safe side of the cutoff, to prove the sweep is
	// bounded by time and not deleting every published row it can see.
	if _, err := pool.Exec(ctx, `
		INSERT INTO outbox (aggregate_type, aggregate_id, event_type, payload,
		                    created_at, available_at, published_at)
		VALUES ($1, 'recent and published', 'test.event', '{}'::jsonb, $2, $2, $2)`,
		retentionMarker, retentionRecent); err != nil {
		t.Fatalf("insert recent outbox row: %v", err)
	}

	if _, err := s.Trading.PrunePublishedOutbox(ctx, retentionCutoff); err != nil {
		t.Fatalf("PrunePublishedOutbox: %v", err)
	}

	survivors := map[string]bool{}
	cursor, err := pool.Query(ctx,
		`SELECT aggregate_id FROM outbox WHERE aggregate_type = $1`, retentionMarker)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer cursor.Close()
	for cursor.Next() {
		var id string
		if err := cursor.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		survivors[id] = true
	}

	if survivors["old and published"] {
		t.Error("an old PUBLISHED event survived; the sweep did not prune")
	}
	if !survivors["old and NEVER published"] {
		t.Error("an UNDELIVERED event was deleted. It is owed to a consumer and " +
			"nothing will ever ask for it again")
	}
	if !survivors["recent and published"] {
		t.Error("a published event newer than the cutoff was deleted; the sweep " +
			"is not bounded by time")
	}
}

// TestPruningStrategyRunsTakesTheirSignalsWithThem pins the cascade.
//
// strategy_signals has no prune of its own and no retention window. It is
// bounded solely by ON DELETE CASCADE on run_id. If that is ever relaxed, this
// test fails here rather than in production, where the symptom would be an
// hourly foreign-key violation written to a log and swallowed, and a table that
// grows for ever while the sweep reports success.
func TestPruningStrategyRunsTakesTheirSignalsWithThem(t *testing.T) {
	ctx, s, pool := retentionStore(t)
	instrument := anInstrument(ctx, t, pool)

	var strategyID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM strategies ORDER BY id LIMIT 1`).Scan(&strategyID); err != nil {
		t.Skipf("no strategies; seed the database first (%v)", err)
	}

	var runIDs []uuid.UUID
	t.Cleanup(func() {
		for _, id := range runIDs {
			_, _ = pool.Exec(ctx, `DELETE FROM strategy_runs WHERE id = $1`, id)
		}
	})

	insert := func(startedAt time.Time) uuid.UUID {
		t.Helper()
		var runID uuid.UUID
		if err := pool.QueryRow(ctx, `
			INSERT INTO strategy_runs (strategy_id, strategy_version, instrument_id,
			                           timeframe, status, started_at)
			VALUES ($1, 1, $2, '1h', 'no_signal', $3) RETURNING id`,
			strategyID, instrument, startedAt).Scan(&runID); err != nil {
			t.Fatalf("insert strategy_run at %s: %v", startedAt, err)
		}
		runIDs = append(runIDs, runID)
		if _, err := pool.Exec(ctx, `
			INSERT INTO strategy_signals (run_id, strategy_id, strategy_version,
			                              instrument_id, timeframe, action, confidence, bar_time)
			VALUES ($1, $2, 1, $3, '1h', 'no_trade', 0, $4)`,
			runID, strategyID, instrument, startedAt); err != nil {
			t.Fatalf("insert strategy_signal: %v", err)
		}
		return runID
	}

	oldRun := insert(retentionAncient)
	recentRun := insert(retentionRecent)

	if _, err := s.Research.PruneStrategyRuns(ctx, retentionCutoff); err != nil {
		t.Fatalf("PruneStrategyRuns: %v", err)
	}

	count := func(table, column string, id uuid.UUID) int {
		t.Helper()
		var n int
		q := `SELECT count(*) FROM ` + table + ` WHERE ` + column + ` = $1`
		if err := pool.QueryRow(ctx, q, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}

	if n := count("strategy_runs", "id", oldRun); n != 0 {
		t.Errorf("a run older than the cutoff survived the sweep")
	}
	if n := count("strategy_signals", "run_id", oldRun); n != 0 {
		t.Errorf("%d signal(s) outlived the pruned run they belong to. The cascade "+
			"is the ONLY thing bounding strategy_signals", n)
	}
	if n := count("strategy_runs", "id", recentRun); n != 1 {
		t.Errorf("a run newer than the cutoff was deleted")
	}
	if n := count("strategy_signals", "run_id", recentRun); n != 1 {
		t.Errorf("a signal belonging to a surviving run was deleted")
	}
}
