#!/usr/bin/env bash
# Bring Vantage up on a fresh Ubuntu or Debian host.
#
#   curl -fsSL https://raw.githubusercontent.com/Keathan2609/vantage/master/scripts/bootstrap-host.sh -o bootstrap-host.sh
#   less bootstrap-host.sh          # read it before you run it
#   sudo bash bootstrap-host.sh
#
# Read it first. It installs Docker from Docker's own apt repository and starts
# containers that will hold your trading record; that is not something to pipe
# straight into a shell on anybody's word, including mine.
#
# # What this does NOT do
#
# It opens no ports. The stack binds every listener to 127.0.0.1 and the only
# route in is the Cloudflare tunnel, which dials OUT. If you find yourself
# adding a firewall rule to make this reachable, something has gone wrong --
# read docs/DEPLOYMENT.md rather than opening 8080 to the internet.
#
# It creates no accounts, buys nothing, and asks for no credential. The
# secrets it needs must already be in .env.production, which you write.
#
# # Idempotent
#
# Safe to re-run. Docker is installed only if absent, the repository is pulled
# rather than re-cloned, and compose reconciles the running stack with the
# files rather than starting a second copy.
set -euo pipefail

REPO_URL="${VANTAGE_REPO_URL:-https://github.com/Keathan2609/vantage.git}"
INSTALL_DIR="${VANTAGE_INSTALL_DIR:-/opt/vantage}"

say()  { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m    %s\033[0m\n' "$*"; }
die()  { printf '\n\033[1;31mSTOP: %s\033[0m\n\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run this with sudo: sudo bash $0"

# ---------------------------------------------------------------------------
say "Checking the host"

. /etc/os-release 2>/dev/null || die "cannot identify this operating system"
case "${ID:-}" in
    ubuntu|debian) ;;
    *) die "this script handles Ubuntu and Debian. On ${PRETTY_NAME:-this system}, install Docker yourself and skip to the compose command in docs/DEPLOYMENT.md" ;;
esac

# 2 GB is comfortable; below ~1 GB the Next.js build is the thing that dies,
# and it dies with an out-of-memory kill that reads as a corrupt install.
mem_mb=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)
if [ "$mem_mb" -lt 1800 ]; then
    warn "this host has ${mem_mb} MB of RAM. 2 GB is the comfortable figure."
    warn "The web image build is what fails first, and it fails as an OOM kill"
    warn "that looks like a broken build. Add swap, or build elsewhere."
fi

arch=$(uname -m)
say "Host: ${PRETTY_NAME:-unknown}, ${arch}, ${mem_mb} MB RAM"

# ---------------------------------------------------------------------------
say "Docker"

if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    say "Docker and the compose plugin are already present: $(docker --version)"
else
    say "Installing Docker from Docker's apt repository"
    apt-get update -qq
    apt-get install -y -qq ca-certificates curl gnupg git >/dev/null

    install -m 0755 -d /etc/apt/keyrings
    if [ ! -f /etc/apt/keyrings/docker.asc ]; then
        curl -fsSL "https://download.docker.com/linux/${ID}/gpg" -o /etc/apt/keyrings/docker.asc
        chmod a+r /etc/apt/keyrings/docker.asc
    fi
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/${ID} ${VERSION_CODENAME} stable" \
        > /etc/apt/sources.list.d/docker.list

    apt-get update -qq
    apt-get install -y -qq docker-ce docker-ce-cli containerd.io \
        docker-buildx-plugin docker-compose-plugin >/dev/null
    systemctl enable --now docker
fi

docker compose version >/dev/null 2>&1 || die "the docker compose plugin is not available after install"

# ---------------------------------------------------------------------------
say "Source"

if [ -d "$INSTALL_DIR/.git" ]; then
    say "Updating $INSTALL_DIR"
    git -C "$INSTALL_DIR" pull --ff-only
else
    say "Cloning into $INSTALL_DIR"
    git clone --depth 1 "$REPO_URL" "$INSTALL_DIR"
fi
cd "$INSTALL_DIR"

# ---------------------------------------------------------------------------
say "Configuration"

ENV_FILE="$INSTALL_DIR/.env.production"
if [ ! -f "$ENV_FILE" ]; then
    cp .env.production.example "$ENV_FILE"
    chmod 600 "$ENV_FILE"
    die "$ENV_FILE has been created from the template and every value in it is blank.

Fill it in, then run this script again. docs/DEPLOYMENT.md walks through it.
At minimum you need:

  * the two public origins, which must EXACTLY match the hostnames you map in
    the Cloudflare tunnel, including https and with no trailing slash
  * three application keys          openssl rand -base64 32
  * the database password(s)        openssl rand -base64 24
  * CLOUDFLARE_TUNNEL_TOKEN         from the Cloudflare dashboard

Nothing in that file may be left at a development default. The control plane
refuses to start in production if the example keys are still in place, which
is the behaviour you want and will look like a crash if you are not expecting
it."
fi

# A blank required value fails the compose command anyway -- every one is
# ${VAR:?} -- but failing here names the file instead of a variable.
missing=()
for key in VANTAGE_PUBLIC_WEB_ORIGIN VANTAGE_PUBLIC_API_ORIGIN \
           VANTAGE_DATA_ENCRYPTION_KEY VANTAGE_SESSION_SIGNING_KEY \
           VANTAGE_QUANT_SERVICE_TOKEN CLOUDFLARE_TUNNEL_TOKEN \
           VANTAGE_DATABASE_URL VANTAGE_MIGRATION_DATABASE_URL; do
    value=$(grep -E "^${key}=" "$ENV_FILE" | head -1 | cut -d= -f2-)
    [ -z "$value" ] && missing+=("$key")
done
if [ "${#missing[@]}" -gt 0 ]; then
    die "these are still blank in $ENV_FILE:
$(printf '  %s\n' "${missing[@]}")"
fi

chmod 600 "$ENV_FILE"

# Supabase or a local database? Decided by whether the Supabase host is set,
# so there is one source of truth rather than a flag that can disagree with
# the connection strings.
OVERLAYS=(-f infra/docker/docker-compose.yml -f infra/docker/docker-compose.prod.yml)
SERVICES=(control-api quant web cloudflared backup)
if grep -qE '^SUPABASE_DB_HOST=.+' "$ENV_FILE"; then
    say "SUPABASE_DB_HOST is set: using the managed database, no local Postgres"
    OVERLAYS+=(-f infra/docker/docker-compose.supabase.yml)
else
    say "No SUPABASE_DB_HOST: running Postgres in a container on this host"
    SERVICES=(postgres redis "${SERVICES[@]}")
fi

# ---------------------------------------------------------------------------
say "Building and starting"
warn "The web image build takes several minutes on a small host."

docker compose --env-file "$ENV_FILE" "${OVERLAYS[@]}" up -d --build "${SERVICES[@]}"

# ---------------------------------------------------------------------------
say "Waiting for the control plane to report ready"

ready=""
for _ in $(seq 1 60); do
    ready=$(docker compose --env-file "$ENV_FILE" "${OVERLAYS[@]}" exec -T control-api \
        /vantage-control-api healthcheck 2>&1) && break
    sleep 5
done

printf '\n%s\n' "$ready"

cat <<'NEXT'

==> Next, in this order

 1. Create your administrator. The seed command refuses to run outside
    development because its passwords are published in this repository, so
    bootstrap instead:

      cd /opt/vantage
      docker compose --env-file .env.production \
        -f infra/docker/docker-compose.yml -f infra/docker/docker-compose.prod.yml \
        exec control-api /vantage-control-api create-admin you@example.com "Your Name"

    It asks for a password twice without echoing it, applies the same policy
    the application does, and refuses once an administrator exists.

 2. Put Cloudflare Access in front of BOTH hostnames in the dashboard, then
    check from your laptop that the gate is actually there:

      curl -sI https://vantage.yourdomain.com | head -1

    A Cloudflare redirect is correct. A Vantage page means Access is NOT in
    front of it, and the login endpoint runs Argon2id at 64 MB per attempt --
    the one place an unauthenticated stranger can make this machine work.
    Fix that before anything else.

 3. Confirm the execution mode is what you think it is:

      curl -s https://vantage-api.yourdomain.com/api/v1/version \
        | grep -o '"live_trading_available":[a-z]*'

    It must say false. It cannot say anything else in this build, and checking
    is how you find out you are talking to the thing you think you are.

 4. Sign in and set up multi-factor authentication immediately. The admin
    account cannot trade; create a trader account from the admin interface for
    that. An admin session compromised through the admin surface must not be
    able to move money, which is why the two are separate.

NEXT
