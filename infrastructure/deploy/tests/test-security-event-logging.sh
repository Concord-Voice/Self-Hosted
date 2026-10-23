#!/usr/bin/env bash
# #2506 narrow executable seam; all privileged identities are virtual in owned copies.
set -euo pipefail
BOUNDARY_ONLY=false
SOURCE_ROOT=""
PUBLICATION_ONLY=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --nightwatch-boundary-only) BOUNDARY_ONLY=true; shift ;;
    --publication-boundary-only) PUBLICATION_ONLY=true; shift ;;
    --source-root)
      [[ $# -ge 2 && -n "$2" ]] || { printf 'Test source root requires a path\n' >&2; exit 2; }
      SOURCE_ROOT=$2; shift 2 ;;
    *) printf 'Unsupported test selector\n' >&2; exit 2 ;;
  esac
done
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
SOURCE_ROOT="${SOURCE_ROOT:-$ROOT}"
if [[ "$PUBLICATION_ONLY" == true ]]; then
  python3 "$(dirname "${BASH_SOURCE[0]}")/security_event_publication_test.py" "$SOURCE_ROOT"
  exit $?
fi
# Test-only source selection lets ROOT replay saved production bytes. No path
# override is added to either production entrypoint.
HELPER="$SOURCE_ROOT/infrastructure/deploy/selfhost-security-event-logging.sh"
PROVISION_SOURCE="$SOURCE_ROOT/[internal]provision-production.sh"
TMP="$(mktemp -d)"
trap '/bin/rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'PASS: %s\n' "$*"; }
[[ -f "$HELPER" ]] || fail "public security-event helper exists independently of managed provisioner"
REAL_PYTHON="$(command -v python3)"
export REAL_PYTHON
# Managed provisioning consumes the same adjacent helper from the deployed tree.
# Keep this delivery contract in the feature suite, before any host-fixture work;
# the generic manifest lint deliberately cannot infer composed shell paths.
"$REAL_PYTHON" - "$ROOT" "$PROVISION_SOURCE" <<'PY'
from pathlib import Path
import sys

root = Path(sys.argv[1])
helper = 'infrastructure/deploy/selfhost-security-event-logging.sh'
entries = [line.split('#', 1)[0].strip().rstrip('/')
           for line in (root / '[internal]deploy-manifest.txt').read_text().splitlines()]
covered = any(entry and (helper == entry or helper.startswith(entry + '/'))
              for entry in entries)
if not covered:
    raise SystemExit('FAIL: managed security-event helper must be shipped by exact or parent deploy-manifest entry')
managed = Path(sys.argv[2]).read_text()
required = ('check_security_event_logging_source()',
            '[[ -f "${SCRIPT_DIR}/selfhost-security-event-logging.sh"',
            'source "${SCRIPT_DIR}/selfhost-security-event-logging.sh"')
if not all(fragment in managed for fragment in required):
    raise SystemExit('FAIL: managed provisioner must still require and read the manifested adjacent security-event helper')
print('PASS: managed security-event helper delivery and consumer contract')
PY
mkdir -p "$TMP/bin" "$TMP/copy"
export FIXTURE_ROOT="$TMP/host" FIXTURE_GROUPS="$TMP/groups" FIXTURE_CALLS="$TMP/calls" FIXTURE_META="$TMP/meta.json"
printf '{}\n' >"$FIXTURE_META"
: >"$FIXTURE_GROUPS"; : >"$FIXTURE_CALLS"
# Patch pinned paths and EUID only in the disposable copy. No public override is added.
"$REAL_PYTHON" - "$HELPER" "$PROVISION_SOURCE" "$TMP/copy" "$FIXTURE_ROOT" <<'PY'
from pathlib import Path
import re, sys
helper, managed, dest, root = sys.argv[1:]
for source in (helper, managed):
    text = Path(source).read_text()
    for old, new in (('/var/log/concord-security', root+'/logs'),
                     ('/etc/logrotate.d/concord-security', root+'/etc/concord-security'),
                     ('/etc/systemd/system', root+'/systemd'),
                     ('/var/lib/nightwatch/normalizer/commissioned', root+'/normalizer/commissioned'),
                     ('/usr/local/lib/concord/selfhost-security-event-logging.sh', root+'/approved-lib/selfhost-security-event-logging.sh')):
        text = text.replace(old, new)
    text = re.sub(r'\$\{EUID\}|\$EUID', '${FIXTURE_EUID:-0}', text)
    # Managed trust-boundary routes use absolute executables. Redirect their
    # privileged effects too, so a missing guard cannot touch the real host.
    if source == managed:
        bindir = str(Path(dest).parent / 'bin')
        for executable in ('/usr/bin/install', '/usr/bin/chown', '/usr/bin/chmod',
                           '/usr/bin/mv', '/usr/bin/ln', '/bin/rm', '/usr/bin/id',
                           '/usr/sbin/visudo'):
            text = text.replace(executable, bindir + '/' + Path(executable).name)

    # If privilege checks change shape, fail rather than accidentally test as real root.
    assert '${FIXTURE_EUID:-0}' in text, 'fixture must replace an EUID root guard'
    Path(dest, Path(source).name).write_text(text)
PY
# These stubs reject unsupported host operations and require every path to be owned.
cat >"$TMP/bin/host-fixture.py" <<'PY'
import configparser, json, os, pathlib, shutil, stat, sys
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
root = pathlib.Path(os.environ['FIXTURE_ROOT'])
meta_path = pathlib.Path(os.environ['FIXTURE_META'])
meta = json.loads(meta_path.read_text())
def owned(value):
    path = pathlib.Path(value)
    if not path.is_absolute() or root not in path.parents and path != root:
        raise RuntimeError('fixture rejected non-owned host path')
    return path
with open(os.environ['FIXTURE_CALLS'], 'a') as out:
    out.write(name+' '+ ' '.join(args)+'\n')
if name == 'getent':
    if args == ['group']: print(pathlib.Path(os.environ['FIXTURE_GROUPS']).read_text(), end='')
    elif args == ['passwd']: print(os.environ.get('FIXTURE_USERS', ''), end='')
    else: raise RuntimeError('unexpected NSS query')
elif name == 'groupadd':
    assert args == ['--system', '--gid', '10001', 'concord-security-writers']
    with open(os.environ['FIXTURE_GROUPS'], 'a') as out: out.write('concord-security-writers:x:10001:\n')
elif name == 'systemctl':
    assert args in (['daemon-reload'], ['enable', '--now', 'concord-security-logrotate.timer'])
elif name == 'install':
    assert args[:2] == ['-d', '-o'] and args[2] == 'root' and args[3] == '-g' and args[5] == '-m' and len(args) == 8
    gid = {'root':0, 'concord-security-writers':10001}[args[4]]
    path = owned(args[7]); path.mkdir(parents=True, exist_ok=True)
    mode = int(args[6], 8); path.chmod(mode); meta[str(path)] = [0,gid,mode]
elif name == 'chown':
    assert args[0] == 'root:root'
    for value in args[1:]:
        path = owned(value); record = meta.setdefault(str(path), [0,0,stat.S_IMODE(path.stat().st_mode)])
        record[:2] = [0,0]
elif name == 'chmod':
    mode = int(args[0],8)
    for value in args[1:]:
        path = owned(value); path.chmod(mode); meta.setdefault(str(path), [0,0,mode])[2] = mode
elif name == 'mv':
    assert args[0] in ('-fT', '-T') and all(arg.startswith('-') for arg in args[:-2])
    source, dest = owned(args[-2]), owned(args[-1])
    os.replace(source, dest)
    if str(source) in meta: meta[str(dest)] = meta.pop(str(source))
elif name == 'rm':
    for value in (arg for arg in args if not arg.startswith('-')):
        path = owned(value)
        if path.is_symlink() or path.is_file(): path.unlink()
        elif path.is_dir(): shutil.rmtree(path)
elif name == 'logrotate':
    assert '-d' in args or '--debug' in args
    text = owned(args[-1]).read_text()
    assert text.count('{') == text.count('}')
    assert text.count('create 0620 root concord-security-writers') == 2
elif name == 'systemd-analyze':
    assert args[0] == 'verify'
    for value in args[1:]:
        if value.startswith('-'): continue
        path = owned(value)
        parser = configparser.ConfigParser(interpolation=None, strict=False)
        parser.optionxform = str; parser.read_string(path.read_text())
        assert parser.has_section('Unit')
        if path.name.endswith('.service'):
            assert parser['Service']['Type'] == 'oneshot'
            assert parser['Service']['ExecStart'] == '/usr/sbin/logrotate '+str(root/'etc/concord-security')
        else:
            assert path.name.endswith('.timer')
            assert parser['Timer']['Unit'] == 'concord-security-logrotate.service'
            assert parser['Timer']['OnCalendar'] == '*-*-* *:*:00'
else: raise RuntimeError('unsupported host fixture')
meta_path.write_text(json.dumps(meta))
PY
for hostcmd in getent groupadd systemctl install chown chmod mv ln rm id visudo logrotate systemd-analyze; do
  printf '#!/usr/bin/env bash\nexec "$REAL_PYTHON" "%s" "$@"\n' "$TMP/bin/$hostcmd.py" >"$TMP/bin/$hostcmd"
  cp "$TMP/bin/host-fixture.py" "$TMP/bin/$hostcmd.py"
  # Script basename is the command name, not the .py suffix.
  sed -i "s/name = pathlib.Path(sys.argv\[0\]).name/name = pathlib.Path(sys.argv[0]).stem/" "$TMP/bin/$hostcmd.py"
  chmod +x "$TMP/bin/$hostcmd"
done
# Run the real stream no-follow/open/fchmod code, substituting only host ownership.
cat >"$TMP/bin/python3" <<'SH'
#!/usr/bin/env bash
exec "$REAL_PYTHON" "$FIXTURE_PY_RUNNER" "$@"
SH
cat >"$TMP/python-runner.py" <<'PY'
import json, os, pathlib, runpy, stat, sys
args = sys.argv[1:]
if not args or args[0] != '-':
    os.execv(os.environ['REAL_PYTHON'], [os.environ['REAL_PYTHON']] + args)
source = sys.stdin.read()
meta_path = pathlib.Path(os.environ['FIXTURE_META'])
original_fstat = os.fstat
original_chmod = os.fchmod
def fdpath(fd):
    path = os.readlink('/proc/self/fd/'+str(fd))
    root = os.environ['FIXTURE_ROOT']
    if not path.startswith(root+'/'): raise RuntimeError('stream fixture rejected non-owned descriptor')
    return path
def fchown(fd, uid, gid):
    assert uid == 0 and gid == 10001
    path = fdpath(fd); meta = json.loads(meta_path.read_text())
    meta[path] = [uid, gid, stat.S_IMODE(original_fstat(fd).st_mode)]
    meta_path.write_text(json.dumps(meta))
def fchmod(fd, mode):
    path = fdpath(fd); original_chmod(fd, mode)
    meta = json.loads(meta_path.read_text()); meta.setdefault(path,[0,0,mode])[2] = mode
    meta_path.write_text(json.dumps(meta))
def fstat(fd):
    result = original_fstat(fd); values = list(result)
    record = json.loads(meta_path.read_text()).get(fdpath(fd))
    if record: values[4], values[5] = record[:2]
    return os.stat_result(values)
os.fchown = fchown; os.fchmod = fchmod; os.fstat = fstat
sys.argv = args
exec(compile(source, '<real-security-stream-helper>', 'exec'), {'__name__':'__main__'})
PY
export FIXTURE_PY_RUNNER="$TMP/python-runner.py"
chmod +x "$TMP/bin/python3"
export PATH="$TMP/bin:$PATH"
COPY="$TMP/copy/selfhost-security-event-logging.sh"
MANAGED="$TMP/copy/provision-production.sh"
# Operator maintenance recipes historically stage only the provisioner. Those
# unrelated routes must still reach their own parser without the optional pair.
mkdir -p "$TMP/standalone"
cp "$MANAGED" "$TMP/standalone/provision-production.sh"
STANDALONE="$TMP/standalone/provision-production.sh"
reset_case() {
  /bin/rm -rf "$FIXTURE_ROOT"
  mkdir -p "$FIXTURE_ROOT"
  printf 'concord-security-writers:x:10001:\n' >"$FIXTURE_GROUPS"
  printf '{}\n' >"$FIXTURE_META"; : >"$FIXTURE_CALLS"
  unset FIXTURE_USERS
}
invoke() {
  local script=$1; shift
  rc=0
  output="$(bash "$script" "$@" 2>&1)" || rc=$?
}
refuses() {
  local message=$1
  [[ $rc -ne 0 && "$output" == *"$message"* ]] || fail "expected named refusal '$message': rc=$rc output=$output"
}
# These are source projections, not proof of root ownership on a real host.
# The actual provisioner definitions, bootstrap call window, and wrapper writer
# remain intact; unrelated full-provisioning actions are outside this seam.
"$REAL_PYTHON" - "$MANAGED" "$TMP/copy" <<'BOUNDARY_SOURCES'
from pathlib import Path
import sys

source, dest = Path(sys.argv[1]).read_text(), Path(sys.argv[2])
dispatch = 'if [[ "${1:-}" == "install-healthwatch" ]]; then\n'
assert source.count(dispatch) == 1, 'managed definitions must have a unique dispatch boundary'
(dest / 'managed-definitions.sh').write_text(source.split(dispatch, 1)[0])
bootstrap = '\ninstall_bindmount_placeholders\n'
assert source.count(bootstrap) == 1, 'bootstrap preparation window must be unique'
tail = source.split(bootstrap, 1)[1]
end = '\nchown -R concord:concord "$DEPLOY_DIR"\n'
assert end in tail, 'bootstrap preparation window must end before ownership changes'
(dest / 'bootstrap-preparation.sh').write_text(tail.split(end, 1)[0] + '\n')
writer = '  cat > "$security_event_helper_tmp" ' + '<' * 2
assert source.count(writer) == 1, 'root-owned security wrapper writer must be unique'
tail = writer + source.split(writer, 1)[1]
end = '  /bin/bash -n "$security_event_helper_tmp"\n'
assert end in tail, 'security wrapper writer must end at its real parse check'
(dest / 'security-wrapper-writer.sh').write_text(tail.split(end, 1)[0] + end)
BOUNDARY_SOURCES
snapshot_boundary() {
  "$REAL_PYTHON" - "$FIXTURE_ROOT" "$FIXTURE_GROUPS" "$FIXTURE_META" <<'SNAPSHOT'
from pathlib import Path
import json, os, stat, sys

root = Path(sys.argv[1])
records = {}
for path in [root, *sorted(root.rglob('*'))]:
    details = path.lstat()
    value = [details.st_mode, details.st_ino, details.st_uid, details.st_gid,
             details.st_mtime_ns, details.st_size]
    if stat.S_ISLNK(details.st_mode):
        value.append(os.readlink(path))
    elif stat.S_ISREG(details.st_mode):
        value.append(path.read_bytes().hex())
    records[str(path.relative_to(root))] = value
for extra in sys.argv[2:]:
    path = Path(extra)
    records[extra] = [path.stat().st_ino, path.stat().st_mtime_ns, path.read_bytes().hex()]
print(json.dumps(records, sort_keys=True))
SNAPSHOT
}
assert_logging_artifacts() {
  local policy=$1
  "$REAL_PYTHON" - "$FIXTURE_ROOT" "$FIXTURE_META" "$policy" "$TMP" <<'ARTIFACTS' || fail "$policy logging artifact contract"
import json, pathlib, re, stat, sys

root, meta_path, policy, temp = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]), sys.argv[3], pathlib.Path(sys.argv[4])
meta = json.loads(meta_path.read_text())
expected = {'logs':(0,10001,0o750), 'logs/control-plane':(0,10001,0o2710), 'logs/media-plane':(0,10001,0o2710),
 'logs/control-plane/events.jsonl':(0,10001,0o620), 'logs/media-plane/events.jsonl':(0,10001,0o620),
 'etc/concord-security':(0,0,0o644), 'systemd/concord-security-logrotate.service':(0,0,0o644),
 'systemd/concord-security-logrotate.timer':(0,0,0o644)}
for name, wanted in expected.items():
    path = root / name
    assert tuple(meta[str(path)]) == wanted, (name, meta.get(str(path)))
    assert stat.S_IMODE(path.stat().st_mode) == wanted[2], name
for service in ('control-plane', 'media-plane'):
    assert (root / 'logs' / service / 'events.jsonl').read_bytes() == b''
rotation = (root / 'etc/concord-security').read_text()
blocks = re.findall(r'^    prerotate\n.*?^    endscript\n', rotation, re.M | re.S)
if policy == 'public':
    for name, path in (('rotate', root / 'etc/concord-security'),
                       ('service', root / 'systemd/concord-security-logrotate.service'),
                       ('timer', root / 'systemd/concord-security-logrotate.timer')):
        (temp / ('boundary-public.' + name)).write_bytes(path.read_bytes())
    assert not blocks, 'public rotation must not contain prerotate'
    assert 'nightwatch' not in rotation.lower() and '--drain' not in rotation
else:
    assert len(blocks) == 2 and blocks[0] == blocks[1], 'both managed stanzas must retain the same drain'
    drain = '/usr/bin/flock -n /run/nightwatch-normalizer/normalizer.lock /usr/bin/python3 /usr/local/lib/nightwatch-source/[internal]nightwatch/normalizer/nightwatch-app-normalize.py --drain || exit 1'
    for block in blocks:
        assert block.count(drain) == 1
        assert f'[ -e {root}/normalizer/commissioned ] || [ -L {root}/normalizer/commissioned ]' in block
        assert f'[ ! -L {root}/normalizer/commissioned ]' in block and "'0:0:600' ] || exit 1" in block
    def common(text):
        text = re.sub(r'^    prerotate\n.*?^    endscript\n', '', text, flags=re.M | re.S)
        return [line for line in text.splitlines() if line.strip()]
    assert common(rotation) == common((temp / 'boundary-public.rotate').read_text()), 'stream rotation policy differs beyond managed drain'
    for name in ('service', 'timer'):
        path = root / 'systemd' / ('concord-security-logrotate.' + name)
        assert path.read_bytes() == (temp / ('boundary-public.' + name)).read_bytes(), name
ARTIFACTS
  grep -qxF 'systemctl daemon-reload' "$FIXTURE_CALLS" || fail "$policy did not reload units"
  grep -qxF 'systemctl enable --now concord-security-logrotate.timer' "$FIXTURE_CALLS" || fail "$policy did not enable the timer"
}
public_boundary_case() {
  local marker=$1 existing=$2 before
  reset_case
  : >"$FIXTURE_GROUPS"
  if [[ "$existing" == configured ]]; then
    mkdir -p "$FIXTURE_ROOT/etc" "$FIXTURE_ROOT/systemd" "$FIXTURE_ROOT/logs/control-plane" "$FIXTURE_ROOT/logs/media-plane"
    printf 'managed-rotation-private-sentinel\n' >"$FIXTURE_ROOT/etc/concord-security"
    printf 'managed-service-private-sentinel\n' >"$FIXTURE_ROOT/systemd/concord-security-logrotate.service"
    printf 'managed-timer-private-sentinel\n' >"$FIXTURE_ROOT/systemd/concord-security-logrotate.timer"
    printf 'stream-private-sentinel\n' >"$FIXTURE_ROOT/logs/control-plane/events.jsonl"
    printf 'sibling-private-sentinel\n' >"$FIXTURE_ROOT/logs/media-plane/events.jsonl"
  fi
  mkdir -p "$FIXTURE_ROOT/normalizer"
  case "$marker" in
    regular) printf 'commissioned-private-sentinel\n' >"$FIXTURE_ROOT/normalizer/commissioned" ;;
    live)
      printf 'commissioned-private-sentinel\n' >"$FIXTURE_ROOT/normalizer/target"
      /bin/ln -s target "$FIXTURE_ROOT/normalizer/commissioned" ;;
    dangling) /bin/ln -s absent-target "$FIXTURE_ROOT/normalizer/commissioned" ;;
    *) fail "unknown marker fixture" ;;
  esac
  before="$(snapshot_boundary)"
  # Ambient managed-looking values cannot select the private policy or bypass
  # the public CLI's refusal. The CLI still takes only its fixed public verb.
  SECURITY_EVENT_PREROTATE_BODY='fixture-untrusted-hook' CONCORD_SECURITY_MANAGED=1 \
    invoke "$COPY" security-event-logging
  refuses 'public security-event logging refuses managed normalizer state'
  [[ ! -s "$FIXTURE_CALLS" ]] || fail "$marker/$existing public refusal reached host commands"
  [[ "$(snapshot_boundary)" == "$before" ]] || fail "$marker/$existing refusal altered marker, streams, config, groups, or ownership metadata"
  [[ "$output" != *private-sentinel* && "$output" != *fixture-untrusted-hook* ]] || fail "commissioned refusal emitted fixture contents"
  pass "projected public CLI refuses $marker marker on $existing host before all effects"
}
managed_boundary_case() {
  local route=$1
  reset_case
  mkdir -p "$FIXTURE_ROOT/normalizer"
  printf 'commissioned-private-sentinel\n' >"$FIXTURE_ROOT/normalizer/commissioned"
  local before
  before="$(stat -c '%i:%a:%Y' "$FIXTURE_ROOT/normalizer/commissioned")"
  case "$route" in
    subcommand) invoke "$MANAGED" security-event-logging ;;
    bootstrap)
      invoke /dev/stdin "$TMP/copy" <<'BOOTSTRAP'
set -euo pipefail
source "$1/managed-definitions.sh"
require_security_event_logging
source "$1/bootstrap-preparation.sh"
BOOTSTRAP
      ;;
    wrapper)
      mkdir -p "$FIXTURE_ROOT/approved-lib"
      cp "$COPY" "$FIXTURE_ROOT/approved-lib/selfhost-security-event-logging.sh"
      invoke /dev/stdin "$TMP/copy" <<'WRAPPER'
set -euo pipefail
source "$1/managed-definitions.sh"
require_security_event_logging
security_event_helper_tmp="$FIXTURE_ROOT/managed-wrapper"
source "$1/security-wrapper-writer.sh"
bash "$security_event_helper_tmp"
WRAPPER
      ;;
    *) fail "unknown managed route fixture" ;;
  esac
  [[ $rc -eq 0 ]] || fail "projected managed $route must accept commissioned state: $output"
  [[ $(cat "$FIXTURE_ROOT/normalizer/commissioned") == commissioned-private-sentinel \
     && $(stat -c '%i:%a:%Y' "$FIXTURE_ROOT/normalizer/commissioned") == "$before" ]] || fail "managed $route altered commissioned marker"
  [[ "$output" != *commissioned-private-sentinel* ]] || fail "managed $route emitted commissioned marker contents"
  assert_logging_artifacts managed
  pass "projected actual managed $route retains both drains and common logging artifacts"
}
nightwatch_boundary_contracts() {
  local failures=0 marker existing route
  # The clean public control is retained even on the vulnerable source, so RED
  # distinguishes public drain inheritance from the commissioned-state refusal.
  if ! (
    reset_case
    : >"$FIXTURE_GROUPS"
    SECURITY_EVENT_PREROTATE_BODY='fixture-untrusted-hook' CONCORD_SECURITY_MANAGED=1 \
      invoke "$COPY" security-event-logging
    [[ $rc -eq 0 ]] || fail "clean public CLI control: $output"
    grep -qxF 'groupadd --system --gid 10001 concord-security-writers' "$FIXTURE_CALLS" || fail "clean public control did not create reserved group"
    assert_logging_artifacts public
    pass "projected clean public CLI installs streams, permissions, units, and timer without Nightwatch"
  ); then failures=$((failures + 1)); fi
  for marker in regular live dangling; do
    for existing in fresh configured; do
      if ! (public_boundary_case "$marker" "$existing"); then failures=$((failures + 1)); fi
    done
  done
  if ! (
    reset_case
    invoke "$COPY" security-event-logging --managed
    refuses 'takes no arguments'
    [[ ! -s "$FIXTURE_CALLS" ]] || fail "public managed-selector argument reached host commands"
    invoke "$COPY" managed-security-event-logging
    refuses 'Usage: selfhost-security-event-logging.sh security-event-logging'
    [[ ! -s "$FIXTURE_CALLS" ]] || fail "public managed verb reached host commands"
    invoke /dev/stdin "$COPY" <<'PUBLIC_HOOK'
set -euo pipefail
log(){ :; }
err(){ printf '%s\n' "$*" >&2; }
source "$1"
install_security_event_logging "$FIXTURE_ROOT/logs" "$FIXTURE_ROOT/etc/concord-security" "$FIXTURE_ROOT/systemd" fixture-untrusted-hook
PUBLIC_HOOK
    refuses 'public security-event logging takes no hook argument'
    [[ ! -s "$FIXTURE_CALLS" && ! -e "$FIXTURE_ROOT/logs" ]] || fail "public sourced fourth-argument refusal reached an effect"
    pass "projected public command and sourced API expose no managed selector or hook argument"
  ); then failures=$((failures + 1)); fi
  for route in subcommand bootstrap wrapper; do
    if ! (managed_boundary_case "$route"); then failures=$((failures + 1)); fi
  done
  [[ $failures -eq 0 ]] || fail "$failures Nightwatch source-projection boundary cases failed"
}
if [[ "$BOUNDARY_ONLY" == true ]]; then
  nightwatch_boundary_contracts
  printf 'ALL NIGHTWATCH SOURCE-PROJECTION BOUNDARIES PASS\n'
  exit 0
fi
reset_case
invoke "$STANDALONE" --help
[[ $rc -eq 0 && "$output" == *'Usage:'* && "$output" == *'install-sudoers-boundary'* ]] || fail "standalone managed help works without optional security helper: $output"
[[ ! -s "$FIXTURE_CALLS" ]] || fail "standalone help reached a host effect"
pass "standalone provisioner help needs no adjacent security helper"
invoke "$STANDALONE" install-healthwatch --not-an-option
refuses 'install-healthwatch accepts only --deploy-dir'
[[ ! -s "$FIXTURE_CALLS" ]] || fail "standalone maintenance parser reached a host effect"
pass "standalone unrelated maintenance dispatch works without security helper"
for staged_route in security-event-logging install-sudoers-boundary bootstrap; do
  if [[ "$staged_route" == bootstrap ]]; then
    invoke "$STANDALONE" --domain voice.example.invalid --email ops@example.invalid
  else invoke "$STANDALONE" "$staged_route"; fi
  [[ $rc -ne 0 ]] || fail "missing security helper accepted $staged_route"
  [[ "$output" == *'selfhost-security-event-logging.sh'* \
    && ( "$output" == *'required'* || "$output" == *'missing'* || "$output" == *'staging'* ) \
    && "$output" != *'No such file or directory'* ]] || fail "missing helper needs explicit staged-pair diagnostic for $staged_route: $output"
  [[ ! -s "$FIXTURE_CALLS" ]] || fail "missing helper $staged_route reached host commands"
  pass "$staged_route refuses missing staged helper explicitly before effects"
done
# Paired staging reaches the same unrelated dispatches without invoking primitive.
invoke "$MANAGED" --help
[[ $rc -eq 0 && "$output" == *'Usage:'* ]] || fail "paired managed staging help: $output"
invoke "$MANAGED" install-healthwatch --not-an-option
refuses 'install-healthwatch accepts only --deploy-dir'
[[ ! -s "$FIXTURE_CALLS" ]] || fail "paired staging parser unexpectedly provisions"
pass "staged provisioner/helper pair preserves independent dispatch behavior"
# Defensive import-order test: synthetic metadata and a harmless import callback.
# No untrusted command payload and no privileged execution; trusted control stops
# at the same callback so no bootstrap/sudoers host action can be reached.
ORDER_STAGE="$TMP/import-order-stage"
ORDER_BIN="$TMP/import-order-bin"
mkdir -p "$ORDER_STAGE/sudoers.d" "$ORDER_STAGE/../docker/iptables" "$ORDER_BIN"
cp "$ROOT/infrastructure/docker/iptables/docker-user-rules.sh" "$ORDER_STAGE/../docker/iptables/docker-user-rules.sh"
cp "$ROOT/[internal]sudoers.d/concord-deploy" "$ORDER_STAGE/sudoers.d/concord-deploy"
# Operator readiness is a separate native-sshd regression. This fixture isolates
# import trust/order, so satisfy that new prerequisite without reading host NSS
# or keys; reject every command other than the read-only preflight seam.
cat >"$ORDER_STAGE/managed-ssh.py" <<'OPERATOR_PREFLIGHT'
import sys
assert sys.argv[1:] == ['check-operator', '--ssh-port', '22']
OPERATOR_PREFLIGHT
export FIXTURE_ORDER_HELPER="$ORDER_STAGE/selfhost-security-event-logging.sh"
export FIXTURE_ORDER_ANCESTOR="$TMP" FIXTURE_IMPORT_ORDER="$TMP/import-order.calls"
cat >"$FIXTURE_ORDER_HELPER" <<'IMPORT_CALLBACK'
printf 'import-callback\n' >>"$FIXTURE_IMPORT_ORDER"
exit 77
IMPORT_CALLBACK
# Replace only stat in this separately owned managed copy; metadata is fixture
# input, while the production trust validator and real dispatch remain intact.
"$REAL_PYTHON" - "$MANAGED" "$ORDER_STAGE/provision-production.sh" "$ORDER_BIN/stat" <<'ORDER_COPY'
from pathlib import Path
import sys
source, dest, stat_stub = sys.argv[1:]
text = Path(source).read_text()
assert '/usr/bin/stat' in text, 'trust metadata seam must remain explicit'
Path(dest).write_text(text.replace('/usr/bin/stat', stat_stub))
ORDER_COPY
cat >"$ORDER_BIN/stat" <<'ORDER_STAT'
#!/usr/bin/env bash
[[ $# -eq 3 && $1 == -c && $2 == '%u:%g:%a' ]] || exit 92
fixture_meta=0:0:700
case "$FIXTURE_ORDER_BAD" in
  helper-owner) [[ $3 != "$FIXTURE_ORDER_HELPER" ]] || fixture_meta=20001:0:700 ;;
  helper-group) [[ $3 != "$FIXTURE_ORDER_HELPER" ]] || fixture_meta=0:20001:700 ;;
  helper-mode) [[ $3 != "$FIXTURE_ORDER_HELPER" ]] || fixture_meta=0:0:720 ;;
  ancestry-owner) [[ $3 != "$FIXTURE_ORDER_ANCESTOR" ]] || fixture_meta=20001:0:700 ;;
  ancestry-mode) [[ $3 != "$FIXTURE_ORDER_ANCESTOR" ]] || fixture_meta=0:0:702 ;;
  trusted) ;;
  *) exit 92 ;;
esac
printf 'trust-metadata %s\n' "$fixture_meta" >>"$FIXTURE_IMPORT_ORDER"
printf '%s\n' "$fixture_meta"
ORDER_STAT
/bin/chmod +x "$ORDER_BIN/stat"
for order_route in install-sudoers-boundary bootstrap; do
  # Positive control proves this fixture really reaches import after validation.
  export FIXTURE_ORDER_BAD=trusted
  : >"$FIXTURE_IMPORT_ORDER"; : >"$FIXTURE_CALLS"
  if [[ "$order_route" == bootstrap ]]; then
    invoke "$ORDER_STAGE/provision-production.sh" --domain voice.example.invalid --email ops@example.invalid
  else invoke "$ORDER_STAGE/provision-production.sh" "$order_route"; fi
  [[ $rc -eq 77 ]] && grep -qx 'import-callback' "$FIXTURE_IMPORT_ORDER" \
    || fail "trusted staged $order_route must reach harmless import callback"
  # First record must be metadata validation, never evaluation of helper bytes.
  read -r order_first <"$FIXTURE_IMPORT_ORDER"
  [[ "$order_first" == 'trust-metadata 0:0:700' ]] || fail "$order_route imports helper before validating staged trust"
  [[ ! -s "$FIXTURE_CALLS" ]] || fail "trusted callback reached a privileged boundary"
  pass "$order_route validates trusted staging before harmless import callback"
  for order_bad in helper-owner helper-group helper-mode ancestry-owner ancestry-mode; do
    export FIXTURE_ORDER_BAD="$order_bad"
    : >"$FIXTURE_IMPORT_ORDER"; : >"$FIXTURE_CALLS"
    if [[ "$order_route" == bootstrap ]]; then
      invoke "$ORDER_STAGE/provision-production.sh" --domain voice.example.invalid --email ops@example.invalid
    else invoke "$ORDER_STAGE/provision-production.sh" "$order_route"; fi
    [[ $rc -ne 0 && $rc -ne 77 ]] || fail "$order_route must refuse $order_bad before import callback"
    ! grep -qx 'import-callback' "$FIXTURE_IMPORT_ORDER" || fail "$order_route imported invalid $order_bad helper staging"
    [[ "$output" == *'root-owned'* || "$output" == *'trusted'* || "$output" == *'unsafe'* ]] \
      || fail "$order_route $order_bad needs a named trust refusal: $output"
    [[ ! -s "$FIXTURE_CALLS" ]] || fail "$order_route $order_bad reached privileged host boundary"
    pass "$order_route refuses $order_bad staging before importing helper"
  done
done
unset FIXTURE_ORDER_BAD
reset_case
invoke "$COPY" unrelated-verb
[[ $rc -ne 0 ]] && [[ "$output" == *'security-event-logging'* ]] || fail "wrong verb names only the narrow public verb"
[[ ! -s "$FIXTURE_CALLS" ]] || fail "wrong verb caused host effect"
pass "wrong verb refuses before host effects"
invoke "$COPY" security-event-logging extra
refuses 'takes no arguments'
[[ ! -s "$FIXTURE_CALLS" ]] || fail "extra argument caused host effect"
pass "public helper rejects positional path overrides"
FIXTURE_EUID=1000 invoke "$COPY" security-event-logging
refuses 'must be run as root'
[[ ! -s "$FIXTURE_CALLS" ]] || fail "non-root refusal caused host effect"
pass "non-root helper refuses before host effects"
# Sourceability is the shared managed/test API, not a public path override.
source_output="$(bash -c 'source "$1"; declare -F install_security_event_logging' bash "$COPY")"
[[ "$source_output" == install_security_event_logging && ! -s "$FIXTURE_CALLS" ]] || fail "sourcing helper must only define the primitive"
pass "sourcing narrow helper does not provision"
for conflict in name gid duplicate members primary; do
  reset_case
  case "$conflict" in
    name) printf 'concord-security-writers:x:20002:\n' >"$FIXTURE_GROUPS"; diagnostic='expected 10001' ;;
    gid) printf 'other-group:x:10001:\n' >"$FIXTURE_GROUPS"; diagnostic='already assigned' ;;
    duplicate) printf 'alias:x:10001:\n' >>"$FIXTURE_GROUPS"; diagnostic='exactly one group record' ;;
    members) printf 'concord-security-writers:x:10001:test-member\n' >"$FIXTURE_GROUPS"; diagnostic='supplementary members' ;;
    primary) export FIXTURE_USERS='test-account:x:1000:10001::/none:/bin/false'; diagnostic='primary group' ;;
  esac
  invoke "$COPY" security-event-logging; refuses "$diagnostic"
  [[ ! -e "$FIXTURE_ROOT/logs" ]] || fail "$conflict conflict created log root"
  ! grep -qE '^(install|chown|chmod|systemctl|groupadd) ' "$FIXTURE_CALLS" || fail "$conflict conflict reached mutation"
  [[ "$output" != *test-member* && "$output" != *test-account* ]] || fail "account conflict emitted identifying fixture data"
  pass "group $conflict conflict refuses before filesystem effects"
done
for link_kind in root stream; do
  reset_case
  mkdir -p "$FIXTURE_ROOT/outside"
  printf 'stream-private-sentinel\n' >"$FIXTURE_ROOT/outside/target"
  if [[ "$link_kind" == root ]]; then /bin/ln -s "$FIXTURE_ROOT/outside" "$FIXTURE_ROOT/logs"
  else
    mkdir -p "$FIXTURE_ROOT/logs/control-plane"
    /bin/ln -s "$FIXTURE_ROOT/outside/target" "$FIXTURE_ROOT/logs/control-plane/events.jsonl"
  fi
  invoke "$COPY" security-event-logging; refuses 'must not be a symlink'
  [[ $(cat "$FIXTURE_ROOT/outside/target") == stream-private-sentinel ]] || fail "symlink refusal altered target"
  [[ "$output" != *stream-private-sentinel* ]] || fail "symlink refusal emitted stream content"
  ! grep -q '^systemctl ' "$FIXTURE_CALLS" || fail "symlink refusal enabled timer"
  pass "symlinked $link_kind refuses without following target"
done
# A pre-existing stream with widened mode must fail without repairing or reading it.
reset_case
invoke "$COPY" security-event-logging
[[ $rc -eq 0 ]] || fail "unsafe-stream refusal positive control: $output"
printf 'stream-private-sentinel\n' >"$FIXTURE_ROOT/logs/control-plane/events.jsonl"
chmod 0660 "$FIXTURE_ROOT/logs/control-plane/events.jsonl"
: >"$FIXTURE_CALLS"
invoke "$COPY" security-event-logging
refuses 'could not safely prepare security event stream'
[[ $(stat -c %a "$FIXTURE_ROOT/logs/control-plane/events.jsonl") == 660 ]] || fail "unsafe stream was silently repaired"
[[ $(cat "$FIXTURE_ROOT/logs/control-plane/events.jsonl") == stream-private-sentinel && "$output" != *stream-private-sentinel* ]] || fail "unsafe stream refusal altered or emitted content"
! grep -q '^systemctl ' "$FIXTURE_CALLS" || fail "unsafe stream enabled timer"
pass "unsafe existing stream mode refuses without repair or content disclosure"
reset_case
: >"$FIXTURE_GROUPS"
invoke "$COPY" security-event-logging
[[ $rc -eq 0 ]] || fail "safe direct bootstrap: $output"
grep -qxF 'groupadd --system --gid 10001 concord-security-writers' "$FIXTURE_CALLS" || fail "missing group created with wrong identity"
# Assert virtual owner/group and actual modes, while never using real chown/groupadd.
"$REAL_PYTHON" - "$FIXTURE_ROOT" "$FIXTURE_META" <<'PY' || fail "root/group/mode artifact contract"
import json, pathlib, stat, sys
root = pathlib.Path(sys.argv[1]); meta = json.loads(pathlib.Path(sys.argv[2]).read_text())
expected = {'logs':(0,10001,0o750), 'logs/control-plane':(0,10001,0o2710), 'logs/media-plane':(0,10001,0o2710),
 'logs/control-plane/events.jsonl':(0,10001,0o620), 'logs/media-plane/events.jsonl':(0,10001,0o620),
 'etc/concord-security':(0,0,0o644), 'systemd/concord-security-logrotate.service':(0,0,0o644),
 'systemd/concord-security-logrotate.timer':(0,0,0o644)}
for name, wanted in expected.items():
    path=root/name
    assert tuple(meta[str(path)]) == wanted, (name,meta.get(str(path)))
    assert stat.S_IMODE(path.stat().st_mode) == wanted[2], name
PY
pass "bootstrap artifacts request exact root/group/mode metadata"
printf 'stream-private-sentinel\n' >"$FIXTURE_ROOT/logs/control-plane/events.jsonl"
first_inode="$(stat -c %i "$FIXTURE_ROOT/logs/control-plane/events.jsonl")"
cp "$FIXTURE_ROOT/etc/concord-security" "$TMP/direct.rotate"
cp "$FIXTURE_ROOT/systemd/concord-security-logrotate.service" "$TMP/direct.service"
cp "$FIXTURE_ROOT/systemd/concord-security-logrotate.timer" "$TMP/direct.timer"
invoke "$COPY" security-event-logging
[[ $rc -eq 0 ]] || fail "idempotent bootstrap: $output"
[[ $(cat "$FIXTURE_ROOT/logs/control-plane/events.jsonl") == stream-private-sentinel \
  && $(stat -c %i "$FIXTURE_ROOT/logs/control-plane/events.jsonl") == "$first_inode" ]] || fail "repeat bootstrap replaced/truncated stream"
[[ "$output" != *stream-private-sentinel* ]] || fail "bootstrap output emitted sensitive stream content"
[[ $(grep -c '^groupadd ' "$FIXTURE_CALLS") == 1 ]] || fail "repeat bootstrap recreated group"
grep -qF 'create 0620 root concord-security-writers' "$TMP/direct.rotate" || fail "rotation creates wrong stream identity"
grep -qF 'OnCalendar=*-*-* *:*:00' "$TMP/direct.timer" || fail "rotation cadence differs"
grep -qF 'Persistent=true' "$TMP/direct.timer" || fail "timer is not persistent"
grep -qxF 'systemctl daemon-reload' "$FIXTURE_CALLS" || fail "missing daemon-reload"
grep -qxF 'systemctl enable --now concord-security-logrotate.timer' "$FIXTURE_CALLS" || fail "rotation timer not enabled"
pass "bootstrap preserves existing stream and enables minute logrotate timer"
# Managed rotation adds its private drain while preserving the public streams,
# permissions, service, timer, and all remaining rotation policy.
reset_case
invoke "$MANAGED" security-event-logging
[[ $rc -eq 0 ]] || fail "managed security-event route: $output"
"$REAL_PYTHON" - "$TMP/direct.rotate" "$FIXTURE_ROOT/etc/concord-security" <<'COMMON_ROTATION' || fail "managed/direct common rotation policy differs"
from pathlib import Path
import re, sys

public, managed = (Path(path).read_text() for path in sys.argv[1:])
blocks = re.findall(r'^    prerotate\n.*?^    endscript\n', managed, re.M | re.S)
assert 'prerotate' not in public and 'nightwatch' not in public.lower()
assert len(blocks) == 2 and blocks[0] == blocks[1]
assert all('nightwatch-app-normalize.py --drain || exit 1' in block for block in blocks)
def common(text):
    text = re.sub(r'^    prerotate\n.*?^    endscript\n', '', text, flags=re.M | re.S)
    return [line for line in text.splitlines() if line.strip()]
assert common(public) == common(managed)
COMMON_ROTATION
cmp "$TMP/direct.service" "$FIXTURE_ROOT/systemd/concord-security-logrotate.service" || fail "managed/direct service artifacts differ"
cmp "$TMP/direct.timer" "$FIXTURE_ROOT/systemd/concord-security-logrotate.timer" || fail "managed/direct timer artifacts differ"
pass "managed and public artifacts share common policy and units with only managed drains"
nightwatch_boundary_contracts
"$REAL_PYTHON" "$(dirname "${BASH_SOURCE[0]}")/security_event_publication_test.py" "$SOURCE_ROOT"
printf 'ALL PASS\n'
