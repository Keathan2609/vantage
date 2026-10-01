package orchestrator

import (
	"time"

	"github.com/vantage/control-api/internal/domain"
)

// skipAtNewestBar records a refusal against the newest bar in hand, or without
// one when there are no bars at all.
//
// The per-bar unique indexes on `strategy_runs` are PARTIAL —
// `WHERE bar_time IS NOT NULL` — so a refusal recorded with a nil bar time is
// inserted afresh on every scheduler tick rather than deduplicated. At a
// 30-second interval an hourly bar is 120 ticks, so a condition that persists
// across a day writes about 23 900 rows for twelve strategies instead of a few
// hundred, and `PurgeStrategyRunsInRange` cannot delete them because it only
// matches rows that HAVE a bar time.
//
// Every refusal that happens after the bars are loaded therefore keys on one.
// The pre-flight refusals above genuinely cannot: they run before any bar is
// fetched, and inventing a key for them would be worse than the duplication.
func skipAtNewestBar(
	skip func(string) (Outcome, error),
	skipAtBar func(string, *time.Time) (Outcome, error),
	bars []domain.Bar,
	reason string,
) (Outcome, error) {
	if len(bars) == 0 {
		return skip(reason)
	}
	at := bars[len(bars)-1].OpenTime
	return skipAtBar(reason, &at)
}
