# The backup sidecar.
#
# Built FROM the same postgres image the server runs, so pg_dump's version
# always matches the server's. A dump taken by an older pg_dump against a newer
# server is refused outright, and a newer one against an older server restores
# with surprises; pinning both to one image removes the question entirely.
FROM postgres:17.6-alpine

# age for encryption, rclone for the off-site copy.
#
# Installed at BUILD time, not at container start. An `apk add` in the entrypoint
# needs the network every time the container restarts, and a backup job that
# cannot start because a package mirror is down is a backup job that silently
# stops running.
#
# age rather than gpg: one file format, one flag, no keyring, no agent, and no
# way to accidentally encrypt to the wrong key because a keyring had a stale
# entry in it. This project does not implement cryptography and does not intend
# to; this is composing a reviewed tool.
RUN apk add --no-cache age rclone

COPY backup.sh /backup.sh

# Strip any carriage returns the build context happened to carry.
#
# .gitattributes already forces LF on checkout, so a clone is fine. A working
# tree is not guaranteed to be: an editor, a generated patch or a tool writing
# in text mode on Windows can leave CRLF behind, and the image then fails at
# `set -eu` with "illegal option -", which names neither the file nor the line
# ending. That happened while building this, twice.
#
# `tr -d` rather than a sed expression, because matching a carriage return in
# sed meant putting a literal CR BYTE in this file. An invisible byte in a build
# file is one careless paste away from silently becoming a no-op that still
# builds green. This version says what it does in characters you can see.
#
# `sh -n` is the check that matters: it parses the script without running it, so
# a bad script fails the BUILD rather than the first backup.
RUN tr -d '\r' < /backup.sh > /backup.clean \
    && mv /backup.clean /backup.sh \
    && sh -n /backup.sh

# Run as the unprivileged postgres user, like every other container here.
#
# The directory is created WITH its ownership in the image, which is what makes
# this work: Docker initialises an empty named volume from the image's directory
# at that path, ownership included. Without the mkdir the volume arrives owned
# by root, the unprivileged process cannot write to it, and the first backup
# fails with a permission error that reads like a volume problem.
RUN mkdir -p /backups && chown postgres:postgres /backups

# rclone's configuration lives at an explicit path rather than under $HOME.
#
# rclone defaults to $HOME/.config/rclone/rclone.conf. Mounting the operator's
# config under /root was fine while this ran as root and became unreadable the
# moment it did not: /root is mode 0700, so uid 70 gets "permission denied" and
# every off-site upload fails while the local backup keeps succeeding -- the
# quietest possible way to lose the only copy that survives losing the machine.
# An absolute path in RCLONE_CONFIG does not depend on which user runs this.
ENV RCLONE_CONFIG=/etc/rclone/rclone.conf

USER postgres

ENTRYPOINT ["/bin/sh", "/backup.sh"]
