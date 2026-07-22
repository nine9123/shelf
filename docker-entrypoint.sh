#!/bin/sh
set -e

# The cache (CACHE_PATH on the host) is typically a bind mount owned by whoever
# created it. Take ownership so the unprivileged runtime user can write there,
# then drop to that user. Not recursive: the contents are created by that same
# user, so this stays instant even with a huge cover cache.
mkdir -p "$CACHE_DIR"
chown "$PUID:$PGID" "$CACHE_DIR" 2>/dev/null || true

# The library stays read-only and untouched. Run the server as PUID:PGID.
exec gosu "$PUID:$PGID" /usr/local/bin/shelf "$@"
