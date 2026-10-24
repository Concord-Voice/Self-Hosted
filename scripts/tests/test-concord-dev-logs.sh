#!/usr/bin/env bash
# Tests for verb_logs behavior: service→file mapping, --clear semantics,
# docker delegation. Hermetic via LOG_DIR-env-override sandbox.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CONCORD_DEV="$PROJECT_ROOT/scripts/concord-dev.sh"

# Sandboxed LOG_DIR — concord-dev.sh honors the LOG_DIR env var when set.
export LOG_DIR=$(mktemp -d)
trap 'rm -rf "$LOG_DIR"' EXIT

# Seed log files with sentinel content.
cat > "$LOG_DIR/control-plane.log" <<'EOF'
INFO control-plane started
ERROR sample error line
INFO recovery
EOF
cat > "$LOG_DIR/media-plane.log" <<'EOF'
INFO media-plane started
EOF
cat > "$LOG_DIR/vite-renderer.log" <<'EOF'
vite: ready
EOF
cat > "$LOG_DIR/desktop-client-1.log" <<'EOF'
electron client 1 started
EOF
cat > "$LOG_DIR/desktop-client-2.log" <<'EOF'
electron client 2 started
EOF

# ── Test 1: logs control-plane reads control-plane.log ───────────────
out=$("$CONCORD_DEV" logs control-plane --no-follow -n 5 2>/dev/null)
if grep -q "control-plane started" <<<"$out"; then
  echo "PASS: logs control-plane reads control-plane.log"
else
  echo "FAIL: logs control-plane should read control-plane.log (got: $out)"
  exit 1
fi

# ── Test 2: logs media-plane reads media-plane.log ───────────────────
out=$("$CONCORD_DEV" logs media-plane --no-follow -n 5 2>/dev/null)
if grep -q "media-plane started" <<<"$out"; then
  echo "PASS: logs media-plane reads media-plane.log"
else
  echo "FAIL: logs media-plane should read media-plane.log (got: $out)"
  exit 1
fi

# ── Test 3: logs vite reads vite-renderer.log (CRITICAL CONTRACT) ────
# 'vite' maps to 'vite-renderer.log' (NOT 'vite.log'). A regression
# that flattened the mapping would silently read the wrong file.
out=$("$CONCORD_DEV" logs vite --no-follow -n 5 2>/dev/null)
if grep -q "vite: ready" <<<"$out"; then
  echo "PASS: logs vite maps to vite-renderer.log"
else
  echo "FAIL: logs vite should read vite-renderer.log (got: $out)"
  exit 1
fi

# ── Test 4: logs client defaults to index 1 ──────────────────────────
out=$("$CONCORD_DEV" logs client --no-follow -n 5 2>/dev/null)
if grep -q "electron client 1 started" <<<"$out"; then
  echo "PASS: logs client defaults to desktop-client-1.log"
else
  echo "FAIL: logs client should default to desktop-client-1 (got: $out)"
  exit 1
fi

# ── Test 5: logs client 2 reads desktop-client-2.log ─────────────────
out=$("$CONCORD_DEV" logs client 2 --no-follow -n 5 2>/dev/null)
if grep -q "electron client 2 started" <<<"$out"; then
  echo "PASS: logs client 2 reads desktop-client-2.log"
else
  echo "FAIL: logs client 2 should read desktop-client-2 (got: $out)"
  exit 1
fi

# ── Test 6: logs --errors filters to ERROR/error/FATAL/panic ─────────
out=$("$CONCORD_DEV" logs control-plane --no-follow --errors 2>/dev/null)
if grep -q "ERROR" <<<"$out" && ! grep -q "started" <<<"$out"; then
  echo "PASS: logs --errors filters to ERROR-marked lines only"
else
  echo "FAIL: logs --errors should keep ERROR and drop INFO (got: $out)"
  exit 1
fi

# ── Test 7: logs all reads ALL .log files ────────────────────────────
out=$("$CONCORD_DEV" logs all --no-follow -n 100 2>/dev/null)
if grep -q "control-plane started" <<<"$out" && grep -q "vite: ready" <<<"$out"; then
  echo "PASS: logs all reads all .log files in LOG_DIR"
else
  echo "FAIL: logs all should aggregate all .log files (got: $out)"
  exit 1
fi

# ── Test 8: logs <svc> --clear truncates ONLY that file ──────────────
echo "extra content" >> "$LOG_DIR/control-plane.log"
SIZE_MP_BEFORE=$(wc -c < "$LOG_DIR/media-plane.log")
"$CONCORD_DEV" logs control-plane --no-follow --clear >/dev/null 2>&1
SIZE_CP_AFTER=$(wc -c < "$LOG_DIR/control-plane.log")
SIZE_MP_AFTER=$(wc -c < "$LOG_DIR/media-plane.log")
if [[ $SIZE_CP_AFTER -eq 0 && $SIZE_MP_BEFORE -eq $SIZE_MP_AFTER ]]; then
  echo "PASS: logs <svc> --clear truncates only that file"
else
  echo "FAIL: --clear should truncate only control-plane.log (cp_after=$SIZE_CP_AFTER, mp_before=$SIZE_MP_BEFORE, mp_after=$SIZE_MP_AFTER)"
  exit 1
fi

# ── Test 9: logs all --clear truncates EVERY .log (no all.log) ────────
# Pre-Gitar-fix bug: `> "$LOG_DIR/all.log"` was creating a useless file
# while real log files stayed full. Post-fix: iterates over *.log.
echo "re-seeded" > "$LOG_DIR/control-plane.log"
echo "re-seeded" > "$LOG_DIR/media-plane.log"
"$CONCORD_DEV" logs all --no-follow --clear >/dev/null 2>&1 || true
SIZE_CP_AFTER=$(wc -c < "$LOG_DIR/control-plane.log")
SIZE_MP_AFTER=$(wc -c < "$LOG_DIR/media-plane.log")
ALL_LOG_EXISTS=0
[[ -f "$LOG_DIR/all.log" ]] && ALL_LOG_EXISTS=1
if [[ $SIZE_CP_AFTER -eq 0 && $SIZE_MP_AFTER -eq 0 && $ALL_LOG_EXISTS -eq 0 ]]; then
  echo "PASS: logs all --clear truncates every .log file and creates no all.log"
else
  echo "FAIL: logs all --clear should truncate all .log AND not create all.log (cp=$SIZE_CP_AFTER, mp=$SIZE_MP_AFTER, all.log exists=$ALL_LOG_EXISTS)"
  exit 1
fi

# ── Test 9b: logs vite --clear truncates vite-renderer.log (mapping bug) ─
# Pre-fix bug: --clear used $LOG_DIR/$service.log directly, which for
# service=vite would create/truncate a useless 'vite.log' file while
# 'vite-renderer.log' stayed full. Post-fix: --clear runs AFTER the
# service→log_file mapping so the resolved path is used.
echo "vite content to clear" > "$LOG_DIR/vite-renderer.log"
SIZE_BEFORE=$(wc -c < "$LOG_DIR/vite-renderer.log")
[[ $SIZE_BEFORE -gt 0 ]] || { echo "FAIL: seed failed"; exit 1; }
"$CONCORD_DEV" logs vite --no-follow --clear >/dev/null 2>&1 || true
SIZE_AFTER=$(wc -c < "$LOG_DIR/vite-renderer.log")
VITE_LOG_BUG_EXISTS=0
[[ -f "$LOG_DIR/vite.log" ]] && VITE_LOG_BUG_EXISTS=1
if [[ $SIZE_AFTER -eq 0 && $VITE_LOG_BUG_EXISTS -eq 0 ]]; then
  echo "PASS: logs vite --clear truncates vite-renderer.log (not vite.log)"
else
  echo "FAIL: logs vite --clear should truncate vite-renderer.log AND not create vite.log (after=$SIZE_AFTER, vite.log exists=$VITE_LOG_BUG_EXISTS)"
  exit 1
fi

# ── Test 9c: logs client 2 --clear truncates desktop-client-2.log ────
echo "client 2 content" > "$LOG_DIR/desktop-client-2.log"
SIZE_BEFORE=$(wc -c < "$LOG_DIR/desktop-client-2.log")
[[ $SIZE_BEFORE -gt 0 ]] || { echo "FAIL: seed failed"; exit 1; }
"$CONCORD_DEV" logs client 2 --no-follow --clear >/dev/null 2>&1 || true
SIZE_AFTER=$(wc -c < "$LOG_DIR/desktop-client-2.log")
CLIENT_LOG_BUG_EXISTS=0
[[ -f "$LOG_DIR/client.log" ]] && CLIENT_LOG_BUG_EXISTS=1
if [[ $SIZE_AFTER -eq 0 && $CLIENT_LOG_BUG_EXISTS -eq 0 ]]; then
  echo "PASS: logs client 2 --clear truncates desktop-client-2.log (not client.log)"
else
  echo "FAIL: logs client 2 --clear should truncate desktop-client-2.log AND not create client.log (after=$SIZE_AFTER, client.log exists=$CLIENT_LOG_BUG_EXISTS)"
  exit 1
fi

# ── Test 10: logs docker <svc> delegates to `compose logs` ────────────
# Stub `docker` to record what compose subcommand was invoked.
STUB_DIR=$(mktemp -d)
TRAP_OLD=$(trap -p EXIT)
trap "rm -rf '$LOG_DIR' '$STUB_DIR'" EXIT

cat > "$STUB_DIR/docker" <<'STUB'
#!/usr/bin/env bash
echo "$*" > "$DOCKER_STUB_LOG"
exit 0
STUB
chmod +x "$STUB_DIR/docker"

export DOCKER_STUB_LOG=$(mktemp)
PATH="$STUB_DIR:$PATH" "$CONCORD_DEV" logs docker postgres --no-follow >/dev/null 2>&1 || true
if grep -q "compose .* logs --tail=50 postgres" "$DOCKER_STUB_LOG"; then
  echo "PASS: logs docker <svc> delegates to 'compose logs'"
else
  echo "FAIL: logs docker postgres should call 'compose logs ... postgres' (got: $(cat "$DOCKER_STUB_LOG"))"
  exit 1
fi

rm -f "$DOCKER_STUB_LOG"

echo ""
echo "All logs tests passed."
