package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vantage/control-api/internal/domain"
)

// Acquisition provenance, coverage and immutable research snapshots.
//
// Separated from market.go because the questions differ. market.go answers
// "what is the price"; this answers "where did that come from, what is
// missing, and can a research run still reproduce it".

// SegmentKind is why an acquisition happened.
type SegmentKind string

const (
	SegmentBackfill SegmentKind = "BACKFILL"
	SegmentSync     SegmentKind = "SYNC"
	SegmentRepair   SegmentKind = "REPAIR"
)

// SegmentStatus is how an acquisition ended.
type SegmentStatus string

const (
	SegmentPending  SegmentStatus = "PENDING"
	SegmentPartial  SegmentStatus = "PARTIAL"
	SegmentComplete SegmentStatus = "COMPLETE"
	SegmentFailed   SegmentStatus = "FAILED"
	// SegmentEmpty is a success that returned nothing. Distinct from FAILED
	// because "the provider has no data for this window" is an answer, and
	// re-requesting it forever would be the alternative.
	SegmentEmpty SegmentStatus = "EMPTY"
)

// MarketDataSegment is one acquisition attempt, requested and received.
type MarketDataSegment struct {
	ID                   string        `json:"id"`
	InstrumentID         string        `json:"instrument_id"`
	Timeframe            string        `json:"timeframe"`
	Provider             string        `json:"provider"`
	ProviderSymbol       string        `json:"provider_symbol"`
	Kind                 SegmentKind   `json:"kind"`
	RequestedStart       time.Time     `json:"requested_start"`
	RequestedEnd         time.Time     `json:"requested_end"`
	ReceivedStart        *time.Time    `json:"received_start,omitempty"`
	ReceivedEnd          *time.Time    `json:"received_end,omitempty"`
	RowsReturned         int           `json:"rows_returned"`
	RowsStored           int           `json:"rows_stored"`
	RowsDuplicate        int           `json:"rows_duplicate"`
	RowsInvalid          int           `json:"rows_invalid"`
	Status               SegmentStatus `json:"status"`
	FailureReason        string        `json:"failure_reason,omitempty"`
	SourceType           string        `json:"source_type"`
	NormalizationVersion int           `json:"normalization_version"`
	IngestionVersion     int           `json:"ingestion_version"`
	CodeSHA              string        `json:"code_sha,omitempty"`
	SegmentHash          string        `json:"segment_hash,omitempty"`
	Warnings             []string      `json:"warnings"`
	RequestedAt          time.Time     `json:"requested_at"`
	CompletedAt          *time.Time    `json:"completed_at,omitempty"`
}

// RecordSegment writes one acquisition record.
func (s *MarketStore) RecordSegment(ctx context.Context, seg MarketDataSegment) error {
	warnings, err := json.Marshal(seg.Warnings)
	if err != nil {
		return fmt.Errorf("store: encode segment warnings: %w", err)
	}
	if seg.Warnings == nil {
		warnings = []byte("[]")
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO market_data_segments (
			id, instrument_id, timeframe, provider, provider_symbol, kind,
			requested_start, requested_end, received_start, received_end,
			rows_returned, rows_stored, rows_duplicate, rows_invalid,
			status, failure_reason, source_type, normalization_version,
			ingestion_version, code_sha, segment_hash, warnings, completed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
		ON CONFLICT (id) DO UPDATE SET
			received_start = EXCLUDED.received_start,
			received_end   = EXCLUDED.received_end,
			rows_returned  = EXCLUDED.rows_returned,
			rows_stored    = EXCLUDED.rows_stored,
			rows_duplicate = EXCLUDED.rows_duplicate,
			rows_invalid   = EXCLUDED.rows_invalid,
			status         = EXCLUDED.status,
			failure_reason = EXCLUDED.failure_reason,
			segment_hash   = EXCLUDED.segment_hash,
			warnings       = EXCLUDED.warnings,
			completed_at   = EXCLUDED.completed_at`,
		seg.ID, seg.InstrumentID, seg.Timeframe, seg.Provider, seg.ProviderSymbol,
		string(seg.Kind), seg.RequestedStart, seg.RequestedEnd,
		seg.ReceivedStart, seg.ReceivedEnd,
		seg.RowsReturned, seg.RowsStored, seg.RowsDuplicate, seg.RowsInvalid,
		string(seg.Status), seg.FailureReason, seg.SourceType,
		seg.NormalizationVersion, seg.IngestionVersion, seg.CodeSHA, seg.SegmentHash,
		warnings, seg.CompletedAt)
	return mapError(err)
}

// Segments lists recent acquisition records for an instrument and timeframe.
func (s *MarketStore) Segments(
	ctx context.Context, instrumentID string, tf domain.Timeframe, limit int,
) ([]MarketDataSegment, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, instrument_id, timeframe, provider, provider_symbol, kind,
		       requested_start, requested_end, received_start, received_end,
		       rows_returned, rows_stored, rows_duplicate, rows_invalid,
		       status, failure_reason, source_type, normalization_version,
		       ingestion_version, code_sha, segment_hash, warnings,
		       requested_at, completed_at
		FROM market_data_segments
		WHERE instrument_id = $1 AND timeframe = $2
		ORDER BY requested_at DESC
		LIMIT $3`, instrumentID, string(tf), limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var out []MarketDataSegment
	for rows.Next() {
		var seg MarketDataSegment
		var warnings []byte
		if err := rows.Scan(
			&seg.ID, &seg.InstrumentID, &seg.Timeframe, &seg.Provider, &seg.ProviderSymbol,
			&seg.Kind, &seg.RequestedStart, &seg.RequestedEnd, &seg.ReceivedStart,
			&seg.ReceivedEnd, &seg.RowsReturned, &seg.RowsStored, &seg.RowsDuplicate,
			&seg.RowsInvalid, &seg.Status, &seg.FailureReason, &seg.SourceType,
			&seg.NormalizationVersion, &seg.IngestionVersion, &seg.CodeSHA,
			&seg.SegmentHash, &warnings, &seg.RequestedAt, &seg.CompletedAt,
		); err != nil {
			return nil, mapError(err)
		}
		if len(warnings) > 0 {
			_ = json.Unmarshal(warnings, &seg.Warnings)
		}
		if seg.Warnings == nil {
			seg.Warnings = []string{}
		}
		out = append(out, seg)
	}
	return out, mapError(rows.Err())
}

// ---------------------------------------------------------------------------
// Coverage
// ---------------------------------------------------------------------------

// Coverage is what Vantage actually holds for one instrument and timeframe.
//
// The source of truth for "do we have enough data yet", which is otherwise a
// question people answer by guessing.
type Coverage struct {
	InstrumentID   string     `json:"instrument_id"`
	Timeframe      string     `json:"timeframe"`
	Bars           int        `json:"bars"`
	EarliestBar    *time.Time `json:"earliest_bar,omitempty"`
	LatestBar      *time.Time `json:"latest_bar,omitempty"`
	Providers      []string   `json:"providers"`
	LastSyncAt     *time.Time `json:"last_sync_at,omitempty"`
	LastSyncKind   string     `json:"last_sync_kind,omitempty"`
	LastSyncStatus string     `json:"last_sync_status,omitempty"`
}

// Coverage summarises local holdings.
func (s *MarketStore) Coverage(
	ctx context.Context, instrumentID string, tf domain.Timeframe,
) (Coverage, error) {
	c := Coverage{InstrumentID: instrumentID, Timeframe: string(tf), Providers: []string{}}

	var providers []string
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), min(open_time), max(open_time),
		       coalesce(array_agg(DISTINCT provider) FILTER (WHERE provider IS NOT NULL), '{}')
		FROM market_bars
		WHERE instrument_id = $1 AND timeframe = $2 AND complete = TRUE`,
		instrumentID, string(tf)).Scan(&c.Bars, &c.EarliestBar, &c.LatestBar, &providers)
	if err != nil {
		return Coverage{}, mapError(err)
	}
	c.Providers = providers
	if c.Providers == nil {
		c.Providers = []string{}
	}

	// The most recent acquisition, if any. Absent is normal for a store that
	// was seeded rather than synchronised, and is reported as absent rather
	// than as a zero time that reads like 1970.
	var at *time.Time
	var kind, status *string
	err = s.pool.QueryRow(ctx, `
		SELECT completed_at, kind, status
		FROM market_data_segments
		WHERE instrument_id = $1 AND timeframe = $2
		ORDER BY requested_at DESC
		LIMIT 1`, instrumentID, string(tf)).Scan(&at, &kind, &status)
	switch {
	case err == nil:
		c.LastSyncAt = at
		if kind != nil {
			c.LastSyncKind = *kind
		}
		if status != nil {
			c.LastSyncStatus = *status
		}
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return Coverage{}, mapError(err)
	}
	return c, nil
}

// LatestBarTime returns the newest stored bar's open time, if any.
//
// Incremental sync is built on this: fetch from here forward rather than
// re-downloading a decade every time.
func (s *MarketStore) LatestBarTime(
	ctx context.Context, instrumentID string, tf domain.Timeframe,
) (time.Time, bool, error) {
	var t *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT max(open_time) FROM market_bars
		WHERE instrument_id = $1 AND timeframe = $2 AND complete = TRUE`,
		instrumentID, string(tf)).Scan(&t)
	if err != nil {
		return time.Time{}, false, mapError(err)
	}
	if t == nil {
		return time.Time{}, false, nil
	}
	return t.UTC(), true, nil
}

// BarGap is a contiguous run of missing bars.
type BarGap struct {
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	MissingBars int       `json:"missing_bars"`
}

// FindGaps locates missing runs between the stored bars in a range.
//
// # What counts as a gap
//
// Only the space BETWEEN bars that exist. A range extending beyond the stored
// series is not a gap -- nothing in the data can say the series ought to have
// continued -- and reporting it as one would make every fresh instrument look
// broken and send a repair at a range the provider may never have had.
//
// Market closures are not filtered here. This is a structural query; deciding
// which absences are legitimate needs the market clock and belongs to the
// caller, which has one.
func (s *MarketStore) FindGaps(
	ctx context.Context, instrumentID string, tf domain.Timeframe, from, to time.Time,
) ([]BarGap, error) {
	d, err := tf.Duration()
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT open_time FROM market_bars
		WHERE instrument_id = $1 AND timeframe = $2
		  AND open_time >= $3 AND open_time < $4 AND complete = TRUE
		ORDER BY open_time ASC`, instrumentID, string(tf), from, to)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var times []time.Time
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err != nil {
			return nil, mapError(err)
		}
		times = append(times, t.UTC())
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}

	gaps := []BarGap{}
	for i := 1; i < len(times); i++ {
		expected := times[i-1].Add(d)
		if times[i].After(expected) {
			missing := int(times[i].Sub(expected) / d)
			gaps = append(gaps, BarGap{From: expected, To: times[i], MissingBars: missing})
		}
	}
	return gaps, nil
}

// ---------------------------------------------------------------------------
// Research dataset snapshots
// ---------------------------------------------------------------------------

// ResearchDataset is an immutable identity for a range of stored bars.
type ResearchDataset struct {
	ID                   string    `json:"id"`
	InstrumentID         string    `json:"instrument_id"`
	Timeframe            string    `json:"timeframe"`
	Provider             string    `json:"provider"`
	SourceType           string    `json:"source_type"`
	RangeStart           time.Time `json:"range_start"`
	RangeEnd             time.Time `json:"range_end"`
	BarCount             int       `json:"bar_count"`
	DatasetHash          string    `json:"dataset_hash"`
	NormalizationVersion int       `json:"normalization_version"`
	QualityStatus        string    `json:"quality_status"`
	QualityWarnings      []string  `json:"quality_warnings"`
	CodeSHA              string    `json:"code_sha,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
}

// UpsertResearchDataset records a snapshot, returning the existing row when
// the same identity has been created before.
//
// Idempotent by identity rather than by id: the same instrument, timeframe,
// provider, range, normalization version and bar hash IS the same dataset, and
// minting a second id for it would let two research runs claim different
// provenance for identical bars.
func (s *MarketStore) UpsertResearchDataset(
	ctx context.Context, d ResearchDataset,
) (ResearchDataset, error) {
	warnings, err := json.Marshal(d.QualityWarnings)
	if err != nil {
		return ResearchDataset{}, fmt.Errorf("store: encode dataset warnings: %w", err)
	}
	if d.QualityWarnings == nil {
		warnings = []byte("[]")
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO research_datasets (
			id, instrument_id, timeframe, provider, source_type,
			range_start, range_end, bar_count, dataset_hash,
			normalization_version, quality_status, quality_warnings, code_sha)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (instrument_id, timeframe, provider, range_start, range_end,
		             normalization_version, dataset_hash)
		-- The id and creation time are the identity's history and are kept.
		-- Everything else is a pure FUNCTION of that identity -- the same bars
		-- assessed by the same checker give the same answer -- so it is
		-- refreshed rather than preserved. That makes the table self-healing
		-- when a derivation bug is fixed: a row written by buggy code would
		-- otherwise keep its wrong value forever, and a snapshot mislabelled
		-- HISTORICAL_MARKET is exactly the kind of lie nobody re-checks.
		DO UPDATE SET
			id             = research_datasets.id,
			source_type    = EXCLUDED.source_type,
			bar_count      = EXCLUDED.bar_count,
			quality_status = EXCLUDED.quality_status,
			quality_warnings = EXCLUDED.quality_warnings,
			code_sha       = EXCLUDED.code_sha
		RETURNING id, instrument_id, timeframe, provider, source_type,
		          range_start, range_end, bar_count, dataset_hash,
		          normalization_version, quality_status, quality_warnings,
		          code_sha, created_at`,
		d.ID, d.InstrumentID, d.Timeframe, d.Provider, d.SourceType,
		d.RangeStart, d.RangeEnd, d.BarCount, d.DatasetHash,
		d.NormalizationVersion, d.QualityStatus, warnings, d.CodeSHA)

	return scanResearchDataset(row)
}

// ResearchDatasetByID returns one snapshot.
func (s *MarketStore) ResearchDatasetByID(
	ctx context.Context, id string,
) (ResearchDataset, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, instrument_id, timeframe, provider, source_type,
		       range_start, range_end, bar_count, dataset_hash,
		       normalization_version, quality_status, quality_warnings,
		       code_sha, created_at
		FROM research_datasets WHERE id = $1`, id)
	return scanResearchDataset(row)
}

// ListResearchDatasets returns recent snapshots, newest first.
func (s *MarketStore) ListResearchDatasets(
	ctx context.Context, limit int,
) ([]ResearchDataset, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, instrument_id, timeframe, provider, source_type,
		       range_start, range_end, bar_count, dataset_hash,
		       normalization_version, quality_status, quality_warnings,
		       code_sha, created_at
		FROM research_datasets ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	out := []ResearchDataset{}
	for rows.Next() {
		d, err := scanResearchDatasetRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, mapError(rows.Err())
}

type scannable interface {
	Scan(dest ...any) error
}

func scanResearchDataset(row scannable) (ResearchDataset, error) {
	var d ResearchDataset
	var warnings []byte
	if err := row.Scan(&d.ID, &d.InstrumentID, &d.Timeframe, &d.Provider, &d.SourceType,
		&d.RangeStart, &d.RangeEnd, &d.BarCount, &d.DatasetHash,
		&d.NormalizationVersion, &d.QualityStatus, &warnings, &d.CodeSHA,
		&d.CreatedAt); err != nil {
		return ResearchDataset{}, mapError(err)
	}
	if len(warnings) > 0 {
		_ = json.Unmarshal(warnings, &d.QualityWarnings)
	}
	if d.QualityWarnings == nil {
		d.QualityWarnings = []string{}
	}
	return d, nil
}

func scanResearchDatasetRows(rows pgx.Rows) (ResearchDataset, error) {
	return scanResearchDataset(rows)
}
