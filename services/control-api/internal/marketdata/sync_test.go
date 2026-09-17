package marketdata

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
)

// Chunking, validation and hashing are pure and are tested here. The parts
// that need a database -- idempotent storage, coverage, gap repair -- are
// exercised by the integration suite against a real Postgres, because a
// fake store would prove only that the fake behaves.

func hour(h int) time.Time {
	return time.Date(2026, 1, 1, h, 0, 0, 0, time.UTC)
}

func TestChunkingCoversTheWholeRangeWithoutOverlap(t *testing.T) {
	// A chunk boundary that overlapped would re-fetch bars and waste quota; a
	// boundary that skipped would leave a gap that looks like missing market
	// data. Neither is detectable later, so it is checked here.
	windows := []window{{hour(0), hour(10)}}
	chunks := chunkWindows(windows, time.Hour, 3)

	if len(chunks) == 0 {
		t.Fatal("no chunks produced")
	}
	if !chunks[0].from.Equal(hour(0)) {
		t.Fatalf("first chunk starts at %s, want %s", chunks[0].from, hour(0))
	}
	last := chunks[len(chunks)-1]
	if !last.to.Equal(hour(10)) {
		t.Fatalf("last chunk ends at %s, want %s", last.to, hour(10))
	}
	for i := 1; i < len(chunks); i++ {
		if !chunks[i].from.Equal(chunks[i-1].to) {
			t.Fatalf("chunk %d starts at %s but the previous ended at %s",
				i, chunks[i].from, chunks[i-1].to)
		}
	}
}

func TestChunkingIsDeterministic(t *testing.T) {
	// The same request must produce the same boundaries, or a failed run
	// cannot be repeated and a segment record cannot be matched to it.
	windows := []window{{hour(0), hour(37)}}
	first := chunkWindows(windows, time.Hour, 5)
	second := chunkWindows(windows, time.Hour, 5)

	if len(first) != len(second) {
		t.Fatalf("chunk counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if !first[i].from.Equal(second[i].from) || !first[i].to.Equal(second[i].to) {
			t.Fatalf("chunk %d differs between runs", i)
		}
	}
}

func TestChunkingRespectsTheBarCeiling(t *testing.T) {
	chunks := chunkWindows([]window{{hour(0), hour(100)}}, time.Hour, 7)
	for i, c := range chunks {
		if bars := int(c.to.Sub(c.from) / time.Hour); bars > 7 {
			t.Fatalf("chunk %d covers %d bars, ceiling is 7", i, bars)
		}
	}
}

func TestChunkingSeveralDisjointWindows(t *testing.T) {
	// Repair passes one window per gap. They must stay separate: merging them
	// would re-request the healthy range between two holes.
	chunks := chunkWindows([]window{
		{hour(0), hour(2)},
		{hour(10), hour(12)},
	}, time.Hour, 10)

	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2 disjoint windows", len(chunks))
	}
	if chunks[0].to.Equal(chunks[1].from) {
		t.Fatal("two disjoint windows were merged into a contiguous range")
	}
}

func TestAnEmptyOrInvertedWindowProducesNoChunks(t *testing.T) {
	if got := chunkWindows([]window{{hour(5), hour(5)}}, time.Hour, 10); len(got) != 0 {
		t.Fatalf("an empty window produced %d chunks", len(got))
	}
	if got := chunkWindows([]window{{hour(9), hour(3)}}, time.Hour, 10); len(got) != 0 {
		t.Fatalf("an inverted window produced %d chunks", len(got))
	}
}

// --- bar validation -----------------------------------------------------------

func bar(open, high, low, close string) domain.Bar {
	return domain.Bar{
		InstrumentID: "XAUUSD", Timeframe: domain.Timeframe("1h"),
		OpenTime:  hour(1),
		CloseTime: hour(2),
		Open:      decimal.RequireFromString(open),
		High:      decimal.RequireFromString(high),
		Low:       decimal.RequireFromString(low),
		Close:     decimal.RequireFromString(close),
		Volume:    decimal.NewFromInt(10),
		Complete:  true,
	}
}

func TestACoherentBarIsAccepted(t *testing.T) {
	if !validBar(bar("2000", "2010", "1990", "2005")) {
		t.Fatal("a coherent bar was refused")
	}
}

func TestIncoherentOrImpossibleBarsAreRefused(t *testing.T) {
	// Every one of these describes a market that cannot exist, and an
	// indicator computed over it produces confident nonsense.
	cases := map[string]domain.Bar{
		"high below low":   bar("2000", "1980", "1990", "2005"),
		"high below open":  bar("2000", "1995", "1990", "1992"),
		"high below close": bar("2000", "2001", "1990", "2005"),
		"low above open":   bar("2000", "2010", "2005", "2008"),
		"low above close":  bar("2000", "2010", "2004", "2002"),
		"zero price":       bar("0", "2010", "1990", "2005"),
		"negative price":   bar("-1", "2010", "1990", "2005"),
	}
	for name, b := range cases {
		if validBar(b) {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestABarWithAnInvertedWindowIsRefused(t *testing.T) {
	b := bar("2000", "2010", "1990", "2005")
	b.CloseTime = b.OpenTime.Add(-time.Hour)
	if validBar(b) {
		t.Fatal("a bar closing before it opened was accepted")
	}
}

func TestANegativeVolumeIsRefused(t *testing.T) {
	b := bar("2000", "2010", "1990", "2005")
	b.Volume = decimal.NewFromInt(-5)
	if validBar(b) {
		t.Fatal("a negative volume was accepted")
	}
}

// --- dataset hashing ----------------------------------------------------------

func TestTheBarHashIsStableAndOrderIndependent(t *testing.T) {
	// A research dataset's identity rests on this. If the hash moved with the
	// retrieval order, the same bars fetched two ways would look like two
	// different datasets and neither could be reproduced.
	a := bar("2000", "2010", "1990", "2005")
	b := bar("2001", "2011", "1991", "2006")
	b.OpenTime, b.CloseTime = hour(2), hour(3)

	forward := HashBars([]domain.Bar{a, b})
	backward := HashBars([]domain.Bar{b, a})

	if forward == "" {
		t.Fatal("hashing produced an empty digest")
	}
	if forward != backward {
		t.Fatal("the hash depends on input order")
	}
}

func TestTheBarHashChangesWhenAPriceChanges(t *testing.T) {
	// Otherwise a snapshot could not detect that its underlying bars moved,
	// which is the one thing the hash exists to do.
	a := bar("2000", "2010", "1990", "2005")
	moved := bar("2000", "2010", "1990", "2005.01")
	if HashBars([]domain.Bar{a}) == HashBars([]domain.Bar{moved}) {
		t.Fatal("changing a close price did not change the hash")
	}
}

func TestHashingNothingIsEmptyRatherThanADigestOfNothing(t *testing.T) {
	// A digest of the empty string is a real-looking hash for a dataset that
	// does not exist, and two empty datasets would compare equal to it.
	if HashBars(nil) != "" {
		t.Fatal("an empty series produced a digest")
	}
}

// --- series assessment --------------------------------------------------------

func seriesBar(openTime time.Time, close string) domain.Bar {
	return domain.Bar{
		InstrumentID: "XAUUSD", Timeframe: domain.Timeframe("1h"),
		OpenTime:  openTime,
		CloseTime: openTime.Add(time.Hour),
		Open:      decimal.RequireFromString("2000"),
		High:      decimal.RequireFromString("2010"),
		Low:       decimal.RequireFromString("1990"),
		Close:     decimal.RequireFromString(close),
		Volume:    decimal.NewFromInt(10),
		Complete:  true,
	}
}

func contiguous(n int) []domain.Bar {
	out := make([]domain.Bar, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, seriesBar(hour(0).Add(time.Duration(i)*time.Hour),
			decimal.NewFromInt(int64(2000+i%7)).String()))
	}
	return out
}

func TestACleanSeriesIsValid(t *testing.T) {
	status, warnings := assessSeries(contiguous(300), domain.Timeframe("1h"), nil)
	if status != "VALID" {
		t.Fatalf("status = %s, warnings = %v", status, warnings)
	}
}

func TestDuplicateTimestampsMakeASeriesInvalid(t *testing.T) {
	// A forward outcome computed over a duplicated instant reads the wrong
	// bars, and the result looks exactly like a real one.
	bars := contiguous(300)
	bars[10] = bars[9]
	status, _ := assessSeries(bars, domain.Timeframe("1h"), nil)
	if status != "INVALID" {
		t.Fatalf("status = %s, want INVALID", status)
	}
}

func TestAReversedSeriesIsInvalid(t *testing.T) {
	bars := contiguous(300)
	bars[100], bars[101] = bars[101], bars[100]
	status, _ := assessSeries(bars, domain.Timeframe("1h"), nil)
	if status != "INVALID" {
		t.Fatalf("status = %s, want INVALID", status)
	}
}

func TestAnIncoherentBarMakesTheSeriesInvalid(t *testing.T) {
	bars := contiguous(300)
	bars[50].High = decimal.RequireFromString("1")
	status, _ := assessSeries(bars, domain.Timeframe("1h"), nil)
	if status != "INVALID" {
		t.Fatalf("status = %s, want INVALID", status)
	}
}

func TestAGapIsAWarningRatherThanAnInvalidation(t *testing.T) {
	// A validator that refused every imperfect series would refuse every real
	// market file: a gap in August does not make a question about January
	// unanswerable.
	head := contiguous(150)
	tail := make([]domain.Bar, 0, 150)
	for i := 0; i < 150; i++ {
		tail = append(tail, seriesBar(
			hour(0).Add(time.Duration(400+i)*time.Hour), "2003"))
	}
	status, warnings := assessSeries(append(head, tail...), domain.Timeframe("1h"), nil)
	if status != "VALID_WITH_WARNINGS" {
		t.Fatalf("status = %s, want VALID_WITH_WARNINGS", status)
	}
	if len(warnings) == 0 {
		t.Fatal("a gap produced no warning, so it would be invisible downstream")
	}
	joined := ""
	for _, w := range warnings {
		joined += w
	}
	if !strings.Contains(joined, "missing") {
		t.Fatalf("warnings do not mention the missing bars: %v", warnings)
	}
	if !strings.Contains(joined, "interpolated") {
		t.Fatal("the warning does not say that nothing was fabricated to fill the gap")
	}
}

func TestAFlatlineIsWarnedAbout(t *testing.T) {
	// A stalled feed, not a still market.
	bars := make([]domain.Bar, 0, 300)
	for i := 0; i < 300; i++ {
		bars = append(bars, seriesBar(hour(0).Add(time.Duration(i)*time.Hour), "2000"))
	}
	status, warnings := assessSeries(bars, domain.Timeframe("1h"), nil)
	if status != "VALID_WITH_WARNINGS" {
		t.Fatalf("status = %s, want VALID_WITH_WARNINGS", status)
	}
	joined := ""
	for _, w := range warnings {
		joined += w
	}
	if !strings.Contains(joined, "identical closes") {
		t.Fatalf("a 300-bar flatline produced no flatline warning: %v", warnings)
	}
}

func TestAShortSeriesIsWarnedAbout(t *testing.T) {
	status, warnings := assessSeries(contiguous(20), domain.Timeframe("1h"), nil)
	if status != "VALID_WITH_WARNINGS" {
		t.Fatalf("status = %s, want VALID_WITH_WARNINGS", status)
	}
	if len(warnings) == 0 {
		t.Fatal("a 20-bar series produced no warning about its length")
	}
}

// --- source classification ----------------------------------------------------

func TestGeneratedBarsAreNeverLabelledHistoricalMarket(t *testing.T) {
	// The defect this was written for: Snapshot hard-coded HISTORICAL_MARKET,
	// so a range of `mock` bars was recorded as real market evidence. Two
	// milestones of work exist to keep generated and observed data apart, and
	// one constant walked straight past all of it.
	for _, provider := range []string{"mock", "replay"} {
		got, err := classifySource([]string{provider})
		if err != nil {
			t.Fatalf("classifySource(%q): %v", provider, err)
		}
		if got != "SYNTHETIC_CONTROLLED" {
			t.Errorf("provider %q classified as %s, want SYNTHETIC_CONTROLLED",
				provider, got)
		}
	}
}

func TestARealProviderIsHistoricalMarket(t *testing.T) {
	got, err := classifySource([]string{"twelvedata"})
	if err != nil {
		t.Fatalf("classifySource: %v", err)
	}
	if got != "HISTORICAL_MARKET" {
		t.Fatalf("got %s, want HISTORICAL_MARKET", got)
	}
}

func TestAMixedRangeIsRefusedRatherThanLabelled(t *testing.T) {
	// Calling it historical would smuggle generated bars into real evidence;
	// calling it synthetic would discard real observations. Neither is true.
	_, err := classifySource([]string{"mock", "twelvedata"})
	if err == nil {
		t.Fatal("a range mixing generated and real bars was given a source type")
	}
	if !errors.Is(err, ErrMixedSourceTypes) {
		t.Fatalf("error = %v, want ErrMixedSourceTypes", err)
	}
}

func TestAnUnknownProviderDefaultsToReal(t *testing.T) {
	// The safe direction: a NEW provider is treated as market data and has to
	// be argued down, rather than quietly counting as synthetic.
	got, err := classifySource([]string{"some-new-vendor"})
	if err != nil {
		t.Fatalf("classifySource: %v", err)
	}
	if got != "HISTORICAL_MARKET" {
		t.Fatalf("got %s, want HISTORICAL_MARKET", got)
	}
}

func TestBarsWithNoProviderAttributionAreRefused(t *testing.T) {
	if _, err := classifySource(nil); err == nil {
		t.Fatal("bars with no provider attribution produced a source type")
	}
}
