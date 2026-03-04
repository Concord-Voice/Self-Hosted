#!/bin/sh
# Fix WAL archive directory ownership on every container start.
# Docker named volumes default to root:root, but postgres runs as UID 70 (alpine).
# Without this, the archive_command fails with "Permission denied" after restarts.
if [ -d /var/lib/postgresql/wal_archive ]; then
  chown postgres:postgres /var/lib/postgresql/wal_archive 2>/dev/null || true
fi

exec docker-entrypoint.sh "$@"
