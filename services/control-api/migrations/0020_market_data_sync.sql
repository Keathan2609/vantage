-- Record where every stored bar came from, and what was asked for to get it.
--
-- # Why this table exists
--
-- `market_bars` says what a bar IS. It does not say what happened when it was
-- acquired: which range was requested, which range actually came back, how
-- many rows, whether the provider truncated the window, whether the request
-- failed halfway. Without that, a gap in the series is indistinguishable from
-- a range nobody ever asked for, and "repair the missing data" has no way to
-- know what to re-request.
--
-- Research needs it too. A historical result whose provenance is "it was in
-- the database" cannot be checked by anyone later. A segment row is the
-- evidence that a particular provider returned a particular range at a
-- particular time, under a particular normalization version.
--
-- # Why requested and received ranges are separate columns
--
-- They differ constantly and the difference is the information. A provider
-- asked for five years and returning one is not an error -- the plan may only
-- carry one -- but silently recording the returned range as though it were the
-- requested one would erase the fact that four years were asked for and are
-- not there. That erasure is how a backfill loop re-requests the same missing
-- window forever, or worse, stops asking.
--
-- # Why the raw payload is not stored
--
-- Tempting, and wrong at this size. A year of hourly bars is a few megabytes
-- of JSON per instrument and the value decays immediately: nobody re-parses a
-- provider payload from 2024. What is kept instead is enough to REPRODUCE the
-- normalization -- the request, the response shape, the row count and a hash
-- of the normalized bars -- which is the part a later reader actually needs.
-- See section 13 of the brief; this is the second option it offers.

CREATE TABLE IF NOT EXISTS market_data_segments (
    id                   TEXT PRIMARY KEY,
    instrument_id        TEXT NOT NULL REFERENCES instruments (id) ON DELETE CASCADE,
    timeframe            TEXT NOT NULL,
    provider             TEXT NOT NULL,
    -- The provider's own spelling, kept so a later reader can tell which
    -- symbol was actually requested rather than trusting that the mapping
    -- table has not changed since.
    provider_symbol      TEXT NOT NULL,

    kind                 TEXT NOT NULL,

    requested_start      TIMESTAMPTZ NOT NULL,
    requested_end        TIMESTAMPTZ NOT NULL,
    -- Nullable on purpose: a failed request received nothing, and writing the
    -- requested range here would claim data that never arrived.
    received_start       TIMESTAMPTZ,
    received_end         TIMESTAMPTZ,

    rows_returned        INTEGER NOT NULL DEFAULT 0,
    rows_stored          INTEGER NOT NULL DEFAULT 0,
    rows_duplicate       INTEGER NOT NULL DEFAULT 0,
    rows_invalid         INTEGER NOT NULL DEFAULT 0,

    status               TEXT NOT NULL,
    failure_reason       TEXT NOT NULL DEFAULT '',

    source_type          TEXT NOT NULL DEFAULT 'HISTORICAL_MARKET',
    normalization_version INTEGER NOT NULL DEFAULT 1,
    ingestion_version    INTEGER NOT NULL DEFAULT 1,
    code_sha             TEXT NOT NULL DEFAULT '',
    -- A hash of the normalized bars this segment stored, so a research dataset
    -- built from them can be shown to rest on exactly this acquisition.
    segment_hash         TEXT NOT NULL DEFAULT '',
    warnings             JSONB NOT NULL DEFAULT '[]'::jsonb,

    requested_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at         TIMESTAMPTZ,

    CONSTRAINT market_data_segments_timeframe_ck
        CHECK (timeframe IN ('1m','5m','15m','30m','1h','4h','1d')),
    CONSTRAINT market_data_segments_kind_ck
        CHECK (kind IN ('BACKFILL','SYNC','REPAIR')),
    CONSTRAINT market_data_segments_status_ck
        CHECK (status IN ('PENDING','PARTIAL','COMPLETE','FAILED','EMPTY')),
    CONSTRAINT market_data_segments_window_ck
        CHECK (requested_end > requested_start),
    -- Either both received bounds are present or neither is. A half-recorded
    -- range is worse than none: it reads as a fact and is not one.
    CONSTRAINT market_data_segments_received_ck CHECK (
        (received_start IS NULL AND received_end IS NULL)
        OR (received_start IS NOT NULL AND received_end IS NOT NULL
            AND received_end >= received_start)
    )
);

-- The coverage question is always "what do I have for this instrument, this
-- timeframe, around this time", so that is the index.
CREATE INDEX IF NOT EXISTS market_data_segments_coverage_idx
    ON market_data_segments (instrument_id, timeframe, requested_start DESC);

CREATE INDEX IF NOT EXISTS market_data_segments_provider_idx
    ON market_data_segments (provider, completed_at DESC);

-- `market_bars` already has PRIMARY KEY (instrument_id, timeframe, open_time),
-- which IS the canonical bar identity and is what makes ingestion idempotent.
-- What it lacks is an index for the query the chart and the coverage catalog
-- actually run: a bounded ascending window. The primary key serves it for the
-- leading columns, but a descending-newest lookup -- "what is the latest bar I
-- hold" -- scans it backwards on every sync decision.
CREATE INDEX IF NOT EXISTS market_bars_latest_idx
    ON market_bars (instrument_id, timeframe, open_time DESC);

-- Provider attribution. Answering "which bars came from Twelve Data" without
-- this reads the whole table.
CREATE INDEX IF NOT EXISTS market_bars_provider_idx
    ON market_bars (provider, instrument_id, timeframe);

-- Immutable research dataset snapshots.
--
-- # Why a snapshot rather than a query
--
-- A research run that references "XAUUSD 1h from 2020 to 2024" references a
-- MUTABLE thing: the next sync adds bars, a repair fills a gap, and the run is
-- no longer reproducible even though nothing about it looks different. The
-- snapshot fixes an identity -- instrument, timeframe, provider, range,
-- normalization version and a deterministic hash of the bars themselves -- and
-- the run references that.
--
-- The bars are NOT copied. Copying them would double the storage and create a
-- second thing that can drift; the hash is what detects drift, and a snapshot
-- whose recomputed hash no longer matches is a snapshot that must not be used.
CREATE TABLE IF NOT EXISTS research_datasets (
    id                   TEXT PRIMARY KEY,
    instrument_id        TEXT NOT NULL REFERENCES instruments (id) ON DELETE CASCADE,
    timeframe            TEXT NOT NULL,
    provider             TEXT NOT NULL,
    source_type          TEXT NOT NULL,

    range_start          TIMESTAMPTZ NOT NULL,
    range_end            TIMESTAMPTZ NOT NULL,
    bar_count            INTEGER NOT NULL,

    dataset_hash         TEXT NOT NULL,
    normalization_version INTEGER NOT NULL DEFAULT 1,
    quality_status       TEXT NOT NULL,
    quality_warnings     JSONB NOT NULL DEFAULT '[]'::jsonb,
    code_sha             TEXT NOT NULL DEFAULT '',

    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT research_datasets_source_ck
        CHECK (source_type IN ('SYNTHETIC_CONTROLLED','HISTORICAL_MARKET')),
    -- INVALID data may never become a dataset. The gate is here, in the
    -- schema, as well as in the code that writes the row: a research dataset
    -- built on refused bars is the one mistake this whole layer exists to
    -- prevent, and one enforcement point is not enough for it.
    CONSTRAINT research_datasets_quality_ck
        CHECK (quality_status IN ('VALID','VALID_WITH_WARNINGS')),
    CONSTRAINT research_datasets_range_ck CHECK (range_end > range_start),
    CONSTRAINT research_datasets_bars_ck CHECK (bar_count > 0)
);

-- The same identity must not produce two rows. A snapshot is defined by its
-- inputs, so re-requesting it returns the existing one rather than creating a
-- second identity for the same bars.
CREATE UNIQUE INDEX IF NOT EXISTS research_datasets_identity_idx
    ON research_datasets (instrument_id, timeframe, provider, range_start, range_end,
                          normalization_version, dataset_hash);

CREATE INDEX IF NOT EXISTS research_datasets_lookup_idx
    ON research_datasets (instrument_id, timeframe, created_at DESC);
