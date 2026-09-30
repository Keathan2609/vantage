package marketdata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/metrics"
	"github.com/vantage/control-api/internal/store"
)

// Acquisition: turning a request for history into stored, attributed bars.
//
// # Three operations, one path
//
//	BACKFILL  a named range, chunked, for history that is not there yet
//	SYNC      from the newest stored bar forward, for keeping up
//	REPAIR    the specific ranges that are missing, and nothing else
//
// They differ only in how the ranges are chosen. Everything after that --
// chunking, fetching, validating, storing, recording provenance -- is shared,
// because three copies of "fetch and store" would drift and only one of them
// would have the bug fixed.
//
// # Nothing is ever fabricated
//
// A provider that returns less than was asked for gets a PARTIAL segment and
// the shortfall stays visible. No bar is interpolated, forward-filled or
// synthesised to close a gap. A fabricated candle is a fabricated trade, and
// every strategy downstream would read it as fact.

// IngestionVersion changes when the storage path changes in a way that could
// alter what a bar becomes. Recorded on every segment so a later reader can
// tell which code produced a row.
const IngestionVersion = 1

// NormalizationVersion changes when normalisation changes. Separate from
// ingestion because one can move without the other.
const NormalizationVersion = 1

// maxChunkBars bounds a single provider request.
//
// Below the provider's own 5000 ceiling on purpose: a request at exactly the
// limit returns a full page with no way to distinguish "this is all there is"
// from "this is all that fits", and the difference decides whether to ask
// again.
const maxChunkBars = 4000

// maxChunksPerRun bounds a single operation.
//
// A ten-year hourly backfill is roughly 22 chunks at this size. The ceiling is
// generous enough for that and low enough that a mistaken request cannot spend
// an afternoon hammering a provider.
const maxChunksPerRun = 64

// Syncer acquires history from a provider into the local store.
type Syncer struct {
	store    *store.Store
	provider Provider
	symbols  *SymbolMap
	clock    domain.Clock
	market   *domain.MarketClock
	codeSHA  string
}

// NewSyncer builds a syncer.
func NewSyncer(
	s *store.Store, p Provider, symbols *SymbolMap,
	clock domain.Clock, market *domain.MarketClock, codeSHA string,
) *Syncer {
	return &Syncer{
		store: s, provider: p, symbols: symbols,
		clock: clock, market: market, codeSHA: codeSHA,
	}
}

// Result summarises one acquisition operation.
type Result struct {
	Kind          store.SegmentKind         `json:"kind"`
	InstrumentID  string                    `json:"instrument_id"`
	Timeframe     string                    `json:"timeframe"`
	Provider      string                    `json:"provider"`
	RequestedFrom time.Time                 `json:"requested_from"`
	RequestedTo   time.Time                 `json:"requested_to"`
	Chunks        int                       `json:"chunks"`
	BarsReturned  int                       `json:"bars_returned"`
	BarsStored    int                       `json:"bars_stored"`
	BarsDuplicate int                       `json:"bars_duplicate"`
	BarsInvalid   int                       `json:"bars_invalid"`
	Segments      []store.MarketDataSegment `json:"segments"`
	DurationMS    int64                     `json:"duration_ms"`
	Warnings      []string                  `json:"warnings"`
}

// ErrUnsupportedInstrument is returned when the provider has no mapping.
var ErrUnsupportedInstrument = errors.New("marketdata: provider does not carry this instrument")

// Backfill acquires a named historical range, chunked.
func (s *Syncer) Backfill(
	ctx context.Context, instrumentID string, tf domain.Timeframe, from, to time.Time,
) (Result, error) {
	return s.acquire(ctx, store.SegmentBackfill, instrumentID, tf, []window{{from, to}})
}

// Sync fetches only what is newer than the latest stored bar.
//
// The whole point of incremental synchronisation: re-downloading a decade
// every hour would exhaust a provider quota to learn nothing. When nothing is
// stored yet there is no "newer than", and the caller is told to backfill
// rather than being given a silent default range that might be wrong by years.
func (s *Syncer) Sync(
	ctx context.Context, instrumentID string, tf domain.Timeframe,
) (Result, error) {
	latest, ok, err := s.store.Market.LatestBarTime(ctx, instrumentID, tf, s.provider.Name())
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, fmt.Errorf(
			"marketdata: %s has supplied no bars for %s %s yet; run a backfill "+
				"first so the starting point is chosen deliberately rather than "+
				"guessed. Bars from another source do not count -- syncing "+
				"forward from them would skip the history this provider has",
			s.provider.Name(), instrumentID, tf)
	}
	dur, err := tf.Duration()
	if err != nil {
		return Result{}, err
	}
	// From the bar AFTER the latest stored one. Starting at the latest would
	// re-fetch it every time; the upsert would absorb it, but the request is
	// still wasted quota.
	from := latest.Add(dur)
	to := s.completedBarEnd(tf)
	if !from.Before(to) {
		return Result{
			Kind: store.SegmentSync, InstrumentID: instrumentID,
			Timeframe: string(tf), Provider: s.provider.Name(),
			RequestedFrom: from, RequestedTo: to,
			Segments: []store.MarketDataSegment{},
			Warnings: []string{"already up to date with the newest completed bar"},
		}, nil
	}
	return s.acquire(ctx, store.SegmentSync, instrumentID, tf, []window{{from, to}})
}

// Repair re-requests only the ranges that are missing.
//
// Only the holes, never the whole range. A repair that re-downloaded
// everything would work and would also spend a quota proportional to the
// history rather than to the damage.
func (s *Syncer) Repair(
	ctx context.Context, instrumentID string, tf domain.Timeframe, from, to time.Time,
) (Result, error) {
	gaps, err := s.store.Market.FindGaps(ctx, instrumentID, tf, from, to)
	if err != nil {
		return Result{}, err
	}
	windows := make([]window, 0, len(gaps))
	for _, g := range gaps {
		// Market closures are not damage. Filtering them here is what stops a
		// repair from asking a provider for every weekend, forever, and
		// recording an EMPTY segment each time.
		if s.market != nil && !s.rangeHasOpenMarket(g.From, g.To, tf) {
			continue
		}
		windows = append(windows, window{g.From, g.To})
	}
	if len(windows) == 0 {
		return Result{
			Kind: store.SegmentRepair, InstrumentID: instrumentID,
			Timeframe: string(tf), Provider: s.provider.Name(),
			RequestedFrom: from, RequestedTo: to,
			Segments: []store.MarketDataSegment{},
			Warnings: []string{"no repairable gap found; absences fall in closed-market hours"},
		}, nil
	}
	return s.acquire(ctx, store.SegmentRepair, instrumentID, tf, windows)
}

type window struct{ from, to time.Time }

// acquire is the shared path: chunk, fetch, validate, store, record.
func (s *Syncer) acquire(
	ctx context.Context, kind store.SegmentKind, instrumentID string,
	tf domain.Timeframe, windows []window,
) (Result, error) {
	started := s.clock.Now()
	log := logging.FromContext(ctx)

	if !s.symbols.Supports(instrumentID) {
		return Result{}, fmt.Errorf("%w: %s", ErrUnsupportedInstrument, instrumentID)
	}
	vendorSymbol, err := s.symbols.Vendor(instrumentID)
	if err != nil {
		return Result{}, err
	}
	inst, err := s.store.Market.Instrument(ctx, instrumentID)
	if err != nil {
		return Result{}, fmt.Errorf("marketdata: unknown instrument %s: %w", instrumentID, err)
	}
	dur, err := tf.Duration()
	if err != nil {
		return Result{}, err
	}

	result := Result{
		Kind: kind, InstrumentID: instrumentID, Timeframe: string(tf),
		Provider: s.provider.Name(), Segments: []store.MarketDataSegment{},
		Warnings: []string{},
	}
	if len(windows) > 0 {
		result.RequestedFrom, result.RequestedTo = windows[0].from, windows[len(windows)-1].to
	}

	chunks := chunkWindows(windows, dur, maxChunkBars)
	if len(chunks) > maxChunksPerRun {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"the requested range needs %d chunks; only the first %d were fetched. "+
				"Run again to continue rather than raising the ceiling",
			len(chunks), maxChunksPerRun))
		chunks = chunks[:maxChunksPerRun]
	}

	for _, c := range chunks {
		seg := store.MarketDataSegment{
			ID:             uuid.NewString(),
			InstrumentID:   instrumentID,
			Timeframe:      string(tf),
			Provider:       s.provider.Name(),
			ProviderSymbol: vendorSymbol,
			Kind:           kind,
			RequestedStart: c.from,
			RequestedEnd:   c.to,
			Status:         store.SegmentPending,
			// DERIVED from the provider, never hard-coded. The Syncer takes any
			// Provider and is exported, so a synthetic one would otherwise
			// stamp HISTORICAL_MARKET onto generated bars -- the exact mistake
			// ENGINEERING_GUIDE.md rule 14 records, which this file's own snapshot path
			// already had to have corrected once.
			SourceType:           sourceTypeFor(s.provider),
			NormalizationVersion: NormalizationVersion,
			IngestionVersion:     IngestionVersion,
			CodeSHA:              s.codeSHA,
			Warnings:             []string{},
			RequestedAt:          s.clock.Now(),
		}

		bars, ferr := s.provider.HistoricalBars(ctx, inst, tf, c.from, c.to)
		if ferr != nil {
			seg.Status = store.SegmentFailed
			// The provider scrubs its own errors, but this record is PERSISTED
			// and served by the coverage endpoint, so it does not rely on that
			// alone: a future provider that forgets would write a credential
			// into the database, where it outlives the process that leaked it.
			seg.FailureReason = truncate(scrubProviderError(s.provider, ferr), 400)
			completed := s.clock.Now()
			seg.CompletedAt = &completed
			_ = s.store.Market.RecordSegment(ctx, seg)
			result.Segments = append(result.Segments, seg)

			// A failed chunk does not abandon the run: later chunks may be
			// fine, and the failure is recorded rather than swallowed. But a
			// provider that is not configured will fail identically forever,
			// so that one stops immediately.
			if errors.Is(ferr, ErrProviderNotConfigured) {
				result.DurationMS = s.clock.Now().Sub(started).Milliseconds()
				return result, ferr
			}
			log.Warn("market data chunk failed",
				"instrument", instrumentID, "timeframe", string(tf),
				"from", c.from.Format(time.RFC3339),
				"error", scrubProviderError(s.provider, ferr))
			continue
		}

		stored, duplicate, invalid, hash := s.persist(ctx, instrumentID, tf, bars)

		seg.RowsReturned = len(bars)
		seg.RowsStored = stored
		seg.RowsDuplicate = duplicate
		seg.RowsInvalid = invalid
		seg.SegmentHash = hash
		if len(bars) > 0 {
			first, last := bars[0].OpenTime, bars[len(bars)-1].OpenTime
			seg.ReceivedStart, seg.ReceivedEnd = &first, &last
			seg.Status = store.SegmentComplete
			// The requested and received ranges are compared, not assumed
			// equal. A provider whose plan stops short returns a shorter
			// window and that shortfall must stay visible, or a backfill loop
			// re-requests the same missing years forever.
			if first.Sub(c.from) > dur || c.to.Sub(last) > 2*dur {
				seg.Status = store.SegmentPartial
				seg.Warnings = append(seg.Warnings, fmt.Sprintf(
					"requested [%s, %s) and received [%s, %s]",
					c.from.Format(time.RFC3339), c.to.Format(time.RFC3339),
					first.Format(time.RFC3339), last.Format(time.RFC3339)))
			}
		} else {
			seg.Status = store.SegmentEmpty
		}

		completed := s.clock.Now()
		seg.CompletedAt = &completed
		if rerr := s.store.Market.RecordSegment(ctx, seg); rerr != nil {
			log.Error("could not record market data segment", "error", rerr.Error())
		}

		result.Segments = append(result.Segments, seg)
		result.BarsReturned += seg.RowsReturned
		result.BarsStored += seg.RowsStored
		result.BarsDuplicate += seg.RowsDuplicate
		result.BarsInvalid += seg.RowsInvalid
	}

	result.Chunks = len(chunks)
	result.DurationMS = s.clock.Now().Sub(started).Milliseconds()
	metrics.MarketDataSyncDuration.WithLabelValues(s.provider.Name(), string(kind)).
		Observe(float64(result.DurationMS) / 1000)
	return result, nil
}

// persist validates and stores a chunk, returning stored/duplicate/invalid.
func (s *Syncer) persist(
	ctx context.Context, instrumentID string, tf domain.Timeframe, bars []domain.Bar,
) (stored, duplicate, invalid int, hash string) {
	if len(bars) == 0 {
		return 0, 0, 0, ""
	}
	log := logging.FromContext(ctx)

	// Which of these do we already hold? Counted before writing so that
	// "ingested" means new rather than "the upsert ran". A backfill reporting
	// thousands ingested when it re-fetched an existing range would hide that
	// it did no work.
	//
	// The upper bound is the last bar's open time plus ONE BAR, not plus one
	// nanosecond. BarsInRange is half-open (`open_time < $4`), and Postgres
	// stores microseconds -- so a nanosecond added to a microsecond-aligned
	// timestamp truncates straight back to it, the bound stays exclusive, and
	// the last bar of every chunk was never found in `existing`. It was then
	// counted as newly stored on every re-fetch, which is exactly the false
	// reassurance this block exists to prevent.
	dur, derr := tf.Duration()
	if derr != nil {
		log.Warn("unknown timeframe while de-duplicating", "timeframe", string(tf))
		return 0, 0, 0, ""
	}
	existing, err := s.store.Market.BarsInRange(
		ctx, instrumentID, tf, bars[0].OpenTime, bars[len(bars)-1].OpenTime.Add(dur))
	if err != nil {
		log.Warn("could not read existing bars before ingest", "error", err.Error())
	}
	have := make(map[int64]struct{}, len(existing))
	for _, b := range existing {
		have[b.OpenTime.UnixNano()] = struct{}{}
	}

	clean := make([]domain.Bar, 0, len(bars))
	for _, b := range bars {
		if !validBar(b) {
			invalid++
			metrics.MarketDataInvalidBars.WithLabelValues(s.provider.Name(), instrumentID).Inc()
			continue
		}
		// Only completed bars. A forming candle written as history is read by
		// a strategy as fact and then changes underneath it.
		if !b.Complete {
			invalid++
			continue
		}
		if _, seen := have[b.OpenTime.UnixNano()]; seen {
			duplicate++
		} else {
			stored++
		}
		clean = append(clean, b)
	}

	if len(clean) > 0 {
		if err := s.store.Market.UpsertBars(ctx, clean); err != nil {
			log.Error("could not store market bars", "error", err.Error())
			return 0, 0, invalid, ""
		}
	}

	metrics.MarketDataBarsIngested.
		WithLabelValues(s.provider.Name(), instrumentID, string(tf)).Add(float64(stored))
	metrics.MarketDataDuplicatesIgnored.
		WithLabelValues(s.provider.Name(), instrumentID, string(tf)).Add(float64(duplicate))

	return stored, duplicate, invalid, HashBars(clean)
}

// validBar refuses what must never reach a strategy.
//
// The database enforces most of this too. Both checks exist because the
// constraint rejects the whole batch while this rejects the one bad bar, and
// because a corrupt bar arriving from outside deserves to be counted rather
// than to surface as a failed transaction.
func validBar(b domain.Bar) bool {
	if !b.Open.IsPositive() || !b.High.IsPositive() ||
		!b.Low.IsPositive() || !b.Close.IsPositive() {
		return false
	}
	if b.High.LessThan(b.Low) ||
		b.High.LessThan(b.Open) || b.High.LessThan(b.Close) ||
		b.Low.GreaterThan(b.Open) || b.Low.GreaterThan(b.Close) {
		return false
	}
	if b.Volume.IsNegative() {
		return false
	}
	if !b.CloseTime.After(b.OpenTime) {
		return false
	}
	return true
}

// HashBars is a deterministic digest of a bar series.
//
// Ordered by open time and formatted at fixed precision, so the same bars
// always hash the same way regardless of how they were retrieved. This is what
// lets a research dataset prove it rests on a particular acquisition, and what
// detects a snapshot whose underlying bars have since changed.
func HashBars(bars []domain.Bar) string {
	if len(bars) == 0 {
		return ""
	}
	ordered := make([]domain.Bar, len(bars))
	copy(ordered, bars)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].OpenTime.Before(ordered[j].OpenTime)
	})

	h := sha256.New()
	for _, b := range ordered {
		fmt.Fprintf(h, "%s|%s|%d|%s|%s|%s|%s|%s\n",
			b.InstrumentID, b.Timeframe, b.OpenTime.UTC().Unix(),
			b.Open.String(), b.High.String(), b.Low.String(), b.Close.String(),
			b.Volume.String())
	}
	return hex.EncodeToString(h.Sum(nil))
}

// chunkWindows splits requested ranges into provider-sized pieces.
//
// Deterministic: the same request always produces the same chunk boundaries,
// so a failed run can be repeated and a segment record can be matched to the
// request that produced it.
func chunkWindows(windows []window, barDuration time.Duration, maxBars int) []window {
	out := []window{}
	if barDuration <= 0 || maxBars <= 0 {
		return out
	}
	span := barDuration * time.Duration(maxBars)
	for _, w := range windows {
		if !w.to.After(w.from) {
			continue
		}
		for start := w.from; start.Before(w.to); start = start.Add(span) {
			end := start.Add(span)
			if end.After(w.to) {
				end = w.to
			}
			out = append(out, window{start, end})
		}
	}
	return out
}

// completedBarEnd is the exclusive upper bound of the newest COMPLETE bar.
//
// The current partial candle is deliberately excluded. A strategy that treats
// an incomplete bar as an observation is reading a value that will change, and
// on the next tick its own history silently rewrites itself.
func (s *Syncer) completedBarEnd(tf domain.Timeframe) time.Time {
	dur, err := tf.Duration()
	if err != nil {
		return s.clock.Now().UTC()
	}
	return s.clock.Now().UTC().Truncate(dur)
}

// rangeHasOpenMarket reports whether any bar in a range falls in market hours.
func (s *Syncer) rangeHasOpenMarket(from, to time.Time, tf domain.Timeframe) bool {
	dur, err := tf.Duration()
	if err != nil || s.market == nil {
		return true
	}
	// Bounded: a multi-year gap is open somewhere, and walking every hour of
	// it to prove that would cost more than the fetch it guards.
	const probeLimit = 2000
	probes := 0
	for t := from; t.Before(to) && probes < probeLimit; t = t.Add(dur) {
		if s.market.Status(t).Tradable() {
			return true
		}
		probes++
	}
	return probes >= probeLimit
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// scrubProviderError renders a provider error as text with any credential removed.
//
// The Syncer does not know what a given provider treats as secret, so it asks.
// A provider that can redact itself does; one that cannot is rendered plainly,
// which is correct for the mock and replay providers because they have no
// credential to leak.
//
// This exists because the error text from here is PERSISTED to
// market_data_segments and served by the coverage endpoint, so a leak at this
// point outlives the process that produced it.
func scrubProviderError(p Provider, err error) string {
	if err == nil {
		return ""
	}
	if s, ok := p.(interface{ scrub(string) string }); ok {
		return s.scrub(err.Error())
	}
	return err.Error()
}
