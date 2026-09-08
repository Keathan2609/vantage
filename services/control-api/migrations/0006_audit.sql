-- 0006_audit: the append-only, hash-chained audit log.
--
-- Tamper EVIDENCE, not immutability. A party with write access to the database
-- can still rewrite history, but they must rewrite every subsequent row to keep
-- the chain consistent. A verifier holding a previously observed head hash — or
-- one shipped off-box — detects the divergence. See docs/SECURITY.md.

CREATE TABLE audit_events (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sequence       BIGINT NOT NULL,
    occurred_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor_user_id  UUID REFERENCES users (id) ON DELETE SET NULL,
    actor_type     TEXT NOT NULL,
    action         TEXT NOT NULL,
    target_type    TEXT NOT NULL,
    target_id      TEXT,
    account_id     UUID REFERENCES accounts (id) ON DELETE SET NULL,
    result         TEXT NOT NULL,
    request_id     TEXT NOT NULL DEFAULT '',
    correlation_id TEXT NOT NULL DEFAULT '',
    ip_address     INET,
    user_agent     TEXT,
    -- Metadata is non-secret context only. Credentials, session tokens, MFA
    -- secrets and encryption keys must never appear here; the audit log is one
    -- of the most widely read tables in an incident.
    metadata       JSONB NOT NULL DEFAULT '{}'::jsonb,
    prev_hash      TEXT NOT NULL,
    hash           TEXT NOT NULL,

    CONSTRAINT audit_events_actor_type_ck CHECK (
        actor_type IN ('user', 'system', 'strategy', 'admin', 'anonymous')
    ),
    CONSTRAINT audit_events_result_ck CHECK (result IN ('success', 'failure', 'blocked')),
    CONSTRAINT audit_events_sequence_ck CHECK (sequence > 0),
    CONSTRAINT audit_events_hash_ck CHECK (
        length(hash) = 64 AND length(prev_hash) = 64
    )
);

-- The chain is a total order. A gap or a repeat is itself evidence.
CREATE UNIQUE INDEX audit_events_sequence_uniq ON audit_events (sequence);
CREATE UNIQUE INDEX audit_events_hash_uniq ON audit_events (hash);
CREATE INDEX audit_events_occurred_idx ON audit_events (occurred_at DESC);
CREATE INDEX audit_events_actor_idx ON audit_events (actor_user_id, occurred_at DESC);
CREATE INDEX audit_events_action_idx ON audit_events (action, occurred_at DESC);
CREATE INDEX audit_events_account_idx ON audit_events (account_id, occurred_at DESC);

CREATE TRIGGER audit_events_append_only
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION vantage_reject_mutation();

-- Sequence allocation for the audit chain. A dedicated sequence object would
-- allow gaps on rollback, which would be indistinguishable from deletion, so
-- the next value is derived from the table under an advisory lock held by the
-- writer instead.
CREATE TABLE audit_chain_state (
    id         BOOLEAN PRIMARY KEY DEFAULT TRUE,
    head_hash  TEXT NOT NULL,
    head_sequence BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT audit_chain_state_singleton_ck CHECK (id = TRUE),
    CONSTRAINT audit_chain_state_sequence_ck CHECK (head_sequence >= 0)
);

INSERT INTO audit_chain_state (id, head_hash, head_sequence)
VALUES (TRUE, repeat('0', 64), 0);

-- ---------------------------------------------------------------------------
-- Rate limiting
--
-- Redis carries rate-limit state in normal operation. This table is the
-- durable fallback so that losing Redis degrades throughput rather than
-- removing the protection entirely: the login endpoint must stay rate-limited
-- even when the cache is down.
-- ---------------------------------------------------------------------------
CREATE TABLE rate_limit_buckets (
    bucket_key   TEXT PRIMARY KEY,
    tokens       NUMERIC(28,10) NOT NULL,
    last_refill  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL
);

CREATE INDEX rate_limit_buckets_expiry_idx ON rate_limit_buckets (expires_at);

-- ---------------------------------------------------------------------------
-- Scheduler leases
--
-- Multiple control-plane instances must not run the same strategy evaluation
-- concurrently. A lease is taken before a scheduled run and released after;
-- expiry bounds the damage if a holder dies.
-- ---------------------------------------------------------------------------
CREATE TABLE scheduler_leases (
    lease_key   TEXT PRIMARY KEY,
    holder      TEXT NOT NULL,
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,

    CONSTRAINT scheduler_leases_expiry_ck CHECK (expires_at > acquired_at)
);
