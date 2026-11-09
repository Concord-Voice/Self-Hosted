#!/bin/sh
# Bootstrap of the self-host authority's own PostgreSQL (DoR D-109, D-173).
# initdb runs it once, on an empty volume, as the postgres OS user. It does
# what the managed realm's provisioner does on the VM: the three roles, the
# database owned by authority_owner, and the `authority` schema on the
# search_path before the first migration (B1-D2). This cluster holds no CP
# data, and the CP's cluster never holds these roles.
#
# The two LOGIN passwords come from the files the installer wrote. The
# authority itself reads them from passfiles, never from its URL.
set -eu

# psql reads each file itself (\set with a backtick command), so neither
# password is ever on a command line another process can read. psql does not
# stop when that command fails, so check the files first: an empty password
# would only clear the role's password, and the bootstrap would still finish.
for f in /run/secrets/authority_app_password /run/secrets/authority_owner_password; do
  [ -r "$f" ] && [ -s "$f" ] || { echo "authority bootstrap: $f is missing, empty or unreadable" >&2; exit 1; }
done
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres <<'SQL'
\set pw_app `cat /run/secrets/authority_app_password`
\set pw_owner `cat /run/secrets/authority_owner_password`
CREATE ROLE authority_owner LOGIN PASSWORD :'pw_owner';
CREATE ROLE authority_app LOGIN PASSWORD :'pw_app';
CREATE ROLE authority_backup NOLOGIN;
CREATE DATABASE authority OWNER authority_owner;
\connect authority
CREATE SCHEMA authority AUTHORIZATION authority_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
ALTER DATABASE authority SET search_path = authority;
SQL

# Socket only (listen_addresses is empty), and only the two roles, each to
# its own database. Everything else is refused, the superuser included.
cat > "$PGDATA/pg_hba.conf" <<'HBA'
local authority authority_app   scram-sha-256
local authority authority_owner scram-sha-256
local all       all             reject
HBA

# Last: the healthcheck waits for this marker, so a bootstrap that stops
# earlier never reports healthy.
touch "$PGDATA/authority-bootstrap.done"
