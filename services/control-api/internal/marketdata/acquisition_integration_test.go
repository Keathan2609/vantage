// Package marketdata_test exercises historical acquisition end to end.
//
// # Why this suite exists
//
// Backfill, Sync, Repair and Snapshot had NO test that called them. Every test
// in internal/marketdata exercised a pure helper -- chunking, bar validation,
// hashing, series assessment. The orchestration in between, which is the part
// that would run against a real provider, had never been executed by anything.
// With no API key configured, the first evidence that the chain worked would
// have been the production run itself.
//
// These tests run the REAL Syncer against the REAL store and a provider that
// speaks the documented Twelve Data protocol (see fakeprovider_test.go). They
// need a seeded database and skip cleanly without one.
//
//	$env:VANTAGE_MARKETDATA_E2E = "1"
//	$env:VANTAGE_DATABASE_URL   = "postgres://..."
package marketdata

import (
	"context"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/db"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/store"
)

const (
	// The canonical instrument mapped to XAU/USD. The broker-contract
	// instrument XAUUSD.m is deliberately NOT mapped: a spot series is not
	// that contract's history.
	// acqAPIKey is deliberately self-describing: a credential-shaped literal
	// in a repository must say what it is not.
	acqAPIKey     = "test-key-not-a-real-credential"
	acqInstrument = "XAUUSD"
	acqTimeframe  = domain.Timeframe("1h")
)

// harness is a Syncer wired to a fake provider and a real database.
type harness struct {
	syncer *Syncer
	store  *store.Store
	pool   *db.Pool
	// td is the provider under test. Held so a test can reach the transport
	// layer -- the only path that can leak the API key.
	td       *TwelveDataProvider
	provider *fakeTwelveData
	clock    fixedClock
	ctx      context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if os.Getenv("VANTAGE_MARKETDATA_E2E") == "" {
		t.Skip("set VANTAGE_MARKETDATA_E2E=1 to run acquisition tests against a database")
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

	s := store.New(pool)
	// The instrument must exist: market_bars has a foreign key to it, and a
	// test that silently created its own would not be testing the platform's
	// universe.
	if _, err := s.Market.Instrument(ctx, acqInstrument); err != nil {
		t.Skipf("instrument %s is not present; seed the database first (%v)", acqInstrument, err)
	}

	provider := newFakeTwelveData()
	t.Cleanup(provider.Close)

	symbols, err := NewSymbolMap("twelvedata", TwelveDataSymbols)
	if err != nil {
		t.Fatalf("symbol map: %v", err)
	}
	clock := fixedClock{time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
	td, err := NewTwelveDataProvider(
		"https://api.twelvedata.com", acqAPIKey,
		symbols, clock, 600, 10*time.Second)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	// Set directly: inside the package there is no need to export a test hook,
	// and the https requirement in the constructor stays intact for every
	// caller that actually goes through it.
	td.baseURL = provider.URL

	market, err := domain.NewMarketClock(domain.ForexMetalsCalendar())
	if err != nil {
		t.Fatalf("market clock: %v", err)
	}

	h := &harness{
		syncer:   NewSyncer(s, td, symbols, clock, market, "test-sha"),
		store:    s,
		pool:     pool,
		td:       td,
		provider: provider,
		clock:    clock,
		ctx:      ctx,
	}
	h.clean(t)
	t.Cleanup(func() { h.clean(t) })
	return h
}

// clean removes only what this suite creates, so it can run against a
// development database without destroying its other data.
//
// Raw SQL through the pool the test opened, rather than delete helpers added
// to the store: production code should not grow an API that exists only so a
// test can undo itself.
func (h *harness) clean(t *testing.T) {
	t.Helper()
	// Every provider, not just twelvedata. The forming-bar tests write a bar
	// attributed to `mock` on purpose, and a provider filter here left it
	// behind on the one run that matters: if the UpsertBars completeness guard
	// regressed, the stray row survived teardown and the NEXT run failed in
	// TestBackfillStoresWhatTheProviderReturned, which reads the same range and
	// counts rows. One real defect would have become a permanent failure
	// misattributed to an unrelated test.
	//
	// `acqInstrument` is XAUUSD, which the development seed does not use --
	// its instrument is XAUUSD.m -- so this cannot delete seeded data.
	h.exec(t, `DELETE FROM market_bars
		WHERE instrument_id = $1 AND timeframe = $2`,
		acqInstrument, string(acqTimeframe))
	h.exec(t, `DELETE FROM market_data_segments WHERE instrument_id = $1 AND timeframe = $2`,
		acqInstrument, string(acqTimeframe))
	h.exec(t, `DELETE FROM research_datasets WHERE instrument_id = $1 AND timeframe = $2`,
		acqInstrument, string(acqTimeframe))
}

func (h *harness) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if err := h.pool.InTx(h.ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(h.ctx, sql, args...)
		return err
	}); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
}

// deleteBarRange punches a hole in the stored series, so a repair has
// something real to find.
func (h *harness) deleteBarRange(t *testing.T, from, to time.Time) {
	t.Helper()
	h.exec(t, `DELETE FROM market_bars
		WHERE instrument_id = $1 AND timeframe = $2 AND open_time >= $3 AND open_time < $4`,
		acqInstrument, string(acqTimeframe), from, to)
}

// --- backfill ----------------------------------------------------------------

func TestBackfillStoresWhatTheProviderReturned(t *testing.T) {
	h := newHarness(t)
	from := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	h.provider.Seed(from, 200)

	result, err := h.syncer.Backfill(h.ctx, acqInstrument, acqTimeframe, from, from.Add(200*time.Hour))
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if result.BarsStored == 0 {
		t.Fatalf("backfill stored nothing: %+v", result)
	}

	stored, err := h.store.Market.BarsInRange(h.ctx, acqInstrument, acqTimeframe,
		from, from.Add(200*time.Hour))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(stored) != result.BarsStored {
		t.Errorf("reported %d stored but %d are in the database",
			result.BarsStored, len(stored))
	}
	for _, b := range stored {
		if b.Provider != "twelvedata" {
			t.Fatalf("a bar was attributed to %q, not the provider that supplied it", b.Provider)
		}
		if !b.Complete {
			t.Fatal("an incomplete bar was stored as history")
		}
	}
	// Ascending, whatever order the provider chose to answer in.
	for i := 1; i < len(stored); i++ {
		if !stored[i-1].OpenTime.Before(stored[i].OpenTime) {
			t.Fatalf("bars are not ascending at %d", i)
		}
	}
}

func TestTheRequestUsesTheDocumentedDateFormat(t *testing.T) {
	// The fake provider REFUSES any other format, the way a provider that
	// reinterprets a date would not -- it would answer a different window and
	// still look like a success.
	h := newHarness(t)
	from := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	h.provider.Seed(from, 50)

	if _, err := h.syncer.Backfill(h.ctx, acqInstrument, acqTimeframe,
		from, from.Add(50*time.Hour)); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	requests := h.provider.Requests()
	if len(requests) == 0 {
		t.Fatal("no request reached the provider")
	}
	for _, q := range requests {
		for _, field := range []string{"start_date", "end_date"} {
			raw := q.Get(field)
			if _, err := time.Parse(fakeRequestDateLayout, raw); err != nil {
				t.Errorf("%s = %q is not the documented format %s",
					field, raw, fakeRequestDateLayout)
			}
		}
		if q.Get("timezone") != "UTC" {
			t.Errorf("timezone = %q; the provider's default is the exchange's zone, "+
				"which would move every session boundary", q.Get("timezone"))
		}
		if q.Get("symbol") != "XAU/USD" {
			t.Errorf("symbol = %q, want the vendor spelling XAU/USD", q.Get("symbol"))
		}
	}
}

func TestALargeRangeIsChunkedAndFullyCovered(t *testing.T) {
	// One response cannot carry an arbitrary range. A chunk boundary that
	// overlapped would waste quota; one that skipped would leave a hole that
	// looks like missing market data.
	h := newHarness(t)
	// Wholly in the PAST. A range running up to the present shares its last
	// key with the quote aggregator's forming bar, and this test would then be
	// measuring a race rather than the chunker. (That race was a real defect
	// and is fixed in UpsertBars; this test should still isolate one thing.)
	from := time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)
	const hours = 9000 // more than one 5000-row response, and more than one chunk
	h.provider.Seed(from, hours)

	result, err := h.syncer.Backfill(h.ctx, acqInstrument, acqTimeframe,
		from, from.Add(hours*time.Hour))
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if result.Chunks < 2 {
		t.Fatalf("a %d-hour range produced %d chunk(s); it should page", hours, result.Chunks)
	}

	stored, err := h.store.Market.BarsInRange(h.ctx, acqInstrument, acqTimeframe,
		from, from.Add(hours*time.Hour))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(stored) != hours {
		t.Errorf("stored %d of %d bars; the chunker lost some", len(stored), hours)
	}
	// No duplicates at the seams.
	seen := map[int64]bool{}
	for _, b := range stored {
		if seen[b.OpenTime.Unix()] {
			t.Fatalf("duplicate bar at %s: the chunks overlap", b.OpenTime)
		}
		seen[b.OpenTime.Unix()] = true
	}
}

func TestReacquiringTheSameRangeIsIdempotent(t *testing.T) {
	h := newHarness(t)
	from := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	h.provider.Seed(from, 300)
	window := from.Add(300 * time.Hour)

	first, err := h.syncer.Backfill(h.ctx, acqInstrument, acqTimeframe, from, window)
	if err != nil {
		t.Fatalf("first backfill: %v", err)
	}
	second, err := h.syncer.Backfill(h.ctx, acqInstrument, acqTimeframe, from, window)
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}

	if second.BarsStored != 0 {
		t.Errorf("the second pass reported %d NEW bars; re-fetching a held range "+
			"must count as duplicate, or a backfill that did no work looks like "+
			"one that did", second.BarsStored)
	}
	if second.BarsDuplicate != first.BarsStored {
		t.Errorf("second pass saw %d duplicates against %d originally stored",
			second.BarsDuplicate, first.BarsStored)
	}

	stored, err := h.store.Market.BarsInRange(h.ctx, acqInstrument, acqTimeframe, from, window)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(stored) != first.BarsStored {
		t.Errorf("the row count changed across a repeat: %d then %d",
			first.BarsStored, len(stored))
	}
}

// --- provenance ---------------------------------------------------------------

func TestEveryAcquisitionRecordsWhatWasAskedAndWhatArrived(t *testing.T) {
	h := newHarness(t)
	from := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	h.provider.Seed(from, 100)

	if _, err := h.syncer.Backfill(h.ctx, acqInstrument, acqTimeframe,
		from, from.Add(100*time.Hour)); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	segments, err := h.store.Market.Segments(h.ctx, acqInstrument, acqTimeframe, 50)
	if err != nil {
		t.Fatalf("segments: %v", err)
	}
	if len(segments) == 0 {
		t.Fatal("an acquisition recorded no provenance")
	}
	seg := segments[0]
	if seg.Provider != "twelvedata" || seg.ProviderSymbol != "XAU/USD" {
		t.Errorf("provenance does not name the provider and its symbol: %+v", seg)
	}
	if seg.SourceType != "HISTORICAL_MARKET" {
		t.Errorf("source type = %q for a real provider", seg.SourceType)
	}
	if seg.ReceivedStart == nil || seg.ReceivedEnd == nil {
		t.Error("the received range was not recorded, so a short answer would be invisible")
	}
	if seg.SegmentHash == "" {
		t.Error("no hash was recorded, so this acquisition cannot be tied to a dataset")
	}
	if seg.CodeSHA == "" {
		t.Error("no code identity was recorded")
	}
}

func TestAShortAnswerIsRecordedAsPartialRatherThanComplete(t *testing.T) {
	// A provider whose plan stops short returns less than was asked for. If
	// that is recorded as the requested range, a backfill loop re-requests the
	// same missing years forever -- or worse, stops asking.
	h := newHarness(t)
	from := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	// Ask for 400 hours; the provider only has the last 100.
	h.provider.Seed(from.Add(300*time.Hour), 100)

	if _, err := h.syncer.Backfill(h.ctx, acqInstrument, acqTimeframe,
		from, from.Add(400*time.Hour)); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	segments, err := h.store.Market.Segments(h.ctx, acqInstrument, acqTimeframe, 50)
	if err != nil {
		t.Fatalf("segments: %v", err)
	}
	var partial bool
	for _, s := range segments {
		if s.Status == store.SegmentPartial {
			partial = true
			if len(s.Warnings) == 0 {
				t.Error("a PARTIAL segment carries no warning saying what was short")
			}
		}
	}
	if !partial {
		t.Errorf("a provider that answered short was not recorded as PARTIAL: %+v", segments)
	}
}

// --- failure handling ---------------------------------------------------------

func TestARateLimitedRequestIsRetriedAndSucceeds(t *testing.T) {
	h := newHarness(t)
	from := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	h.provider.Seed(from, 100)
	h.provider.FailNext(1, "rate_limit")

	result, err := h.syncer.Backfill(h.ctx, acqInstrument, acqTimeframe,
		from, from.Add(100*time.Hour))
	if err != nil {
		t.Fatalf("a single rate-limited response ended the run: %v", err)
	}
	if result.BarsStored == 0 {
		t.Error("nothing was stored after a retryable failure")
	}
}

func TestAnErrorDeliveredWithHTTP200IsNotReadAsAnEmptySuccess(t *testing.T) {
	// This API reports most failures with HTTP 200 and a code in the body. A
	// status-code check alone would record an EMPTY segment -- "the provider
	// has no data here" -- and the range would never be requested again.
	h := newHarness(t)
	from := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	h.provider.Seed(from, 100)
	h.provider.FailNext(10, "error_body")

	_, _ = h.syncer.Backfill(h.ctx, acqInstrument, acqTimeframe, from, from.Add(100*time.Hour))

	segments, err := h.store.Market.Segments(h.ctx, acqInstrument, acqTimeframe, 50)
	if err != nil {
		t.Fatalf("segments: %v", err)
	}
	if len(segments) == 0 {
		t.Fatal("a failed acquisition recorded nothing at all")
	}
	for _, s := range segments {
		if s.Status == store.SegmentEmpty {
			t.Error("an error body was recorded as EMPTY, which reads as " +
				"'the provider has nothing here' and stops the range being retried")
		}
		if s.Status == store.SegmentFailed && s.FailureReason == "" {
			t.Error("a FAILED segment records no reason")
		}
	}
}

func TestAFailedAcquisitionNeverLeaksTheAPIKey(t *testing.T) {
	// The key is a query parameter, so any error carrying a URL carries the
	// credential. The segment record is PERSISTED and served by the coverage
	// endpoint, so a leak here outlives the process that produced it.
	//
	// The failure must be at the TRANSPORT level, and that is the entire point
	// of this test. An HTTP 500 or a 401 produces a static error string with no
	// URL in it, so a test driving one passes whether or not the scrub exists.
	// ENGINEERING_GUIDE.md records that trap; the first version of this test walked into
	// it anyway by using the fake's "server" mode.
	//
	// `http.Client.Do` returns a *url.Error carrying the whole request URL, and
	// Go's own redaction strips userinfo passwords while leaving the query
	// untouched. Pointing the provider at a closed port is the cheapest way to
	// reach it.
	//
	// This asserts the END-TO-END guarantee over BOTH locks -- the provider
	// keeping only the transport cause, and the syncer scrubbing again before
	// persisting -- so it passes while either one holds. That is deliberate:
	// the guarantee is "the key is never persisted", not "lock one works".
	// `TestATransportFailureDoesNotCarryTheKey` pins the provider's lock on its
	// own. Verified by removing both: the segment then records
	// `...?apikey=test-key-not-a-real-credential&...` and this fails.
	h := newHarness(t)
	from := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not reserve a port: %v", err)
	}
	addr := closed.Addr().String()
	if cerr := closed.Close(); cerr != nil {
		t.Fatalf("could not close the listener: %v", cerr)
	}
	h.td.baseURL = "http://" + addr

	_, _ = h.syncer.Backfill(h.ctx, acqInstrument, acqTimeframe, from, from.Add(50*time.Hour))

	segments, err := h.store.Market.Segments(h.ctx, acqInstrument, acqTimeframe, 50)
	if err != nil {
		t.Fatalf("segments: %v", err)
	}
	var sawFailure bool
	for _, s := range segments {
		if s.FailureReason == "" {
			continue
		}
		sawFailure = true
		if strings.Contains(s.FailureReason, acqAPIKey) {
			t.Fatalf("the API key was persisted in a segment record: %s", s.FailureReason)
		}
		if strings.Contains(s.FailureReason, url.QueryEscape(acqAPIKey)) {
			t.Fatalf("the percent-encoded API key was persisted: %s", s.FailureReason)
		}
	}
	// Without this the test passes when nothing failed at all, which is the
	// shape of a test that has stopped testing anything.
	if !sawFailure {
		t.Fatal("no segment recorded a failure, so nothing was checked for a leak")
	}

	// The same error reaches provider health, which the market-data status
	// endpoint serves to the browser.
	if ph := h.td.Health(); strings.Contains(ph.LastFailureReason, acqAPIKey) {
		t.Fatalf("the API key reached provider health: %s", ph.LastFailureReason)
	}
}

// --- sync, gaps and repair ----------------------------------------------------

func TestSyncFetchesOnlyWhatIsNewer(t *testing.T) {
	h := newHarness(t)
	from := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	h.provider.Seed(from, 100)

	if _, err := h.syncer.Backfill(h.ctx, acqInstrument, acqTimeframe,
		from, from.Add(100*time.Hour)); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// The provider gains more history; the clock moves so those bars have closed.
	h.provider.Seed(from.Add(100*time.Hour), 50)

	before := len(h.provider.Requests())
	result, err := h.syncer.Sync(h.ctx, acqInstrument, acqTimeframe)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	after := h.provider.Requests()

	// Assert it fetched SOMETHING first. Without this the loop below has no
	// iterations and the test passes for a Sync that regressed to doing
	// nothing at all -- which is the failure this suite exists to catch, and
	// it would announce itself as "sync correctly fetched only what is newer".
	if len(after) <= before {
		t.Fatalf("sync made no request at all; 50 newer bars were waiting at the "+
			"provider and it fetched none of them (requests before %d, after %d)",
			before, len(after))
	}
	if result.BarsStored == 0 {
		t.Errorf("sync stored no bars although 50 newer ones were available")
	}

	// And, having fetched, it must not have re-requested from the beginning.
	for _, q := range after[before:] {
		start, perr := time.Parse(fakeRequestDateLayout, q.Get("start_date"))
		if perr != nil {
			t.Fatalf("unparseable start_date %q", q.Get("start_date"))
		}
		if start.Before(from.Add(99 * time.Hour)) {
			t.Errorf("sync asked from %s, which re-downloads history it already holds", start)
		}
	}
}

func TestAGapIsDetectedAndRepairedWithoutTouchingTheRest(t *testing.T) {
	h := newHarness(t)
	from := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	h.provider.Seed(from, 300)
	window := from.Add(300 * time.Hour)

	// Acquire everything, then delete a run from the middle of the STORE to
	// simulate data lost after the fact.
	if _, err := h.syncer.Backfill(h.ctx, acqInstrument, acqTimeframe, from, window); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	// The hole must fall in MARKET HOURS. An earlier fixture put it 150 hours
	// in, which is a Sunday, and Repair correctly declined to request a range
	// the venue was shut for -- the test was wrong and the platform was right.
	// Tuesday mid-morning UTC is unambiguously open.
	holeFrom := time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)
	h.deleteBarRange(t, holeFrom, holeFrom.Add(10*time.Hour))

	gaps, err := h.store.Market.FindGaps(h.ctx, acqInstrument, acqTimeframe, from, window)
	if err != nil {
		t.Fatalf("find gaps: %v", err)
	}
	if len(gaps) == 0 {
		t.Fatal("a ten-bar hole was not detected")
	}

	before := len(h.provider.Requests())
	if _, err := h.syncer.Repair(h.ctx, acqInstrument, acqTimeframe, from, window); err != nil {
		t.Fatalf("repair: %v", err)
	}
	requestsMade := len(h.provider.Requests()) - before

	after, err := h.store.Market.FindGaps(h.ctx, acqInstrument, acqTimeframe, from, window)
	if err != nil {
		t.Fatalf("find gaps after repair: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("%d gap(s) remain after repair: %+v", len(after), after)
	}
	// Only the hole should have been requested, not the whole range.
	if requestsMade > 3 {
		t.Errorf("repair made %d requests for one ten-bar hole; it is re-downloading "+
			"the range rather than the damage", requestsMade)
	}
}

func TestRepairNeverFabricatesABarTheProviderDoesNotHave(t *testing.T) {
	// The most important guarantee in this file. A fabricated candle is a
	// fabricated trade, and everything downstream reads it as fact.
	h := newHarness(t)
	from := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	h.provider.Seed(from, 300)
	window := from.Add(300 * time.Hour)

	if _, err := h.syncer.Backfill(h.ctx, acqInstrument, acqTimeframe, from, window); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	holeFrom := time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC) // Tuesday, market open
	h.deleteBarRange(t, holeFrom, holeFrom.Add(10*time.Hour))
	// The provider no longer has those bars either.
	h.provider.Remove(holeFrom, 10)

	if _, err := h.syncer.Repair(h.ctx, acqInstrument, acqTimeframe, from, window); err != nil {
		t.Fatalf("repair: %v", err)
	}

	stored, err := h.store.Market.BarsInRange(h.ctx, acqInstrument, acqTimeframe,
		holeFrom, holeFrom.Add(10*time.Hour))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("repair invented %d bar(s) the provider does not have", len(stored))
	}
}

// --- coverage and research datasets -------------------------------------------

func TestCoverageReportsWhatWasActuallyAcquired(t *testing.T) {
	h := newHarness(t)
	from := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	h.provider.Seed(from, 200)

	if _, err := h.syncer.Backfill(h.ctx, acqInstrument, acqTimeframe,
		from, from.Add(200*time.Hour)); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	coverage, err := h.store.Market.Coverage(h.ctx, acqInstrument, acqTimeframe)
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if coverage.Bars == 0 {
		t.Fatal("coverage reports no bars after a successful backfill")
	}
	if coverage.EarliestBar == nil || coverage.LatestBar == nil {
		t.Fatal("coverage reports no range")
	}
	var sawProvider bool
	for _, p := range coverage.Providers {
		if p == "twelvedata" {
			sawProvider = true
		}
	}
	if !sawProvider {
		t.Errorf("coverage does not attribute the bars to the provider: %+v", coverage.Providers)
	}
	if coverage.LastSyncAt == nil {
		t.Error("coverage records no acquisition, so an operator cannot tell when data last arrived")
	}
}

func TestASnapshotOfProviderBarsIsHistoricalMarketAndCarriesAManifest(t *testing.T) {
	h := newHarness(t)
	from := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	h.provider.Seed(from, 400)
	window := from.Add(400 * time.Hour)

	if _, err := h.syncer.Backfill(h.ctx, acqInstrument, acqTimeframe, from, window); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	dir := t.TempDir()
	snap, err := h.syncer.Snapshot(h.ctx, SnapshotRequest{
		InstrumentID: acqInstrument, Timeframe: acqTimeframe,
		From: from, To: window, ResearchDir: dir, CodeSHA: "test-sha",
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Dataset.SourceType != "HISTORICAL_MARKET" {
		t.Errorf("provider bars were labelled %q", snap.Dataset.SourceType)
	}
	if snap.Dataset.QualityStatus == "INVALID" {
		t.Fatal("a clean acquisition produced an INVALID dataset")
	}
	if snap.Path == "" {
		t.Fatal("no file was written for the research plane")
	}
	if _, err := os.Stat(snap.Path); err != nil {
		t.Fatalf("the snapshot CSV is not on disk: %v", err)
	}
	manifest := strings.TrimSuffix(snap.Path, ".csv") + ".manifest.json"
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("the snapshot has no manifest beside it: %v", err)
	}
	// A CSV without its manifest imports as real market evidence by default,
	// so the manifest must never be the thing that is missing.
	body, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	for _, want := range []string{"HISTORICAL_MARKET", "twelvedata", "dataset_hash", "UTC"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the manifest does not record %q", want)
		}
	}
}

// --- two writers, one key -----------------------------------------------------

func TestAFormingBarNeverOverwritesAFinishedOne(t *testing.T) {
	// The quote aggregator and the historical syncer share market_bars'
	// primary key. Both are correct alone; the upsert had no opinion about
	// order, so whichever wrote last won.
	//
	// A backfill covering the CURRENT hour stores a finished bar, and the
	// aggregator's next tick replaced it with a one-tick partial -- at which
	// point the bar left every complete = TRUE query, including the one
	// strategies read. Found by the chunking test above: 9000 bars acquired,
	// 8999 visible, the missing one being the hour the process was in.
	h := newHarness(t)
	at := time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)

	finished := domain.Bar{
		InstrumentID: acqInstrument, Timeframe: acqTimeframe,
		OpenTime: at, CloseTime: at.Add(time.Hour),
		Open:     decimal.RequireFromString("2000"),
		High:     decimal.RequireFromString("2050"),
		Low:      decimal.RequireFromString("1990"),
		Close:    decimal.RequireFromString("2040"),
		Volume:   decimal.NewFromInt(500),
		Complete: true, Provider: "twelvedata",
	}
	if err := h.store.Market.UpsertBars(h.ctx, []domain.Bar{finished}); err != nil {
		t.Fatalf("store the finished bar: %v", err)
	}

	// The aggregator now opens the same interval from a single tick.
	forming := finished
	forming.Open, forming.High, forming.Low, forming.Close =
		decimal.RequireFromString("2001"), decimal.RequireFromString("2001"),
		decimal.RequireFromString("2001"), decimal.RequireFromString("2001")
	forming.Volume = decimal.NewFromInt(1)
	forming.Complete = false
	forming.Provider = "mock"
	if err := h.store.Market.UpsertBars(h.ctx, []domain.Bar{forming}); err != nil {
		t.Fatalf("store the forming bar: %v", err)
	}

	got, err := h.store.Market.BarsInRange(h.ctx, acqInstrument, acqTimeframe, at, at.Add(time.Hour))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("the finished bar disappeared: %d rows visible as complete", len(got))
	}
	if got[0].High.String() != "2050" || got[0].Low.String() != "1990" {
		t.Errorf("the finished bar's range was overwritten by a one-tick partial: H=%s L=%s",
			got[0].High, got[0].Low)
	}
	if got[0].Provider != "twelvedata" {
		t.Errorf("provider = %q; the finished bar was replaced", got[0].Provider)
	}
}

func TestAProviderMayStillCorrectItsOwnFinishedBar(t *testing.T) {
	// The guard refuses only the complete -> incomplete direction. Replacing a
	// finished bar with a corrected finished bar is legitimate and must work,
	// or a provider could never revise history.
	h := newHarness(t)
	at := time.Date(2026, 1, 6, 10, 0, 0, 0, time.UTC)

	original := domain.Bar{
		InstrumentID: acqInstrument, Timeframe: acqTimeframe,
		OpenTime: at, CloseTime: at.Add(time.Hour),
		Open:     decimal.RequireFromString("2000"),
		High:     decimal.RequireFromString("2050"),
		Low:      decimal.RequireFromString("1990"),
		Close:    decimal.RequireFromString("2040"),
		Volume:   decimal.NewFromInt(500),
		Complete: true, Provider: "twelvedata",
	}
	if err := h.store.Market.UpsertBars(h.ctx, []domain.Bar{original}); err != nil {
		t.Fatalf("store: %v", err)
	}
	corrected := original
	corrected.High = decimal.RequireFromString("2060")
	if err := h.store.Market.UpsertBars(h.ctx, []domain.Bar{corrected}); err != nil {
		t.Fatalf("correct: %v", err)
	}

	got, err := h.store.Market.BarsInRange(h.ctx, acqInstrument, acqTimeframe, at, at.Add(time.Hour))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) != 1 || got[0].High.String() != "2060" {
		t.Fatalf("a corrected finished bar was refused: %+v", got)
	}
}
