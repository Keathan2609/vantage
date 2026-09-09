-- Replay runs: the durable record of every market replay this installation ran.
--
-- # Why a table and not just the API response
--
-- A replay result is only evidence if it can be tied to exactly what produced
-- it, and "the same run twice" is only a claim until two records can be put
-- side by side. Until this table existed the run record lived in memory and
-- died with the process, so comparing two runs meant capturing an HTTP
-- response by hand and trusting that nothing had changed between them. That is
-- not a reproducibility guarantee; it is a habit.
--
-- The four identity columns are the whole point. dataset_hash catches an
-- edited fixture, code_sha catches a changed strategy or risk rule,
-- config_hash catches a moved threshold, and seed catches deliberate
-- randomness. A run missing any of them cannot support a statement about
-- determinism.
--
-- # Why `simulated` is a column that can only be true
--
-- Every number a replay produces is simulated, and the one failure mode worth
-- designing against is a replay result being read later as a live one. The
-- CHECK makes that unrepresentable rather than merely unlikely, in the same
-- way the execution-mode constraints do elsewhere in the schema. A future
-- writer who needs `simulated = false` has to change the schema, which is a
-- conversation rather than an accident.
--
-- Idempotent: CI applies migrations twice.

CREATE TABLE IF NOT EXISTS replay_runs (
    id              uuid        PRIMARY KEY,

    -- Identity of what ran.
    dataset_id      text        NOT NULL,
    dataset_hash    text        NOT NULL,
    code_sha        text        NOT NULL,
    config_hash     text        NOT NULL,
    seed            bigint      NOT NULL,

    -- from_time and to_time are DATASET time, not wall time. A run that
    -- covered three replay days in nine seconds is described by the days.
    from_time       timestamptz NOT NULL,
    to_time         timestamptz NOT NULL,

    -- started_at and finished_at are wall time, because "how long did this
    -- take on this machine" is an operational question, not a market one.
    started_at      timestamptz NOT NULL,
    finished_at     timestamptz,

    state           text        NOT NULL,
    steps           integer     NOT NULL DEFAULT 0,
    bars_processed  integer     NOT NULL DEFAULT 0,
    step_errors     integer     NOT NULL DEFAULT 0,
    -- Empty rather than NULL: a run that failed and a run that has not
    -- finished are distinguished by state, and a nullable text column invites
    -- a caller to conflate them.
    failure         text        NOT NULL DEFAULT '',
    -- Conditions that were known at Start to prevent this run from trading --
    -- an expired trading authority, autopilot off. Recorded because a run
    -- with zero orders and a run that was never allowed to place one look
    -- identical afterwards, and that difference is the whole question.
    warnings        text[]      NOT NULL DEFAULT '{}',

    simulated       boolean     NOT NULL DEFAULT true,

    CONSTRAINT replay_runs_state_ck CHECK (
        state IN ('idle', 'running', 'paused', 'stopped', 'done', 'failed')),
    CONSTRAINT replay_runs_span_ck CHECK (to_time >= from_time),
    -- A 64-character hex digest. A truncated or placeholder hash would make
    -- the identity columns decorative.
    CONSTRAINT replay_runs_dataset_hash_ck CHECK (dataset_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT replay_runs_dataset_id_ck CHECK (length(btrim(dataset_id)) > 0),
    CONSTRAINT replay_runs_counters_ck CHECK (
        steps >= 0 AND bars_processed >= 0 AND step_errors >= 0),
    CONSTRAINT replay_runs_finish_ck CHECK (
        finished_at IS NULL OR finished_at >= started_at),
    -- Never false. See the header.
    CONSTRAINT replay_runs_simulated_ck CHECK (simulated)
);

-- Newest first is the only listing anybody wants.
CREATE INDEX IF NOT EXISTS replay_runs_started_idx
    ON replay_runs (started_at DESC);

-- "Every run of this dataset" is the comparison query, so it gets an index.
CREATE INDEX IF NOT EXISTS replay_runs_dataset_idx
    ON replay_runs (dataset_id, started_at DESC);

-- Insert and update, never delete.
--
-- The row is mutable by design -- a run's state and counters move while it
-- plays -- so this is not one of the append-only tables. But a recorded run is
-- evidence, and the control plane has no legitimate reason to be able to make
-- one disappear. Removing a run is the owner's decision, taken deliberately,
-- not something an application bug can do.
GRANT SELECT, INSERT, UPDATE ON replay_runs TO vantage_app;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'vantage_app') THEN
        -- 0007 grants DELETE on every table by default, including future ones.
        EXECUTE 'REVOKE DELETE ON replay_runs FROM vantage_app';
    END IF;
END $$;
