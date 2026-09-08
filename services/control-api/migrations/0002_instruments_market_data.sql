-- 0002_instruments_market_data: instrument specifications and price data.

-- ---------------------------------------------------------------------------
-- Instruments
--
-- The venue's arithmetic rules live in data, not in code. Nothing in the
-- platform hard-codes XAUUSD: gold is one row in this table, and adding EURUSD
-- or an index is a seed change rather than a code change.
-- ---------------------------------------------------------------------------
CREATE TABLE instruments (
    id                  TEXT PRIMARY KEY,
    symbol              TEXT NOT NULL,
    name                TEXT NOT NULL,
    asset_class         TEXT NOT NULL,
    base_currency       TEXT NOT NULL,
    quote_currency      TEXT NOT NULL,
    enabled             BOOLEAN NOT NULL DEFAULT TRUE,
    session_calendar_id TEXT NOT NULL DEFAULT 'fx_metals_24x5',

    contract_size       NUMERIC(28,10) NOT NULL,
    price_precision     INTEGER NOT NULL,
    tick_size           NUMERIC(28,10) NOT NULL,
    quantity_precision  INTEGER NOT NULL,
    min_quantity        NUMERIC(28,10) NOT NULL,
    max_quantity        NUMERIC(28,10) NOT NULL,
    quantity_step       NUMERIC(28,10) NOT NULL,
    margin_rate         NUMERIC(28,10) NOT NULL,
    max_leverage        NUMERIC(28,10) NOT NULL,
    supported_order_types TEXT[] NOT NULL,
    commission_per_lot  NUMERIC(28,10) NOT NULL DEFAULT 0,
    swap_long_per_lot   NUMERIC(28,10) NOT NULL DEFAULT 0,
    swap_short_per_lot  NUMERIC(28,10) NOT NULL DEFAULT 0,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT instruments_asset_class_ck CHECK (asset_class IN (
        'metal', 'forex', 'index', 'commodity', 'crypto', 'equity'
    )),
    CONSTRAINT instruments_base_ccy_ck CHECK (base_currency ~ '^[A-Z]{3}$'),
    CONSTRAINT instruments_quote_ccy_ck CHECK (quote_currency ~ '^[A-Z]{3}$'),
    CONSTRAINT instruments_contract_size_ck CHECK (contract_size > 0),
    CONSTRAINT instruments_tick_size_ck CHECK (tick_size > 0),
    CONSTRAINT instruments_quantities_ck CHECK (
        min_quantity > 0
        AND max_quantity >= min_quantity
        AND quantity_step > 0
        AND quantity_step <= min_quantity
    ),
    CONSTRAINT instruments_precision_ck CHECK (
        price_precision BETWEEN 0 AND 10 AND quantity_precision BETWEEN 0 AND 10
    ),
    -- A zero margin rate would imply infinite leverage.
    CONSTRAINT instruments_margin_ck CHECK (margin_rate > 0 AND margin_rate <= 1),
    CONSTRAINT instruments_leverage_ck CHECK (max_leverage > 0 AND max_leverage <= 1000),
    CONSTRAINT instruments_order_types_ck CHECK (array_length(supported_order_types, 1) >= 1)
);

CREATE UNIQUE INDEX instruments_symbol_uniq ON instruments (symbol);

-- Market holiday closures, referenced by the MarketClock.
CREATE TABLE market_holidays (
    calendar_id  TEXT NOT NULL,
    holiday_date DATE NOT NULL,
    reason       TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (calendar_id, holiday_date)
);

-- ---------------------------------------------------------------------------
-- Quotes
--
-- Both timestamps are recorded. source_time is what the provider claims;
-- ingested_at is when Vantage received it. Freshness is judged on both, so a
-- provider cannot make stale data look live by stamping its own clock forward.
-- ---------------------------------------------------------------------------
CREATE TABLE market_quotes (
    id            BIGSERIAL PRIMARY KEY,
    instrument_id TEXT NOT NULL REFERENCES instruments (id) ON DELETE CASCADE,
    bid           NUMERIC(28,10) NOT NULL,
    ask           NUMERIC(28,10) NOT NULL,
    source_time   TIMESTAMPTZ NOT NULL,
    ingested_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    provider      TEXT NOT NULL,

    CONSTRAINT market_quotes_prices_ck CHECK (bid > 0 AND ask > 0),
    -- A crossed book is a data defect, not a tradable state. Rejecting it here
    -- means no downstream consumer has to defend against it.
    CONSTRAINT market_quotes_not_crossed_ck CHECK (ask >= bid)
);

CREATE INDEX market_quotes_instrument_time_idx
    ON market_quotes (instrument_id, ingested_at DESC);

-- Latest quote per instrument, maintained by the ingestion path. Kept separate
-- from the history table so the hot read in the order pipeline is a single-row
-- primary-key lookup rather than an ORDER BY over history.
CREATE TABLE market_quotes_latest (
    instrument_id TEXT PRIMARY KEY REFERENCES instruments (id) ON DELETE CASCADE,
    bid           NUMERIC(28,10) NOT NULL,
    ask           NUMERIC(28,10) NOT NULL,
    source_time   TIMESTAMPTZ NOT NULL,
    ingested_at   TIMESTAMPTZ NOT NULL,
    provider      TEXT NOT NULL,
    -- Previous values are retained so timestamp regression and duplicate ticks
    -- are detectable without a second query.
    prev_bid         NUMERIC(28,10),
    prev_ask         NUMERIC(28,10),
    prev_source_time TIMESTAMPTZ,

    CONSTRAINT market_quotes_latest_prices_ck CHECK (bid > 0 AND ask > 0),
    CONSTRAINT market_quotes_latest_not_crossed_ck CHECK (ask >= bid)
);

-- ---------------------------------------------------------------------------
-- Bars
-- ---------------------------------------------------------------------------
CREATE TABLE market_bars (
    instrument_id TEXT NOT NULL REFERENCES instruments (id) ON DELETE CASCADE,
    timeframe     TEXT NOT NULL,
    open_time     TIMESTAMPTZ NOT NULL,
    close_time    TIMESTAMPTZ NOT NULL,
    open          NUMERIC(28,10) NOT NULL,
    high          NUMERIC(28,10) NOT NULL,
    low           NUMERIC(28,10) NOT NULL,
    close         NUMERIC(28,10) NOT NULL,
    volume        NUMERIC(28,10) NOT NULL DEFAULT 0,
    complete      BOOLEAN NOT NULL DEFAULT TRUE,
    provider      TEXT NOT NULL,
    ingested_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (instrument_id, timeframe, open_time),

    CONSTRAINT market_bars_timeframe_ck CHECK (timeframe IN ('1m','5m','15m','1h','4h','1d')),
    CONSTRAINT market_bars_prices_ck CHECK (open > 0 AND high > 0 AND low > 0 AND close > 0),
    -- OHLC coherence. A bar whose high is below its close is corrupt, and an
    -- indicator computed over it produces confident nonsense.
    CONSTRAINT market_bars_ohlc_ck CHECK (
        high >= low
        AND high >= open AND high >= close
        AND low  <= open AND low  <= close
    ),
    CONSTRAINT market_bars_window_ck CHECK (close_time > open_time),
    CONSTRAINT market_bars_volume_ck CHECK (volume >= 0)
);

CREATE INDEX market_bars_lookup_idx
    ON market_bars (instrument_id, timeframe, open_time DESC);

-- ---------------------------------------------------------------------------
-- Data-quality observations
--
-- Every automated decision records which health verdict it acted on, so "why
-- did the system not trade at 14:32?" is answerable from stored evidence
-- rather than from a re-simulation that may no longer reproduce.
-- ---------------------------------------------------------------------------
CREATE TABLE market_data_health (
    id            BIGSERIAL PRIMARY KEY,
    instrument_id TEXT NOT NULL REFERENCES instruments (id) ON DELETE CASCADE,
    state         TEXT NOT NULL,
    issues        TEXT[] NOT NULL DEFAULT '{}',
    quote_age_ms  BIGINT NOT NULL,
    spread        NUMERIC(28,10),
    spread_fraction NUMERIC(28,10),
    provider      TEXT NOT NULL,
    evaluated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT market_data_health_state_ck CHECK (
        state IN ('ok', 'degraded', 'stale', 'invalid', 'no_data')
    )
);

CREATE INDEX market_data_health_instrument_idx
    ON market_data_health (instrument_id, evaluated_at DESC);

-- ---------------------------------------------------------------------------
-- FX rates
--
-- An account in ZAR trading an instrument quoted in USD requires an explicit
-- conversion. Rates are stored with their source and timestamp so a historical
-- conversion can be reproduced exactly.
-- ---------------------------------------------------------------------------
CREATE TABLE fx_rates (
    base_currency  TEXT NOT NULL,
    quote_currency TEXT NOT NULL,
    rate           NUMERIC(28,10) NOT NULL,
    source_time    TIMESTAMPTZ NOT NULL,
    ingested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    provider       TEXT NOT NULL,

    PRIMARY KEY (base_currency, quote_currency, source_time),

    CONSTRAINT fx_rates_base_ck CHECK (base_currency ~ '^[A-Z]{3}$'),
    CONSTRAINT fx_rates_quote_ck CHECK (quote_currency ~ '^[A-Z]{3}$'),
    CONSTRAINT fx_rates_rate_ck CHECK (rate > 0),
    CONSTRAINT fx_rates_distinct_ck CHECK (base_currency <> quote_currency)
);

CREATE TABLE fx_rates_latest (
    base_currency  TEXT NOT NULL,
    quote_currency TEXT NOT NULL,
    rate           NUMERIC(28,10) NOT NULL,
    source_time    TIMESTAMPTZ NOT NULL,
    ingested_at    TIMESTAMPTZ NOT NULL,
    provider       TEXT NOT NULL,

    PRIMARY KEY (base_currency, quote_currency),

    CONSTRAINT fx_rates_latest_rate_ck CHECK (rate > 0)
);
