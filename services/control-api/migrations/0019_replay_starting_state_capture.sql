-- Distinguish "the account was flat" from "nobody could read the account".
--
-- # The defect
--
-- `starting_positions` is `integer NOT NULL DEFAULT 0`, and the input gatherer
-- is deliberately non-fatal: a replay whose starting balance could not be read
-- was still allowed to start, and the record said so by carrying empty fields.
-- For the BALANCE that works, because an empty string is not a balance. For
-- the POSITION COUNT it does not: a run that began on a flat account and a run
-- whose portfolio snapshot failed both record 0, and nothing distinguishes
-- them afterwards.
--
-- An empty position set is valid data. A failed snapshot is missing data.
-- Reading one as the other is how a calibration run silently attributes a
-- result to a starting state it never observed.
--
-- # Why a status column and not a nullable count
--
-- Making `starting_positions` nullable would encode the distinction but not
-- the REASON, and it would leave every other gathered field -- balance,
-- currency, risk hash, authority hash -- with the same ambiguity and no
-- corresponding fix. One status covers the whole capture, and the error text
-- says what went wrong.
--
-- NOT_ATTEMPTED is a third state and a real one: a run started before this
-- column existed, or by a process with no gatherer installed, genuinely did
-- not try. Backfilling those as CAPTURED would manufacture an observation.
--
-- Idempotent: CI applies migrations twice.

ALTER TABLE replay_runs
    ADD COLUMN IF NOT EXISTS starting_state_capture text NOT NULL DEFAULT 'NOT_ATTEMPTED';

-- The reason, when capture failed. Empty otherwise. Never a stack trace and
-- never a connection string: this is read by an operator deciding whether the
-- run's result can be trusted.
ALTER TABLE replay_runs
    ADD COLUMN IF NOT EXISTS starting_state_error text NOT NULL DEFAULT '';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'replay_runs_starting_capture_ck'
    ) THEN
        ALTER TABLE replay_runs
            ADD CONSTRAINT replay_runs_starting_capture_ck CHECK (
                starting_state_capture IN ('CAPTURED', 'CAPTURE_FAILED', 'NOT_ATTEMPTED'));
    END IF;
END $$;

-- A failed capture must carry its reason, and a successful one must not
-- pretend to have had a problem. The constraint is what stops "CAPTURE_FAILED
-- with no explanation" becoming the common case the moment someone adds a
-- code path that forgets to set it.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'replay_runs_starting_capture_reason_ck'
    ) THEN
        ALTER TABLE replay_runs
            ADD CONSTRAINT replay_runs_starting_capture_reason_ck CHECK (
                (starting_state_capture = 'CAPTURE_FAILED' AND starting_state_error <> '')
                OR (starting_state_capture <> 'CAPTURE_FAILED' AND starting_state_error = ''));
    END IF;
END $$;

-- "Which runs cannot be trusted as research evidence" is the query this
-- column exists to answer.
CREATE INDEX IF NOT EXISTS replay_runs_capture_idx
    ON replay_runs (starting_state_capture, started_at DESC)
    WHERE starting_state_capture <> 'CAPTURED';
