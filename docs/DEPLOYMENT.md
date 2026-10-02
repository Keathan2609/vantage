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

Then write the three connection strings from the passwords you just set, rather
than substituting by hand:

```bash
set -a; . ./.env.production; set +a
cat >> .env.production <<EOF
VANTAGE_DATABASE_URL=postgres://vantage_app:${VANTAGE_POSTGRES_APP_PASSWORD}@postgres:5432/vantage?sslmode=disable
VANTAGE_MIGRATION_DATABASE_URL=postgres://vantage_owner:${VANTAGE_POSTGRES_OWNER_PASSWORD}@postgres:5432/vantage?sslmode=disable
VANTAGE_QUANT_READONLY_DATABASE_URL=postgres://vantage_research:${VANTAGE_POSTGRES_RESEARCH_PASSWORD}@postgres:5432/vantage?sslmode=disable
EOF
```

Delete the three empty placeholder lines the example file shipped with, so each
variable is set once. The host is `postgres` because that is the container name
on the compose network, not localhost.

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
curl -s https://vantage-api.yourdomain.com/api/v1/version | grep -o '"live_trading_available":[a-z]*'
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
