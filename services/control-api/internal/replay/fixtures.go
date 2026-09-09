package replay

import (
	"embed"
	"io/fs"
)

// Fixtures are the committed scenario datasets.
//
// # Why they are embedded and small
//
// A replay dataset is a test fixture, and a fixture that lives outside the
// binary is a fixture that can differ from the code that reads it. Embedding
// them means the dataset hash recorded on a run refers to something the build
// contains.
//
// They are deliberately TINY -- tens to low hundreds of bars each. A committed
// multi-year tick archive would dominate the repository, would be impossible
// to review in a diff, and would make every clone slower for the sake of data
// nobody reads. Each fixture is exactly long enough for the condition it
// represents to be visible to an indicator with a warm-up period.
//
// # Where a large dataset would live
//
// Not here. Real historical data belongs outside version control -- object
// storage with a content hash, referenced by that hash from a run record, and
// fetched on demand. The `Registry` is deliberately an allowlist of
// declarations rather than a directory scan, so adding an external source
// later means adding a loader, not loosening a path check. Documented in
// docs/MARKET_REPLAY.md.
//
//go:embed fixtures/*.csv
var fixtures embed.FS

// FixtureFS returns the embedded fixture filesystem.
func FixtureFS() fs.FS { return fixtures }

// Declared is the allowlist of fixtures this build can replay.
//
// Code rather than a directory listing on purpose: a stray CSV dropped into
// the fixtures directory must not become a runnable dataset, and a dataset ID
// arriving from an API request must resolve to something fixed at build time.
func Declared() []Declaration {
	return []Declaration{
		{
			ID:          "trend-clean",
			File:        "trend_clean.csv",
			Description: "Scenario A. A sustained uptrend with ordinary pullbacks, on which a trend strategy should be able to act.",
		},
		{
			ID:          "range-bound",
			File:        "range_bound.csv",
			Description: "Scenario B. Oscillation with no net drift, on which a trend strategy should not churn.",
		},
		{
			ID:          "volatility-shock",
			File:        "volatility_shock.csv",
			Description: "Scenario C. A calm first half and a violent second half, so risk and regime controls have something real to react to.",
		},
		{
			ID:          "spread-spike",
			File:        "spread_spike.csv",
			Description: "Scenario E. An unchanged price path with the spread widening to 2% of mid partway through.",
		},
		{
			ID:          "drawdown",
			File:        "drawdown.csv",
			Description: "Scenario H. A monotonic decline, so daily-loss and drawdown controls activate against a real loss.",
		},
		{
			ID:          "trend-reversal",
			File:        "trend_reversal.csv",
			Description: "Scenario K. A clean uptrend that reverses into a downtrend, so a strategy that entered correctly must stop adding risk.",
		},
		{
			ID:          "false-breakout",
			File:        "false_breakout.csv",
			Description: "Scenario L. A break of prior structure that immediately fails, exercising stop behaviour.",
		},
		{
			ID:          "correlated-pair",
			File:        "correlated_pair.csv",
			Description: "Scenario M. Two instruments trending together, so portfolio risk must constrain the combined exposure rather than treating them as diversification.",
		},
		{
			ID:          "day-boundary",
			File:        "day_boundary.csv",
			Description: "Scenario T. A series crossing a UTC day boundary and the venue's daily maintenance break, so daily risk resets can be checked against documented semantics.",
		},
	}
}

// LoadFixtures builds the registry from the embedded fixtures.
func LoadFixtures() (*Registry, error) {
	return LoadRegistry(fixtures, "fixtures", Declared())
}
