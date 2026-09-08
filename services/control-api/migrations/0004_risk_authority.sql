-- 0004_risk_authority: risk limits, risk events, trading authority, kill switches.

-- ---------------------------------------------------------------------------
-- Risk limits
--
-- One row per account. These are ceilings the risk engine enforces; strategy
-- and model code can neither read them for the purpose of widening them nor
-- write them at all. Changing a limit is an authenticated user action that
-- produces an audit event.
-- ---------------------------------------------------------------------------
CREATE TABLE risk_limits (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    currency   TEXT NOT NULL,

    max_order_quantity          NUMERIC(28,10) NOT NULL,
    max_order_notional          NUMERIC(28,10) NOT NULL,
    max_risk_per_trade_fraction NUMERIC(28,10) NOT NULL,
    require_stop_loss           BOOLEAN NOT NULL DEFAULT TRUE,

    max_open_positions          INTEGER NOT NULL,
    max_pending_orders          INTEGER NOT NULL,
    max_gross_exposure          NUMERIC(28,10) NOT NULL,
    max_net_exposure            NUMERIC(28,10) NOT NULL,
    max_per_instrument_exposure NUMERIC(28,10) NOT NULL,
    max_concentration_fraction  NUMERIC(28,10) NOT NULL,
    max_leverage                NUMERIC(28,10) NOT NULL,

    max_daily_loss              NUMERIC(28,10) NOT NULL,
    max_drawdown_fraction       NUMERIC(28,10) NOT NULL,

    max_spread_fraction         NUMERIC(28,10) NOT NULL,
    max_slippage_fraction       NUMERIC(28,10) NOT NULL,

    event_blackout_before_minutes INTEGER NOT NULL DEFAULT 15,
    event_blackout_after_minutes  INTEGER NOT NULL DEFAULT 10,
    block_on_high_impact_events   BOOLEAN NOT NULL DEFAULT TRUE,

    updated_by UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    version    BIGINT NOT NULL DEFAULT 1,

    CONSTRAINT risk_limits_currency_ck CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT risk_limits_positive_ck CHECK (
        max_order_quantity > 0
        AND max_order_notional > 0
        AND max_gross_exposure > 0
        AND max_net_exposure > 0
        AND max_per_instrument_exposure > 0
        AND max_daily_loss > 0
        AND max_leverage > 0
    ),
    CONSTRAINT risk_limits_counts_ck CHECK (
        max_open_positions >= 0 AND max_pending_orders >= 0
    ),
    -- Fractions are fractions. A "max risk per trade" of 5 would mean 500% of
    -- equity per trade, which is the kind of typo that empties an account.
    CONSTRAINT risk_limits_fractions_ck CHECK (
        max_risk_per_trade_fraction > 0 AND max_risk_per_trade_fraction <= 1
        AND max_drawdown_fraction > 0 AND max_drawdown_fraction <= 1
        AND max_concentration_fraction > 0 AND max_concentration_fraction <= 1
        AND max_spread_fraction > 0 AND max_spread_fraction <= 1
        AND max_slippage_fraction > 0 AND max_slippage_fraction <= 1
    ),
    -- An upper bound on risk per trade that the user cannot exceed even
    -- deliberately. 10% of equity on one trade is already aggressive; beyond
    -- that the platform declines to participate.
    CONSTRAINT risk_limits_risk_ceiling_ck CHECK (max_risk_per_trade_fraction <= 0.10),
    CONSTRAINT risk_limits_blackout_ck CHECK (
        event_blackout_before_minutes BETWEEN 0 AND 1440
        AND event_blackout_after_minutes BETWEEN 0 AND 1440
    ),
    CONSTRAINT risk_limits_version_ck CHECK (version > 0)
);

CREATE UNIQUE INDEX risk_limits_account_uniq ON risk_limits (account_id);

-- Append-only history of limit changes: who widened what, and when.
CREATE TABLE risk_limit_history (
    id         BIGSERIAL PRIMARY KEY,
    account_id UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    changed_by UUID REFERENCES users (id) ON DELETE SET NULL,
    before     JSONB NOT NULL,
    after      JSONB NOT NULL,
    reason     TEXT,
    changed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX risk_limit_history_account_idx ON risk_limit_history (account_id, changed_at DESC);

CREATE TRIGGER risk_limit_history_append_only
    BEFORE UPDATE OR DELETE ON risk_limit_history
    FOR EACH ROW EXECUTE FUNCTION vantage_reject_mutation();

-- ---------------------------------------------------------------------------
-- Risk events
-- ---------------------------------------------------------------------------
CREATE TABLE risk_events (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id    UUID REFERENCES accounts (id) ON DELETE CASCADE,
    user_id       UUID REFERENCES users (id) ON DELETE SET NULL,
    severity      TEXT NOT NULL,
    check_name    TEXT NOT NULL,
    code          TEXT NOT NULL,
    message       TEXT NOT NULL,
    instrument_id TEXT,
    strategy_id   UUID,
    order_id      UUID,
    detail        JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT risk_events_severity_ck CHECK (severity IN ('info', 'warning', 'critical'))
);

CREATE INDEX risk_events_account_idx ON risk_events (account_id, created_at DESC);
CREATE INDEX risk_events_code_idx ON risk_events (code, created_at DESC);

-- ---------------------------------------------------------------------------
-- Trading authority
--
-- The scoped, revocable permission under which Vantage may act on an account.
-- It is a control mechanism, NOT a legal instrument: see
-- docs/REGULATORY_BOUNDARY.md. Allow-lists are enumerations, so anything not
-- listed is denied.
-- ---------------------------------------------------------------------------
CREATE TABLE trading_authorities (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    account_id UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    mode       TEXT NOT NULL,

    active             BOOLEAN NOT NULL DEFAULT TRUE,
    automation_enabled BOOLEAN NOT NULL DEFAULT FALSE,

    allowed_instruments TEXT[] NOT NULL DEFAULT '{}',
    allowed_strategy_ids UUID[] NOT NULL DEFAULT '{}',
    allowed_order_types  TEXT[] NOT NULL DEFAULT '{}',

    max_order_quantity    NUMERIC(28,10) NOT NULL,
    max_order_notional    NUMERIC(28,10) NOT NULL,
    max_position_exposure NUMERIC(28,10) NOT NULL,
    max_leverage          NUMERIC(28,10) NOT NULL,
    max_daily_loss        NUMERIC(28,10) NOT NULL,
    currency              TEXT NOT NULL,

    valid_from  TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_until TIMESTAMPTZ,

    created_by  UUID NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ,
    revocation_reason TEXT,
    version     BIGINT NOT NULL DEFAULT 1,

    CONSTRAINT trading_authorities_mode_ck CHECK (mode = 'paper'),
    CONSTRAINT trading_authorities_currency_ck CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT trading_authorities_limits_ck CHECK (
        max_order_quantity > 0
        AND max_order_notional > 0
        AND max_position_exposure > 0
        AND max_leverage > 0
        AND max_daily_loss > 0
    ),
    CONSTRAINT trading_authorities_validity_ck CHECK (
        valid_until IS NULL OR valid_until > valid_from
    ),
    CONSTRAINT trading_authorities_revocation_ck CHECK (
        revoked_at IS NULL OR revocation_reason IS NOT NULL
    ),
    CONSTRAINT trading_authorities_version_ck CHECK (version > 0)
);

-- At most one live authority per account: overlapping grants would make "what
-- is Vantage allowed to do here?" ambiguous at exactly the wrong moment.
CREATE UNIQUE INDEX trading_authorities_one_active_uniq
    ON trading_authorities (account_id)
    WHERE revoked_at IS NULL AND active = TRUE;

CREATE INDEX trading_authorities_user_idx ON trading_authorities (user_id);

CREATE TABLE trading_authority_history (
    id           BIGSERIAL PRIMARY KEY,
    authority_id UUID NOT NULL,
    account_id   UUID NOT NULL,
    changed_by   UUID REFERENCES users (id) ON DELETE SET NULL,
    action       TEXT NOT NULL,
    before       JSONB,
    after        JSONB,
    reason       TEXT,
    changed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT trading_authority_history_action_ck CHECK (
        action IN ('created', 'updated', 'revoked', 'reactivated')
    )
);

CREATE INDEX trading_authority_history_account_idx
    ON trading_authority_history (account_id, changed_at DESC);

CREATE TRIGGER trading_authority_history_append_only
    BEFORE UPDATE OR DELETE ON trading_authority_history
    FOR EACH ROW EXECUTE FUNCTION vantage_reject_mutation();

-- ---------------------------------------------------------------------------
-- Kill switches
--
-- A kill switch stops NEW orders within its scope. It does not liquidate:
-- closing positions is the separate, explicitly authorised Flatten operation,
-- because automatic liquidation on an alarm would dump positions into exactly
-- the conditions that raised the alarm.
-- ---------------------------------------------------------------------------
CREATE TABLE kill_switches (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope          TEXT NOT NULL,
    target_id      TEXT,
    active         BOOLEAN NOT NULL DEFAULT TRUE,
    reason         TEXT NOT NULL,
    activated_by   UUID REFERENCES users (id) ON DELETE SET NULL,
    activated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    deactivated_by UUID REFERENCES users (id) ON DELETE SET NULL,
    deactivated_at TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT kill_switches_scope_ck CHECK (
        scope IN ('global', 'user', 'account', 'broker', 'strategy')
    ),
    -- A global switch has no target; every other scope must name one.
    CONSTRAINT kill_switches_target_ck CHECK (
        (scope = 'global' AND target_id IS NULL)
        OR (scope <> 'global' AND target_id IS NOT NULL)
    ),
    CONSTRAINT kill_switches_deactivation_ck CHECK (
        active = TRUE OR deactivated_at IS NOT NULL
    )
);

-- One active switch per scope+target, so activation is idempotent and the
-- "is trading halted here?" query is a single indexed lookup.
CREATE UNIQUE INDEX kill_switches_active_global_uniq
    ON kill_switches ((TRUE))
    WHERE active = TRUE AND scope = 'global';

CREATE UNIQUE INDEX kill_switches_active_target_uniq
    ON kill_switches (scope, target_id)
    WHERE active = TRUE AND scope <> 'global';

CREATE INDEX kill_switches_active_idx ON kill_switches (scope, target_id) WHERE active = TRUE;

-- ---------------------------------------------------------------------------
-- Notifications
-- ---------------------------------------------------------------------------
CREATE TABLE notifications (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    account_id UUID REFERENCES accounts (id) ON DELETE CASCADE,
    severity   TEXT NOT NULL,
    category   TEXT NOT NULL,
    title      TEXT NOT NULL,
    body       TEXT NOT NULL,
    metadata   JSONB NOT NULL DEFAULT '{}'::jsonb,
    read_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT notifications_severity_ck CHECK (severity IN ('info', 'warning', 'critical')),
    CONSTRAINT notifications_category_ck CHECK (category IN (
        'trading', 'risk', 'security', 'system', 'data', 'strategy', 'model', 'reconciliation'
    ))
);

CREATE INDEX notifications_user_unread_idx ON notifications (user_id, created_at DESC)
    WHERE read_at IS NULL;
