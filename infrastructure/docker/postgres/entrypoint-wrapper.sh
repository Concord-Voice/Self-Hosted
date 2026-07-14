#!/bin/sh
# Fix WAL archive directory ownership on every container start.
# Docker named volumes default to root:root, but postgres runs as UID 70 (alpine).
# Without this, the archive_command fails with "Permission denied" after restarts.
if [ -d /var/lib/postgresql/wal_archive ]; then
  chown postgres:postgres /var/lib/postgresql/wal_archive 2>/dev/null || true
fi

# Fresh clusters install these rules from docker-entrypoint-initdb.d after
# initdb creates pg_hba.conf. Existing volumes need them reconciled before
# PostgreSQL starts so the reader login can never escape its target database.
if [ -s "${PGDATA:-/var/lib/postgresql/data}/PG_VERSION" ]; then
  /usr/local/bin/configure-ops-metrics-hba.sh
fi

exec docker-entrypoint.sh "$@"
