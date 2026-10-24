"""Owned hostname admission checks; no root gate, real issuance, or live host."""
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

SOURCE_ROOT = Path(sys.argv.pop(1)).resolve()
# Independent DNS contract and the public service inventory, not producer constants.
PREFIXES = ("", "api.", "media.", "turn.", "spa.")
MAX_APEX = ".".join(("a" * 63, "b" * 63, "c" * 63, "d" * 55))
LABEL_CONTROLS = ("a" * 63 + ".example", "safe." + "b" * 63 + ".example", "safe." + "c" * 63)
LABEL_OVERFLOWS = ("a" * 64 + ".example", "safe." + "b" * 64 + ".example", "safe." + "c" * 64)

# Source the byte-identical owned script and enter its real mode consumers.
# Downstream issuance/import observers stop at named boundaries. Let’s Encrypt
# runs its actual owned renewal-hook writer with virtual owner metadata, then
# presents the actual requested hostnames to a closed Certbot observer.
DRIVER = r'''
source "$1" || exit 97
domain=$3
certbot() {
  printf '%s\0' "$@" >"$DOMAIN_FIXTURE/issuer-argv"
  printf 'certificate-issuance-request\n' >>"$DOMAIN_FIXTURE/events"
  return 74
}
install() {
  [[ $# -eq 8 && $1 == -o && $2 == root && $3 == -g && $4 == root
     && $5 == -m && $6 == 0755 && $7 == "$HOOK_DIR/".copy-certs.*/candidate
     && $8 == "${7%/candidate}/installed" && -d "${7%/candidate}"
     && ! -L "${7%/candidate}" && $(command stat -c '%a' "${7%/candidate}") == 700 ]] || return 97
  DOMAIN_PREPARED_HOOK=$8
  command cp -- "$7" "$8" || return 97
  command chmod 0755 "$8" || return 97
  printf 'renewal-hook-prepare\n' >>"$DOMAIN_FIXTURE/events"
}
stat() {
  if [[ $# -eq 3 && $1 == -Lc && $2 == '%u:%g:%a'
     && ( $3 == "${DOMAIN_PREPARED_HOOK:-}" || $3 == "$HOOK_DIR/copy-certs.sh" ) ]]; then
    [[ -f "$3" && ! -L "$3" && $(command stat -c '%a' "$3") == 755 ]] || return 97
    printf '0:0:755\n'
  else
    command stat "$@"
  fi
}
mv() {
  [[ $# -eq 4 && $1 == -Tf && $2 == -- && $3 == "${DOMAIN_PREPARED_HOOK:-}"
     && $4 == "$HOOK_DIR/copy-certs.sh" && -f "$3" && ! -L "$3" ]] || return 97
  command mv -Tf -- "$3" "$4" || return 97
  printf 'renewal-hook-install\n' >>"$DOMAIN_FIXTURE/events"
}
replace_source_generation() {
  [[ $# -eq 4 && $1 == local && $2 == "$domain" && $3 == 30 ]] || return 97
  printf '%s\0' "$@" >"$DOMAIN_FIXTURE/generation-argv"
  printf 'local-generation-request\n' >>"$DOMAIN_FIXTURE/events"
  return 74
}
validate_import_pair() {
  [[ $# -eq 3 && $1 == "$DOMAIN_FIXTURE/input/cert" && $2 == "$DOMAIN_FIXTURE/input/key"
     && $3 == "$domain" ]] || return 97
  printf '%s\0' "$@" >"$DOMAIN_FIXTURE/import-argv"
  printf 'import-certificate-validation-request\n' >>"$DOMAIN_FIXTURE/events"
  return 74
}
forbidden_effect() { printf 'forbidden-external-effect\n' >>"$DOMAIN_FIXTURE/events"; return 97; }
openssl() { forbidden_effect; }
docker() { forbidden_effect; }
systemctl() { forbidden_effect; }
curl() { forbidden_effect; }
getent() { forbidden_effect; }
case "$2" in
  local) mode_local --host "$domain" --days 30 ;;
  letsencrypt) mode_letsencrypt --domain "$domain" --email fixture@example.invalid ;;
  import) mode_import --host "$domain" --cert "$DOMAIN_FIXTURE/input/cert" --key "$DOMAIN_FIXTURE/input/key" ;;
  *) exit 97 ;;
esac
'''


class SelfhostDomainAdmission(unittest.TestCase):
    def invoke(self, mode, domain):
        with tempfile.TemporaryDirectory(prefix="selfhost-domain-") as name:
            root = Path(name)
            for child in ("source", "live", "hooks", "input", "home"):
                (root / child).mkdir()
            sources = {}
            for filename in ("provision-cert.sh", "copy-certs.sh"):
                original = SOURCE_ROOT / "infrastructure/deploy" / filename
                target = root / "source" / filename
                shutil.copy2(original, target)
                self.assertEqual(target.read_bytes(), original.read_bytes(), "owned source copy must retain every production byte")
                sources[target] = hashlib.sha256(target.read_bytes()).hexdigest()
            # These opaque owned paths only satisfy mode_import's file-presence
            # prerequisite; the test ends at its SAN/certificate reader boundary.
            (root / "input/cert").write_text("owned opaque certificate input\n")
            (root / "input/key").write_text("owned opaque key input\n")
            (root / "events").write_text("")
            env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(root / "home"),
                   "LE_LIVE_ROOT": str(root / "live"), "HOOK_DIR": str(root / "hooks"),
                   "DOMAIN_FIXTURE": str(root)}
            result = subprocess.run(["bash", "-c", DRIVER, "owned-domain-mode",
                                     str(root / "source/provision-cert.sh"), mode, domain],
                                    env=env, text=True, capture_output=True, timeout=10)
            output = result.stdout + result.stderr
            for fault in ("syntax error", "unbound variable", "command not found", "Traceback"):
                self.assertNotIn(fault, output, "fixture must reach actual hostname admission, not setup failure")
            events = (root / "events").read_text().splitlines()
            self.assertNotIn("forbidden-external-effect", events, "hostname fixture must keep all external boundaries closed")
            self.assertNotEqual(result.returncode, 97, "finite observer must accept the actual production command shape")
            self.assertEqual(sources, {path: hashlib.sha256(path.read_bytes()).hexdigest() for path in sources},
                             "mode invocation must not change its owned production source")
            artifacts = {}
            for artifact in ("issuer-argv", "generation-argv", "import-argv"):
                path = root / artifact
                if path.exists():
                    artifacts[artifact] = path.read_bytes().decode().rstrip("\0").split("\0")
            return result.returncode, output, events, artifacts, tuple((root / "hooks").iterdir())

    def admitted(self, domain):
        self.assertTrue(all(len(label) <= 63 for label in domain.split(".")), "positive DNS fixture labels must be valid")
        self.assertTrue(all(len(prefix + domain) <= 253 for prefix in PREFIXES), "all positive generated DNS names must fit")
        for mode in ("local", "letsencrypt", "import"):
            with self.subTest(mode=mode, apex_bytes=len(domain)):
                status, _, events, artifacts, hooks = self.invoke(mode, domain)
                if mode == "local":
                    self.assertEqual(status, 74, "supported hostname must reach the local generation observer")
                    self.assertEqual(events, ["local-generation-request"])
                    self.assertEqual(artifacts["generation-argv"],
                                     ["local", domain, "30", ",".join("DNS:" + prefix + domain for prefix in PREFIXES)],
                                     "local mode must preserve the supported host bytes in every actual SAN")
                    self.assertFalse(hooks)
                elif mode == "letsencrypt":
                    self.assertEqual(status, 1, "named closed issuer returns its intentional failure after admission")
                    self.assertEqual(events, ["renewal-hook-prepare", "renewal-hook-install", "certificate-issuance-request"])
                    args = artifacts["issuer-argv"]
                    names = [args[index + 1] for index, value in enumerate(args) if value == "-d"]
                    self.assertEqual(names, [prefix + domain for prefix in PREFIXES[:-1]],
                                     "actual Certbot request must preserve the supported apex and service names")
                    # The closed issuer deliberately fails. With no prior hook,
                    # the renewal transaction must restore the empty directory.
                    self.assertFalse(hooks, "failed issuance must restore the prior renewal authority")
                else:
                    self.assertEqual(status, 1, "supported hostname must reach the closed certificate input reader")
                    self.assertEqual(events, ["import-certificate-validation-request"])
                    self.assertEqual(artifacts["import-argv"][-1], domain, "import admission must preserve the supported deployment host")
                    self.assertFalse(hooks)

    def refused(self, domain):
        for mode in ("local", "letsencrypt", "import"):
            with self.subTest(mode=mode, apex_bytes=len(domain)):
                status, output, events, artifacts, hooks = self.invoke(mode, domain)
                self.assertEqual(events, [], "unsupported DNS label/generated service must refuse before saved renewal configuration or certificate input/issuance effects")
                self.assertEqual(status, 2, "standalone mode must reject an unsupported deployment hostname during admission")
                self.assertIn("invalid host", output)
                self.assertEqual(artifacts, {}, "unsupported hostname must reach no certificate consumer")
                self.assertFalse(hooks, "unsupported hostname must create no renewal authority")

    def test_labels_of_63_bytes_remain_admitted_by_all_modes(self):
        for domain in LABEL_CONTROLS:
            self.admitted(domain)

    def test_labels_of_64_bytes_refuse_before_all_mode_effects(self):
        for domain in LABEL_OVERFLOWS:
            self.refused(domain)

    def test_generated_media_name_of_253_bytes_remains_admitted(self):
        self.assertEqual(len(MAX_APEX), 247)
        self.assertEqual(len("media." + MAX_APEX), 253)
        self.admitted(MAX_APEX)

    def test_generated_media_name_of_254_bytes_refuses_before_effects(self):
        self.assertEqual(len("media." + MAX_APEX + "d"), 254)
        self.refused(MAX_APEX + "d")

    def test_apex_of_253_bytes_refuses_generated_service_overflow(self):
        domain = ".".join(("a" * 63, "b" * 63, "c" * 63, "d" * 61))
        self.assertEqual(len(domain), 253)
        self.refused(domain)

    def test_ordinary_and_63_byte_hyphenated_labels_preserve_literal_bytes(self):
        for domain in ("example.com", "a" + "-" * 61 + "z.example"):
            self.admitted(domain)


if __name__ == "__main__":
    unittest.main(verbosity=2)
