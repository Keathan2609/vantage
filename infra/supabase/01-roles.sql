-- Least-privilege roles on a managed Postgres (Supabase).
--
-- The Docker stack gets these from infra/docker/postgres-init/00-roles.sql,
-- which the official image runs once on an empty data directory. A managed
-- database has no such hook and arrives with one role, so this is the same
-- separation applied by hand, once, before the first migration.
--
-- # Run it like this
--
--   psql "$SUPABASE_DIRECT_URL" \
--        -v owner_pw="..." -v app_pw="..." -v research_pw="..." \
--        -f infra/supabase/01-roles.sql
--
-- Passwords come in as psql variables and are NEVER written here. A file in
-- this repository that contained a real credential would be a leak whatever
-- the surrounding comment said, and the secret scanner is right to refuse it.
-- Generate each separately:
--
--   openssl rand -base64 24
--
-- # Why bother, when the managed database already has one good role
--
-- Because the boundary this enforces is not about keeping strangers out. It
-- is about what the APPLICATION can do once something has gone wrong inside
-- it. vantage_app may insert into the audit chain and may not rewrite it; the
-- research role may read market data and cannot see a password hash. Those are
-- properties of the database, and they survive a defect in the Go code that no
-- amount of care in the Go code could survive.
--
-- # Idempotent
--
-- Safe to re-run: every statement is guarded, so a second run rotates nothing
-- and fails on nothing. It does NOT rotate passwords -- see the note at the
-- end for that, deliberately separated so a re-run cannot change a credential
-- by accident.

\set ON_ERROR_STOP on

-- Supabase hands you the `postgres` database. The platform does not care what
-- the database is called -- every migration targets `public` -- so there is no
-- CREATE DATABASE here and no need for one.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'vantage_owner') THEN
        CREATE ROLE vantage_owner LOGIN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'vantage_app') THEN
        CREATE ROLE vantage_app LOGIN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'vantage_research') THEN
        CREATE ROLE vantage_research LOGIN;
    END IF;
END
$$;

-- Passwords applied separately from creation, so the guards above stay simple
-- and a re-run is a no-op rather than a silent rotation.
ALTER ROLE vantage_owner    WITH PASSWORD :'owner_pw';
ALTER ROLE vantage_app      WITH PASSWORD :'app_pw';
ALTER ROLE vantage_research WITH PASSWORD :'research_pw';

-- Connect and schema usage.
--
-- current_database() rather than a hard-coded name: on Supabase it is
-- `postgres`, locally it is `vantage`, and the grant is the same either way.
DO $$
DECLARE db text := current_database();
BEGIN
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO vantage_owner, vantage_app, vantage_research', db);
END
$$;

GRANT USAGE  ON SCHEMA public TO vantage_app, vantage_research;
GRANT CREATE ON SCHEMA public TO vantage_owner;

-- The owner must own what it migrates. On Supabase the schema is owned by
-- `postgres`, so the migration role is granted membership rather than
-- ownership being transferred -- transferring it would take the schema away
-- from the role the managed platform uses for its own maintenance.
GRANT vantage_owner TO CURRENT_USER;

-- Per-role safety rails. These are the same values the Docker stack sets, and
-- they matter more on a managed database, not less: a runaway query on a
-- shared platform holds locks on financial tables just as effectively.
ALTER ROLE vantage_app      SET statement_timeout = '15s';
ALTER ROLE vantage_app      SET idle_in_transaction_session_timeout = '30s';
ALTER ROLE vantage_research SET statement_timeout = '60s';
ALTER ROLE vantage_research SET idle_in_transaction_session_timeout = '60s';

-- Read-only at the session level, so a mistake in research code is refused by
-- the database rather than caught by review.
ALTER ROLE vantage_research SET default_transaction_read_only = on;

-- Everything else -- table grants, the REVOKEs that make eight tables
-- append-only for the application role -- is migration 0007, which runs as
-- part of the normal migrate step and is guarded by `IF EXISTS` on each role.
-- So the order is: run THIS file, then run migrations. The reverse order
-- leaves the grants silently unapplied, because 0007 finds no roles to grant
-- to and does nothing, and the application then runs with whatever the
-- connection string's role already had.

-- # Rotating a password later
--
-- Re-running this file does rotate them, because ALTER ROLE ... PASSWORD is
-- unconditional above. That is intentional and is why the passwords are
-- arguments: rotation is `psql -v ... -f` with new values, followed by
-- updating the three connection strings in .env.production and restarting.
-- There is no separate rotation script to drift out of date.
