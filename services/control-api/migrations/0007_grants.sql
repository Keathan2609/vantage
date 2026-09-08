-- 0007_grants: least-privilege grants for the runtime roles.
--
-- Applied by the migration runner (which connects as the schema owner). The
-- guards make this a no-op on installations that do not use separate roles,
-- such as a plain local Postgres where one role owns everything.
--
-- The important asymmetry: the control plane may INSERT into append-only
-- tables but may not UPDATE or DELETE them, and the research role may not
-- write anything at all. The append-only triggers already refuse those
-- operations; these grants mean the attempt is rejected before it reaches a
-- trigger that a sufficiently privileged attacker might otherwise disable.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'vantage_app') THEN
        -- Read/write on operational tables.
        EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO vantage_app';
        EXECUTE 'GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO vantage_app';

        -- Append-only tables: insert and read, never modify or remove.
        EXECUTE 'REVOKE UPDATE, DELETE ON
                    audit_events,
                    transactions,
                    fills,
                    order_state_transitions,
                    decision_snapshots,
                    risk_limit_history,
                    trading_authority_history,
                    strategy_lifecycle_history
                 FROM vantage_app';

        -- Schema changes are the owner's job alone.
        EXECUTE 'REVOKE CREATE ON SCHEMA public FROM vantage_app';
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'vantage_research') THEN
        -- Research reads market data, research artefacts and its own outputs.
        -- It is granted nothing on users, sessions, accounts, orders, fills,
        -- transactions, authorities, kill switches or broker connections:
        -- research has no business reading credentials or moving money, and
        -- the database is where that boundary is actually enforced.
        EXECUTE 'GRANT SELECT ON
                    instruments,
                    market_bars,
                    market_quotes,
                    market_quotes_latest,
                    market_data_health,
                    market_holidays,
                    fx_rates,
                    fx_rates_latest,
                    economic_events,
                    economic_event_instrument_map,
                    news_items,
                    strategies,
                    strategy_versions,
                    strategy_runs,
                    strategy_signals,
                    backtests,
                    backtest_trades,
                    ml_datasets,
                    ml_models,
                    ml_model_versions,
                    ml_evaluations,
                    ml_predictions,
                    ml_drift_events
                 TO vantage_research';
    END IF;
END
$$;

-- Future tables created by the owner inherit the same defaults.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'vantage_app') THEN
        EXECUTE 'ALTER DEFAULT PRIVILEGES IN SCHEMA public
                 GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO vantage_app';
        EXECUTE 'ALTER DEFAULT PRIVILEGES IN SCHEMA public
                 GRANT USAGE, SELECT ON SEQUENCES TO vantage_app';
    END IF;
END
$$;
