#!/usr/bin/env python3
"""extract-routes.py — OpenAPI-covered operation inventory + drift check (#822).

Parses supported gin route registrations for the public and admin metrics
surfaces, then compares each resulting (METHOD, /path) inventory against its
own OpenAPI specification. Public delegated registrars track
`X := Y.Group("prefix")` bindings, including nesting.

Gin path syntax is normalized to OpenAPI: `:param` -> `{param}`, `*param` -> `{param}`.
Conditional duplicate registrations (e.g. the media available/unavailable branches)
dedupe naturally.

Modes:
  --list            print the OpenAPI-covered live operation inventory
  --missing         print routes present in the router but absent from the spec
  --stale           print spec routes absent from the router
  --check           exit 1 if any missing or stale routes exist (CI gate mode)

The CI gate wrapper is scripts/api/check-openapi-coverage.sh.
"""

import re
import sys
from pathlib import Path

ROUTER = "services/control-plane/internal/api/router.go"
SPEC = "docs/api/openapi.yaml"
ADMIN_METRICS_ROUTER = "services/control-plane/internal/admin/routes.go"
ADMIN_METRICS_SPEC = "docs/api/admin-metrics.openapi.yaml"
DELEGATED_REGISTRARS = {
    "presenceHistoryHandler": (
        "services/control-plane/internal/presencehistory/handler.go",
        "users",
    ),
}

# Package-level registrar functions, keyed by the exact `pkg.Func` call text.
# Same contract as DELEGATED_REGISTRARS: value is (source file, the name the
# mounted group is called INSIDE that file). Declaring one here is load-bearing
# in both directions -- an entry whose call is absent from router.go is a hard
# error, so this doubles as a wiring guard for the routes it covers.
DELEGATED_FUNC_REGISTRARS = {
    "media.RegisterUploadSessionRoutes": (
        "services/control-plane/internal/media/upload_session.go",
        "group",
    ),
}

GROUP_RE = re.compile(r'^\s*(\w+)\s*:?=\s*(\w+)\.Group\(\s*"([^"]*)"')
# Routes register with either a string literal or a named Go constant
# (e.g. `pathIDKeys = "/:id/keys"` then `channels.GET(pathIDKeys, ...)`) —
# the constant form caused 11 false-stale findings in the first run (#822).
# HEAD/OPTIONS parse through the normal pipeline but are EXEMPT from the
# spec-coverage contract (the spec documents the five primary verbs; gin's
# router.HEAD("/health") is infra, found by the fail-loud guard the moment
# it landed). Any/Handle/computed forms still hit the guard.
ROUTE_RE = re.compile(r'^\s*(\w+)\.(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\(\s*"([^"]*)"')
ROUTE_CONST_RE = re.compile(r'^\s*(\w+)\.(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\(\s*(\w+)\s*[,)]')
# Constant + literal concatenation: `dm.DELETE(pathIDMembers+"/:userId", ...)`
# — the third registration form (#822; two DM member routes were invisible).
ROUTE_CONCAT_RE = re.compile(r'^\s*(\w+)\.(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\(\s*(\w+)\s*\+\s*"([^"]*)"')
CONST_RE = re.compile(r'^\s*(\w+)\s*=\s*"([^"]*)"\s*(?://.*)?$')
# Guard net: any HTTP-verb-shaped call (incl. verbs/forms the resolvers don't
# handle: HEAD, OPTIONS, Any, Handle, computed paths) on a known group var.
UNPARSED_RE = re.compile(r'^\s*(\w+)\.(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|Any|Handle)\(')
# Two delegated-registration shapes. The method form (`h.RegisterRoutes(group, ...)`)
# came first; the package-function form (`media.RegisterUploadSessionRoutes(group, ...)`)
# was invisible to this extractor until #2157 PR 2, which meant the four chunked
# upload session routes read as STALE spec entries -- documented, live, and
# reported as dead. Worse, the same blindness meant this gate could never have
# caught those routes going UNREGISTERED either, which is exactly what happened.
DELEGATED_CALL_RE = re.compile(r'(?m)^\s*(\w+)\.RegisterRoutes\(')
DELEGATED_FUNC_CALL_RE = re.compile(r'(?m)^\s*(\w+\.\w+)\(')


API_BASE = "/api/v1"  # the spec's servers: URLs carry this prefix; spec paths are relative to it

# Infrastructure probes exempt from the spec-coverage contract (#3106).
# ENUMERATED, deliberately not a prefix or regex: a pattern is an open hole
# through which a future real route escapes coverage silently, whereas a set
# makes every addition a visible one-line diff. A future /livez adds a string.
# /voice-join-pause is concord-ctl.sh's loopback proof of the legacy-conversion
# join pause; nginx never proxies it.
INFRA_ROUTES = frozenset({"/health", "/readyz", "/voice-join-pause"})
ADMIN_METRICS_BASE = "/admin/api/v1"


def gin_to_openapi(path: str) -> str:
    path = re.sub(r":(\w+)", r"{\1}", path)
    path = re.sub(r"\*(\w+)", r"{\1}", path)
    path = re.sub(r"/{2,}", "/", path)  # Group("/") nesting yields // — collapse
    return path


def to_spec_relative(
    method: str,
    path: str,
    api_base: str = API_BASE,
) -> tuple[str, str] | None:
    """Map a live route into its spec's server-relative path space.
    Exempt from the spec-coverage contract (return None): infrastructure
    routes (INFRA_ROUTES) and HEAD/OPTIONS operations."""
    if method in ("HEAD", "OPTIONS"):
        return None
    if path in INFRA_ROUTES and api_base == API_BASE:
        return None
    if path.startswith(api_base + "/"):
        return (method, path[len(api_base):])
    if path == api_base:
        return (method, "/")
    # Unknown prefix (e.g. a future /api/v2): keep it in the inventory
    # un-rebased so --check fails loudly, and say why.
    print(f"[WARN] route outside {api_base}: {method} {path} — compared un-rebased; "
          f"extend surface base handling if a new base is introduced", file=sys.stderr)
    return (method, path)


def parse_route_source(
    source_path: str,
    initial_prefixes: dict[str, str],
    api_base: str = API_BASE,
    route_variables: set[str] | None = None,
) -> tuple[set[tuple[str, str]], dict[str, str]]:
    prefixes = dict(initial_prefixes)
    consts: dict[str, str] = {}
    routes: set[tuple[str, str]] = set()
    unparsed: list[str] = []
    with open(source_path) as f:
        for line in f:
            m = GROUP_RE.match(line)
            if m:
                var, parent, prefix = m.groups()
                if parent in prefixes:
                    prefixes[var] = prefixes[parent] + prefix
                continue
            m = CONST_RE.match(line)
            if m and m.group(2).startswith("/"):
                consts[m.group(1)] = m.group(2)
                continue
            m = ROUTE_RE.match(line)
            if not m:
                mcc = ROUTE_CONCAT_RE.match(line)
                if mcc and mcc.group(3) in consts:
                    var, method = mcc.group(1), mcc.group(2)
                    if route_variables is not None and var not in route_variables:
                        continue
                    path = consts[mcc.group(3)] + mcc.group(4)
                    base = prefixes.get(var)
                    if base is None:
                        print(f"[WARN] route on unknown group variable: {line.strip()}", file=sys.stderr)
                        continue
                    rel = to_spec_relative(method, gin_to_openapi(base + path), api_base)
                    if rel is not None:
                        routes.add(rel)
                    continue
                mc = ROUTE_CONST_RE.match(line)
                if mc and mc.group(3) in consts:
                    m = None
                    var, method, path = mc.group(1), mc.group(2), consts[mc.group(3)]
                    if route_variables is not None and var not in route_variables:
                        continue
                    base = prefixes.get(var)
                    if base is None:
                        print(f"[WARN] route on unknown group variable: {line.strip()}", file=sys.stderr)
                        continue
                    rel = to_spec_relative(method, gin_to_openapi(base + path), api_base)
                    if rel is not None:
                        routes.add(rel)
                    continue
                elif mc:
                    print(f"[WARN] route with unresolved path constant: {line.strip()}", file=sys.stderr)
                    continue
            if m:
                var, method, path = m.groups()
                if route_variables is not None and var not in route_variables:
                    continue
                base = prefixes.get(var)
                if base is None:
                    # Route on an unknown receiver (e.g. a sub-engine) — surface loudly
                    # rather than silently dropping coverage.
                    print(f"[WARN] route on unknown group variable: {line.strip()}", file=sys.stderr)
                    continue
                full = base + path
                rel = to_spec_relative(method, gin_to_openapi(full), api_base)
                if rel is not None:
                    routes.add(rel)
                continue
            # Fail-loud guard: ANY other registration-looking call on a known
            # group variable means a form this parser can't see — a silently
            # dropped route is a silent DAST/coverage gap (Gitar, PR #1496).
            mu = UNPARSED_RE.match(line)
            if (mu and mu.group(1) in prefixes and
                    (route_variables is None or mu.group(1) in route_variables)):
                print(f"[ERROR] unparseable route registration (extend extract-routes.py): "
                      f"{line.strip()}", file=sys.stderr)
                unparsed.append(line.strip())
    if unparsed:
        sys.exit(10)
    return routes, prefixes


def live_routes(router_path: str = ROUTER) -> set[tuple[str, str]]:
    routes, prefixes = parse_route_source(router_path, {"router": ""})
    router_source = Path(router_path).read_text()
    seen: set[str] = set()
    errors: list[str] = []

    for call in DELEGATED_CALL_RE.finditer(router_source):
        receiver = call.group(1)
        registration = DELEGATED_REGISTRARS.get(receiver)
        if registration is None:
            errors.append(f"unknown delegated registrar: {receiver}.RegisterRoutes")
            continue
        seen.add(receiver)

        argument = re.match(r"\s*(\w+)\s*,", router_source[call.end():])
        if argument is None:
            errors.append(f"unresolved first argument for delegated registrar: {receiver}.RegisterRoutes")
            continue
        mount_var = argument.group(1)
        mount_prefix = prefixes.get(mount_var)
        if mount_prefix is None:
            errors.append(
                f"unknown router group {mount_var!r} for delegated registrar: "
                f"{receiver}.RegisterRoutes"
            )
            continue

        source_path, source_group = registration
        if not Path(source_path).is_file():
            errors.append(f"missing delegated registrar source: {source_path}")
            continue
        delegated, _ = parse_route_source(source_path, {source_group: mount_prefix})
        routes.update(delegated)

    seen_funcs: set[str] = set()
    for call in DELEGATED_FUNC_CALL_RE.finditer(router_source):
        name = call.group(1)
        registration = DELEGATED_FUNC_REGISTRARS.get(name)
        if registration is None:
            # Unknown pkg.Func( calls are ordinary code, not registrars. Unlike
            # the method form there is no naming convention to key off, so this
            # cannot fail loud on an unlisted one -- which is why the reverse
            # check below matters more here.
            continue
        seen_funcs.add(name)

        argument = re.match(r"\s*(\w+)\s*,", router_source[call.end():])
        if argument is None:
            errors.append(f"unresolved first argument for delegated registrar: {name}")
            continue
        mount_prefix = prefixes.get(argument.group(1))
        if mount_prefix is None:
            errors.append(
                f"unknown router group {argument.group(1)!r} for delegated registrar: {name}"
            )
            continue

        source_path, source_group = registration
        if not Path(source_path).is_file():
            errors.append(f"missing delegated registrar source: {source_path}")
            continue
        delegated, _ = parse_route_source(source_path, {source_group: mount_prefix})
        routes.update(delegated)

    for receiver in DELEGATED_REGISTRARS.keys() - seen:
        errors.append(f"declared delegated registrar is not mounted: {receiver}.RegisterRoutes")
    for name in DELEGATED_FUNC_REGISTRARS.keys() - seen_funcs:
        errors.append(f"declared delegated registrar is not mounted: {name}")

    if errors:
        for error in errors:
            print(f"[ERROR] {error}", file=sys.stderr)
        sys.exit(10)
    return routes


def admin_metrics_live_routes(
    router_path: str = ADMIN_METRICS_ROUTER,
) -> set[tuple[str, str]]:
    """Inventory only supported admin metrics operations.

    The registrar also contains pre-existing admin-auth routes and explicit
    Match registrations that enforce 405 for mutating verbs. Those are separate
    contracts; only GET calls on metricsRoutes belong to this surface.
    """
    routes, _ = parse_route_source(
        router_path,
        {"rg": ""},
        api_base=ADMIN_METRICS_BASE,
        route_variables={"metricsRoutes"},
    )
    return routes


def spec_routes(spec_path: str = SPEC) -> set[tuple[str, str]]:
    """Spec-side inventory. Prefers PyYAML when available (indentation-proof);
    falls back to a minimal hand-parse matching assemble-spec.py's dump style
    (2-space paths / 4-space methods) so CI has no hard YAML-lib dependency."""
    try:
        import yaml  # noqa: PLC0415

        with open(spec_path) as f:
            spec = yaml.safe_load(f)
        methods = {"get", "post", "put", "patch", "delete"}
        return {
            (m.upper(), p)
            for p, item in (spec.get("paths") or {}).items()
            for m in item
            if m in methods
        }
    except ImportError:
        pass
    routes: set[tuple[str, str]] = set()
    in_paths = False
    current = None
    methods = {"get", "post", "put", "patch", "delete"}
    with open(spec_path) as f:
        for raw in f:
            line = raw.rstrip("\n")
            if re.match(r"^paths:\s*$", line):
                in_paths = True
                continue
            if in_paths and re.match(r"^\S", line):
                break  # left the paths block
            if not in_paths:
                continue
            m = re.match(r"^  (/[^\s:]*):\s*$", line)
            if m:
                current = m.group(1)
                continue
            m = re.match(r"^    (\w+):\s*$", line)
            if m and current and m.group(1) in methods:
                routes.add((m.group(1).upper(), current))
    return routes


def unmatched_operations(
    source: set[tuple[str, str]], peer: set[tuple[str, str]]
) -> set[tuple[str, str]]:
    """Compare wire paths while retaining source names in drift diagnostics."""
    peer_keys = {
        (method, re.sub(r"\{[^/{}]+\}", "{}", path)) for method, path in peer
    }
    return {
        (method, path)
        for method, path in source
        if (method, re.sub(r"\{[^/{}]+\}", "{}", path)) not in peer_keys
    }


def main() -> int:
    mode = sys.argv[1] if len(sys.argv) > 1 else "--check"
    surfaces = [
        ("public", SPEC, live_routes(), spec_routes(SPEC)),
        (
            "admin-metrics",
            ADMIN_METRICS_SPEC,
            admin_metrics_live_routes(),
            spec_routes(ADMIN_METRICS_SPEC),
        ),
    ]
    missing = [
        (surface, spec_path, method, path)
        for surface, spec_path, live, spec in surfaces
        for method, path in sorted(unmatched_operations(live, spec))
    ]
    stale = [
        (surface, spec_path, method, path)
        for surface, spec_path, live, spec in surfaces
        for method, path in sorted(unmatched_operations(spec, live))
    ]
    live_count = sum(len(live) for _, _, live, _ in surfaces)
    spec_count = sum(len(spec) for _, _, _, spec in surfaces)

    if mode == "--list":
        supported = set().union(*(live for _, _, live, _ in surfaces))
        for method, path in sorted(supported):
            print(f"{method} {path}")
        print(
            f"# {len(supported)} OpenAPI-covered live operations across "
            f"{len(surfaces)} surfaces",
            file=sys.stderr,
        )
        return 0
    if mode == "--missing":
        for surface, _, method, path in missing:
            print(f"[{surface}] {method} {path}")
        print(f"# {len(missing)} missing of {live_count} live", file=sys.stderr)
        return 0
    if mode == "--stale":
        for surface, _, method, path in stale:
            print(f"[{surface}] {method} {path}")
        print(f"# {len(stale)} stale of {spec_count} documented", file=sys.stderr)
        return 0
    if mode == "--check":
        ok = True
        if missing:
            print(f"[FAIL] {len(missing)} live route(s) missing from their surface specs:")
            for surface, spec_path, method, path in missing:
                print(f"  [{surface}] {method} {path} -> {spec_path}")
            ok = False
        if stale:
            print(f"[FAIL] {len(stale)} documented route(s) no longer exist in the router:")
            for surface, spec_path, method, path in stale:
                print(f"  [{surface}] {method} {path} <- {spec_path}")
            ok = False
        if ok:
            print(
                f"[OK] surface specs cover all {live_count} OpenAPI-covered live operations, "
                "no stale entries"
            )
            return 0
        return 1
    print(f"unknown mode: {mode}", file=sys.stderr)
    return 10


if __name__ == "__main__":
    sys.exit(main())
