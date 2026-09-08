-- Least-privilege database roles for Vantage.
--
-- Runs once, as the superuser, when the Postgres data directory is first
-- initialised. Three roles with three jobs:
--
--   vantage_owner    owns the schema; used ONLY to run migrations (DDL).
--   vantage_app      the control plane's runtime role: DML only, no DDL, no
--                    superuser. A compromised control plane cannot drop a
--                    table, disable a trigger, or read pg_authid.
--   vantage_research the quant service's role: READ ONLY, and only on the
--                    tables research legitimately needs. Research code cannot
--                    write to the ledger even if it tries, because the
--                    database will not let it.
--
-- These passwords are development-only and are published in .env.example.
-- A deployed environment sets them from a secret manager.

CREATE ROLE vantage_owner LOGIN PASSWORD 'vantage_owner_dev_password';
CREATE ROLE vantage_app LOGIN PASSWORD 'vantage_app_dev_password';
CREATE ROLE vantage_research LOGIN PASSWORD 'vantage_research_dev_password';

-- The application roles must not be able to create objects of their own in
-- public; everything they touch is created by the owner during migration.
REVOKE ALL ON SCHEMA public FROM PUBLIC;

GRANT CONNECT ON DATABASE vantage TO vantage_owner, vantage_app, vantage_research;

ALTER SCHEMA public OWNER TO vantage_owner;
GRANT USAGE ON SCHEMA public TO vantage_app, vantage_research;
GRANT CREATE ON SCHEMA public TO vantage_owner;

-- The owner needs database-level CREATE so it can rebuild the schema during
-- development resets. The runtime and research roles deliberately do not.
GRANT CREATE ON DATABASE vantage TO vantage_owner;

-- Statement timeouts per role, so a runaway research query cannot hold locks
-- that block the order pipeline.
ALTER ROLE vantage_app SET statement_timeout = '15s';
ALTER ROLE vantage_app SET idle_in_transaction_session_timeout = '30s';
ALTER ROLE vantage_research SET statement_timeout = '60s';
ALTER ROLE vantage_research SET idle_in_transaction_session_timeout = '60s';
ALTER ROLE vantage_research SET default_transaction_read_only = on;
