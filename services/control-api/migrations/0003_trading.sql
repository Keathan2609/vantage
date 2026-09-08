-- 0003_trading: orders, fills, positions, idempotency, outbox, reconciliation.

-- ---------------------------------------------------------------------------
-- Command idempotency registry
--
-- Every financial command (place, cancel, flatten) is registered here BEFORE
-- it is acted on. The primary key is what makes duplicate submission
-- impossible rather than unlikely: two concurrent requests carrying the same
-- key contend on the same row, and exactly one wins.
--
-- Semantics, deliberately strict:
--   same key + same payload      -> return the stored result, do not re-execute
--   same key + different payload -> reject; the client has a bug, and guessing
--                                   which payload was intended could place an
--                                   order nobody asked for
-- ---------------------------------------------------------------------------
CREATE TABLE command_idempotency (
    account_id      UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    idempotency_key TEXT NOT NULL,
    command_type    TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    command_id      UUID NOT NULL,
    actor_user_id   UUID NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    status          TEXT NOT NULL,
    result_order_id UUID,
    result_payload  JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,

    PRIMARY KEY (account_id, idempotency_key),

    CONSTRAINT command_idempotency_status_ck CHECK (
        status IN ('in_progress', 'succeeded', 'failed', 'rejected')
    ),
    CONSTRAINT command_idempotency_type_ck CHECK (
        command_type IN ('place_order', 'cancel_order', 'flatten_position', 'modify_order')
    ),
    CONSTRAINT command_idempotency_key_len_ck CHECK (
        length(idempotency_key) BETWEEN 8 AND 200
    )
);

CREATE INDEX command_idempotency_created_idx ON command_idempotency (created_at);

-- ---------------------------------------------------------------------------
-- Orders
-- ---------------------------------------------------------------------------
CREATE TABLE orders (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id       UUID NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    user_id          UUID NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    instrument_id    TEXT NOT NULL REFERENCES instruments (id) ON DELETE RESTRICT,
    mode             TEXT NOT NULL,
    side             TEXT NOT NULL,
    type             TEXT NOT NULL,
    time_in_force    TEXT NOT NULL,
    status           TEXT NOT NULL,
    quantity         NUMERIC(28,10) NOT NULL,
    filled_quantity  NUMERIC(28,10) NOT NULL DEFAULT 0,
    avg_fill_price   NUMERIC(28,10) NOT NULL DEFAULT 0,
    limit_price      NUMERIC(28,10),
    stop_price       NUMERIC(28,10),
    stop_loss        NUMERIC(28,10),
    take_profit      NUMERIC(28,10),
    source           TEXT NOT NULL,
    strategy_id      UUID,
    strategy_version INTEGER,
    decision_id      UUID,
    command_id       UUID NOT NULL,
    idempotency_key  TEXT NOT NULL,
    broker_name      TEXT NOT NULL,
    broker_order_id  TEXT,
    reject_code      TEXT,
    reject_reason    TEXT,
    version          BIGINT NOT NULL DEFAULT 1,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    submitted_at     TIMESTAMPTZ,
    closed_at        TIMESTAMPTZ,

    CONSTRAINT orders_mode_ck CHECK (mode = 'paper'),
    CONSTRAINT orders_side_ck CHECK (side IN ('buy', 'sell')),
    CONSTRAINT orders_type_ck CHECK (type IN ('market', 'limit', 'stop', 'stop_limit')),
    CONSTRAINT orders_tif_ck CHECK (time_in_force IN ('gtc', 'ioc', 'fok', 'day')),
    CONSTRAINT orders_source_ck CHECK (source IN ('manual', 'strategy', 'autopilot', 'risk_control')),
    CONSTRAINT orders_status_ck CHECK (status IN (
        'CREATED', 'VALIDATING', 'ACCEPTED', 'SUBMITTED', 'PARTIALLY_FILLED',
        'FILLED', 'REJECTED', 'CANCEL_PENDING', 'CANCELLED', 'EXPIRED', 'FAILED'
    )),
    CONSTRAINT orders_quantity_ck CHECK (quantity > 0),
    -- An order can never be filled beyond its size. Without this, a duplicated
    -- fill inflates a position and the ledger silently follows.
    CONSTRAINT orders_filled_bounds_ck CHECK (filled_quantity >= 0 AND filled_quantity <= quantity),
    CONSTRAINT orders_prices_ck CHECK (
        (limit_price IS NULL OR limit_price > 0)
        AND (stop_price IS NULL OR stop_price > 0)
        AND (stop_loss IS NULL OR stop_loss > 0)
        AND (take_profit IS NULL OR take_profit > 0)
    ),
    -- Order types must carry the prices they require.
    CONSTRAINT orders_limit_price_required_ck CHECK (
        type NOT IN ('limit', 'stop_limit') OR limit_price IS NOT NULL
    ),
    CONSTRAINT orders_stop_price_required_ck CHECK (
        type NOT IN ('stop', 'stop_limit') OR stop_price IS NOT NULL
    ),
    CONSTRAINT orders_strategy_pair_ck CHECK (
        (strategy_id IS NULL AND strategy_version IS NULL)
        OR (strategy_id IS NOT NULL AND strategy_version IS NOT NULL)
    ),
    -- A strategy- or autopilot-sourced order must name the strategy that
    -- produced it, so every automated trade is attributable.
    CONSTRAINT orders_automated_attribution_ck CHECK (
        source NOT IN ('strategy', 'autopilot') OR strategy_id IS NOT NULL
    ),
    CONSTRAINT orders_reject_pair_ck CHECK (
        (status <> 'REJECTED') OR (reject_code IS NOT NULL)
    ),
    CONSTRAINT orders_version_ck CHECK (version > 0)
);

-- The database-level guarantee against duplicate orders.
CREATE UNIQUE INDEX orders_account_idempotency_uniq ON orders (account_id, idempotency_key);
CREATE UNIQUE INDEX orders_command_id_uniq ON orders (command_id);
-- A broker order id, once known, identifies exactly one Vantage order.
CREATE UNIQUE INDEX orders_broker_order_uniq ON orders (broker_name, broker_order_id)
    WHERE broker_order_id IS NOT NULL;

CREATE INDEX orders_account_created_idx ON orders (account_id, created_at DESC);
CREATE INDEX orders_open_idx ON orders (account_id, instrument_id)
    WHERE status IN ('CREATED','VALIDATING','ACCEPTED','SUBMITTED','PARTIALLY_FILLED','CANCEL_PENDING');
CREATE INDEX orders_strategy_idx ON orders (strategy_id, created_at DESC)
    WHERE strategy_id IS NOT NULL;

-- Every state change is recorded. The order row carries the current state; this
-- table carries how it got there, including the transitions that failed.
CREATE TABLE order_state_transitions (
    id           BIGSERIAL PRIMARY KEY,
    order_id     UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    from_status  TEXT,
    to_status    TEXT NOT NULL,
    reason       TEXT,
    actor_type   TEXT NOT NULL DEFAULT 'system',
    actor_user_id UUID,
    metadata     JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX order_state_transitions_order_idx
    ON order_state_transitions (order_id, occurred_at);

CREATE TRIGGER order_state_transitions_append_only
    BEFORE UPDATE OR DELETE ON order_state_transitions
    FOR EACH ROW EXECUTE FUNCTION vantage_reject_mutation();

-- ---------------------------------------------------------------------------
-- Fills
--
-- Append-only. The unique index on (broker_name, broker_fill_id) is the
-- defence against processing the same execution twice — which is a real
-- scenario whenever a broker stream reconnects and replays.
-- ---------------------------------------------------------------------------
CREATE TABLE fills (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id       UUID NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
    account_id     UUID NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    instrument_id  TEXT NOT NULL REFERENCES instruments (id) ON DELETE RESTRICT,
    side           TEXT NOT NULL,
    quantity       NUMERIC(28,10) NOT NULL,
    price          NUMERIC(28,10) NOT NULL,
    commission     NUMERIC(28,10) NOT NULL DEFAULT 0,
    commission_currency TEXT NOT NULL,
    slippage       NUMERIC(28,10) NOT NULL DEFAULT 0,
    liquidity      TEXT NOT NULL DEFAULT 'unknown',
    broker_name    TEXT NOT NULL,
    broker_fill_id TEXT NOT NULL,
    mode           TEXT NOT NULL,
    executed_at    TIMESTAMPTZ NOT NULL,
    recorded_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT fills_side_ck CHECK (side IN ('buy', 'sell')),
    CONSTRAINT fills_quantity_ck CHECK (quantity > 0),
    CONSTRAINT fills_price_ck CHECK (price > 0),
    CONSTRAINT fills_commission_ck CHECK (commission >= 0),
    CONSTRAINT fills_mode_ck CHECK (mode = 'paper'),
    CONSTRAINT fills_currency_ck CHECK (commission_currency ~ '^[A-Z]{3}$')
);

CREATE UNIQUE INDEX fills_broker_fill_uniq ON fills (broker_name, broker_fill_id);
CREATE INDEX fills_order_idx ON fills (order_id, executed_at);
CREATE INDEX fills_account_idx ON fills (account_id, executed_at DESC);

CREATE TRIGGER fills_append_only
    BEFORE UPDATE OR DELETE ON fills
    FOR EACH ROW EXECUTE FUNCTION vantage_reject_mutation();

-- ---------------------------------------------------------------------------
-- Positions
--
-- Netting model: at most one OPEN position per (account, instrument). The
-- partial unique index enforces it, so a race between two fill handlers cannot
-- produce two open positions that each believe they hold the exposure.
-- ---------------------------------------------------------------------------
CREATE TABLE positions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id      UUID NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    instrument_id   TEXT NOT NULL REFERENCES instruments (id) ON DELETE RESTRICT,
    mode            TEXT NOT NULL,
    side            TEXT NOT NULL,
    quantity        NUMERIC(28,10) NOT NULL,
    avg_entry_price NUMERIC(28,10) NOT NULL,
    status          TEXT NOT NULL,
    stop_loss       NUMERIC(28,10),
    take_profit     NUMERIC(28,10),
    realized_pnl    NUMERIC(28,10) NOT NULL DEFAULT 0,
    commission      NUMERIC(28,10) NOT NULL DEFAULT 0,
    swap            NUMERIC(28,10) NOT NULL DEFAULT 0,
    currency        TEXT NOT NULL,
    strategy_id     UUID,
    version         BIGINT NOT NULL DEFAULT 1,
    opened_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at       TIMESTAMPTZ,

    CONSTRAINT positions_mode_ck CHECK (mode = 'paper'),
    CONSTRAINT positions_side_ck CHECK (side IN ('buy', 'sell')),
    CONSTRAINT positions_status_ck CHECK (status IN ('open', 'closed')),
    CONSTRAINT positions_currency_ck CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT positions_entry_price_ck CHECK (avg_entry_price > 0),
    -- An open position holds size; a closed one holds none. This pair of rules
    -- prevents the "ghost position" state that quietly breaks exposure maths.
    CONSTRAINT positions_open_quantity_ck CHECK (status <> 'open' OR quantity > 0),
    CONSTRAINT positions_closed_quantity_ck CHECK (status <> 'closed' OR quantity = 0),
    CONSTRAINT positions_closed_at_ck CHECK (status <> 'closed' OR closed_at IS NOT NULL),
    CONSTRAINT positions_version_ck CHECK (version > 0)
);

CREATE UNIQUE INDEX positions_one_open_per_instrument_uniq
    ON positions (account_id, instrument_id)
    WHERE status = 'open';

CREATE INDEX positions_account_idx ON positions (account_id, status);
CREATE INDEX positions_strategy_idx ON positions (strategy_id) WHERE strategy_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Transactional outbox
--
-- Side effects (notifications, metrics fan-out, downstream events) are written
-- in the same transaction as the state change that caused them, then published
-- asynchronously. This is what makes "at-least-once delivery with idempotent
-- processing" true rather than aspirational: no event can be lost because a
-- process died between committing a fill and emitting its notification.
-- ---------------------------------------------------------------------------
CREATE TABLE outbox (
    id             BIGSERIAL PRIMARY KEY,
    aggregate_type TEXT NOT NULL,
    aggregate_id   TEXT NOT NULL,
    event_type     TEXT NOT NULL,
    payload        JSONB NOT NULL,
    correlation_id TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    available_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at   TIMESTAMPTZ,
    attempts       INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT,

    CONSTRAINT outbox_attempts_ck CHECK (attempts >= 0)
);

-- Partial index over the pending set only: the published rows, which dominate
-- the table over time, are not indexed for the dispatcher's hot query.
CREATE INDEX outbox_pending_idx ON outbox (available_at, id)
    WHERE published_at IS NULL;

-- ---------------------------------------------------------------------------
-- Reconciliation
--
-- Vantage never assumes its own state matches the broker's. Every run records
-- what it compared and what it found; unresolved discrepancies are expected to
-- halt automated trading rather than be papered over.
-- ---------------------------------------------------------------------------
CREATE TABLE reconciliation_runs (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id     UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    broker_name    TEXT NOT NULL,
    trigger        TEXT NOT NULL,
    status         TEXT NOT NULL,
    orders_compared    INTEGER NOT NULL DEFAULT 0,
    positions_compared INTEGER NOT NULL DEFAULT 0,
    discrepancies      INTEGER NOT NULL DEFAULT 0,
    started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at    TIMESTAMPTZ,
    error          TEXT,

    CONSTRAINT reconciliation_runs_trigger_ck CHECK (
        trigger IN ('startup', 'scheduled', 'manual', 'reconnect', 'post_failure')
    ),
    CONSTRAINT reconciliation_runs_status_ck CHECK (
        status IN ('running', 'clean', 'discrepancies_found', 'failed')
    )
);

CREATE INDEX reconciliation_runs_account_idx
    ON reconciliation_runs (account_id, started_at DESC);

CREATE TABLE reconciliation_discrepancies (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id        UUID NOT NULL REFERENCES reconciliation_runs (id) ON DELETE CASCADE,
    account_id    UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    kind          TEXT NOT NULL,
    severity      TEXT NOT NULL,
    order_id      UUID,
    position_id   UUID,
    instrument_id TEXT,
    vantage_value TEXT,
    broker_value  TEXT,
    description   TEXT NOT NULL,
    resolved_at   TIMESTAMPTZ,
    resolution    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT reconciliation_discrepancies_kind_ck CHECK (kind IN (
        'order_missing_at_broker', 'order_unknown_to_vantage',
        'order_status_mismatch', 'fill_quantity_mismatch',
        'position_missing_at_broker', 'position_unknown_to_vantage',
        'position_quantity_mismatch', 'balance_mismatch'
    )),
    CONSTRAINT reconciliation_discrepancies_severity_ck CHECK (
        severity IN ('info', 'warning', 'critical')
    )
);

CREATE INDEX reconciliation_discrepancies_unresolved_idx
    ON reconciliation_discrepancies (account_id, created_at DESC)
    WHERE resolved_at IS NULL;
