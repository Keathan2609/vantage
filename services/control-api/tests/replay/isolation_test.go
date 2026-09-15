package replay

import (
	"fmt"
	"testing"

	"github.com/shopspring/decimal"
)

// Replay isolation, proved rather than asserted.
//
// # The finding this closes
//
// A previous milestone recorded, honestly and without a fix: "the regime is
// classified over a window that mixes seeded history with dataset bars". A
// replay whose indicators, regime and features partly describe whatever
// happened to be in the database is not reproducible research -- the same
// dataset would classify differently on two machines, and the difference would
// be attributed to the market.
//
// `ReplayWindow.Floor()` is the control: every historical read is bounded at
// the run's declared `warmup_start`, and the store expresses it as
// `open_time >= $4`. These tests are the evidence that the control binds, and
// they are deliberately hostile -- each plants 400 bars of the OPPOSITE market
// shape immediately before the floor, which is more than the 300-bar window a
// strategy reads. If the floor leaked at all, the window would be entirely
// contaminating bars and the classification would flip.
//
// # Why the probe bars are not 'replay' provider
//
// `PurgeReplayMarketData` deletes `provider = 'replay'` rows at the start of
// every run, so planting replay-provider bars would prove only that the purge
// works. These are planted under their own provider AFTER the run starts, so
// they survive the purge and sit in the table for the whole run. That is the
// seeded-history case exactly: data the replay does not own and cannot delete.

// contaminationProvider marks the planted bars so they are identifiable,
// removable, and obviously not part of any dataset.
const contaminationProvider = "isolation-probe"

// plantBarsBefore inserts `count` hourly bars ending just before `before`,
// following a deterministic shape.
//
// shape "trend" rises monotonically; shape "range" oscillates inside a tight
// band. Both are far more extreme than any fixture, so a classifier that saw
// them could not report anything else.
func plantBarsBefore(t *testing.T, instrument, before, shape string, count int) {
	t.Helper()

	// A rising close for the trend, a sine-like oscillation for the range.
	// Written as SQL so the bars are generated where they are stored: a Go
	// loop issuing 400 inserts would be slower and no clearer.
	var closeExpr string
	switch shape {
	case "trend":
		// 2000 -> 2400 over the series: a 20% move in 400 hours, which no
		// range classifier could read as ranging.
		closeExpr = "2000 + (i * 1.0)"
	case "range":
		// Oscillates between 2495 and 2505 with no drift at all.
		closeExpr = "2500 + (5 * sin(i::numeric / 3))"
	default:
		t.Fatalf("unknown shape %q", shape)
	}

	psql(t, fmt.Sprintf(`
		INSERT INTO market_bars (instrument_id, timeframe, open_time, close_time,
		                         open, high, low, close, volume, complete, provider)
		SELECT '%s', '1h',
		       ts, ts + interval '1 hour',
		       %s, %s + 1.5, %s - 1.5, %s,
		       1000, TRUE, '%s'
		FROM (
			SELECT timestamptz '%s' - (i || ' hours')::interval AS ts, (%d - i) AS i
			FROM generate_series(1, %d) AS i
		) s
		ON CONFLICT DO NOTHING`,
		instrument, closeExpr, closeExpr, closeExpr, closeExpr,
		contaminationProvider, before, count, count))
}

func removePlantedBars(t *testing.T) {
	t.Helper()
	psql(t, fmt.Sprintf(
		`DELETE FROM market_bars WHERE provider = '%s'`, contaminationProvider))
}

// warmupStartOf reads the run's declared floor.
func warmupStartOf(t *testing.T, runID string) string {
	t.Helper()
	return psql(t, fmt.Sprintf(
		`SELECT to_char(warmup_start AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SSZ')
		 FROM replay_runs WHERE id = '%s'`, runID))
}

func TestSeededHistoryCannotReachAReplaysClassification(t *testing.T) {
	// The brief's test, both ways round. Each case plants the OPPOSITE shape
	// immediately before the floor and asserts the dataset's own shape still
	// wins.
	for _, c := range []struct {
		name       string
		dataset    string
		plantShape string
		wantRegime string
		// wrongRegime is what the planted bars would produce if they leaked.
		wrongRegime string
	}{
		{
			name:    "a trending seed does not make a range dataset look trending",
			dataset: "range-bound", plantShape: "trend",
			wantRegime: "RANGING", wrongRegime: "TRENDING",
		},
		{
			name:    "a ranging seed does not make a trend dataset look ranging",
			dataset: "trend-clean", plantShape: "range",
			wantRegime: "TRENDING", wrongRegime: "RANGING",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			t.Cleanup(func() { h.t = t; removePlantedBars(t); h.stopQuietly() })

			removePlantedBars(t)
			h.start(c.dataset, "replay isolation: "+c.name)
			runID := latestRunID(t)

			floor := warmupStartOf(t, runID)
			if floor == "" {
				t.Fatal("the run recorded no warmup_start, so it declares no floor " +
					"and nothing below can be checked")
			}

			// 400 bars is more than the 300-bar window a strategy reads, so a
			// leaking floor would fill the window entirely with these.
			const planted = 400
			plantBarsBefore(t, "XAUUSD.m", floor, c.plantShape, planted)

			// The probe must actually be in the table, or this test proves
			// nothing. Counted AFTER planting and BEFORE stepping.
			present := psqlInt(t, fmt.Sprintf(`
				SELECT count(*) FROM market_bars
				WHERE provider = '%s' AND open_time < timestamptz '%s'`,
				contaminationProvider, floor))
			if present < planted {
				t.Fatalf("only %d of %d probe bars were planted before the floor, "+
					"so the contamination this test exists to detect is not present",
					present, planted)
			}
			t.Logf("%d %s bars planted immediately before the %s floor",
				present, c.plantShape, floor)

			h.step(140)
			o := h.observe()

			if o.Decisions == 0 {
				t.Fatalf("the run produced no decision, so no classification was "+
					"made and the floor was never exercised\n%s", o.describe())
			}

			// 1. THE INVARIANT. The dataset's own shape is what was classified.
			//
			// Scoped to THIS RUN. `observed.Regimes` counts every decision in
			// the database, so the second case of this table saw the first
			// case's 77 RANGING decisions and reported them as contamination.
			// That was an assertion over the wrong population -- the same
			// mistake this suite has made before -- and it is why the count is
			// now taken from the run's own window rather than from the total.
			regimeInRun := func(regime string) int {
				return psqlInt(t, fmt.Sprintf(`
					SELECT count(*) FROM decision_snapshots
					WHERE regime = '%s'
					  AND created_at >= (SELECT started_at FROM replay_runs WHERE id = '%s')`,
					regime, runID))
			}
			wrong := regimeInRun(c.wrongRegime)
			right := regimeInRun(c.wantRegime)
			if wrong > 0 {
				t.Errorf("%d decisions IN THIS RUN were classified %s while "+
					"replaying a %s dataset with %d %s bars planted before the "+
					"floor.\nThe floor at %s did not bound the historical read, "+
					"so the classification describes data the run does not own.\n%s",
					wrong, c.wrongRegime, c.dataset, present, c.plantShape,
					floor, o.describe())
			}
			if right == 0 {
				t.Errorf("no decision in this run was classified %s on a %s "+
					"dataset; the regimes across the whole database were %v\n%s",
					c.wantRegime, c.dataset, o.Regimes, o.describe())
			}

			// 2. A SECOND, INDEPENDENT WITNESS that the floor bound the read.
			//
			// Early in a run the window holds only the handful of replay bars
			// produced so far, so strategies skip for insufficient history and
			// say how many bars they could see. If the 400 planted bars had
			// leaked in, the window would have been full from the first
			// instant and there would be no such skip at all.
			//
			// This is worth asserting separately because it cannot be
			// satisfied by a classifier that happens to be right: it is a
			// statement about what the query returned.
			shortWindows := psqlInt(t, fmt.Sprintf(`
				SELECT count(*) FROM strategy_runs
				WHERE started_at >= (SELECT started_at FROM replay_runs WHERE id = '%s')
				  AND skip_reason LIKE 'only %% complete bars available%%'`, runID))
			if shortWindows == 0 {
				t.Errorf("no strategy ever reported a short history window, yet the "+
					"run began with almost no replay bars. The %d planted bars "+
					"reached the 300-bar read.\n%s", present, o.describe())
			}
			t.Logf("scenario isolation: in this run %d decisions classified %s, "+
				"%d classified %s, and %d short-window skips prove the read was floored",
				right, c.wantRegime, wrong, c.wrongRegime, shortWindows)
		})
	}
}

func TestAReplayDeclaresEveryInputItCanBeReproducedFrom(t *testing.T) {
	// A run is only reproducible from inputs that were written down. This
	// asserts the record is complete rather than that it is correct -- the
	// values are checked by the determinism suite, which re-runs from them.
	h := newHarness(t)
	t.Cleanup(func() { h.t = t; h.stopQuietly() })

	h.start("trend-clean", "replay isolation: the run record must be complete")
	runID := latestRunID(t)

	for _, col := range []struct {
		name string
		why  string
	}{
		{"dataset_id", "which data was played"},
		{"dataset_hash", "that the data has not changed since"},
		{"code_sha", "which code produced the result"},
		{"config_hash", "which thresholds were in force"},
		{"seed", "the venue's random sequence"},
		{"warmup_start", "the floor on every historical read"},
		{"evaluation_start", "when executable intents became permissible"},
		{"evaluation_end", "where the run stops"},
		{"starting_balance", "what the account held before it"},
		{"starting_currency", "what that balance is denominated in"},
		{"risk_config_hash", "the account's ceilings"},
		{"authority_config_hash", "the authority's ceilings"},
		{"correlation_policy", "the correlation thresholds"},
		{"regime_policy", "the classifier's thresholds"},
		{"strategy_versions", "which strategy code ran"},
	} {
		got := psql(t, fmt.Sprintf(
			`SELECT coalesce(%s::text, '') FROM replay_runs WHERE id = '%s'`,
			col.name, runID))
		if got == "" {
			t.Errorf("the run records no %s, so a reader cannot tell %s",
				col.name, col.why)
		}
	}

	// starting_positions is `NOT NULL DEFAULT 0`, so its presence proves
	// nothing and is not asserted. That is a real gap and is recorded rather
	// than tested around: a run whose input gathering FAILED records 0
	// positions, which is indistinguishable from a flat account. The gatherer
	// is deliberately non-fatal -- a replay whose starting balance could not be
	// read is still a replay -- so the only honest fix is a separate
	// "inputs_gathered" flag, and that is noted in docs/MARKET_REPLAY.md
	// rather than invented here.
	//
	// What IS checkable: the balance is a parseable decimal rather than a
	// placeholder, so a reader can tell a recorded zero from an unrecorded one.
	balance := psql(t, fmt.Sprintf(
		`SELECT coalesce(starting_balance::text, '') FROM replay_runs WHERE id = '%s'`,
		runID))
	if _, err := decimal.NewFromString(balance); err != nil {
		t.Errorf("starting_balance %q is not a decimal, so the account's opening "+
			"state cannot be read back: %v", balance, err)
	}
}
