#!/usr/bin/env python3
"""Fail unless every pin in required-pr-build.yml to this repository is a state of main
that GitHub can run.

The ruleset loads required-pr-build.yml from protected main but runs the reusable
graph at the pinned SHA. A pin to a branch-only commit would therefore run
unreviewed workflow code, with SONAR_TOKEN and security-events: write, for every
PR once it merged. The pin PR's own protected run still uses the old pin and
cannot see the new one, so `Required PR build result` runs this from main against
the PR's file (#3866).

Two independent readers must agree, so a value one of them misreads fails closed:
  * a strict line rule: every line naming `uses` must be exactly
    `uses: <owner>/<repo>[/<path>]@<40-hex>`, optionally quoted, optionally
    followed by ` # comment`, in ASCII;
  * a YAML read of the composed node graph: the multiset of every `uses` value it
    finds, under keys decoded from escapes or explicit-key syntax, must equal the
    line rule's. The node graph keeps keys as written, so YAML 1.1 typed keys
    (`on`, `yes` and `true` all construct to True) cannot fold two jobs into one.

The read also refuses the YAML features where parsers diverge and that a
required workflow never needs: anchors, aliases, explicit tags, merge keys,
non-scalar keys, duplicate keys, keys that only case-fold to `uses`, the YAML
1.1-only line breaks, and a byte-order mark anywhere but the start.

Then: a job's `uses` (a reusable workflow) may come only from this repository,
and a pin to this repository may only be a job's `uses`. A third-party pin the
PR changes must be on a branch or tag of its repository, fetched from GitHub,
since GitHub resolves a fork's commit under the parent's name too. Every pin to
this repository must be on main's first-parent history (commits of a branch
merged with a merge commit are ancestors of main that main never was), must
descend from one of the base's pins for that path, and must be runnable once
merged: a file that declares on.workflow_call, accepts every input and secret
the job passes, receives every required input (a default does not excuse one)
and every required secret, gets a literal of the declared type for each boolean
or number input, and declares every output the file reads from that job (in an
`if` value or a `${{ }}` body, where a single-quoted string is not a read unless
it is an index). `with` must be a mapping, and `secrets` a mapping or `inherit`.

The graph the pin brings in runs with the same secrets, so it is walked too,
once per path through it: each file gets the required file's YAML refusals
(anchors aside, which both readers expand alike), every nested call must be
local or pinned to a 40-hex sha and is held to the same startup rules, and every
step's `uses` must be 40-hex pinned or an image digest (a local `./` action is
refused, since GitHub reads it from the job's workspace rather than the pinned
commit); a pin to this repository must be on main's first-parent history and is
followed, and another repository must be one of THIRD_PARTY_OWNERS. Along every path the check can
read, no job and no workflow-level block may ask for a permission beyond what
its caller granted, no workflow may call itself, and the depth stays within
GitHub's ten levels; the graph may hold at most 50 unique reusable workflows.
Another repository's workflow is counted but not read. A merged pin GitHub
cannot run blocks every PR, with no bypass actor.

Usage: verify-required-build-pins.py --repo DIR --main-ref REF --workflow FILE --base-workflow FILE
Exit 0 when every pin holds; otherwise 1, with one ::error:: line per violation,
or a single one when the check cannot run.
"""
import argparse
from collections import Counter
import os
import re
import subprocess
import sys
import tempfile
from types import SimpleNamespace

import yaml

THIS_REPO = "concord-voice/concord-voice-alpha"
# GitHub redirects a renamed or transferred repository's old name, and no API
# lists those names, so any other owner/repo could be this repository in
# disguise. A required build's actions, and the reusable workflows its pinned
# graph calls from elsewhere, are therefore limited to these owners. main's copy
# of this script judges every PR, so adding an owner takes its own PR first.
THIRD_PARTY_OWNERS = {"actions", "google"}
UPSTREAM = "https://github.com/"
WORKFLOW_PATH = re.compile(r"^/\.github/workflows/[A-Za-z0-9_][A-Za-z0-9_.-]*\.ya?ml$", re.ASCII)
LOCAL_WORKFLOW = re.compile(r"\./(\.github/workflows/[A-Za-z0-9_][A-Za-z0-9_.-]*\.ya?ml)", re.ASCII)
YAML11_ONLY_BREAKS = (chr(0x85), chr(0x2028), chr(0x2029))
BOM = chr(0xFEFF)
USES_WORD = re.compile(r"(?<![A-Za-z0-9_])uses(?![A-Za-z0-9_])", re.IGNORECASE)
# Path segments are non-empty and never `.` or `..`, so one file has one spelling.
VALUE = (r"(?P<repo>[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)(?P<path>(?:/(?!\.\.?(?=[/@]))[A-Za-z0-9_.-]+)+)?"
         r"@(?P<sha>[0-9a-f]{40})")
PINNED = re.compile(VALUE, re.ASCII)
IMAGE_DIGEST = re.compile(r"docker://[^@\s]+@sha256:[0-9a-f]{64}", re.ASCII)
# A `${{ }}` body: single-quoted literals (with '' escapes) may hold `}}`.
EXPRESSION = re.compile(r"\$\{\{((?:[^'}]|'(?:[^']|'')*'|\}(?!\}))*)\}\}")
# A single-quoted literal is a string, not a property read, unless it is an index.
LITERAL = re.compile(r"(\[\s*)?'(?:[^']|'')*'")
LINE = re.compile(r"^ *(?:- )?uses: (?P<q>[\"']?)(?P<v>" + VALUE + r")(?P=q)(?: +#[ -~]*)?$", re.ASCII)
LEVELS = {"none": 0, "read": 1, "write": 2}
SCOPES = ("actions", "artifact-metadata", "attestations", "checks", "code-quality", "contents", "copilot-requests",
          "deployments", "discussions", "drives", "id-token", "issues", "models", "packages", "pages",
          "pull-requests", "repository-projects", "security-events", "statuses", "vulnerability-alerts")
TRUE = ("true", "True", "TRUE")
BOOLEAN = TRUE + ("false", "False", "FALSE")
# Plain decimals only: a number GitHub might also read in another notation is refused, never passed.
NUMBER = re.compile(r"[-+]?[0-9]+(?:\.[0-9]+)?", re.ASCII)
# GitHub runs at most ten levels of workflows (the caller and nine reusable ones)
# and at most 50 unique reusable workflows from one file, nested ones included.
LEVEL_LIMIT = 10
WORKFLOW_LIMIT = 50


def load_strings(text):
    # BaseLoader constructs only str, list and dict, so no tag can build an object
    # and every scalar stays the string GitHub reads.
    loader = yaml.BaseLoader(text)
    try:
        return loader.get_single_data()
    finally:
        loader.dispose()


def scalar(node):
    return node.value if isinstance(node, yaml.ScalarNode) else "<non-scalar>"


def unsafe_characters(text):
    # PyYAML reads YAML 1.1, where these are line breaks; GitHub reads YAML 1.2,
    # where they are content. Either reader could then see a value the other
    # does not, so refuse them. Lines split on LF alone, as YAML 1.2 does.
    if any(char in text for char in YAML11_ONLY_BREAKS):
        return "contains a YAML 1.1-only line break (U+0085, U+2028 or U+2029)"
    # libyaml, and the readers built on it, skip a byte-order mark at the start of
    # any line, so a key after one can belong to a job there and to the top level
    # for PyYAML's pure-Python reader. Only a leading one reads the same to both.
    if BOM in text[1:]:
        return "contains a byte-order mark (U+FEFF) after its first character"
    return None


def read_yaml(text, anchors=False):
    """Return (refusal, every `uses` value, job-level `uses` values) from the node graph."""
    for event in yaml.parse(text, Loader=yaml.SafeLoader):
        if not anchors and (isinstance(event, yaml.AliasEvent) or getattr(event, "anchor", None)):
            return "uses a YAML anchor or alias", [], []
        if getattr(event, "tag", None) is not None:
            return "uses an explicit YAML tag", [], []
    root = yaml.compose(text, Loader=yaml.SafeLoader)
    uses, stack, visited = [], [root], set()
    while stack:
        node = stack.pop()
        if id(node) in visited:
            continue  # an alias shares its anchor's node, so expansion cannot multiply the walk
        visited.add(id(node))
        if isinstance(node, yaml.MappingNode):
            seen = set()
            for key, value in node.value:
                if not isinstance(key, yaml.ScalarNode):
                    return "has a mapping key that is not a scalar", [], []
                if key.value == "<<":
                    return "uses a YAML merge key", [], []
                if key.value.casefold() == "uses" and key.value != "uses":
                    return "has a key that only case-folds to `uses`", [], []
                # PyYAML keeps the last of two equal keys; other parsers refuse the
                # file or keep the first, so a duplicate hidden from the line rule
                # would make both readers agree on the visible copy.
                if key.value in seen:
                    return f"repeats a mapping key at line {key.start_mark.line + 1}", [], []
                seen.add(key.value)
                if key.value == "uses":
                    uses.append(scalar(value))
                stack.append(value)
        elif isinstance(node, yaml.SequenceNode):
            stack.extend(node.value)
    jobs = []
    if isinstance(root, yaml.MappingNode):
        for key, value in root.value:
            if key.value == "jobs" and isinstance(value, yaml.MappingNode):
                for _, job in value.value:
                    if isinstance(job, yaml.MappingNode):
                        jobs += [scalar(v) for k, v in job.value if k.value == "uses"]
    return None, uses, jobs


def expressions(document):
    """The text GitHub evaluates in a loaded document: each `if` value whole, and every
    `${{ }}` body elsewhere. Each container is visited once."""
    found, stack, seen = [], [(None, document)], set()
    while stack:
        key, item = stack.pop()
        if isinstance(item, str):
            found += [item] if key == "if" else EXPRESSION.findall(item)
        elif id(item) not in seen:
            seen.add(id(item))
            if isinstance(item, dict):
                stack += list(item.items())
            elif isinstance(item, list):
                stack += [(None, value) for value in item]
    return found


def step_problem(action, walk):
    """Why a step's `uses` in the pinned graph could run code no review saw, or None."""
    if not isinstance(action, str):
        return "is not a string"
    if action.startswith("./"):
        # GitHub reads a local action from the job's workspace, which holds whatever
        # the job checked out (often the PR), not the pinned commit.
        return (f"runs code from the job's workspace, not the pinned commit; name it as "
                f"`Concord-Voice/Concord-Voice-Alpha/{action[2:]}@<sha>` instead")
    if IMAGE_DIGEST.fullmatch(action):
        return None
    pinned = PINNED.fullmatch(action)
    if not pinned:
        return "is not pinned to a 40-hex sha or an image digest, so it can change after review"
    if pinned.group("repo").lower() == THIS_REPO and pinned.group("sha") not in walk.first_parent:
        return "is not a commit on main's first-parent history"
    return None


def with_nodes(text, job_id):
    """The node of each `with` value of job `job_id`: a literal's type needs its quoting."""
    node = yaml.compose(text, Loader=yaml.SafeLoader)
    for name in ("jobs", job_id, "with"):
        if not isinstance(node, yaml.MappingNode):
            return {}
        node = next((value for key, value in node.value
                     if isinstance(key, yaml.ScalarNode) and key.value == name), None)
    if not isinstance(node, yaml.MappingNode):
        return {}
    return {key.value: value for key, value in node.value if isinstance(key, yaml.ScalarNode)}


def git(repo, *args):
    return subprocess.run(["git", "-C", repo, *args], capture_output=True, text=True)


def is_ancestor(repo, older, newer):
    return git(repo, "merge-base", "--is-ancestor", older, newer).returncode == 0


def blob(repo, sha, path):
    if git(repo, "cat-file", "-t", f"{sha}:{path}").stdout.strip() != "blob":
        return None
    return git(repo, "show", f"{sha}:{path}").stdout


def upstream_has(name, sha, cache):
    """Whether a branch or tag of github.com/<name> reaches `sha`; None when it cannot be fetched."""
    if name not in cache:
        scratch = tempfile.TemporaryDirectory()
        # GitHub resolves a fork's commit under the parent's name too, so the SHA
        # alone proves nothing. A ref-only fetch brings only what branches and tags
        # reach, and with no partial-clone filter git never asks for a missing
        # object by its SHA, which GitHub would serve from the fork.
        fetched = git(scratch.name, "init", "-q", "--bare").returncode == 0 and subprocess.run(
            ["git", "-C", scratch.name, "fetch", "-q", "--no-tags", UPSTREAM + name,
             "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"],
            capture_output=True, env={**os.environ, "GIT_TERMINAL_PROMPT": "0"}, timeout=300).returncode == 0
        cache[name] = (scratch, fetched)
    scratch, fetched = cache[name]
    return git(scratch.name, "cat-file", "-e", f"{sha}^{{commit}}").returncode == 0 if fetched else None


def load_workflow(repo, sha, path):
    text = blob(repo, sha, path)
    try:
        workflow = load_strings(text) if text is not None else None
    except yaml.YAMLError:
        return None, None
    return (workflow, text) if isinstance(workflow, dict) else (None, None)


def internal_pins(text):
    pins = {}
    for line in text.replace("\r\n", "\n").split("\n"):
        match = LINE.match(line)
        if match and match.group("repo").lower() == THIS_REPO:
            pins.setdefault(match.group("path") or "", set()).add(match.group("sha"))
    return pins


def permissions(value, unknown):
    """Map a permissions block to {scope: level}; a level GitHub does not define gets `unknown`."""
    if value is None:
        return None
    if isinstance(value, str):
        level = {"read-all": 1, "write-all": 2}.get(value.strip(), 0)
        return {scope: level for scope in SCOPES}
    if isinstance(value, dict):
        return {scope: LEVELS.get(level, unknown) if isinstance(level, str) else unknown
                for scope, level in value.items() if scope != "metadata"}
    return {scope: unknown for scope in SCOPES}


def permission_problems(repo, sha, path, grant, walk, level=2, chain=()):
    # Mirrors lint-workflow-call-contract.sh, which applies the same rule to every
    # workflow in ordinary CI; this copy is the merge-blocking one for the pins.
    # Each (file, grant, level) is judged on its own: a file reached under two
    # grants must satisfy both, and one reached again deeper must fit the level
    # limit from there. A file already on its own call chain is a loop.
    if (sha, path) in chain:
        return [f"calls `{path}` at {sha}, which is already on its own call chain"]
    key = (sha, path, tuple(sorted(grant.items())), level)
    if key in walk.memo:
        return walk.memo[key]
    walk.memo[key] = []
    chain += ((sha, path),)
    workflow, text = load_workflow(repo, sha, path)
    if workflow is None:
        walk.memo[key] = [f"needs `{path}` at {sha}, which is missing or unreadable"]
        return walk.memo[key]
    # The walk reads each file once, with PyYAML, so a file GitHub could read
    # differently is refused as the required file is. Anchors read the same to both.
    refused = unsafe_characters(text) or read_yaml(text, anchors=True)[0]
    if refused:
        walk.memo[key] = [f"needs `{path}` at {sha}, which {refused}"]
        return walk.memo[key]
    found = []
    top = permissions(workflow.get("permissions"), 3)
    # GitHub judges a callee's workflow-level block on its own, even when every
    # job replaces it.
    over = sorted(scope for scope, level_asked in (top or {}).items() if level_asked > grant.get(scope, 0))
    if over:
        found.append(f"has `{path}` asking at workflow level for {', '.join(over)} beyond its caller's grant")
    jobs = workflow.get("jobs") if isinstance(workflow.get("jobs"), dict) else {}
    for job_id, job in jobs.items():
        if not isinstance(job, dict):
            continue
        # A reviewed file still runs whatever a mutable reference points at later.
        steps = job.get("steps") if isinstance(job.get("steps"), list) else []
        for step in steps:
            problem = step_problem(step["uses"], walk) if isinstance(step, dict) and "uses" in step else None
            if problem:
                found.append(f"has a step action `{step['uses']}` in job `{job_id}` of `{path}` that {problem}")
        # Job-level permissions replace the workflow's; without either, the job
        # receives exactly what its caller granted.
        asked = permissions(job["permissions"], 3) if "permissions" in job else (top if top is not None else grant)
        over = sorted(scope for scope, level_asked in asked.items() if level_asked > grant.get(scope, 0))
        if over:
            found.append(f"has nested job `{job_id}` in `{path}` asking for {', '.join(over)} beyond its caller's grant")
            continue
        if "uses" not in job:
            continue
        # The graph a pin brings in runs with the caller's secrets too, so a nested
        # call is held to the rule a pin in the required file is.
        nested = job["uses"] if isinstance(job["uses"], str) else ""
        local = LOCAL_WORKFLOW.fullmatch(nested)
        pinned = PINNED.fullmatch(nested)
        if not (local or pinned):
            found.append(f"calls `{nested}` from `{path}`, which is neither a local workflow nor pinned to a 40-hex sha")
        elif level >= LEVEL_LIMIT:
            found.append(f"calls a workflow from `{path}` beyond GitHub's limit of {LEVEL_LIMIT} levels")
        elif local:
            found += nested_contract(repo, sha, local.group(1), (sha, path, text), job, job_id, walk)
            found += permission_problems(repo, sha, local.group(1), asked, walk, level + 1, chain)
        elif pinned.group("repo").lower() != THIS_REPO:
            # Another repository's workflow cannot be read here, so its own
            # permissions and nesting are not checked.
            if pinned.group("repo").split("/")[0].lower() not in THIRD_PARTY_OWNERS:
                found.append(f"calls `{nested}` from `{path}`, from an owner this check does not allow "
                             f"(THIRD_PARTY_OWNERS in verify-required-build-pins.py)")
            walk.external.add(nested)
        elif not WORKFLOW_PATH.match(pinned.group("path") or ""):
            found.append(f"calls `{nested}` from `{path}`, which is not a workflow file directly under .github/workflows")
        elif pinned.group("sha") not in walk.first_parent:
            found.append(f"calls `{nested}` from `{path}`, which is not a commit on main's first-parent history")
        else:
            target = pinned.group("path").lstrip("/")
            found += nested_contract(repo, pinned.group("sha"), target, (sha, path, text), job, job_id, walk)
            found += permission_problems(repo, pinned.group("sha"), target, asked, walk, level + 1, chain)
    walk.memo[key] = found
    return found


def nested_contract(repo, sha, path, caller, job, job_id, walk):
    """A nested call is held to the startup rules a pin in the required file is, once per calling job."""
    caller_sha, caller_path, caller_text = caller
    if (caller_sha, caller_path, job_id) in walk.contracts:
        return []
    walk.contracts.add((caller_sha, caller_path, job_id))
    callee, _ = load_workflow(repo, sha, path)
    if callee is None:
        return []  # permission_problems reports the missing file
    return [f"has nested job `{job_id}` in `{caller_path}` that {problem}"
            for problem in contract_problems(callee, job, job_id, caller_text)]


def contract_problems(callee, job, job_id, caller_text):
    """The rules GitHub checks before a calling job can start the workflow it names."""
    on = callee.get("on")
    if isinstance(on, dict) and "workflow_call" in on:
        call = on["workflow_call"] if isinstance(on["workflow_call"], dict) else {}
    elif on == "workflow_call" or (isinstance(on, list) and "workflow_call" in on):
        call = {}
    else:
        return ["names a workflow that is not reusable (no on.workflow_call) at that commit"]
    found = []
    inputs = call.get("inputs") if isinstance(call.get("inputs"), dict) else {}
    passed = job.get("with", "")
    if not isinstance(passed, dict):
        if passed != "":
            found.append("passes a `with` that is not a mapping")
        passed = {}
    found += [f"passes input `{name}`, which the callee does not declare" for name in passed if name not in inputs]
    # A default does not excuse a required input: GitHub's own workflow parser
    # refuses the call whether or not one is declared.
    found += [f"does not pass required input `{name}`" for name, spec in inputs.items()
              if isinstance(spec, dict) and spec.get("required") in TRUE and name not in passed]
    for name, node in with_nodes(caller_text, job_id).items():
        kind = inputs[name].get("type") if isinstance(inputs.get(name), dict) else None
        if not isinstance(node, yaml.ScalarNode):
            found.append(f"passes input `{name}` a value that is not a scalar")
        elif "${{" in node.value or kind not in ("boolean", "number"):
            continue  # an expression is typed at run time; any literal is a string
        elif node.style is not None or not (node.value in BOOLEAN if kind == "boolean"
                                            else NUMBER.fullmatch(node.value)):
            found.append(f"passes input `{name}` a literal that is not a {kind}")
    declared = call.get("secrets") if isinstance(call.get("secrets"), dict) else {}
    given = job.get("secrets", "")
    if given != "inherit":
        if not isinstance(given, dict):
            if given != "":
                found.append("passes `secrets` that is neither a mapping nor `inherit`")
            given = {}
        found += [f"passes secret `{name}`, which the callee does not declare" for name in given
                  if name not in declared]
        found += [f"does not pass required secret `{name}`" for name, spec in declared.items()
                  if isinstance(spec, dict) and spec.get("required") in TRUE and name not in given]
    outputs = call.get("outputs") if isinstance(call.get("outputs"), dict) else {}
    # GitHub evaluates `if` values and `${{ }}` bodies (in a shell comment inside a
    # block scalar too), never a YAML comment or other prose, and an expression
    # names a property by `.` or by index, in any case.
    code = LITERAL.sub(lambda m: m.group(0) if m.group(1) else "''",
                       "\n".join(expressions(load_strings(caller_text))))
    job_ref = rf"needs\s*(?:\.\s*{re.escape(job_id)}|\[\s*['\"]{re.escape(job_id)}['\"]\s*\])"
    outputs_ref = r"(?:\.\s*outputs|\[\s*['\"]outputs['\"]\s*\])"
    name_ref = r"(?:\.\s*([A-Za-z0-9_-]+)|\[\s*['\"]([^'\"]+)['\"]\s*\])"
    read = {a or b for a, b in re.findall(rf"{job_ref}\s*{outputs_ref}\s*{name_ref}", code, re.IGNORECASE)}
    declared_outputs = {str(name).lower() for name in outputs}
    found += [f"reads output `{name}`, which the callee does not declare" for name in sorted(read)
              if name.lower() not in declared_outputs]
    return found


def callee_problems(repo, sha, path, caller, job, job_id, caller_text, walk):
    callee, _ = load_workflow(repo, sha, path)
    if callee is None:
        return ["names a file that is missing or not a YAML mapping at that commit"]
    found = contract_problems(callee, job, job_id, caller_text)
    # A level GitHub does not define grants nothing; asking for one is always over.
    grant = permissions(job["permissions"], -1) if "permissions" in job else permissions(caller.get("permissions"), -1)
    if grant is None:
        found.append("has no permissions block, so its grant is a repository setting this check cannot read")
    else:
        found += permission_problems(repo, sha, path, grant, walk)
    return found


def violations(text, repo, main_ref, base_text=""):
    refused = unsafe_characters(text)
    if refused:
        return [f"the file {refused}"]
    found = []
    pins = []
    for number, line in enumerate(text.replace("\r\n", "\n").split("\n"), 1):
        if line.lstrip(" \t").startswith("#") or not USES_WORD.search(line):
            continue
        match = LINE.match(line)
        if match:
            pins.append(match)
        else:
            found.append(f"line {number} names `uses` but is not `uses: <owner>/<repo>[/<path>]@<40-hex sha>` "
                         f"(reword prose that mentions `uses`)")
    try:
        refused, yaml_values, job_values = read_yaml(text)
        caller = None if refused else load_strings(text)
    except yaml.YAMLError as error:
        return found + [f"the file is not valid YAML ({type(error).__name__})"]
    if refused:
        return found + [f"the file {refused}"]
    line_level = Counter(pin.group("v") for pin in pins)
    if Counter(yaml_values) != line_level:
        found.append("the YAML parser and the line rule disagree about the `uses` values")
    job_level = Counter(job_values)
    jobs = caller.get("jobs") if isinstance(caller, dict) else None
    jobs = jobs if isinstance(jobs, dict) else {}
    walk = SimpleNamespace(first_parent=set(git(repo, "rev-list", "--first-parent", main_ref).stdout.split()),
                           memo={}, external=set(), contracts=set())
    base = internal_pins(base_text)
    base_values = {match.group("v") for match in map(LINE.match, base_text.replace("\r\n", "\n").split("\n"))
                   if match}
    upstream = {}
    internal = 0
    checked = set()
    for pin in pins:
        value, sha, path = pin.group("v"), pin.group("sha"), pin.group("path") or ""
        if pin.group("repo").lower() != THIS_REPO:
            if job_level[value] or "/.github/workflows/" in path:
                found.append(f"`{value}` calls a reusable workflow outside this repository")
            elif pin.group("repo").split("/")[0].lower() not in THIRD_PARTY_OWNERS:
                found.append(f"`{value}` is an action from an owner this check does not allow "
                             f"(THIRD_PARTY_OWNERS in verify-required-build-pins.py)")
            elif value not in base_values and value not in checked:
                # Only a pin this PR changes is fetched, so an upstream outage holds
                # back nothing else.
                checked.add(value)
                reached = upstream_has(pin.group("repo"), sha, upstream)
                if reached is None:
                    found.append(f"`{value}` cannot be checked, since github.com/{pin.group('repo')} "
                                 f"could not be fetched")
                elif not reached:
                    found.append(f"`{value}` is on no branch or tag of github.com/{pin.group('repo')} "
                                 f"(GitHub also resolves a fork's commits under that name)")
            continue
        internal += 1
        if value in checked:
            continue
        checked.add(value)
        if not WORKFLOW_PATH.match(path):
            found.append(f"`{value}` must name a workflow file directly under .github/workflows")
            continue
        if job_level[value] != line_level[value]:
            found.append(f"`{value}` must be a job's `uses`, since a pin to this repository runs a reusable workflow")
            continue
        if sha not in walk.first_parent:
            found.append(f"`{value}` is not a commit on main's first-parent history")
            continue
        for job_id, job in jobs.items():
            if isinstance(job, dict) and job.get("uses") == value:
                found += [f"`{value}` (job `{job_id}`) {problem}"
                          for problem in callee_problems(repo, sha, path.lstrip("/"), caller, job, job_id, text,
                                                         walk)]
        # A path may carry several pins (a canary job, say); descending from any of
        # the base's keeps a later untouched PR from reading the older one as a rollback.
        previous = base.get(path, set())
        if previous and not any(is_ancestor(repo, older, sha) for older in previous):
            found.append(f"`{value}` moves back from the base's pin {', '.join(sorted(previous))}")
    if internal == 0:
        found.append("no pin to this repository was found")
    called = len({key[:2] for key in walk.memo}) + len(walk.external)
    if called > WORKFLOW_LIMIT:
        found.append(f"calls {called} unique reusable workflows, beyond GitHub's limit of {WORKFLOW_LIMIT}")
    return found


def annotate(message):
    # Workflow-command escaping, so file text cannot shape the annotation.
    return message.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo", required=True)
    parser.add_argument("--main-ref", required=True)
    parser.add_argument("--workflow", required=True)
    parser.add_argument("--base-workflow", required=True)
    args = parser.parse_args()
    resolved = git(args.repo, "rev-parse", "--verify", "--quiet", f"{args.main_ref}^{{commit}}")
    if resolved.returncode != 0:
        print(f"::error::{annotate(args.main_ref)} does not resolve, so the required build pins cannot be checked.")
        return 1
    try:
        with open(args.workflow, encoding="utf-8") as handle, open(args.base_workflow, encoding="utf-8") as base:
            found = violations(handle.read(), args.repo, args.main_ref, base.read())
    except Exception as error:  # any failure blocks, and says so in an annotation
        print(f"::error::required-pr-build.yml pin check could not run ({type(error).__name__}).")
        return 1
    for message in found:
        print(f"::error::required-pr-build.yml pin check: {annotate(message)}.")
    if not found:
        print("::notice::Every required build pin is a runnable state of main.")
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main())
