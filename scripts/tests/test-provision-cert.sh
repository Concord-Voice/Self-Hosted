#!/usr/bin/env bash
# #2504 Task 2 regression tests for provision-cert.sh — source + PATH-stub externals.
# Convention mirrors scripts/tests/test-install-selfhost.sh.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"

# Fault injection runs only against owned filesystem fixtures and virtual host
# ownership. The adjacent suite exercises main and the real Coturn copy hook.
if [[ "${1:-}" == --replacement-boundary-only ]]; then
  replacement_source_root="$ROOT"
  if [[ $# -eq 3 && $2 == --source-root && -n "$3" ]]; then replacement_source_root=$3
  elif [[ $# -ne 1 ]]; then echo 'FAIL: replacement boundary accepts only an optional test source root' >&2; exit 2; fi
  python3 "$HERE/provision_cert_replacement_test.py" "$replacement_source_root"
  exit $?
fi

# Regression PR #3584: invoke the deploy hook ACTUALLY installed by the
# provisioner, with owned lineages and closed Docker/systemd observers.
if [[ "${1:-}" == --renewal-hook-activation-boundary-only ]]; then
  activation_source_root="$ROOT"
  if [[ $# -eq 3 && $2 == --source-root && -n "$3" ]]; then activation_source_root=$3
  elif [[ $# -ne 1 ]]; then echo 'FAIL: renewal hook activation accepts only an optional test source root' >&2; exit 2; fi
  python3 "$HERE/provision_cert_replacement_test.py" "$activation_source_root" \
    CertificateReplacement.test_partial_renewal_hook_install_preserves_working_target_before_certbot \
    CertificateReplacement.test_failed_renewal_hook_rename_preserves_working_target_before_certbot \
    CertificateReplacement.test_completed_renewal_hook_activation_delivers_complete_selected_helper
  exit $?
fi

if [[ "${1:-}" == --renewal-lineage-boundary-only ]]; then
  renewal_source_root="$ROOT"
  if [[ $# -eq 3 && $2 == --source-root && -n "$3" ]]; then renewal_source_root=$3
  elif [[ $# -ne 1 ]]; then echo 'FAIL: renewal lineage accepts only an optional test source root' >&2; exit 2; fi
  python3 "$HERE/provision_cert_replacement_test.py" "$renewal_source_root" \
    CertificateReplacement.test_installed_renewal_hook_ignores_unrelated_lineage_before_effects \
    CertificateReplacement.test_installed_renewal_hook_ignores_prefix_sibling_lineage_before_effects \
    CertificateReplacement.test_installed_renewal_hook_ignores_same_name_in_different_root_before_effects \
    CertificateReplacement.test_installed_renewal_hook_delivers_selected_lineage \
    CertificateReplacement.test_installed_renewal_hook_delivers_selected_lineage_with_reordered_domains
  exit $?
fi

# Real future-dated certificates through main, staging, and the actual copy
# hook. Filesystem effects are owned; Docker/systemd operations are closed.
if [[ "${1:-}" == --certificate-validity-boundary-only ]]; then
  validity_source_root="$ROOT"
  if [[ $# -eq 3 && $2 == --source-root && -n "$3" ]]; then validity_source_root=$3
  elif [[ $# -ne 1 ]]; then echo 'FAIL: validity boundary accepts only an optional test source root' >&2; exit 2; fi
  python3 "$HERE/provision_cert_replacement_test.py" "$validity_source_root" \
    CertificateReplacement.test_future_not_before_import_refuses_before_activation_delivery_and_restart \
    CertificateReplacement.test_future_not_before_copied_pair_refuses_before_activation_delivery_and_restart \
    CertificateReplacement.test_future_not_before_source_delivery_refuses_before_copy_and_restart \
    CertificateReplacement.test_future_not_before_delivery_refuses_before_copy_and_restart \
    CertificateReplacement.test_already_valid_import_activates_delivers_and_restarts \
    CertificateReplacement.test_already_valid_certbot_lineage_delivers_and_restarts \
    CertificateReplacement.test_already_valid_delivery_copies_and_restarts
  exit $?
fi

# Actual import-mode hostname admission with owned OpenSSL fixtures. Root
# delivery is replaced by named effect observers; generation still requires the
# disposable Linux fixture used by the full suite. This proves the SAN boundary,
# not main's root gate, certificate trust, TLS delivery or a running consumer.
# --host is the deployment base: nginx and coturn share its four-name pair.
if [[ "${1:-}" == --import-san-boundary-only ]]; then
  san_source_root="$ROOT"
  if [[ $# -eq 3 && $2 == --source-root && -n "$3" ]]; then san_source_root=$3
  elif [[ $# -ne 1 ]]; then echo 'FAIL: import SAN boundary accepts only an optional test source root' >&2; exit 2; fi
  python3 - "$san_source_root/infrastructure/deploy/provision-cert.sh" <<'IMPORT_SAN_TESTS'
import hashlib
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest

SOURCE = Path(sys.argv[1]).resolve()
BASE = "san-boundary.example"
REQUIRED = (BASE, "api." + BASE, "media." + BASE, "turn." + BASE)
DRIVER = r'''
source "$1"
stat() {
  if [[ $# -eq 3 && ( $1 == -c || $1 == -Lc ) && $2 == '%u:%g:%a' ]]; then
    [[ $3 == "${LE_LIVE_ROOT%/*}" || $3 == "$LE_LIVE_ROOT" || $3 == "$LE_LIVE_ROOT/"* ]] || return 97
    printf '0:0:%s\n' "$(command stat -Lc '%a' "$3")"
  else command stat "$@"; fi
}
chown() {
  [[ $1 == root:root ]] || return 97
  shift
  for path in "$@"; do [[ $path == "$LE_LIVE_ROOT" || $path == "$LE_LIVE_ROOT/"* ]] || return 97; done
}
if [[ "$IMPORT_SAN_DECODER_UNAVAILABLE" == 1 ]]; then
  python3() {
    printf '%s\n' decoder-unavailable >>"$IMPORT_SAN_DECODER_EVENTS"
    printf '%s\n' owned-decoder-private-canary
    printf '%s\n' owned-decoder-private-canary >&2
    return 41
  }
fi
require_copy_hook() { printf '%s\n' copy-prerequisite >>"$IMPORT_SAN_EVENTS"; }
complete_delivery() {
  [[ $# -eq 1 && $1 == "$LE_LIVE_ROOT/$IMPORT_SAN_BASE" ]] || return 97
  printf '%s\n' delivery >>"$IMPORT_SAN_EVENTS"
}
delivery_complete() { printf '%s\n' completion >>"$IMPORT_SAN_EVENTS"; }
mode_import --cert "$2" --key "$3" --host "$IMPORT_SAN_BASE"
'''


def fixture_command(argv):
    result = subprocess.run(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            timeout=30, check=False)
    if result.returncode:
        raise AssertionError("owned certificate fixture command failed")
    return result.stdout


def snapshot(*roots):
    records = {}
    for root in roots:
        for path in [root, *sorted(root.rglob("*"))]:
            metadata = path.lstat()
            record = [metadata.st_mode, metadata.st_ino, metadata.st_uid,
                      metadata.st_gid, metadata.st_size, metadata.st_mtime_ns]
            if stat.S_ISREG(metadata.st_mode):
                record.append(hashlib.sha256(path.read_bytes()).hexdigest())
            records[str(path)] = record
    return records


class ImportedDeploymentSANs(unittest.TestCase):
    def exercise(self, names, *, expected_missing=None, existing=True, base=BASE,
                 general_names=None, general_name_entries=None, subject_name=None,
                 decoder_unavailable=False):
        with tempfile.TemporaryDirectory(prefix="concord-import-san-") as name:
            root = Path(name)
            source = root / "source"
            source.mkdir()
            target = source / "provision-cert.sh"
            shutil.copyfile(SOURCE, target)
            inputs, live, hooks, fanout = [root / label for label in
                                          ("inputs", "live", "hooks", "fanout")]
            for directory in (inputs, live, hooks, fanout):
                directory.mkdir(mode=0o700)
            cert, key = inputs / "fullchain.pem", inputs / "privkey.pem"
            subject = base if subject_name is None else subject_name
            if general_name_entries is None:
                typed_names = tuple("DNS:" + x for x in names) if general_names is None else general_names
                san_options = ["-addext", "subjectAltName=" + ",".join(typed_names)]
            else:
                self.assertIsNone(general_names, "owned fixture must select one GeneralName encoding")
                # A SAN section preserves commas INSIDE one URI value. Using
                # -addext's comma-separated spelling would instead create real
                # dNSNames and make the negative fixture accidentally valid.
                config = inputs / "openssl.cnf"
                config.write_text("[req]\ndistinguished_name=distinguished_names\n"
                                  "x509_extensions=extensions\nprompt=no\n"
                                  "[distinguished_names]\nCN=" + subject + "\n"
                                  "[extensions]\nsubjectAltName=@names\n[names]\n" +
                                  "".join(label + "=" + value + "\n"
                                          for label, value in general_name_entries), encoding="utf-8")
                san_options = ["-config", str(config)]
            fixture_command(["openssl", "req", "-x509", "-newkey", "ec",
                             "-pkeyopt", "ec_paramgen_curve:P-256", "-sha256",
                             "-nodes", "-days", "30", "-subj", "/CN=" + subject,
                             *san_options,
                             "-keyout", str(key), "-out", str(cert)])
            key.chmod(0o600)
            fixture_command(["openssl", "x509", "-in", str(cert), "-noout", "-checkend", "0"])
            self.assertEqual(fixture_command(["openssl", "x509", "-in", str(cert), "-noout", "-pubkey"]),
                             fixture_command(["openssl", "pkey", "-in", str(key), "-pubout"]))
            # OpenSSL independently checks fixture hostname coverage; never
            # derive the expected host set from the production SAN parser.
            required = (base, "api." + base, "media." + base, "turn." + base)
            missing = []
            for host in required:
                checked = subprocess.run(["openssl", "x509", "-in", str(cert),
                                          "-noout", "-checkhost", host],
                                         stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                         timeout=30, check=False)
                self.assertIn(checked.returncode, (0, 1), "fixture hostname check unavailable")
                verdict = checked.stdout.decode("utf-8").strip()
                matches = "Hostname " + host + " does match certificate"
                misses = "Hostname " + host + " does NOT match certificate"
                self.assertIn(verdict, (matches, misses), "fixture hostname verdict unavailable")
                if verdict == misses:
                    missing.append(host)
            if expected_missing is None:
                self.assertEqual([], missing, "positive fixture must cover every consumer")
            else:
                self.assertIn(expected_missing, missing, "negative fixture must miss the named consumer")
            if existing:
                lineage = live / base
                lineage.mkdir(mode=0o700)
                if expected_missing is None and not decoder_unavailable:
                    # A successful replacement needs a trustworthy rollback
                    # generation; refusal cases deliberately retain inert bytes.
                    fixture_command(["openssl", "req", "-x509", "-newkey", "ec",
                                     "-pkeyopt", "ec_paramgen_curve:P-384", "-sha256",
                                     "-nodes", "-days", "30", "-subj", "/CN=" + base,
                                     "-keyout", str(lineage / "privkey.pem"),
                                     "-out", str(lineage / "fullchain.pem")])
                else:
                    (lineage / "fullchain.pem").write_text("retained-certificate-canary\n")
                    (lineage / "privkey.pem").write_text("retained-key-canary\n")
                (lineage / "privkey.pem").chmod(0o600)
                (lineage / "fullchain.pem").chmod(0o644)
            (fanout / "cert.pem").write_text("retained-fanout-certificate-canary\n")
            (fanout / "key.pem").write_text("retained-fanout-key-canary\n")
            events = root / "events"
            events.write_text("")
            decoder_events = root / "decoder-events"
            decoder_events.write_text("")
            before = snapshot(inputs, live, hooks, fanout)
            environment = dict(os.environ)
            environment.update(LE_LIVE_ROOT=str(live), HOOK_DIR=str(hooks),
                               COTURN_CERT_ROOT=str(fanout), IMPORT_SAN_EVENTS=str(events),
                               IMPORT_SAN_BASE=base,
                               IMPORT_SAN_DECODER_UNAVAILABLE="1" if decoder_unavailable else "0",
                               IMPORT_SAN_DECODER_EVENTS=str(decoder_events))
            result = subprocess.run(["bash", "-c", DRIVER, "import-san-driver",
                                     str(target), str(cert), str(key)],
                                    env=environment, stdout=subprocess.PIPE,
                                    stderr=subprocess.PIPE, timeout=30, check=False)
            output = (result.stdout + result.stderr).decode("utf-8", errors="replace")
            for failure in ("command not found", "unbound variable", "syntax error", "Traceback"):
                self.assertNotIn(failure, output, "consumer must reach the actual SAN boundary")
            self.assertNotIn("PRIVATE KEY", output)
            self.assertFalse("owned-decoder-private-canary" in output,
                             "decoder output must remain private on failure")
            if expected_missing is not None:
                self.assertEqual(1, result.returncode, "missing consumer hostname must refuse")
                self.assertIn("certificate SAN does not cover " + expected_missing, output)
                self.assertEqual("", events.read_text(), "refuse before copy prerequisite or delivery")
                self.assertEqual(before, snapshot(inputs, live, hooks, fanout),
                                 "refusal must preserve source and canonical/fanout bytes and metadata")
            elif decoder_unavailable:
                self.assertEqual(1, result.returncode, "unavailable DNS decoder must refuse")
                self.assertIn("import: cannot read certificate DNS SANs", output)
                self.assertEqual("decoder-unavailable\n", decoder_events.read_text(),
                                 "actual certificate decoder command must reach the owned refusal seam")
                self.assertEqual("", events.read_text(), "decoder refusal precedes copy prerequisite or delivery")
                self.assertEqual(before, snapshot(inputs, live, hooks, fanout),
                                 "decoder refusal must conserve source and canonical/fanout bytes and metadata")
            else:
                self.assertEqual(0, result.returncode, "complete consumer coverage must admit")
                self.assertEqual("copy-prerequisite\ndelivery\ncompletion\n", events.read_text(),
                                 "positive controls must spend actual import/install branches")
                self.assertEqual(cert.read_bytes(), (live / base / "fullchain.pem").read_bytes())
                self.assertTrue(key.read_bytes() == (live / base / "privkey.pem").read_bytes(),
                                "import must preserve owned source key bytes in canonical delivery")
                self.assertEqual(0o600, (live / base / "privkey.pem").stat().st_mode & 0o777)

    def test_apex_only_refuses_before_existing_lineage_mutation(self):
        self.exercise((BASE,), expected_missing="api." + BASE)

    def test_apex_only_refuses_before_fresh_lineage_creation(self):
        self.exercise((BASE,), expected_missing="api." + BASE, existing=False)

    def test_each_required_hostname_is_checked(self):
        for missing in REQUIRED:
            with self.subTest(missing=missing):
                self.exercise(tuple(host for host in REQUIRED if host != missing),
                              expected_missing=missing)

    def test_complete_explicit_sans_admit(self):
        self.exercise(REQUIRED)

    def test_uri_dns_lookalikes_do_not_cover_service_hostnames(self):
        # Only the apex has an actual dNSName. OpenSSL's hostname oracle must
        # independently miss api/media/turn even though the URI bytes contain
        # DNS:<hostname> text that an unanchored production parser could spend.
        self.exercise((), general_names=("DNS:" + BASE,
                                         "URI:DNS:api." + BASE,
                                         "URI:DNS:media." + BASE,
                                         "URI:DNS:turn." + BASE),
                      expected_missing="api." + BASE)

    def test_real_dns_coverage_with_unrelated_uri_admits(self):
        # A correct parser ignores other GeneralName types, rather than
        # refusing an otherwise complete imported pair merely for their presence.
        self.exercise((), general_names=tuple("DNS:" + x for x in REQUIRED) +
                      ("URI:DNS:unrelated." + BASE,))

    def test_uri_commas_cannot_manufacture_dns_coverage(self):
        # One URI GeneralName contains all three DNS-looking chunks. OpenSSL
        # still sees no service dNSNames; splitting its display on commas must
        # not promote those chunks into authority for the deployment pair.
        self.exercise((), general_name_entries=(
            ("DNS.1", BASE),
            ("URI.1", "urn:owned,DNS:api." + BASE + ",DNS:media." + BASE + ",DNS:turn." + BASE)),
            expected_missing="api." + BASE)

    def test_real_dns_coverage_with_uri_commas_admits(self):
        self.exercise((), general_name_entries=tuple(
            ("DNS." + str(index), host) for index, host in enumerate(REQUIRED, 1)) +
            (("URI.1", "urn:owned,DNS:api." + BASE + ",DNS:media." + BASE + ",DNS:turn." + BASE),))

    def test_uri_commas_cannot_manufacture_apex_or_service_dns_coverage(self):
        # No actual dNSName and an unrelated subject avoid any accidental
        # common-name hostname control while independently proving apex absence.
        self.exercise((), subject_name="unrelated.example", general_name_entries=(
            ("URI.1", "urn:owned,DNS:" + BASE + ",DNS:api." + BASE +
             ",DNS:media." + BASE + ",DNS:turn." + BASE),), expected_missing=BASE)

    def test_unavailable_dns_decoder_refuses_before_effects(self):
        self.exercise(REQUIRED, decoder_unavailable=True)

    def test_wildcard_plus_apex_admits(self):
        self.exercise((BASE, "*." + BASE))

    def test_wildcard_without_apex_refuses(self):
        self.exercise(("*." + BASE,), expected_missing=BASE)

    def test_parent_wildcard_cannot_cover_nested_service_hosts(self):
        base = "edge." + BASE
        self.exercise(("*." + BASE,), base=base, expected_missing="api." + base)


unittest.main(argv=[sys.argv[0]], verbosity=2)
IMPORT_SAN_TESTS
  exit $?
fi

# Explicit unit admission for environments without Docker. This calls the real
# three public mode guard/argument/install/upload boundaries, with root delivery
# prerequisites and certificate issuance/completion replaced by closed recorders.
# It does not call main, override EUID, prove TLS delivery, or run real renewal.
if [[ "${1:-}" == --managed-hook-boundary-only ]]; then
  unit_source_root="$ROOT"
  if [[ $# -eq 3 && $2 == --source-root && -n "$3" ]]; then unit_source_root=$3
  elif [[ $# -ne 1 ]]; then echo 'FAIL: unit boundary accepts only an optional test source root' >&2; exit 2; fi
  unit_root="$(mktemp -d)" || exit 1
  trap 'rm -rf "$unit_root"' EXIT
  unit_fails=0
  certificate_upload_mode_unit() (
    local checkout=$2 authorization=$3 retained=$4 expectation=$5
    local mode=${6:-letsencrypt} lineage=${7:-fresh}
    local fixture="$unit_root/$1" domain=boundary.example rc=0
    local artifact before_metadata='' before_link=''
    local before_lineage='' cert_input key_input email=ops@example.invalid
    local -a selector=()
    mkdir -p "$fixture/source" "$fixture/bin" "$fixture/hooks" "$fixture/live" "$fixture/input" || return 1
    cp "$unit_source_root/infrastructure/deploy/provision-cert.sh" "$fixture/source/provision-cert.sh" || return 1
    cert_input="$fixture/input/fullchain.pem"; key_input="$fixture/input/privkey.pem"
    printf 'input-certificate-private-sentinel\n' >"$cert_input"
    printf 'input-key-private-sentinel\n' >"$key_input"
    if [[ ( "$mode" == import || "$mode" == local ) && "$expectation" == accept ]]; then
      # Accepted local/import controls provide complete owned candidates. The
      # issuance double records the local request while native validation stays
      # real; guard-refusal fixtures keep inert placeholders.
      command openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
        -sha256 -nodes -days 30 -subj "/CN=$domain" \
        -addext "subjectAltName=DNS:$domain,DNS:api.$domain,DNS:media.$domain,DNS:turn.$domain,DNS:spa.$domain" \
        -keyout "$key_input" -out "$cert_input" >/dev/null 2>&1 || return 1
    fi
    chmod 0644 "$cert_input"; chmod 0600 "$key_input"
    if [[ $lineage == existing ]]; then
      mkdir -p "$fixture/live/$domain" || return 1
      printf 'retained-certificate-private-sentinel\n' >"$fixture/live/$domain/fullchain.pem"
      printf 'retained-key-private-sentinel\n' >"$fixture/live/$domain/privkey.pem"
      if [[ "$expectation" == accept && ( "$mode" == local || "$mode" == import ) ]]; then
        command openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-384 \
          -sha256 -nodes -days 30 -subj "/CN=$domain" \
          -keyout "$fixture/live/$domain/privkey.pem" -out "$fixture/live/$domain/fullchain.pem" \
          >/dev/null 2>&1 || return 1
      fi
      chmod 0644 "$fixture/live/$domain/fullchain.pem"; chmod 0600 "$fixture/live/$domain/privkey.pem"
    elif [[ $lineage != fresh ]]; then return 2; fi
    artifact="$fixture/hooks/upload-cert-to-cloudflare.sh"
    printf '%s\n' 'echo configuration-read >>"$UNIT_BOUNDARY_EVENTS"' >"$fixture/configuration"
    printf '%s\n' \
      '#!/usr/bin/env bash' \
      'set -euo pipefail' \
      '[[ $# -eq 0 && "${RENEWED_LINEAGE:-}" == "$LE_LIVE_ROOT/$UNIT_BOUNDARY_DOMAIN" ]] || exit 97' \
      'echo upload-hook >>"$UNIT_BOUNDARY_EVENTS"' \
      'source "$UNIT_BOUNDARY_CONFIGURATION"' \
      'curl --fixture-managed-upload "$RENEWED_LINEAGE"' >"$fixture/uploader"
    printf '%s\n' \
      '#!/usr/bin/env bash' \
      'set -euo pipefail' \
      '[[ $# -eq 2 && $1 == --fixture-managed-upload && $2 == "$LE_LIVE_ROOT/$UNIT_BOUNDARY_DOMAIN" ]] || exit 97' \
      'echo upload >>"$UNIT_BOUNDARY_EVENTS"' >"$fixture/bin/curl"
    chmod 0755 "$fixture/uploader" "$fixture/bin/curl" || return 1
    case "$checkout" in
      public) ;;
      managed) cp "$fixture/uploader" "$fixture/source/upload-cert-to-cloudflare.sh" || return 1 ;;
      *) return 2 ;;
    esac
    case "$retained" in
      absent) ;;
      regular) cp "$fixture/uploader" "$artifact" && chmod 0750 "$artifact" || return 1 ;;
      symlink) ln -s "$fixture/uploader" "$artifact" || return 1 ;;
      dangling) ln -s "$fixture/missing-managed-uploader" "$artifact" || return 1 ;;
      *) return 2 ;;
    esac
    if [[ "$retained" != absent ]]; then
      before_metadata="$(stat -c '%F:%i:%u:%g:%a:%s:%Y' "$artifact")" || return 1
      if [[ -L "$artifact" ]]; then before_link="$(readlink "$artifact")" || return 1
      else cp "$artifact" "$fixture/retained-before" || return 1; fi
    fi
    case "$authorization" in
      selfhost) ;;
      managed) selector=(--managed-cloudflare-upload) ;;
      unknown) selector=(--managed-cloudflare-upload=unknown) ;;
      data)
        case "$mode" in
          letsencrypt) email=--managed-cloudflare-upload ;;
          import) cert_input=--managed-cloudflare-upload ;;
          *) return 2 ;;
        esac ;;
      *) return 2 ;;
    esac
    export LE_LIVE_ROOT="$fixture/live" HOOK_DIR="$fixture/hooks"
    export UNIT_BOUNDARY_EVENTS="$fixture/events" UNIT_BOUNDARY_CONFIGURATION="$fixture/configuration" UNIT_BOUNDARY_DOMAIN="$domain"
    export PATH="$fixture/bin:$PATH"
    : >"$UNIT_BOUNDARY_EVENTS"
    unit_snapshot() {
      python3 -c '
from pathlib import Path
import json, stat, sys
records = {}
for name in sys.argv[1:]:
    root = Path(name)
    for path in [root, *sorted(root.rglob("*"))] if root.is_dir() else [root]:
        details = path.lstat()
        record = [details.st_mode, details.st_ino, details.st_uid, details.st_gid,
                  details.st_size, details.st_mtime_ns]
        if stat.S_ISREG(details.st_mode):
            record.append(path.read_bytes().hex())
        records[str(path)] = record
print(json.dumps(records, sort_keys=True))
' "$fixture/live" "$fixture/input" "$fixture/configuration" "$fixture/uploader"
    }
    before_lineage="$(unit_snapshot)" || return 1
    source "$fixture/source/provision-cert.sh" || return 1
    stat() {
      if [[ $# -eq 3 && ( $1 == -c || $1 == -Lc ) && $2 == '%u:%g:%a' ]]; then
        [[ $3 == "${LE_LIVE_ROOT%/*}" || $3 == "$LE_LIVE_ROOT" || $3 == "$LE_LIVE_ROOT/"* ]] || return 97
        printf '0:0:%s\n' "$(command stat -Lc '%a' "$3")"
      else command stat "$@"; fi
    }
    chown() {
      [[ $1 == root:root ]] || return 97
      shift
      local path
      for path in "$@"; do [[ $path == "$LE_LIVE_ROOT" || $path == "$LE_LIVE_ROOT/"* ]] || return 97; done
    }
    require_copy_hook() { printf '%s\n' required-hook-check >>"$UNIT_BOUNDARY_EVENTS"; }
    install_required_renewal_hook() { printf '%s\n' required-hook-install >>"$UNIT_BOUNDARY_EVENTS"; }
    certbot() {
      [[ "${1:-}" == certonly ]] || return 97
      printf '%s\n' certbot >>"$UNIT_BOUNDARY_EVENTS"
      mkdir -p "$LE_LIVE_ROOT/$UNIT_BOUNDARY_DOMAIN" || return 1
      # Presence only: these files are neither certificates nor private keys,
      # and the completion recorder never reads their contents.
      : >"$LE_LIVE_ROOT/$UNIT_BOUNDARY_DOMAIN/fullchain.pem"
      : >"$LE_LIVE_ROOT/$UNIT_BOUNDARY_DOMAIN/privkey.pem"
    }
    complete_delivery() {
      [[ $# -eq 1 && $1 == "$LE_LIVE_ROOT/$UNIT_BOUNDARY_DOMAIN" ]] || return 97
      printf '%s\n' delivery >>"$UNIT_BOUNDARY_EVENTS"
    }
    delivery_complete() { printf '%s\n' complete >>"$UNIT_BOUNDARY_EVENTS"; }
    fingerprint() { printf '%s\n' fingerprint >>"$UNIT_BOUNDARY_EVENTS"; printf '%s' fixture-fingerprint; }
    python3() {
      if [[ "$mode" == import && $# -eq 3 && $1 == -c && $3 == "$cert_input" ]]; then
        # Observe the metadata actually consumed, keeping native decoding and
        # both production Python programs real. Validity is a separate read
        # from DNS admission even though both receive the same certificate.
        command python3 -c '
import os, ssl, sys
native = getattr(getattr(ssl, "_ssl", None), "_test_decode_cert", None)
seen = set()
class Metadata(dict):
    def observe(self, key):
        kind = {"subjectAltName": "native-dns-decoder",
                "notBefore": "native-validity-decoder", "notAfter": "native-validity-decoder"}.get(key)
        if kind and kind not in seen:
            with open(os.environ["UNIT_BOUNDARY_EVENTS"], "a") as events:
                events.write(kind + "\n")
            seen.add(kind)
    def __getitem__(self, key):
        self.observe(key)
        return super().__getitem__(key)
    def get(self, key, default=None):
        self.observe(key)
        return super().get(key, default)
if callable(native):
    ssl._ssl._test_decode_cert = lambda path: Metadata(native(path))
code = sys.argv[1]
sys.argv = ["-c", sys.argv[2]]
exec(compile(code, "<owned-native-metadata-observer>", "exec"), {"__name__": "__main__"})
' "$2" "$3"
        return $?
      fi
      command python3 "$@"
    }
    openssl() {
      local verb=${1:-}; shift
      case "$verb:$*" in
        req:*)
          # The two option values after -subj/-addext stay data. Admit only the
          # actual local request. Candidate destinations must remain owned and
          # have the two conventional names in one generation directory.
          [[ $# -eq 15 && $1 == -x509 && $2 == -newkey && $3 == rsa:4096 && $4 == -sha256 && $5 == -nodes
             && $6 == -days && $7 == 825 && $8 == -keyout && $9 == "$LE_LIVE_ROOT/"*/privkey.pem
             && ${10} == -out && ${11} == "${9%/privkey.pem}/fullchain.pem" && ${12} == -subj
             && ${13} == "/CN=$domain" && ${14} == -addext && ${15} == "subjectAltName=DNS:$domain,DNS:api.$domain,DNS:media.$domain,DNS:turn.$domain,DNS:spa.$domain" ]] || return 97
          printf '%s\n' openssl-local >>"$UNIT_BOUNDARY_EVENTS"
          cp "$fixture/input/fullchain.pem" "${11}" && cp "$fixture/input/privkey.pem" "${9}" ;;
        "x509:-in $fixture/input/fullchain.pem -noout -pubkey")
          printf '%s\n' openssl-cert-spki >>"$UNIT_BOUNDARY_EVENTS"; command openssl "$verb" "$@" ;;
        "pkey:-in $fixture/input/privkey.pem -pubout")
          printf '%s\n' openssl-key-spki >>"$UNIT_BOUNDARY_EVENTS"; command openssl "$verb" "$@" ;;
        sha256:)
          printf '%s\n' openssl-hash >>"$UNIT_BOUNDARY_EVENTS"; command openssl "$verb" "$@" ;;
        "x509:-in $fixture/input/fullchain.pem -noout -checkend 0")
          printf '%s\n' openssl-expiry >>"$UNIT_BOUNDARY_EVENTS"; command openssl "$verb" "$@" ;;
        x509:*|pkey:*)
          [[ ${1:-} == -in && ${2:-} == "$LE_LIVE_ROOT/"* ]] || return 97
          command openssl "$verb" "$@" ;;
        *) printf '%s\n' unsupported-openssl >>"$UNIT_BOUNDARY_EVENTS"; return 97 ;;
      esac
    }
    case "$mode" in
      letsencrypt) mode_letsencrypt --domain "$domain" --email "$email" "${selector[@]}" >"$fixture/output" 2>&1 || rc=$? ;;
      local) mode_local --host "$domain" "${selector[@]}" >"$fixture/output" 2>&1 || rc=$? ;;
      import) mode_import --cert "$cert_input" --key "$key_input" --host "$domain" "${selector[@]}" >"$fixture/output" 2>&1 || rc=$? ;;
      *) return 2 ;;
    esac
    ! grep -qE 'command not found|unbound variable|syntax error|Traceback' "$fixture/output" || return 1
    if [[ "$expectation" == refuse || "$expectation" == usage ]]; then
      if [[ "$expectation" == refuse ]]; then
        [[ $rc -eq 1 ]] && grep -Fq 'tls-provision: managed renewal hook present; self-host issuance not started' "$fixture/output" || return 1
      else [[ $rc -eq 2 ]] || return 1; fi
      [[ ! -s "$UNIT_BOUNDARY_EVENTS" && "$(unit_snapshot)" == "$before_lineage" ]] || return 1
      ! grep -qF private-sentinel "$fixture/output" || return 1
      if [[ "$retained" != absent ]]; then
        [[ "$(stat -c '%F:%i:%u:%g:%a:%s:%Y' "$artifact")" == "$before_metadata" ]] || return 1
        if [[ -n "$before_link" ]]; then
          [[ -L "$artifact" && "$(readlink "$artifact")" == "$before_link" ]] || return 1
        else cmp -s "$fixture/retained-before" "$artifact" || return 1; fi
      else [[ ! -e "$artifact" && ! -L "$artifact" ]] || return 1; fi
      return 0
    fi
    [[ $rc -eq 0 && -f "$fixture/live/$domain/fullchain.pem" && -f "$fixture/live/$domain/privkey.pem" ]] || return 1
    if [[ "$mode" == local ]]; then
      [[ $(cat "$UNIT_BOUNDARY_EVENTS") == $'required-hook-check\nopenssl-local\nfingerprint\ndelivery\ncomplete' \
         || $(cat "$UNIT_BOUNDARY_EVENTS") == $'required-hook-check\nopenssl-local\ndelivery\nfingerprint\ncomplete' ]] || return 1
      cmp -s "$fixture/input/fullchain.pem" "$fixture/live/$domain/fullchain.pem" \
        && cmp -s "$fixture/input/privkey.pem" "$fixture/live/$domain/privkey.pem" || return 1
    elif [[ "$mode" == import ]]; then
      [[ $(cat "$UNIT_BOUNDARY_EVENTS") == $'openssl-cert-spki\nopenssl-key-spki\nopenssl-hash\nopenssl-hash\nopenssl-expiry\nnative-dns-decoder\nnative-validity-decoder\nrequired-hook-check\nfingerprint\ndelivery\ncomplete' \
         || $(cat "$UNIT_BOUNDARY_EVENTS") == $'openssl-cert-spki\nopenssl-key-spki\nopenssl-hash\nopenssl-hash\nopenssl-expiry\nnative-dns-decoder\nnative-validity-decoder\nrequired-hook-check\nopenssl-hash\nopenssl-hash\ndelivery\nfingerprint\ncomplete' ]] || return 1
      cmp -s "$fixture/input/fullchain.pem" "$fixture/live/$domain/fullchain.pem" \
        && cmp -s "$fixture/input/privkey.pem" "$fixture/live/$domain/privkey.pem" || return 1
    elif [[ "$authorization" == managed ]]; then
      [[ "$(cat "$UNIT_BOUNDARY_EVENTS")" == $'required-hook-check\nrequired-hook-install\ncertbot\ndelivery\nupload-hook\nconfiguration-read\nupload\ncomplete' ]] || return 1
      cmp -s "$fixture/source/upload-cert-to-cloudflare.sh" "$artifact" && [[ -x "$artifact" ]] || return 1
    else
      [[ "$(cat "$UNIT_BOUNDARY_EVENTS")" == $'required-hook-check\nrequired-hook-install\ncertbot\ndelivery\ncomplete'
         && ! -e "$artifact" && ! -L "$artifact" ]] || return 1
    fi
  )
  for unit_case in \
    'public-clean public selfhost absent accept' \
    'public-adjacent managed selfhost absent accept' \
    'public-retained public selfhost regular refuse' \
    'public-retained-adjacent managed selfhost regular refuse' \
    'public-retained-symlink public selfhost symlink refuse' \
    'public-retained-dangling public selfhost dangling refuse' \
    'managed-authorized managed managed absent accept' \
    'managed-retained managed managed regular accept' \
    'unknown-selector managed unknown absent usage'; do
    read -r unit_label unit_checkout unit_authorization unit_retained unit_expectation <<<"$unit_case"
    if certificate_upload_mode_unit "$unit_label" "$unit_checkout" "$unit_authorization" "$unit_retained" "$unit_expectation"; then
      echo "PASS: certificate mode boundary unit $unit_label"
    else
      echo "FAIL: certificate mode boundary unit $unit_label"
      unit_fails=$((unit_fails + 1))
    fi
  done
  for unit_mode in local import letsencrypt; do
    for unit_retained in regular symlink dangling; do
      for unit_lineage in fresh existing; do
        unit_label="$unit_mode-$unit_retained-$unit_lineage"
        if certificate_upload_mode_unit "$unit_label" public selfhost "$unit_retained" refuse "$unit_mode" "$unit_lineage"; then
          echo "PASS: certificate public-mode boundary $unit_label preserves source and hook before effects"
        else
          echo "FAIL: certificate public-mode boundary $unit_label"
          unit_fails=$((unit_fails + 1))
        fi
      done
    done
  done
  for unit_case in \
    'local-clean public selfhost absent accept local fresh' \
    'import-clean public selfhost absent accept import fresh' \
    'local-managed-flag public managed regular usage local existing' \
    'import-managed-flag public managed regular usage import existing' \
    'le-managed-email-data public data regular refuse letsencrypt existing' \
    'import-managed-cert-data public data regular refuse import existing'; do
    read -r unit_label unit_checkout unit_authorization unit_retained unit_expectation unit_mode unit_lineage <<<"$unit_case"
    if certificate_upload_mode_unit "$unit_label" "$unit_checkout" "$unit_authorization" "$unit_retained" "$unit_expectation" "$unit_mode" "$unit_lineage"; then
      echo "PASS: certificate public-mode boundary $unit_label"
    else
      echo "FAIL: certificate public-mode boundary $unit_label"
      unit_fails=$((unit_fails + 1))
    fi
  done
  echo 'UNIT ONLY: mode guard/argument/install/upload proof; main/root, TLS delivery and renewal runtime remain unverified.'
  echo "── $unit_fails unit failures ──"
  exit $((unit_fails > 0))
fi

# Run the acceptance body in a disposable, real-UID-0 namespace. Only these
# four public files are projected read-only; no checkout, socket, credentials,
# /etc or /opt host path enters the container. CI's supported-host image is the
# default. Root may select its recorded local fixture image explicitly.
if [[ "${SELFHOST_CERT_TEST_INNER:-}" != 1 ]]; then
  set -e
  command -v docker >/dev/null 2>&1 || { echo "FAIL: required Docker runtime unavailable" >&2; exit 1; }
  docker info >/dev/null 2>&1 || { echo "FAIL: required Docker daemon unavailable" >&2; exit 1; }
  stage="$(mktemp -d)"
  tooling_build="$(mktemp -d)"
  if [[ -n "${SELFHOST_CERT_TEST_IMAGE:-}" ]]; then
    image="$SELFHOST_CERT_TEST_IMAGE"
  else
    image="concord-2504-cert-test-tools:ubuntu24"
    cat >"$tooling_build/Dockerfile" <<'DOCKERFILE'
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends bash coreutils openssl util-linux findutils gawk grep sed python3 \
    && rm -rf /var/lib/apt/lists/*
DOCKERFILE
    docker build --network default -t "$image" "$tooling_build" >/dev/null || {
      echo "FAIL: public tooling bootstrap image build failed" >&2; rm -rf "$stage" "$tooling_build"; exit 1;
    }
  fi
  docker image inspect "$image" >/dev/null 2>&1 || { echo "FAIL: prepared public-tool fixture image unavailable: $image" >&2; rm -rf "$stage" "$tooling_build"; exit 1; }
  trap 'rm -rf "$stage" "$tooling_build"' EXIT
  mkdir -p "$stage/scripts/tests" "$stage/infrastructure/deploy"
  install -m 0644 "$HERE/test-provision-cert.sh" "$stage/scripts/tests/test-provision-cert.sh"
  install -m 0644 "$HERE/provision_cert_replacement_test.py" "$stage/scripts/tests/provision_cert_replacement_test.py"
  install -m 0644 "$ROOT/infrastructure/deploy/provision-cert.sh" "$stage/infrastructure/deploy/provision-cert.sh"
  install -m 0644 "$ROOT/infrastructure/deploy/copy-certs.sh" "$stage/infrastructure/deploy/copy-certs.sh"
  chmod -R a+rX "$stage"
  docker run --rm --network none --user 0:0 \
    -e SELFHOST_CERT_TEST_INNER=1 -v "$stage:/fixture:ro" "$image" \
    bash /fixture/scripts/tests/test-provision-cert.sh
  rc=$?
  rm -rf "$stage"
  exit "$rc"
fi
[[ "$EUID" -eq 0 ]] || { echo "FAIL: provision acceptance requires actual uid 0" >&2; exit 1; }
bash "$HERE/test-provision-cert.sh" --import-san-boundary-only || exit $?
bash "$HERE/test-provision-cert.sh" --managed-hook-boundary-only || exit $?
bash "$HERE/test-provision-cert.sh" --replacement-boundary-only || exit $?
for utility in bash python3 openssl setpriv cmp stat grep cut sed tr install mkdir chmod chown cp rmdir readlink; do
  command -v "$utility" >/dev/null 2>&1 || { echo "FAIL: fixture image lacks required public utility: $utility" >&2; exit 1; }
done
TARGET="$ROOT/infrastructure/deploy/provision-cert.sh"
STUB="$(mktemp -d)"
WORK="$(mktemp -d)"
chmod 711 "$WORK"
trap 'rm -rf "$STUB" "$WORK" 2>/dev/null || true' EXIT
fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$(( fails + 1 )); }
nonroot_dir_write_control() {
  local directory="$1" probe="$1/.uid-65534-write-control"
  setpriv --reuid=65534 --regid=65534 --clear-groups mkdir "$probe" &&
    setpriv --reuid=65534 --regid=65534 --clear-groups rmdir "$probe"
}

# Privilege is proved with the namespace's actual uid, never an EUID override.
if command -v setpriv >/dev/null 2>&1; then
  setpriv --reuid=65534 --regid=65534 --clear-groups \
    bash "$ROOT/infrastructure/deploy/provision-cert.sh" --help >"$WORK/nonroot-help.out" 2>&1
  [[ $? -eq 0 ]] && grep -qi 'usage' "$WORK/nonroot-help.out" \
    && pass "nonroot help remains available" || fail "nonroot help failed"
  setpriv --reuid=65534 --regid=65534 --clear-groups \
    bash "$ROOT/infrastructure/deploy/provision-cert.sh" >"$WORK/nonroot-usage.out" 2>&1
  usage_rc=$?
  [[ "$usage_rc" -eq 2 ]] && grep -qi 'usage' "$WORK/nonroot-usage.out" \
    && pass "nonroot usage remains available with usage status" || fail "nonroot usage status/output incorrect"
  nonroot_live="$WORK/nonroot-live"
  mkdir -p "$nonroot_live" "$WORK/nonroot-hooks" "$WORK/nonroot-fanout"
  chown 65534:65534 "$nonroot_live" "$WORK/nonroot-hooks" "$WORK/nonroot-fanout"
  chmod 700 "$nonroot_live" "$WORK/nonroot-hooks" "$WORK/nonroot-fanout"
  if setpriv --reuid=65534 --regid=65534 --clear-groups mkdir "$nonroot_live/write-control" &&
      [[ -d "$nonroot_live/write-control" ]]; then
    rmdir "$nonroot_live/write-control"
    pass "actual nonroot identity can mutate its local source destination control"
  else fail "nonroot local destination control could not be prepared"; fi
  if setpriv --reuid=65534 --regid=65534 --clear-groups env \
      LE_LIVE_ROOT="$nonroot_live" HOOK_DIR="$WORK/nonroot-hooks" COTURN_CERT_ROOT="$WORK/nonroot-fanout" \
      COPY_CERTS_HOOK="$ROOT/infrastructure/deploy/copy-certs.sh" \
      bash "$ROOT/infrastructure/deploy/provision-cert.sh" local --host nonroot.test \
      >"$WORK/nonroot.out" 2>&1; then
    fail "recognized local provisioning accepted actual nonroot uid"
  else
    grep -Fq 'tls-provision: root required; certificate delivery not started' "$WORK/nonroot.out" &&
      [[ ! -e "$nonroot_live/nonroot.test" ]] &&
      pass "actual nonroot provisioning refuses before source mutation" || fail "nonroot refusal missing or after mutation"
  fi
else fail "setpriv is required for actual nonroot provisioning control"; fi

cat >"$STUB/docker" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in
  ps)
    # Query-aware private Docker projection: only the one owned fixture is
    # returned, and only when the caller's name/service/project filters match.
    [[ "${DOCKER_FIXTURE_MODE:-absent}" == running ]] || exit 0
    args=" $* "
    [[ "$args" == *"label=com.docker.compose.service=coturn"* ]] || exit 0
    if [[ "$args" == *"label=com.docker.compose.project="* && "$args" != *"label=com.docker.compose.project=concordvoice"* ]]; then exit 0; fi
    if [[ "$args" == *"name="* && "$args" != *"concordvoice-coturn"* ]]; then exit 0; fi
    printf '%s\n' '{"ID":"2504c0ffee11","Names":"concordvoice-coturn","Labels":{"com.docker.compose.project":"concordvoice","com.docker.compose.service":"coturn"}}'
    ;;
  inspect)
    [[ "${!#}" == 2504c0ffee11 ]] || exit 94
    printf '%s\n' '{"Name":"/concordvoice-coturn","Image":"coturn/coturn:4.18.0-alpine@sha256:c29135c08565810d37053f384584392b7c24b69570fc81a721b878d6fe63e04e","Project":"concordvoice","Service":"coturn","Status":"running","Running":true,"Paused":false,"Restarting":false}'
    ;;
  restart)
    [[ "${2:-}" == concordvoice-coturn || "${2:-}" == 2504c0ffee11 ]] || exit 95
    printf '%s\n' "${2}" >>"$DOCKER_RESTART_LOG"
    ;;
  *) echo "unexpected docker operation" >&2; exit 97 ;;
esac
EOF
cat >"$STUB/systemctl" <<'EOF'
#!/bin/bash
printf '%s\n' "$*" >>"$SYSTEMCTL_LOG"
case "$*" in
  'is-active --quiet nginx') [[ "${SYSTEMCTL_NGINX_ACTIVE:-0}" == 1 ]] ;;
  'reload nginx') [[ "${SYSTEMCTL_RELOAD_FAIL:-0}" != 1 ]] ;;
  *) exit 93 ;;
esac
EOF
chmod +x "$STUB/docker" "$STUB/systemctl"
chmod 755 "$STUB"
export PATH="$STUB:$PATH" COPY_CERTS_HOOK="$ROOT/infrastructure/deploy/copy-certs.sh"
export COTURN_CERT_ROOT="$WORK/fanout"
export SYSTEMCTL_LOG="$WORK/systemctl.calls" SYSTEMCTL_NGINX_ACTIVE=0 SYSTEMCTL_RELOAD_FAIL=0
export DOCKER_RESTART_LOG="$WORK/docker-restarts" DOCKER_FIXTURE_MODE=absent

# ── Task 1: skeleton ──────────────────────────────────────────────────────────
# Sourcing must NOT run main.
out="$(source "$TARGET" 2>&1)"; rc=$?
[[ $rc -eq 0 && -z "$out" ]] && pass "source does not run main" || fail "source ran main (rc=$rc out=$out)"

# --help prints usage, exit 0.
out="$(bash "$TARGET" --help 2>&1)"; rc=$?
[[ $rc -eq 0 ]] && echo "$out" | grep -qi 'usage' && pass "--help usage" || fail "--help broken (rc=$rc)"

# unknown mode → rc 2.
bash "$TARGET" bogus >/dev/null 2>&1; [[ $? -eq 2 ]] && pass "unknown mode rc=2" || fail "unknown mode wrong rc"

# derive_sans builds the DNS+IP SAN string.
( source "$TARGET"; san="$(derive_sans homelab.lan 10.0.0.5)"
  [[ "$san" == "DNS:homelab.lan,DNS:api.homelab.lan,DNS:media.homelab.lan,DNS:turn.homelab.lan,DNS:spa.homelab.lan,IP:10.0.0.5" ]] ) \
  && pass "derive_sans with IP" || fail "derive_sans with IP"

( source "$TARGET"; san="$(derive_sans homelab.lan "")"
  [[ "$san" == "DNS:homelab.lan,DNS:api.homelab.lan,DNS:media.homelab.lan,DNS:turn.homelab.lan,DNS:spa.homelab.lan" ]] ) \
  && pass "derive_sans no IP" || fail "derive_sans no IP"

# ── Task 2: local self-signed ─────────────────────────────────────────────────
LE_ROOT="$WORK/le"; mkdir -p "$LE_ROOT"
out="$( LE_LIVE_ROOT="$LE_ROOT" HOOK_DIR="$WORK/hooks" \
        bash "$TARGET" local --host homelab.lan --ip 10.0.0.5 --days 30 2>&1 )"; rc=$?
CERT="$LE_ROOT/homelab.lan/fullchain.pem"; KEY="$LE_ROOT/homelab.lan/privkey.pem"
[[ $rc -eq 0 && -f "$CERT" && -f "$KEY" ]] && pass "local writes cert+key" || fail "local missing files (rc=$rc out=$out)"
cmp -s "$CERT" "$COTURN_CERT_ROOT/coturn/cert.pem" && \
  cmp -s "$KEY" "$COTURN_CERT_ROOT/coturn/key.pem" && \
  pass "local source bytes reach the real copy hook" || fail "local copy hook did not deliver source bytes"
[[ "$(stat -c '%u:%g:%a' "$COTURN_CERT_ROOT/coturn/key.pem")" == "65534:65533:400" ]] \
  && pass "local delivered key metadata" || fail "local delivered key metadata incorrect"
grep -Fq 'is-active --quiet nginx' "$SYSTEMCTL_LOG" &&
  ! grep -Fq 'reload nginx' "$SYSTEMCTL_LOG" &&
  pass "provisioning controls nginx as inactive and does not reload it" || fail "nginx inactive control did not reach expected seam"

# Required sibling failure is distinct from a successful no-op hook and must
# happen before local source creation. A valid local control above reached the
# real sibling and delivered exact bytes.
missing_live="$WORK/missing-hook-live"
if COPY_CERTS_HOOK="$WORK/no-such-copy-hook" LE_LIVE_ROOT="$missing_live" \
    bash "$TARGET" local --host missing-hook.test >"$WORK/missing-hook.out" 2>&1; then
  fail "local provisioning accepted unavailable required copy hook"
else
  grep -Fq 'tls-provision: required copy hook unavailable; delivery not complete' "$WORK/missing-hook.out" &&
    [[ ! -e "$missing_live/missing-hook.test" ]] && pass "missing sibling is rejected before local issuance" || fail "missing sibling failure late or misclassified"
fi

san="$(openssl x509 -in "$CERT" -noout -text 2>/dev/null | grep -A1 'Subject Alternative Name' | tail -1)"
san_ok=1
for want in homelab.lan api.homelab.lan media.homelab.lan turn.homelab.lan spa.homelab.lan 10.0.0.5; do
  echo "$san" | grep -q "$want" || { fail "local SAN missing $want"; san_ok=0; }
done
[[ $san_ok -eq 1 ]] && pass "local SAN complete"

perm="$(stat -c '%a' "$KEY" 2>/dev/null || stat -f '%Lp' "$KEY")"
[[ "$perm" == "600" ]] && pass "local key 0600" || fail "local key perm=$perm"

# No private key bytes in output.
echo "$out" | grep -q 'PRIVATE KEY' && fail "local leaked key material" || pass "local no key in log"

# A first-start delivery is prepared but must not be described as a restarted
# or TLS-ready consumer. The public operator line distinguishes the stage.
grep -Fq 'coturn is not started; delivery is prepared for first start.' <<<"$out" &&
  grep -Fq 'TLS readiness has not been checked.' <<<"$out" &&
  grep -Fq 'Certificate source and coturn delivery complete.' <<<"$out" &&
  pass "first-start delivery reports prepared and unchecked stage" ||
  fail "first-start delivery stage guidance missing or overstated"

# The same real sibling hook sees one matching, running owned coturn fixture.
# It must restart that consumer and report restart without claiming readiness.
DOCKER_FIXTURE_MODE=running
running_live="$WORK/running-live"
out="$(LE_LIVE_ROOT="$running_live" HOOK_DIR="$WORK/running-hooks" \
      bash "$TARGET" local --host running.test --days 30 2>&1)"; rc=$?
[[ $rc -eq 0 ]] && pass "running coturn local delivery succeeds" || fail "running coturn delivery failed (rc=$rc out=$out)"
[[ "$(wc -l <"$DOCKER_RESTART_LOG" 2>/dev/null)" -eq 1 ]] &&
  pass "running coturn fixture was restarted exactly once" || fail "running coturn fixture was not restarted exactly once"
grep -Fq 'coturn was restarted; TLS readiness has not been checked.' <<<"$out" &&
  grep -Fq 'Certificate source and coturn delivery complete.' <<<"$out" &&
  ! grep -Eiq 'TLS is ready|TLS readiness (has been checked|is ready|passed|verified)' <<<"$out" &&
  pass "running delivery reports restarted but unchecked stage" ||
  fail "running delivery stage guidance missing or claims TLS readiness"
DOCKER_FIXTURE_MODE=absent

# A zero-exit copy hook is insufficient unless it returns the exact bounded
# stage protocol. This wrapper reaches the real copy hook first, then supplies
# a malformed stage while an ambient legacy value attempts to look successful.
ACTUAL_COPY_SUCCEEDED="$WORK/protocol-copy-succeeded"
REAL_COPY_CERTS_HOOK="$ROOT/infrastructure/deploy/copy-certs.sh"
PROTOCOL_HOOK="$WORK/unrecognized-stage-hook"
cat >"$PROTOCOL_HOOK" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
bash "$REAL_COPY_CERTS_HOOK" >/dev/null 2>&1
: >"$ACTUAL_COPY_SUCCEEDED"
printf '%s\n' 'copy-certs: certificate pair delivery complete.' 'copy-certs: coturn-stage=unknown'
EOF
chmod 755 "$PROTOCOL_HOOK"
protocol_live="$WORK/protocol-live"
out="$(REAL_COPY_CERTS_HOOK="$REAL_COPY_CERTS_HOOK" ACTUAL_COPY_SUCCEEDED="$ACTUAL_COPY_SUCCEEDED" \
      COPY_CERTS_HOOK="$PROTOCOL_HOOK" COPY_COTURN_STAGE=restarted \
      LE_LIVE_ROOT="$protocol_live" HOOK_DIR="$WORK/protocol-hooks" \
      bash "$TARGET" local --host protocol.test --days 30 2>&1)"; rc=$?
if [[ $rc -ne 0 ]] && grep -Fq 'coturn delivery result unavailable; delivery not complete' <<<"$out" &&
   [[ -f "$ACTUAL_COPY_SUCCEEDED" ]] &&
   cmp -s "$protocol_live/protocol.test/fullchain.pem" "$COTURN_CERT_ROOT/coturn/cert.pem" &&
   cmp -s "$protocol_live/protocol.test/privkey.pem" "$COTURN_CERT_ROOT/coturn/key.pem" &&
   ! grep -Fq 'Certificate source and coturn delivery complete.' <<<"$out" &&
   ! grep -Fq 'coturn was restarted; TLS readiness has not been checked.' <<<"$out"; then
  pass "unrecognized hook stage refuses after successful real copy without claiming completion"
else
  fail "unrecognized hook stage or ambient stage value bypassed the bounded protocol (rc=$rc out=$out)"
fi

# The real hook can copy the pair successfully and still fail at an active
# nginx reload. Provisioning must propagate that downstream failure rather than
# publishing any generic delivery or coturn-stage success guidance.
nginx_failure_live="$WORK/nginx-failure-live"
: >"$SYSTEMCTL_LOG"
out="$(SYSTEMCTL_NGINX_ACTIVE=1 SYSTEMCTL_RELOAD_FAIL=1 \
      LE_LIVE_ROOT="$nginx_failure_live" HOOK_DIR="$WORK/nginx-failure-hooks" \
      bash "$TARGET" local --host nginx-failure.test --days 30 2>&1)"; rc=$?
if [[ $rc -ne 0 ]] &&
   grep -Fq 'copy-certs: nginx reload failed; delivery not complete' <<<"$out" &&
   grep -Fqx 'is-active --quiet nginx' "$SYSTEMCTL_LOG" &&
   grep -Fqx 'reload nginx' "$SYSTEMCTL_LOG" &&
   cmp -s "$nginx_failure_live/nginx-failure.test/fullchain.pem" "$COTURN_CERT_ROOT/coturn/cert.pem" &&
   cmp -s "$nginx_failure_live/nginx-failure.test/privkey.pem" "$COTURN_CERT_ROOT/coturn/key.pem" &&
   ! grep -Fq 'Certificate source and coturn delivery complete.' <<<"$out" &&
   ! grep -Fq 'coturn is not started; delivery is prepared for first start.' <<<"$out" &&
   ! grep -Fq 'coturn was restarted; TLS readiness has not been checked.' <<<"$out"; then
  pass "nginx reload failure propagates after real copy without success guidance"
else
  fail "nginx reload failure was hidden or reported after success (rc=$rc out=$out)"
fi
SYSTEMCTL_NGINX_ACTIVE=0 SYSTEMCTL_RELOAD_FAIL=0

# ── Task 3: import (fail-closed) ──────────────────────────────────────────────
IMP="$WORK/imp"
mkdir -p "$IMP"
# Good pair: the deployment base plus every shared nginx/coturn consumer.
openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 30 \
  -keyout "$IMP/good.key" -out "$IMP/good.pem" \
  -subj "/CN=good.lan" -addext "subjectAltName=DNS:good.lan,DNS:api.good.lan,DNS:media.good.lan,DNS:turn.good.lan" >/dev/null 2>&1
NONROOT_IMP="$WORK/nonroot-import"
mkdir -p "$NONROOT_IMP/input" "$NONROOT_IMP/live" "$NONROOT_IMP/hooks" "$NONROOT_IMP/fanout"
cp "$IMP/good.pem" "$NONROOT_IMP/input/good.pem"
cp "$IMP/good.key" "$NONROOT_IMP/input/good.key"
chown -R 65534:65534 "$NONROOT_IMP"
chmod 700 "$NONROOT_IMP" "$NONROOT_IMP/input" "$NONROOT_IMP/live" "$NONROOT_IMP/hooks" "$NONROOT_IMP/fanout"
chmod 644 "$NONROOT_IMP/input/good.pem"
chmod 600 "$NONROOT_IMP/input/good.key"
if setpriv --reuid=65534 --regid=65534 --clear-groups test -r "$NONROOT_IMP/input/good.key" &&
   nonroot_dir_write_control "$NONROOT_IMP/live" &&
   nonroot_dir_write_control "$NONROOT_IMP/hooks" &&
   nonroot_dir_write_control "$NONROOT_IMP/fanout" &&
   setpriv --reuid=65534 --regid=65534 --clear-groups test -x "$STUB/docker" &&
   setpriv --reuid=65534 --regid=65534 --clear-groups test -x "$STUB/systemctl"; then
  pass "nonroot import has readable key, writable destinations, and reachable operation stubs"
else fail "nonroot import fixture permissions are invalid"; fi
if setpriv --reuid=65534 --regid=65534 --clear-groups env \
    LE_LIVE_ROOT="$NONROOT_IMP/live" HOOK_DIR="$NONROOT_IMP/hooks" \
    COTURN_CERT_ROOT="$NONROOT_IMP/fanout/certs" COPY_CERTS_HOOK="$ROOT/infrastructure/deploy/copy-certs.sh" \
    PATH="$STUB:/usr/bin:/bin" /bin/bash "$TARGET" import \
    --cert "$NONROOT_IMP/input/good.pem" --key "$NONROOT_IMP/input/good.key" --host good.lan \
    >"$WORK/nonroot-import.out" 2>&1; then
  fail "actual nonroot import accepted"
else
  grep -Fq 'tls-provision: root required; certificate delivery not started' "$WORK/nonroot-import.out" &&
    [[ ! -e "$NONROOT_IMP/live/good.lan" && ! -e "$NONROOT_IMP/fanout/certs" ]] &&
    pass "actual nonroot import refuses before source/delivery mutation" || fail "nonroot import refusal late or misclassified"
fi

# The configured required-copy seam must be a readable regular file before an
# otherwise-valid import writes either source artifact. A directory is not a
# valid hook even though it exists.
for hook_shape in missing directory; do
  invalid_hook="$WORK/import-hook-$hook_shape"
  # This case owns a fresh source root; IMP_LE belongs to later crypto cases.
  import_hook_live="$WORK/import-hook-live-$hook_shape"
  [[ "$hook_shape" == directory ]] && mkdir -p "$invalid_hook"
  if COPY_CERTS_HOOK="$invalid_hook" LE_LIVE_ROOT="$import_hook_live" HOOK_DIR="$WORK/import-hooks-$hook_shape" \
      COTURN_CERT_ROOT="$WORK/import-fanout-$hook_shape" \
      bash "$TARGET" import --cert "$IMP/good.pem" --key "$IMP/good.key" --host good.lan \
      >"$WORK/import-hook-$hook_shape.out" 2>&1; then
    fail "import accepted required copy hook $hook_shape"
  else
    grep -Fq 'tls-provision: required copy hook unavailable; delivery not complete' "$WORK/import-hook-$hook_shape.out" &&
      [[ ! -e "$import_hook_live/good.lan" && ! -e "$WORK/import-fanout-$hook_shape" ]] &&
      pass "import $hook_shape required hook fails before source mutation" || fail "import $hook_shape hook refusal late or misclassified"
  fi
done
# Unrelated key (for the mismatch case).
openssl genrsa -out "$IMP/other.key" 2048 >/dev/null 2>&1
# Expired cert: issued via a throwaway CA with -startdate/-enddate in the past
# (openssl ca supports this on OpenSSL 3.0; req -not_before does not). No SAN
# needed — import rejects on expiry before the SAN check.
CA="$WORK/ca"; mkdir -p "$CA/newcerts"; : >"$CA/index.txt"; echo 1000 >"$CA/serial"
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -keyout "$CA/ca.key" -out "$CA/ca.pem" -subj "/CN=Test CA" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -keyout "$IMP/exp.key" -out "$CA/exp.csr" -subj "/CN=good.lan" >/dev/null 2>&1
cat >"$CA/openssl.cnf" <<EOF
[ca]
default_ca = CA_default
[CA_default]
dir = $CA
database = \$dir/index.txt
serial = \$dir/serial
new_certs_dir = \$dir/newcerts
certificate = \$dir/ca.pem
private_key = \$dir/ca.key
default_md = sha256
policy = pol
[pol]
commonName = optional
EOF
openssl ca -batch -config "$CA/openssl.cnf" -startdate 20200101000000Z -enddate 20200102000000Z \
  -in "$CA/exp.csr" -out "$IMP/exp.pem" >/dev/null 2>&1
# Fixture-integrity guard — fail LOUDLY if openssl ca silently produced no/invalid
# cert (would otherwise make the expired test pass for the wrong reason on a
# different OpenSSL build). The expired test below must exercise the checkend path.
openssl x509 -in "$IMP/exp.pem" -noout -enddate >/dev/null 2>&1 || fail "expired fixture is not a parseable cert (openssl ca failed)"
openssl x509 -in "$IMP/exp.pem" -noout -checkend 0 >/dev/null 2>&1 && fail "expired fixture is NOT expired (checkend says valid)"

IMP_LE="$WORK/le_imp"
imp() { LE_LIVE_ROOT="$IMP_LE" HOOK_DIR="$WORK/hooks" bash "$TARGET" import "$@" >/dev/null 2>&1; }
imp_err() { LE_LIVE_ROOT="$IMP_LE" HOOK_DIR="$WORK/hooks" bash "$TARGET" import "$@" 2>&1; }

# Good pair installs.
rm -rf "$IMP_LE"
imp --cert "$IMP/good.pem" --key "$IMP/good.key" --host good.lan \
  && [[ -f "$IMP_LE/good.lan/fullchain.pem" ]] && pass "import good installs" || fail "import good failed"
cmp -s "$IMP/good.pem" "$COTURN_CERT_ROOT/coturn/cert.pem" && \
  cmp -s "$IMP/good.key" "$COTURN_CERT_ROOT/coturn/key.pem" && \
  pass "import source bytes reach the real copy hook" || fail "import copy hook did not deliver source bytes"
# Mismatched pair → non-zero, nothing written.
rm -rf "$IMP_LE"
if imp --cert "$IMP/good.pem" --key "$IMP/other.key" --host good.lan; then
  fail "import mismatch accepted"
else
  [[ ! -d "$IMP_LE/good.lan" ]] && pass "import mismatch rejected, no write" || fail "import mismatch wrote files"
fi
# Expired → non-zero, AND for the RIGHT reason (the checkend path, not an
# unrelated SPKI/file-read failure from a broken fixture).
rm -rf "$IMP_LE"
exp_err="$(imp_err --cert "$IMP/exp.pem" --key "$IMP/exp.key" --host good.lan)"
if imp --cert "$IMP/exp.pem" --key "$IMP/exp.key" --host good.lan; then
  fail "import expired accepted"
else
  printf '%s' "$exp_err" | grep -q 'is expired' && pass "import expired rejected (for expiry reason)" || fail "import expired wrong reason ($exp_err)"
fi
# Wrong SAN (cert covers good.lan, host=other.lan) → non-zero.
rm -rf "$IMP_LE"
imp --cert "$IMP/good.pem" --key "$IMP/good.key" --host other.lan \
  && fail "import wrong-SAN accepted" || pass "import wrong-SAN rejected"

# ── #1919 review fixes: SAN-matching correctness + host charset gate ───────────
# --host is a deployment base, not one leaf. Preserve one-label wildcard
# admission for its service children while also covering that base explicitly.
openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 30 \
  -keyout "$IMP/wild.key" -out "$IMP/wild.pem" \
  -subj "/CN=wild" -addext "subjectAltName=DNS:media.example.com,DNS:*.media.example.com" >/dev/null 2>&1
rm -rf "$IMP_LE"
imp --cert "$IMP/wild.pem" --key "$IMP/wild.key" --host media.example.com \
  && pass "import wildcard covers subdomain" || fail "import wildcard subdomain rejected"
rm -rf "$IMP_LE"
imp --cert "$IMP/wild.pem" --key "$IMP/wild.key" --host example.com \
  && fail "import wildcard wrongly covered apex" || pass "import wildcard does NOT cover apex"
# Over-broad *.com cert must NOT cover example.com (the Gitar/WF security finding).
openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 30 \
  -keyout "$IMP/tld.key" -out "$IMP/tld.pem" \
  -subj "/CN=tld" -addext "subjectAltName=DNS:*.com" >/dev/null 2>&1
rm -rf "$IMP_LE"
imp --cert "$IMP/tld.pem" --key "$IMP/tld.key" --host example.com \
  && fail "import accepted over-broad *.com" || pass "import rejects over-broad *.com wildcard"
# Dot-bypass: a cert SAN DNS:exampleXcom must NOT satisfy host example.com
# (old ERE made '.' match any char; literal token match closes it).
openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 30 \
  -keyout "$IMP/dot.key" -out "$IMP/dot.pem" \
  -subj "/CN=dot" -addext "subjectAltName=DNS:exampleXcom" >/dev/null 2>&1
rm -rf "$IMP_LE"
imp --cert "$IMP/dot.pem" --key "$IMP/dot.key" --host example.com \
  && fail "import dot-bypass accepted" || pass "import rejects ERE dot-bypass"
# Host charset gate: '/' (subj-DN injection) and regex metachars rejected (rc 2).
imp --cert "$IMP/good.pem" --key "$IMP/good.key" --host 'good.lan/O=Evil' \
  && fail "import accepted '/' in host" || pass "import rejects '/' in host"
imp --cert "$IMP/good.pem" --key "$IMP/good.key" --host 'g.*d.lan' \
  && fail "import accepted regex-metachar host" || pass "import rejects regex-metachar host"

# ── #1919 review fixes: local/ip charset gate (SAN/subj injection) ─────────────
( LE_LIVE_ROOT="$WORK/le_inj" HOOK_DIR="$WORK/h" bash "$TARGET" local --host 'evil.lan/O=x' ) >/dev/null 2>&1 \
  && fail "local accepted '/' host" || pass "local rejects '/' host (subj-DN injection)"
( LE_LIVE_ROOT="$WORK/le_inj" HOOK_DIR="$WORK/h" bash "$TARGET" local --host homelab.lan --ip '1.2.3.4,DNS:evil.com' ) >/dev/null 2>&1 \
  && fail "local accepted ',' in ip" || pass "local rejects ',' in ip (SAN injection)"
( LE_LIVE_ROOT="$WORK/le_inj" HOOK_DIR="$WORK/h" bash "$TARGET" local --host homelab.lan --days abc ) >/dev/null 2>&1 \
  && fail "local accepted non-numeric --days" || pass "local rejects non-numeric --days"
( LE_LIVE_ROOT="$WORK/le_inj" HOOK_DIR="$WORK/h" bash "$TARGET" local --host homelab.lan --days 0 ) >/dev/null 2>&1 \
  && fail "local accepted --days 0 (already-expired cert)" || pass "local rejects --days 0"

# ── #3561 review: validate_ip is the same predicate as install-selfhost.sh's
# validate_public_ip (the installer prints `--ip <value>` for this script, so the
# two must accept and refuse exactly the same strings). The IPv6 branch requires at
# least one ':' and allows dots after it (IPv4-mapped / NAT64). Empty is accepted.
for good in 1.2.3.4 2001:db8::1 ::ffff:192.0.2.1 64:ff9b::203.0.113.10 ''; do
  ( source "$TARGET"; validate_ip "$good" ) >/dev/null 2>&1; vi_rc=$?
  [[ $vi_rc -eq 0 ]] \
    && pass "validate_ip accepts '${good:-empty}'" \
    || fail "validate_ip rc=$vi_rc for '${good:-empty}', want 0"
done
# rc exactly 1 (127 = missing function would satisfy a bare "non-zero" check).
# 'cafe' is all hex digits with no colon: not an address.
for bad in cafe '1.2.3.4;id' 'fe80::1%eth0' '1.2.3.4:' '[2001:db8::1]'; do
  ( source "$TARGET"; validate_ip "$bad" ) >/dev/null 2>&1; vi_rc=$?
  [[ $vi_rc -eq 1 ]] \
    && pass "validate_ip rejects '$bad' with rc 1" \
    || fail "validate_ip rc=$vi_rc for '$bad', want 1"
done
# End to end: a colon-less hex --ip is refused (rc 2) and writes nothing.
rm -rf "$WORK/le_cafe"
( LE_LIVE_ROOT="$WORK/le_cafe" HOOK_DIR="$WORK/h" bash "$TARGET" local --host homelab.lan --ip cafe ) >/dev/null 2>&1; vi_rc=$?
[[ $vi_rc -eq 2 && ! -d "$WORK/le_cafe/homelab.lan" ]] \
  && pass "local rejects a colon-less hex --ip (rc 2, nothing written)" \
  || fail "local accepted --ip cafe or wrong rc (rc=$vi_rc)"

# ── Task 4: letsencrypt (certbot mocked) ──────────────────────────────────────
# A renewal executable can bind its lineage before the shared helper. Capture
# the actual install input, rather than expecting a verbatim helper copy.
cat >"$STUB/capture-renewal-candidate" <<'EOF'
#!/usr/bin/env bash
artifact=$2
if [[ "${2##*/}" == installed && "${2%/*}" == */.copy-certs.* ]]; then
  artifact=${2%/*}; artifact=${artifact%/*}/copy-certs.sh
elif [[ "${2##*/}" != copy-certs.sh ]]; then exit 0; fi
cp -- "$1" "$artifact.candidate" &&
  printf '%s\n' "$1" >"$artifact.candidate-operand" &&
  printf '%s\n' "$2" >"$artifact.prepared-operand" &&
  stat -Lc '%d:%i' "$1" >"$artifact.candidate-inode" || exit 96
printf '%s\n' "$artifact"
EOF
chmod +x "$STUB/capture-renewal-candidate"
installed_renewal_matches_candidate() {
  local artifact=$1
  [[ -f "$artifact" && ! -L "$artifact" && -f "$artifact.candidate" ]] &&
    cmp -s "$artifact.candidate" "$artifact" &&
    [[ "$(stat -Lc '%u:%g:%a' "$artifact")" == '0:0:755' &&
       "$(stat -Lc '%d:%i' "$artifact")" != "$(cat "$artifact.candidate-inode")" ]] &&
    [[ ! "$artifact" -ef "$artifact.candidate" ]]
}
REAL_CERT_INSTALL="$(command -v install)"; export REAL_CERT_INSTALL
cat >"$STUB/install" <<'EOF'
#!/usr/bin/env bash
args=("$@")
arg_count=${#args[@]}
capture-renewal-candidate "${args[arg_count-2]:-}" "${args[arg_count-1]:-}" >/dev/null || exit 96
exec "$REAL_CERT_INSTALL" "$@"
EOF
chmod +x "$STUB/install"
mkdir -p "$WORK/issued"
openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 30 \
  -keyout "$WORK/issued/privkey.pem" -out "$WORK/issued/fullchain.pem" \
  -subj "/CN=example.com" -addext "subjectAltName=DNS:example.com,DNS:api.example.com,DNS:media.example.com,DNS:turn.example.com" >/dev/null 2>&1
chmod 600 "$WORK/issued/privkey.pem"
cat >"$STUB/certbot" <<'EOF'
#!/usr/bin/env bash
echo "certbot $*" >>"$CERTBOT_LOG"
[[ "${CERTBOT_MODE:-pair}" == missing ]] && exit 0
[[ "${CERTBOT_MODE:-pair}" == fail ]] && exit 1
certbot_args=("$@")
# Simulate issuance, or a later renewal of the exact owned lineage. Renewal
# executes every installed executable directory hook, including an uploader;
# modelling only copy-certs would hide the inherited managed-hook boundary.
if [[ "${1:-}" == renew ]]; then
  [[ $# -eq 3 && $2 == --cert-name && -n $3 ]] || exit 97
  d="$3"
  [[ -f "$LE_LIVE_ROOT/$d/fullchain.pem" && -f "$LE_LIVE_ROOT/$d/privkey.pem" ]] || exit 97
else
  [[ "${1:-}" == certonly ]] || exit 97
  d=""; while (( $# )); do [[ "$1" == "-d" ]] && { d="${d:-$2}"; }; shift; done
  [[ -n "$d" ]] || exit 97
fi
mkdir -p "$LE_LIVE_ROOT/$d"
if [[ "${CERTBOT_MODE:-pair}" != existing ]]; then
  cp "$VALID_PAIR_ROOT/fullchain.pem" "$LE_LIVE_ROOT/$d/fullchain.pem"
  cp "$VALID_PAIR_ROOT/privkey.pem" "$LE_LIVE_ROOT/$d/privkey.pem"
  chmod 600 "$LE_LIVE_ROOT/$d/privkey.pem"
  # Certbot runs installed deploy hooks after successful issuance/renewal.
  run_deploy_hooks=1
  for certbot_arg in "${certbot_args[@]}"; do
    [[ "$certbot_arg" == --no-directory-hooks ]] && run_deploy_hooks=0
  done
  if [[ "$run_deploy_hooks" == 1 ]]; then
    shopt -s nullglob
    for deploy_hook in "$HOOK_DIR"/*.sh; do
      [[ -x "$deploy_hook" ]] || continue
      if [[ "${deploy_hook##*/}" == copy-certs.sh ]]; then
        printf 'deploy-hook\n' >>"$CERTBOT_HOOK_LOG"
      else
        printf 'directory-hook=%s\n' "${deploy_hook##*/}" >>"$CERTBOT_HOOK_LOG"
      fi
      hook_rc=0
      RENEWED_LINEAGE="$LE_LIVE_ROOT/$d" bash "$deploy_hook" || hook_rc=$?
      # Certbot logs deploy-hook failures and continues processing successfully;
      # a caller that needs delivery certainty must check the actual delivery
      # path rather than treating Certbot's zero exit as hook success.
      printf 'deploy-hook-status=%s\n' "$hook_rc" >>"$CERTBOT_HOOK_LOG"
    done
  fi
fi
printf 'certbot-exit=0\n' >>"$CERTBOT_HOOK_LOG"
EOF
chmod +x "$STUB/certbot"
LE_LE="$WORK/le_le"; export CERTBOT_LOG="$WORK/certbot.log"; : >"$CERTBOT_LOG"
CERTBOT_HOOK_LOG="$WORK/certbot-hooks.log"; export CERTBOT_HOOK_LOG; : >"$CERTBOT_HOOK_LOG"
# LE must admit the configured regular sibling hook before installing renewal
# hooks or invoking Certbot. Pair invalid-hook cases with the successful LE
# source/fan-out control below.
for hook_shape in missing directory; do
  invalid_hook="$WORK/le-hook-$hook_shape"
  [[ "$hook_shape" == directory ]] && mkdir -p "$invalid_hook"
  : >"$CERTBOT_LOG"
  if COPY_CERTS_HOOK="$invalid_hook" PATH="$STUB:$PATH" VALID_PAIR_ROOT="$WORK/issued" \
      LE_LIVE_ROOT="$WORK/le-hook-live-$hook_shape" HOOK_DIR="$WORK/le-hook-installed-$hook_shape" \
      COTURN_CERT_ROOT="$WORK/le-hook-fanout-$hook_shape" \
      bash "$TARGET" letsencrypt --domain hookfail.example --email ops@example.com \
      >"$WORK/le-hook-$hook_shape.out" 2>&1; then
    fail "letsencrypt accepted required copy hook $hook_shape"
  else
    grep -Fq 'tls-provision: required copy hook unavailable; delivery not complete' "$WORK/le-hook-$hook_shape.out" &&
      [[ ! -s "$CERTBOT_LOG" && ! -e "$WORK/le-hook-live-$hook_shape/hookfail.example" &&
         ! -e "$WORK/le-hook-installed-$hook_shape/copy-certs.sh" &&
         ! -e "$WORK/le-hook-fanout-$hook_shape" ]] &&
      pass "letsencrypt $hook_shape required hook fails before install and issuance" || fail "letsencrypt $hook_shape hook refusal late or misclassified"
  fi
done
# Restore a clean call log after deliberate preflight-refusal cases.
# The next root LE success must observe only its own issuance call.
: >"$CERTBOT_LOG"
out="$( PATH="$STUB:$PATH" VALID_PAIR_ROOT="$WORK/issued" LE_LIVE_ROOT="$LE_LE" HOOK_DIR="$WORK/hooks2" \
        bash "$TARGET" letsencrypt --domain example.com --email ops@example.com 2>&1 )"; rc=$?
line="$(cat "$CERTBOT_LOG")"
[[ $rc -eq 0 ]] && pass "letsencrypt rc=0" || fail "letsencrypt rc=$rc out=$out"
echo "$line" | grep -q -- '--reuse-key'  && pass "LE --reuse-key preserved" || fail "LE missing --reuse-key"
echo "$line" | grep -q -- '--webroot'    && pass "LE --webroot preserved"   || fail "LE missing --webroot"
le_san_ok=1
for d in example.com api.example.com media.example.com turn.example.com; do
  echo "$line" | grep -q -- "-d $d" || { fail "LE missing SAN -d $d"; le_san_ok=0; }
done
[[ $le_san_ok -eq 1 ]] && pass "LE SAN set derived"
cmp -s "$LE_LE/example.com/fullchain.pem" "$COTURN_CERT_ROOT/coturn/cert.pem" && \
  cmp -s "$LE_LE/example.com/privkey.pem" "$COTURN_CERT_ROOT/coturn/key.pem" \
  && pass "deterministic LE valid pair reaches the real copy hook" || fail "LE pair not delivered"
installed_renewal_matches_candidate "$WORK/hooks2/copy-certs.sh" && \
  pass "LE installed artifact is the shipped executable owned by root" || fail "LE installed artifact control mismatched bytes or metadata"
[[ "$(grep -cFx 'deploy-hook' "$CERTBOT_HOOK_LOG" 2>/dev/null || true)" -le 1 ]] && \
  pass "successful issuance did not invoke the installed deploy hook more than once" || fail "Certbot invoked its installed deploy hook repeatedly"

# Certbot already delivered the issued lineage through its installed deploy
# hook. The actual consumer effects must occur once, which catches the former
# unconditional second complete_delivery path.
LE_ONCE="$WORK/le-automatic-once"; : >"$DOCKER_RESTART_LOG"; : >"$SYSTEMCTL_LOG"; : >"$CERTBOT_HOOK_LOG"
out="$(CERTBOT_MODE=pair SYSTEMCTL_NGINX_ACTIVE=1 PATH="$STUB:$PATH" VALID_PAIR_ROOT="$WORK/issued" \
      LE_LIVE_ROOT="$LE_ONCE" HOOK_DIR="$WORK/hooks-once" COTURN_CERT_ROOT="$WORK/fanout-once" \
      DOCKER_FIXTURE_MODE=running bash "$TARGET" letsencrypt --domain once.example --email ops@example.com 2>&1)"; rc=$?
[[ $rc -eq 0 && "$(wc -l <"$DOCKER_RESTART_LOG")" -eq 1 && \
   "$(grep -cFx 'reload nginx' "$SYSTEMCTL_LOG" 2>/dev/null || true)" -eq 1 && \
   "$(grep -cFx 'deploy-hook' "$CERTBOT_HOOK_LOG" 2>/dev/null || true)" -le 1 ]] && \
  pass "issuance delivers and restarts/reloads each consumer exactly once" || \
  fail "issuance double-delivered or skipped consumer effect (rc=$rc out=$out restarts=$(wc -l <"$DOCKER_RESTART_LOG") systemctl=$(cat "$SYSTEMCTL_LOG"))"

# A successful Certbot result with an already-present lineage represents the
# no-issuance path: Certbot does not run deploy hooks, so explicit fan-out is
# still required and must perform each consumer effect exactly once.
LE_EXISTING="$WORK/le-existing"; mkdir -p "$LE_EXISTING/reused.example"
cp "$WORK/issued/fullchain.pem" "$LE_EXISTING/reused.example/fullchain.pem"
cp "$WORK/issued/privkey.pem" "$LE_EXISTING/reused.example/privkey.pem"
chmod 600 "$LE_EXISTING/reused.example/privkey.pem"
: >"$DOCKER_RESTART_LOG"; : >"$SYSTEMCTL_LOG"; : >"$CERTBOT_HOOK_LOG"
out="$(CERTBOT_MODE=existing SYSTEMCTL_NGINX_ACTIVE=1 PATH="$STUB:$PATH" VALID_PAIR_ROOT="$WORK/issued" \
      LE_LIVE_ROOT="$LE_EXISTING" HOOK_DIR="$WORK/hooks-existing" COTURN_CERT_ROOT="$WORK/fanout-existing" \
      DOCKER_FIXTURE_MODE=running bash "$TARGET" letsencrypt --domain reused.example --email ops@example.com 2>&1)"; rc=$?
[[ $rc -eq 0 && "$(grep -cFx 'deploy-hook' "$CERTBOT_HOOK_LOG" 2>/dev/null || true)" -eq 0 && \
   "$(wc -l <"$DOCKER_RESTART_LOG")" -eq 1 && \
   "$(grep -cFx 'reload nginx' "$SYSTEMCTL_LOG" 2>/dev/null || true)" -eq 1 ]] && \
  pass "existing-certificate path manually delivers once without a Certbot hook" || \
  fail "existing-certificate delivery was missing, duplicated, or automatically invoked (rc=$rc out=$out)"

# Certbot records a failing deploy hook but still exits successfully. The
# provisioning command must independently reject incomplete delivery and omit
# its delivery-complete guidance. The hook failure is exercised at the owned
# systemctl seam after real certificate validation and copy.
: >"$DOCKER_RESTART_LOG"; : >"$SYSTEMCTL_LOG"; : >"$CERTBOT_HOOK_LOG"
if CERTBOT_MODE=pair SYSTEMCTL_NGINX_ACTIVE=1 SYSTEMCTL_RELOAD_FAIL=1 PATH="$STUB:$PATH" \
    VALID_PAIR_ROOT="$WORK/issued" LE_LIVE_ROOT="$WORK/le-hook-failure" HOOK_DIR="$WORK/hooks-hook-failure" \
    COTURN_CERT_ROOT="$WORK/fanout-hook-failure" DOCKER_FIXTURE_MODE=running \
    bash "$TARGET" letsencrypt --domain hook-failure.example --email ops@example.com \
    >"$WORK/hook-failure.out" 2>&1; then
  fail "Certbot hook failure was accepted"
else
  ! grep -Fq 'Certificate source and coturn delivery complete.' "$WORK/hook-failure.out" && \
    grep -q '^certbot-exit=0$' "$CERTBOT_HOOK_LOG" && \
    grep -qFx 'reload nginx' "$SYSTEMCTL_LOG" && \
    pass "delivery hook failure is rejected despite Certbot success and has no completion guidance" || \
    fail "hook failure was not recorded or output claimed delivery completion"
fi

# Certbot failure before issuance must likewise skip the deploy hook and any
# generic success guidance.
: >"$CERTBOT_HOOK_LOG"
if CERTBOT_MODE=fail PATH="$STUB:$PATH" VALID_PAIR_ROOT="$WORK/issued" LE_LIVE_ROOT="$WORK/le-certbot-failure" \
    HOOK_DIR="$WORK/hooks-certbot-failure" COTURN_CERT_ROOT="$WORK/fanout-certbot-failure" \
    bash "$TARGET" letsencrypt --domain issuance-failure.example --email ops@example.com \
    >"$WORK/certbot-failure.out" 2>&1; then
  fail "Certbot failure was accepted"
else
  ! grep -Eq 'TLS certificate obtained\.|Certificate source and coturn delivery complete\.' "$WORK/certbot-failure.out" && \
    ! grep -q '^deploy-hook$' "$CERTBOT_HOOK_LOG" && \
    grep -Fq 'tls-provision: certificate issuance failed; delivery not complete' "$WORK/certbot-failure.out" && \
    pass "Certbot failure cannot report issuance or delivery completion" || \
    fail "Certbot failure output claimed completion or invoked a delivery hook"
fi

# The actual nonroot LE control has a valid, readable source fixture, writable
# lineage/hook/fanout destinations, reachable executables, and a Certbot log
# proven writable by that UID before the zero-call assertion.
NONROOT_LE="$WORK/nonroot-le"
mkdir -p "$NONROOT_LE/live" "$NONROOT_LE/hooks" "$NONROOT_LE/fanout" "$NONROOT_LE/pair"
cp "$WORK/issued/fullchain.pem" "$NONROOT_LE/pair/fullchain.pem"
cp "$WORK/issued/privkey.pem" "$NONROOT_LE/pair/privkey.pem"
chown -R 65534:65534 "$NONROOT_LE"
chmod 700 "$NONROOT_LE" "$NONROOT_LE/live" "$NONROOT_LE/hooks" "$NONROOT_LE/fanout" "$NONROOT_LE/pair"
chmod 644 "$NONROOT_LE/pair/fullchain.pem"
chmod 600 "$NONROOT_LE/pair/privkey.pem"
if nonroot_dir_write_control "$NONROOT_LE/live" &&
   nonroot_dir_write_control "$NONROOT_LE/hooks" &&
   nonroot_dir_write_control "$NONROOT_LE/fanout" &&
   setpriv --reuid=65534 --regid=65534 --clear-groups test -r "$NONROOT_LE/pair/privkey.pem" &&
   setpriv --reuid=65534 --regid=65534 --clear-groups test -x "$STUB/certbot" &&
   setpriv --reuid=65534 --regid=65534 --clear-groups test -x /usr/bin/install; then
  pass "nonroot LE has readable source key, writable destinations, and reachable Certbot/install"
else fail "nonroot LE fixture permissions or tool reachability invalid"; fi
NONROOT_CERTBOT_LOG="$NONROOT_LE/certbot.calls"
: >"$NONROOT_CERTBOT_LOG"; chown 65534:65534 "$NONROOT_CERTBOT_LOG"; chmod 600 "$NONROOT_CERTBOT_LOG"
if setpriv --reuid=65534 --regid=65534 --clear-groups env CERTBOT_LOG="$NONROOT_CERTBOT_LOG" \
    /bin/bash -c 'printf writable-control >>"$CERTBOT_LOG"' &&
    grep -q 'writable-control' "$NONROOT_CERTBOT_LOG"; then
  : >"$NONROOT_CERTBOT_LOG"
  pass "nonroot Certbot log write control succeeds before LE refusal assertion"
else fail "nonroot Certbot log is not a valid writable control"; fi
if setpriv --reuid=65534 --regid=65534 --clear-groups env \
    CERTBOT_LOG="$NONROOT_CERTBOT_LOG" VALID_PAIR_ROOT="$NONROOT_LE/pair" \
    LE_LIVE_ROOT="$NONROOT_LE/live" HOOK_DIR="$NONROOT_LE/hooks" \
    COTURN_CERT_ROOT="$NONROOT_LE/fanout/certs" COPY_CERTS_HOOK="$ROOT/infrastructure/deploy/copy-certs.sh" \
    PATH="$STUB:/usr/bin:/bin" /bin/bash "$TARGET" letsencrypt \
    --domain nonroot.example --email ops@example.com >"$WORK/nonroot-le.out" 2>&1; then
  fail "actual nonroot letsencrypt accepted"
else
  grep -Fq 'tls-provision: root required; certificate delivery not started' "$WORK/nonroot-le.out" &&
    [[ ! -e "$NONROOT_LE/live/nonroot.example" && ! -e "$NONROOT_LE/hooks/copy-certs.sh" &&
       ! -s "$NONROOT_CERTBOT_LOG" && ! -e "$NONROOT_LE/fanout/certs" ]] &&
    pass "actual nonroot letsencrypt refuses before hook install, issuance, or source mutation" || fail "nonroot LE refusal late, misclassified, or vacuous"
fi
# Missing --domain/--email → usage rc 2.
PATH="$STUB:$PATH" bash "$TARGET" letsencrypt --domain example.com >/dev/null 2>&1
[[ $? -eq 2 ]] && pass "LE missing --email rc=2" || fail "LE missing --email wrong rc"

# A successful Certbot status without a complete source pair is incomplete
# source creation, never successful delivery.
rm -rf "$COTURN_CERT_ROOT"; : >"$CERTBOT_LOG"
if CERTBOT_MODE=missing PATH="$STUB:$PATH" VALID_PAIR_ROOT="$WORK/issued" \
    LE_LIVE_ROOT="$WORK/le-no-pair" HOOK_DIR="$WORK/hooks-no-pair" \
    bash "$TARGET" letsencrypt --domain missingpair.example --email ops@example.com \
    >"$WORK/no-pair.out" 2>&1; then
  fail "Certbot success without source pair accepted"
else
  grep -Fq 'tls-provision: certificate source incomplete; delivery not complete' "$WORK/no-pair.out" &&
    [[ ! -s "$COTURN_CERT_ROOT/coturn/cert.pem" || ! -s "$COTURN_CERT_ROOT/coturn/key.pem" ]] &&
    pass "Certbot-success missing-pair branch fails before delivery completion" || fail "missing-pair branch had wrong failure class"
fi

# Default self-host issuance must not acquire a managed upload capability from
# an adjacent private checkout or an inherited global renewal hook. Every case
# executes the checked-in entrypoint and real copy hook from an owned clone.
# The uploader/configuration/curl fixtures only mark the attempted boundaries;
# they contain no credentials, read no key bytes and make no network request.
# Refusing a retained uploader before Certbot prevents a new public lineage from
# entering that managed renewal lifecycle without deleting the managed artifact.
certificate_upload_boundary() (
  local checkout=$2 authorization=$3 retained=$4 lineage=$5 expectation=$6
  local fixture="$WORK/upload-boundary-$1" domain=boundary.example rc=0
  local entrypoint hooks artifact before_metadata='' before_link=''
  local -a selector=()
  mkdir -p "$fixture/source/infrastructure/deploy" "$fixture/bin" "$fixture/hooks" "$fixture/live" || return 1
  entrypoint="$fixture/source/infrastructure/deploy/provision-cert.sh"
  hooks="$fixture/hooks"; artifact="$hooks/upload-cert-to-cloudflare.sh"
  cp "$TARGET" "$entrypoint" || return 1
  cp "$ROOT/infrastructure/deploy/copy-certs.sh" "$fixture/source/infrastructure/deploy/copy-certs.sh" || return 1
  cat >"$fixture/configuration" <<'BOUNDARY_CONFIG'
printf '%s\n' configuration-read >>"$BOUNDARY_EVENTS"
BOUNDARY_CONFIG
  cat >"$fixture/uploader" <<'BOUNDARY_UPLOAD'
#!/usr/bin/env bash
set -euo pipefail
[[ $# -eq 0 && "${RENEWED_LINEAGE:-}" == "$LE_LIVE_ROOT/$BOUNDARY_DOMAIN" ]] || exit 97
printf '%s\n' upload-hook >>"$BOUNDARY_EVENTS"
source "$BOUNDARY_CONFIGURATION"
curl --fixture-managed-upload "$RENEWED_LINEAGE"
BOUNDARY_UPLOAD
  cat >"$fixture/bin/curl" <<'BOUNDARY_CURL'
#!/usr/bin/env bash
set -euo pipefail
[[ $# -eq 2 && $1 == --fixture-managed-upload && $2 == "$LE_LIVE_ROOT/$BOUNDARY_DOMAIN" ]] || exit 97
printf '%s\n' upload >>"$BOUNDARY_EVENTS"
BOUNDARY_CURL
  chmod 0755 "$fixture/uploader" "$fixture/bin/curl" || return 1
  case "$checkout" in
    public) [[ ! -e "$fixture/source/infrastructure/deploy/upload-cert-to-cloudflare.sh" ]] || return 1 ;;
    managed) cp "$fixture/uploader" "$fixture/source/infrastructure/deploy/upload-cert-to-cloudflare.sh" || return 1 ;;
    *) return 2 ;;
  esac
  case "$retained" in
    absent) ;;
    regular) cp "$fixture/uploader" "$artifact" && chmod 0750 "$artifact" || return 1 ;;
    symlink) ln -s "$fixture/uploader" "$artifact" || return 1 ;;
    dangling) ln -s "$fixture/missing-managed-uploader" "$artifact" || return 1 ;;
    *) return 2 ;;
  esac
  if [[ "$retained" != absent ]]; then
    before_metadata="$(stat -c '%F:%i:%u:%g:%a:%s:%Y' "$artifact")" || return 1
    if [[ -L "$artifact" ]]; then
      before_link="$(readlink "$artifact")" || return 1
    else
      cp "$artifact" "$fixture/retained-before" || return 1
    fi
  fi
  case "$lineage" in
    fresh) ;;
    existing)
      mkdir -p "$fixture/live/$domain" || return 1
      cp "$WORK/issued/fullchain.pem" "$fixture/live/$domain/fullchain.pem" || return 1
      cp "$WORK/issued/privkey.pem" "$fixture/live/$domain/privkey.pem" || return 1
      chmod 0600 "$fixture/live/$domain/privkey.pem" || return 1 ;;
    *) return 2 ;;
  esac
  case "$authorization" in
    selfhost) ;;
    managed) selector=(--managed-cloudflare-upload) ;;
    unknown) selector=(--managed-cloudflare-upload=unknown) ;;
    *) return 2 ;;
  esac
  export BOUNDARY_EVENTS="$fixture/events" BOUNDARY_CONFIGURATION="$fixture/configuration" BOUNDARY_DOMAIN="$domain"
  export LE_LIVE_ROOT="$fixture/live" HOOK_DIR="$hooks" COTURN_CERT_ROOT="$fixture/fanout"
  export COPY_CERTS_HOOK="$fixture/source/infrastructure/deploy/copy-certs.sh" VALID_PAIR_ROOT="$WORK/issued"
  export CERTBOT_LOG="$fixture/certbot.calls" CERTBOT_HOOK_LOG="$fixture/renewal.calls"
  export DOCKER_RESTART_LOG="$fixture/restarts" SYSTEMCTL_LOG="$fixture/systemctl.calls"
  export DOCKER_FIXTURE_MODE=absent SYSTEMCTL_NGINX_ACTIVE=0 SYSTEMCTL_RELOAD_FAIL=0
  export PATH="$fixture/bin:$STUB:$PATH"
  : >"$BOUNDARY_EVENTS"; : >"$CERTBOT_LOG"; : >"$CERTBOT_HOOK_LOG"
  if [[ "$lineage" == existing ]]; then export CERTBOT_MODE=existing; else export CERTBOT_MODE=pair; fi
  bash "$entrypoint" letsencrypt --domain "$domain" --email ops@example.invalid "${selector[@]}" \
    >"$fixture/output" 2>&1 || rc=$?
  ! grep -qE 'command not found|unbound variable|syntax error|Traceback' "$fixture/output" || return 1
  if [[ "$expectation" == refuse || "$expectation" == usage ]]; then
    if [[ "$expectation" == refuse ]]; then
      [[ $rc -eq 1 ]] && grep -Fq 'tls-provision: managed renewal hook present; self-host issuance not started' "$fixture/output" || return 1
    else
      [[ $rc -eq 2 ]] || return 1
    fi
    [[ ! -s "$BOUNDARY_EVENTS" && ! -s "$CERTBOT_LOG" && ! -s "$CERTBOT_HOOK_LOG"
       && ! -e "$hooks/copy-certs.sh" && ! -e "$fixture/fanout" ]] || return 1
    ! grep -Fq 'Certificate source and coturn delivery complete.' "$fixture/output" || return 1
    if [[ "$lineage" == fresh ]]; then
      [[ ! -e "$fixture/live/$domain" ]] || return 1
    else
      cmp -s "$WORK/issued/fullchain.pem" "$fixture/live/$domain/fullchain.pem" &&
        cmp -s "$WORK/issued/privkey.pem" "$fixture/live/$domain/privkey.pem" &&
        [[ "$(stat -c '%u:%g:%a' "$fixture/live/$domain/privkey.pem")" == '0:0:600' ]] || return 1
    fi
    if [[ "$retained" != absent ]]; then
      [[ "$(stat -c '%F:%i:%u:%g:%a:%s:%Y' "$artifact")" == "$before_metadata" ]] || return 1
      if [[ -n "$before_link" ]]; then
        [[ -L "$artifact" && "$(readlink "$artifact")" == "$before_link" ]] || return 1
      else
        cmp -s "$fixture/retained-before" "$artifact" || return 1
      fi
    else
      [[ ! -e "$artifact" && ! -L "$artifact" ]] || return 1
    fi
    return 0
  fi
  [[ $rc -eq 0 ]] && grep -Fq 'Certificate source and coturn delivery complete.' "$fixture/output" || return 1
  cmp -s "$fixture/live/$domain/fullchain.pem" "$fixture/fanout/coturn/cert.pem" &&
    cmp -s "$fixture/live/$domain/privkey.pem" "$fixture/fanout/coturn/key.pem" || return 1
  installed_renewal_matches_candidate "$hooks/copy-certs.sh" || return 1
  grep -q -- '--no-directory-hooks' "$CERTBOT_LOG" &&
    grep -q -- '--reuse-key' "$CERTBOT_LOG" && grep -q -- '--webroot' "$CERTBOT_LOG" || return 1
  if [[ "$authorization" == managed ]]; then
    [[ "$(cat "$BOUNDARY_EVENTS")" == $'upload-hook\nconfiguration-read\nupload' ]] || return 1
    cmp -s "$fixture/source/infrastructure/deploy/upload-cert-to-cloudflare.sh" "$artifact" && [[ -x "$artifact" ]] || return 1
  else
    [[ ! -s "$BOUNDARY_EVENTS" && ! -e "$artifact" && ! -L "$artifact" ]] || return 1
  fi
  # Certbot's later timer invocation does not inherit the provisioner's CLI
  # selector. It sees only the actual installed directory hooks. Public success
  # must have copy-certs only; managed success intentionally retains its uploader.
  : >"$BOUNDARY_EVENTS"; : >"$CERTBOT_HOOK_LOG"
  CERTBOT_MODE=pair certbot renew --cert-name "$domain" >"$fixture/renewal-output" 2>&1 || return 1
  [[ "$(grep -cFx 'deploy-hook' "$CERTBOT_HOOK_LOG" 2>/dev/null || true)" -eq 1 ]] || return 1
  ! grep -qE '^deploy-hook-status=[1-9][0-9]*$' "$CERTBOT_HOOK_LOG" || return 1
  cmp -s "$fixture/live/$domain/fullchain.pem" "$fixture/fanout/coturn/cert.pem" &&
    cmp -s "$fixture/live/$domain/privkey.pem" "$fixture/fanout/coturn/key.pem" || return 1
  if [[ "$authorization" == managed ]]; then
    [[ "$(cat "$BOUNDARY_EVENTS")" == $'upload-hook\nconfiguration-read\nupload' ]] &&
      [[ "$(grep -cFx 'deploy-hook-status=0' "$CERTBOT_HOOK_LOG" 2>/dev/null || true)" -eq 2 ]] &&
      grep -qFx 'directory-hook=upload-cert-to-cloudflare.sh' "$CERTBOT_HOOK_LOG" || return 1
  else
    [[ ! -s "$BOUNDARY_EVENTS" && ! -e "$artifact" && ! -L "$artifact" ]] &&
      [[ "$(grep -cFx 'deploy-hook-status=0' "$CERTBOT_HOOK_LOG" 2>/dev/null || true)" -eq 1 ]] &&
      ! grep -qFx 'directory-hook=upload-cert-to-cloudflare.sh' "$CERTBOT_HOOK_LOG" || return 1
  fi
)
for boundary_case in \
  'public-clean public selfhost absent fresh accept' \
  'public-adjacent managed selfhost absent fresh accept' \
  'public-retained public selfhost regular fresh refuse' \
  'public-reused-managed managed selfhost regular existing refuse' \
  'public-retained-symlink public selfhost symlink fresh refuse' \
  'public-retained-dangling public selfhost dangling fresh refuse' \
  'managed-authorized managed managed absent fresh accept' \
  'managed-reused managed managed regular existing accept' \
  'unknown-selector managed unknown absent fresh usage'; do
  read -r boundary_label boundary_checkout boundary_authorization boundary_retained boundary_lineage boundary_expectation <<<"$boundary_case"
  if certificate_upload_boundary "$boundary_label" "$boundary_checkout" "$boundary_authorization" \
      "$boundary_retained" "$boundary_lineage" "$boundary_expectation"; then
    pass "certificate upload boundary $boundary_label controls issuance, key access and retained renewal hooks"
  else
    fail "certificate upload boundary $boundary_label violated authorization or fixture contract"
  fi
done

# Required renewal artifact verification is separate from the readable sibling
# used for immediate delivery. Fail, omit, stale executable, non-executable,
# and wrong-owner cases must all stop before Certbot issuance.
cat >"$STUB/install" <<'EOF'
#!/usr/bin/env bash
args=("$@")
arg_count=${#args[@]}
source_operand="${args[arg_count-2]:-}"
destination_operand="${args[arg_count-1]:-}"
renewal_artifact="$(capture-renewal-candidate "$source_operand" "$destination_operand")" || exit 96
provider_status=0
case "${INSTALL_MODE:-normal}" in
  fail) provider_status=1 ;;
  omit) ;;
  stale)
    printf '#!/bin/sh\nexit 0\n' >"$destination_operand"
    chmod 0755 "$destination_operand"
    ;;
  symlink) ln -s "$source_operand" "$destination_operand" || provider_status=$? ;;
  nonexec) "$REAL_CERT_INSTALL" -m 0644 "$source_operand" "$destination_operand" || provider_status=$? ;;
  wrongowner)
    "$REAL_CERT_INSTALL" "$@" && chown 65534:65534 "$destination_operand" || provider_status=$? ;;
  *) exec "$REAL_CERT_INSTALL" "$@" ;;
esac
# Preparation is cleaned up on refusal. Observe the injected artifact before
# that cleanup, while the real provisioner still owns all admission decisions.
if [[ -n "$renewal_artifact" ]]; then
  if [[ -L "$destination_operand" ]]; then
    printf '%s\n' symlink >"$renewal_artifact.fault-kind"
    readlink "$destination_operand" >"$renewal_artifact.fault-link"
  elif [[ -f "$destination_operand" ]]; then
    printf '%s\n' regular >"$renewal_artifact.fault-kind"
    cp -- "$destination_operand" "$renewal_artifact.fault-artifact"
  else printf '%s\n' absent >"$renewal_artifact.fault-kind"; fi
  if [[ -e "$destination_operand" ]]; then
    stat -Lc '%u:%g:%a' "$destination_operand" >"$renewal_artifact.fault-metadata"
  fi
fi
exit "$provider_status"
EOF
chmod +x "$STUB/install"
for install_mode in fail omit stale nonexec wrongowner symlink; do
  : >"$CERTBOT_LOG"
  if INSTALL_MODE="$install_mode" PATH="$STUB:$PATH" VALID_PAIR_ROOT="$WORK/issued" \
      LE_LIVE_ROOT="$WORK/install-$install_mode-live" HOOK_DIR="$WORK/install-$install_mode-hooks" \
      bash "$TARGET" letsencrypt --domain installfail.example --email ops@example.com \
      >"$WORK/install-$install_mode.out" 2>&1; then
    fail "LE accepted required installed-hook $install_mode failure"
  else
    artifact="$WORK/install-$install_mode-hooks/copy-certs.sh"
    artifact_shape=1
    [[ ! -e "$artifact" && ! -L "$artifact" ]] || artifact_shape=0
    fault_kind="$(cat "$artifact.fault-kind")"
    case "$install_mode" in
      fail|omit) [[ "$fault_kind" == absent ]] || artifact_shape=0 ;;
      stale) [[ "$fault_kind" == regular && "$(cat "$artifact.fault-metadata")" == "0:0:755" ]] &&
        ! cmp -s "$artifact.candidate" "$artifact.fault-artifact" || artifact_shape=0 ;;
      symlink) [[ "$fault_kind" == symlink &&
        "$(cat "$artifact.fault-link")" == "$(cat "$artifact.candidate-operand")" ]] || artifact_shape=0 ;;
      nonexec) [[ "$fault_kind" == regular ]] && cmp -s "$artifact.candidate" "$artifact.fault-artifact" &&
        [[ "$(cat "$artifact.fault-metadata")" == "0:0:644" ]] || artifact_shape=0 ;;
      wrongowner) [[ "$fault_kind" == regular ]] && cmp -s "$artifact.candidate" "$artifact.fault-artifact" &&
        [[ "$(cat "$artifact.fault-metadata")" == "65534:65534:755" ]] || artifact_shape=0 ;;
    esac
    grep -Fq 'tls-provision: renewal hook installation failed; delivery not complete' "$WORK/install-$install_mode.out" &&
      [[ ! -s "$CERTBOT_LOG" && "$artifact_shape" == 1 ]] && pass "LE $install_mode installed-hook artifact fails before issuance" || fail "LE installed-hook $install_mode failure late, malformed, or misclassified"
  fi
done

echo "── ${fails} failures ──"; exit $(( fails > 0 ))
