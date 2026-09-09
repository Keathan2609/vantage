-- Autopilot: a single global switch for autonomous decision-making.
--
-- # Why this is not a kill switch
--
-- A kill switch halts ORDERS -- every order, from every source, including an
-- operator's. That is the right control for "stop everything now" and the
-- wrong one for "stop the robot".
--
-- Autopilot OFF stops the autonomous pipeline only: no scanning, no strategy
-- evaluation that could route, no order intents created without a human. Every
-- read path stays live and an operator can still trade manually. The two
-- controls answer different questions and conflating them would mean an
-- operator who wants to take over by hand has to first disable the protection
-- that stops the machine competing with them.
--
-- # Why a table rather than configuration
--
-- Configuration is set at boot. This has to be changeable while running,
-- attributable to whoever changed it, and durable across a restart -- an
-- autopilot that comes back ON because the process was restarted is the worst
-- possible default. It also has to be readable inside the same transaction
-- that places an order, which a config value in memory is not.
--
-- Idempotent: CI applies migrations twice.

CREATE TABLE IF NOT EXISTS autopilot_state (
    -- A single row, enforced by the primary key. There is one autopilot.
    id              boolean     PRIMARY KEY DEFAULT true,
    enabled         boolean     NOT NULL DEFAULT false,
    -- The reason is mandatory and non-empty. A switch flipped without a
    -- recorded reason is a switch nobody can account for later.
    reason          text        NOT NULL,
    changed_by      uuid        REFERENCES users (id),
    changed_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT autopilot_state_single_row_ck CHECK (id),
    CONSTRAINT autopilot_state_reason_ck CHECK (length(btrim(reason)) > 0)
);

-- Default OFF, deliberately.
--
-- The platform has never traded unattended. An installation that arrives with
-- autonomous trading already enabled would be making that decision on the
-- operator's behalf.
INSERT INTO autopilot_state (id, enabled, reason)
VALUES (true, false, 'default: autonomous trading has not been enabled on this installation')
ON CONFLICT (id) DO NOTHING;

-- Append-only history, so "when was autopilot on?" is answerable.
--
-- The single-row table above holds only the current state. Without this, a
-- period of autonomous trading leaves no trace once the switch is flipped
-- back, and an investigation into a trade cannot establish whether autopilot
-- was even running when it was placed.
CREATE TABLE IF NOT EXISTS autopilot_state_history (
    id          bigserial   PRIMARY KEY,
    enabled     boolean     NOT NULL,
    reason      text        NOT NULL,
    changed_by  uuid        REFERENCES users (id),
    changed_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT autopilot_history_reason_ck CHECK (length(btrim(reason)) > 0)
);

CREATE INDEX IF NOT EXISTS autopilot_state_history_time_idx
    ON autopilot_state_history (changed_at DESC);

INSERT INTO autopilot_state_history (enabled, reason)
SELECT false, 'default: autonomous trading has not been enabled on this installation'
WHERE NOT EXISTS (SELECT 1 FROM autopilot_state_history);

-- Append-only. The same trigger function the other history tables use.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'vantage_reject_mutation') THEN
        DROP TRIGGER IF EXISTS autopilot_state_history_append_only
            ON autopilot_state_history;
        CREATE TRIGGER autopilot_state_history_append_only
            BEFORE UPDATE OR DELETE ON autopilot_state_history
            FOR EACH ROW EXECUTE FUNCTION vantage_reject_mutation();
    END IF;
END $$;

GRANT SELECT, INSERT, UPDATE ON autopilot_state TO vantage_app;
GRANT SELECT, INSERT ON autopilot_state_history TO vantage_app;
GRANT USAGE, SELECT ON SEQUENCE autopilot_state_history_id_seq TO vantage_app;
