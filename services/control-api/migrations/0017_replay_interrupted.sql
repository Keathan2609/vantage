-- The INTERRUPTED state: a run whose process stopped while it was active.
--
-- Distinct from `failed`, which means the pipeline errored, and from
-- `stopped`, which means an operator ended the run. Conflating them would lose
-- the one fact an operator needs at boot: this run did not finish and nobody
-- decided that it should not.
--
-- The policy it supports is written out in internal/replay/restart.go: a
-- replay STOPS at a process restart and requires an explicit operator resume.
-- Engaging a replay puts the whole process on dataset time, and a control
-- plane that came back up and silently moved its own clock to 2027 because a
-- row said a run was in progress would be deciding that for the operator, at
-- the moment nobody is watching.
--
-- Idempotent: CI applies migrations twice.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'replay_runs_state_ck'
    ) THEN
        ALTER TABLE replay_runs DROP CONSTRAINT replay_runs_state_ck;
    END IF;

    ALTER TABLE replay_runs
        ADD CONSTRAINT replay_runs_state_ck CHECK (
            state IN ('idle', 'running', 'paused', 'stopped', 'done', 'failed',
                      'interrupted'));
END $$;

-- "Which runs need an operator's attention" is the boot-time question.
CREATE INDEX IF NOT EXISTS replay_runs_interrupted_idx
    ON replay_runs (started_at DESC)
    WHERE state = 'interrupted';
