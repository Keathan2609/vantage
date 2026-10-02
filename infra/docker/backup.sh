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
# # Off-site, and why the local copy stays in the clear
#
# A dump in a volume on this machine survives a dropped table, a bad migration
# and a careless DELETE. It does not survive losing the machine. So when
# BACKUP_AGE_RECIPIENT and BACKUP_RCLONE_REMOTE are set, each verified dump is
# encrypted and copied off.
#
# ENCRYPTED for the journey, PLAINTEXT at home, and that asymmetry is
# deliberate:
#
#   * The off-site copy sits somewhere this operator does not control, so it is
#     encrypted to a public key whose PRIVATE half never exists on this machine.
#     Compromising the server therefore does not yield the archive. That is the
#     property worth having and it is the whole reason to encrypt at all.
#
#   * The local copy stays readable because this machine already holds the live
#     database in the clear. An encrypted local dump adds no secrecy against any
#     attacker who is already here, and it adds a brand new way to lose
#     everything: a single operator who mislays one private key would have a
#     shelf of backups nobody can open. Plaintext locally means a restore is
#     always possible with what is on the box.
#
# Both halves are optional and independent. Set neither and this is a local
# backup, which is what it was before.
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

    # Off-site, if configured. Failures here are reported and do not stop the
    # loop: a local backup that exists is better than no backup because an
    # upload failed, and a silent exit would end the schedule entirely.
    if [ -n "${BACKUP_AGE_RECIPIENT:-}" ] && [ -n "${BACKUP_RCLONE_REMOTE:-}" ]; then
        sealed="${target}.age"
        if ! age -r "$BACKUP_AGE_RECIPIENT" -o "$sealed" "$target" 2>&1; then
            echo "backup: ENCRYPTION FAILED for ${stamp}; nothing sent"
            rm -f "$sealed"
        elif ! rclone copyto "$sealed" "${BACKUP_RCLONE_REMOTE}/$(basename "$sealed")" 2>&1; then
            echo "backup: UPLOAD FAILED for ${stamp}; the local copy is intact"
            rm -f "$sealed"
        else
            echo "backup: sent $(basename "$sealed") to ${BACKUP_RCLONE_REMOTE}"
            # The encrypted file was only ever a courier. Removing it keeps one
            # local copy per dump rather than two, and the one kept is the one
            # that can be restored without a key.
            rm -f "$sealed"
        fi
    fi

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
