-- Record the multi-strategy consensus verdict on every decision.
--
-- # What changed above this column
--
-- Until now the scheduler called EvaluateAndRoute once per
-- (strategy, instrument) with Execute:true, and each call routed its own
-- signal independently. Two strategies disagreeing on one bar therefore
-- produced two opposing orders and both filled: measured on the
-- conflicting-signals replay fixture, 38 instants split the strategy set and
-- all 38 produced filled orders on both sides of the same instrument.
--
-- orchestrator.Decide -- a pure, versioned aggregation policy that puts vetoes
-- before votes and never takes a majority vote -- existed the whole time and
-- had no production caller. It has one now: the strategies evaluated for one
-- (account, instrument, bar) are aggregated into ONE verdict before anything
-- reaches the OMS.
--
-- # Why the verdict needs a column
--
-- Because the interesting outcome is NO TRADE, and until now no row was
-- written for one. A decision snapshot is created by the OMS, and the OMS is
-- only called when there is an order to place; an aggregate that declines
-- never reached it. "Why did it not trade?" was answerable only from the
-- individual strategy_runs, which record what each strategy said and cannot
-- record what the policy did with the disagreement.
--
-- The snapshot table already anticipated this: outcome has always permitted
-- 'no_trade', and strategy_id and strategy_version have always been nullable.
-- A verdict is not attributable to one strategy, which is the point of it.
--
-- # Why jsonb and not columns
--
-- Nothing groups by a veto or a rationale line. They are read one decision at
-- a time, by a human asking why -- unlike regime, which is a GROUP BY and
-- therefore earned its own column in 0015. The two figures that might be
-- aggregated, the action and the confidence, already have columns:
-- signal_action and confidence carry the verdict's own, not any one
-- strategy's.
--
-- Idempotent: CI applies migrations twice.

-- The whole verdict: action, confidence, policy_version, reason, vetoes,
-- rationale and every contribution INCLUDING the discarded ones. The discards
-- are the part an operator needs most -- "the mean-reversion strategy said
-- SELL and was discarded because it declares itself valid in RANGING and the
-- regime is TRENDING" is the answer to the question actually being asked.
--
-- Defaults to an empty object rather than NULL so a reader never has to
-- distinguish "no consensus ran" from "the column was not written". Decisions
-- taken before this column existed have no verdict, and {} says exactly that;
-- backfilling one would manufacture a decision the platform never took.
ALTER TABLE decision_snapshots
    ADD COLUMN IF NOT EXISTS consensus jsonb NOT NULL DEFAULT '{}'::jsonb;

-- "Every instant this account considered this instrument, traded or not" is
-- the query that makes a no-trade verdict findable at all. Without it, the
-- no_trade rows are only reachable by scanning the account's whole history,
-- and there are far more of them than there are orders -- NO TRADE is the
-- common outcome by design.
CREATE INDEX IF NOT EXISTS decision_snapshots_no_trade_idx
    ON decision_snapshots (account_id, instrument_id, bar_time DESC)
    WHERE outcome = 'no_trade';
