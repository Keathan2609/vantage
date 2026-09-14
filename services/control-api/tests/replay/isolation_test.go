package replay

import (
	"fmt"
	"strings"
	"testing"
)

// Replay isolation, driven against the running stack.
//
// # The contamination this proves is gone
//
// A strategy is evaluated over "the last 300 bars the store holds", and a
// replay's market-data purge removes only the rows the replay provided --
// deliberately, so it does not destroy the seeded fixture. So a replay's
// decisions were taken over a window that MIXED seeded history with dataset
// bars, and the regime, the indicators and the features partly described the
// seed.
//
// Measured before the fix: a deliberately range-bound fixture was classified
// TRENDING twelve times and RANGING never, while the same close series
// classified in isolation gave ADX 11.2 and RANGING. The dataset was
// range-bound; the window it was judged in was not.
//
// The test the brief asks for is the direct one: seed strongly trending
// history, replay a RANGE dataset, and require that the seed does not reach
// the result -- then the inverse.

// seedBarStats describes what the SEEDED history looks like for an instrument,
// so a test can assert that the seed is genuinely different from the dataset it
// is meant not to contaminate.
func seedBarStats(t *testing.T, instrument, timeframe string) (count int, direction string) {
	t.Helper()
	count = psqlInt(t, fmt.Sprintf(`
		SELECT count(*) FROM market_bars
		WHERE instrument_id = '%s' AND timeframe = '%s' AND provider <> 'replay'`,
		instrument, timeframe))
	if count == 0 {
		return 0, "none"
	}
	// Net displacement over the seeded series: positive is an uptrend.
	net := psql(t, fmt.Sprintf(`
		SELECT round(
			(max(close) FILTER (WHERE rn = 1) - max(close) FILTER (WHERE rn = total))::numeric, 4)
		FROM (
			SELECT close,
			       row_number() OVER (ORDER BY open_time DESC) AS rn,
			       count(*) OVER () AS total
			FROM market_bars
			WHERE instrument_id = '%s' AND timeframe = '%s' AND provider <> 'replay'
		) s`, instrument, timeframe))
	return count, net
}

func TestAReplayIsNotContaminatedBySeededHistory(t *testing.T) {
	h := newHarness(t)

	// The seeded history exists and is substantial: without it this test
	// proves nothing, because there would be nothing to contaminate with.
	seeded, direction := seedBarStats(t, "XAUUSD.m", "1h")
	if seeded < 100 {
		t.Skipf("only %d seeded 1h bars for XAUUSD.m; there is no history to "+
			"contaminate with, so this test would pass vacuously", seeded)
	}
	t.Logf("seeded history: %d bars, net close change %s", seeded, direction)

	for _, tc := range []struct {
		dataset string
		// wantMajority is the regime the DATASET should produce when it is the
		// only history in view.
		wantMajority string
		forbidden    string
	}{
		// A range-bound dataset must not be read as trending, whatever the
		// seeded history did. This is the exact case that failed before.
		{dataset: "range-bound", wantMajority: "RANGING", forbidden: "TRENDING"},
		// The inverse, as the brief requires: a trending dataset must not be
		// read as ranging.
		{dataset: "trend-clean", wantMajority: "TRENDING", forbidden: "RANGING"},
	} {
		tc := tc
		t.Run(tc.dataset, func(t *testing.T) {
			h.t = t
			t.Cleanup(func() {
				h.t = t
				h.do("POST", "/api/v1/replay/control", map[string]any{
					"action": "stop", "reason": "isolation teardown",
				}, nil)
			})

			before := h.observe()
			run := h.start(tc.dataset, "proving isolation from seeded history")
			h.step(90)

			// The PRIMARY assertion, made while the run is still active so the
			// replay's own bars have not yet been purged.
			//
			// Asserted on the DATA rather than on the resulting regime,
			// because a dataset that produces no actionable signal -- which a
			// range is entitled to do -- would leave nothing to compare and
			// the test would pass by doing nothing.
			// Plain to_char with no embedded literal: the quoting has to survive
			// a shell round trip to psql, and an escaped 'T' did not.
			warmupStart := psql(t, `SELECT to_char(warmup_start AT TIME ZONE 'UTC',
				'YYYY-MM-DD HH24:MI:SS') FROM replay_runs ORDER BY started_at DESC LIMIT 1`)
			if warmupStart == "" {
				t.Fatal("the run recorded no warm-up start")
			}

			visible := fmt.Sprintf(`FROM market_bars
				WHERE instrument_id = 'XAUUSD.m' AND timeframe = '1h' AND complete
				  AND open_time >= '%s'`, warmupStart)

			total := psqlInt(t, "SELECT count(*) "+visible)
			fromSeed := psqlInt(t, "SELECT count(*) "+visible+" AND provider <> 'replay'")
			belowFloor := psqlInt(t, `SELECT count(*) FROM market_bars
				WHERE instrument_id = 'XAUUSD.m' AND timeframe = '1h'
				  AND provider <> 'replay'`)

			if total == 0 {
				t.Fatalf("the run made no bars visible above its own floor, so the "+
					"isolation cannot be judged (warm-up start %s)", warmupStart)
			}
			if fromSeed != 0 {
				t.Errorf("%d of the %d bars visible to this run came from OUTSIDE the "+
					"declared dataset. A replay's decisions must rest only on its own "+
					"declared data, or the result depends on what happened to be "+
					"seeded.", fromSeed, total)
			}
			if belowFloor == 0 {
				t.Errorf("there are no seeded bars below the floor, so this test " +
					"proves nothing: there was nothing to exclude")
			}
			t.Logf("run %s: %d bars visible above the floor, all from the replay; "+
				"%d seeded bars correctly excluded", run.ID, total, belowFloor)

			o := h.observe().since(before)

			// The regime comparison is a SECONDARY check, made only when the
			// dataset actually produced decisions. A range is entitled to
			// produce none.
			got := o.Regimes[tc.wantMajority]
			bad := o.Regimes[tc.forbidden]
			if o.Decisions > 0 && got <= bad {
				t.Errorf("run %s on %s recorded %s %d times and %s %d times.\n"+
					"The dataset is %s by construction.\n%s",
					run.ID, tc.dataset, tc.wantMajority, got, tc.forbidden, bad,
					tc.wantMajority, o.describe())
			}
		})
	}
}

func TestTheWarmupProducesNoOrders(t *testing.T) {
	// Warm-up must build state and trade nothing. A replay that traded its
	// first bar would be trading indicator noise -- ADX means nothing for
	// fourteen periods -- and calling that a result is how a backtest flatters
	// itself.
	h := newHarness(t)
	t.Cleanup(func() {
		h.do("POST", "/api/v1/replay/control", map[string]any{
			"action": "stop", "reason": "warm-up test teardown",
		}, nil)
	})

	before := h.observe()
	h.start("trend-clean", "proving that warm-up produces no executable intent")

	// Fewer steps than the declared warm-up, so the whole run is warm-up.
	h.step(30)
	o := h.observe().since(before)

	if o.Orders != 0 {
		t.Errorf("%d orders were created during the warm-up phase\n%s",
			o.Orders, o.describe())
	}
	if o.Fills != 0 {
		t.Errorf("%d fills occurred during the warm-up phase\n%s", o.Fills, o.describe())
	}
	if o.Ledger != 0 {
		t.Errorf("the warm-up wrote %d ledger entries\n%s", o.Ledger, o.describe())
	}

	// And the warm-up must have BUILT something, or "no orders" is satisfied by
	// a pipeline that was not running at all -- which would make this test pass
	// while proving nothing.
	//
	// Asserted on ingested bars rather than on strategy_runs: the orchestrator
	// returns early for a skip, BEFORE it records a run, so a strategy that
	// skipped for want of history or for warm-up leaves no row at all. That is
	// a real visibility gap, recorded as one in the report rather than
	// something this test can assert around.
	replayBars := psqlInt(t, `SELECT count(*) FROM market_bars WHERE provider = 'replay'`)
	if replayBars == 0 {
		t.Errorf("the warm-up ingested no bars, so no state was built and the "+
			"absence of orders proves nothing\n%s", o.describe())
	}
	t.Logf("warm-up ingested %d replay bars and created no orders, fills or "+
		"ledger entries", replayBars)
}

func TestTheRunRecordsItsDeclaredWindow(t *testing.T) {
	// A result is only reproducible from inputs that were written down.
	h := newHarness(t)
	t.Cleanup(func() {
		h.do("POST", "/api/v1/replay/control", map[string]any{
			"action": "stop", "reason": "window record teardown",
		}, nil)
	})

	h.start("trend-clean", "checking that the window reaches the run record")

	var status struct {
		Run struct {
			WarmupStart     string `json:"warmup_start"`
			EvaluationStart string `json:"evaluation_start"`
			EvaluationEnd   string `json:"evaluation_end"`
		} `json:"run"`
	}
	h.do("GET", "/api/v1/replay", nil, &status)

	for name, value := range map[string]string{
		"warmup_start":     status.Run.WarmupStart,
		"evaluation_start": status.Run.EvaluationStart,
		"evaluation_end":   status.Run.EvaluationEnd,
	} {
		if value == "" || strings.HasPrefix(value, "0001-01-01") {
			t.Errorf("the run record does not carry %s (got %q)", name, value)
		}
	}
	if status.Run.EvaluationStart <= status.Run.WarmupStart {
		t.Errorf("evaluation starts %s, not after the warm-up start %s",
			status.Run.EvaluationStart, status.Run.WarmupStart)
	}
}
