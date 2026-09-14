-- The full declared inputs of a replay run.
--
-- # Why every one of these is a column and not a comment
--
-- "A run should be reproducible from its declared inputs" is only true if the
-- inputs were declared. A run record carrying the dataset and the seed but not
-- the warm-up boundary, the starting balance or the configuration hashes
-- cannot be re-executed, and two such runs cannot be compared: the differences
-- would be attributed to the code when they belonged to a threshold nobody
-- wrote down.
--
-- The timestamps are the ones that make "what did this run see" answerable.
-- `warmup_start` is the floor applied to every historical read, so it bounds
-- what could possibly have influenced a decision; `evaluation_start` is when
-- executable intents became permissible. Both are dataset time, not wall time.
--
-- The starting state matters for a different reason: a run that began with an
-- open position and a drawn-down balance behaves differently from one that
-- began flat, and neither the dataset hash nor the code SHA records that.
--
-- The configuration digests are short hashes rather than full documents. The
-- point is to detect that something moved, not to reconstruct it here: a
-- reader who finds two runs with different risk digests knows the comparison
-- is invalid, which is the question a digest can answer.
--
-- Idempotent: CI applies migrations twice.

ALTER TABLE replay_runs
    ADD COLUMN IF NOT EXISTS warmup_start     timestamptz,
    ADD COLUMN IF NOT EXISTS evaluation_start timestamptz,
    ADD COLUMN IF NOT EXISTS evaluation_end   timestamptz,
    -- Nullable: a run recorded before this migration genuinely has no
    -- declared window, and backfilling one would manufacture history.
    ADD COLUMN IF NOT EXISTS allow_warmup_trading boolean NOT NULL DEFAULT false;

ALTER TABLE replay_runs
    -- The account as it stood when the run began.
    ADD COLUMN IF NOT EXISTS starting_balance   numeric(28,10),
    ADD COLUMN IF NOT EXISTS starting_currency  text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS starting_positions integer NOT NULL DEFAULT 0;

ALTER TABLE replay_runs
    -- What was in force. Digests, not documents: enough to detect that a
    -- comparison is invalid, which is the question they exist to answer.
    ADD COLUMN IF NOT EXISTS risk_config_hash        text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS authority_config_hash   text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS correlation_policy      text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS regime_policy           text NOT NULL DEFAULT '',
    -- Which strategy and model versions were live. An array rather than a
    -- digest because "which strategies ran" is a question a reader asks
    -- directly, and a hash would force them to go and look it up.
    ADD COLUMN IF NOT EXISTS strategy_versions jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS model_versions    jsonb NOT NULL DEFAULT '[]'::jsonb;

-- The window has to be coherent when it is recorded at all. A run whose
-- evaluation began before its warm-up would be uninterpretable, and a CHECK
-- catches it at the point of writing rather than at the point of reading.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'replay_runs_window_ck'
    ) THEN
        ALTER TABLE replay_runs
            ADD CONSTRAINT replay_runs_window_ck CHECK (
                (warmup_start IS NULL AND evaluation_start IS NULL AND evaluation_end IS NULL)
                OR (warmup_start IS NOT NULL
                    AND evaluation_start IS NOT NULL
                    AND evaluation_end IS NOT NULL
                    AND evaluation_start >= warmup_start
                    AND evaluation_end >= evaluation_start));
    END IF;
END $$;

COMMENT ON COLUMN replay_runs.warmup_start IS
    'Dataset time. The floor applied to every historical read during this run: '
    'nothing before it could have influenced a decision.';
COMMENT ON COLUMN replay_runs.evaluation_start IS
    'Dataset time. Executable intents became permissible here; before it the '
    'run built indicators, regime and correlation state only.';
