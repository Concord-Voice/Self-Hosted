"""Hardening of the Alpha Required PR Build ruleset and its appliers.

Covers the reviewed JSON bodies (strict currency, explicit workflow ref, reserved
tag name), the appliers' create, --update and --rollback paths against a hermetic
gh shim (no network, nothing real is ever written), and pr-ci.yml's protected-owner
step, the one workflow that reads the ruleset's ref spelling.

Run directly: python3 .github/scripts/tests/test_ruleset_hardening.py
"""

from pathlib import Path
import copy
import json
import os
import re
import shutil
import stat
import subprocess
import tempfile
import unittest
import unittest.mock

import yaml

ROOT = Path(__file__).resolve().parents[3]
GITHUB = ROOT / "infrastructure/github"
WORKFLOW_JSON = GITHUB / "alpha-required-workflow-ruleset.json"
TAG_JSON = GITHUB / "alpha-reserved-tag-ruleset.json"
WORKFLOW_APPLY = GITHUB / "apply-alpha-required-workflow-ruleset.sh"
TAG_APPLY = GITHUB / "apply-alpha-reserved-tag-ruleset.sh"
PATH_IN_REPO = ".github/workflows/required-pr-build.yml"

# The body live (and committed) before this change. --update must recognise
# exactly this as "previous" and --rollback must restore exactly this.
V1 = {
    "name": "Alpha Required PR Build",
    "target": "branch",
    "enforcement": "active",
    "conditions": {
        "repository_id": {"repository_ids": [1158152129]},
        "ref_name": {"include": ["~DEFAULT_BRANCH"], "exclude": []},
    },
    "bypass_actors": [{"actor_id": 3967879, "actor_type": "Integration", "bypass_mode": "always"}],
    "rules": [{
        "type": "workflows",
        "parameters": {
            "do_not_enforce_on_create": False,
            "workflows": [{"repository_id": 1158152129, "path": PATH_IN_REPO, "ref": "main"}],
        },
    }],
}
CURRENCY_RULE = {
    "type": "required_status_checks",
    "parameters": {
        "strict_required_status_checks_policy": True,
        "do_not_enforce_on_create": False,
        "required_status_checks": [{"context": "Required PR build result", "integration_id": 15368}],
    },
}
V2 = copy.deepcopy(V1)
V2["rules"][0]["parameters"]["workflows"][0]["ref"] = "refs/heads/main"
V2["rules"].append(copy.deepcopy(CURRENCY_RULE))

TAG = {
    "name": "Alpha Reserved Tag Names",
    "target": "tag",
    "enforcement": "active",
    "conditions": {
        "repository_id": {"repository_ids": [1158152129]},
        "ref_name": {"include": ["refs/tags/main"], "exclude": []},
    },
    "bypass_actors": [],
    "rules": [{"type": "creation"}],
}

SERVER_FIELDS = {
    "id": 123,
    "source_type": "Organization",
    "source": "Concord-Voice",
    "created_at": "2026-10-14T00:00:00.000Z",
    "updated_at": "2026-10-14T00:00:00.000Z",
    "current_user_can_bypass": "never",
}

# Stands in for `gh`. Behaviour is declarative: cfg["live"] is the ruleset the API
# returns, cfg["behave"][endpoint][n] says how the n-th call to that endpoint fails.
FAKE_GH = r'''#!/usr/bin/env python3
import base64, hashlib, json, os, subprocess, sys
from pathlib import Path

args = sys.argv[1:]
cfg = json.loads(Path(os.environ["FAKE_CFG"]).read_text())
state_path = Path(os.environ["FAKE_STATE"])
log_path = Path(os.environ["FAKE_LOG"])
counts_path = Path(os.environ["FAKE_COUNTS"])


def log(entry):
    with log_path.open("a") as handle:
        handle.write(json.dumps(entry) + "\n")


def bump(key):
    counts = json.loads(counts_path.read_text()) if counts_path.exists() else {}
    counts[key] = counts.get(key, 0) + 1
    counts_path.write_text(json.dumps(counts))
    return counts[key]


def behaviour(key, n):
    return cfg.get("behave", {}).get(key, {}).get(str(n), "ok")


def nth(key):
    return behaviour(key, bump(key))


def load_state():
    if state_path.exists():
        return json.loads(state_path.read_text())
    return cfg.get("live")


def drifted(body):
    body = json.loads(json.dumps(body))
    body["name"] = "unexpected drift"
    return body


server = {"id": 123, "source_type": "Organization", "source": "Concord-Voice",
          "updated_at": "2026-10-14T00:00:00.000Z"}

if args == ["api", "--paginate", "orgs/Concord-Voice/rulesets"]:
    log({"call": "list"})
    how = nth("list")
    if how == "fail":
        sys.exit(1)
    if how == "garbage":
        print("this is not json")
        sys.exit(0)
    # --paginate prints one JSON array per page, back to back.
    for page in cfg.get("rulesets_pages", [cfg["rulesets"]]):
        print(json.dumps(page))
elif args == ["api", "orgs/Concord-Voice/rulesets/123"]:
    log({"call": "get"})
    how = nth("get")
    body = load_state()
    if how == "fail" or body is None:
        sys.exit(1)
    if how == "empty":
        # A success with nothing in it: no rule was read, so none can be proved ours.
        sys.exit(0)
    if how == "garbage":
        print("this is not json")
    elif how == "concurrent":
        # Another administrator's edit lands and stays: every bypass actor is revoked.
        body = json.loads(json.dumps(body))
        body["bypass_actors"] = []
        state_path.write_text(json.dumps(body))
        print(json.dumps(body))
    elif how == "no-bypass":
        body = json.loads(json.dumps(body))
        body.pop("bypass_actors", None)
        print(json.dumps(body))
    elif how == "edited":
        # Another administrator's edit lands and stays: a ref is excluded from the rule.
        body = json.loads(json.dumps(body))
        body["conditions"]["ref_name"]["exclude"] = ["refs/heads/other"]
        state_path.write_text(json.dumps(body))
        print(json.dumps(body))
    elif how == "multidoc":
        # An answer holding two documents: the rule as it stands (an edit that landed
        # and stays), then a copy of the body this run created.
        sent = next(entry["payload"] for entry in map(json.loads, log_path.read_text().splitlines())
                    if entry["call"] == "post")
        print(json.dumps(body))
        print(json.dumps({**sent, **server}))
    else:
        print(json.dumps(drifted(body) if how == "drift" else body))
elif args[:4] == ["api", "-X", "PUT", "orgs/Concord-Voice/rulesets/123"] and args[4] == "--input":
    payload = json.loads(Path(args[5]).read_text())
    log({"call": "put", "payload": payload})
    how = nth("put")
    if how == "fail":
        sys.exit(1)
    if "tamper_dir" in cfg and how != "fail":
        # An attacker who can write the snapshot directory swaps the saved body mid-run.
        for saved in Path(cfg["tamper_dir"]).glob("alpha-required-pr-build-123-previous-*.json"):
            saved.write_text(json.dumps({"name": "attacker body"}))
    stored = json.loads(json.dumps(payload))
    if how in ("mangled", "mangled-garbage"):
        stored["rules"] = [r for r in stored["rules"] if r["type"] != "required_status_checks"]
    if how == "mangled-bypass":
        stored["bypass_actors"] = []
    if how == "renamed":
        # The server stored something other than what was sent, and says so.
        stored["name"] += " (normalized)"
    if how != "ignored":
        state_path.write_text(json.dumps({**stored, **server}))
    if how == "fail-applied":
        sys.exit(1)
    if how in ("garbage", "mangled-garbage"):
        print("this is not json")
    elif how == "multidoc":
        # Two documents: the rule as written, then a copy that carries the edit
        # another administrator is about to make.
        print(json.dumps({**stored, **server}))
        edit = json.loads(json.dumps({**stored, **server}))
        edit["conditions"]["ref_name"]["exclude"] = ["refs/heads/other"]
        print(json.dumps(edit))
    else:
        print(json.dumps({**stored, **server}))
elif args[:4] == ["api", "-X", "POST", "orgs/Concord-Voice/rulesets"] and args[4] == "--input":
    payload = json.loads(Path(args[5]).read_text())
    # Where the body was staged: its directory and that directory's mode.
    staging = Path(args[5]).parent
    log({"call": "post", "payload": payload, "dir": str(staging),
         "dir_mode": os.stat(staging).st_mode & 0o7777})
    how = nth("post")
    stored = json.loads(json.dumps(payload))
    if how == "renamed":
        stored["name"] += " (normalized)"
    state_path.write_text(json.dumps({**stored, **server}))
    if how == "fail":
        sys.exit(1)
    if how == "noid":
        print("{}")
    elif how == "bodyless":
        print('{"id":123}')
    else:
        # Like the real API: the create answers with the ruleset it stored.
        print(json.dumps({**stored, **server}))
elif args == ["api", "-X", "DELETE", "orgs/Concord-Voice/rulesets/123"]:
    log({"call": "delete"})
    if nth("delete") == "fail":
        sys.exit(1)
    state_path.write_text("null")
elif args[:2] == ["api", "repos/Concord-Voice/Concord-Voice-Alpha/git/matching-refs/tags/main"]:
    log({"call": "tags"})
    n = bump("tags")
    how = behaviour("tags", n)
    if how == "fail":
        sys.exit(1)
    if how == "empty":
        sys.exit(0)
    if "tags_pages" in cfg:
        # --paginate with --jq prints one result per page.
        pages = cfg["tags_pages"]
    elif "tags_seq" in cfg:
        # The tags that exist at the n-th call (the last entry repeats).
        pages = [cfg["tags_seq"][min(n, len(cfg["tags_seq"])) - 1]]
    else:
        pages = [cfg.get("tags", [])]
    for page in pages:
        refs = [{"ref": ref} for ref in page]
        if "--jq" in args:
            out = subprocess.run(["jq", "-c", args[args.index("--jq") + 1]], input=json.dumps(refs),
                                 text=True, capture_output=True, check=True).stdout
            sys.stdout.write(out)
        else:
            print(json.dumps(refs))
elif any("repos/Concord-Voice/Concord-Voice-Alpha/commits/main" in arg for arg in args):
    log({"call": "commit"})
    how = nth("commit")
    if how == "fail":
        sys.exit(1)
    sha = {"move": "c" * 40, "badsha": "not-a-sha"}.get(how, "a" * 40)
    if "--jq" in args and args[args.index("--jq") + 1] == ".sha":
        print(sha)
    else:
        print(json.dumps({"sha": sha}))
elif any("repos/Concord-Voice/Concord-Voice-Alpha/contents/" in arg for arg in args):
    path = next(arg.split("/contents/", 1)[1].split("?", 1)[0] for arg in args if "/contents/" in arg)
    log({"call": "contents", "path": path})
    if path in cfg.get("source_missing", []):
        sys.exit(1)
    content = Path(os.environ["REPO_ROOT"], path).read_bytes()
    if path in cfg.get("source_stale", []):
        content += b"\n# newer than the reviewed local copy\n"
    blob = hashlib.sha1(b"blob " + str(len(content)).encode() + b"\0" + content).hexdigest()
    reported = "somewhere/else/" + path if path in cfg.get("source_wrong_path", []) else path
    print(json.dumps({"path": reported, "sha": blob,
                      "content": base64.b64encode(content).decode(), "encoding": "base64"}))
else:
    raise SystemExit(f"unexpected gh call: {args}")
'''


def live(body):
    """The shape the organization API returns: the body plus server-owned fields."""
    return {**copy.deepcopy(body), **SERVER_FIELDS}


def canon(body):
    """Python twin of the appliers' snapshot(): drop server fields, default
    do_not_enforce_on_create, order rules by type."""
    rules = copy.deepcopy(body["rules"])
    for rule in rules:
        if rule["type"] in ("workflows", "required_status_checks"):
            rule["parameters"].setdefault("do_not_enforce_on_create", False)
    rules.sort(key=lambda rule: rule["type"])
    keys = ("name", "target", "enforcement", "conditions", "bypass_actors")
    return {**{key: body[key] for key in keys}, "rules": rules}


class Harness(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="ruleset-hardening-")
        self.addCleanup(temporary.cleanup)
        self.tmp = Path(temporary.name)
        self.bin = self.tmp / "bin"
        self.bin.mkdir()
        shim = self.bin / "gh"
        shim.write_text(FAKE_GH)
        shim.chmod(0o755)
        self.paths = {name: self.tmp / name for name in ("cfg", "state", "log", "counts")}

    def run_script(self, script, args, cfg, cwd=ROOT):
        for name in ("state", "log", "counts"):
            self.paths[name].unlink(missing_ok=True)
        self.paths["cfg"].write_text(json.dumps(cfg))
        env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
        env.update({
            "PATH": f"{self.bin}:{os.environ['PATH']}",
            "FAKE_CFG": str(self.paths["cfg"]), "FAKE_STATE": str(self.paths["state"]),
            "FAKE_LOG": str(self.paths["log"]), "FAKE_COUNTS": str(self.paths["counts"]),
            "REPO_ROOT": str(ROOT),
        })
        proc = subprocess.run(["bash", str(script), *args], cwd=cwd, env=env,
                              capture_output=True, text=True)
        calls = ([json.loads(line) for line in self.paths["log"].read_text().splitlines()]
                 if self.paths["log"].exists() else [])
        state = (json.loads(self.paths["state"].read_text())
                 if self.paths["state"].exists() else None)
        return proc, calls, state

    @staticmethod
    def kinds(calls):
        return [call["call"] for call in calls]

    @staticmethod
    def puts(calls):
        return [call["payload"] for call in calls if call["call"] == "put"]


class ReviewedBodies(unittest.TestCase):
    def test_workflow_ruleset_is_the_reviewed_v2_body(self):
        self.assertEqual(json.loads(WORKFLOW_JSON.read_text()), V2)

    def test_required_context_is_the_result_job_of_the_required_workflow(self):
        # Renaming the job would leave the rule waiting for a check nothing reports,
        # and an admin cannot bypass this ruleset: every PR would be blocked.
        workflow = yaml.safe_load((ROOT / PATH_IN_REPO).read_text())
        context = CURRENCY_RULE["parameters"]["required_status_checks"][0]["context"]
        self.assertEqual(workflow["jobs"]["result"]["name"], context)

    def test_required_context_resolves_to_a_workflow_job(self):
        lint = subprocess.run(
            ["bash", str(ROOT / ".github/scripts/lint-branch-protection.sh"),
             "--branch-protection", str(WORKFLOW_JSON)],
            cwd=ROOT, capture_output=True, text=True)
        self.assertEqual(lint.returncode, 0, lint.stderr)

    def test_every_main_ruleset_keeps_the_deploy_app_bypass(self):
        lint = subprocess.run(["bash", str(ROOT / ".github/scripts/lint-deploy-app-bypass.sh")],
                              cwd=ROOT, capture_output=True, text=True)
        self.assertEqual(lint.returncode, 0, lint.stderr)

    def test_tag_ruleset_is_the_reviewed_body(self):
        self.assertEqual(json.loads(TAG_JSON.read_text()), TAG)

    def test_tag_ruleset_has_no_bypass_actor(self):
        # Not even the deploy bot or an admin may create the reserved tag.
        self.assertEqual(json.loads(TAG_JSON.read_text())["bypass_actors"], [])

    def test_both_appliers_carry_the_same_shared_functions(self):
        # Two scripts, one question each ("does a tag named main exist?", "is this
        # rule still the one I wrote?"). A copy that is hardened alone leaves the
        # other weaker, so the shared functions must stay identical.
        def function_text(script, name):
            source = script.read_text()
            # A later definition of the same name replaces the first when the script
            # runs, and the first is the only one read below, so a second one would
            # let the copies differ with this test still green.
            definitions = re.findall(rf"^[ \t]*(?:function[ \t]+)?{name}[ \t]*(?:\([ \t]*\))?[ \t]*\{{",
                                     source, re.M)
            self.assertEqual(len(definitions), 1, f"{script.name} must define {name}() exactly once")
            match = re.search(rf"^{name}\(\) \{{\n.*?^\}}\n", source, re.S | re.M)
            self.assertIsNotNone(match, f"{script.name} has no {name}()")
            return match.group(0)
        for name in ("tag_main_exists", "remember", "prove_ours", "rollback_created", "fail_after_create"):
            with self.subTest(name):
                self.assertEqual(function_text(WORKFLOW_APPLY, name), function_text(TAG_APPLY, name))


class PinnedShape(Harness):
    """An edited JSON must not be applied: the script pins the reviewed contract."""

    def refused_with(self, mutate=None, raw=None, script=WORKFLOW_APPLY, desired=WORKFLOW_JSON):
        """Run `script` beside an edited copy of its JSON and expect it to refuse
        before it calls GitHub. `raw` is the file text, for non-JSON-object cases."""
        directory = self.tmp / "copy"
        shutil.rmtree(directory, ignore_errors=True)
        directory.mkdir()
        shutil.copy(script, directory / script.name)
        if raw is None:
            body = json.loads(desired.read_text())
            mutate(body)
            raw = json.dumps(body)
        (directory / desired.name).write_text(raw)
        proc, calls, _ = self.run_script(directory / script.name, ["--dry-run"], {"rulesets": []})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("no longer matches the reviewed", proc.stderr)
        self.assertEqual(calls, [], "must refuse before reading or writing GitHub")

    def test_every_edit_of_the_workflow_body_is_refused(self):
        def workflow_entry(body):
            return body["rules"][0]["parameters"]["workflows"][0]
        edits = {
            "name": lambda b: b.update(name="Alpha Required PR Build 2"),
            "target tag": lambda b: b.update(target="tag"),
            "enforcement disabled": lambda b: b.update(enforcement="disabled"),
            "enforcement evaluate": lambda b: b.update(enforcement="evaluate"),
            "another repository added": lambda b: b["conditions"]["repository_id"]["repository_ids"].append(1),
            "another repository only": lambda b: b["conditions"]["repository_id"].update(repository_ids=[2]),
            "every branch": lambda b: b["conditions"]["ref_name"].update(include=["~ALL"]),
            "an exclusion": lambda b: b["conditions"]["ref_name"].update(exclude=["refs/heads/main"]),
            "bypass mode exempt": lambda b: b["bypass_actors"][0].update(bypass_mode="exempt"),
            "another bypass app": lambda b: b["bypass_actors"][0].update(actor_id=1),
            "no bypass actor": lambda b: b.update(bypass_actors=[]),
            "workflow path": lambda b: workflow_entry(b).update(path=".github/workflows/other.yml"),
            "workflow repository": lambda b: workflow_entry(b).update(repository_id=1),
            "workflow pinned to a sha": lambda b: workflow_entry(b).update(sha="a" * 40),
            "a second workflow": lambda b: b["rules"][0]["parameters"]["workflows"].append(
                {"repository_id": 1158152129, "path": ".github/workflows/other.yml", "ref": "refs/heads/main"}),
            "workflow rule skips creation": lambda b: b["rules"][0]["parameters"].update(
                do_not_enforce_on_create=True),
            "currency rule skips creation": lambda b: b["rules"][1]["parameters"].update(
                do_not_enforce_on_create=True),
            "an extra required check": lambda b: b["rules"][1]["parameters"]["required_status_checks"].append(
                {"context": "Gitar", "integration_id": 827041}),
            "an extra rule": lambda b: b["rules"].append({"type": "deletion"}),
            "workflow rule dropped": lambda b: b.update(rules=b["rules"][1:]),
        }
        for label, mutate in edits.items():
            with self.subTest(label):
                self.refused_with(mutate)

    def test_a_file_that_is_not_exactly_one_reviewed_document_is_refused(self):
        # `jq -e` alone judges only the LAST document, so a bad first document
        # followed by the reviewed body would pass it.
        reviewed = WORKFLOW_JSON.read_text()
        bad = json.loads(reviewed)
        bad["bypass_actors"].append({"actor_id": 1, "actor_type": "OrganizationAdmin",
                                     "bypass_mode": "always"})
        for label, raw in (
            ("bad then reviewed", json.dumps(bad) + "\n" + reviewed),
            ("reviewed then bad", reviewed + "\n" + json.dumps(bad)),
            ("reviewed twice", reviewed + "\n" + reviewed),
            ("reviewed then garbage", reviewed + "\nnot json"),
            ("empty", ""),
        ):
            with self.subTest(label):
                self.refused_with(raw=raw)

    def test_every_edit_of_the_tag_body_is_refused(self):
        edits = {
            "name": lambda b: b.update(name="Alpha Reserved Tag Names 2"),
            "target branch": lambda b: b.update(target="branch"),
            "enforcement disabled": lambda b: b.update(enforcement="disabled"),
            "enforcement evaluate": lambda b: b.update(enforcement="evaluate"),
            "another repository added": lambda b: b["conditions"]["repository_id"]["repository_ids"].append(1),
            "another repository only": lambda b: b["conditions"]["repository_id"].update(repository_ids=[2]),
            "every tag": lambda b: b["conditions"]["ref_name"].update(include=["refs/tags/*"]),
            "another tag name": lambda b: b["conditions"]["ref_name"].update(include=["refs/tags/other"]),
            "an exclusion": lambda b: b["conditions"]["ref_name"].update(exclude=["refs/tags/main"]),
            "a bypass actor": lambda b: b.update(bypass_actors=[
                {"actor_id": 3967879, "actor_type": "Integration", "bypass_mode": "always"}]),
            "no rules": lambda b: b.update(rules=[]),
            "an extra rule": lambda b: b["rules"].append({"type": "deletion"}),
            "another rule": lambda b: b.update(rules=[{"type": "deletion"}]),
            "creation with parameters": lambda b: b["rules"][0].update(parameters={}),
        }
        for label, mutate in edits.items():
            with self.subTest(label):
                self.refused_with(mutate, script=TAG_APPLY, desired=TAG_JSON)

    def test_a_tag_file_that_is_not_exactly_one_reviewed_document_is_refused(self):
        reviewed = TAG_JSON.read_text()
        bad = json.loads(reviewed)
        bad["bypass_actors"] = [{"actor_id": 1, "actor_type": "OrganizationAdmin",
                                 "bypass_mode": "always"}]
        for label, raw in (
            ("bad then reviewed", json.dumps(bad) + "\n" + reviewed),
            ("reviewed then bad", reviewed + "\n" + json.dumps(bad)),
            ("reviewed twice", reviewed + "\n" + reviewed),
            ("empty", ""),
        ):
            with self.subTest(label):
                self.refused_with(raw=raw, script=TAG_APPLY, desired=TAG_JSON)

    def test_strict_policy_cannot_be_switched_off(self):
        self.refused_with(lambda body: body["rules"][1]["parameters"].update(
            strict_required_status_checks_policy=False))

    def test_required_check_cannot_be_changed(self):
        self.refused_with(lambda body: body["rules"][1]["parameters"]["required_status_checks"][0]
                          .update(context="Gitar"))

    def test_required_check_app_cannot_be_changed(self):
        self.refused_with(lambda body: body["rules"][1]["parameters"]["required_status_checks"][0]
                          .update(integration_id=1))

    def test_currency_rule_cannot_be_dropped(self):
        self.refused_with(lambda body: body.update(rules=body["rules"][:1]))

    def test_workflow_ref_cannot_be_weakened_to_the_short_name(self):
        self.refused_with(lambda body: body["rules"][0]["parameters"]["workflows"][0]
                          .update(ref="main"))

    def test_admin_cannot_be_added_as_a_bypass_actor(self):
        self.refused_with(lambda body: body["bypass_actors"].append(
            {"actor_id": 1, "actor_type": "OrganizationAdmin", "bypass_mode": "always"}))


class ProtectedSourceGate(Harness):
    """The applier refuses unless the six gate files here equal protected main's."""

    GATE_FILES = [
        ".github/workflows/required-pr-build.yml", ".github/workflows/pr-ci.yml",
        ".github/workflows/osv-scanner.yml", ".github/ci/scan-relevance.yml",
        ".github/scripts/ci-scan-relevance.mjs", ".github/scripts/verify-pr-scan-results.py",
    ]

    def refused(self, error, cwd=ROOT, **cfg):
        # A dry run must refuse too: the diff of a rule that could not be applied is noise.
        for args in (["--dry-run"], ["--apply"]):
            with self.subTest(args=args):
                proc, calls, _ = self.run_script(WORKFLOW_APPLY, args, {"rulesets": [], **cfg}, cwd=cwd)
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn(error, proc.stderr)
                self.assertEqual([c for c in self.kinds(calls) if c in ("post", "put", "delete")], [])
                self.assertNotIn("list", self.kinds(calls), "refuses before reading any ruleset")

    def test_it_reads_the_six_gate_files_at_one_immutable_main_sha(self):
        proc, calls, _ = self.run_script(WORKFLOW_APPLY, ["--dry-run"], {"rulesets": []})
        self.assertEqual(proc.returncode, 0, proc)
        self.assertEqual([c["path"] for c in calls if c["call"] == "contents"], self.GATE_FILES)
        kinds = self.kinds(calls)
        self.assertEqual(kinds.count("commit"), 2, "once before the files, once after")
        self.assertEqual(kinds[0], "commit")
        self.assertEqual(kinds[7], "commit")

    def test_every_gate_file_must_match(self):
        for path in self.GATE_FILES:
            with self.subTest(path=path):
                self.refused("protected source differs from reviewed local gate files", source_stale=[path])

    def test_a_missing_gate_file_is_refused(self):
        for path in (self.GATE_FILES[0], self.GATE_FILES[-1]):
            with self.subTest(path=path):
                self.refused(f"protected source is unavailable: {path}", source_missing=[path])

    def test_a_gate_file_reported_under_another_path_is_refused(self):
        for path in (self.GATE_FILES[0], self.GATE_FILES[-1]):
            with self.subTest(path=path):
                self.refused(f"protected source path or blob is invalid: {path}", source_wrong_path=[path])

    def test_main_that_cannot_be_read_is_refused(self):
        self.refused("cannot read protected main SHA", behave={"commit": {"1": "fail"}})

    def test_a_main_sha_that_is_not_a_sha_is_refused(self):
        self.refused("protected main SHA is invalid", behave={"commit": {"1": "badsha"}})

    def test_main_that_moves_while_validating_is_refused(self):
        self.refused("protected main changed while validating workflow source",
                     behave={"commit": {"2": "move"}})

    def test_main_that_cannot_be_read_again_is_refused(self):
        self.refused("cannot re-read protected main SHA", behave={"commit": {"2": "fail"}})

    def test_a_checkout_that_cannot_hash_its_files_is_refused(self):
        # Run from a directory that is not the repository: the relative gate paths
        # do not exist there, so the local blob cannot be computed.
        self.refused("cannot hash reviewed local source", cwd=self.tmp)

    def test_an_update_is_held_to_the_same_gate(self):
        proc, calls, _ = self.run_script(
            WORKFLOW_APPLY, ["--update", "--snapshot-dir", str(self.tmp / "s"), "--apply"],
            {"rulesets": [{"id": 123, "name": V1["name"]}], "live": live(V1),
             "source_stale": [self.GATE_FILES[1]]})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("protected source differs from reviewed local gate files", proc.stderr)
        self.assertEqual(self.puts(calls), [])


class CreateRollbackIsAttributed:
    """The create path may delete or disable only a rule it can show it wrote.

    A new rule is staged disabled, read back, then activated, and a failure in
    between takes it down again. Another administrator can have edited it in the
    meantime, and deleting it then destroys their work as surely as an overwrite.
    The proof is the live state matching a body this run sent, or one the server
    returned for a write of this run. Mixed into both appliers; the class that
    mixes it in supplies `apply` and `mutations`."""

    def edited(self, state):
        return state is not None and state["conditions"]["ref_name"]["exclude"] == ["refs/heads/other"]

    def test_an_edit_that_lands_before_the_rollback_is_never_deleted(self):
        for label, at, expected in (
            ("after the create", "1", ["post"]),
            ("before the activation", "2", ["post"]),
            ("after the activation", "3", ["post", "put"]),
        ):
            with self.subTest(label):
                proc, calls, state = self.apply(behave={"get": {at: "edited"}})
                self.assertEqual(proc.returncode, 1, proc)
                self.assertEqual(self.mutations(calls), expected, "nothing is deleted or rewritten")
                self.assertTrue(self.edited(state), "their edit is still live")
                self.assertIn("CRITICAL: ruleset 123 no longer reads back as a state this run wrote",
                              proc.stderr)
                self.assertIn("settings/rules/123", proc.stderr, "names the rule to inspect")
                self.assertNotIn("ROLLBACK", proc.stderr)

    def test_a_rule_that_cannot_be_read_again_is_left_in_place(self):
        # The readback that starts the rollback is wrong, and so is the one that
        # would prove the rule is still ours: it fails, is not JSON, or succeeds
        # with nothing in it. The last is the one an empty line in the list of
        # remembered states would match, so it needs its own case.
        for how in ("fail", "garbage", "empty"):
            with self.subTest(how):
                proc, calls, state = self.apply(behave={"get": {"1": "drift", "2": how}})
                self.assertEqual(proc.returncode, 1, proc)
                self.assertEqual(self.mutations(calls), ["post"])
                self.assertIsNotNone(state, "the rule still exists")
                self.assertIn("CRITICAL: ruleset 123 no longer reads back", proc.stderr)

    def test_the_disabling_fallback_is_not_sent_over_an_edit(self):
        # The activation fails, the delete fails, and an edit lands before the
        # fallback would disable the rule. Gets: 1 readback, 2 re-read, 3 first
        # proof, 4 second proof (edited).
        proc, calls, state = self.apply(
            behave={"put": {"1": "fail"}, "delete": {"1": "fail"}, "get": {"4": "edited"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertEqual(len(self.puts(calls)), 1, "only the activation attempt: nothing over their edit")
        self.assertTrue(self.edited(state))
        self.assertIn("CRITICAL: ruleset 123 no longer reads back", proc.stderr)
        self.assertNotIn("is disabled", proc.stderr)

    def test_a_rule_the_server_stored_differently_is_still_removed(self):
        # The server's own account of what it stored is part of the proof, so a
        # normalised body is not mistaken for someone else's edit.
        for label, behave in (("at the create", {"post": {"1": "renamed"}}),
                              ("at the activation", {"put": {"1": "renamed"}})):
            with self.subTest(label):
                proc, calls, state = self.apply(behave=behave)
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("ROLLBACK: deleted new ruleset 123", proc.stderr)
                self.assertIsNone(state)
                self.assertNotIn("CRITICAL", proc.stderr)

    def test_a_create_response_without_a_body_proves_only_what_was_sent(self):
        # Gets: 1 readback, 2 re-read (drifted), 3 proof.
        proc, calls, state = self.apply(behave={"post": {"1": "bodyless"}, "get": {"2": "drift"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("ROLLBACK: deleted new ruleset 123", proc.stderr)
        self.assertIsNone(state, "an unedited rule is the body this run sent")
        proc, calls, state = self.apply(behave={"post": {"1": "bodyless"}, "get": {"2": "edited"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertEqual(self.mutations(calls), ["post"])
        self.assertTrue(self.edited(state), "an edit is not covered by what was sent")
        self.assertIn("CRITICAL: ruleset 123 no longer reads back", proc.stderr)

    def test_an_answer_with_two_documents_is_not_proof(self):
        # More than one document is not one state, and each line of it would match the
        # remembered states on its own. The edit lands at the first readback; the proof
        # then reads that edited rule followed by a copy of the body this run sent.
        proc, calls, state = self.apply(behave={"get": {"1": "edited", "2": "multidoc"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertEqual(self.mutations(calls), ["post"], "nothing is deleted or rewritten")
        self.assertTrue(self.edited(state), "their edit is still live")
        self.assertIn("CRITICAL: ruleset 123 no longer reads back", proc.stderr)
        self.assertNotIn("ROLLBACK", proc.stderr)

    def test_a_write_response_with_two_documents_remembers_none_of_them(self):
        # The activation answers with the rule and then a copy that carries the edit an
        # administrator makes next. Remembering that copy would let the edit read as ours
        # once it lands at the active readback.
        proc, calls, state = self.apply(behave={"put": {"1": "multidoc"}, "get": {"3": "edited"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertEqual(self.mutations(calls), ["post", "put"], "nothing is deleted or rewritten")
        self.assertTrue(self.edited(state), "their edit is still live")
        self.assertIn("CRITICAL: ruleset 123 no longer reads back", proc.stderr)
        self.assertNotIn("ROLLBACK", proc.stderr)

    def test_the_body_this_run_sends_is_kept_in_a_private_directory(self):
        # Staged beside other users' files, the body could be swapped between being
        # written and being sent. With TMPDIR pointing at a directory anyone may write,
        # it must still be staged in a private one of its own, and removed afterwards.
        # GNU mktemp honours TMPDIR and the BSD one on macOS does not, so a shim makes the
        # scenario the same on both; without it this test could not fail on a Mac.
        shim = self.bin / "mktemp"
        shim.write_text('#!/bin/sh\n'
                        'dir=${TMPDIR:-/tmp}\n'
                        'case "$1" in\n'
                        '  -d) command -p mktemp -d "$dir/tmp.XXXXXXXXXX" ;;\n'
                        '  *) command -p mktemp "$dir/tmp.XXXXXXXXXX" ;;\n'
                        'esac\n')
        shim.chmod(0o755)
        shared = self.tmp / "shared-tmp"
        shared.mkdir()
        shared.chmod(0o777)
        with unittest.mock.patch.dict(os.environ, {"TMPDIR": str(shared)}):
            proc, calls, _ = self.apply()
        self.assertEqual(proc.returncode, 0, proc)
        post = next(call for call in calls if call["call"] == "post")
        self.assertNotEqual(Path(post["dir"]).resolve(), shared.resolve(), "not in the shared directory itself")
        self.assertEqual(post["dir_mode"], 0o700)
        self.assertEqual(list(shared.iterdir()), [], "nothing is left behind")


class WorkflowApplierCreate(CreateRollbackIsAttributed, Harness):
    def apply(self, args=("--apply",), **cfg):
        return self.run_script(WORKFLOW_APPLY, list(args), {"rulesets": [], **cfg})

    def mutations(self, calls):
        return [c for c in self.kinds(calls) if c in ("post", "put", "delete")]

    def test_dry_run_and_the_default_create_nothing(self):
        for args in (["--dry-run"], []):
            with self.subTest(args=args):
                proc, calls, _ = self.apply(args)
                self.assertEqual(proc.returncode, 0, proc)
                self.assertIn("[dry-run] would create", proc.stdout)
                self.assertEqual(self.mutations(calls), [])

    def test_apply_creates_disabled_then_activates(self):
        proc, calls, state = self.apply()
        self.assertEqual(proc.returncode, 0, proc)
        kinds = self.kinds(calls)
        self.assertLess(kinds.index("post"), kinds.index("put"))
        post = next(c["payload"] for c in calls if c["call"] == "post")
        self.assertEqual(post, {**V2, "enforcement": "disabled"})
        self.assertEqual(self.puts(calls), [V2])
        self.assertEqual(canon(state), canon(V2))
        self.assertEqual(kinds.count("get"), 3, "create readback, re-read, active readback")
        self.assertIn("VERIFIED", proc.stdout)

    def test_failures_after_create_remove_the_new_ruleset(self):
        for label, behave, reason in (
            ("activation write fails", {"put": {"1": "fail"}}, "activation request failed"),
            ("readback after create unreadable", {"get": {"1": "fail"}}, "failed to read disabled ruleset"),
            ("readback after create malformed", {"get": {"1": "garbage"}}, "disabled ruleset readback is malformed"),
            ("readback after create differs", {"get": {"1": "drift"}}, "disabled ruleset readback differs"),
            ("re-read before activation unreadable", {"get": {"2": "fail"}}, "failed to re-read disabled ruleset"),
            ("re-read before activation malformed", {"get": {"2": "garbage"}}, "disabled ruleset re-read is malformed"),
            ("re-read before activation drifted", {"get": {"2": "drift"}}, "disabled ruleset changed before activation"),
            ("activation accepted but not applied", {"put": {"1": "ignored"}}, "active ruleset readback differs"),
            ("active readback unreadable", {"get": {"3": "fail"}}, "failed to read active ruleset"),
            ("active readback malformed", {"get": {"3": "garbage"}}, "active ruleset readback is malformed"),
        ):
            with self.subTest(label):
                proc, calls, state = self.apply(behave=behave)
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn(reason, proc.stderr)
                self.assertIn("ROLLBACK: deleted new ruleset 123", proc.stderr)
                self.assertIn("delete", self.kinds(calls))
                self.assertIsNone(state, "the new ruleset is gone")
                self.assertNotIn("CRITICAL", proc.stderr)

    def test_a_ruleset_that_cannot_be_removed_falls_back_to_disabling_it(self):
        proc, calls, state = self.apply(behave={"put": {"1": "fail"}, "delete": {"1": "fail"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertEqual(state["enforcement"], "disabled")
        self.assertIn("ROLLBACK: ruleset 123 is disabled", proc.stderr)
        self.assertNotIn("CRITICAL", proc.stderr)

    def test_a_rollback_that_cannot_be_verified_is_critical(self):
        # Activation reports failure but landed (the rule is ACTIVE) and the delete
        # fails; then the disabling write either fails or is accepted and ignored.
        for how in ("fail", "ignored"):
            with self.subTest(how):
                proc, calls, state = self.apply(
                    behave={"put": {"1": "fail-applied", "2": how}, "delete": {"1": "fail"}})
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("CRITICAL: rollback of ruleset 123 could not be verified", proc.stderr)
                self.assertIn("settings/rules/123", proc.stderr, "names the rule to clean up by hand")

    def test_an_unconfirmed_create_is_critical(self):
        for how, text in (("fail", "create request failed and its outcome is unknown"),
                          ("noid", "create returned no ruleset ID")):
            with self.subTest(how):
                proc, calls, _ = self.apply(behave={"post": {"1": how}})
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn(f"CRITICAL: {text}", proc.stderr)
                self.assertNotIn("delete", self.kinds(calls), "nothing is known to delete")

    def test_an_existing_ruleset_in_any_other_state_is_refused(self):
        body = copy.deepcopy(V2)
        body["enforcement"] = "disabled"
        proc, calls, _ = self.apply(rulesets=[{"id": 123, "name": V2["name"]}], live=live(body))
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("differs from desired state; refusing replacement", proc.stderr)
        self.assertEqual(self.mutations(calls), [])

    def test_duplicate_rulesets_are_refused(self):
        proc, calls, _ = self.apply(
            rulesets=[{"id": 123, "name": V2["name"]}, {"id": 124, "name": V2["name"]}], live=live(V1))
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("duplicate Alpha Required PR Build rulesets", proc.stderr)
        self.assertEqual(self.mutations(calls), [])

    def test_an_unreadable_ruleset_list_blocks(self):
        proc, calls, _ = self.apply(behave={"list": {"1": "fail"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("cannot read organization rulesets", proc.stderr)
        self.assertEqual(self.mutations(calls), [])

    def test_a_malformed_or_unreadable_existing_ruleset_is_refused(self):
        for how, text in (("garbage", "ruleset 123 response is malformed"), ("fail", "cannot read ruleset 123")):
            with self.subTest(how):
                proc, calls, _ = self.apply(rulesets=[{"id": 123, "name": V2["name"]}], live=live(V2),
                                            behave={"get": {"1": how}})
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn(text, proc.stderr)
                self.assertEqual(self.mutations(calls), [])

    def test_a_malformed_ruleset_list_is_refused(self):
        proc, calls, _ = self.apply(behave={"list": {"1": "garbage"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("the organization ruleset list is malformed", proc.stderr)
        self.assertEqual(self.mutations(calls), [])

    def test_a_ruleset_id_that_is_not_a_positive_integer_is_refused(self):
        for bad in ("abc", 0, -5, "01"):
            with self.subTest(id=bad):
                proc, calls, _ = self.apply(rulesets=[{"id": bad, "name": V2["name"]}], live=live(V2))
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("invalid ruleset ID", proc.stderr)
                self.assertEqual(self.mutations(calls), [])

    def test_the_list_is_read_across_pages(self):
        pages = [[{"id": 5, "name": "Other"}, {"id": 6, "name": "More"}],
                 [{"id": 123, "name": V2["name"]}]]
        proc, calls, _ = self.apply(rulesets_pages=pages, live=live(V2))
        self.assertEqual(proc.returncode, 0, proc)
        self.assertIn("already matches desired state", proc.stdout)

    def test_the_same_ruleset_on_two_pages_is_not_a_duplicate(self):
        pages = [[{"id": 123, "name": V2["name"]}], [{"id": 123, "name": V2["name"]}]]
        proc, calls, _ = self.apply(rulesets_pages=pages, live=live(V2))
        self.assertEqual(proc.returncode, 0, proc)

    def test_two_rulesets_on_two_pages_are_duplicates(self):
        pages = [[{"id": 123, "name": V2["name"]}], [{"id": 124, "name": V2["name"]}]]
        proc, calls, _ = self.apply(rulesets_pages=pages, live=live(V2))
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("duplicate Alpha Required PR Build rulesets", proc.stderr)

    def test_live_previous_state_points_at_update(self):
        proc, calls, _ = self.run_script(
            WORKFLOW_APPLY, ["--apply"],
            {"rulesets": [{"id": 123, "name": V1["name"]}], "live": live(V1)})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("--update", proc.stderr)
        self.assertEqual([c for c in self.kinds(calls) if c in ("put", "post", "delete")], [])

    def test_live_desired_state_is_a_no_op(self):
        proc, calls, _ = self.run_script(
            WORKFLOW_APPLY, ["--apply"],
            {"rulesets": [{"id": 123, "name": V2["name"]}], "live": live(V2)})
        self.assertEqual(proc.returncode, 0, proc)
        self.assertIn("already matches desired state", proc.stdout)
        self.assertEqual([c for c in self.kinds(calls) if c in ("put", "post", "delete")], [])


class WorkflowApplierUpdate(Harness):
    ONE = {"rulesets": [{"id": 123, "name": V1["name"]}]}

    def update(self, extra=(), **cfg):
        snapshots = self.tmp / "snapshots"
        return (*self.run_script(
            WORKFLOW_APPLY, ["--update", "--snapshot-dir", str(snapshots), *extra],
            {**self.ONE, **cfg}), snapshots)

    def test_usage_errors_exit_two(self):
        for args in (["--update"], ["--snapshot-dir", "x"], ["--update", "--rollback"],
                     ["--dry-run", "--apply"], ["--bogus"], ["--rollback", "extra"]):
            with self.subTest(args=args):
                proc, calls, _ = self.run_script(WORKFLOW_APPLY, args, self.ONE)
                self.assertEqual(proc.returncode, 2, proc)
                self.assertEqual(calls, [])

    def test_dry_run_prints_the_diff_and_writes_nothing(self):
        proc, calls, state, snapshots = self.update(live=live(V1))
        self.assertEqual(proc.returncode, 0, proc)
        self.assertIn("[dry-run]", proc.stdout)
        self.assertIn('+            "ref": "refs/heads/main"', proc.stdout)
        self.assertIn('-            "ref": "main"', proc.stdout)
        self.assertIn("required_status_checks", proc.stdout)
        self.assertEqual([c for c in self.kinds(calls) if c in ("put", "post", "delete")], [])
        self.assertFalse(snapshots.exists(), "a dry run must not create the snapshot directory")

    def test_apply_writes_once_and_saves_the_previous_body(self):
        proc, calls, state, snapshots = self.update(["--apply"], live=live(V1))
        self.assertEqual(proc.returncode, 0, proc)
        self.assertEqual(len(self.puts(calls)), 1, self.kinds(calls))
        self.assertEqual(self.puts(calls)[0], V2)
        self.assertEqual(canon(state), canon(V2))
        saved = sorted(snapshots.glob("alpha-required-pr-build-123-previous-*.json"))
        self.assertEqual(len(saved), 1, list(snapshots.iterdir()))
        self.assertEqual(canon(json.loads(saved[0].read_text())), canon(V1))
        self.assertEqual(stat.S_IMODE(saved[0].stat().st_mode), 0o600)
        evidence = list(snapshots.glob("*-live-*.json"))
        self.assertEqual(len(evidence), 1, "the raw pre-change API response is kept as evidence")
        self.assertEqual(stat.S_IMODE(evidence[0].stat().st_mode), 0o600)
        self.assertIn("VERIFIED", proc.stdout)
        self.assertIn("--rollback", proc.stdout)
        # check, re-read before the write, readback
        self.assertGreaterEqual(self.kinds(calls).count("get"), 3)

    def test_apply_is_idempotent_when_live_is_already_desired(self):
        proc, calls, _, snapshots = self.update(["--apply"], live=live(V2))
        self.assertEqual(proc.returncode, 0, proc)
        self.assertIn("already matches desired state", proc.stdout)
        self.assertEqual(self.puts(calls), [])
        self.assertFalse(snapshots.exists())

    def test_a_server_that_omits_default_false_parameters_still_matches(self):
        # The API may leave out do_not_enforce_on_create when it is false; the reviewed
        # body states it, and a rule without it is the same rule.
        omitted = live(V2)
        for rule in omitted["rules"]:
            rule["parameters"].pop("do_not_enforce_on_create")
        proc, calls, _, _ = self.update(["--apply"], live=omitted)
        self.assertEqual(proc.returncode, 0, proc)
        self.assertIn("already matches desired state", proc.stdout)
        self.assertEqual(self.puts(calls), [])

    def test_rule_order_from_the_server_does_not_matter(self):
        reordered = live(V2)
        reordered["rules"].reverse()
        proc, calls, _, _ = self.update(["--apply"], live=reordered)
        self.assertEqual(proc.returncode, 0, proc)
        self.assertIn("already matches desired state", proc.stdout)
        self.assertEqual(self.puts(calls), [])

    def test_a_drifted_ruleset_is_refused(self):
        for label, mutate in (
            ("extra bypass actor", lambda b: b["bypass_actors"].append(
                {"actor_id": 1, "actor_type": "OrganizationAdmin", "bypass_mode": "always"})),
            ("enforcement", lambda b: b.update(enforcement="disabled")),
            ("strict off", lambda b: b["rules"].append(
                {**copy.deepcopy(CURRENCY_RULE),
                 "parameters": {**CURRENCY_RULE["parameters"],
                                "strict_required_status_checks_policy": False}})),
        ):
            # A dry run must refuse too: otherwise it prints a diff for a write the
            # apply would then reject, and the pre-write re-read hides a missing check.
            for args in (["--apply"], []):
                with self.subTest(label=label, args=args):
                    body = copy.deepcopy(V1)
                    mutate(body)
                    proc, calls, _, _ = self.update(args, live=live(body))
                    self.assertEqual(proc.returncode, 1, proc)
                    self.assertIn("differs from both the reviewed previous and the desired", proc.stderr)
                    self.assertEqual(self.puts(calls), [])

    def test_a_missing_ruleset_is_refused(self):
        proc, calls, _, _ = self.update(["--apply"], rulesets=[])
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("does not exist", proc.stderr)
        self.assertEqual(self.puts(calls), [])

    def test_duplicate_rulesets_are_refused(self):
        proc, calls, _, _ = self.update(
            ["--apply"], live=live(V1),
            rulesets=[{"id": 123, "name": V1["name"]}, {"id": 124, "name": V1["name"]}])
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("duplicate Alpha Required PR Build rulesets", proc.stderr)
        self.assertEqual(self.puts(calls), [])

    def test_a_ruleset_id_that_is_not_a_positive_integer_is_refused(self):
        for bad in ("abc", 0, -5, "01"):
            with self.subTest(id=bad):
                proc, calls, _, _ = self.update(["--apply"], live=live(V1),
                                                rulesets=[{"id": bad, "name": V1["name"]}])
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("invalid ruleset ID", proc.stderr)
                self.assertEqual(self.puts(calls), [])

    def test_the_list_is_read_across_pages(self):
        pages = [[{"id": 5, "name": "Other"}, {"id": 6, "name": "More"}],
                 [{"id": 123, "name": V1["name"]}]]
        proc, calls, state, _ = self.update(["--apply"], live=live(V1), rulesets_pages=pages)
        self.assertEqual(proc.returncode, 0, proc)
        self.assertEqual(canon(state), canon(V2))

    def test_the_same_ruleset_on_two_pages_is_not_a_duplicate(self):
        pages = [[{"id": 123, "name": V1["name"]}], [{"id": 123, "name": V1["name"]}]]
        proc, calls, _, _ = self.update(["--apply"], live=live(V1), rulesets_pages=pages)
        self.assertEqual(proc.returncode, 0, proc)

    def test_two_rulesets_on_two_pages_are_duplicates(self):
        pages = [[{"id": 123, "name": V1["name"]}], [{"id": 124, "name": V1["name"]}]]
        proc, calls, _, _ = self.update(["--apply"], live=live(V1), rulesets_pages=pages)
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("duplicate Alpha Required PR Build rulesets", proc.stderr)
        self.assertEqual(self.puts(calls), [])

    def test_a_live_ruleset_without_bypass_actors_is_drift(self):
        proc, calls, _, _ = self.update(["--apply"], live=live(V1), behave={"get": {"1": "no-bypass"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("differs from both the reviewed previous and the desired", proc.stderr)
        self.assertEqual(self.puts(calls), [])

    def test_a_malformed_live_response_is_refused(self):
        for args in (["--apply"], []):
            with self.subTest(args=args):
                proc, calls, _, _ = self.update(args, live=live(V1), behave={"get": {"1": "garbage"}})
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("response is malformed", proc.stderr)
                self.assertEqual(self.puts(calls), [])

    def test_a_ruleset_that_cannot_be_read_is_refused(self):
        proc, calls, _, _ = self.update(["--apply"], live=live(V1), behave={"get": {"1": "fail"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("cannot read ruleset 123", proc.stderr)
        self.assertEqual(self.puts(calls), [])

    def test_a_failed_re_read_before_the_write_is_refused(self):
        proc, calls, _, _ = self.update(["--apply"], live=live(V1), behave={"get": {"2": "fail"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("cannot re-read ruleset 123 before writing", proc.stderr)
        self.assertEqual(self.puts(calls), [])

    def test_a_malformed_re_read_before_the_write_is_refused(self):
        proc, calls, _, _ = self.update(["--apply"], live=live(V1), behave={"get": {"2": "garbage"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("re-read is malformed", proc.stderr)
        self.assertEqual(self.puts(calls), [])

    def test_an_unreadable_ruleset_list_blocks(self):
        proc, calls, _, _ = self.update(["--apply"], live=live(V1), behave={"list": {"1": "fail"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("cannot read organization rulesets", proc.stderr)
        self.assertEqual(self.puts(calls), [])

    def test_a_stale_local_checkout_is_refused(self):
        proc, calls, _, _ = self.update(["--apply"], live=live(V1),
                                        source_stale=[".github/workflows/pr-ci.yml"])
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("protected source differs from reviewed local gate files", proc.stderr)
        self.assertEqual(self.puts(calls), [])

    def test_a_failed_write_leaves_the_ruleset_unchanged(self):
        proc, calls, state, _ = self.update(["--apply"], live=live(V1),
                                            behave={"put": {"1": "fail"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("is unchanged", proc.stderr)
        self.assertEqual(len(self.puts(calls)), 1, "no restore write when nothing changed")
        self.assertNotIn("CRITICAL", proc.stderr)

    def test_a_write_the_server_ignored_is_reported(self):
        proc, calls, _, _ = self.update(["--apply"], live=live(V1),
                                        behave={"put": {"1": "ignored"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("did not take effect", proc.stderr)
        self.assertEqual(len(self.puts(calls)), 1)

    def test_a_failed_write_that_landed_is_success_with_a_warning(self):
        proc, calls, state, _ = self.update(["--apply"], live=live(V1),
                                            behave={"put": {"1": "fail-applied"}})
        self.assertEqual(proc.returncode, 0, proc)
        self.assertIn("WARNING", proc.stderr)
        self.assertEqual(canon(state), canon(V2))
        self.assertEqual(len(self.puts(calls)), 1)

    def test_a_bad_readback_restores_the_previous_body(self):
        proc, calls, state, _ = self.update(["--apply"], live=live(V1),
                                            behave={"put": {"1": "mangled"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertEqual(len(self.puts(calls)), 2, self.kinds(calls))
        self.assertEqual(self.puts(calls)[1], V1)
        self.assertEqual(canon(state), canon(V1))
        self.assertIn("ROLLBACK: restored the reviewed previous state", proc.stderr)
        self.assertNotIn("CRITICAL", proc.stderr)

    def test_a_failed_restore_is_critical_and_names_the_way_back(self):
        proc, calls, state, snapshots = self.update(
            ["--apply"], live=live(V1), behave={"put": {"1": "mangled", "2": "fail"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("CRITICAL: the previous body could not be restored", proc.stderr)
        self.assertIn("--rollback", proc.stderr)
        saved = next(snapshots.glob("alpha-required-pr-build-123-previous-*.json"))
        self.assertIn(str(saved), proc.stderr)

    def test_an_unreadable_ruleset_after_the_write_is_critical(self):
        proc, calls, _, _ = self.update(["--apply"], live=live(V1),
                                        behave={"get": {"3": "fail"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("CRITICAL: ruleset 123 could not be read back after the write", proc.stderr)
        self.assertEqual(len(self.puts(calls)), 1, "state unknown: never write blindly")

    def test_a_ruleset_changed_by_someone_else_is_not_overwritten(self):
        # The write failed and the readback shows neither reviewed state.
        proc, calls, _, _ = self.update(["--apply"], live=live(V1),
                                        behave={"put": {"1": "fail"}, "get": {"3": "drift"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("CRITICAL: ruleset 123 is in neither reviewed state", proc.stderr)
        self.assertIn("will not overwrite it", proc.stderr)
        self.assertEqual(len(self.puts(calls)), 1)
        # The message names the saved previous body, but only as a by-hand step after
        # the operator has confirmed the live state is theirs to replace.
        self.assertIn("once you have confirmed the live state is yours to replace", proc.stderr)
        self.assertIn("restore the saved previous body: gh api -X PUT orgs/Concord-Voice/rulesets/123 --input",
                      proc.stderr)

    def test_a_concurrent_edit_after_the_write_is_not_overwritten(self):
        # Another administrator revokes every bypass actor between our write and our
        # readback. The readback is in neither reviewed state, but it is not what our
        # write produced either, so "restoring" the previous body would silently undo
        # their edit. The script must stop with the evidence and write nothing more.
        proc, calls, state, _ = self.update(["--apply"], live=live(V1),
                                            behave={"get": {"3": "concurrent"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertEqual(self.puts(calls), [V2], "exactly one write: nothing restored over their edit")
        self.assertEqual(state["bypass_actors"], [], "their edit is still live")
        self.assertIn("CRITICAL: ruleset 123 changed after this run's write", proc.stderr)
        self.assertIn("will not overwrite it", proc.stderr)
        self.assertNotIn("ROLLBACK", proc.stderr)

    def test_a_concurrent_edit_is_not_overwritten_when_the_write_was_ignored(self):
        # The server ignored our write (live stays at the previous body) and then the
        # other administrator's edit landed: the readback is not ours in any sense.
        proc, calls, state, _ = self.update(["--apply"], live=live(V1),
                                            behave={"put": {"1": "ignored"}, "get": {"3": "concurrent"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertEqual(len(self.puts(calls)), 1)
        self.assertEqual(state["bypass_actors"], [])
        self.assertIn("will not overwrite it", proc.stderr)

    def test_an_unparseable_write_response_blocks_the_restore(self):
        # The server mangled our write, and its response cannot be read, so nothing
        # proves the readback is our own write rather than someone else's edit.
        proc, calls, _, _ = self.update(["--apply"], live=live(V1),
                                        behave={"put": {"1": "mangled-garbage"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertEqual(len(self.puts(calls)), 1)
        self.assertIn("CRITICAL: ruleset 123 changed after this run's write", proc.stderr)

    def test_the_restore_is_not_sent_if_the_ruleset_changes_again_before_it(self):
        # get calls: 1 check, 2 re-read before the write, 3 readback (mangled),
        # 4 re-read before the restore, 5 readback after the restore.
        proc, calls, state, _ = self.update(["--apply"], live=live(V1),
                                            behave={"put": {"1": "mangled"}, "get": {"4": "concurrent"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertEqual(len(self.puts(calls)), 1, "the restore must not run over a newer edit")
        self.assertEqual(state["bypass_actors"], [])
        self.assertIn("CRITICAL: ruleset 123 changed again before the previous body could be restored",
                      proc.stderr)

    def test_a_restore_that_does_not_verify_is_critical(self):
        proc, calls, _, _ = self.update(["--apply"], live=live(V1),
                                        behave={"put": {"1": "mangled", "2": "ignored"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertEqual(len(self.puts(calls)), 2)
        self.assertIn("CRITICAL: the previous body could not be restored", proc.stderr)

    def test_a_swapped_snapshot_file_cannot_become_the_restore_body(self):
        # The restore PUT is sent from a private copy. The snapshot directory is
        # evidence only: a file swapped there mid-run must never be what gets PUT.
        snapshots = self.tmp / "snapshots"
        proc, calls, state, _ = self.update(["--apply"], live=live(V1), tamper_dir=str(snapshots),
                                            behave={"put": {"1": "mangled"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertEqual(self.puts(calls), [V2, V1])
        self.assertEqual(canon(state), canon(V1))

    STAMPED = "alpha-required-pr-build-123-{kind}-20990101T000000Z.json"

    def fix_the_clock(self):
        # Two runs in the same second build the same file names.
        clock = self.bin / "date"
        clock.write_text("#!/bin/sh\nprintf '20990101T000000Z\\n'\n")
        clock.chmod(0o755)

    def test_an_existing_evidence_file_is_never_overwritten(self):
        self.fix_the_clock()
        snapshots = self.tmp / "snapshots"
        snapshots.mkdir(mode=0o700)
        for kind in ("live", "previous"):
            with self.subTest(kind=kind):
                earlier = snapshots / self.STAMPED.format(kind=kind)
                earlier.write_text("the earlier run's evidence")
                proc, calls, _, _ = self.update(["--apply"], live=live(V1))
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("is not overwritten", proc.stderr)
                self.assertEqual(earlier.read_text(), "the earlier run's evidence")
                self.assertEqual(self.puts(calls), [], "no write before the evidence is safe")
                earlier.unlink()
                for left in snapshots.iterdir():
                    left.unlink()

    def test_a_planted_link_is_not_followed(self):
        self.fix_the_clock()
        snapshots = self.tmp / "snapshots"
        victim = self.tmp / "victim.txt"
        dangling = self.tmp / "dangling.txt"
        for kind in ("live", "previous"):
            for target in (victim, dangling):
                with self.subTest(kind=kind, target=target.name):
                    shutil.rmtree(snapshots, ignore_errors=True)
                    snapshots.mkdir(mode=0o700)
                    victim.write_text("keep me")
                    (snapshots / self.STAMPED.format(kind=kind)).symlink_to(target)
                    proc, calls, _, _ = self.update(["--apply"], live=live(V1))
                    self.assertEqual(proc.returncode, 1, proc)
                    self.assertEqual(victim.read_text(), "keep me")
                    self.assertFalse(dangling.exists(), "a dangling link must not be created through")
                    self.assertEqual(self.puts(calls), [])

    def test_a_directory_others_can_write_is_refused(self):
        snapshots = self.tmp / "snapshots"
        snapshots.mkdir()
        for mode in (0o770, 0o707, 0o777, 0o1777):
            with self.subTest(mode=oct(mode)):
                snapshots.chmod(mode)
                proc, calls, _, _ = self.update(["--apply"], live=live(V1))
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("not writable by group or others", proc.stderr)
                self.assertEqual(self.puts(calls), [])
                self.assertEqual(list(snapshots.iterdir()), [], "nothing is written into it")
        # Control: a directory only you can write is accepted, so the refusals above
        # are about the permissions and not about every existing directory.
        snapshots.chmod(0o755)
        proc, calls, _, _ = self.update(["--apply"], live=live(V1))
        self.assertEqual(proc.returncode, 0, proc)

    @unittest.skipIf(os.geteuid() == 0, "root owns the directory used here")
    def test_a_directory_you_do_not_own_is_refused(self):
        proc, calls, _ = self.run_script(
            WORKFLOW_APPLY, ["--update", "--snapshot-dir", "/usr", "--apply"],
            {**self.ONE, "live": live(V1)})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("must be owned by you", proc.stderr)
        self.assertEqual(self.puts(calls), [])

    def update_into(self, snapshots, **cfg):
        return self.run_script(WORKFLOW_APPLY, ["--update", "--snapshot-dir", str(snapshots), "--apply"],
                               {**self.ONE, "live": live(V1), **cfg})

    def test_a_directory_below_one_others_can_write_is_refused(self):
        # Whoever can rename an entry in a directory above can swap the snapshot
        # directory for their own once it has been checked, and then read or plant
        # the saved bodies. The whole chain is judged, not only the last step.
        above = self.tmp / "above"
        for mode in (0o777, 0o775, 0o757):
            for between in ((), ("mid",)):
                with self.subTest(mode=oct(mode), between=between):
                    shutil.rmtree(above, ignore_errors=True)
                    snapshots = above.joinpath(*between, "snapshots")
                    snapshots.mkdir(parents=True, mode=0o700)
                    above.chmod(mode)
                    proc, calls, _ = self.update_into(snapshots)
                    self.assertEqual(proc.returncode, 1, proc)
                    self.assertIn("above the snapshot directory", proc.stderr)
                    self.assertIn(str(above.resolve()), proc.stderr, "names the directory to change")
                    self.assertEqual(self.puts(calls), [])
                    self.assertEqual(list(snapshots.iterdir()), [], "nothing is written into it")
                    # Control: the same layout under a directory only you can write.
                    above.chmod(0o755)
                    proc, calls, _ = self.update_into(snapshots)
                    self.assertEqual(proc.returncode, 0, proc)

    def test_a_sticky_directory_above_is_accepted(self):
        # /tmp is world-writable and sticky: only an entry's owner can rename it.
        sticky = self.tmp / "sticky"
        sticky.mkdir()
        sticky.chmod(0o1777)
        proc, calls, _ = self.update_into(sticky / "snapshots")
        self.assertEqual(proc.returncode, 0, proc)
        self.assertEqual(len(self.puts(calls)), 1)

    def test_a_link_is_resolved_before_the_path_is_judged(self):
        above = self.tmp / "above"
        unsafe = above / "snapshots"
        unsafe.mkdir(parents=True, mode=0o700)
        above.chmod(0o777)
        link = self.tmp / "link"
        link.symlink_to(unsafe)
        proc, calls, _ = self.update_into(link)
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("above the snapshot directory", proc.stderr)
        self.assertEqual(self.puts(calls), [])
        # A link to a directory that is fine is followed, and what is printed is the
        # real path: a restore command must not go through a link that can move.
        safe = self.tmp / "safe" / "snapshots"
        safe.mkdir(parents=True, mode=0o700)
        good = self.tmp / "good-link"
        good.symlink_to(safe)
        proc, calls, _ = self.update_into(good)
        self.assertEqual(proc.returncode, 0, proc)
        self.assertEqual(len(list(safe.glob("alpha-required-pr-build-123-previous-*.json"))), 1)
        saved = next(line for line in proc.stdout.splitlines() if line.startswith("Previous body saved: "))
        self.assertTrue(saved.split(": ", 1)[1].startswith(f"{safe.resolve()}/"), saved)
        self.assertNotIn("good-link", saved)

    def test_a_newline_in_the_real_path_is_refused(self):
        # `$(...)` drops trailing newlines, so a path part that ends in one used to be
        # judged as its twin without it: the twin passed and the real directory was never
        # looked at. A path with a newline anywhere in it is refused outright.
        sticky = self.tmp / "sticky"
        sticky.mkdir()
        sticky.chmod(0o1777)
        # Control: the same unsafe parent under an ordinary name is refused for what it is.
        plain = sticky / "plain"
        plain.mkdir()
        plain.chmod(0o777)
        proc, calls, _ = self.update_into(plain / "run1")
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("above the snapshot directory", proc.stderr)
        self.assertNotIn("newline", proc.stderr)
        # A parent named with a newline, reached through a link, is refused for the newline.
        # Its twin is the sticky directory above it, which the old walk judged instead.
        broken = sticky / "\n"
        broken.mkdir()
        broken.chmod(0o777)
        (sticky / "snapshots").symlink_to(broken)
        proc, calls, _ = self.update_into(sticky / "snapshots" / "run1")
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("contains a newline", proc.stderr)
        self.assertNotIn("above the snapshot directory", proc.stderr)
        self.assertEqual(self.puts(calls), [])
        run1 = broken / "run1"
        self.assertEqual(list(run1.iterdir()) if run1.exists() else [], [], "nothing is written into it")
        # The real directory ends in a newline and a link of the same name without one
        # points at a directory you own: the old code judged and wrote through the link.
        mine = self.tmp / "mine"
        mine.mkdir(mode=0o700)
        base = self.tmp / "base"
        base.mkdir()
        real = base / "x\n"
        real.mkdir(mode=0o700)
        (base / "x").symlink_to(mine)
        proc, calls, _ = self.update_into(real)
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("contains a newline", proc.stderr)
        self.assertEqual(self.puts(calls), [])
        self.assertEqual(list(mine.iterdir()), [], "nothing lands in the twin")
        self.assertEqual(list(real.iterdir()), [], "nor in the real directory")
        # The message does not print the path, which could carry a forged log line.
        for spelling in (str(base), str(base.resolve())):
            self.assertNotIn(spelling, proc.stderr)

    def test_a_find_that_cannot_answer_refuses_the_directory(self):
        # find prints a directory only when its test holds, so a find that fails, or
        # that has nothing to say about an ancestor, leaves nothing and the directory
        # is refused rather than waved through. The shim answers the query named and
        # passes everything else to the real find.
        shim = self.bin / "find"
        snapshots = self.tmp / "snapshots"
        last = "above the snapshot directory"
        for label, body, text, needs_other_owner in (
            ("fails every query", "exit 1", "must be owned by you and not writable by group or others", False),
            ("fails the sticky-bit query", 'case "$*" in *"-perm -1000"*) exit 1 ;; esac', last, False),
            ("fails the root-owner query", 'case "$*" in *"-user 0"*) exit 1 ;; esac', last, True),
            ("finds no ancestor that root owns", 'case "$*" in *"-user 0"*) exit 0 ;; esac', last, True),
        ):
            if needs_other_owner and os.geteuid() == 0:
                continue  # root owns every ancestor, so the owner query is never asked
            with self.subTest(label):
                shutil.rmtree(snapshots, ignore_errors=True)
                shim.write_text(f'#!/bin/sh\n{body}\n' + ('' if body == "exit 1" else 'exec /usr/bin/find "$@"\n'))
                shim.chmod(0o755)
                proc, calls, _ = self.update_into(snapshots)
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn(text, proc.stderr)
                self.assertEqual(self.puts(calls), [])

    def test_a_cd_path_does_not_move_the_snapshot_directory(self):
        # A relative --snapshot-dir plus a CDPATH that happens to hold the same name
        # would send `cd` elsewhere and make it print where. The script is run from
        # the repository root (it hashes the reviewed files relative to it), so the
        # relative directory lives under a throwaway directory there.
        here = Path(tempfile.mkdtemp(prefix="cdpath-", dir=ROOT))
        self.addCleanup(shutil.rmtree, here, ignore_errors=True)
        decoy = self.tmp / "decoy"
        (decoy / here.name / "snapshots").mkdir(parents=True, mode=0o700)
        relative = f"{here.name}/snapshots"
        with unittest.mock.patch.dict(os.environ, {"CDPATH": str(decoy)}):
            proc, calls, _ = self.run_script(
                WORKFLOW_APPLY, ["--update", "--snapshot-dir", relative, "--apply"],
                {**self.ONE, "live": live(V1)})
        self.assertEqual(proc.returncode, 0, proc)
        self.assertEqual(len(list((here / "snapshots").glob("alpha-required-pr-build-123-previous-*.json"))), 1)
        self.assertEqual(list((decoy / here.name / "snapshots").iterdir()), [], "nothing lands in the decoy")

    def test_the_snapshot_directory_is_created_private(self):
        proc, _, _, snapshots = self.update(["--apply"], live=live(V1))
        self.assertEqual(proc.returncode, 0, proc)
        self.assertEqual(stat.S_IMODE(snapshots.stat().st_mode), 0o700)

    def test_the_printed_restore_command_quotes_the_path(self):
        spaced = self.tmp / "snap shots"
        proc, calls, _ = self.run_script(
            WORKFLOW_APPLY, ["--update", "--snapshot-dir", str(spaced), "--apply"],
            {**self.ONE, "live": live(V1), "behave": {"get": {"3": "fail"}}})
        self.assertEqual(proc.returncode, 1, proc)
        # The script prints the real path, so ask the filesystem for it too: a
        # temporary directory is itself under a link on macOS (/var -> /private/var).
        saved = next(spaced.resolve().glob("alpha-required-pr-build-123-previous-*.json"))
        quoted = subprocess.run(["bash", "-c", 'printf "%q" "$1"', "_", str(saved)],
                                capture_output=True, text=True, check=True).stdout
        self.assertNotEqual(quoted, str(saved), "the fixture path must need quoting")
        self.assertIn(f"--input {quoted}", proc.stderr)

    def test_a_ruleset_that_changes_before_the_write_is_not_overwritten(self):
        proc, calls, _, _ = self.update(["--apply"], live=live(V1),
                                        behave={"get": {"2": "drift"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("changed before the write; refusing to overwrite it", proc.stderr)
        self.assertEqual(self.puts(calls), [])

    def test_the_derived_previous_state_is_the_historical_body(self):
        # Same fixture, no shortcut: a live ruleset equal to the historical V1
        # literal must be accepted as "previous", so the derivation is pinned to
        # history rather than to whatever the JSON says today.
        proc, calls, _, _ = self.update(live=live(V1))
        self.assertEqual(proc.returncode, 0, proc)
        self.assertIn("[dry-run]", proc.stdout)


class WorkflowApplierRollback(Harness):
    ONE = {"rulesets": [{"id": 123, "name": V1["name"]}]}

    def test_default_is_a_dry_run(self):
        proc, calls, _ = self.run_script(WORKFLOW_APPLY, ["--rollback"],
                                         {**self.ONE, "live": live(V2)})
        self.assertEqual(proc.returncode, 0, proc)
        self.assertIn("[dry-run]", proc.stdout)
        self.assertEqual(self.puts(calls), [])

    def test_apply_restores_exactly_the_previous_body(self):
        proc, calls, state = self.run_script(WORKFLOW_APPLY, ["--rollback", "--apply"],
                                             {**self.ONE, "live": live(V2)})
        self.assertEqual(proc.returncode, 0, proc)
        self.assertEqual(self.puts(calls), [V1], "only reviewed fields, no server-owned ones")
        self.assertEqual(canon(state), canon(V1))
        self.assertIn("VERIFIED", proc.stdout)

    def test_apply_is_idempotent_when_already_previous(self):
        proc, calls, _ = self.run_script(WORKFLOW_APPLY, ["--rollback", "--apply"],
                                         {**self.ONE, "live": live(V1)})
        self.assertEqual(proc.returncode, 0, proc)
        self.assertEqual(self.puts(calls), [])

    def test_a_drifted_ruleset_is_refused(self):
        body = copy.deepcopy(V2)
        body["bypass_actors"].append({"actor_id": 1, "actor_type": "OrganizationAdmin",
                                      "bypass_mode": "always"})
        for args in (["--rollback", "--apply"], ["--rollback"]):
            with self.subTest(args=args):
                proc, calls, _ = self.run_script(WORKFLOW_APPLY, args, {**self.ONE, "live": live(body)})
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("differs from both the reviewed previous and the desired", proc.stderr)
                self.assertEqual(self.puts(calls), [])

    def test_it_reads_no_protected_source_so_it_works_when_main_moves(self):
        # A rollback must not depend on the local gate files matching protected main.
        # It still needs the reviewed JSON (see the tampered-JSON test), so this is
        # not "from any checkout".
        proc, calls, _ = self.run_script(
            WORKFLOW_APPLY, ["--rollback", "--apply"],
            {**self.ONE, "live": live(V2), "source_stale": [".github/workflows/pr-ci.yml"]})
        self.assertEqual(proc.returncode, 0, proc)
        self.assertNotIn("commit", self.kinds(calls))
        self.assertNotIn("contents", self.kinds(calls))

    def test_a_tampered_json_is_refused_with_the_way_out(self):
        # The previous body is derived from the checked-in JSON, so a checkout whose
        # JSON differs cannot roll back. It must say so, and say what to do instead,
        # before it calls GitHub.
        directory = self.tmp / "copy"
        directory.mkdir()
        shutil.copy(WORKFLOW_APPLY, directory / WORKFLOW_APPLY.name)
        body = json.loads(WORKFLOW_JSON.read_text())
        body["rules"][1]["parameters"]["strict_required_status_checks_policy"] = False
        (directory / WORKFLOW_JSON.name).write_text(json.dumps(body))
        for args in (["--rollback"], ["--rollback", "--apply"]):
            with self.subTest(args=args):
                proc, calls, _ = self.run_script(directory / WORKFLOW_APPLY.name, args,
                                                 {**self.ONE, "live": live(V2)})
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("no longer matches the reviewed", proc.stderr)
                self.assertIn("restore the saved previous body by hand", proc.stderr)
                self.assertIn("alpha-ruleset-hardening.md#roll-back", proc.stderr)
                self.assertEqual(calls, [], "must refuse before reading or writing GitHub")

    def test_the_hint_is_only_for_a_rollback(self):
        directory = self.tmp / "copy"
        directory.mkdir()
        shutil.copy(WORKFLOW_APPLY, directory / WORKFLOW_APPLY.name)
        body = json.loads(WORKFLOW_JSON.read_text())
        body["rules"][1]["parameters"]["strict_required_status_checks_policy"] = False
        (directory / WORKFLOW_JSON.name).write_text(json.dumps(body))
        proc, _, _ = self.run_script(directory / WORKFLOW_APPLY.name,
                                     ["--update", "--snapshot-dir", str(self.tmp / "s")],
                                     {**self.ONE, "live": live(V1)})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertNotIn("by hand", proc.stderr)

    def test_a_restore_that_does_not_verify_is_critical(self):
        proc, calls, _ = self.run_script(
            WORKFLOW_APPLY, ["--rollback", "--apply"],
            {**self.ONE, "live": live(V2), "behave": {"put": {"1": "ignored"}}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("CRITICAL", proc.stderr)

    def test_a_missing_ruleset_is_refused(self):
        proc, calls, _ = self.run_script(WORKFLOW_APPLY, ["--rollback", "--apply"], {"rulesets": []})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("does not exist; nothing to rollback", proc.stderr)
        self.assertEqual(self.puts(calls), [])

    def test_a_failed_write_is_critical_and_changes_nothing(self):
        proc, calls, state = self.run_script(
            WORKFLOW_APPLY, ["--rollback", "--apply"],
            {**self.ONE, "live": live(V2), "behave": {"put": {"1": "fail"}}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("CRITICAL: the write failed; ruleset 123 is unchanged", proc.stderr)
        self.assertIsNone(state, "the failed write applied nothing")
        self.assertEqual(len(self.puts(calls)), 1)

    def test_a_readback_in_neither_state_is_never_overwritten_again(self):
        # An update restores its own mangled write; a rollback has nothing reviewed
        # left to restore, so it reports and stops.
        proc, calls, _ = self.run_script(
            WORKFLOW_APPLY, ["--rollback", "--apply"],
            {**self.ONE, "live": live(V2), "behave": {"put": {"1": "mangled-bypass"}}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertEqual(len(self.puts(calls)), 1)
        self.assertIn("CRITICAL: ruleset 123 is in neither reviewed state and this run will not overwrite it",
                      proc.stderr)
        self.assertNotIn("restore the saved previous body", proc.stderr, "a rollback saves no file")

    def test_a_malformed_live_response_is_refused(self):
        for args in (["--rollback", "--apply"], ["--rollback"]):
            with self.subTest(args=args):
                proc, calls, _ = self.run_script(
                    WORKFLOW_APPLY, args,
                    {**self.ONE, "live": live(V2), "behave": {"get": {"1": "garbage"}}})
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("response is malformed", proc.stderr)
                self.assertEqual(self.puts(calls), [])

    def test_a_failed_re_read_before_the_write_is_refused(self):
        proc, calls, _ = self.run_script(
            WORKFLOW_APPLY, ["--rollback", "--apply"],
            {**self.ONE, "live": live(V2), "behave": {"get": {"2": "fail"}}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("cannot re-read ruleset 123 before writing", proc.stderr)
        self.assertEqual(self.puts(calls), [])

    def test_it_refuses_while_a_tag_named_main_exists(self):
        # The rollback puts the short ref `main` back. With a tag of that name the
        # short ref is ambiguous again, which is what the qualified ref prevented.
        for args in (["--rollback"], ["--rollback", "--apply"]):
            with self.subTest(args=args):
                proc, calls, _ = self.run_script(
                    WORKFLOW_APPLY, args,
                    {**self.ONE, "live": live(V2), "tags": ["refs/tags/main"]})
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("a tag named main exists", proc.stderr)
                self.assertEqual(self.kinds(calls), ["tags"], "nothing else is read or written")

    def test_a_tag_on_a_later_page_also_refuses(self):
        proc, calls, _ = self.run_script(
            WORKFLOW_APPLY, ["--rollback", "--apply"],
            {**self.ONE, "live": live(V2), "tags_pages": [["refs/tags/a"], ["refs/tags/main"]]})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertEqual(self.puts(calls), [])

    def test_it_fails_closed_when_the_tag_check_cannot_answer(self):
        for how in ("fail", "empty"):
            with self.subTest(how):
                proc, calls, _ = self.run_script(
                    WORKFLOW_APPLY, ["--rollback", "--apply"],
                    {**self.ONE, "live": live(V2), "behave": {"tags": {"1": how}}})
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("cannot check for an existing tag named main", proc.stderr)
                self.assertEqual(self.kinds(calls), ["tags"])

    def test_tags_that_only_start_with_main_do_not_refuse(self):
        proc, calls, state = self.run_script(
            WORKFLOW_APPLY, ["--rollback", "--apply"],
            {**self.ONE, "live": live(V2), "tags": ["refs/tags/main-1", "refs/tags/mainline"]})
        self.assertEqual(proc.returncode, 0, proc)
        self.assertEqual(canon(state), canon(V1))

    def test_an_update_is_not_held_back_by_a_tag_named_main(self):
        # The update moves toward the qualified ref, which is safe with such a tag
        # around. Only the rollback, which restores the short ref, looks for it.
        snapshots = self.tmp / "snapshots"
        proc, calls, _ = self.run_script(
            WORKFLOW_APPLY, ["--update", "--snapshot-dir", str(snapshots), "--apply"],
            {**self.ONE, "live": live(V1), "tags": ["refs/tags/main"]})
        self.assertEqual(proc.returncode, 0, proc)
        self.assertNotIn("tags", self.kinds(calls))


class TagApplier(CreateRollbackIsAttributed, Harness):
    ONE = {"rulesets": [{"id": 123, "name": TAG["name"]}]}

    def apply(self, args=("--apply",), **cfg):
        return self.run_script(TAG_APPLY, list(args), {"rulesets": [], **cfg})

    def mutations(self, calls):
        return [c for c in self.kinds(calls) if c in ("post", "put", "delete")]

    def test_dry_run_creates_nothing(self):
        proc, calls, _ = self.apply(["--dry-run"])
        self.assertEqual(proc.returncode, 0, proc)
        self.assertIn("[dry-run]", proc.stdout)
        self.assertEqual(self.mutations(calls), [])

    def test_default_is_a_dry_run(self):
        proc, calls, _ = self.apply([])
        self.assertEqual(proc.returncode, 0, proc)
        self.assertEqual(self.mutations(calls), [])

    def test_apply_creates_disabled_then_activates(self):
        proc, calls, state = self.apply()
        self.assertEqual(proc.returncode, 0, proc)
        kinds = self.kinds(calls)
        self.assertLess(kinds.index("post"), kinds.index("put"))
        post = next(c["payload"] for c in calls if c["call"] == "post")
        self.assertEqual(post, {**TAG, "enforcement": "disabled"})
        self.assertEqual(self.puts(calls), [TAG])
        self.assertEqual(state["enforcement"], "active")
        self.assertEqual(kinds.count("get"), 3, "create readback, re-read, active readback")
        self.assertIn("VERIFIED", proc.stdout)

    def test_matching_ruleset_is_a_no_op(self):
        proc, calls, _ = self.apply(rulesets=self.ONE["rulesets"], live=live(TAG))
        self.assertEqual(proc.returncode, 0, proc)
        self.assertIn("already matches desired state", proc.stdout)
        self.assertEqual(self.mutations(calls), [])

    def test_server_adding_an_empty_parameters_object_still_matches(self):
        body = live(TAG)
        body["rules"] = [{"type": "creation", "parameters": {}}]
        proc, calls, _ = self.apply(rulesets=self.ONE["rulesets"], live=body)
        self.assertEqual(proc.returncode, 0, proc)
        self.assertEqual(self.mutations(calls), [])

    def test_drifted_ruleset_is_refused(self):
        for label, mutate in (
            ("disabled", lambda b: b.update(enforcement="disabled")),
            ("bypass actor", lambda b: b["bypass_actors"].append(
                {"actor_id": 3967879, "actor_type": "Integration", "bypass_mode": "always"})),
            ("other tag", lambda b: b["conditions"]["ref_name"].update(include=["refs/tags/other"])),
        ):
            with self.subTest(label):
                body = copy.deepcopy(TAG)
                mutate(body)
                proc, calls, _ = self.apply(rulesets=self.ONE["rulesets"], live=live(body))
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("differs from desired state; refusing replacement", proc.stderr)
                self.assertEqual(self.mutations(calls), [])

    def test_duplicate_rulesets_are_refused(self):
        proc, calls, _ = self.apply(rulesets=self.ONE["rulesets"] + [{"id": 124, "name": TAG["name"]}])
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("duplicate Alpha Reserved Tag Names rulesets", proc.stderr)
        self.assertEqual(self.mutations(calls), [])

    def test_the_list_is_read_across_pages(self):
        pages = [[{"id": 5, "name": "Other"}], [{"id": 123, "name": TAG["name"]}]]
        proc, calls, _ = self.apply(rulesets_pages=pages, live=live(TAG))
        self.assertEqual(proc.returncode, 0, proc)
        self.assertIn("already matches desired state", proc.stdout)

    def test_the_same_ruleset_on_two_pages_is_not_a_duplicate(self):
        pages = [[{"id": 123, "name": TAG["name"]}], [{"id": 123, "name": TAG["name"]}]]
        proc, calls, _ = self.apply(rulesets_pages=pages, live=live(TAG))
        self.assertEqual(proc.returncode, 0, proc)

    def test_two_rulesets_on_two_pages_are_duplicates(self):
        pages = [[{"id": 123, "name": TAG["name"]}], [{"id": 124, "name": TAG["name"]}]]
        proc, calls, _ = self.apply(rulesets_pages=pages, live=live(TAG))
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("duplicate Alpha Reserved Tag Names rulesets", proc.stderr)

    def test_a_ruleset_id_that_is_not_a_positive_integer_is_refused(self):
        for bad in ("abc", 0, -5, "01"):
            with self.subTest(id=bad):
                proc, calls, _ = self.apply(rulesets=[{"id": bad, "name": TAG["name"]}], live=live(TAG))
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("invalid ruleset ID", proc.stderr)
                self.assertEqual(self.mutations(calls), [])

    def test_a_malformed_or_unreadable_existing_ruleset_is_refused(self):
        for how, text in (("garbage", "ruleset 123 response is malformed"), ("fail", "cannot read ruleset 123")):
            with self.subTest(how):
                proc, calls, _ = self.apply(rulesets=self.ONE["rulesets"], live=live(TAG),
                                            behave={"get": {"1": how}})
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn(text, proc.stderr)
                self.assertEqual(self.mutations(calls), [])

    def test_a_malformed_ruleset_list_is_refused(self):
        proc, calls, _ = self.apply(behave={"list": {"1": "garbage"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("the organization ruleset list is malformed", proc.stderr)
        self.assertEqual(self.mutations(calls), [])

    def test_an_existing_tag_named_main_blocks_even_a_dry_run(self):
        for args in (["--dry-run"], ["--apply"]):
            with self.subTest(args=args):
                proc, calls, _ = self.apply(args, tags=["refs/tags/main"])
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("a tag named main already exists", proc.stderr)
                self.assertEqual(self.mutations(calls), [])

    def test_a_tag_that_merely_starts_with_main_does_not_block(self):
        proc, calls, _ = self.apply(tags=["refs/tags/main-1", "refs/tags/mainline"])
        self.assertEqual(proc.returncode, 0, proc)

    def test_an_unverifiable_tag_check_blocks(self):
        # An error and an empty answer both prove nothing about the tag.
        for how in ("fail", "empty"):
            with self.subTest(how):
                proc, calls, _ = self.apply(behave={"tags": {"1": how}})
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("cannot check for an existing tag named main", proc.stderr)
                self.assertEqual(self.mutations(calls), [])

    def test_a_tag_on_a_later_page_blocks(self):
        # --paginate prints one answer per page; a tag on page two is still a tag.
        proc, calls, _ = self.apply(tags_pages=[["refs/tags/a"], ["refs/tags/main"]])
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("a tag named main already exists", proc.stderr)
        self.assertEqual(self.mutations(calls), [])

    def test_no_tag_on_any_page_is_free(self):
        proc, calls, _ = self.apply(tags_pages=[["refs/tags/a"], ["refs/tags/main-2"]])
        self.assertEqual(proc.returncode, 0, proc)
        self.assertIn("VERIFIED", proc.stdout)

    def test_the_name_is_checked_again_after_activation(self):
        proc, calls, _ = self.apply()
        self.assertEqual(proc.returncode, 0, proc)
        kinds = self.kinds(calls)
        self.assertEqual(kinds.count("tags"), 2, "once before the create, once after activation")
        self.assertEqual(kinds[-1], "tags", "the last thing the script learns is that the name is free")

    def test_a_tag_that_appears_after_activation_is_critical(self):
        # call 1 (before the create) sees no tag; call 2 (after activation) sees one.
        proc, calls, state = self.apply(tags_seq=[[], ["refs/tags/main"]])
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("CRITICAL: ruleset 123 is active, but a tag named main exists", proc.stderr)
        self.assertNotIn("VERIFIED", proc.stdout)
        self.assertEqual(state["enforcement"], "active", "the ruleset is left in place")
        self.assertEqual(self.mutations(calls).count("delete"), 0)
        self.assertEqual(self.puts(calls), [TAG], "only the activation: nothing is rolled back")

    def test_an_unverifiable_tag_check_after_activation_is_critical(self):
        for how in ("fail", "empty"):
            with self.subTest(how):
                proc, calls, state = self.apply(behave={"tags": {"2": how}})
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("CRITICAL: ruleset 123 is active, but whether a tag named main exists could not be checked",
                              proc.stderr)
                self.assertNotIn("VERIFIED", proc.stdout)
                self.assertEqual(state["enforcement"], "active")
                self.assertEqual(self.mutations(calls).count("delete"), 0)

    def test_an_unreadable_ruleset_list_blocks(self):
        proc, calls, _ = self.apply(behave={"list": {"1": "fail"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("cannot read organization rulesets", proc.stderr)
        self.assertEqual(self.mutations(calls), [])

    def test_failures_after_create_remove_the_new_ruleset(self):
        for label, behave, reason in (
            ("activation write fails", {"put": {"1": "fail"}}, "activation request failed"),
            ("readback after create unreadable", {"get": {"1": "fail"}}, "failed to read disabled ruleset"),
            ("readback after create malformed", {"get": {"1": "garbage"}}, "disabled ruleset readback is malformed"),
            ("readback after create differs", {"get": {"1": "drift"}}, "disabled ruleset readback differs"),
            ("re-read before activation unreadable", {"get": {"2": "fail"}}, "failed to re-read disabled ruleset"),
            ("re-read before activation malformed", {"get": {"2": "garbage"}}, "disabled ruleset re-read is malformed"),
            ("re-read before activation drifted", {"get": {"2": "drift"}}, "disabled ruleset changed before activation"),
            ("activation accepted but not applied", {"put": {"1": "ignored"}}, "active ruleset readback differs"),
            ("active readback unreadable", {"get": {"3": "fail"}}, "failed to read active ruleset"),
            ("active readback malformed", {"get": {"3": "garbage"}}, "active ruleset readback is malformed"),
        ):
            with self.subTest(label):
                proc, calls, state = self.apply(behave=behave)
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn(reason, proc.stderr)
                self.assertIn("ROLLBACK: deleted new ruleset 123", proc.stderr)
                self.assertIn("delete", self.kinds(calls))
                self.assertIsNone(state, "the new ruleset is gone")
                self.assertNotIn("CRITICAL", proc.stderr)

    def test_a_ruleset_that_cannot_be_removed_falls_back_to_disabling_it(self):
        proc, calls, state = self.apply(behave={"put": {"1": "fail"}, "delete": {"1": "fail"}})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertEqual(state["enforcement"], "disabled")
        self.assertIn("ROLLBACK: ruleset 123 is disabled", proc.stderr)
        self.assertNotIn("CRITICAL", proc.stderr)

    def test_a_rollback_that_cannot_be_verified_is_critical(self):
        # Activation reports failure but landed (the rule is ACTIVE) and the delete
        # fails; then the disabling write either fails or is accepted and ignored.
        for how in ("fail", "ignored"):
            with self.subTest(how):
                proc, calls, state = self.apply(
                    behave={"put": {"1": "fail-applied", "2": how}, "delete": {"1": "fail"}})
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn("CRITICAL: rollback of ruleset 123 could not be verified", proc.stderr)
                self.assertIn("settings/rules/123", proc.stderr, "names the rule to clean up by hand")

    def test_an_unconfirmed_create_is_critical(self):
        for how, text in (("fail", "create request failed and its outcome is unknown"),
                          ("noid", "create returned no ruleset ID")):
            with self.subTest(how):
                proc, calls, _ = self.apply(behave={"post": {"1": how}})
                self.assertEqual(proc.returncode, 1, proc)
                self.assertIn(f"CRITICAL: {text}", proc.stderr)
                self.assertNotIn("delete", self.kinds(calls), "nothing is known to delete")

    def test_usage_errors_exit_two(self):
        for args in (["--bogus"], ["--apply", "--dry-run"], ["--update"]):
            with self.subTest(args=args):
                proc, calls, _ = self.apply(args)
                self.assertEqual(proc.returncode, 2, proc)
                self.assertEqual(calls, [])

    def test_a_tampered_json_is_refused(self):
        directory = self.tmp / "copy"
        directory.mkdir()
        shutil.copy(TAG_APPLY, directory / TAG_APPLY.name)
        body = json.loads(TAG_JSON.read_text())
        body["bypass_actors"] = [{"actor_id": 1, "actor_type": "OrganizationAdmin",
                                  "bypass_mode": "always"}]
        (directory / TAG_JSON.name).write_text(json.dumps(body))
        proc, calls, _ = self.run_script(directory / TAG_APPLY.name, ["--dry-run"], {"rulesets": []})
        self.assertEqual(proc.returncode, 1, proc)
        self.assertIn("no longer matches the reviewed", proc.stderr)
        self.assertEqual(calls, [])


class ProtectedOwnerStep(unittest.TestCase):
    """pr-ci.yml decides from the effective main rules whether the protected
    workflow owns the scanner DAG. It must accept either spelling of main, and
    nothing that is not the main branch or that pins the workflow to a commit."""

    UNSET = object()  # a `sha` key that is absent, as GitHub's response has it when none is set

    @classmethod
    def setUpClass(cls):
        pr_ci = yaml.safe_load((ROOT / ".github/workflows/pr-ci.yml").read_text())
        step = next(s for s in pr_ci["jobs"]["detect-changes"]["steps"]
                    if s.get("id") == "protected-owner")
        cls.script = step["run"]

    def confirmed(self, ref, path=PATH_IN_REPO, repository_id=1158152129, sha=UNSET):
        with tempfile.TemporaryDirectory(prefix="protected-owner-") as temporary:
            tmp = Path(temporary)
            shim = tmp / "gh"
            shim.write_text('#!/bin/sh\ncat "$RULES_FIXTURE"\n')
            shim.chmod(0o755)
            workflow = {"repository_id": repository_id, "path": path}
            if ref is not None:
                workflow["ref"] = ref
            if sha is not self.UNSET:
                workflow["sha"] = sha
            fixture = tmp / "rules.json"
            fixture.write_text(json.dumps([{"type": "workflows", "ruleset_id": 123,
                                            "parameters": {"workflows": [workflow]}}]))
            output = tmp / "output"
            env = {**os.environ, "MODE": "required-full",
                   "SELECTION": json.dumps({"known": True, "hotfix": False, "mode": "required-full"}),
                   "RULES_FIXTURE": str(fixture), "GITHUB_OUTPUT": str(output),
                   "RUNNER_TEMP": str(tmp), "GITHUB_REPOSITORY": "Concord-Voice/Concord-Voice-Alpha",
                   "GH_TOKEN": "unused", "PATH": f"{tmp}:{os.environ['PATH']}"}
            run = subprocess.run(["bash", "-c", self.script], env=env, capture_output=True, text=True)
            self.assertEqual(run.returncode, 0, run.stderr)
            return output.read_text().splitlines()[-1] == "confirmed=true"

    def test_the_short_branch_name_is_accepted(self):
        self.assertTrue(self.confirmed("main"))

    def test_the_full_branch_name_is_accepted(self):
        self.assertTrue(self.confirmed("refs/heads/main"))

    def test_a_tag_is_not_the_main_branch(self):
        self.assertFalse(self.confirmed("refs/tags/main"))

    def test_other_refs_are_rejected(self):
        for ref in ("heads/main", "refs/heads/release", "master", "", None):
            with self.subTest(ref=ref):
                self.assertFalse(self.confirmed(ref))

    def test_a_different_workflow_or_repository_is_rejected(self):
        self.assertFalse(self.confirmed("refs/heads/main", path=".github/workflows/other.yml"))
        self.assertFalse(self.confirmed("refs/heads/main", repository_id=1))

    def test_an_entry_pinned_to_a_commit_is_not_confirmed(self):
        # GitHub: `sha` is "the commit SHA of the workflow file to use"; how it combines
        # with `ref` is not documented, so a pinned entry does not prove that main's copy
        # runs. An empty value is not "no pin" either.
        for ref in ("main", "refs/heads/main"):
            for sha in ("0" * 40, ""):
                with self.subTest(ref=ref, sha=sha):
                    self.assertFalse(self.confirmed(ref, sha=sha))

    def test_an_entry_with_no_pin_is_confirmed_whether_sha_is_null_or_absent(self):
        # The live effective-rules response (2026-10-14) has no `sha` key at all.
        for ref in ("main", "refs/heads/main"):
            with self.subTest(ref=ref):
                self.assertTrue(self.confirmed(ref))
                self.assertTrue(self.confirmed(ref, sha=None))


if __name__ == "__main__":
    unittest.main()
