-- 0008_mock_venue: the simulated venue's own system of record.
--
-- These tables belong to the MOCK BROKER, not to Vantage. They exist so the
-- simulated venue keeps state independently of the control plane, which makes
-- reconciliation a genuine comparison of two separate stores rather than a
-- table joined against itself.
--
-- Only the mock adapter reads or writes these tables. No control-plane service
-- may query them directly: doing so would be the equivalent of reaching into a
-- real broker's internal database, and it would make reconciliation
-- meaningless.
--
-- When a real adapter is added, its venue state lives at the venue and these
-- tables simply go unused.

CREATE TABLE mock_venue_accounts (
    account_ref TEXT PRIMARY KEY,
    currency    TEXT NOT NULL,
    balance     NUMERIC(28,10) NOT NULL,
    leverage    NUMERIC(28,10) NOT NULL DEFAULT 100,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT mock_venue_accounts_currency_ck CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT mock_venue_accounts_leverage_ck CHECK (leverage > 0)
);

CREATE TABLE mock_venue_orders (
    broker_order_id  TEXT PRIMARY KEY,
    client_order_id  TEXT NOT NULL,
    account_ref      TEXT NOT NULL REFERENCES mock_venue_accounts (account_ref) ON DELETE CASCADE,
    symbol           TEXT NOT NULL,
    side             TEXT NOT NULL,
    type             TEXT NOT NULL,
    time_in_force    TEXT NOT NULL,
    status           TEXT NOT NULL,
    quantity         NUMERIC(28,10) NOT NULL,
    filled_quantity  NUMERIC(28,10) NOT NULL DEFAULT 0,
    avg_fill_price   NUMERIC(28,10) NOT NULL DEFAULT 0,
    limit_price      NUMERIC(28,10),
    stop_price       NUMERIC(28,10),
    stop_loss        NUMERIC(28,10),
    take_profit      NUMERIC(28,10),
    reject_reason    TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT mock_venue_orders_side_ck CHECK (side IN ('buy','sell')),
    CONSTRAINT mock_venue_orders_status_ck CHECK (status IN (
        'accepted','working','partially_filled','filled','cancelled','rejected','expired'
    )),
    CONSTRAINT mock_venue_orders_quantity_ck CHECK (quantity > 0),
    CONSTRAINT mock_venue_orders_filled_ck CHECK (filled_quantity >= 0 AND filled_quantity <= quantity)
);

-- The venue deduplicates on the client order id, exactly as a venue that
-- supports client order identifiers does. This is what makes a retry after a
-- lost response safe rather than a second position.
CREATE UNIQUE INDEX mock_venue_orders_client_uniq
    ON mock_venue_orders (account_ref, client_order_id);

CREATE INDEX mock_venue_orders_open_idx ON mock_venue_orders (account_ref, status)
    WHERE status IN ('accepted','working','partially_filled');

CREATE TABLE mock_venue_fills (
    broker_fill_id  TEXT PRIMARY KEY,
    broker_order_id TEXT NOT NULL REFERENCES mock_venue_orders (broker_order_id) ON DELETE CASCADE,
    account_ref     TEXT NOT NULL,
    symbol          TEXT NOT NULL,
    side            TEXT NOT NULL,
    quantity        NUMERIC(28,10) NOT NULL,
    price           NUMERIC(28,10) NOT NULL,
    commission      NUMERIC(28,10) NOT NULL DEFAULT 0,
    commission_ccy  TEXT NOT NULL,
    liquidity       TEXT NOT NULL DEFAULT 'taker',
    executed_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT mock_venue_fills_quantity_ck CHECK (quantity > 0),
    CONSTRAINT mock_venue_fills_price_ck CHECK (price > 0)
);

CREATE INDEX mock_venue_fills_poll_idx ON mock_venue_fills (account_ref, executed_at);

CREATE TABLE mock_venue_positions (
    account_ref     TEXT NOT NULL REFERENCES mock_venue_accounts (account_ref) ON DELETE CASCADE,
    symbol          TEXT NOT NULL,
    side            TEXT NOT NULL,
    quantity        NUMERIC(28,10) NOT NULL,
    avg_entry_price NUMERIC(28,10) NOT NULL,
    opened_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (account_ref, symbol),

    CONSTRAINT mock_venue_positions_side_ck CHECK (side IN ('buy','sell')),
    CONSTRAINT mock_venue_positions_quantity_ck CHECK (quantity > 0),
    CONSTRAINT mock_venue_positions_price_ck CHECK (avg_entry_price > 0)
);

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'vantage_app') THEN
        EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON
                    mock_venue_accounts, mock_venue_orders, mock_venue_fills, mock_venue_positions
                 TO vantage_app';
    END IF;
    -- The research role gets nothing here: simulated venue state is not
    -- research data, and reading it would let a model see broker internals no
    -- real deployment could expose.
END
$$;
