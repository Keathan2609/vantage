-- Tag every order created while a market replay owned the clock.
--
-- # Why a trading table carries a development tool's id
--
-- Because otherwise its numbers contaminate the ones that matter. A replay
-- writes real orders through the real OMS into the real ledger -- that is the
-- whole point of it, and the reason it is evidence -- but those orders were
-- decided against dataset prices at a dataset instant. Mixed into the same
-- account as a paper-forward session's orders, they are indistinguishable, and
-- every question about how the platform behaves on live simulated data gets a
-- polluted answer.
--
-- So this is not bookkeeping for the replay's benefit. It is what lets
-- BACKTEST/REPLAY be separated from PAPER_FORWARD at all, which is the
-- comparison a paper-forward programme rests on. An untagged order was decided
-- on a live feed; a tagged one was not.
--
-- # Why the foreign key, and why it is not ON DELETE CASCADE
--
-- A run with orders attributed to it cannot be deleted, which is correct: the
-- run record is the only thing that says which dataset, code SHA and
-- configuration produced those orders. Cascading would delete the orders,
-- which would be a financial record destroyed to tidy a development artefact.
-- The application cannot DELETE replay_runs at all (see 0013), so this is a
-- second line rather than the first.
--
-- # Why nullable
--
-- NULL means "no replay was engaged when this order was created", which is the
-- ordinary case and the one that matters. A default would be a lie in one
-- direction or the other.
--
-- Idempotent: CI applies migrations twice.

ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS replay_run_id uuid REFERENCES replay_runs (id);

COMMENT ON COLUMN orders.replay_run_id IS
    'The market replay that owned the clock when this order was created. '
    'NULL means a live feed. Orders with a value were decided against dataset '
    'prices at a dataset instant and are not paper-forward evidence.';

-- Partial: the overwhelming majority of rows are NULL, and the query is always
-- "the orders belonging to this run".
CREATE INDEX IF NOT EXISTS orders_replay_run_idx
    ON orders (replay_run_id, created_at DESC)
    WHERE replay_run_id IS NOT NULL;
