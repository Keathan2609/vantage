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
# One sed, no dependency on anyone's git configuration.
RUN sed -i 's/$//' /backup.sh && sh -n /backup.sh
ENTRYPOINT ["/bin/sh", "/backup.sh"]
