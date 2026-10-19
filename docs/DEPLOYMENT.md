# Deployment

How to run Vantage somewhere you can reach from any device, without putting a
login page on the open internet.

## What this architecture can and cannot run on

**It needs one always-on process.** The control plane is not a request handler
with some background work bolted on. It ingests market data every 2 seconds,
evaluates strategies every 30, dispatches the outbox every 3, reconciles every
5 minutes, and holds live state in memory between those ticks: the bar
aggregator's forming bars, a regime tracker per instrument, the correlation
matrix, rate-limit buckets and the research circuit breaker.

That rules out serverless hosts for the control plane, **Vercel included**.
Vercel functions are request-scoped and cold-start. The 2-second ingest loop has
nowhere to live, and the forming bars would be discarded on every invocation,
which is rule 9's frozen-series failure by construction rather than by accident.

Vercel can host the Next.js terminal, and does it well. It just cannot host the
thing the terminal talks to. Splitting them across two providers is possible and
buys nothing here, so the layout below keeps all four containers together.

**What it does need:** a machine that stays on, with Docker. 2 GB of RAM is
comfortable. That can be your own computer, a 5 USD VPS, or Oracle Cloud's
Always Free ARM tier, which is genuinely free and genuinely always on.

The same reasoning rules out Cloudflare Workers for the control plane, for the
avoidance of doubt. Workers are request-scoped like Vercel functions. Cloudflare
still does the two jobs it is best at here -- the tunnel and the gate -- and
Supabase can hold the database; neither can hold the process.

## Running the database on Supabase

Supported, and it changes one thing: where Postgres is. Every connection string
already arrives whole from the environment, so this is a change to
`.env.production` and to nothing else -- no code, and no edit to either of the
other compose files.

### Use the SESSION pooler, port 5432

The single choice here that misbehaves quietly if it is wrong.

Supabase offers a transaction pooler on 6543 and a session pooler on 5432.
**Use 5432.** `pgx` opens prepared statements, and `internal/db.Open` sets four
startup parameters on every connection: `statement_timeout`,
`idle_in_transaction_session_timeout`, `application_name` and `timezone`. A
transaction-mode pooler hands a different backend to each transaction, so
prepared statements disappear between uses and unrecognised startup parameters
are rejected. The resulting failures are intermittent and read as application
bugs.

The direct connection also works and is the lowest latency, but on the free
tier it resolves over **IPv6 only**, so a host without IPv6 fails to connect in
a way that looks like a firewall rule.

Set `sslmode=require`. The development URLs use `sslmode=disable`, and carrying
that over by accident is not something anyone notices later.

### Steps

1. Create a Supabase project. Note the project reference and the database
   password.
2. From **Project Settings**, **Database**, copy the **Session pooler**
   connection string. It looks like
   `postgresql://postgres.<ref>:<password>@aws-0-<region>.pooler.supabase.com:5432/postgres`.
3. Create the three least-privilege roles. A managed database has no
   `docker-entrypoint-initdb.d`, so the separation the Docker stack gets for
   free is applied once by hand:

   ```bash
   psql "<the session pooler URL>" \
     -v owner_pw="$(openssl rand -base64 24)" \
     -v app_pw="$(openssl rand -base64 24)" \
     -v research_pw="$(openssl rand -base64 24)" \
     -f infra/supabase/01-roles.sql
   ```

   Keep those three values; they go into the connection strings below. **Run
   this BEFORE the first migration.** Migration 0007 grants table privileges to
   these roles and is guarded by `IF EXISTS` on each one, so applying
   migrations first leaves the grants silently unapplied and the application
   running with whatever its role already had.

4. Write the two application connection strings into `.env.production`, using
   the role names and the passwords from step 3 against the pooler host:

   ```
   VANTAGE_DATABASE_URL=postgres://vantage_app:...@aws-0-<region>.pooler.supabase.com:5432/postgres?sslmode=require
   VANTAGE_MIGRATION_DATABASE_URL=postgres://vantage_owner:...@aws-0-<region>.pooler.supabase.com:5432/postgres?sslmode=require
   ```

   The database is `postgres`, not `vantage`. Supabase gives you one database
   and every migration targets the `public` schema, so the name is irrelevant.

5. Add the backup sidecar's view of the same database:

   ```
   SUPABASE_DB_HOST=aws-0-<region>.pooler.supabase.com
   SUPABASE_DB_PORT=5432
   SUPABASE_DB_USER=postgres.<ref>
   SUPABASE_DB_PASSWORD=...
   SUPABASE_DB_NAME=postgres
   ```

6. Start it, naming the services explicitly:

   ```bash
   cd infra/docker
   docker compose --env-file ../../.env.production \
     -f docker-compose.yml -f docker-compose.prod.yml -f docker-compose.supabase.yml \
     up -d --build control-api quant web cloudflared backup
   ```

   Naming them matters. The base file still defines a `postgres` service and an
   overlay cannot delete one, so a bare `up` would start a local database that
   nothing connects to.

### What this costs you

Be clear-eyed about it. The audit chain, the ledger, the order history and the
password hashes move onto a third party's infrastructure. TOTP secrets are
encrypted at rest with `VANTAGE_DATA_ENCRYPTION_KEY`, which stays on your
host, so Supabase holds ciphertext for those and plaintext for everything else.
Whoever controls the Supabase project can read the trading record.

For a single-operator paper platform that is a reasonable trade for not
administering a database. It is worth stating rather than discovering, and
`docs/THREAT_MODEL.md` assumes a self-hosted database -- if you take this path,
that document is describing a deployment you no longer have.

Backups still run. A managed provider protects against disk failure, not
against a bad migration, a careless DELETE, or losing access to the account, so
the sidecar keeps taking its own verified dumps and shipping them off-site
encrypted to a key Supabase never sees.

Redis stays local. Supabase does not offer it, the platform uses it only for
rate-limit buckets, and a bucket that resets on restart costs nothing --
adding a second managed provider to hold ten counters would be the worse trade.

## The shape

```
   your phone / laptop, anywhere
              |
              v
       Cloudflare edge          TLS terminates here. Cloudflare Access
              |                 challenges before anything is forwarded.
              v
      cloudflared container     Outbound connection only. No inbound port,
              |                 no public IP, no port forwarding.
     +--------+--------+
     |                 |
   web:3000      control-api:8080
                       |
              +--------+--------+
              |                 |
         postgres:5432     redis:6379     Internal network. Not reachable
                                          from the tunnel container.
```

Nothing is published to the internet. Every port in the compose file binds to
`127.0.0.1`, so even with the host firewall wide open there is nothing to
connect to. The only route in is the tunnel, which dials out.

**Cloudflare Access is the gate.** It sits in front of both hostnames and
challenges for identity before a request reaches Vantage at all. That matters
more than it sounds: the login endpoint runs Argon2id at 64 MB per attempt, and
it is the one place an unauthenticated stranger can make your machine do real
work. Access means they never reach it.

Both the tunnel and Access are free. Access is free for up to 50 users.

## The free host that actually works: Oracle Cloud Always Free

Of the free tiers, this is the one that fits. Render and Railway spin a free
service down when idle, which for this platform means the ingest loop stops and
the bar series freezes -- rule 9's failure, delivered by the hosting plan.
Fly.io keeps a machine running but wants a card. Oracle's Always Free ARM tier
gives up to 4 cores and 24 GB, permanently, and costs nothing.

**On the reclamation worry.** Oracle reclaims Always Free compute that has been
idle for seven days -- below roughly 15% CPU, 20% network and 20% memory. A
control plane ingesting every two seconds, evaluating every thirty and
reconciling every five minutes is never idle, so this workload is close to the
best possible fit for that policy. The real friction is capacity: ARM
(`VM.Standard.A1.Flex`) is frequently unavailable in popular regions at signup,
and you may have to try another region or retry over a few days.

Do **not** take the x86 Always Free shape (`VM.Standard.E2.1.Micro`). It has
1 GB of RAM and the Next.js build dies on it with an out-of-memory kill that
reads as a corrupt install.

Pick Ubuntu 22.04 or 24.04, 2 cores and 8 GB is generous, then:

```bash
curl -fsSL https://raw.githubusercontent.com/Keathan2609/vantage/master/scripts/bootstrap-host.sh -o bootstrap-host.sh
less bootstrap-host.sh      # read it first
sudo bash bootstrap-host.sh
```

It installs Docker, clones to `/opt/vantage`, refuses to continue until
`.env.production` is filled in, and brings the stack up. It opens no ports: the
only route in is the tunnel, which dials out. Re-running it is safe.

**Leave the Oracle security list alone.** Every listener binds to `127.0.0.1`
and the tunnel makes an outbound connection, so there is nothing to allow
through. If you find yourself opening 8080 to make this reachable, stop --
that is the one change that would put an Argon2id login endpoint on the open
internet.

**On ARM.** All four base images are multi-arch and nothing pins a platform or
a `GOARCH`, so the stack is expected to build natively. It has not been built
on ARM here, so treat that as reasoned rather than verified; the bootstrap
builds on the host, so you will know within one command.

## Steps

### 1. Get a domain onto Cloudflare

You need a domain with its nameservers pointed at Cloudflare. A cheap `.com` is
fine. Cloudflare's free plan is all this needs.

### 2. Create the tunnel

In the Cloudflare dashboard: **Zero Trust**, **Networks**, **Tunnels**, **Create
a tunnel**, choose **Cloudflared**. Name it, then copy the token it shows.

Add two public hostnames on the tunnel:

| Hostname | Service |
| --- | --- |
| `vantage.yourdomain.com` | `http://web:3000` |
| `vantage-api.yourdomain.com` | `http://control-api:8080` |

The service URLs use the container names because cloudflared resolves them on
the compose network.

### 3. Put Access in front of both

**Zero Trust**, **Access**, **Applications**, **Add an application**,
**Self-hosted**. Create one for each hostname, or one covering
`*.yourdomain.com`.

For the policy, the simplest that works is **Allow** with the rule **Emails** and
your own address. You will get a one-time code by email the first time each
device connects. If you would rather use Google or GitHub sign-in, add the
identity provider under **Settings**, **Authentication** first.

Do both hostnames. The terminal is a browser application that calls the API
directly, so the API hostname is reached by your browser and needs the same
gate. Access issues its cookie per hostname, so the first visit to each will
challenge once.

### 4. Fill in the environment

```bash
cp .env.production.example .env.production
```

Generate a separate value for every blank:

```bash
openssl rand -base64 32    # the three application keys
openssl rand -base64 24    # each of the four database passwords
```

Then write the two connection strings from the passwords you just set, rather
than substituting by hand:

```bash
set -a; . ./.env.production; set +a
cat >> .env.production <<EOF
VANTAGE_DATABASE_URL=postgres://vantage_app:${VANTAGE_POSTGRES_APP_PASSWORD}@postgres:5432/vantage?sslmode=disable
VANTAGE_MIGRATION_DATABASE_URL=postgres://vantage_owner:${VANTAGE_POSTGRES_OWNER_PASSWORD}@postgres:5432/vantage?sslmode=disable
EOF
```

Delete the two empty placeholder lines the example file shipped with, so each
variable is set once. The host is `postgres` because that is the container name
on the compose network, not localhost.

There is no third line for the research role. The research service has no
database driver and never had one, so the connection string it used to be given
was a credential nothing could use. The read-only `vantage_research` role is
still created, for an analyst connecting by hand.

Set `VANTAGE_PUBLIC_WEB_ORIGIN` and `VANTAGE_PUBLIC_API_ORIGIN` to the two
hostnames from step 2, with `https://` and no trailing slash. These have to be
exact. CORS allows one origin and compares it character for character, so a
mismatch appears in the browser as a CORS failure that reads like a bug in the
application.

Paste the tunnel token into `CLOUDFLARE_TUNNEL_TOKEN`.

### 5. Start it

```bash
cd infra/docker
docker compose --env-file ../../.env.production \
  -f docker-compose.yml -f docker-compose.prod.yml up -d --build
```

Migrations run at startup. A missing secret stops the command rather than
starting the stack with a blank one.

### 6. Create your account

`seed` refuses to run outside development, because the accounts it creates have
passwords published in this repository. Bootstrap instead:

```bash
docker compose --env-file ../../.env.production \
  -f docker-compose.yml -f docker-compose.prod.yml \
  exec control-api /vantage-control-api create-admin you@example.com "Your Name"
```

It asks for a password twice, without echoing it, and applies the same policy
the application does. It refuses once an administrator exists, so it cannot be
used to mint accounts on a running system; further administrators are created
through the admin interface, where the action is audited.

Then open `https://vantage.yourdomain.com`, clear the Access challenge, and sign
in. **Set up multi-factor authentication immediately.**

The admin account cannot trade. Create a trader account from the admin interface
for that, which is the separation the platform is built around: an admin session
compromised through the admin surface should not be able to move money.

## Checks worth running once it is up

```bash
# From the host. Should report ready, paper, and quant ok.
docker compose -f docker-compose.yml -f docker-compose.prod.yml \
  exec control-api /vantage-control-api healthcheck

# From your laptop. Should return the Access challenge, NOT the Vantage login.
curl -sI https://vantage.yourdomain.com | head -1
```

If that second command returns a Vantage page rather than a Cloudflare redirect,
Access is not actually in front of the hostname. Fix that before going further.

Confirm the execution mode is what you expect:

```bash
curl -s https://vantage-api.yourdomain.com/version | grep -o '"live_trading_available":[a-z]*'
```

It must say `false`. It cannot say anything else in this build, and checking is
how you find out you are talking to the thing you think you are.

## Operating it

### Backups

A `backup` container runs with the production overlay. Daily by default, keeping
fourteen copies, into the `postgres-backups` volume. Change either with
`BACKUP_INTERVAL_SECONDS` and `BACKUP_KEEP`.

It writes each dump under a `.partial` name, reads the archive back with
`pg_restore --list`, and only renames it into place once that succeeds. A file
in the directory is therefore one that has been read successfully at least once,
which is the difference between "pg_dump exited 0" and "this restores". Retention
counts only verified dumps, so a run of failures cannot quietly age out the last
good one.

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml logs backup | tail
docker compose -f docker-compose.yml -f docker-compose.prod.yml   exec backup ls -lh /backups
```

### Sending backups off the machine

A dump in a volume on this machine survives a dropped table, a bad migration and
a careless DELETE. It does not survive losing the machine. Set two variables and
each verified dump is encrypted and copied off.

**Generate a keypair, and keep the private half somewhere else.**

```bash
docker run --rm -it alpine sh -c 'apk add -q age && age-keygen'
```

Copy the private key into a password manager, or anywhere that is not this
server. Put only the public key in `.env.production`:

```
BACKUP_AGE_RECIPIENT=age1...your public key...
BACKUP_RCLONE_REMOTE=offsite:vantage-backups
BACKUP_RCLONE_CONFIG=/home/you/.config/rclone/rclone.conf
```

The private half never reaching the server is the entire point. Someone who
compromises this machine gets the live database, which they were always going to
get, and cannot read a single archived backup.

**Configure the remote with rclone**, which talks to Backblaze B2, Cloudflare R2,
S3, Google Drive, a second machine over SFTP and about forty other things:

```bash
rclone config        # name the remote `offsite`
```

B2 and R2 both have free tiers larger than this will need for a long time. Point
`BACKUP_RCLONE_CONFIG` at the resulting file; it is mounted read only.

Set neither variable and backups stay local, which is what they were before.

**Restoring from an off-site copy** takes one extra step:

```bash
rclone copy offsite:vantage-backups/vantage-TIMESTAMP.dump.age .
age -d -i /path/to/your.key -o restored.dump vantage-TIMESTAMP.dump.age
```

Then restore `restored.dump` exactly as above.

This path was tested end to end rather than assumed: dump, verify, encrypt,
copy to a remote, decrypt with a key the backup container never held, restore,
and `verify-audit` reporting 110 of 110 events verified on the result.

### Restoring

```bash
# A scratch database to restore into, so the live one is untouched.
docker compose -f docker-compose.yml -f docker-compose.prod.yml exec postgres   psql -U vantage_superuser -d postgres -c 'CREATE DATABASE vantage_restore OWNER vantage_owner;'

docker compose -f docker-compose.yml -f docker-compose.prod.yml exec backup sh -c   'pg_restore -h postgres -U vantage_superuser -d vantage_restore --no-owner      $(ls -1 /backups/vantage-*.dump | sort | tail -1)'
```

`--no-owner` because the restore runs as the superuser and the roles already
exist. **Do not add `--no-privileges`.** The first run of the restore drill used
it and produced a database the application could not read at all, failing with
"permission denied for table audit_events". The GRANTs are part of the backup
and have to come back with it.

Then check the restore is sound rather than assuming it:

```bash
# The application's own verifier, pointed at the restored copy.
docker compose -f docker-compose.yml -f docker-compose.prod.yml exec   -e VANTAGE_DATABASE_URL=postgres://u:p@postgres:5432/vantage_restore?sslmode=disable   control-api /vantage-control-api verify-audit
```

Substitute the app role and its password. It must report `verified: yes`. That
is the strongest single check available: if the hash chain still verifies, the
audit log came back byte for byte, and tamper evidence survived the round trip.

`scripts/backup-restore-drill.ps1` does all of the above against the development
stack and is the thing to run when you want to rehearse rather than recover.

**Updates.** `git pull`, then re-run the `up -d --build` command. Migrations are
forward only and apply at startup. The terminal's API origin is baked into the
image at build time, so a change to `VANTAGE_PUBLIC_API_ORIGIN` needs a rebuild
rather than a restart.

**Logs.** Capped at 50 MB per file, 5 files per service. `docker compose logs -f
control-api` to follow.

**What is not set up.** No metrics scraping, no alert delivery beyond the
in-application notifications, no log shipping. `/metrics` is served on a separate
internal listener and is not exposed through the tunnel, deliberately.

## If you would rather not use a domain

Tailscale is the alternative, and for a single operator it is arguably the
better one. Install it on the host and on your devices, and the stack is
reachable at the host's Tailscale address from anywhere, with no public
hostname, no certificate and no gate to configure, because there is no public
surface at all.

The cost is that it only works on devices where you have installed Tailscale and
signed in. The Cloudflare route above works from any browser, which is what
"accessible from any device" usually means in practice.
