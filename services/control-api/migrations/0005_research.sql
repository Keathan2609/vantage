-- 0005_research: strategies, signals, decisions, backtests, ML, calendar, news.
--
-- Everything in this migration belongs to the RESEARCH plane. Rows here
-- describe opinions, experiments and evidence. None of them can move money:
-- the only path from research to execution is a signal that the orchestrator
-- may choose to turn into an order intent, which then passes every control in
-- the order pipeline.

-- ---------------------------------------------------------------------------
-- Strategies
-- ---------------------------------------------------------------------------
CREATE TABLE strategies (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key         TEXT NOT NULL,
    name        TEXT NOT NULL,
    family      TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    -- Approaches whose exposure profile is inherently dangerous (grid,
    -- martingale, averaging down). Disabled by default and never exempt from
    -- account-level ceilings.
    high_risk   BOOLEAN NOT NULL DEFAULT FALSE,
    enabled     BOOLEAN NOT NULL DEFAULT FALSE,
    owner_user_id UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT strategies_family_ck CHECK (family IN (
        'trend_following', 'momentum', 'mean_reversion', 'breakout', 'volatility',
        'multi_timeframe', 'session', 'statistical', 'event_driven', 'ensemble',
        'machine_learning', 'high_risk_research'
    )),
    -- High-risk research strategies may not be enabled by a plain data edit;
    -- enabling one is a deliberate act recorded in the audit log.
    CONSTRAINT strategies_high_risk_default_off_ck CHECK (
        high_risk = FALSE OR enabled = FALSE
    ),
    CONSTRAINT strategies_key_ck CHECK (key ~ '^[a-z0-9_]{3,64}$')
);

CREATE UNIQUE INDEX strategies_key_uniq ON strategies (key);

CREATE TABLE strategy_versions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    strategy_id  UUID NOT NULL REFERENCES strategies (id) ON DELETE CASCADE,
    version      INTEGER NOT NULL,
    -- Code identity. A backtest result is meaningless without knowing exactly
    -- which implementation produced it.
    code_hash    TEXT NOT NULL,
    git_sha      TEXT NOT NULL DEFAULT '',
    parameters   JSONB NOT NULL,
    timeframe    TEXT NOT NULL,
    instruments  TEXT[] NOT NULL,
    lifecycle    TEXT NOT NULL DEFAULT 'DRAFT',
    valid_regimes TEXT[] NOT NULL DEFAULT '{}',
    notes        TEXT NOT NULL DEFAULT '',
    created_by   UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    promoted_at  TIMESTAMPTZ,
    retired_at   TIMESTAMPTZ,

    CONSTRAINT strategy_versions_version_ck CHECK (version > 0),
    CONSTRAINT strategy_versions_timeframe_ck CHECK (
        timeframe IN ('1m','5m','15m','1h','4h','1d')
    ),
    CONSTRAINT strategy_versions_lifecycle_ck CHECK (lifecycle IN (
        'DRAFT', 'RESEARCH', 'BACKTESTED', 'VALIDATED', 'PAPER', 'DEMO', 'LIVE', 'RETIRED'
    )),
    -- This build's ceiling, enforced by the database. DEMO and LIVE rows are
    -- not storable, so no code path and no manual UPDATE can promote past
    -- PAPER. Lifting this is an explicit live-readiness step.
    CONSTRAINT strategy_versions_paper_ceiling_ck CHECK (
        lifecycle IN ('DRAFT', 'RESEARCH', 'BACKTESTED', 'VALIDATED', 'PAPER', 'RETIRED')
    ),
    CONSTRAINT strategy_versions_instruments_ck CHECK (array_length(instruments, 1) >= 1)
);

CREATE UNIQUE INDEX strategy_versions_uniq ON strategy_versions (strategy_id, version);
CREATE INDEX strategy_versions_lifecycle_idx ON strategy_versions (lifecycle);

CREATE TABLE strategy_lifecycle_history (
    id            BIGSERIAL PRIMARY KEY,
    strategy_id   UUID NOT NULL,
    version       INTEGER NOT NULL,
    from_state    TEXT,
    to_state      TEXT NOT NULL,
    evidence      JSONB NOT NULL DEFAULT '{}'::jsonb,
    changed_by    UUID REFERENCES users (id) ON DELETE SET NULL,
    reason        TEXT,
    changed_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER strategy_lifecycle_history_append_only
    BEFORE UPDATE OR DELETE ON strategy_lifecycle_history
    FOR EACH ROW EXECUTE FUNCTION vantage_reject_mutation();

-- How much of an account's risk budget one strategy may consume.
CREATE TABLE strategy_allocations (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id       UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    strategy_id      UUID NOT NULL REFERENCES strategies (id) ON DELETE CASCADE,
    strategy_version INTEGER NOT NULL,
    enabled          BOOLEAN NOT NULL DEFAULT FALSE,
    max_risk_fraction NUMERIC(28,10) NOT NULL,
    max_open_positions INTEGER NOT NULL DEFAULT 1,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT strategy_allocations_fraction_ck CHECK (
        max_risk_fraction > 0 AND max_risk_fraction <= 1
    ),
    CONSTRAINT strategy_allocations_positions_ck CHECK (max_open_positions >= 0)
);

CREATE UNIQUE INDEX strategy_allocations_uniq
    ON strategy_allocations (account_id, strategy_id);

-- ---------------------------------------------------------------------------
-- Strategy runs and signals
-- ---------------------------------------------------------------------------
CREATE TABLE strategy_runs (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    strategy_id      UUID NOT NULL REFERENCES strategies (id) ON DELETE CASCADE,
    strategy_version INTEGER NOT NULL,
    account_id       UUID REFERENCES accounts (id) ON DELETE CASCADE,
    instrument_id    TEXT NOT NULL REFERENCES instruments (id) ON DELETE CASCADE,
    timeframe        TEXT NOT NULL,
    bar_time         TIMESTAMPTZ,
    status           TEXT NOT NULL,
    skip_reason      TEXT,
    error            TEXT,
    duration_ms      BIGINT NOT NULL DEFAULT 0,
    started_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at      TIMESTAMPTZ,

    CONSTRAINT strategy_runs_status_ck CHECK (
        status IN ('succeeded', 'no_signal', 'skipped', 'failed')
    )
);

CREATE INDEX strategy_runs_strategy_idx ON strategy_runs (strategy_id, started_at DESC);
CREATE INDEX strategy_runs_account_idx ON strategy_runs (account_id, started_at DESC);

-- One evaluation per strategy version per bar: the guard against a scheduler
-- running the same bar twice and emitting duplicate signals.
CREATE UNIQUE INDEX strategy_runs_bar_uniq
    ON strategy_runs (strategy_id, strategy_version, instrument_id, timeframe, bar_time)
    WHERE bar_time IS NOT NULL AND account_id IS NULL;

CREATE UNIQUE INDEX strategy_runs_account_bar_uniq
    ON strategy_runs (account_id, strategy_id, strategy_version, instrument_id, timeframe, bar_time)
    WHERE bar_time IS NOT NULL AND account_id IS NOT NULL;

CREATE TABLE strategy_signals (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id           UUID NOT NULL REFERENCES strategy_runs (id) ON DELETE CASCADE,
    strategy_id      UUID NOT NULL REFERENCES strategies (id) ON DELETE CASCADE,
    strategy_version INTEGER NOT NULL,
    account_id       UUID REFERENCES accounts (id) ON DELETE CASCADE,
    instrument_id    TEXT NOT NULL REFERENCES instruments (id) ON DELETE CASCADE,
    timeframe        TEXT NOT NULL,
    action           TEXT NOT NULL,
    confidence       NUMERIC(28,10) NOT NULL,
    suggested_stop   NUMERIC(28,10),
    suggested_target NUMERIC(28,10),
    explanation      TEXT NOT NULL DEFAULT '',
    features         JSONB NOT NULL DEFAULT '{}'::jsonb,
    bar_time         TIMESTAMPTZ NOT NULL,
    generated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT strategy_signals_action_ck CHECK (
        action IN ('buy', 'sell', 'hold', 'close', 'no_trade')
    ),
    CONSTRAINT strategy_signals_confidence_ck CHECK (confidence >= 0 AND confidence <= 1)
);

CREATE INDEX strategy_signals_instrument_idx
    ON strategy_signals (instrument_id, generated_at DESC);
CREATE INDEX strategy_signals_strategy_idx
    ON strategy_signals (strategy_id, generated_at DESC);

-- ---------------------------------------------------------------------------
-- Decision snapshots
--
-- The immutable record of WHY an order intent existed, including the intents
-- that were refused. Never contains secrets.
-- ---------------------------------------------------------------------------
CREATE TABLE decision_snapshots (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id        UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    strategy_id       UUID,
    strategy_version  INTEGER,
    model_id          UUID,
    model_version     INTEGER,
    instrument_id     TEXT NOT NULL REFERENCES instruments (id) ON DELETE CASCADE,
    bar_time          TIMESTAMPTZ,
    quote             JSONB NOT NULL DEFAULT '{}'::jsonb,
    indicators        JSONB NOT NULL DEFAULT '{}'::jsonb,
    features          JSONB NOT NULL DEFAULT '{}'::jsonb,
    event_context     JSONB NOT NULL DEFAULT '{}'::jsonb,
    portfolio_context JSONB NOT NULL DEFAULT '{}'::jsonb,
    risk_state        JSONB NOT NULL DEFAULT '{}'::jsonb,
    authority_state   JSONB NOT NULL DEFAULT '{}'::jsonb,
    market_data_health JSONB NOT NULL DEFAULT '{}'::jsonb,
    signal_action     TEXT NOT NULL,
    confidence        NUMERIC(28,10),
    requested_quantity NUMERIC(28,10),
    approved_quantity  NUMERIC(28,10),
    outcome           TEXT NOT NULL,
    outcome_code      TEXT,
    outcome_reason    TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT decision_snapshots_outcome_ck CHECK (
        outcome IN ('accepted', 'rejected', 'no_trade')
    )
);

CREATE INDEX decision_snapshots_account_idx ON decision_snapshots (account_id, created_at DESC);
CREATE INDEX decision_snapshots_instrument_idx ON decision_snapshots (instrument_id, created_at DESC);

CREATE TRIGGER decision_snapshots_append_only
    BEFORE UPDATE OR DELETE ON decision_snapshots
    FOR EACH ROW EXECUTE FUNCTION vantage_reject_mutation();

-- ---------------------------------------------------------------------------
-- Backtests
-- ---------------------------------------------------------------------------
CREATE TABLE backtests (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    strategy_id      UUID NOT NULL REFERENCES strategies (id) ON DELETE CASCADE,
    strategy_version INTEGER NOT NULL,
    instrument_id    TEXT NOT NULL REFERENCES instruments (id) ON DELETE CASCADE,
    timeframe        TEXT NOT NULL,
    -- Data provenance: which rows, in which range, hashed. Two runs claiming
    -- the same result must be over the same data.
    dataset_hash     TEXT NOT NULL,
    period_start     TIMESTAMPTZ NOT NULL,
    period_end       TIMESTAMPTZ NOT NULL,
    -- Which slice of the timeline this run represents. Mixing these up is how
    -- an in-sample result gets presented as evidence of an edge.
    sample_kind      TEXT NOT NULL,
    initial_capital  NUMERIC(28,10) NOT NULL,
    currency         TEXT NOT NULL,
    parameters       JSONB NOT NULL,
    cost_model       JSONB NOT NULL,
    -- A frictionless run is explicitly labelled so it can never be read as a
    -- realistic expectation.
    frictionless     BOOLEAN NOT NULL DEFAULT FALSE,
    metrics          JSONB NOT NULL DEFAULT '{}'::jsonb,
    equity_curve     JSONB NOT NULL DEFAULT '[]'::jsonb,
    warnings         TEXT[] NOT NULL DEFAULT '{}',
    code_hash        TEXT NOT NULL DEFAULT '',
    seed             BIGINT,
    status           TEXT NOT NULL DEFAULT 'completed',
    created_by       UUID REFERENCES users (id) ON DELETE SET NULL,
    started_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at      TIMESTAMPTZ,

    CONSTRAINT backtests_period_ck CHECK (period_end > period_start),
    CONSTRAINT backtests_capital_ck CHECK (initial_capital > 0),
    CONSTRAINT backtests_currency_ck CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT backtests_sample_kind_ck CHECK (
        sample_kind IN ('in_sample', 'validation', 'out_of_sample', 'walk_forward', 'paper_forward')
    ),
    CONSTRAINT backtests_status_ck CHECK (status IN ('running', 'completed', 'failed'))
);

CREATE INDEX backtests_strategy_idx ON backtests (strategy_id, started_at DESC);

CREATE TABLE backtest_trades (
    id           BIGSERIAL PRIMARY KEY,
    backtest_id  UUID NOT NULL REFERENCES backtests (id) ON DELETE CASCADE,
    instrument_id TEXT NOT NULL,
    side         TEXT NOT NULL,
    quantity     NUMERIC(28,10) NOT NULL,
    entry_time   TIMESTAMPTZ NOT NULL,
    entry_price  NUMERIC(28,10) NOT NULL,
    exit_time    TIMESTAMPTZ,
    exit_price   NUMERIC(28,10),
    gross_pnl    NUMERIC(28,10) NOT NULL DEFAULT 0,
    commission   NUMERIC(28,10) NOT NULL DEFAULT 0,
    slippage     NUMERIC(28,10) NOT NULL DEFAULT 0,
    swap         NUMERIC(28,10) NOT NULL DEFAULT 0,
    net_pnl      NUMERIC(28,10) NOT NULL DEFAULT 0,
    mae          NUMERIC(28,10),
    mfe          NUMERIC(28,10),
    exit_reason  TEXT,

    CONSTRAINT backtest_trades_side_ck CHECK (side IN ('buy', 'sell')),
    CONSTRAINT backtest_trades_quantity_ck CHECK (quantity > 0)
);

CREATE INDEX backtest_trades_backtest_idx ON backtest_trades (backtest_id, entry_time);

-- ---------------------------------------------------------------------------
-- Machine learning
-- ---------------------------------------------------------------------------
CREATE TABLE ml_datasets (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL,
    instrument_id TEXT NOT NULL REFERENCES instruments (id) ON DELETE CASCADE,
    timeframe     TEXT NOT NULL,
    feature_set   JSONB NOT NULL,
    label_definition JSONB NOT NULL,
    period_start  TIMESTAMPTZ NOT NULL,
    period_end    TIMESTAMPTZ NOT NULL,
    row_count     INTEGER NOT NULL DEFAULT 0,
    content_hash  TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ml_datasets_period_ck CHECK (period_end > period_start),
    CONSTRAINT ml_datasets_rows_ck CHECK (row_count >= 0)
);

CREATE UNIQUE INDEX ml_datasets_hash_uniq ON ml_datasets (content_hash);

CREATE TABLE ml_models (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key         TEXT NOT NULL,
    name        TEXT NOT NULL,
    task        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ml_models_task_ck CHECK (task IN (
        'direction_probability', 'regime_classification', 'volatility_forecast',
        'anomaly_detection', 'signal_filter', 'opportunity_ranking'
    )),
    CONSTRAINT ml_models_key_ck CHECK (key ~ '^[a-z0-9_]{3,64}$')
);

CREATE UNIQUE INDEX ml_models_key_uniq ON ml_models (key);

CREATE TABLE ml_model_versions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id      UUID NOT NULL REFERENCES ml_models (id) ON DELETE CASCADE,
    version       INTEGER NOT NULL,
    dataset_id    UUID NOT NULL REFERENCES ml_datasets (id) ON DELETE RESTRICT,
    algorithm     TEXT NOT NULL,
    hyperparameters JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Reproducibility record. Without every one of these, a "reproduced" model
    -- is a coincidence.
    code_git_sha  TEXT NOT NULL DEFAULT '',
    dataset_hash  TEXT NOT NULL,
    feature_definition JSONB NOT NULL,
    label_definition   JSONB NOT NULL,
    train_start   TIMESTAMPTZ NOT NULL,
    train_end     TIMESTAMPTZ NOT NULL,
    validation_start TIMESTAMPTZ,
    validation_end   TIMESTAMPTZ,
    test_start    TIMESTAMPTZ,
    test_end      TIMESTAMPTZ,
    random_seed   BIGINT NOT NULL DEFAULT 0,
    dependency_versions JSONB NOT NULL DEFAULT '{}'::jsonb,
    artifact_path TEXT,
    artifact_hash TEXT,
    lifecycle     TEXT NOT NULL DEFAULT 'EXPERIMENTAL',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    promoted_at   TIMESTAMPTZ,
    retired_at    TIMESTAMPTZ,

    CONSTRAINT ml_model_versions_version_ck CHECK (version > 0),
    -- Chronology. Training must not overlap validation, and validation must not
    -- overlap test: overlapping windows are how leakage enters unnoticed.
    CONSTRAINT ml_model_versions_train_ck CHECK (train_end > train_start),
    CONSTRAINT ml_model_versions_validation_ck CHECK (
        validation_start IS NULL OR validation_start >= train_end
    ),
    CONSTRAINT ml_model_versions_test_ck CHECK (
        test_start IS NULL OR validation_end IS NULL OR test_start >= validation_end
    ),
    CONSTRAINT ml_model_versions_lifecycle_ck CHECK (lifecycle IN (
        'EXPERIMENTAL', 'VALIDATED', 'PAPER', 'DEMO', 'LIVE', 'RETIRED'
    )),
    -- Same ceiling as strategies: PAPER is as far as this build goes.
    CONSTRAINT ml_model_versions_paper_ceiling_ck CHECK (
        lifecycle IN ('EXPERIMENTAL', 'VALIDATED', 'PAPER', 'RETIRED')
    )
);

CREATE UNIQUE INDEX ml_model_versions_uniq ON ml_model_versions (model_id, version);

CREATE TABLE ml_evaluations (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_version_id UUID NOT NULL REFERENCES ml_model_versions (id) ON DELETE CASCADE,
    split            TEXT NOT NULL,
    metrics          JSONB NOT NULL,
    period_start     TIMESTAMPTZ NOT NULL,
    period_end       TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ml_evaluations_split_ck CHECK (
        split IN ('train', 'validation', 'test', 'walk_forward', 'paper')
    )
);

CREATE INDEX ml_evaluations_model_idx ON ml_evaluations (model_version_id);

CREATE TABLE ml_predictions (
    id               BIGSERIAL PRIMARY KEY,
    model_version_id UUID NOT NULL REFERENCES ml_model_versions (id) ON DELETE CASCADE,
    instrument_id    TEXT NOT NULL REFERENCES instruments (id) ON DELETE CASCADE,
    bar_time         TIMESTAMPTZ NOT NULL,
    prediction       JSONB NOT NULL,
    probability      NUMERIC(28,10),
    features_hash    TEXT NOT NULL,
    latency_ms       INTEGER NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ml_predictions_probability_ck CHECK (
        probability IS NULL OR (probability >= 0 AND probability <= 1)
    )
);

CREATE UNIQUE INDEX ml_predictions_uniq
    ON ml_predictions (model_version_id, instrument_id, bar_time);

CREATE TABLE ml_drift_events (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_version_id UUID NOT NULL REFERENCES ml_model_versions (id) ON DELETE CASCADE,
    kind             TEXT NOT NULL,
    severity         TEXT NOT NULL,
    detail           JSONB NOT NULL DEFAULT '{}'::jsonb,
    detected_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ml_drift_events_kind_ck CHECK (kind IN (
        'feature_drift', 'prediction_drift', 'calibration_drift',
        'missing_features', 'performance_decay'
    )),
    CONSTRAINT ml_drift_events_severity_ck CHECK (severity IN ('info', 'warning', 'critical'))
);

-- ---------------------------------------------------------------------------
-- Economic calendar and news
--
-- Calendar events and news headlines are different systems with different
-- shapes and different reliability. They are modelled separately.
-- ---------------------------------------------------------------------------
CREATE TABLE economic_events (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    external_id    TEXT NOT NULL,
    scheduled_at   TIMESTAMPTZ NOT NULL,
    country        TEXT NOT NULL,
    currency       TEXT NOT NULL,
    impact         TEXT NOT NULL,
    event_name     TEXT NOT NULL,
    category       TEXT,
    actual         TEXT,
    forecast       TEXT,
    previous       TEXT,
    revised        TEXT,
    unit           TEXT,
    status         TEXT NOT NULL DEFAULT 'scheduled',
    source         TEXT NOT NULL,
    -- Both timestamps, so historical research can use point-in-time data
    -- rather than values that were revised after the fact.
    released_at    TIMESTAMPTZ,
    ingested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT economic_events_impact_ck CHECK (impact IN ('low', 'medium', 'high')),
    CONSTRAINT economic_events_currency_ck CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT economic_events_status_ck CHECK (
        status IN ('scheduled', 'released', 'revised', 'cancelled')
    )
);

CREATE UNIQUE INDEX economic_events_external_uniq ON economic_events (source, external_id);
CREATE INDEX economic_events_schedule_idx ON economic_events (scheduled_at);
CREATE INDEX economic_events_currency_impact_idx
    ON economic_events (currency, impact, scheduled_at);

-- Which instruments an economic event plausibly affects. Not every event
-- matters to every instrument: a USD CPI print is material to XAUUSD, a New
-- Zealand trade balance is not.
CREATE TABLE economic_event_instrument_map (
    currency      TEXT NOT NULL,
    instrument_id TEXT NOT NULL REFERENCES instruments (id) ON DELETE CASCADE,
    relevance     NUMERIC(28,10) NOT NULL DEFAULT 1,
    PRIMARY KEY (currency, instrument_id),
    CONSTRAINT economic_event_map_relevance_ck CHECK (relevance > 0 AND relevance <= 1)
);

CREATE TABLE news_items (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    external_id    TEXT NOT NULL,
    source         TEXT NOT NULL,
    headline       TEXT NOT NULL,
    summary        TEXT,
    url            TEXT,
    published_at   TIMESTAMPTZ NOT NULL,
    received_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    related_instruments TEXT[] NOT NULL DEFAULT '{}',
    related_currencies  TEXT[] NOT NULL DEFAULT '{}',
    sentiment      NUMERIC(28,10),
    sentiment_model TEXT,
    topics         TEXT[] NOT NULL DEFAULT '{}',

    CONSTRAINT news_items_sentiment_ck CHECK (
        sentiment IS NULL OR (sentiment >= -1 AND sentiment <= 1)
    )
);

CREATE UNIQUE INDEX news_items_external_uniq ON news_items (source, external_id);
CREATE INDEX news_items_published_idx ON news_items (published_at DESC);
