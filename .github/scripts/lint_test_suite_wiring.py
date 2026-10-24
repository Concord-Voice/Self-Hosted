# lint_test_suite_wiring.py — the body of lint-test-suite-wiring.sh.
#
# Run it through that wrapper, not directly: the wrapper resolves the repo root
# and checks it has .github/scripts/tests, and its header is the contract (what
# the lint checks, what it deliberately does not model, and --help).
#
#   python3 lint_test_suite_wiring.py <repo-root>

import pathlib
import re
import shlex
import sys

try:
    import yaml
except ImportError:                                  # pragma: no cover
    print("ERROR: PyYAML is required", file=sys.stderr)
    raise SystemExit(2)

root = pathlib.Path(sys.argv[1])
wf_dir = root / ".github" / "workflows"
tests_dir = root / ".github" / "scripts" / "tests"

# Commands that RUN a script passed as their argument.
RUNNERS = {"bash", "sh", "zsh", "source", "."}
# `bash [option]... script-file [arg]...` -- everything after the script file is
# a POSITIONAL PARAMETER, so `bash wrapper.sh <suite>` runs the wrapper and hands
# it the suite as "$1". Accepting the suite anywhere in argv certified it.
#
# `-o` and `-O` consume the NEXT word, which is why this cannot simply be "the
# first token not starting with `-`": in `bash -euo pipefail <suite>` that rule
# names `pipefail` as the script and reds a correct invocation.
OPT_VALUE_CHARS = "oO"
OPT_VALUE_LONG = {"--rcfile", "--init-file"}


def script_operand(args):
    """The script-file operand of a shell invocation, or None."""
    i = 0
    while i < len(args):
        a = args[i]
        if a == "--":                      # end of options: the next word is it
            return args[i + 1] if i + 1 < len(args) else None
        if a in OPT_VALUE_LONG:
            i += 2
        elif a.startswith("--"):
            i += 1
        elif a.startswith(("-", "+")) and len(a) > 1:
            i += 2 if any(c in OPT_VALUE_CHARS for c in a[1:]) else 1
        else:
            return a
    return None
# Short flags after which a suite path among the arguments is NOT an executed
# script. Both disqualify the invocation, for different reasons:
#   -n  bash READS the script and never runs it (`help set`: "Read commands but
#       do not execute them"), so the assertions never fire.
#   -c  the NEXT argument is the command STRING; the one after it becomes $0, so
#       a suite path there is a name bash reports in errors, not a file it runs.
#       `bash -c 'echo skipped' x.test.sh` prints `skipped` and nothing else.
#   -s  commands are read from STDIN and the path is only a positional parameter.
#       Verified against bash 5.2: `bash -s x.test.sh </dev/null` exits 0 having
#       executed nothing.
# Bundled forms carry them too (`-en`, `-ec`), so the test is per-character.
#
# Deliberately NOT recursed into: a genuine `bash -c "bash x.test.sh"` is
# reported as unwired rather than parsed. That is the fail-CLOSED direction --
# the lint under-certifies and the author rewrites the invocation plainly --
# whereas parsing the string would be a shell interpreter inside a lint.
NO_SCRIPT_FLAG_CHARS = "ncs"
# Long options that make bash PRINT AND EXIT without running a script. The
# short-flag set below covers -n/-c/-s; these are the same idea spelled out, and
# were missed because the generic long-option branch simply skipped them.
NOEXEC_LONG = {"--noexec", "--version", "--help", "--usage",
               "--dump-strings", "--dump-po-strings"}
# Shell keywords and leading VAR=value assignments sit before the real command.
KEYWORDS = {"if", "then", "else", "elif", "while", "until", "do", "!", "time", "exec"}
ASSIGN_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")
# Command separators. `||` and `&&` must precede the single-character class.
SEGMENT_RE = re.compile(r"\|\||&&|[;|&\n]")
# A here-document BODY is data fed to a command, never commands the shell runs.
# `pr-ci.yml` embeds whole python programs this way, so a suite path quoted
# inside one read as an invocation.
HEREDOC_RE = re.compile(r"<<-?\s*(['\"]?)([A-Za-z_][A-Za-z0-9_]*)\1")
# `if: fromJSON(needs.x.outputs.y)` has no equality operator, so the bare-reference
# branch below discarded it -- but fromJSON turns the STRING 'false' into boolean
# false, so it really does gate. Matched first, and its spans are then excluded
# from the bare-reference scan.
FROMJSON_RE = re.compile(
    r"(?P<bang>!\s*)?fromjson\s*\(\s*"
    r"(?P<src>steps|needs)\.(?P<owner>[A-Za-z0-9_-]+)\.outputs\.(?P<name>[A-Za-z0-9_-]+)"
    r"\s*\)"
    # The comparison AROUND the wrapper, if any. Consuming only the wrapper made
    # `fromJSON(...) == false` read as a positive gate -- the inverse-gate bug,
    # re-entered through the branch added to fix it.
    r"(?:\s*(?P<op>==|!=)\s*(?P<val>true|false|'[^']*'))?",
    re.IGNORECASE,
)
# The shared scan selector is one deliberately narrow exception to the normal
# dorny step/job-output contract below. It exists only for workflow-lint's own
# suite-wiring runner: the family bit is composed from the validated policy map,
# and the two adjacent OR clauses make detector failure/missing output select
# the runner. Do not broaden this into a general JSON/expression interpreter.
SELECTION_GATE_RE = re.compile(
    r"(?P<neg>!\s*)?fromjson\s*\(\s*"
    r"needs\.(?P<owner>[A-Za-z0-9_-]+)\.outputs\.(?P<output>[A-Za-z0-9_-]+)\s*\)\s*"
    r"\.\s*advisory\s*\.\s*(?P<family>[A-Za-z0-9_-]+)"
    r"(?![A-Za-z0-9_.-])"
    r"(?:\s*(?P<op>==|!=)\s*(?P<val>true|false|'[^']*'))?",
    re.IGNORECASE,
)
SELECTION_FAIL_CLOSED_RE = re.compile(
    r"needs\.detect-changes\.result\s*!=\s*['\"]success['\"]\s*\|\|\s*"
    r"needs\.detect-changes\.outputs\.scan-known\s*!=\s*['\"]true['\"]\s*\|\|\s*"
    r"(?:fromjson\s*\(\s*needs\.detect-changes\.outputs\.scan-selection\s*\)\s*"
    r"\.\s*hotfix\s*\|\|\s*)?"
    r"fromjson\s*\(\s*needs\.detect-changes\.outputs\.scan-selection\s*\)\s*"
    r"\.\s*advisory\s*\.\s*(?P<family>[A-Za-z0-9_-]+)"
    r"(?![A-Za-z0-9_.-])",
    re.IGNORECASE,
)
HOTFIX_SELECTION_RE = re.compile(
    r"fromjson\s*\(\s*needs\.detect-changes\.outputs\.scan-selection\s*\)\s*"
    r"\.\s*hotfix\b",
    re.IGNORECASE,
)
GATE_RE = re.compile(
    r"(?P<src>steps|needs)\.(?P<owner>[A-Za-z0-9_-]+)\.outputs\.(?P<name>[A-Za-z0-9_-]+)"
    r"(?:\s*(?P<op>==|!=)\s*'(?P<val>[^']*)')?"
)


def glob_to_re(pat):
    """dorny/paths-filter globs -> regex. Handles the forms this repo uses."""
    out, i = [], 0
    while i < len(pat):
        if pat.startswith("**/", i):
            out.append("(?:.*/)?"); i += 3
        elif pat.startswith("**", i):
            out.append(".*"); i += 2
        elif pat[i] == "*":
            out.append("[^/]*"); i += 1
        elif pat[i] == "?":
            out.append("[^/]"); i += 1
        else:
            out.append(re.escape(pat[i])); i += 1
    return re.compile("^" + "".join(out) + "$")


def covered(path, patterns):
    """Ordered evaluation -- a later `!pattern` overrides an earlier match.

    GitHub and dorny both apply path patterns IN ORDER, so `.github/**` followed
    by `!.github/scripts/tests/**` excludes the suite. Returning True on the
    first positive match ignored every negation after it.
    """
    result = False
    for p in patterns:
        neg = p.startswith("!")
        if glob_to_re(p[1:] if neg else p).match(path):
            result = not neg
    return result


def fromjson_positive(m):
    """Whether a fromJSON gate is TRUE when the filter matches."""
    positive = m.group("bang") is None
    op, val = m.group("op"), (m.group("val") or "").strip("'").lower()
    if op:
        positive = positive and ((op == "==") == (val == "true"))
    return positive


SUITE_DIR = ".github/scripts/tests"


def is_suite_path(tok, suite):
    """True when `tok` names THIS suite, by its whole repo-relative path.

    Basename comparison was the previous rule and it collides in both
    directions: it let `not-demo.test.sh` certify `demo.test.sh` (fixed by
    comparing the basename exactly), and it still let a DIFFERENT file with the
    same basename -- `.github/other/demo.test.sh` -- certify the canonical
    suite. Only the full path distinguishes them.

    A token carrying a shell variable cannot be resolved and therefore does not
    match, so such an invocation is reported unwired. That is the fail-closed
    direction; nothing in this repo invokes a suite that way.
    """
    t = (tok or "").strip()
    while t.startswith("./"):
        t = t[2:]
    return t == "%s/%s" % (SUITE_DIR, suite)


def cond_of(node):
    """The `if:` of a job or step, as text.

    `... or ""` is WRONG here and was the bug: YAML parses `if: false` as the
    BOOLEAN False, which is falsy, so the idiom collapsed the one condition that
    matters most to the empty string and the step read as ungated. A present-but-
    falsy value is not an absent one -- the same confusion this lint exists to
    catch, in the lint itself.
    """
    v = node.get("if", None)
    if v is None:
        return ""
    if v is True:
        return "true"
    if v is False:
        return "false"
    return str(v)


# `github.event_name == 'push'` as a CONJUNCT is push-only just as surely as a
# push-only `on:` block, and `gates_in()` sees only the filter reference -- so the
# rest of the condition was dropped and a post-merge step read as a PR gate.
# Only the `==`/`!=` comparison against a literal event is decided; anything else
# is left alone, because flagging every unrecognised conjunct would red correct
# steps rather than fail closed usefully.
EVENT_NAME_RE = re.compile(
    r"github\.event_name\s*(==|!=)\s*['\"]([A-Za-z_]+)['\"]")


def event_conjunct_excludes_pr(expr):
    """True when a conjunct pins the step to a non-pull-request event."""
    e = (expr or "").strip()

    def branch_excludes(part):
        found = EVENT_NAME_RE.findall(part)
        if not found:
            return False                  # undecidable: assume it may admit a PR
        for op, ev in found:
            if op == "==" and ev not in PR_EVENTS:
                return True
            if op == "!=" and ev in PR_EVENTS:
                return True
        return False

    # A disjunction excludes pull requests only when EVERY branch does, and only
    # when every branch is decidable -- `event_name == 'push' || inputs.force`
    # must stay admissible. Returning False for any `||` missed the all-push case.
    parts = [s for s in re.split(r"\|\|", e) if s.strip()]
    if len(parts) > 1:
        return all(branch_excludes(s) for s in parts)
    return branch_excludes(e)


def statically_false(expr):
    """True for an `if:` GitHub can never satisfy.

    `if: false` and `... && false` parse to no filter reference at all, so an
    empty gate list read as "ungated, therefore reachable" -- the opposite of
    what the condition says. Deliberately narrow: recognising literal falsity is
    not the same as evaluating the expression language, and the wider job is not
    a lint's to do.
    """
    e = re.sub(r"^\$\{\{(.*)\}\}$", r"\1", (expr or "").strip()).strip()
    if e.lower() == "false":
        return True
    return bool(re.search(r"&&\s*false\b|\bfalse\s*&&", e, re.I))


AUTO_EVENTS = ("push", "pull_request", "pull_request_target")
PR_EVENTS = ("pull_request", "pull_request_target")
# Only `pull_request` checks out the PROPOSED revision by default.
# `pull_request_target` runs at the BASE commit -- this repo documents exactly
# that in .github/workflows/labeler.yml -- so a suite riding it is testing the
# old copy of itself. Establishing that a workflow re-checks-out the PR head is
# more modelling than the frozen contract wants, so this fails closed instead.
PR_HEAD_EVENTS = ("pull_request",)
# GitHub's default activity types for a pull_request trigger. A workflow that
# narrows `types:` to none of these -- `[closed]`, say -- starts only once the
# code is no longer under review, so it gates nothing.
# Coverage needs BOTH ends: `types: [opened]` misses every later push
# (`synchronize`), and synchronize-only misses the revision the PR opened with.
# There is no PR_CODE_TYPES beside this -- one existed, unread, and a named
# constant with a rationale and no reader misleads the next editor.
PR_OPENING_TYPES = {"opened", "reopened", "ready_for_review"}


def trigger_admits(doc, path):
    """Does the WORKFLOW's own `on:` path filter start a run for `path`?

    A job-level or step-level gate is not the only way an invocation can be
    unreachable: if the workflow's trigger paths exclude the suite, GitHub never
    starts the workflow for a change to it, so the gate check never gets a say.

    Note `doc.get(True)`: PyYAML parses the bare key `on:` as the BOOLEAN True,
    which is the single most common way a workflow parser silently reads every
    trigger as absent.
    """
    on = doc.get("on", doc.get(True))
    # `on: push` and `on: [push]` are the SAME workflow as the mapping form with
    # an empty body -- they simply cannot carry a path filter. Returning early on
    # "no path filter" answered the wrong question once the rule became "which
    # EVENT is this", and let a push-only workflow through in two of its three
    # spellings. Normalise the shape, then ask the one question.
    if isinstance(on, str):
        on = {on: None}
    elif isinstance(on, list):
        on = {ev: None for ev in on if isinstance(ev, str)}
    if not isinstance(on, dict):
        return True                       # unparseable: not this lint's call
    events = [ev for ev in AUTO_EVENTS if ev in on]
    if not events:
        return True                       # dispatch/schedule only: a different question
    # Only a PR-capable event can gate a PULL REQUEST. "One auto event admitting
    # it is enough" let an unrestricted `push:` mask a `pull_request.paths` that
    # excluded the suite: the workflow ran, but only AFTER the merge it should
    # have gated. A push-only workflow has the same defect with nothing to mask
    # it, so an auto-triggered workflow with no PR event does not admit either.
    pr_events = [ev for ev in events if ev in PR_HEAD_EVENTS]
    if not pr_events:
        return False
    for ev in pr_events:
        cfg = on.get(ev)
        if not isinstance(cfg, dict):
            return True                   # `pull_request:` with no body is unrestricted
        types = cfg.get("types")
        if isinstance(types, list) and not (
                (set(types) & PR_OPENING_TYPES) and "synchronize" in set(types)):
            continue                      # misses the opening revision, later pushes, or both
        inc = cfg.get("paths") or []
        exc = cfg.get("paths-ignore") or []
        if not inc and not exc:
            return True
        if (not inc or covered(path, inc)) and not (exc and covered(path, exc)):
            return True
    return False


def join_continuations(text):
    """Fold `\\`-continued physical lines into the one command they are.

    `echo \\` + newline + `bash <suite>` is a single `echo` command with the
    suite as an ARGUMENT, but a per-line loop sees a second line whose command
    is `bash` and certifies it. This is the same mention-vs-invocation confusion
    as the same-line `echo bash <suite>` case, one parser layer down: the
    continuation moves the command boundary, so the boundary has to be restored
    before anything reads command position.
    """
    return re.sub(r"\\\n[ \t]*", " ", text or "")


FUNC_DEF_RE = re.compile(
    r"^[ \t]*(?:function[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\(\)[ \t]*\{", re.M)
# The same header, anchored to a SEGMENT rather than a line, for `inner() { cmd; }`.
FUNC_HEAD_RE = re.compile(
    r"^[ \t]*(?:function[ \t]+)?[A-Za-z_][A-Za-z0-9_]*[ \t]*\(\)[ \t]*\{")


def strip_uncalled_functions(text):
    """Drop the body of any function nothing calls.

    `disabled() { bash <suite>; }` with no call site is a DEFINITION: the shell
    stores it and runs nothing. A per-line loop sees `bash <suite>` at what looks
    like top level and certifies it -- the same mention-vs-invocation confusion
    as `echo bash <suite>`, this time with the compound-command context lost
    rather than the command position.

    Reachability is computed to a fixpoint so a function called only from
    another called function still counts. Dropping a body is the fail-closed
    direction: the suite then reports NO RUNNER rather than being certified.
    """
    defs, pos = [], 0
    while True:
        m = FUNC_DEF_RE.search(text, pos)
        if not m:
            break
        depth, i = 0, m.end() - 1          # sitting on the opening brace
        while i < len(text):
            if text[i] == "{":
                depth += 1
            elif text[i] == "}":
                depth -= 1
                if depth == 0:
                    break
            i += 1
        if depth != 0:
            return text                    # unbalanced: do not guess
        defs.append((m.group(1), m.start(), i + 1))
        pos = i + 1
    if not defs:
        return text

    def without(dead):
        out, last = [], 0
        for _, s, e in dead:
            out.append(text[last:s])
            last = e
        out.append(text[last:])
        return "".join(out)

    live, changed = set(), True
    while changed:
        changed = False
        visible = without([d for d in defs if d[0] not in live])
        for name, _, _ in defs:
            if name in live:
                continue
            # `{` is a command position too: `outer() { inner; }` calls `inner`.
            if re.search(r"(?:^|[;&|({]|\bthen\b|\bdo\b)[ \t]*%s\b" % re.escape(name),
                         visible, re.M):
                live.add(name)
                changed = True
    return without([d for d in defs if d[0] not in live])


def normalize(text):
    """Heredoc bodies dropped, then continued lines joined.

    Order matters twice over: heredoc terminators are matched per physical line,
    so joining first could swallow a terminator that follows a body line ending
    in `\\`; and function bodies are only recognisable once continuations have
    been folded, since a `{` can arrive on a continued line.
    """
    return strip_uncalled_functions(join_continuations(strip_heredoc_bodies(text)))


def strip_heredoc_bodies(text):
    """Drop here-document BODIES, keeping the line that opens them.

    The opening line carries the real command (`cat <<EOF`, `python3 - <<PY`),
    so it stays; everything up to and including the terminator is data. Without
    this, a suite path merely PRINTED by a heredoc counted as a runner -- the
    same mention-vs-invocation confusion as the echo case, one quoting level
    down, and this repo's workflows are full of embedded heredocs.
    """
    lines = (text or "").split("\n")
    out, i = [], 0
    while i < len(lines):
        line = lines[i]
        out.append(line)
        i += 1
        m = HEREDOC_RE.search(line)
        if not m:
            continue
        term = m.group(2)
        while i < len(lines) and lines[i].strip() != term:
            i += 1
        i += 1                       # the terminator line itself is not a command
    return "\n".join(out)


# A `||` handler after an invocation converts the suite's red into a green step
# unless the handler itself fails. Enumerating the SUCCESSFUL commands is
# unbounded (`true`, `:`, `echo`, `printf`, ...), so this recognises the small
# closed set that PROPAGATES and fails closed on everything else -- the shape
# Codex proposed, and the right default for a guard.
#
# But `exit` and `return` propagate only the status they CARRY, and the first
# revision matched the bare keyword: `|| exit 0` therefore wore the shape of the
# fix while discarding exactly what the fix existed to preserve. Only a literal
# `0` is proof of a zero status -- `exit`, `exit $?`, `exit 1` and `exit "$rc"`
# all propagate, and the variable form is a house idiom in this repo
# (.pre-commit-config.yaml), so demanding a nonzero LITERAL would red the tree.
EXIT_TOKEN_RE = re.compile(r"\b(?:exit|return)\b[ \t]*([^\s;&|)}]*)")


CONDITIONAL_HANDLER_RE = re.compile(r"\b(?:if|case|while|until|elif)\b")


def propagates(handler):
    """True when the `||` handler cannot itself exit 0."""
    # An `exit 1` inside `if false; then ... fi` is present and unreachable, and
    # the compound returns 0. Proving which branch runs is the whole halting
    # problem in miniature, so a handler containing any conditional construct is
    # simply not proof -- fail closed, as everywhere else here.
    if CONDITIONAL_HANDLER_RE.search(handler):
        return False
    statuses = [m.group(1).strip().strip("\"'") for m in EXIT_TOKEN_RE.finditer(handler)]
    if statuses:
        # EVERY exit/return must be non-zero. A handler holding both `exit 0`
        # and `exit 1` might take either, so it is not proof -- fail closed.
        return all(s != "0" for s in statuses)
    return handler.strip() == "false"


# The suite's status is CONSUMED by the condition rather than by the shell.
# `[^;]*$` keeps this to the condition itself: in `if foo; then bash <suite>`
# the suite is in ordinary command position and its failure DOES propagate.
COND_HEAD_RE = re.compile(r"(?:^|[;&|(]|\bthen\b|\bdo\b)\s*(?:if|elif|while|until)\b[^;]*$")

# A single `&` -- never `&&` -- backgrounds the command. The asynchronous list
# exits 0 immediately, so the suite's eventual status reaches nothing.
BACKGROUND_RE = re.compile(r"^[^;\n]*?(?<!&)&(?!&)")

# `!` INVERTS the status, so a failing suite becomes a passing command. It is a
# keyword `invokes()` steps over to find the real command -- correctly, because
# the suite does run -- which makes it purely an enforcement question.
NEGATION_RE = re.compile(
    r"(?:^|[;&|(]|\bthen\b|\bdo\b|\bif\b|\belif\b|\bwhile\b|\buntil\b)\s*!\s+[^;|&]*$")


SHORT_CIRCUIT_RE = re.compile(r"(\|\||&&)")


# An unconditional `exit` ends the block; a later line is dead text. Anchored to
# the start of a line so `foo || exit 1` -- conditional -- is not it.
#
# `return` is deliberately NOT here. It is only valid inside a function, and this
# regex is not function-scoped, so `setup() { return 0; }` followed by a real
# invocation reported NO RUNNER -- a false positive on the very shape
# `strip_uncalled_functions` exists because the author expects.
UNCONDITIONAL_EXIT_RE = re.compile(r"^[ \t]*exit\b[^&|]*$")


def reachable_segments(text):
    """Command segments the shell can actually reach.

    BOTH operators short-circuit, in opposite directions: `false && cmd` never
    runs `cmd` because the left side failed, and `true || cmd` never runs it
    because the left side SUCCEEDED. Splitting the block on the operators made
    either right-hand side look like an ordinary invocation. Only the literal
    forms are recognised -- a lint should not try to evaluate arbitrary commands.
    """
    for line in (text or "").split("\n"):
        if UNCONDITIONAL_EXIT_RE.match(line):
            return                         # the rest of the block cannot run
        parts = SHORT_CIRCUIT_RE.split(line)
        i = 0
        while i < len(parts):
            chunk = parts[i]
            op = parts[i + 1] if i + 1 < len(parts) else None
            for seg in re.split(r"[;|&]", chunk):
                yield seg
            head = chunk.strip().lower()
            if op == "&&" and head == "false":
                break                      # rest of THIS line is unreachable
            if op == "||" and head in ("true", ":"):
                break
            i += 2


# GitHub's default step shell is `bash --noprofile --norc -eo pipefail {0}`, so
# errexit AND pipefail are on unless the step says otherwise. `shell: sh` is
# `sh -e {0}` -- errexit only. Anything else is not a shell script this lint reads.
PIPEFAIL_SHELLS = {None, "", "bash"}
# A bare invocation is enforcing ONLY because the shell carries `-e`. That is
# true of the default and of `shell: sh`; a custom template such as
# `shell: bash {0}` starts bash WITHOUT it. Anything not recognised is treated
# as lacking errexit, which fails closed -- the alternative certifies a suite
# whose red the shell was never going to act on.
ERREXIT_SHELLS = {None, "", "bash", "sh"}
# The two halves of this pair MUST accept the same option grammar in opposite
# polarity. They did not: the enable side modelled bundled short options
# (`-eu`), the disable side matched only a bare `+e` -- `\b` fails on `+eu` --
# and its third alternative `-[a-z]*\+` required a leading `-`, so it could only
# ever match `set -x+`, which is not a shell spelling. `set +eu` therefore
# matched NEITHER side and a suite whose errexit was disabled was certified.
SET_NOEXIT_RE = re.compile(r"(?:^|[;&|])\s*set\s+(?:\+[a-z]*e[a-z]*\b|\+o\s+errexit\b)")
SET_ERREXIT_RE = re.compile(r"(?:^|[;&|])\s*set\s+(?:-e\b|-[a-z]*e[a-z]*\b|-o\s+errexit\b)")
# One anchored alternation, not two. The top-level `|` was unparenthesised, so
# the second alternative dropped the `(?:^|[;&|])` anchor every sibling carries
# deliberately -- harmless only because pipefail_at's outer finditer re-imposes
# it before this pattern sees the text, which is a load-bearing accident.
SET_PIPEFAIL_RE = re.compile(r"(?:^|[;&|])\s*set\s+-[a-z]*o\s+pipefail\b")


SET_NOPIPEFAIL_RE = re.compile(r"(?:^|[;&|])\s*set\s+\+[a-z]*o\s+pipefail\b")


def pipefail_at(text, upto, base):
    """Is pipefail in force at character offset `upto`?

    Scanning the WHOLE block answered a different question: with
    `set +o pipefail; bash <suite> | cat; set -o pipefail` the option is off
    when the suite runs, and the later re-enable made the block look safe. The
    errexit check was already offset-scoped; this is its twin and was not.
    """
    on = base
    for m in re.finditer(r"(?:^|[;&|\n])\s*set\s+[^\n;&|]*", text[:upto]):
        frag = m.group(0)
        if SET_NOPIPEFAIL_RE.search(frag):
            on = False
        elif SET_PIPEFAIL_RE.search(frag):
            on = True
    return on


def errexit_off_at(text, upto):
    """Is `set +e` in force at character offset `upto`?

    The last `set` wins, so this is a scan rather than a membership test: a block
    that disables errexit for one command and re-enables it afterwards still
    enforces everything below the re-enable.
    """
    off = False
    for m in re.finditer(r"(?:^|[;&|\n])\s*set\s+[^\n;&|]*", text[:upto]):
        frag = m.group(0)
        if SET_NOEXIT_RE.search(frag):
            off = True
        elif SET_ERREXIT_RE.search(frag):
            off = False
    return off


def in_lossy_pipeline(line, m, pipefail):
    """The suite is upstream in a pipeline whose status it does not set.

    Without `pipefail` a pipeline reports only its LAST command, so
    `bash <suite> | cat` is green whatever the suite did. Being the last element
    is fine either way.
    """
    if pipefail:
        return False
    after = line[m.end():]
    return bool(re.search(r"(?<!\|)\|(?!\|)", after))


def rest_runs_after(body, line):
    """Is there another command after `line` in the block?"""
    tail = body.split(line, 1)[1] if line in body else ""
    return any(s.strip() and not s.strip().startswith("#") for s in tail.split("\n"))


def defaults_shell(node):
    """`defaults: {run: {shell: ...}}` on a workflow or a job, or None."""
    d = node.get("defaults") if isinstance(node, dict) else None
    r = d.get("run") if isinstance(d, dict) else None
    return r.get("shell") if isinstance(r, dict) else None


def resolve_shell(doc, job, st):
    """Step, then job default, then WORKFLOW default -- GitHub's own order.

    The workflow-level default was missing, so a root `defaults.run.shell` of
    `bash {0}` -- which starts bash WITHOUT errexit -- resolved to None and read
    as the safe implicit shell.
    """
    return st.get("shell") or defaults_shell(job) or defaults_shell(doc)


def swallows_failure(text, suite, shell=None):
    """True when the line invoking `suite` discards its exit status.

    A suite whose failure cannot fail the build is exactly as useless as one
    that never runs -- the same silent-success this lint exists for. Any `||`
    handler is the shell half unless it provably propagates; `continue-on-error`
    is the YAML half and is read from the step and job.
    """
    body = normalize(text)
    pipefail_base = shell in PIPEFAIL_SHELLS
    if shell not in ERREXIT_SHELLS and not SET_ERREXIT_RE.search(body):
        return True                       # unknown shell, no explicit `set -e`
    for line in body.split("\n"):
        parts = re.split(r"\|\|", line)
        for k in range(len(parts) - 1):
            # The suite must sit on the LEFT of a `||` for its status to be the
            # one being discarded.
            if is_suite_path_in(parts[k], suite) and not propagates(parts[k + 1]):
                return True

        m = find_suite_token(line, suite)
        if m is None:
            continue
        left, right = line[:m.start()], line[m.end():]
        # `if bash <suite>; then echo passed; fi` -- bash suppresses errexit for
        # a condition, and an `if` whose branch does not run still exits 0.
        if COND_HEAD_RE.search(left) and not propagates(right):
            return True
        # `bash <suite> &` -- backgrounded. A status-propagating `wait` could in
        # principle recover it; none exists in this repo, so that stays closed
        # until a real one appears to be tested against.
        if BACKGROUND_RE.match(right):
            return True
        # `bash <suite> && echo passed` -- errexit exempts the left of an
        # AND-list, so a failing suite only skips the right side. That still
        # fails the step when the AND-list is the block's LAST command, because
        # the script then exits with the list's status. Verified against bash:
        # alone it exits 7, with any command after it exits 0.
        if re.search(r"^\s*&&", right) and not propagates(right) and rest_runs_after(body, line):
            return True
        # `! bash <suite>` -- an exit-7 suite becomes status 0. The SAME rule
        # keeps `if ! bash <suite>; then exit 1; fi` enforcing, because there the
        # negation feeds a branch that propagates.
        if NEGATION_RE.search(left) and not propagates(right):
            return True
        # `bash <suite> | cat` under a shell without pipefail: the pipeline's
        # status is `cat`'s, so the suite's red never leaves the pipe.
        at = body.index(line) + m.start()
        if in_lossy_pipeline(line, m, pipefail_at(body, at, pipefail_base)) and not propagates(right):
            return True
        # `set +e` earlier in the block disables the errexit that makes a bare
        # invocation enforcing at all.
        if errexit_off_at(body, at) and not propagates(right):
            return True
    return False


SHELL_TOKEN_RE = re.compile(r"[^\s;&|()<>]+")


def find_suite_token(line, suite):
    """The match object for `suite` in `line`, or None.

    Tokenising on whitespace alone leaves shell punctuation attached, so
    `if bash <suite>; then` produced the token `<suite>;` -- which matches no
    suite, and silently disabled the conditional-context check below.
    """
    for m in SHELL_TOKEN_RE.finditer(line):
        if is_suite_path(m.group(0), suite):
            return m
    return None


def is_suite_path_in(line, suite):
    return find_suite_token(line, suite) is not None


def invokes(text, suite):
    """True only when `text` actually EXECUTES `suite`.

    Two properties, each of which was a fail-open in an earlier revision:

    COMMAND POSITION, not adjacency. `echo bash x.test.sh` has `bash` sitting
    immediately before the path, so a preceding-token check called it a runner
    -- but the command is `echo` and `bash` is data being printed. Each segment
    is split on shell separators, leading assignments and keywords are skipped,
    and the runner must be the command the segment actually executes.

    EXACT path, not a suffix. `endswith("demo.test.sh")` is also true of
    `not-demo.test.sh`, so invoking one suite certified a different one. Compare
    the basename.
    """
    for seg in reachable_segments(normalize(text)):
        # A one-line definition puts the header in front of the body, so the
        # command reads as `inner()`. Only bodies of CALLED functions survive
        # `strip_uncalled_functions`, so stripping the header here cannot
        # resurrect a definition nothing runs.
        seg = FUNC_HEAD_RE.sub("", seg)
        seg = seg.strip().lstrip("({").strip()
        if not seg or seg.startswith("#"):
            continue
        try:
            toks = shlex.split(seg, comments=True)
        except ValueError:
            toks = seg.split()
        i = 0
        while i < len(toks) and (ASSIGN_RE.match(toks[i]) or toks[i] in KEYWORDS):
            i += 1
        if i >= len(toks):
            continue
        cmd, args = toks[i], toks[i + 1:]
        if is_suite_path(cmd, suite):
            return True                                  # direct execution
        if cmd in RUNNERS and not any(
            a in NOEXEC_LONG
            or (a.startswith("-") and not a.startswith("--")
                and any(c in a[1:] for c in NO_SCRIPT_FLAG_CHARS))
            for a in args
        ):
            operand = script_operand(args)
            if operand is not None and is_suite_path(operand, suite):
                return True
    return False


def gates_in(expr):
    """Every filter-output reference in an `if:`, with how it is tested.

    dorny emits the STRINGS 'true'/'false', and GitHub treats any non-empty
    string as truthy -- so a bare reference is permanently true and the step
    always runs. That over-runs rather than skips, so it is reachable and not a
    problem. `== 'false'` is the opposite and IS one: the step is skipped on
    exactly the changes that require it.
    """
    out, spans = [], []
    for m in SELECTION_GATE_RE.finditer(expr or ""):
        spans.append((m.start(), m.end()))
        op, raw_val = m.group("op"), m.group("val") or ""
        val = raw_val.lower()
        positive = (
            (op is None and m.group("neg") is None)
            or (op == "==" and val == "true" and raw_val == "true"
                and m.group("neg") is None)
            or (op == "!=" and val == "false" and raw_val == "false"
                and m.group("neg") is None)
        )
        if m.group("neg") is not None and op is None:
            positive = False
        out.append({
            "src": "selection", "owner": m.group("owner"),
            "output": m.group("output"), "name": m.group("family"),
            "positive": positive,
            "fail_closed": bool(
                (fm := SELECTION_FAIL_CLOSED_RE.search(expr or ""))
                and fm.group("family").lower() == m.group("family").lower()
            ),
            "raw": m.group(0).strip(),
        })
    if any(g["src"] == "selection" and g["fail_closed"] for g in out):
        # This exact detector-known predicate is part of the validated fallback
        # contract above, not an independently resolvable paths-filter output.
        for m in re.finditer(
                r"needs\.detect-changes\.outputs\.scan-known\s*!=\s*['\"]true['\"]",
                expr or "", re.IGNORECASE):
            spans.append((m.start(), m.end()))
    # A validated PR label is an additional positive reason to run every
    # advisory family on Hotfix work. It is deliberately neutral to coverage:
    # the family selector remains a separate gate and must cover the suite.
    for m in HOTFIX_SELECTION_RE.finditer(expr or ""):
        spans.append((m.start(), m.end()))
    for m in FROMJSON_RE.finditer(expr or ""):
        if any(a <= m.start() and m.end() <= b for a, b in spans):
            continue                                      # handled by narrow selector contract
        spans.append((m.start(), m.end()))
        out.append({
            "src": m.group("src"), "owner": m.group("owner"),
            "name": m.group("name"), "positive": fromjson_positive(m),
            "raw": m.group(0).strip(),
        })
    for m in GATE_RE.finditer(expr or ""):
        if any(a <= m.start() and m.end() <= b for a, b in spans):
            continue                                     # already taken as a fromJSON gate
        op, val = m.group("op"), m.group("val")
        if op is None:
            # A bare reference is always TRUE only if the output exists -- dorny
            # emits the non-empty strings 'true'/'false', both truthy. A MISSING
            # output is the empty string, which is false, so the step never runs.
            # Dropping the reference conflated the two; it is kept so resolve()
            # can reject an unknown producer or name, and `bare` tells the
            # consumer not to pattern-match a gate that is otherwise always true.
            out.append({
                "src": m.group("src"), "owner": m.group("owner"),
                "name": m.group("name"), "positive": True, "bare": True,
                "raw": m.group(0).strip(),
            })
            continue
        positive = (op == "==" and val == "true") or (op == "!=" and val == "false")
        out.append({
            "src": m.group("src"), "owner": m.group("owner"),
            "name": m.group("name"), "positive": positive,
            "raw": m.group(0).strip(),
        })
    return out


suites = sorted(p.name for p in tests_dir.glob("*.test.sh"))
if not suites:
    print("ERROR: no *.test.sh found -- refusing to pass vacuously", file=sys.stderr)
    raise SystemExit(2)

# (workflow, job, step-id) -> {filter-name: [patterns]}.  Keyed by STEP ID because
# `steps.<id>.outputs.<name>` names both, and an earlier revision checked only the
# name -- so a mistyped step id resolved against the right table and certified a
# step GitHub would never run.
filters = {}
# Keys whose PRODUCER step can never run. A filter step behind `if: false`
# publishes no outputs, so every consumer gated on `steps.<id>.outputs.<name>`
# is skipped -- and recording its declared filters made the gate look satisfiable.
dead_filters = set()
# (workflow, job) -> {job-output-name: raw expression}
job_outputs = {}
# suite -> [(workflow, job, [gate, ...])]
runners = {}

# Both extensions: GitHub Actions accepts either, and `.pre-commit-config.yaml`
# already gates on `^\.github/workflows/.*\.ya?ml$`. A `.yaml` workflow holding a
# runner or a paths-filter was invisible here.
for wf in sorted(set(wf_dir.glob("*.yml")) | set(wf_dir.glob("*.yaml"))):
    try:
        doc = yaml.safe_load(wf.read_text())
    except yaml.YAMLError as exc:
        print("ERROR: %s: %s" % (wf.name, exc), file=sys.stderr)
        raise SystemExit(2)
    if not isinstance(doc, dict):
        continue
    for job_name, job in (doc.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        outs = job.get("outputs") or {}
        if isinstance(outs, dict):
            job_outputs[(wf.name, job_name)] = {k: str(v) for k, v in outs.items()}
        steps = job.get("steps") or []
        for st in steps:
            if not isinstance(st, dict) or "paths-filter" not in str(st.get("uses", "")):
                continue
            raw = (st.get("with") or {}).get("filters", "") or ""
            cur = None
            fkey = (wf.name, job_name, str(st.get("id", "")))
            if (statically_false(cond_of(job)) or statically_false(cond_of(st))
                    or event_conjunct_excludes_pr(cond_of(job))
                    or event_conjunct_excludes_pr(cond_of(st))):
                dead_filters.add(fkey)
            table = filters.setdefault(fkey, {})
            for line in raw.split("\n"):
                if not line.strip() or line.strip().startswith("#"):
                    continue
                if not line[0].isspace() and line.rstrip().endswith(":"):
                    cur = line.strip().rstrip(":")
                    table.setdefault(cur, [])
                elif cur is not None:
                    m = re.search(r"'([^']+)'|\"([^\"]+)\"", line)
                    if m:
                        table[cur].append(m.group(1) or m.group(2))
                    else:
                        # dorny accepts UNQUOTED patterns and this read only
                        # quoted ones, so `- .github/scripts/**` was dropped
                        # silently -- producing a diagnostic that told the author
                        # to add a path already listed, which is the one thing a
                        # fail-closed gap is supposed not to do.
                        u = re.match(r"\s*-\s*(\S.*?)\s*$", line)
                        if u:
                            table[cur].append(u.group(1))
        job_gates = gates_in(cond_of(job))
        for st in steps:
            if not isinstance(st, dict):
                continue
            body = str(st.get("run", "") or "")
            if not body:
                continue
            # A step runs only when BOTH its own condition and its job's hold, so
            # they are conjunctive and any one of them can do the skipping. Reading
            # step conditions alone certified a suite whose whole JOB was gated on
            # an upstream filter that omitted it.
            all_gates = job_gates + gates_in(cond_of(st))
            if statically_false(cond_of(job)) or statically_false(cond_of(st)):
                dead = ("an `if:` that is statically false, so GitHub can never "
                        "run the step")
            elif (event_conjunct_excludes_pr(cond_of(job))
                  or event_conjunct_excludes_pr(cond_of(st))):
                dead = ("an `if:` conjunct pinning it to a non-pull-request event, "
                        "so it runs only AFTER merge and gates no pull request")
            else:
                dead = None
            soft = bool(st.get("continue-on-error")) or bool(job.get("continue-on-error"))
            for suite in suites:
                if invokes(body, suite):
                    runners.setdefault(suite, []).append(
                        (wf.name, job_name, all_gates, doc, dead,
                         soft or swallows_failure(body, suite,
                                                  resolve_shell(doc, job, st))))


def resolve(wf_name, job_name, gate):
    """Patterns a gate admits, or an explanation of why it can never be true."""
    if not gate["positive"]:
        return None, ("tested as `%s`, so the step is SKIPPED on exactly the "
                      "changes that match the filter" % gate["raw"])
    if gate["src"] == "selection":
        if (wf_name != "pr-ci.yml" or job_name != "workflow-lint"
                or gate["owner"] != "detect-changes"
                or gate["output"] != "scan-selection"
                or gate["name"] != "workflow_lint"):
            return None, "is not the supported workflow-lint selector source"
        if not gate.get("fail_closed"):
            return None, "does not include the fail-closed detector fallback"
        policy_path = root / ".github" / "ci" / "scan-relevance.yml"
        try:
            policy = yaml.safe_load(policy_path.read_text())
        except (OSError, yaml.YAMLError) as exc:
            return None, "cannot read the shared relevance policy: %s" % exc
        patterns = policy.get("workflow_lint") if isinstance(policy, dict) else None
        if not isinstance(patterns, list) or not all(isinstance(p, str) for p in patterns):
            return None, "shared relevance policy has no valid workflow_lint selector"
        required = (
            ".github/scripts/ci-scan-relevance.mjs",
            ".github/ci/scan-relevance.yml",
        )
        missing = [p for p in required if not covered(p, patterns)]
        if missing:
            return None, "workflow_lint selector omits required source(s): %s" % ", ".join(missing)
        return patterns, None
    if gate["src"] == "steps":
        key = (wf_name, job_name, gate["owner"])
        if key not in filters:
            return None, ("no paths-filter step with id '%s' in job '%s' -- step "
                          "outputs are job-local, so this is permanently false"
                          % (gate["owner"], job_name))
        if key in dead_filters:
            return None, ("the paths-filter step '%s' in job '%s' is behind an "
                          "`if:` that is statically false, so it publishes no "
                          "outputs and every step gated on them is skipped"
                          % (gate["owner"], job_name))
        table = filters[key]
        if gate["name"] not in table:
            return None, ("step '%s' declares no filter named '%s' (declares: %s)"
                          % (gate["owner"], gate["name"], sorted(table) or "none"))
        return table[gate["name"]], None
    outs = job_outputs.get((wf_name, gate["owner"]))
    if outs is None:
        return None, "no job named '%s' to take outputs from" % gate["owner"]
    expr = outs.get(gate["name"])
    if expr is None:
        return None, ("job '%s' declares no output '%s' (declares: %s)"
                      % (gate["owner"], gate["name"], sorted(outs) or "none"))
    m = re.search(r"steps\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+)", expr)
    if not m:
        return None, ("output '%s.%s' does not come from a paths-filter step"
                      % (gate["owner"], gate["name"]))
    key = (wf_name, gate["owner"], m.group(1))
    if key in dead_filters:
        return None, ("output '%s.%s' resolves to a paths-filter step behind an "
                      "`if:` that is statically false, so it publishes nothing"
                      % (gate["owner"], gate["name"]))
    if key not in filters or m.group(2) not in filters[key]:
        return None, ("output '%s.%s' resolves to steps.%s.outputs.%s, which no "
                      "paths-filter step in job '%s' declares"
                      % (gate["owner"], gate["name"], m.group(1), m.group(2), gate["owner"]))
    return filters[key][m.group(2)], None


pc_hooks = []         # (entry, files regex|None, exclude regex|None)
pre_commit = root / ".pre-commit-config.yaml"
if pre_commit.is_file():
    try:
        pc = yaml.safe_load(pre_commit.read_text()) or {}
    except yaml.YAMLError as exc:
        print("ERROR: .pre-commit-config.yaml: %s" % exc, file=sys.stderr)
        raise SystemExit(2)
    # pre-commit applies the CONFIG-LEVEL files/exclude to every hook on top of
    # the hook's own, so a global `files: '^docs/'` disables an otherwise
    # ungated hook entirely. Reading only the hook's filters missed that.
    top_files, top_exclude = pc.get("files"), pc.get("exclude")
    for repo in (pc.get("repos") or []):
        for hook in (repo.get("hooks") or []):
            pc_hooks.append((str(hook.get("entry", "") or ""),
                             [f for f in (top_files, hook.get("files")) if f],
                             [x for x in (top_exclude, hook.get("exclude")) if x]))


def pc_reaches(files_res, exclude_res, path):
    """Every `files` must match and no `exclude` may, config level and hook level."""
    try:
        for f in files_res:
            if not re.search(f, path):
                return False
        for x in exclude_res:
            if re.search(x, path):
                return False
    except re.error:
        return True                  # unparseable pattern: do not invent a failure
    return True


problems = []
for suite in suites:
    test_path = ".github/scripts/tests/%s" % suite
    stem = suite[: -len(".test.sh")]
    # The script under test is X.sh, plus the snake_case X.py beside it when X.sh
    # keeps its logic there (promote_unreleased_changelog.py). Either one changing
    # alone must still run the suite.
    impl_paths = [p for p in (".github/scripts/%s.sh" % stem,
                              ".github/scripts/%s.py" % stem.replace("-", "_"))
                  if (root / p).is_file()]

    where = runners.get(suite, [])
    hooks = [h for h in pc_hooks if invokes(h[0], suite)]
    before = len(problems)
    reachable = False

    for wf_name, job_name, gate_list, wf_doc, dead, soft in where:
        if soft:
            problems.append(
                "%s: THIS invocation in %s (%s) cannot fail the build "
                "(`continue-on-error`, or the status is discarded) -- an "
                "invocation whose red goes nowhere is as silent as one that "
                "never ran. Another site may still enforce it; this one does not."
                % (suite, wf_name, job_name))
            continue
        if dead:
            problems.append(
                "%s: invoked in %s (%s) behind %s." % (suite, wf_name, job_name, dead))
            continue
        trigger_bad = [p for p in [test_path] + impl_paths
                       if not trigger_admits(wf_doc, p)]
        if trigger_bad:
            problems.append(
                "%s: invoked in %s (%s), but that workflow's own `on:` triggers "
                "do not start a PULL-REQUEST run for %s -- the job gate never "
                "gets a say, and a post-merge run gates nothing."
                % (suite, wf_name, job_name, " or ".join(trigger_bad)))
            continue
        if not gate_list:
            reachable = True
            continue
        ok = True
        for gate in gate_list:
            pats, why = resolve(wf_name, job_name, gate)
            if gate.get("bare") and not why:
                continue                  # declared output, bare test: always true
            if why:
                ok = False
                problems.append("%s: invoked in %s (%s) behind `%s` -- %s."
                                % (suite, wf_name, job_name, gate["raw"], why))
                continue
            if not covered(test_path, pats):
                ok = False
                filter_label = " (%s)" % gate["name"] if gate["src"] == "selection" else ""
                problems.append(
                    "%s: invoked in %s (%s) behind `%s`, but %s matches no pattern "
                    "in that filter%s -- a PR changing only this suite fast-exits "
                    "before running it." % (suite, wf_name, job_name, gate["raw"],
                                            test_path, filter_label))
            for impl_path in impl_paths:
                if not covered(impl_path, pats):
                    ok = False
                    problems.append(
                        "%s: its implementation %s matches no pattern in the filter "
                        "behind `%s` (%s/%s) -- a PR changing only the script under "
                        "test would not run the suite."
                        % (suite, impl_path, gate["raw"], wf_name, job_name))
        if ok:
            reachable = True

    for entry, files_re, exclude_re in hooks:
        if pc_reaches(files_re, exclude_re, test_path):
            reachable = True
        else:
            problems.append(
                "%s: its pre-commit hook does not match %s (files=%r exclude=%r) "
                "-- the hook exists but never fires for the suite."
                % (suite, test_path, files_re, exclude_re))
        for impl_path in impl_paths:
            if not pc_reaches(files_re, exclude_re, impl_path):
                problems.append(
                    "%s: its pre-commit hook does not match %s (files=%r exclude=%r) "
                    "-- editing the script under test would not run its suite."
                    % (suite, impl_path, files_re, exclude_re))

    if not where and not hooks:
        problems.append(
            "%s: NO RUNNER -- invoked by no workflow and no pre-commit hook, so "
            "it has never executed anywhere." % suite)
    elif not reachable and len(problems) == before:
        problems.append("%s: has a runner, but nothing that can reach it." % suite)

if problems:
    print("lint-test-suite-wiring: %d problem(s)\n" % len(problems), file=sys.stderr)
    for pr in problems:
        print("  " + pr, file=sys.stderr)
    print(
        "\nFix by adding the path(s) to the filter, adding a real invocation, or "
        "both. Wiring one without the other is the defect this lint exists for.",
        file=sys.stderr,
    )
    raise SystemExit(1)

print("lint-test-suite-wiring: %d suite(s) wired and reachable." % len(suites))
