-- 0001_foundation: identity, sessions, accounts and the ledger.
--
-- Conventions used throughout the schema:
--
--   * Money and quantities are NUMERIC(28,10). Never float. Every monetary
--     column is accompanied by a currency column; an amount without a currency
--     is not representable.
--   * Timestamps are TIMESTAMPTZ and stored in UTC. Source and ingestion times
--     are recorded separately wherever data arrives from outside Vantage.
--   * Mutable financial rows carry a `version` column for optimistic
--     concurrency. Readers that intend to write must pass the version back.
--   * Financial invariants are expressed as database constraints, not merely
--     as application checks, so a bug in one code path cannot corrupt state
--     that another code path relies on.

-- No extensions are required. gen_random_uuid() has been part of core
-- PostgreSQL since 13, so the schema does not depend on pgcrypto and the
-- migration role does not need database-level CREATE privileges.

-- ---------------------------------------------------------------------------
-- Users
-- ---------------------------------------------------------------------------
CREATE TABLE users (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email               TEXT NOT NULL,
    display_name        TEXT NOT NULL,
    role                TEXT NOT NULL,
    password_hash       TEXT NOT NULL,
    mfa_enabled         BOOLEAN NOT NULL DEFAULT FALSE,
    -- TOTP secret, encrypted with the data-encryption keyring. Never plaintext,
    -- and never returned by any API.
    mfa_secret_cipher   BYTEA,
    mfa_key_version     INTEGER,
    disabled            BOOLEAN NOT NULL DEFAULT FALSE,
    failed_login_count  INTEGER NOT NULL DEFAULT 0,
    locked_until        TIMESTAMPTZ,
    last_login_at       TIMESTAMPTZ,
    password_changed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT users_role_ck CHECK (role IN ('admin', 'trader', 'viewer')),
    CONSTRAINT users_email_ck CHECK (email = lower(email) AND position('@' in email) > 1),
    CONSTRAINT users_failed_login_ck CHECK (failed_login_count >= 0),
    -- An MFA secret without a key version cannot be decrypted, so the pair is
    -- required together or not at all.
    CONSTRAINT users_mfa_cipher_ck CHECK (
        (mfa_secret_cipher IS NULL AND mfa_key_version IS NULL)
        OR (mfa_secret_cipher IS NOT NULL AND mfa_key_version IS NOT NULL)
    ),
    CONSTRAINT users_mfa_enabled_ck CHECK (
        mfa_enabled = FALSE OR mfa_secret_cipher IS NOT NULL
    )
);

CREATE UNIQUE INDEX users_email_uniq ON users (email);

-- ---------------------------------------------------------------------------
-- Sessions
--
-- Only a hash of the session token is stored. A database dump therefore does
-- not yield usable sessions, and the plaintext token exists only in the
-- client's HttpOnly cookie.
-- ---------------------------------------------------------------------------
CREATE TABLE sessions (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash     TEXT NOT NULL,
    csrf_token_hash TEXT NOT NULL,
    -- mfa_satisfied distinguishes a half-authenticated session (password
    -- accepted, TOTP still outstanding) from a fully authenticated one. A
    -- half-authenticated session may only complete the MFA challenge.
    mfa_satisfied  BOOLEAN NOT NULL DEFAULT FALSE,
    ip_address     INET,
    user_agent     TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL,
    revoked_at     TIMESTAMPTZ,
    revoked_reason TEXT,

    CONSTRAINT sessions_expiry_ck CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX sessions_token_hash_uniq ON sessions (token_hash);
CREATE INDEX sessions_user_active_idx ON sessions (user_id, expires_at)
    WHERE revoked_at IS NULL;

-- MFA recovery codes are stored as keyed hashes and are single-use.
CREATE TABLE mfa_recovery_codes (
    id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id   UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash TEXT NOT NULL,
    used_at   TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX mfa_recovery_codes_hash_uniq ON mfa_recovery_codes (user_id, code_hash);

-- ---------------------------------------------------------------------------
-- Accounts
--
-- In the non-custodial model an account represents a broker account the USER
-- owns. Vantage holds no funds. In paper mode the balances are simulated, and
-- the mode column travels with every financial row so simulated history can
-- never be mistaken for real history.
-- ---------------------------------------------------------------------------
CREATE TABLE accounts (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    name            TEXT NOT NULL,
    mode            TEXT NOT NULL,
    currency        TEXT NOT NULL,
    broker_name     TEXT NOT NULL,
    broker_account_ref TEXT,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    trading_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    leverage        NUMERIC(28,10) NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    version         BIGINT NOT NULL DEFAULT 1,

    CONSTRAINT accounts_mode_ck CHECK (mode IN ('paper', 'demo', 'live')),
    -- Structural safety property of this build, enforced by the database.
    -- Lifting this constraint is an explicit item on the live-readiness
    -- checklist in docs/REGULATORY_BOUNDARY.md, not a routine migration.
    CONSTRAINT accounts_paper_only_ck CHECK (mode = 'paper'),
    CONSTRAINT accounts_currency_ck CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT accounts_leverage_ck CHECK (leverage > 0 AND leverage <= 500),
    CONSTRAINT accounts_version_ck CHECK (version > 0)
);

CREATE INDEX accounts_user_idx ON accounts (user_id);
CREATE UNIQUE INDEX accounts_user_name_uniq ON accounts (user_id, lower(name));

-- ---------------------------------------------------------------------------
-- Broker connections
--
-- Credential METADATA only. The credential material itself is not stored by
-- this build at all: no live adapter exists, so there is nothing to store. When
-- one is added, secret_cipher holds envelope-encrypted material and is written
-- and read only by the broker adapter layer. There is deliberately no column
-- that any API serialises.
-- ---------------------------------------------------------------------------
CREATE TABLE broker_connections (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    account_id     UUID REFERENCES accounts (id) ON DELETE SET NULL,
    broker_name    TEXT NOT NULL,
    mode           TEXT NOT NULL,
    label          TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'connected',
    -- Metadata a user may safely see: which credential, when it was added,
    -- when it was last used. Never the credential.
    credential_kind      TEXT,
    credential_last_four TEXT,
    credential_key_version INTEGER,
    secret_cipher   BYTEA,
    last_connected_at TIMESTAMPTZ,
    last_error       TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT broker_connections_mode_ck CHECK (mode IN ('paper', 'demo', 'live')),
    CONSTRAINT broker_connections_paper_only_ck CHECK (mode = 'paper'),
    CONSTRAINT broker_connections_status_ck CHECK (
        status IN ('connected', 'disconnected', 'error', 'revoked')
    ),
    -- Only the mock broker is compiled into this build; a row naming any other
    -- broker would describe a connection that cannot exist.
    CONSTRAINT broker_connections_mock_only_ck CHECK (broker_name = 'mock'),
    CONSTRAINT broker_connections_last_four_ck CHECK (
        credential_last_four IS NULL OR length(credential_last_four) <= 4
    )
);

CREATE INDEX broker_connections_user_idx ON broker_connections (user_id);

-- ---------------------------------------------------------------------------
-- Ledger
--
-- Balances are derived, never stored as a mutable running total. Each
-- transaction records the balance that resulted from it, so the ledger can be
-- replayed and any divergence localised. Entries are append-only.
-- ---------------------------------------------------------------------------
CREATE TABLE transactions (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id     UUID NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    sequence       BIGINT NOT NULL,
    type           TEXT NOT NULL,
    amount         NUMERIC(28,10) NOT NULL,
    currency       TEXT NOT NULL,
    balance_after  NUMERIC(28,10) NOT NULL,
    order_id       UUID,
    fill_id        UUID,
    position_id    UUID,
    description    TEXT NOT NULL DEFAULT '',
    mode           TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT transactions_type_ck CHECK (type IN (
        'deposit', 'withdrawal', 'realized_pnl', 'commission', 'fee', 'swap', 'adjustment'
    )),
    CONSTRAINT transactions_currency_ck CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT transactions_mode_ck CHECK (mode = 'paper'),
    CONSTRAINT transactions_sequence_ck CHECK (sequence > 0),
    -- Deposits are positive, withdrawals negative; sign errors here are the
    -- difference between crediting and debiting an account.
    CONSTRAINT transactions_deposit_sign_ck CHECK (type <> 'deposit' OR amount > 0),
    CONSTRAINT transactions_withdrawal_sign_ck CHECK (type <> 'withdrawal' OR amount < 0),
    CONSTRAINT transactions_commission_sign_ck CHECK (type <> 'commission' OR amount <= 0)
);

-- The ledger is a strictly ordered sequence per account. This unique constraint
-- is what makes concurrent double-posting impossible rather than unlikely.
CREATE UNIQUE INDEX transactions_account_sequence_uniq ON transactions (account_id, sequence);
CREATE INDEX transactions_account_created_idx ON transactions (account_id, created_at DESC);
CREATE INDEX transactions_order_idx ON transactions (order_id) WHERE order_id IS NOT NULL;

-- Append-only enforcement. Application roles may INSERT and SELECT; UPDATE and
-- DELETE are rejected by the database itself, so a compromised application
-- cannot quietly rewrite the ledger.
CREATE OR REPLACE FUNCTION vantage_reject_mutation() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'table % is append-only: % is not permitted',
        TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER transactions_append_only
    BEFORE UPDATE OR DELETE ON transactions
    FOR EACH ROW EXECUTE FUNCTION vantage_reject_mutation();

-- ---------------------------------------------------------------------------
-- Equity snapshots
--
-- Peak equity and day-start equity drive the drawdown and daily-loss limits.
-- They are persisted rather than recomputed from the whole ledger on every
-- order, and they are written by the portfolio service inside the same
-- transaction as the fills that move them.
-- ---------------------------------------------------------------------------
CREATE TABLE account_equity_state (
    account_id       UUID PRIMARY KEY REFERENCES accounts (id) ON DELETE CASCADE,
    currency         TEXT NOT NULL,
    peak_equity      NUMERIC(28,10) NOT NULL,
    day_start_equity NUMERIC(28,10) NOT NULL,
    day_start_at     TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    version          BIGINT NOT NULL DEFAULT 1,

    CONSTRAINT account_equity_state_currency_ck CHECK (currency ~ '^[A-Z]{3}$')
);

CREATE TABLE account_equity_history (
    id           BIGSERIAL PRIMARY KEY,
    account_id   UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    equity       NUMERIC(28,10) NOT NULL,
    balance      NUMERIC(28,10) NOT NULL,
    unrealized_pnl NUMERIC(28,10) NOT NULL,
    margin_used  NUMERIC(28,10) NOT NULL,
    currency     TEXT NOT NULL,
    recorded_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX account_equity_history_account_idx
    ON account_equity_history (account_id, recorded_at DESC);
