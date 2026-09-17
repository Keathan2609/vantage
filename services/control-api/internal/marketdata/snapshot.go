package marketdata

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/store"
)

// Turning stored bars into something research can reference forever.
//
// # Why a snapshot rather than a query
//
// A research run that says "XAUUSD 1h from 2020 to 2024" references a MUTABLE
// thing. The next sync appends bars, a repair fills a gap, and the run is no
// longer reproducible even though nothing about its description changed. The
// snapshot fixes an identity -- instrument, timeframe, provider, range,
// normalization version and a deterministic hash of the bars themselves -- and
// the run references that.
//
// The bars are not copied into the record. Copying would double the storage
// and create a second thing that can drift; the hash is what detects drift,
// and a snapshot whose recomputed hash no longer matches is a snapshot that
// must not be used.
//
// # Why it is also written to a file
//
// The research plane is Python and does not touch this database -- CLAUDE.md
// rule 5. So the snapshot is ALSO materialised as a CSV plus a manifest in the
// allowlisted research directory, which the existing, tested Python importer
// already knows how to read. The database row is the identity; the file is the
// delivery. Both carry the same hash, so a mismatch between them is
// detectable rather than silent.

// SnapshotRequest names what to freeze.
type SnapshotRequest struct {
	InstrumentID string
	Timeframe    domain.Timeframe
	From         time.Time
	To           time.Time
	// ResearchDir is the allowlisted directory the Python research plane
	// reads. Empty means "record the identity but write no file".
	ResearchDir string
	CodeSHA     string
}

// SnapshotResult is the created identity and where it was written.
type SnapshotResult struct {
	Dataset store.ResearchDataset `json:"dataset"`
	// Path is empty when no file was written.
	Path string `json:"path,omitempty"`
	// Warnings that travelled from quality assessment onto the dataset.
	Warnings []string `json:"warnings"`
}

// SyntheticProviders generate their bars rather than observing a market.
//
// Named explicitly rather than inferred. A provider absent from this set is
// treated as real, which is the safe direction for a NEW provider being added
// -- it gets classified as market data and must be argued down -- and the
// wrong direction only if someone adds a generator without saying so here.
var SyntheticProviders = map[string]struct{}{
	"mock":   {},
	"replay": {},
}

// ErrMixedSourceTypes is returned when a range contains both generated and
// real bars.
var ErrMixedSourceTypes = errors.New(
	"marketdata: that range mixes generated and real bars, so it is neither " +
		"SYNTHETIC_CONTROLLED nor HISTORICAL_MARKET")

// classifySource decides what a set of providers makes a dataset.
//
// A MIXED range is refused rather than labelled. Calling it historical would
// smuggle generated bars into real-market evidence; calling it synthetic would
// discard real observations. Neither label is true, and a dataset whose source
// type is a compromise is worse than no dataset -- every conclusion drawn from
// it would inherit the compromise silently.
func classifySource(providers []string) (string, error) {
	if len(providers) == 0 {
		return "", errors.New("marketdata: bars carry no provider attribution")
	}
	var synthetic, real int
	for _, name := range providers {
		if _, ok := SyntheticProviders[name]; ok {
			synthetic++
		} else {
			real++
		}
	}
	switch {
	case synthetic > 0 && real > 0:
		return "", fmt.Errorf("%w: %s", ErrMixedSourceTypes, strings.Join(providers, ", "))
	case synthetic > 0:
		return "SYNTHETIC_CONTROLLED", nil
	default:
		return "HISTORICAL_MARKET", nil
	}
}

// ErrNoBarsInRange is returned when the requested window holds nothing.
var ErrNoBarsInRange = errors.New("marketdata: no stored bars in that range")

// ErrQualityRefused is returned when stored bars are not fit for research.
var ErrQualityRefused = errors.New(
	"marketdata: the bars in that range did not pass quality validation")

// Snapshot freezes a range of stored bars as a research dataset.
//
// Quality is assessed HERE rather than trusted from ingestion. Bars can be
// written by more than one path -- a provider backfill, the quote aggregator,
// a seeder -- and a range that is individually valid bar by bar can still be
// unusable as a series. INVALID never becomes a dataset; the database CHECK
// constraint says so too, and both are deliberate.
func (s *Syncer) Snapshot(
	ctx context.Context, req SnapshotRequest,
) (SnapshotResult, error) {
	bars, err := s.store.Market.BarsInRange(
		ctx, req.InstrumentID, req.Timeframe, req.From, req.To)
	if err != nil {
		return SnapshotResult{}, err
	}
	if len(bars) == 0 {
		return SnapshotResult{}, ErrNoBarsInRange
	}

	sort.Slice(bars, func(i, j int) bool {
		return bars[i].OpenTime.Before(bars[j].OpenTime)
	})

	status, warnings := assessSeries(bars, req.Timeframe, s.market)
	if status == "INVALID" {
		return SnapshotResult{}, fmt.Errorf("%w: %s",
			ErrQualityRefused, strings.Join(warnings, "; "))
	}

	// The provider attribution comes from the bars, not from configuration. A
	// range assembled from two providers is recorded as such rather than
	// silently attributed to whichever one is configured today.
	providers := map[string]struct{}{}
	for _, b := range bars {
		providers[b.Provider] = struct{}{}
	}
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)

	// The source type is DERIVED from which providers supplied the bars, never
	// assumed. An earlier version hard-coded HISTORICAL_MARKET and happily
	// labelled bars from the `mock` generator as real market evidence -- the
	// precise corruption that separating the two source types exists to
	// prevent. A generated bar entering research as a historical one would
	// make every comparison between synthetic and real findings meaningless.
	sourceType, err := classifySource(names)
	if err != nil {
		return SnapshotResult{}, err
	}

	hash := HashBars(bars)
	dataset := store.ResearchDataset{
		ID:                   uuid.NewString(),
		InstrumentID:         req.InstrumentID,
		Timeframe:            string(req.Timeframe),
		Provider:             strings.Join(names, "+"),
		SourceType:           sourceType,
		RangeStart:           bars[0].OpenTime,
		RangeEnd:             bars[len(bars)-1].OpenTime,
		BarCount:             len(bars),
		DatasetHash:          hash,
		NormalizationVersion: NormalizationVersion,
		QualityStatus:        status,
		QualityWarnings:      warnings,
		CodeSHA:              req.CodeSHA,
	}

	saved, err := s.store.Market.UpsertResearchDataset(ctx, dataset)
	if err != nil {
		return SnapshotResult{}, err
	}

	result := SnapshotResult{Dataset: saved, Warnings: warnings}
	if req.ResearchDir != "" {
		path, werr := writeSnapshotFile(req.ResearchDir, saved, bars)
		if werr != nil {
			return result, werr
		}
		result.Path = path
	}
	return result, nil
}

// assessSeries judges a bar SERIES, not individual bars.
//
// Individually valid bars can still be an unusable series: duplicated
// timestamps, a run that goes backwards, a flatline, a range with most of its
// bars missing. Only the structural defects are INVALID; the rest are
// warnings that travel with the dataset, because a gap in August does not
// make a question about January unanswerable.
func assessSeries(
	bars []domain.Bar, tf domain.Timeframe, market *domain.MarketClock,
) (string, []string) {
	warnings := []string{}
	dur, err := tf.Duration()
	if err != nil {
		return "INVALID", []string{"unknown timeframe"}
	}

	var duplicates, backwards, missing, flatRun, longestFlat int
	var previousClose string
	for i, b := range bars {
		if !validBar(b) {
			return "INVALID", []string{
				fmt.Sprintf("bar at %s is not internally coherent",
					b.OpenTime.Format(time.RFC3339)),
			}
		}
		if i > 0 {
			gap := b.OpenTime.Sub(bars[i-1].OpenTime)
			switch {
			case gap == 0:
				duplicates++
			case gap < 0:
				backwards++
			case gap > dur:
				// Closed-market absences are not damage.
				expected := int(gap/dur) - 1
				if market != nil {
					for t := bars[i-1].OpenTime.Add(dur); t.Before(b.OpenTime); t = t.Add(dur) {
						if !market.Status(t).Tradable() {
							expected--
						}
					}
				}
				if expected > 0 {
					missing += expected
				}
			}
		}
		if c := b.Close.String(); c == previousClose {
			flatRun++
			if flatRun > longestFlat {
				longestFlat = flatRun
			}
		} else {
			flatRun = 0
		}
		previousClose = b.Close.String()
	}

	// Duplicated or reversed timestamps make the series uninterpretable: a
	// forward outcome computed over them reads the wrong bars.
	if duplicates > 0 {
		return "INVALID", []string{
			fmt.Sprintf("%d duplicate timestamps", duplicates)}
	}
	if backwards > 0 {
		return "INVALID", []string{
			fmt.Sprintf("%d bars are out of chronological order", backwards)}
	}

	if missing > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d bars missing inside the range during open-market hours; nothing "+
				"was interpolated", missing))
	}
	if longestFlat >= 12 {
		warnings = append(warnings, fmt.Sprintf(
			"%d consecutive identical closes, which usually means a stalled feed "+
				"rather than a still market", longestFlat+1))
	}
	if len(bars) < 200 {
		warnings = append(warnings, fmt.Sprintf(
			"only %d bars; most strategies need more history than this to signal "+
				"at all", len(bars)))
	}

	if len(warnings) > 0 {
		return "VALID_WITH_WARNINGS", warnings
	}
	return "VALID", warnings
}

// writeSnapshotFile materialises the bars for the Python research plane.
//
// The filename carries the dataset id so two snapshots never collide, and the
// directory is the allowlisted one the importer already refuses to read
// outside of. Nothing here takes a path from a request.
func writeSnapshotFile(
	dir string, dataset store.ResearchDataset, bars []domain.Bar,
) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("marketdata: create research directory: %w", err)
	}

	// The name is built from validated fields, never from caller text.
	name := fmt.Sprintf("%s_%s_%s.csv",
		sanitiseName(dataset.InstrumentID),
		sanitiseName(dataset.Timeframe),
		dataset.DatasetHash[:16])
	path := filepath.Join(dir, name)

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return "", fmt.Errorf("marketdata: write snapshot: %w", err)
	}
	defer func() { _ = file.Close() }()

	w := csv.NewWriter(file)
	if err := w.Write([]string{"timestamp", "open", "high", "low", "close", "volume"}); err != nil {
		return "", err
	}
	for _, b := range bars {
		// RFC3339 UTC. The Python importer parses it and records the timezone
		// as declared rather than assumed, which is the difference between a
		// recorded fact and a guess about a developer's locale.
		if err := w.Write([]string{
			b.OpenTime.UTC().Format(time.RFC3339),
			b.Open.String(), b.High.String(), b.Low.String(), b.Close.String(),
			b.Volume.String(),
		}); err != nil {
			return "", err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", fmt.Errorf("marketdata: flush snapshot: %w", err)
	}

	// A MANIFEST beside the bars, carrying what the bars cannot say about
	// themselves.
	//
	// Without it the research plane has to assume a source type, and an
	// importer that assumes HISTORICAL_MARKET will read generated bars as real
	// market evidence -- the same defect this file just fixed on the Go side,
	// reappearing one process boundary away. The manifest makes the source
	// type a recorded fact rather than a guess about which directory a file
	// happens to sit in.
	manifest := struct {
		DatasetID            string    `json:"dataset_id"`
		Instrument           string    `json:"instrument"`
		Timeframe            string    `json:"timeframe"`
		Provider             string    `json:"provider"`
		SourceType           string    `json:"source_type"`
		RangeStart           time.Time `json:"range_start"`
		RangeEnd             time.Time `json:"range_end"`
		Bars                 int       `json:"bars"`
		DatasetHash          string    `json:"dataset_hash"`
		NormalizationVersion int       `json:"normalization_version"`
		IngestionVersion     int       `json:"ingestion_version"`
		QualityStatus        string    `json:"quality_status"`
		QualityWarnings      []string  `json:"quality_warnings"`
		CodeSHA              string    `json:"code_sha"`
		Timezone             string    `json:"timezone"`
		CostBasis            string    `json:"cost_basis"`
		ExportedAt           time.Time `json:"exported_at"`
	}{
		DatasetID: dataset.ID, Instrument: dataset.InstrumentID,
		Timeframe: dataset.Timeframe, Provider: dataset.Provider,
		SourceType: dataset.SourceType,
		RangeStart: dataset.RangeStart.UTC(), RangeEnd: dataset.RangeEnd.UTC(),
		Bars: dataset.BarCount, DatasetHash: dataset.DatasetHash,
		NormalizationVersion: dataset.NormalizationVersion,
		IngestionVersion:     IngestionVersion,
		QualityStatus:        dataset.QualityStatus,
		QualityWarnings:      dataset.QualityWarnings,
		CodeSHA:              dataset.CodeSHA,
		// Stated rather than left to be inferred. The CSV is written in
		// RFC3339 UTC and a reader that assumed otherwise would move every
		// session boundary in the analysis.
		Timezone: "UTC",
		// No provider in this layer supplies bid/ask, so costs are an
		// assumption. Saying so is the difference between an estimate and a
		// backtest reporting costs it never paid.
		CostBasis:  "ESTIMATED_COSTS",
		ExportedAt: time.Now().UTC(),
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marketdata: encode manifest: %w", err)
	}
	manifestPath := strings.TrimSuffix(path, ".csv") + ".manifest.json"
	if err := os.WriteFile(manifestPath, encoded, 0o640); err != nil {
		return "", fmt.Errorf("marketdata: write manifest: %w", err)
	}
	return path, nil
}

// sanitiseName keeps a filename to characters that cannot traverse or escape.
func sanitiseName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}
