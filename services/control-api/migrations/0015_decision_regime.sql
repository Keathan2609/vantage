-- Record the market regime, and its reasons, on every decision.
--
-- # Why a column and not another key in an existing jsonb blob
--
-- Because P&L is attributed by it. "Which regime lost money?" is a GROUP BY,
-- and a jsonb key cannot be indexed usefully for that without an expression
-- index that the next reader would not know existed. The reasons stay in jsonb
-- because nothing groups by them -- they are read one decision at a time, by a
-- human asking why.
--
-- # Why the regime is stored at all rather than recomputed
--
-- A later recalculation must not rewrite history. The regime a decision was
-- taken in is a fact about that moment: the classifier's thresholds may move,
-- the research plane's model may be retrained, and a holiday amendment may
-- change what session a timestamp falls in. Recomputing would silently
-- reattribute historical P&L to regimes the platform never actually acted on,
-- which is worse than having no regime at all -- it looks like evidence.
--
-- # Why the reasons are mandatory in practice
--
-- RISK_OFF is a protective state that stops trading. An operator asked to
-- accept it with no account of itself will eventually switch it off, so the
-- checks that fired -- AND the ones that passed -- travel with the verdict. A
-- verdict listing only its triggers is unfalsifiable.
--
-- Idempotent: CI applies migrations twice.

ALTER TABLE decision_snapshots
    ADD COLUMN IF NOT EXISTS regime text NOT NULL DEFAULT 'UNKNOWN';

ALTER TABLE decision_snapshots
    ADD COLUMN IF NOT EXISTS regime_policy_version text NOT NULL DEFAULT '';

-- The evidence behind the label: every check, what it observed, its threshold,
-- and whether it contributed. jsonb rather than json because nothing depends
-- on its byte representation here (unlike audit metadata, whose hash does).
ALTER TABLE decision_snapshots
    ADD COLUMN IF NOT EXISTS regime_reasons jsonb NOT NULL DEFAULT '[]'::jsonb;

-- UNKNOWN is the default deliberately.
--
-- Decisions taken before this column existed genuinely have no recorded
-- regime, and UNKNOWN is the label that already means "not characterised".
-- Backfilling them with a guess would manufacture history.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'decision_snapshots_regime_ck'
    ) THEN
        ALTER TABLE decision_snapshots
            ADD CONSTRAINT decision_snapshots_regime_ck CHECK (
                regime IN ('TRENDING', 'RANGING', 'HIGH_VOLATILITY',
                           'LOW_VOLATILITY', 'EVENT_RISK', 'RISK_OFF', 'UNKNOWN'));
    END IF;
END $$;

-- "Every decision taken in this regime" is the attribution query.
CREATE INDEX IF NOT EXISTS decision_snapshots_regime_idx
    ON decision_snapshots (regime, created_at DESC);
