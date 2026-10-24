import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / ".github/scripts/verify-required-build-pins.py"
# A hook's GIT_DIR would point the fixture's `git init` at this repository
# ([internal]rules/git-hooks.md), so the fixture git never sees git's local env.
ENV = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
OURS = "Concord-Voice/Self-Hosted/.github/workflows"
CONTRACT = """\
on:
  workflow_call:
    inputs:
      mode:
        type: string
        required: true
      flag:
        type: boolean
        required: true
        default: false
      count:
        type: number
        required: false
    secrets:
      TOKEN:
        required: true
    outputs:
      selection:
        value: x
jobs:
  a:
    permissions:
      contents: read
    runs-on: x
    steps: []
  nested:
    uses: ./.github/workflows/inner.yml
"""
# x inherits the workflow-level permissions; y declares its own.
INNER = """\
on: workflow_call
permissions:
  statuses: write
jobs:
  x:
    runs-on: x
    steps: []
  y:
    permissions:
      security-events: write
    runs-on: x
    steps: []
"""
# chain0.yml calls chain1.yml ... chain8.yml: pinned at chain0 the leaf is the
# eleventh level of workflows, pinned at chain1 the tenth.
CHAIN = {f"chain{i}.yml": "on: workflow_call\njobs:\n  n:\n    uses: ./.github/workflows/"
         f"chain{i + 1}.yml\n" for i in range(9)}
CHAIN["chain9.yml"] = "on: workflow_call\njobs:\n  n:\n    runs-on: x\n    steps: []\n"
LEAF = "on: workflow_call\njobs:\n  x:\n    permissions:\n      contents: read\n    runs-on: x\n    steps: []\n"
# GitHub judges a workflow-level block even when every job replaces it.
TOP_OVER = "on: workflow_call\npermissions:\n  contents: write\n" + LEAF.split("\n", 1)[1]
QUALITY = LEAF.replace("contents: read", "code-quality: read")
# Anchors read the same to PyYAML and GitHub, so a graph file may use them. The
# shared aliases in `env` expand to 2**40 mappings, so each walk must visit a node once.
ANCHORED = ("on: workflow_call\njobs:\n  a: &job\n    permissions: {contents: read}\n    runs-on: x\n"
            "    steps: []\n    env:\n      l0: &l0 {p: x, q: x}\n"
            + "".join(f"      l{i}: &l{i} {{p: *l{i - 1}, q: *l{i - 1}}}\n" for i in range(1, 40))
            + "  b: *job\n  d:\n    permissions: {contents: read}\n    uses: ./.github/workflows/leaf.yml\n")
DUPLICATE = LEAF.replace("    runs-on: x\n", "    runs-on: x\n    runs-on: y\n")


def calls(*targets, grants=None):
    """A reusable workflow whose job i calls targets[i], with grants[i] as its permissions block."""
    jobs = ""
    for i, target in enumerate(targets):
        grant = (grants or [None] * len(targets))[i]
        jobs += f"  j{i}:\n" + (f"    permissions: {grant}\n" if grant is not None else "") + f"    uses: {target}\n"
    return "on: workflow_call\njobs:\n" + jobs


# Fifty leaves: wide.yml and its leaves are 51 unique workflows, wide_ok.yml and its 49 are 50.
WIDE = {f"w{i}.yml": LEAF for i in range(50)}
WIDE["wide.yml"] = calls(*(f"./.github/workflows/w{i}.yml" for i in range(50)))
WIDE["wide_ok.yml"] = calls(*(f"./.github/workflows/w{i}.yml" for i in range(49)))
# Another repository's workflow counts too: 1 + 49 + 1.
WIDE["wide_ext.yml"] = calls(*(f"./.github/workflows/w{i}.yml" for i in range(49)),
                             f"google/osv-scanner-action/.github/workflows/x.yml@{'a' * 40}")


def steps(*actions):
    """A reusable workflow whose one job runs `actions` as steps."""
    return LEAF.replace("    steps: []\n", "    steps:\n" + "".join(f"      - uses: {a}\n" for a in actions))


def git(repo, *args):
    return subprocess.run(
        ["git", "-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.invalid",
         "-c", "commit.gpgsign=false", *args],
        env=ENV, check=True, capture_output=True, text=True,
    ).stdout.strip()


class VerifyRequiredBuildPins(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.repo = os.path.join(cls.tmp.name, "repo")
        subprocess.run(["git", "init", "-q", "-b", "main", cls.repo], env=ENV, check=True)
        # github.com/actions/checkout, served from a local repository. GitHub resolves
        # a fork's commit under the parent's name, so `fork` is a commit the server
        # holds but no branch or tag reaches.
        upstream = os.path.join(cls.tmp.name, "upstreams", "actions", "checkout")
        subprocess.run(["git", "init", "-q", "-b", "main", upstream], env=ENV, check=True)
        git(upstream, "commit", "-q", "--allow-empty", "-m", "on main")
        cls.checkout = f"actions/checkout@{git(upstream, 'rev-parse', 'HEAD')}"
        git(upstream, "switch", "-q", "--detach")
        git(upstream, "commit", "-q", "--allow-empty", "-m", "tagged")
        git(upstream, "tag", "v2")
        cls.tagged = git(upstream, "rev-parse", "HEAD")
        git(upstream, "commit", "-q", "--allow-empty", "-m", "fork")
        cls.fork = git(upstream, "rev-parse", "HEAD")
        git(upstream, "switch", "-q", "main")
        cls.env = {**ENV, "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_VALUE_0": "https://github.com/",
                   "GIT_CONFIG_KEY_0": f"url.file://{cls.tmp.name}/upstreams/.insteadOf"}
        git(cls.repo, "commit", "-q", "--allow-empty", "-m", "before the workflows existed")
        cls.missing = git(cls.repo, "rev-parse", "HEAD")
        cls.commit_workflows("reviewed")
        cls.reviewed = git(cls.repo, "rev-parse", "HEAD")
        git(cls.repo, "switch", "-q", "-c", "side")
        cls.commit_workflows("branch only")
        cls.branch_only = git(cls.repo, "rev-parse", "HEAD")
        git(cls.repo, "switch", "-q", "main")
        cls.commit_workflows("later")
        cls.later = git(cls.repo, "rev-parse", "HEAD")
        cls.commit_files("contract", {
            "contract.yml": CONTRACT, "inner.yml": INNER, "push.yml": "on: push\njobs: {}\n", **CHAIN, **WIDE,
            "leaf.yml": LEAF,
            # The second caller grants less than the first; each call must fit its own grant.
            "diamond.yml": calls("./.github/workflows/leaf.yml", "./.github/workflows/leaf.yml",
                                 grants=["{contents: read}", "{}"]),
            "diamond_ok.yml": calls("./.github/workflows/leaf.yml", "./.github/workflows/leaf.yml",
                                    grants=["{contents: read}", "{contents: read}"]),
            "cycle.yml": calls("./.github/workflows/cycle.yml"),
            # chain3 is walked first at level 3, then reached again at level 6 through chain0.
            "mask.yml": calls("./.github/workflows/chain3.yml", "./.github/workflows/chain0.yml"),
            "nest_branch.yml": calls(f"{OURS}/pr-ci.yml@{cls.branch_only}"),
            "nest_mutable.yml": calls(f"{OURS}/pr-ci.yml@main"),
            "nest_stranger.yml": calls(f"someone/repo/.github/workflows/x.yml@{'0' * 40}"),
            "nest_google.yml": calls(f"google/osv-scanner-action/.github/workflows/x.yml@{'a' * 40}"),
            "nest_path.yml": calls(f"{OURS}/sub/leaf.yml@{cls.later}"),
            "nest_contract.yml": calls("./.github/workflows/contract.yml"),
            "nest_push.yml": calls("./.github/workflows/push.yml"),
            "top_over.yml": TOP_OVER, "quality.yml": QUALITY, "anchored.yml": ANCHORED, "duplicate.yml": DUPLICATE,
            # Codex r1: a reviewed graph file's step still runs whatever a mutable ref
            # points at later.
            "step_mutable.yml": steps("someone/action@main"),
            "step_docker.yml": steps("docker://alpine:3"),
            "step_branch.yml": steps(f"Concord-Voice/Self-Hosted/.github/actions/x@{cls.branch_only}"),
            "step_local.yml": steps("./.github/actions/x"),
            "step_ok.yml": steps(f"dorny/paths-filter@{'c' * 40}",
                                 f"docker://alpine@sha256:{'d' * 64}",
                                 f"Concord-Voice/Self-Hosted/.github/actions/x@{cls.reviewed}"),
            # Review r5 H1: libyaml skips a BOM at the start of a line, so it reads this
            # `uses` inside the job, while PyYAML's pure-Python reader puts it at the top.
            "bom_hide.yml": "on: workflow_call\njobs:\n  shadow:\n    permissions: {contents: read}\n"
                            f"{chr(0xFEFF)}   uses: {OURS}/pr-ci.yml@{cls.branch_only}\n",
        })
        cls.contract = git(cls.repo, "rev-parse", "HEAD")
        cls.commit_files("pinned nest", {
            "pinnest.yml": f"on: workflow_call\njobs:\n  p:\n    uses: {OURS}/inner.yml@{cls.contract}\n",
            "pinnest_contract.yml": calls(f"{OURS}/contract.yml@{cls.contract}")})
        cls.pinnest = git(cls.repo, "rev-parse", "HEAD")
        # A branch merged with a merge commit: its commits are ancestors of main
        # that main never was.
        git(cls.repo, "switch", "-q", "-c", "merged")
        cls.commit_workflows("merged side")
        cls.merged_side = git(cls.repo, "rev-parse", "HEAD")
        git(cls.repo, "switch", "-q", "main")
        git(cls.repo, "merge", "-q", "--no-ff", "-m", "merge side", "merged")

    @classmethod
    def commit_workflows(cls, label):
        cls.commit_files(label, {name: f"# {label}\non: workflow_call\n" for name in ("pr-ci.yml", "osv-scanner.yml")})

    @classmethod
    def commit_files(cls, label, files):
        workflows = Path(cls.repo, ".github/workflows")
        workflows.mkdir(parents=True, exist_ok=True)
        for name, text in files.items():
            (workflows / name).write_text(text)
        git(cls.repo, "add", ".github")
        git(cls.repo, "commit", "-q", "-m", label)

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def run_check(self, text, main_ref="main", repo=None, base=None):
        path = os.path.join(self.tmp.name, "required-pr-build.yml")
        Path(path).write_text(text, encoding="utf-8")
        base_path = os.path.join(self.tmp.name, "base-required-pr-build.yml")
        Path(base_path).write_text(self.workflow(self.reviewed) if base is None else base, encoding="utf-8")
        return subprocess.run(
            [sys.executable, str(SCRIPT), "--repo", repo or self.repo,
             "--main-ref", main_ref, "--workflow", path, "--base-workflow", base_path],
            env=self.env, capture_output=True, text=True, timeout=120,
        )

    def workflow(self, ci_ref, osv_ref=None, owner_path=OURS):
        return (
            "permissions:\n  contents: read\n"
            f"jobs:\n  pr-ci:\n    uses: {owner_path}/pr-ci.yml@{ci_ref}\n"
            f"  osv:\n    uses: {owner_path}/osv-scanner.yml@{osv_ref or ci_ref}\n"
            f"  result:\n    steps:\n      - uses: {self.checkout} # v7.0.1\n"
        )

    def assert_blocked(self, run, needle):
        self.assertEqual(run.returncode, 1, run)
        self.assertIn("::error::", run.stdout)
        self.assertIn(needle, run.stdout)

    def test_pins_on_main_pass(self):
        run = self.run_check(self.workflow(self.reviewed))
        self.assertEqual(run.returncode, 0, run)
        self.assertIn("::notice::", run.stdout)

    def test_forward_pin_passes(self):
        self.assertEqual(self.run_check(self.workflow(self.later)).returncode, 0)

    def test_pin_moving_back_from_the_base_fails(self):
        run = self.run_check(self.workflow(self.reviewed), base=self.workflow(self.later))
        self.assert_blocked(run, "moves back from the base's pin")

    def test_pin_to_a_main_commit_without_the_file_fails(self):
        # Review D1: it passes ancestry, then cannot load once merged.
        run = self.run_check(self.workflow(self.missing), base=self.workflow(self.missing))
        self.assert_blocked(run, "names a file that is missing or not a YAML mapping at that commit")

    def test_comment_lines_naming_uses_are_ignored(self):
        text = "# uses: anything@main\n" + self.workflow(self.reviewed)
        self.assertEqual(self.run_check(text).returncode, 0)

    def test_branch_only_pin_fails(self):
        self.assert_blocked(self.run_check(self.workflow(self.reviewed, self.branch_only)),
                            "is not a commit on main")

    def test_unknown_sha_fails(self):
        self.assert_blocked(self.run_check(self.workflow("e" * 40)), "is not a commit on main")

    def test_owner_case_does_not_bypass(self):
        run = self.run_check(self.workflow(
            self.branch_only, owner_path="concord-voice/SELF-HOSTED/.github/workflows"))
        self.assert_blocked(run, "is not a commit on main")

    def test_mutable_ref_fails(self):
        self.assert_blocked(self.run_check(self.workflow("main")), "names `uses` but is not")

    def test_text_after_a_reviewed_sha_fails(self):
        # Review H1: each suffix leaves a ref git accepts as a branch name.
        for suffix in ("#main", ",x", "}", "'x", "\u00a0", "\u200ax"):
            with self.subTest(suffix=repr(suffix)):
                self.assert_blocked(self.run_check(self.workflow(self.reviewed + suffix, self.reviewed)),
                                    "names `uses` but is not")

    def test_yaml11_only_line_breaks_fail(self):
        # YAML 1.1 (PyYAML) splits a line at these; YAML 1.2 (GitHub) keeps them
        # in the value, so `<sha><U+0085>` would read as a branch name there.
        for char in ("\u0085", "\u2028", "\u2029"):
            with self.subTest(char=repr(char)):
                text = self.workflow(self.reviewed).replace(
                    "osv-scanner.yml@" + self.reviewed, f"osv-scanner.yml@{self.reviewed}{char}    # c")
                self.assert_blocked(self.run_check(text), "YAML 1.1-only line break")

    def test_noncanonical_forms_fail_closed(self):
        for text in (
            f'jobs:\n  pr-ci: {{uses: "{OURS}/pr-ci.yml@{self.reviewed}"}}\n',
            f'jobs:\n  pr-ci:\n    "uses": {OURS}/pr-ci.yml@{self.reviewed}\n',
            f"jobs:\n  pr-ci:\n    uses:\n      {OURS}/pr-ci.yml@{self.reviewed}\n",
            f"jobs:\n  pr-ci:\n    uses: \"{OURS}/pr-ci.yml@{self.reviewed}\n",
        ):
            with self.subTest(text=text):
                self.assertEqual(self.run_check(text).returncode, 1)

    def test_keys_only_the_parser_decodes_fail(self):
        # Review M1: one canonical pin satisfies the line rule while the second
        # `uses` is spelled so that only a YAML parser sees it.
        canonical = f"jobs:\n  osv:\n    uses: {OURS}/osv-scanner.yml@{self.reviewed}\n"
        for hidden, needle in (
            (f'  pr-ci:\n    "u\\x73es": {OURS}/pr-ci.yml@main\n', "disagree"),
            # The alias is refused before the readers compare.
            (f"  pr-ci:\n    env: {{K: &k uses}}\n    *k : {OURS}/pr-ci.yml@main\n", "anchor or alias"),
            (f'  pr-ci:\n    ? "us\\\n      es"\n    : {OURS}/pr-ci.yml@main\n', "disagree"),
        ):
            with self.subTest(hidden=hidden):
                self.assert_blocked(self.run_check(canonical + hidden), needle)

    def test_yaml_features_where_parsers_diverge_fail(self):
        osv = f"  osv:\n    uses: {OURS}/osv-scanner.yml@{self.reviewed}\n"
        for hidden, needle in (
            # Review S1: PyYAML keeps the visible copy, so both readers agree on it.
            (f'  pr-ci:\n    "u\\x73es": {OURS}/pr-ci.yml@{self.branch_only}\n'
             f"    uses: {OURS}/pr-ci.yml@{self.reviewed}\n", "repeats a mapping key"),
            # Review S2: PyYAML reads a bytes key; other parsers read `uses`.
            (f"  pr-ci:\n    ? !!binary dXNlcw==\n    : {OURS}/pr-ci.yml@{self.branch_only}\n",
             "explicit YAML tag"),
            # Review S3: an invalid workflow once merged.
            (f'  pr-ci:\n    "U\\x73es": {OURS}/pr-ci.yml@{self.reviewed}\n', "only case-folds"),
            (f"  pr-ci:\n    <<: {{runs-on: x}}\n", "merge key"),
            (f"  pr-ci:\n    runs-on: &r x\n", "anchor or alias"),
        ):
            with self.subTest(needle=needle):
                self.assert_blocked(self.run_check("jobs:\n" + osv + hidden), needle)

    def test_duplicate_uses_key_fails(self):
        text = (f"jobs:\n  pr-ci:\n    uses: {OURS}/pr-ci.yml@{self.branch_only}\n"
                f"    uses: {OURS}/pr-ci.yml@{self.reviewed}\n")
        self.assertEqual(self.run_check(text).returncode, 1)

    def test_reusable_workflow_from_another_repository_fails(self):
        for owner_path in ("attacker/Self-Hosted/.github/workflows",
                           "Concord-Voice/Self-Hosted.git/.github/workflows"):
            with self.subTest(owner_path=owner_path):
                text = (f"jobs:\n  osv:\n    uses: {OURS}/osv-scanner.yml@{self.reviewed}\n"
                        f"  pr-ci:\n    uses: {owner_path}/pr-ci.yml@{'0' * 40}\n")
                self.assert_blocked(self.run_check(text), "outside this repository")

    def test_path_spellings_fail(self):
        # One file, one spelling: otherwise the rollback rule keys a moved pin
        # under a path the base never had, and GitHub may refuse the path.
        for path in ("/.github/workflows/./pr-ci.yml", "/.github//workflows/pr-ci.yml",
                     "/.github/workflows/../workflows/pr-ci.yml"):
            with self.subTest(path=path):
                text = self.workflow(self.reviewed).replace("/.github/workflows/pr-ci.yml", path)
                self.assert_blocked(self.run_check(text), "names `uses` but is not")

    def test_internal_pin_must_be_a_top_level_workflow(self):
        for value in (f"Concord-Voice/Self-Hosted/some-action@{self.reviewed}",
                      f"{OURS}/nested/pr-ci.yml@{self.reviewed}"):
            with self.subTest(value=value):
                text = self.workflow(self.reviewed) + f"  extra:\n    steps:\n      - uses: {value}\n"
                self.assert_blocked(self.run_check(text), "directly under .github/workflows")

    def test_action_owner_outside_the_allowlist_fails(self):
        # A former name of this repository would redirect here, so an unknown
        # owner/repo cannot be treated as a third party.
        for value in (f"someone/action@{'0' * 40}", f"Concord-Voice/Concord-Voice/action@{'0' * 40}"):
            with self.subTest(value=value):
                text = self.workflow(self.reviewed) + f"  extra:\n    steps:\n      - uses: {value}\n"
                self.assert_blocked(self.run_check(text), "an owner this check does not allow")

    def test_merged_side_branch_commit_fails(self):
        # An ancestor of main through a merge commit's second parent, so it
        # passes ancestry, but no review ever saw it as a whole state of main.
        run = self.run_check(self.workflow(self.reviewed, self.merged_side))
        self.assert_blocked(run, "is not a commit on main's first-parent history")

    def test_two_pins_for_one_path(self):
        # Review F1: a base pinning one path twice must not read its own older
        # pin as a rollback, or every later PR fails.
        canary = f"  canary:\n    uses: {OURS}/pr-ci.yml@{self.later}\n"
        both = self.workflow(self.reviewed) + canary
        self.assertEqual(self.run_check(both).returncode, 0)
        self.assertEqual(self.run_check(both, base=both).returncode, 0)
        older = f"  canary:\n    uses: {OURS}/pr-ci.yml@{self.missing}\n"
        self.assert_blocked(self.run_check(self.workflow(self.reviewed) + older, base=both),
                            "moves back from the base's pin")

    def contract_caller(self, ref=None, job="", grant="      security-events: write\n      statuses: write\n",
                        tail=""):
        return (
            f"jobs:\n  c:\n    uses: {OURS}/contract.yml@{ref or self.contract}\n"
            f"    with:\n      mode: x\n      flag: true\n    secrets:\n      TOKEN: x\n{job}"
            f"    permissions:\n      contents: read\n{grant}"
            f"  r:\n    needs: [c]\n    if: needs.c.outputs.selection\n"
            f"    steps:\n      - uses: {self.checkout}\n{tail}"
        )

    def test_callee_contract_holds(self):
        run = self.run_check(self.contract_caller(), base="")
        self.assertEqual(run.returncode, 0, run)
        inherit = self.contract_caller().replace("    secrets:\n      TOKEN: x\n", "    secrets: inherit\n")
        self.assertEqual(self.run_check(inherit, base="").returncode, 0)

    def test_callee_contract_breaks_fail(self):
        # Review D2-D7: each one passes ancestry, then cannot start once merged.
        base = self.contract_caller()
        for text, needle in (
            (base.replace("      mode: x\n", "      mode: x\n      extra: y\n"), "passes input `extra`"),
            (base.replace("      mode: x\n", ""), "does not pass required input `mode`"),
            # Review r4: a default does not excuse a required input.
            (base.replace("      flag: true\n", ""), "does not pass required input `flag`"),
            (base.replace("    with:\n      mode: x\n      flag: true\n", "    with: x\n"), "a `with` that is not"),
            (base.replace("    secrets:\n      TOKEN: x\n", "    secrets: inhrit\n"), "neither a mapping nor"),
            (base.replace("      TOKEN: x\n", "      TOKEN: x\n      OTHER: y\n"), "passes secret `OTHER`"),
            (base.replace("    secrets:\n      TOKEN: x\n", ""), "does not pass required secret `TOKEN`"),
            (base.replace("needs.c.outputs.selection", "needs.c.outputs.missing"), "reads output `missing`"),
            (base.replace("      security-events: write\n", ""), "nested job `y` in `.github/workflows/inner.yml`"),
            (base.replace("      statuses: write\n", ""), "nested job `x` in `.github/workflows/inner.yml`"),
            (base.replace("security-events: write", "security-events: read"), "asking for security-events"),
            (base.replace("security-events: write", "security-events: admin"), "asking for security-events"),
            (base.replace("    permissions:\n      contents: read\n      security-events: write\n"
                          "      statuses: write\n", ""),
             "has no permissions block"),
            (self.contract_caller(f"{self.contract}").replace("contract.yml", "push.yml"), "is not reusable"),
        ):
            with self.subTest(needle=needle):
                self.assert_blocked(self.run_check(text, base=""), needle)

    def test_pinned_nested_workflow_is_followed(self):
        caller = (f"jobs:\n  c:\n    uses: {OURS}/pinnest.yml@{self.pinnest}\n"
                  "    permissions:\n      security-events: write\n      statuses: write\n")
        self.assertEqual(self.run_check(caller, base="").returncode, 0)
        self.assert_blocked(self.run_check(caller.replace("      statuses: write\n", ""), base=""),
                            "nested job `x` in `.github/workflows/inner.yml`")
        self.assert_blocked(self.run_check(caller.replace("pinnest.yml", "pinnest_contract.yml"), base=""),
                            "nested job `j0` in `.github/workflows/pinnest_contract.yml` that does not pass required")

    def graph_caller(self, name):
        return (f"permissions:\n  contents: read\njobs:\n  c:\n    uses: {OURS}/{name}@{self.contract}\n")

    def test_nested_graph_rules(self):
        # Review N4-N9: the graph a pin brings in runs with the caller's secrets,
        # so its own calls are held to the pin rules, judged once per path.
        for name, needle in (
            ("diamond.yml", "nested job `x` in `.github/workflows/leaf.yml`"),
            ("cycle.yml", "already on its own call chain"),
            ("mask.yml", "beyond GitHub's limit of 10 levels"),
            ("nest_branch.yml", "is not a commit on main's first-parent history"),
            ("nest_mutable.yml", "neither a local workflow nor pinned"),
            ("nest_stranger.yml", "an owner this check does not allow"),
            ("wide.yml", "51 unique reusable workflows"),
            ("wide_ext.yml", "51 unique reusable workflows"),
            ("nest_path.yml", "not a workflow file directly under .github/workflows"),
            ("nest_contract.yml", "nested job `j0` in `.github/workflows/nest_contract.yml` that does not pass "
                                  "required input `mode`"),
            ("nest_push.yml", "nested job `j0` in `.github/workflows/nest_push.yml` that names a workflow that is "
                              "not reusable"),
            ("top_over.yml", "asking at workflow level for contents"),
            ("bom_hide.yml", "which contains a byte-order mark"),
            ("duplicate.yml", "which repeats a mapping key at line 7"),
            ("step_mutable.yml", "`someone/action@main` in job `x` of `.github/workflows/step_mutable.yml` that is "
                                 "not pinned"),
            ("step_docker.yml", "`docker://alpine:3` in job `x` of `.github/workflows/step_docker.yml` that is not "
                                "pinned"),
            ("step_branch.yml", "that is not a commit on main's first-parent history"),
            # Codex r2: a local action runs from the workspace, which may hold the PR.
            ("step_local.yml", "runs code from the job's workspace, not the pinned commit"),
        ):
            with self.subTest(name=name):
                self.assert_blocked(self.run_check(self.graph_caller(name), base=""), needle)
        for name in ("diamond_ok.yml", "nest_google.yml", "wide_ok.yml", "anchored.yml", "step_ok.yml"):
            with self.subTest(name=name):
                run = self.run_check(self.graph_caller(name), base="")
                self.assertEqual(run.returncode, 0, run)

    def test_literal_input_types(self):
        # Review r4: GitHub refuses a literal whose type is not the input's.
        base = self.contract_caller()
        for value, needle in (('"true"', "not a boolean"), ("yes", "not a boolean"), ("", "not a boolean"),
                              ("[true]", "not a scalar")):
            with self.subTest(value=value):
                self.assert_blocked(self.run_check(base.replace("flag: true", f"flag: {value}"), base=""), needle)
        for value in ("'3'", "three", "1e3"):
            with self.subTest(value=value):
                text = base.replace("flag: true\n", f"flag: true\n      count: {value}\n")
                self.assert_blocked(self.run_check(text, base=""), "not a number")
        for flag, count in (("FALSE", "-2.5"), ("${{ github.event_name == 'push' }}", "'${{ 3 }}'")):
            with self.subTest(flag=flag):
                text = base.replace("flag: true\n", f"flag: {flag}\n      count: {count}\n")
                run = self.run_check(text, base="")
                self.assertEqual(run.returncode, 0, run)

    def test_read_all_covers_every_scope(self):
        caller = f"permissions: read-all\njobs:\n  c:\n    uses: {OURS}/quality.yml@{self.contract}\n"
        run = self.run_check(caller, base="")
        self.assertEqual(run.returncode, 0, run)

    def test_output_reads_in_any_spelling(self):
        base = self.contract_caller()
        for spelling in ("needs['c'].outputs['missing']", "needs.C.outputs.MISSING", "needs [ \"c\" ] . outputs.missing"):
            with self.subTest(spelling=spelling):
                text = base.replace("needs.c.outputs.selection", spelling)
                self.assert_blocked(self.run_check(text, base=""), "reads output")
        # Review r5 H2: the index form of `outputs`, a YAML escape, and a shell comment
        # inside a block scalar, which GitHub still expands.
        for text in (base.replace("needs.c.outputs.selection", "needs['c']['outputs']['missing']"),
                     base.replace("needs.c.outputs.selection", '"needs.c.outputs.mis\\x73ing"'),
                     self.contract_caller(tail="      - run: |\n          # ${{ needs.c.outputs.missing }}\n")):
            with self.subTest(text=text[-60:]):
                self.assert_blocked(self.run_check(text, base=""), "reads output `missing`")
        # Codex r1: prose GitHub never evaluates is not a read.
        prose = ("      - name: needs.c.outputs.debug\n        run: echo needs.c.outputs.debug\n"
                 "        if: contains('needs.c.outputs.debug', 'x')\n")
        quoted = "      - run: echo ${{ format('}}', needs.c.outputs.missing) }}\n"
        self.assert_blocked(self.run_check(self.contract_caller(tail=quoted), base=""), "reads output `missing`")
        for text in (self.contract_caller(tail=prose),
                     base.replace("needs.c.outputs.selection", "needs['c'].outputs['Selection']"),
                     "# needs.c.outputs.ghost\n" + base):
            with self.subTest(text=text[:40]):
                self.assertEqual(self.run_check(text, base="").returncode, 0)

    def test_quote_runs_in_an_expression_stay_linear(self):
        # CodeQL py/redos: an unclosed `${{` before a long run of quotes used to
        # backtrack exponentially. It must still finish, and `''` stays an escape.
        flood = "      - run: echo ${{{{'" + "''''" * 40 + "\n"
        self.assertEqual(self.run_check(self.contract_caller(tail=flood), base="").returncode, 0)
        escaped = "      - run: echo ${{ format('it''s }}', needs.c.outputs.missing) }}\n"
        self.assert_blocked(self.run_check(self.contract_caller(tail=escaped), base=""), "reads output `missing`")

    def test_byte_order_mark_after_the_start_fails(self):
        # Review N1: libyaml skips a BOM at the start of any line, PyYAML's pure-
        # Python reader does not, so they can place the key after it differently.
        bom = chr(0xFEFF)
        text = self.workflow(self.reviewed)
        self.assertEqual(self.run_check(bom + text).returncode, 0)
        self.assert_blocked(self.run_check(text.replace("  osv:", bom + "  osv:")), "byte-order mark")

    def test_unreadable_file_fails_with_an_annotation(self):
        path = os.path.join(self.tmp.name, "binary-required-pr-build.yml")
        Path(path).write_bytes(b"jobs: \xff\xfe\n")
        run = subprocess.run(
            [sys.executable, str(SCRIPT), "--repo", self.repo, "--main-ref", "main",
             "--workflow", path, "--base-workflow", path], env=ENV, capture_output=True, text=True)
        self.assert_blocked(run, "could not run (UnicodeDecodeError)")

    def test_third_party_pin_must_be_on_a_branch_or_tag(self):
        # Review r5 H4: an allowed owner's name alone proves nothing, since GitHub
        # resolves a fork's commit under it too.
        base = self.workflow(self.reviewed)
        for sha in (self.tagged, self.checkout.split("@")[1]):
            with self.subTest(sha=sha):
                run = self.run_check(base.replace(self.checkout, f"actions/checkout@{sha}"), base=base)
                self.assertEqual(run.returncode, 0, run)
        fork = base.replace(self.checkout, f"actions/checkout@{self.fork}")
        self.assert_blocked(self.run_check(fork, base=base), "is on no branch or tag of github.com/actions/checkout")
        # A pin the PR does not change is not fetched, so main's pins never hold back a PR.
        self.assertEqual(self.run_check(fork, base=fork).returncode, 0)
        missing = base.replace(self.checkout, f"actions/missing@{'1' * 40}")
        self.assert_blocked(self.run_check(missing, base=base), "github.com/actions/missing could not be fetched")

    def test_odd_values_fail_with_an_annotation(self):
        # Review r5 H3: a list as a permission level, and nesting deep enough to
        # exhaust Python's recursion, used to end in a traceback with no annotation.
        listed = self.contract_caller(grant="      security-events: [write]\n      statuses: write\n")
        run = self.run_check(listed, base="")
        self.assert_blocked(run, "asking for security-events")
        self.assertNotIn("Traceback", run.stderr)
        self.assert_blocked(self.run_check("jobs: " + "[" * 3000 + "]" * 3000 + "\n", base=""),
                            "could not run (RecursionError)")

    def test_nesting_limit(self):
        chain = (f"permissions:\n  contents: read\njobs:\n  c:\n    uses: {OURS}/chain1.yml@{self.contract}\n")
        self.assertEqual(self.run_check(chain, base="").returncode, 0)
        self.assert_blocked(self.run_check(chain.replace("chain1.yml", "chain0.yml"), base=""),
                            "beyond GitHub's limit of 10 levels")

    def test_internal_pin_must_be_a_job_uses(self):
        # A step resolves `uses` as an action, so a workflow file there cannot run.
        for extra in (f"  extra:\n    steps:\n      - uses: {OURS}/pr-ci.yml@{self.reviewed}\n",
                      f"  extra:\n    steps:\n      - uses: {OURS}/push.yml@{self.contract}\n"):
            with self.subTest(extra=extra):
                self.assert_blocked(self.run_check(self.workflow(self.reviewed) + extra), "must be a job's `uses`")

    def test_job_level_action_fails(self):
        # An allowed owner does not make an action a reusable workflow.
        text = self.workflow(self.reviewed) + f"  extra:\n    uses: {self.checkout}\n"
        self.assert_blocked(self.run_check(text), "calls a reusable workflow outside this repository")

    def test_no_internal_pin_fails(self):
        self.assert_blocked(self.run_check(f"jobs:\n  a:\n    steps:\n      - uses: {self.checkout}\n"),
                            "no pin to this repository was found")

    def test_unresolvable_main_ref_fails(self):
        self.assert_blocked(self.run_check(self.workflow(self.reviewed), main_ref="refs/scan/main"),
                            "does not resolve")

    def test_repository_files_pass(self):
        # Positive controls: this checkout's file, and main's file read by this
        # checkout's script, since a merged script judges main's file next (review
        # L2). workflow-lint checks out full history, so CI always has origin/main.
        if subprocess.run(["git", "-C", str(ROOT), "rev-parse", "--verify", "--quiet", "origin/main"],
                          capture_output=True).returncode != 0:
            if os.environ.get("CI") == "true":
                self.fail("origin/main is not available, so the required build pin cannot be checked")
            self.skipTest("origin/main is not fetched")
        if subprocess.run(
            ["git", "-C", str(ROOT), "cat-file", "-e", "origin/main:.github/workflows/required-pr-build.yml"],
            capture_output=True,
        ).returncode != 0:
            # The split repository has no protected workflow on main until this
            # bootstrap PR merges. Keep the positive control active thereafter.
            self.skipTest("bootstrap: required-pr-build.yml is not on main yet")
        main_copy = os.path.join(self.tmp.name, "main-required-pr-build.yml")
        Path(main_copy).write_text(subprocess.run(
            ["git", "-C", str(ROOT), "show", "origin/main:.github/workflows/required-pr-build.yml"],
            check=True, capture_output=True, text=True).stdout)
        for path in (ROOT / ".github/workflows/required-pr-build.yml", main_copy):
            with self.subTest(path=str(path)):
                run = subprocess.run(
                    [sys.executable, str(SCRIPT), "--repo", str(ROOT), "--main-ref", "origin/main",
                     "--workflow", str(path), "--base-workflow", main_copy],
                    capture_output=True, text=True,
                )
                self.assertEqual(run.returncode, 0, run)


if __name__ == "__main__":
    unittest.main()
