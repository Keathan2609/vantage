package replay

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/vantage/control-api/internal/domain"
)

// A dataset is financial evidence: a replay's decisions, fills and P&L are
// only meaningful if the data behind them is exactly what it claims to be.
// These tests cover the two things that makes true -- validation that refuses
// a misleading fixture, and an identity that changes when a price changes and
// not otherwise.

const goodHeader = "instrument_id,timeframe,timestamp,open,high,low,close,volume," +
	"bid,ask,spread_fraction,session,source\n"

// row builds a valid line, so a test can change one field and assert only that.
func row(ts, open, high, low, close string) string {
	return "XAUUSD.m,1h," + ts + "," + open + "," + high + "," + low + "," + close +
		",1000,,,0.00012,london,test\n"
}

func datasetFS(body string) fstest.MapFS {
	return fstest.MapFS{"d.csv": &fstest.MapFile{Data: []byte(body)}}
}

func loadOK(t *testing.T, body string) Dataset {
	t.Helper()
	ds, err := LoadDataset(datasetFS(body), "d", "test dataset", "d.csv")
	if err != nil {
		t.Fatalf("LoadDataset: %v", err)
	}
	return ds
}

func loadErr(t *testing.T, body, because string) error {
	t.Helper()
	_, err := LoadDataset(datasetFS(body), "d", "test dataset", "d.csv")
	if err == nil {
		t.Fatalf("a dataset with %s was accepted", because)
	}
	return err
}

func TestAValidDatasetLoads(t *testing.T) {
	ds := loadOK(t, goodHeader+
		row("2027-03-02T04:00:00Z", "2650", "2660", "2640", "2655")+
		row("2027-03-02T05:00:00Z", "2655", "2665", "2650", "2660"))

	if len(ds.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(ds.Rows))
	}
	if ds.Hash == "" {
		t.Error("no hash was computed")
	}
	from, to := ds.Span()
	if from.Hour() != 4 || to.Hour() != 5 {
		t.Errorf("span = %s..%s", from, to)
	}
	if got := ds.Instruments(); len(got) != 1 || got[0] != "XAUUSD.m" {
		t.Errorf("instruments = %v", got)
	}
}

func TestTimestampsAreNormalisedToUTC(t *testing.T) {
	// A fixture written in a local zone would put bars in the wrong session,
	// and the failure would look like a strategy problem.
	ds := loadOK(t, goodHeader+
		"XAUUSD.m,1h,2027-03-02T09:00:00+05:00,2650,2660,2640,2655,1000,,,0.00012,london,test\n")

	got := ds.Rows[0].Timestamp
	if got.Location() != time.UTC {
		t.Fatalf("timestamp location = %s, want UTC", got.Location())
	}
	if got.Hour() != 4 {
		t.Errorf("timestamp = %s, want 04:00Z (09:00+05:00)", got.Format(time.RFC3339))
	}
}

func TestAWrongColumnOrderIsRefused(t *testing.T) {
	// A fixed order rather than a header map, on purpose: a column silently
	// landing in the wrong field because someone reordered a spreadsheet would
	// swap a high for a low without complaint.
	swapped := "instrument_id,timeframe,timestamp,high,open,low,close,volume," +
		"bid,ask,spread_fraction,session,source\n"
	err := loadErr(t, swapped+row("2027-03-02T04:00:00Z", "2650", "2660", "2640", "2655"),
		"a reordered header")
	if !strings.Contains(err.Error(), "column") {
		t.Errorf("error does not name the column: %v", err)
	}
}

func TestStructurallyImpossibleCandlesAreRefused(t *testing.T) {
	// Every one of these makes an indicator computed over the bar meaningless.
	cases := []struct {
		name  string
		line  string
		about string
	}{
		{"non-positive low", row("2027-03-02T04:00:00Z", "2650", "2660", "0", "2655"),
			"a zero low"},
		{"negative low", row("2027-03-02T04:00:00Z", "2650", "2660", "-5", "2655"),
			"a negative low"},
		{"high below low", row("2027-03-02T04:00:00Z", "2650", "2600", "2700", "2655"),
			"a high below its low"},
		{"high below close", row("2027-03-02T04:00:00Z", "2650", "2652", "2640", "2680"),
			"a high below its close"},
		{"low above open", row("2027-03-02T04:00:00Z", "2650", "2680", "2660", "2670"),
			"a low above its open"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			loadErr(t, goodHeader+c.line, c.about)
		})
	}
}

func TestACrossedOrHalfSpecifiedBookIsRefused(t *testing.T) {
	base := "XAUUSD.m,1h,2027-03-02T04:00:00Z,2650,2660,2640,2655,1000,"

	// Crossed: the data-quality policy would reject this quote anyway, and the
	// refusal would surface far from the fixture that caused it.
	loadErr(t, goodHeader+base+"2656,2654,0.0001,london,test\n", "a crossed book")

	// Half specified is worse than absent because it looks deliberate.
	loadErr(t, goodHeader+base+"2654,,0.0001,london,test\n", "a bid with no ask")
	loadErr(t, goodHeader+base+",2656,0.0001,london,test\n", "an ask with no bid")

	// Both present and sane is fine.
	loadOK(t, goodHeader+base+"2654.9,2655.1,0.0001,london,test\n")
}

func TestARowWithNoQuoteNeedsASpreadToDeriveOneFrom(t *testing.T) {
	// Otherwise the loader would have to invent a spread, and an invented
	// execution cost is the quietest way to make a replay result flattering.
	loadErr(t, goodHeader+
		"XAUUSD.m,1h,2027-03-02T04:00:00Z,2650,2660,2640,2655,1000,,,,london,test\n",
		"neither a book nor a spread")
}

func TestNonAdvancingTimestampsAreRefused(t *testing.T) {
	// Duplicate times make the step order ambiguous, and a replay's whole
	// value is that the order is total.
	loadErr(t, goodHeader+
		row("2027-03-02T04:00:00Z", "2650", "2660", "2640", "2655")+
		row("2027-03-02T04:00:00Z", "2655", "2665", "2650", "2660"),
		"two rows at the same instant")
}

func TestOutOfOrderRowsAreSortedRatherThanRefused(t *testing.T) {
	// Out of order is an inconvenience, not a lie. Duplicated times are a
	// genuine ambiguity and are refused above.
	ds := loadOK(t, goodHeader+
		row("2027-03-02T06:00:00Z", "2660", "2670", "2655", "2665")+
		row("2027-03-02T04:00:00Z", "2650", "2660", "2640", "2655")+
		row("2027-03-02T05:00:00Z", "2655", "2665", "2650", "2660"))

	for i := 1; i < len(ds.Rows); i++ {
		if !ds.Rows[i].Timestamp.After(ds.Rows[i-1].Timestamp) {
			t.Fatalf("rows are not sorted: %s then %s",
				ds.Rows[i-1].Timestamp, ds.Rows[i].Timestamp)
		}
	}
}

func TestAnUnknownTimeframeIsRefused(t *testing.T) {
	loadErr(t, goodHeader+
		"XAUUSD.m,7q,2027-03-02T04:00:00Z,2650,2660,2640,2655,1000,,,0.0001,london,test\n",
		"an unparseable timeframe")
}

func TestAnEmptyDatasetIsRefused(t *testing.T) {
	loadErr(t, goodHeader, "no rows")
}

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

func TestTheHashChangesWhenAPriceChanges(t *testing.T) {
	a := loadOK(t, goodHeader+row("2027-03-02T04:00:00Z", "2650", "2660", "2640", "2655"))
	b := loadOK(t, goodHeader+row("2027-03-02T04:00:00Z", "2650", "2660", "2640", "2655.01"))
	if a.Hash == b.Hash {
		t.Fatal("a changed close price did not change the dataset hash, so a run " +
			"record could not detect an edited fixture")
	}
}

func TestTheHashIgnoresFormattingAndRowOrder(t *testing.T) {
	// Hashing the PARSED rows rather than the file bytes: a line ending or a
	// reordered file should not invalidate every run that used the dataset.
	ordered := goodHeader +
		row("2027-03-02T04:00:00Z", "2650", "2660", "2640", "2655") +
		row("2027-03-02T05:00:00Z", "2655", "2665", "2650", "2660")
	shuffled := goodHeader +
		row("2027-03-02T05:00:00Z", "2655", "2665", "2650", "2660") +
		row("2027-03-02T04:00:00Z", "2650", "2660", "2640", "2655")

	if loadOK(t, ordered).Hash != loadOK(t, shuffled).Hash {
		t.Error("row order changed the dataset hash")
	}

	spaced := strings.ReplaceAll(ordered, ",1000,", ", 1000,")
	if loadOK(t, ordered).Hash != loadOK(t, spaced).Hash {
		t.Error("leading whitespace changed the dataset hash")
	}
}

func TestTheHashIsStableAcrossLoads(t *testing.T) {
	body := goodHeader + row("2027-03-02T04:00:00Z", "2650", "2660", "2640", "2655")
	first := loadOK(t, body).Hash
	for k := 0; k < 5; k++ {
		if got := loadOK(t, body).Hash; got != first {
			t.Fatalf("load %d produced hash %s, want %s", k, got, first)
		}
	}
}

// ---------------------------------------------------------------------------
// Bars
// ---------------------------------------------------------------------------

func TestBarsAreCompleteAndCarryTheReplayProvider(t *testing.T) {
	// Strategies read only complete bars, and a bar produced by a replay must
	// be identifiable as one afterwards -- research that cannot tell replayed
	// data from generated data will eventually treat one as the other.
	ds := loadOK(t, goodHeader+
		row("2027-03-02T04:00:00Z", "2650", "2660", "2640", "2655")+
		row("2027-03-02T05:00:00Z", "2655", "2665", "2650", "2660"))

	bars := ds.Bars("XAUUSD.m")
	if len(bars) != 2 {
		t.Fatalf("bars = %d, want 2", len(bars))
	}
	for i, b := range bars {
		if !b.Complete {
			t.Errorf("bar %d is not complete", i)
		}
		if b.Provider != "replay" {
			t.Errorf("bar %d provider = %q, want replay", i, b.Provider)
		}
		if !b.CloseTime.Equal(b.OpenTime.Add(time.Hour)) {
			t.Errorf("bar %d close time is not one interval after its open", i)
		}
	}
	if got := ds.Bars("NOT-PRESENT"); len(got) != 0 {
		t.Errorf("bars for an absent instrument = %d, want 0", len(got))
	}
}

// ---------------------------------------------------------------------------
// Registry: the allowlist
// ---------------------------------------------------------------------------

func TestTheRegistryResolvesOnlyDeclaredDatasets(t *testing.T) {
	// The registry is what makes a dataset id safe to accept from an API
	// request. Anything it does not hold must simply not resolve.
	fsys := fstest.MapFS{
		"fx/declared.csv": &fstest.MapFile{Data: []byte(goodHeader +
			row("2027-03-02T04:00:00Z", "2650", "2660", "2640", "2655"))},
		"fx/undeclared.csv": &fstest.MapFile{Data: []byte(goodHeader +
			row("2027-03-02T04:00:00Z", "2650", "2660", "2640", "2655"))},
	}
	reg, err := LoadRegistry(fsys, "fx", []Declaration{
		{ID: "declared", File: "declared.csv", Description: "the one in code"},
	})
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}

	if _, ok := reg.Get("declared"); !ok {
		t.Error("the declared dataset did not resolve")
	}
	// Present on the filesystem, absent from the declarations: a stray file
	// dropped into the fixtures directory must not become runnable.
	if _, ok := reg.Get("undeclared"); ok {
		t.Error("an undeclared dataset resolved")
	}
	for _, attempt := range []string{
		"../../../etc/passwd", "fx/declared.csv", "declared.csv",
		"", "DECLARED", "fx/../fx/declared",
	} {
		if _, ok := reg.Get(attempt); ok {
			t.Errorf("%q resolved to a dataset; ids are allowlist keys, not paths", attempt)
		}
	}
}

func TestTheRegistryRefusesDuplicateIDs(t *testing.T) {
	reg := NewRegistry()
	ds := loadOK(t, goodHeader+row("2027-03-02T04:00:00Z", "2650", "2660", "2640", "2655"))
	if err := reg.Add(ds); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := reg.Add(ds); err == nil {
		t.Error("a duplicate id was accepted, so one dataset could shadow another")
	}
	if err := reg.Add(Dataset{}); err == nil {
		t.Error("a dataset with no id was accepted")
	}
}

func TestRegistryListingIsStable(t *testing.T) {
	// The datasets endpoint is read by a person comparing runs. An order that
	// changed between requests would be noise.
	reg, err := LoadFixtures()
	if err != nil {
		t.Fatalf("LoadFixtures: %v", err)
	}
	first := strings.Join(reg.IDs(), ",")
	for k := 0; k < 5; k++ {
		if got := strings.Join(reg.IDs(), ","); got != first {
			t.Fatalf("listing %d differed:\n%s\n%s", k, got, first)
		}
	}
	ids := reg.IDs()
	ids[0] = "tampered"
	if reg.IDs()[0] == "tampered" {
		t.Error("mutating the returned slice changed the registry")
	}
}

// ---------------------------------------------------------------------------
// The committed fixtures
// ---------------------------------------------------------------------------

func TestEveryCommittedFixtureLoadsAndIsDeclared(t *testing.T) {
	reg, err := LoadFixtures()
	if err != nil {
		t.Fatalf("LoadFixtures: %v", err)
	}
	if len(reg.IDs()) != len(Declared()) {
		t.Fatalf("registry has %d datasets, want %d declared",
			len(reg.IDs()), len(Declared()))
	}
	for _, ds := range reg.List() {
		if len(ds.Rows) == 0 {
			t.Errorf("%s has no rows", ds.ID)
		}
		if ds.Description == "" {
			t.Errorf("%s has no description; the datasets endpoint would show a bare id", ds.ID)
		}
		if len(ds.Hash) != 64 {
			t.Errorf("%s hash is %d characters, want a full SHA-256", ds.ID, len(ds.Hash))
		}
	}
}

func TestTheFixturesAreDatedAheadOfAnyPlausibleSeed(t *testing.T) {
	// Not cosmetic. A dataset dated BEHIND the seeded market data makes every
	// replay quote read as timestamp_regressed -- correctly, since it is older
	// -- and the run is refused for a reason that looks like a broken clock.
	// This cost a debugging session; the comment in the fixture generator
	// explains it and this asserts it stays true.
	reg, err := LoadFixtures()
	if err != nil {
		t.Fatalf("LoadFixtures: %v", err)
	}
	floor := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, ds := range reg.List() {
		from, _ := ds.Span()
		if from.Before(floor) {
			t.Errorf("%s starts at %s, before %s. A dataset behind the seed makes every "+
				"quote look regressed", ds.ID, from.Format(time.RFC3339),
				floor.Format(time.RFC3339))
		}
	}
}

func TestTheFixturesAvoidClosedMarketPeriods(t *testing.T) {
	// A bar at a time the venue is shut would be a bar that could not have
	// happened, and a strategy acting on it would be trading the maintenance
	// window.
	clock, err := domain.NewMarketClock(domain.ForexMetalsCalendar())
	if err != nil {
		t.Fatalf("NewMarketClock: %v", err)
	}
	reg, err := LoadFixtures()
	if err != nil {
		t.Fatalf("LoadFixtures: %v", err)
	}
	for _, ds := range reg.List() {
		for _, r := range ds.Rows {
			if !clock.Status(r.Timestamp).Tradable() {
				t.Errorf("%s has a bar at %s, when the market is %s",
					ds.ID, r.Timestamp.Format(time.RFC3339), clock.Status(r.Timestamp))
				break
			}
		}
	}
}

func TestTheFixturesAreSmall(t *testing.T) {
	// A committed dataset is a fixture, not an archive. This is a guard
	// against the repository quietly acquiring a tick history.
	reg, err := LoadFixtures()
	if err != nil {
		t.Fatalf("LoadFixtures: %v", err)
	}
	const maxRows = 2000
	for _, ds := range reg.List() {
		if len(ds.Rows) > maxRows {
			t.Errorf("%s has %d rows. Large datasets belong in content-addressed "+
				"object storage, not in version control", ds.ID, len(ds.Rows))
		}
	}
}

// ---------------------------------------------------------------------------
// Speed
// ---------------------------------------------------------------------------

func TestSpeedParsing(t *testing.T) {
	for _, c := range []struct {
		in   string
		want float64
	}{
		{"", 1}, {"1x", 1}, {"1", 1}, {"10x", 10}, {"100x", 100},
		{"max", 0}, {"MAX", 0}, {" 10x ", 10}, {"2.5x", 2.5},
	} {
		got, err := ParseSpeed(c.in)
		if err != nil {
			t.Errorf("ParseSpeed(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSpeed(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"fast", "-1x", "0x", "abc"} {
		if _, err := ParseSpeed(bad); err == nil {
			t.Errorf("ParseSpeed(%q) was accepted", bad)
		}
	}
}
