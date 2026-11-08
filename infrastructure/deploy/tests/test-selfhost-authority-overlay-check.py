#!/usr/bin/env python3
"""Contract check for docker-compose.authority.yml (DoR D-109, D-173).

Reads `docker compose ... config --format json` on stdin and prints one FAIL
line per broken rule, naming its letter:
  (a) the long-running authority has no owner URL, owner secret or owner role;
  (b) the owner job has a profile and restart "no";
  (c) only the authority mounts the RSK and TLS-key secrets;
  (d) no other service carries an AUTHORITY_* variable or an authority secret;
  (e) the authority's PostgreSQL shares no volume or socket with the CP's
      postgres, has no network, listens on no TCP address and skips the CP
      image's wrapper.
Exit 0 when every rule holds, 1 otherwise.
"""
import json
import sys

AUTHORITY = {"authority", "authority-owner", "authority-postgres"}
OWNER_SECRETS = {"authority_owner_pgpass", "authority_owner_password", "authority_superuser_password"}
KEY_SECRETS = {"authority_rsk", "authority_tls_key"}


def secret_sources(svc):
    return {s["source"] for s in svc.get("secrets") or []}


def volume_sources(svc):
    return {v.get("source") for v in svc.get("volumes") or [] if v.get("source")}


def check(model):
    services = model.get("services", {})
    fails = []
    for name in AUTHORITY:
        if name not in services:
            fails.append(f"{name} is missing")
    if fails:
        return fails
    auth, owner, apg = services["authority"], services["authority-owner"], services["authority-postgres"]

    env = auth.get("environment") or {}
    if "AUTHORITY_MIGRATE_DATABASE_URL" in env:
        fails.append("(a) authority carries AUTHORITY_MIGRATE_DATABASE_URL")
    if secret_sources(auth) & OWNER_SECRETS:
        fails.append("(a) authority mounts an owner secret")
    url = env.get("AUTHORITY_DATABASE_URL") or ""
    if "user=authority_app&" not in url + "&" or "authority_owner" in url:
        fails.append("(a) authority's database URL is not authority_app's")

    if not owner.get("profiles"):
        fails.append("(b) authority-owner has no profile")
    if owner.get("restart") != "no":
        fails.append('(b) authority-owner restart is not "no"')

    for name, svc in services.items():
        held = secret_sources(svc) & KEY_SECRETS
        if held and name != "authority":
            fails.append(f"(c) {name} mounts {', '.join(sorted(held))}")

    for name, svc in services.items():
        if name in AUTHORITY:
            continue
        keys = [k for k in (svc.get("environment") or {}) if k.startswith("AUTHORITY_")]
        if keys:
            fails.append(f"(d) {name} carries {', '.join(sorted(keys))}")
        if any(s.startswith("authority_") for s in secret_sources(svc)):
            fails.append(f"(d) {name} mounts an authority secret")

    apg_vols = volume_sources(apg)
    for name, svc in services.items():
        if name in AUTHORITY:
            continue
        shared = apg_vols & volume_sources(svc)
        if shared:
            fails.append(f"(e) {name} shares {', '.join(sorted(shared))} with authority-postgres")
    if apg.get("network_mode") != "none":
        fails.append("(e) authority-postgres has a network")
    if "listen_addresses=" not in (apg.get("command") or []):
        fails.append("(e) authority-postgres does not set an empty listen_addresses")
    ep = apg.get("entrypoint") or []
    # `compose config` keeps "$$" escaped; the container's shell sees "$@".
    if len(ep) != 4 or ep[:2] != ["sh", "-c"] or not ep[2].endswith('exec docker-entrypoint.sh "$$@"'):
        fails.append("(e) authority-postgres runs the CP image's entrypoint wrapper")
    elif "chown postgres:postgres /run/authority-pg" not in ep[2] or "chmod 0755 /run/authority-pg" not in ep[2]:
        fails.append("(e) authority-postgres does not hand the socket directory to postgres")
    dirs = next((a.split("=", 1)[1] for a in apg.get("command") or [] if a.startswith("unix_socket_directories=")), "")
    if dirs.split(",") != ["/run/authority-pg", "/var/run/postgresql"]:
        fails.append("(e) authority-postgres does not put its socket in the shared directory first")
    return fails


def main():
    try:
        model = json.load(sys.stdin)
    except json.JSONDecodeError:
        print("FAIL: the rendered model is not JSON")
        return 1
    fails = check(model)
    for f in fails:
        print(f"FAIL: {f}")
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
