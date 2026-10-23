#!/usr/bin/env bash
# test-extract-routes.sh — regression tests for extract-routes.py (#822).
#
# Hermetic: builds a fixture router + spec in a temp dir and asserts the
# parser's load-bearing behaviors, including the path-CONSTANT resolution
# whose absence produced 11 false-stale findings in the first #822 run.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

mkdir -p \
  "$TMP/services/control-plane/internal/api" \
  "$TMP/services/control-plane/internal/admin" \
  "$TMP/services/control-plane/internal/presencehistory" \
  "$TMP/services/control-plane/internal/media" \
  "$TMP/docs/api" \
  "$TMP/scripts/api"
cp scripts/api/extract-routes.py "$TMP/scripts/api/"

cat > "$TMP/services/control-plane/internal/api/router.go" <<'GO'
package api

const (
	pathIDKeys = "/:id/keys"
)

func setup() {
	router.GET("/health", h)
	v1 := router.Group("/api/v1")
	{
		v1.GET("/metrics/current", h) // also exists on admin; --list emits the union
		authRequired := v1.Group("/")
		protected := authRequired.Group("/")
		channels := protected.Group("/channels")
		{
			channels.GET(pathIDKeys, h)          // constant-registered
			channels.DELETE(pathIDKeys+"/:userId", h) // constant + literal concat
			channels.POST("/:id/read", h)        // literal
		}
		auth := v1.Group("/auth")
		{
			auth.POST("/login", h)
		}
		userRoutes := protected.Group("/users")
		presenceHistoryHandler.RegisterRoutes(userRoutes, readLimiter, mutationLimiter)
		mediaRoutes := protected.Group("/media")
		media.RegisterUploadSessionRoutes(mediaRoutes, mediaHandler, redis)
	}
}
GO

cat > "$TMP/services/control-plane/internal/admin/routes.go" <<'GO'
package admin

func RegisterRoutes(rg *gin.RouterGroup) {
	grp := rg.Group("/admin")
	api := grp.Group("/api/v1")
	authRoutes := api.Group("/auth")
	authRoutes.POST("/password", auth.Password)
	gated := api.Group("/")
	metricsRoutes := gated.Group("")
	metricsRoutes.GET("/health", metrics.Health)
	metricsRoutes.GET("/metrics/current", metrics.Current)
	metricsRoutes.GET("/metrics/series", metrics.Series)
	metricsRoutes.GET("/counters", metrics.Counters)
	metricsRoutes.Match([]string{"POST", "PUT", "PATCH", "DELETE"}, "/health", metrics.MethodNotAllowed)
}
GO

cat > "$TMP/services/control-plane/internal/presencehistory/handler.go" <<'GO'
package presencehistory

func (h *Handler) RegisterRoutes(users *gin.RouterGroup, readLimiter, mutationLimiter gin.HandlerFunc) {
	history := users.Group("/me/presence-history")
	history.GET("/settings", readLimiter, h.getSettings)
	history.PATCH("/settings", mutationLimiter, h.updateSettings)
	history.GET("", readLimiter, h.list)
	history.DELETE("", mutationLimiter, h.disableAndDelete)
}
GO

# A PACKAGE-LEVEL registrar, not a method. The extractor was blind to this shape
# until #2157 PR 2, which made four live routes read as stale spec entries -- and
# meant the gate could never have caught them going unregistered either.
cat > "$TMP/services/control-plane/internal/media/upload_session.go" <<'GO'
package media

const (
	RouteUploadSession      = "/upload/attachment/session"
	RouteUploadSessionChunk = "/upload/attachment/session/:session_id/chunk/:index"
)

func RegisterUploadSessionRoutes(group *gin.RouterGroup, h *Handler, rdb *redis.Client) {
	group.POST(RouteUploadSession, limiter, h.InitUploadSession)
	group.PUT(RouteUploadSessionChunk, limiter, h.PutUploadChunk)
}
GO

cat > "$TMP/docs/api/openapi.yaml" <<'YAML'
openapi: 3.0.3
servers:
  - url: http://localhost:8080/api/v1
paths:
  /channels/{id}/keys:
    get:
      summary: ok
  /auth/login:
    post:
      summary: ok
  /metrics/current:
    get:
      summary: public operation with the same relative method and path
  /ghost/route:
    delete:
      summary: stale entry
  /users/me/presence-history:
    get:
      summary: ok
    delete:
      summary: ok
  /users/me/presence-history/settings:
    get:
      summary: ok
    patch:
      summary: ok
  /media/upload/attachment/session:
    post:
      summary: registered by a package-level delegated registrar
  /media/upload/attachment/session/{session_id}/chunk/{index}:
    put:
      summary: constant-registered inside a package-level delegated registrar
YAML

cat > "$TMP/docs/api/admin-metrics.openapi.yaml" <<'YAML'
openapi: 3.0.3
servers:
  - url: https://admin.example.test/admin/api/v1
paths:
  /health:
    get:
      summary: ok
  /metrics/current:
    get:
      summary: ok
  /metrics/series:
    get:
      summary: ok
  /counters:
    get:
      summary: ok
YAML

cd "$TMP"
fail() {
  echo "FAIL: $1" >&2
  exit 1
}

# 1. Constant-registered route is in the live inventory (not stale).
python3 scripts/api/extract-routes.py --list | grep -q "GET /channels/{id}/keys" \
  || fail "constant-registered route missing from inventory"

# 2. Group("/") nesting does not produce duplicate slashes.
python3 scripts/api/extract-routes.py --list | grep -q "//" \
  && fail "duplicate slashes in normalized paths" || true

# 2b. Constant+literal concatenation resolves (the third registration form).
python3 scripts/api/extract-routes.py --list | grep -q "DELETE /channels/{id}/keys/{userId}" \
  || fail "concat-registered route missing from inventory"

# 2c. --list is the deduplicated union of supported operations only.
python3 scripts/api/extract-routes.py --list > supported-routes
for route in \
  "GET /health" \
  "GET /metrics/current" \
  "GET /metrics/series" \
  "GET /counters"; do
  grep -Fxq "$route" supported-routes \
    || fail "admin metrics route missing from inventory: $route"
done
[[ $(grep -Fxc "GET /metrics/current" supported-routes) -eq 1 ]] \
  || fail "--list must deduplicate operations shared across surfaces"
if grep -Fxq "POST /auth/password" supported-routes; then
  fail "non-metrics admin route must not become a supported operation"
fi
if grep -Fxq "POST /health" supported-routes; then
  fail "admin method-denial registration must not become a supported operation"
fi

# 2d. A delegated registrar must be declared or extraction fails loudly.
sed -i.bak '/presenceHistoryHandler.RegisterRoutes/a\
\t\tunknownHandler.RegisterRoutes(userRoutes, readLimiter)' \
  services/control-plane/internal/api/router.go
set +e
python3 scripts/api/extract-routes.py --list > /dev/null 2> unknown-delegate.err
status=$?
set -e
[[ $status -eq 10 ]] || fail "unknown delegated registrar should exit 10 (got $status)"
grep -q "unknown delegated registrar" unknown-delegate.err \
  || fail "unknown delegated registrar error was not actionable"
mv services/control-plane/internal/api/router.go.bak \
  services/control-plane/internal/api/router.go

# 2d-bis. A PACKAGE-LEVEL registrar resolves too, and its absence fails loudly.
#
# The extractor understood only the METHOD form (h.RegisterRoutes) until #2157
# PR 2. Package-function registrars were invisible, so their live routes read as
# stale spec entries -- and, far worse, the gate could never have noticed those
# routes going unregistered, which is precisely the bug that shipped.
python3 scripts/api/extract-routes.py --list > pkg-registrar-routes
for route in \
  "POST /media/upload/attachment/session" \
  "PUT /media/upload/attachment/session/{session_id}/chunk/{index}"; do
  grep -Fxq "$route" pkg-registrar-routes \
    || fail "package-registrar route missing from inventory: $route"
done

# Unmounting it must be a hard error, not a silent shrink of the inventory. This
# is what makes the declaration double as a wiring guard.
sed -i.bak '/media.RegisterUploadSessionRoutes/d' \
  services/control-plane/internal/api/router.go
set +e
python3 scripts/api/extract-routes.py --list > /dev/null 2> unmounted-pkg.err
status=$?
set -e
[[ $status -eq 10 ]] || fail "unmounted package registrar should exit 10 (got $status)"
grep -q "declared delegated registrar is not mounted: media.RegisterUploadSessionRoutes" unmounted-pkg.err \
  || fail "unmounted package registrar error did not name the registrar"
mv services/control-plane/internal/api/router.go.bak \
  services/control-plane/internal/api/router.go

# 2e. Explicitly inventoried delegated registrations resolve from their router mount.
python3 scripts/api/extract-routes.py --list > live-routes
python3 scripts/api/extract-routes.py --stale > stale-routes
for route in \
  "GET /users/me/presence-history/settings" \
  "PATCH /users/me/presence-history/settings" \
  "GET /users/me/presence-history" \
  "DELETE /users/me/presence-history"; do
  grep -Fxq "$route" live-routes \
    || fail "delegated route missing from inventory: $route"
  grep -Fxq "[public] $route" stale-routes \
    && fail "delegated route incorrectly reported stale: $route" || true
done

# 3. Missing detection: the literal /channels/{id}/read is undocumented.
python3 scripts/api/extract-routes.py --missing | grep -q "POST /channels/{id}/read" \
  || fail "missing route not detected"

# 4. Stale detection: /ghost/route is documented but unregistered.
python3 scripts/api/extract-routes.py --stale | grep -q "DELETE /ghost/route" \
  || fail "stale route not detected"

# 5. A colliding public operation cannot hide drift in the admin metrics spec.
cp docs/api/admin-metrics.openapi.yaml docs/api/admin-metrics.openapi.yaml.bak
python3 - <<'PY'
from pathlib import Path

path = Path("docs/api/admin-metrics.openapi.yaml")
block = """  /metrics/current:
    get:
      summary: ok
"""
text = path.read_text()
assert block in text
path.write_text(text.replace(block, ""))
PY
if python3 scripts/api/extract-routes.py --check > admin-drift.out 2>&1; then
  fail "--check should fail when only the admin metrics spec drifts"
fi
grep -Fq "[admin-metrics] GET /metrics/current" admin-drift.out \
  || fail "admin metrics drift was not attributed to its own surface"
mv docs/api/admin-metrics.openapi.yaml.bak docs/api/admin-metrics.openapi.yaml

# 6. Public /health is exempt; documented admin metrics /health is clean.
python3 scripts/api/extract-routes.py --missing | grep -q "/health" \
  && fail "/health should be exempt" || true

# 6b. INFRA_ROUTES is an exact-match SET, not a prefix or regex (#3106).
# Probed directly against to_spec_relative rather than through --missing,
# because the negative cases are routes the router does not register, so
# --missing could never report them and the assertion would be vacuous.
python3 - <<'INFRA' || fail "INFRA_ROUTES exemption is not an exact-match set"
import importlib.util, sys
spec = importlib.util.spec_from_file_location("er", "scripts/api/extract-routes.py")
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)

# Every member is exempt.
for path in ("/health", "/readyz"):
    if m.to_spec_relative("GET", path) is not None:
        print(f"{path} must be exempt from the spec-coverage contract", file=sys.stderr)
        sys.exit(1)

# NEGATIVE CONTROLS. Without these the suite passes for an implementation
# that exempts everything, or one that exempts by prefix -- either of which
# would silently drop a future real route out of coverage.
for path in ("/readyzz", "/health/detail", "/readyz/sub"):
    if m.to_spec_relative("GET", path) is None:
        print(f"{path} is NOT an infra route and must stay in coverage", file=sys.stderr)
        sys.exit(1)
INFRA

# 7. --check exits 1 on drift, 0 when both surfaces are clean.
python3 scripts/api/extract-routes.py --check > /dev/null && fail "--check should fail on drift"
python3 - <<'PY'
s = open("docs/api/openapi.yaml").read()
s = s.replace("""  /ghost/route:
    delete:
      summary: stale entry
""", """  /channels/{id}/read:
    post:
      summary: ok
  /channels/{id}/keys/{userId}:
    delete:
      summary: ok
""")
open("docs/api/openapi.yaml", "w").write(s)
PY
python3 scripts/api/extract-routes.py --check > /dev/null || fail "--check should pass when clean"

# 8. Placeholder names are labels, while methods and path shape stay strict.
# Exercise the CLI with both PyYAML and the no-site-packages fallback. Inventories
# and diagnostics retain their source names even when coverage matches by shape.
python3 - <<'PLACEHOLDERS' || fail "placeholder coverage regression"
from pathlib import Path
import subprocess
import sys

router = Path("services/control-plane/internal/api/router.go")
router.write_text(router.read_text().replace(
    '\t\tauth := v1.Group("/auth")',
    '\t\tprotected.GET("/friends/codes/:code", h)\n'
    '\t\tprotected.DELETE("/friends/codes/:id", h)\n'
    '\t\tprotected.GET("/pairs/:first/links/:second", h)\n'
    '\t\tauth := v1.Group("/auth")',
))
spec_path = Path("docs/api/openapi.yaml")
clean_spec = spec_path.read_text() + """  /friends/codes/{code}:
    get:
      summary: lookup by short code
    delete:
      summary: revoke by UUID
"""
expected_live = {
    "GET /friends/codes/{code}",
    "DELETE /friends/codes/{id}",
    "GET /pairs/{first}/links/{second}",
}
failures = []
for parser, options in (("PyYAML", []), ("fallback", ["-S"])):
    def run(mode):
        return subprocess.run(
            [sys.executable, *options, "scripts/api/extract-routes.py", mode],
            text=True, capture_output=True, check=False,
        )

    spec_path.write_text(clean_spec + """  /pairs/{left}/links/{right}:
    get:
      summary: both placeholder labels vary
""")
    result = run("--check")
    if result.returncode != 0 or "[OK] surface specs cover all" not in result.stdout:
        failures.append(f"{parser}: equivalent placeholder labels falsely report drift\n{result.stdout}{result.stderr}")
    else:
        print(f"PASS: {parser} equivalent placeholder labels")
    inventory = run("--list")
    actual_live = {
        line for line in inventory.stdout.splitlines()
        if " /friends/codes/" in line or " /pairs/" in line
    }
    if inventory.returncode != 0 or actual_live != expected_live:
        failures.append(f"{parser}: --list lost original Gin parameter names\n{inventory.stdout}{inventory.stderr}")
    for mode, noun in (("--missing", "missing"), ("--stale", "stale")):
        result = run(mode)
        if result.returncode != 0 or result.stdout or f"# 0 {noun} of " not in result.stderr:
            failures.append(f"{parser}: {mode} falsely reports renamed placeholders\n{result.stdout}{result.stderr}")

    # Literal expected tuples make over-broad matching and normalized error text
    # fail independently of the helper that production uses for comparison.
    for label, path, method, expected_stale in (
        ("different HTTP method", "/pairs/{left}/links/{right}", "post", "POST /pairs/{left}/links/{right}"),
        ("different literal segment", "/groups/{left}/links/{right}", "get", "GET /groups/{left}/links/{right}"),
        ("different placeholder position", "/pairs/links/{left}/{right}", "get", "GET /pairs/links/{left}/{right}"),
        ("different segment count", "/pairs/{left}/links/{right}/extra", "get", "GET /pairs/{left}/links/{right}/extra"),
    ):
        spec_path.write_text(clean_spec + f"  {path}:\n    {method}:\n      summary: deliberate drift\n")
        result = run("--check")
        expected_missing = "[public] GET /pairs/{first}/links/{second} -> docs/api/openapi.yaml"
        expected_stale_line = f"[public] {expected_stale} <- docs/api/openapi.yaml"
        if (result.returncode != 1 or expected_missing not in result.stdout
                or expected_stale_line not in result.stdout):
            failures.append(f"{parser}: {label} was hidden or lost its original names/surface\n{result.stdout}{result.stderr}")
        else:
            print(f"PASS: {parser} rejects {label} with missing/stale diagnostics")

if failures:
    print("\n".join(failures), file=sys.stderr)
    sys.exit(1)
PLACEHOLDERS

echo "All extract-routes tests passed."
