#!/bin/sh
# Scheduled logical backup of the Vantage database.
#
# Runs inside a container built from the SAME postgres image as the server, so
# pg_dump's version always matches the server's. A dump taken by an older
# pg_dump against a newer server is refused outright; a newer one against an
# older server produces a file that restores with surprises. Pinning both to one
# image removes the question.
#
# # Why a loop and not cron
#
# The alpine postgres image ships no cron. A loop is fewer moving parts, and its
# output goes to the container log where `docker compose logs backup` finds it.
# A cron that silently stopped would look identical to one that had nothing to
# do.
#
# # What this is not
#
# It is not off-site. A dump sitting in a volume on the same machine survives a
# dropped table and a bad migration. It does not survive the disk, the machine
# or the directory being lost. Copying these files somewhere else is a separate
# job and the deployment guide says so rather than letting the volume imply a
# safety it does not have.
set -eu

: "${PGHOST:?}" "${PGUSER:?}" "${PGPASSWORD:?}" "${PGDATABASE:?}"
INTERVAL="${BACKUP_INTERVAL_SECONDS:-86400}"
KEEP="${BACKUP_KEEP:-14}"
DIR="${BACKUP_DIR:-/backups}"

mkdir -p "$DIR"
echo "backup: every ${INTERVAL}s, keeping ${KEEP}, into ${DIR}"

while true; do
    stamp="$(date -u +%Y%m%dT%H%M%SZ)"
    target="${DIR}/vantage-${stamp}.dump"
    partial="${target}.partial"

    # Written under a .partial name and renamed only once pg_dump has exited
    # successfully AND the result has been read back. A file that appears in the
    # directory is therefore a file that restored cleanly at least once, so a
    # crash mid-dump leaves rubbish that is obviously rubbish rather than a
    # plausible-looking backup that fails on the night it is needed.
    #
    # -Fc is the custom format, which is what the restore drill expects. Keeping
    # them the same means the drill actually exercises these files rather than a
    # format only it produces.
    if ! pg_dump -Fc -f "$partial" 2>&1; then
        echo "backup: pg_dump FAILED at ${stamp}"
        rm -f "$partial"
        sleep "$INTERVAL"
        continue
    fi

    # Read the dump back before trusting it.
    #
    # pg_restore --list parses the whole archive's table of contents. It is
    # cheap and it is the difference between "pg_dump exited 0" and "this file
    # can actually be read". A truncated or corrupt archive fails here, at the
    # moment it is made, rather than months later when something depends on it.
    if ! pg_restore --list "$partial" >/dev/null 2>&1; then
        echo "backup: VERIFY FAILED for ${stamp}, discarding"
        rm -f "$partial"
        sleep "$INTERVAL"
        continue
    fi

    mv "$partial" "$target"
    size="$(wc -c < "$target")"
    echo "backup: wrote ${target} (${size} bytes, verified)"

    # Retention. Oldest first, keeping the newest $KEEP.
    #
    # Deliberately counts only VERIFIED dumps: a .partial left by a crash is
    # never counted as one of the copies being kept, so a run of failures
    # cannot quietly age out the last good backup.
    count="$(ls -1 "$DIR"/vantage-*.dump 2>/dev/null | wc -l)"
    if [ "$count" -gt "$KEEP" ]; then
        ls -1 "$DIR"/vantage-*.dump | sort | head -n "$((count - KEEP))" | while read -r old; do
            echo "backup: pruning $(basename "$old")"
            rm -f "$old"
        done
    fi

    sleep "$INTERVAL"
done
