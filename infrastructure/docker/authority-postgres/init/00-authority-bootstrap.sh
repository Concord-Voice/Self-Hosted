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

pw_app="$(cat /run/secrets/authority_app_password)"
pw_owner="$(cat /run/secrets/authority_owner_password)"

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
  -v pw_app="$pw_app" -v pw_owner="$pw_owner" <<'SQL'
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
