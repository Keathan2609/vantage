-- 0010_reconciliation: an explicit issue taxonomy, repair provenance, and the
-- durable state that decides whether automation may run.
--
-- Why this replaces reconciliation_discrepancies
--
-- The previous table recorded a mismatch as a free-text kind plus two opaque
-- value strings. That was enough to DETECT divergence and halt trading, which
-- is what the earlier audit proved it does correctly. It is not enough to
-- REPAIR divergence, because a repair needs to know three things the old shape
-- could not express:
--
--   * what class of problem this is, precisely, so a rule can decide whether
--     it is safe to fix automatically -- "fill_quantity_mismatch" conflates a
--     provable missing execution with an unattributable one
--   * the evidence, retained, so an operator reviewing the decision months
--     later can see what the venue actually said rather than a summary of it
--   * the resolution history, because a repair is a financial action and
--     "who changed this, when, and why" is not optional
--
-- Rows are copied across rather than discarded, then the old table is dropped.
-- Two tables describing one concept is worse than one: the next person cannot
-- tell which is authoritative, and a reader who queries the stale one draws a
-- conclusion from data nothing updates.

-- ---------------------------------------------------------------------------
-- Issues
-- ---------------------------------------------------------------------------

CREATE TABLE reconciliation_issues (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id        UUID NOT NULL REFERENCES reconciliation_runs (id) ON DELETE CASCADE,
    account_id    UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    broker_name   TEXT NOT NULL,

    issue_type    TEXT NOT NULL,
    severity      TEXT NOT NULL,
    status        TEXT NOT NULL,
    repair_class  TEXT NOT NULL,

    -- What the issue is about. All optional because the taxonomy spans order,
    -- position, execution and balance problems, and forcing a single subject
    -- column would mean encoding the type into a string again.
    order_id            UUID REFERENCES orders (id) ON DELETE SET NULL,
    position_id         UUID,
    instrument_id       TEXT,
    broker_order_id     TEXT,
    broker_execution_id TEXT,

    -- The evidence. `json`, not `jsonb`, for the same reason audit metadata is:
    -- jsonb re-serialises, so a hash or a byte-for-byte comparison of what the
    -- venue said would not survive a round trip.
    local_snapshot  JSON NOT NULL DEFAULT '{}',
    broker_snapshot JSON NOT NULL DEFAULT '{}',
    evidence        JSON NOT NULL DEFAULT '{}',
    description     TEXT NOT NULL,

    -- fingerprint identifies the same real-world problem across runs, so a
    -- reconciliation loop every 60 seconds reports one open issue rather than
    -- a thousand. It is computed by the application from the type and subject.
    fingerprint     TEXT NOT NULL,

    correlation_id  TEXT NOT NULL DEFAULT '',
    detected_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_checked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    check_count     INTEGER NOT NULL DEFAULT 1,

    resolved_at        TIMESTAMPTZ,
    resolution_action  TEXT,
    resolution_reason  TEXT,
    resolved_by        UUID REFERENCES users (id) ON DELETE SET NULL,

    CONSTRAINT reconciliation_issues_type_ck CHECK (issue_type IN (
        'ORDER_MISSING_LOCALLY',
        'ORDER_MISSING_AT_BROKER',
        'FILL_MISSING_LOCALLY',
        'EXTRA_BROKER_FILL',
        'PARTIAL_FILL_MISMATCH',
        'ORDER_STATUS_MISMATCH',
        'POSITION_MISMATCH',
        'BALANCE_MISMATCH',
        'VENUE_ID_MISMATCH',
        'UNKNOWN_EXECUTION_STATE',
        'DUPLICATE_EXECUTION_REPORT',
        'OUT_OF_ORDER_EXECUTION_REPORT',
        'EXTERNAL_BROKER_ACTIVITY'
    )),
    CONSTRAINT reconciliation_issues_severity_ck CHECK (
        severity IN ('info', 'warning', 'critical')
    ),
    CONSTRAINT reconciliation_issues_status_ck CHECK (status IN (
        'OPEN',
        'AUTOMATICALLY_REPAIRED',
        'OPERATOR_ACTION_REQUIRED',
        'RESOLVED',
        'UNRESOLVABLE'
    )),
    CONSTRAINT reconciliation_issues_repair_class_ck CHECK (repair_class IN (
        'AUTOMATICALLY_SAFE',
        'OPERATOR_REVIEW_REQUIRED',
        'UNRESOLVABLE_AUTOMATICALLY'
    )),
    -- A resolved issue must say how and why it was resolved. Without this a
    -- row can be closed with no explanation, which is exactly the record an
    -- operator needs when the same divergence reappears.
    CONSTRAINT reconciliation_issues_resolution_ck CHECK (
        (resolved_at IS NULL AND resolution_action IS NULL)
        OR (resolved_at IS NOT NULL AND resolution_action IS NOT NULL
            AND resolution_reason IS NOT NULL AND length(resolution_reason) > 0)
    ),
    -- A terminal status must be resolved, and an open one must not be.
    CONSTRAINT reconciliation_issues_status_pair_ck CHECK (
        (status IN ('OPEN', 'OPERATOR_ACTION_REQUIRED') AND resolved_at IS NULL)
        OR (status IN ('AUTOMATICALLY_REPAIRED', 'RESOLVED', 'UNRESOLVABLE')
            AND resolved_at IS NOT NULL)
    )
);

-- One OPEN issue per real problem. Partial, so the history of previously
-- resolved occurrences is retained rather than overwritten -- a divergence
-- that keeps coming back is itself a finding.
CREATE UNIQUE INDEX reconciliation_issues_open_fingerprint_uniq
    ON reconciliation_issues (account_id, fingerprint)
    WHERE resolved_at IS NULL;

CREATE INDEX reconciliation_issues_unresolved_idx
    ON reconciliation_issues (account_id, severity, detected_at DESC)
    WHERE resolved_at IS NULL;

CREATE INDEX reconciliation_issues_order_idx
    ON reconciliation_issues (order_id) WHERE order_id IS NOT NULL;

CREATE INDEX reconciliation_issues_run_idx
    ON reconciliation_issues (run_id);

-- ---------------------------------------------------------------------------
-- Issue history
--
-- Append-only. Every automatic repair and every operator action lands here, so
-- the sequence of what was decided about a divergence is reconstructable
-- without inferring it from the issue's current state.
-- ---------------------------------------------------------------------------

CREATE TABLE reconciliation_issue_events (
    id            BIGSERIAL PRIMARY KEY,
    issue_id      UUID NOT NULL REFERENCES reconciliation_issues (id) ON DELETE CASCADE,
    action        TEXT NOT NULL,
    from_status   TEXT,
    to_status     TEXT NOT NULL,
    actor_type    TEXT NOT NULL,
    actor_user_id UUID REFERENCES users (id) ON DELETE SET NULL,
    reason        TEXT NOT NULL,
    evidence      JSON NOT NULL DEFAULT '{}',
    correlation_id TEXT NOT NULL DEFAULT '',
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT reconciliation_issue_events_actor_ck CHECK (
        actor_type IN ('system', 'user')
    ),
    -- A system action needs no user; a user action must name one. This is the
    -- difference between "reconciliation repaired this" and "an operator
    -- decided this", and it must not be ambiguous.
    CONSTRAINT reconciliation_issue_events_actor_pair_ck CHECK (
        (actor_type = 'system' AND actor_user_id IS NULL)
        OR (actor_type = 'user' AND actor_user_id IS NOT NULL)
    ),
    CONSTRAINT reconciliation_issue_events_reason_ck CHECK (length(reason) > 0)
);

CREATE INDEX reconciliation_issue_events_issue_idx
    ON reconciliation_issue_events (issue_id, occurred_at);

CREATE TRIGGER reconciliation_issue_events_append_only
    BEFORE UPDATE OR DELETE ON reconciliation_issue_events
    FOR EACH ROW EXECUTE FUNCTION vantage_reject_mutation();

-- ---------------------------------------------------------------------------
-- Per-account reconciliation state
--
-- Derived state is normally better than stored state, and the trading verdict
-- IS derived from open issues so it cannot drift. What is stored here is the
-- scheduling and coverage information that cannot be derived: when a run last
-- succeeded, and whether the account has ever been reconciled at all.
--
-- The distinction matters for start-up. "No open issues" is true of an account
-- that has never been checked, and that is not the same as "safe".
-- ---------------------------------------------------------------------------

CREATE TABLE reconciliation_account_state (
    account_id       UUID PRIMARY KEY REFERENCES accounts (id) ON DELETE CASCADE,
    broker_name      TEXT NOT NULL,
    last_run_id      UUID REFERENCES reconciliation_runs (id) ON DELETE SET NULL,
    last_started_at  TIMESTAMPTZ,
    last_success_at  TIMESTAMPTZ,
    last_status      TEXT,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    -- The execution cursor for PollExecutions. Resuming from here after a
    -- crash is what makes a pull model replayable instead of lossy.
    executions_cursor TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT reconciliation_account_state_failures_ck CHECK (consecutive_failures >= 0)
);

-- ---------------------------------------------------------------------------
-- Explicit unknown-execution state on the order itself
--
-- FAILED already means "Vantage does not know the venue-side outcome", and
-- that is documented. But the name reads as a closed, terminal failure, and
-- the earlier audit produced orders that were genuinely uncertain while
-- looking merely broken.
--
-- This column makes the uncertainty a first-class, queryable property rather
-- than something inferred from a status name. It is deliberately NOT a new
-- order status: adding one would weaken the state machine's invariants for
-- every normal execution in order to describe an exceptional condition.
--
-- Set when an outcome is unknown or an unresolved issue references the order.
-- Cleared only when reconciliation establishes what actually happened.
-- ---------------------------------------------------------------------------

ALTER TABLE orders
    ADD COLUMN reconciliation_required BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX orders_reconciliation_required_idx
    ON orders (account_id) WHERE reconciliation_required;

-- Every order already FAILED has an unknown venue outcome by definition, so
-- the flag is backfilled rather than left false and quietly wrong.
UPDATE orders SET reconciliation_required = TRUE WHERE status = 'FAILED';

-- ---------------------------------------------------------------------------
-- Repair provenance on state transitions
--
-- A reconciliation repair and a normal execution result must not look the same
-- in the history. Reading `SUBMITTED -> FILLED` with no further information
-- cannot distinguish "the venue told us at the time" from "we reconstructed
-- this three hours later from a snapshot".
-- ---------------------------------------------------------------------------

ALTER TABLE order_state_transitions
    ADD COLUMN reconciliation_issue_id UUID
        REFERENCES reconciliation_issues (id) ON DELETE SET NULL,
    ADD COLUMN is_repair BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX order_state_transitions_repair_idx
    ON order_state_transitions (order_id) WHERE is_repair;

-- A repair must name the issue that justified it. A transition claiming to be
-- a repair with nothing to point at is not evidence of anything.
ALTER TABLE order_state_transitions
    ADD CONSTRAINT order_state_transitions_repair_ck CHECK (
        NOT is_repair OR reconciliation_issue_id IS NOT NULL
    );

-- ---------------------------------------------------------------------------
-- Fill provenance
--
-- A fill imported by reconciliation went through the same accounting path as
-- one returned by a PlaceOrder call -- that is the point -- but it was
-- discovered rather than received, and the ledger should say so.
-- ---------------------------------------------------------------------------

ALTER TABLE fills
    ADD COLUMN ingest_source TEXT NOT NULL DEFAULT 'execution_response',
    ADD COLUMN reconciliation_issue_id UUID
        REFERENCES reconciliation_issues (id) ON DELETE SET NULL;

ALTER TABLE fills
    ADD CONSTRAINT fills_ingest_source_ck CHECK (
        ingest_source IN ('execution_response', 'reconciliation_import', 'execution_poll')
    );

-- ---------------------------------------------------------------------------
-- Carry the old discrepancies across, then drop the table
-- ---------------------------------------------------------------------------

INSERT INTO reconciliation_issues (
    run_id, account_id, broker_name, issue_type, severity, status, repair_class,
    order_id, position_id, instrument_id, local_snapshot, broker_snapshot,
    description, fingerprint, detected_at, last_checked_at,
    resolved_at, resolution_action, resolution_reason
)
SELECT
    d.run_id,
    d.account_id,
    COALESCE(r.broker_name, 'mock'),
    -- The old free-text kinds map onto the taxonomy. Anything unrecognised
    -- becomes UNKNOWN_EXECUTION_STATE rather than being dropped: an
    -- unclassifiable historical divergence is still a divergence.
    CASE d.kind
        WHEN 'order_missing_at_broker'     THEN 'ORDER_MISSING_AT_BROKER'
        WHEN 'order_never_reached_venue'   THEN 'ORDER_MISSING_AT_BROKER'
        WHEN 'order_unknown_to_vantage'    THEN 'ORDER_MISSING_LOCALLY'
        WHEN 'order_status_mismatch'       THEN 'ORDER_STATUS_MISMATCH'
        WHEN 'fill_quantity_mismatch'      THEN 'PARTIAL_FILL_MISMATCH'
        WHEN 'position_missing_at_broker'  THEN 'POSITION_MISMATCH'
        WHEN 'position_unknown_to_vantage' THEN 'POSITION_MISMATCH'
        WHEN 'position_quantity_mismatch'  THEN 'POSITION_MISMATCH'
        WHEN 'balance_mismatch'            THEN 'BALANCE_MISMATCH'
        ELSE 'UNKNOWN_EXECUTION_STATE'
    END,
    d.severity,
    CASE WHEN d.resolved_at IS NOT NULL THEN 'RESOLVED' ELSE 'OPEN' END,
    -- Historical rows carry no repair classification. Calling them
    -- OPERATOR_REVIEW_REQUIRED is the safe reading: nothing should be
    -- auto-repaired on the strength of a migrated summary.
    'OPERATOR_REVIEW_REQUIRED',
    d.order_id,
    d.position_id,
    d.instrument_id,
    json_build_object('value', COALESCE(d.vantage_value, '')),
    json_build_object('value', COALESCE(d.broker_value, '')),
    d.description,
    -- Migrated rows keep a distinct fingerprint each, so the partial unique
    -- index cannot reject the copy for rows that were open simultaneously.
    'migrated:' || d.id::text,
    d.created_at,
    d.created_at,
    d.resolved_at,
    CASE WHEN d.resolved_at IS NOT NULL THEN 'RESOLVE_MANUALLY' ELSE NULL END,
    CASE WHEN d.resolved_at IS NOT NULL
         THEN COALESCE(NULLIF(d.resolution, ''), 'migrated from reconciliation_discrepancies')
         ELSE NULL END
FROM reconciliation_discrepancies d
LEFT JOIN reconciliation_runs r ON r.id = d.run_id;

DROP TABLE reconciliation_discrepancies;

-- ---------------------------------------------------------------------------
-- Grants
--
-- The application role gets DML only, as everywhere else. The research role
-- gets nothing here: reconciliation is execution state, and research has no
-- business reading it.
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT, UPDATE ON reconciliation_issues TO vantage_app;
GRANT SELECT, INSERT ON reconciliation_issue_events TO vantage_app;
GRANT USAGE, SELECT ON SEQUENCE reconciliation_issue_events_id_seq TO vantage_app;
GRANT SELECT, INSERT, UPDATE ON reconciliation_account_state TO vantage_app;
