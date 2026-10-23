#!/usr/bin/env bash
#
# Regression test for scripts/security/check-pin-exceptions.mjs.
#
# This suite exists because review on PR #2769 (Gitar) caught that the lint
# shipped as a commit-gating security control with NO committed tests — its
# sibling override-range-lint has 23. Mutation checks run by hand are not a
# control; they leave with the session.
#
# It locks the contract: an undocumented pin or ignore blocks, a malformed entry
# blocks, an expired review_by only warns, and — the two cases review found —
# an `ignore:` key with a trailing comment is still seen, and an ignore is
# matched against the workspace it actually belongs to.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LINT="$SCRIPT_DIR/check-pin-exceptions.mjs"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0

assert() {
  local name="$1" got="$2" want="$3" out="$4" pat="${5:-}"
  if [ "$got" -ne "$want" ]; then
    echo "FAIL: $name — expected exit $want, got $got"
    printf '    %s\n' "${out//$'\n'/$'\n'    }"
    fail=$((fail + 1))
    return
  fi
  if [ -n "$pat" ] && ! grep -qE "$pat" <<<"$out"; then
    echo "FAIL: $name — output missing /$pat/"
    printf '    %s\n' "${out//$'\n'/$'\n'    }"
    fail=$((fail + 1))
    return
  fi
  echo "ok:   $name"
  pass=$((pass + 1))
}

# Every workspace the checker covers, read FROM THE MODULE so this suite cannot
# hold a stale parallel copy. The scope-drift assertion compares WORKSPACES against
# the npm directories dependabot.yml declares, so a fixture that omits any of them
# fails every case on `stale` — which is exactly what happened when this was first
# written against a hardcoded three-entry list. Derive, never duplicate.
ALL_WS="$(node -e "import('$LINT').then((m) => console.log(m.WORKSPACES.join('\n')))")"
[ -n "$ALL_WS" ] || { echo "FAIL: could not read WORKSPACES from $LINT"; exit 1; }

# The npm update blocks a fixture needs so the drift assertion is satisfied. Appended
# to every fixture's dependabot.yml; duplicate directories are harmless (the parser
# returns a Set).
npm_blocks() {
  local ws
  while IFS= read -r ws; do
    [ -n "$ws" ] && printf '  - package-ecosystem: "npm"\n    directory: "/%s"\n' "$ws"
  done <<<"$ALL_WS"
}

# mkroot <name> <register-json> <dependabot-yml> [desktop-package-json]
mkroot() {
  local name="$1" reg="$2" dbot="$3" pkg="${4:-}"
  local d="$WORK/$name"
  local default_pkg='{"name":"d"}'
  mkdir -p "$d/.github" "$d/client/desktop" "$d/client/admin" "$d/services/media-plane"
  printf '%s\n' "$reg"  > "$d/.github/dependency-pin-exceptions.json"
  { printf '%s\n' "$dbot"; npm_blocks; } > "$d/.github/dependabot.yml"
  printf '%s\n' "${pkg:-$default_pkg}" > "$d/client/desktop/package.json"
  printf '%s\n' '{"name":"a"}' > "$d/client/admin/package.json"
  printf '%s\n' '{"name":"m"}' > "$d/services/media-plane/package.json"
  echo "$d"
}

run() {
  local name="$1" dir="$2" want="$3" pat="${4:-}"
  local out got
  out="$(PIN_LINT_NOW=2026-08-15T00:00:00Z node "$LINT" "$dir" 2>&1)"
  got=$?
  assert "$name" "$got" "$want" "$out" "$pat"
}

EMPTY_REG='{"exceptions":[]}'
ONE_REG='{"exceptions":[{"kind":"exact-pin","ecosystem":"npm","workspace":"client/desktop","package":"jose","pinned":"6.2.8","reason":"r","review_by":"2027-01-01"}]}'
# An ignore: rule requires kind=held-back specifically (case 12). The fixtures
# below use it so the workspace and multi-directory assertions test the thing
# they name, rather than incidentally depending on kind the way they used to.
ONE_HELD='{"exceptions":[{"kind":"held-back","ecosystem":"npm","workspace":"client/desktop","package":"jose","pinned":"6.2.8","available":"7.0.0","reason":"r","review_by":"2027-01-01"}]}'
BOTH_HELD='{"exceptions":[
  {"kind":"held-back","ecosystem":"npm","workspace":"client/desktop","package":"jose","pinned":"6.2.8","available":"7.0.0","reason":"r","review_by":"2027-01-01"},
  {"kind":"held-back","ecosystem":"npm","workspace":"client/admin","package":"jose","pinned":"6.2.8","available":"7.0.0","reason":"r","review_by":"2027-01-01"}
]}'
NO_IGNORES='version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/client/desktop"'

# ── 1. Clean tree.
D=$(mkroot clean "$EMPTY_REG" "$NO_IGNORES")
run "passes when there is nothing held" "$D" 0 "every pin and ignore accounted for"

# ── 2. An exact pin with no register entry blocks.
D=$(mkroot undocpin "$EMPTY_REG" "$NO_IGNORES" '{"name":"d","dependencies":{"jose":"6.2.8"}}')
run "blocks an undocumented exact pin"   "$D" 1 "exact pin\(s\) with no register entry"
run "names the offending package"        "$D" 1 "jose@6.2.8"

# ── 3. Documented pin passes; a caret range is not a pin at all.
D=$(mkroot docpin "$ONE_REG" "$NO_IGNORES" '{"name":"d","dependencies":{"jose":"6.2.8"}}')
run "passes once the pin is documented"  "$D" 0 "1 documented exception"
D=$(mkroot rangedep "$EMPTY_REG" "$NO_IGNORES" '{"name":"d","dependencies":{"jose":"^6.2.8"}}')
run "a caret range is not an exact pin"  "$D" 0 "accounted for"

# ── 4. An undocumented dependabot ignore blocks.
IGN='version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/client/desktop"
    ignore:
      - dependency-name: "left-pad"
        versions: [">=2.0.0"]'
D=$(mkroot undocign "$EMPTY_REG" "$IGN")
run "blocks an undocumented ignore rule" "$D" 1 "ignore rule\(s\) with no register entry"

# ── 5. TRAILING COMMENT on the ignore key (Gitar, PR #2769).
#     `/^\s*ignore:\s*$/` missed this, making the whole block invisible — a
#     fail-OPEN in a lint whose purpose is to refuse silent holds.
IGN_COMMENT='version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/client/desktop"
    ignore:  # temporary, see #1234
      - dependency-name: "left-pad"
        versions: [">=2.0.0"]'
D=$(mkroot igncomment "$EMPTY_REG" "$IGN_COMMENT")
run "sees an ignore: key with a trailing comment" "$D" 1 "left-pad"

# ── 6. WORKSPACE-SCOPED ignore matching (CodeRabbit, PR #2769, Major).
#     A register entry for client/desktop must NOT satisfy an ignore rule that
#     lives in the client/admin update block.
IGN_ADMIN='version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/client/admin"
    ignore:
      - dependency-name: "jose"
        versions: [">=7.0.0"]'
D=$(mkroot wrongws "$ONE_REG" "$IGN_ADMIN")
run "a desktop entry does not cover an admin ignore" "$D" 1 "jose \(in client/admin\)"
IGN_DESKTOP='version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/client/desktop"
    ignore:
      - dependency-name: "jose"
        versions: [">=7.0.0"]'
D=$(mkroot rightws "$ONE_HELD" "$IGN_DESKTOP")
run "the matching workspace does cover it"          "$D" 0 "accounted for"

# ── 7. Malformed entries block.
BAD_KIND='{"exceptions":[{"kind":"whatever","ecosystem":"npm","workspace":"client/desktop","package":"x","pinned":"1.0.0","reason":"r","review_by":"2027-01-01"}]}'
D=$(mkroot badkind "$BAD_KIND" "$NO_IGNORES")
run "rejects an unknown kind" "$D" 1 "kind must be one of"

MISSING='{"exceptions":[{"kind":"exact-pin","ecosystem":"npm","workspace":"client/desktop","package":"x","pinned":"1.0.0","review_by":"2027-01-01"}]}'
D=$(mkroot missing "$MISSING" "$NO_IGNORES")
run "rejects a missing reason" "$D" 1 'missing or empty "reason"'

HELD_NO_AVAIL='{"exceptions":[{"kind":"held-back","ecosystem":"npm","workspace":"client/desktop","package":"x","pinned":"1.0.0","reason":"r","review_by":"2027-01-01"}]}'
D=$(mkroot heldnoavail "$HELD_NO_AVAIL" "$NO_IGNORES")
run "held-back must name the refused version" "$D" 1 'requires "available"'

# ── 8. INVALID CALENDAR DATE (CodeRabbit, PR #2769).
#     A shape-only check accepted 2026-13-45, an expiry that can never fire.
BAD_DATE='{"exceptions":[{"kind":"exact-pin","ecosystem":"npm","workspace":"client/desktop","package":"x","pinned":"1.0.0","reason":"r","review_by":"2026-13-45"}]}'
D=$(mkroot baddate "$BAD_DATE" "$NO_IGNORES")
run "rejects an impossible calendar date" "$D" 1 "must be a real YYYY-MM-DD date"

# ── 9. Expiry WARNS but never blocks — a wall-clock date must not red-gate CI.
EXPIRED='{"exceptions":[{"kind":"exact-pin","ecosystem":"npm","workspace":"client/desktop","package":"jose","pinned":"6.2.8","reason":"r","review_by":"2020-01-01"}]}'
D=$(mkroot expired "$EXPIRED" "$NO_IGNORES" '{"name":"d","dependencies":{"jose":"6.2.8"}}')
run "an expired review_by warns"            "$D" 0 "WARN  review_by 2020-01-01 has passed"
run "...and does not block"                 "$D" 0 "past review_by \(warning only\)"

# ── 10. Fail closed on a missing/broken register.
D="$WORK/noreg"; mkdir -p "$D/.github"
run "exits 2 when the register is absent" "$D" 2 "register not found"
D=$(mkroot badjson '{ not json' "$NO_IGNORES")
run "exits 2 on an unparseable register"  "$D" 2 "cannot read"

# ── 11. MULTI-DIRECTORY BLOCKS (CodeRabbit, PR #2769).
#     A block may declare `directories:` (a list) instead of `directory:`, and
#     one ignore rule then applies to every entry. Keeping only the first meant
#     the rule was demanded for one directory and unchecked in the rest — the
#     fail-OPEN shape this lint exists to refuse. The multi-directory `docker`
#     block carries no ignore today, so the defect was latent; it would have
#     opened the moment one was added, which is exactly when it matters.
MULTI_DIR='version: 2
updates:
  - package-ecosystem: "docker"
    directories:
      - "/client/desktop"
      - "/client/admin"
    ignore:
      - dependency-name: "jose"
        versions: [">=7.0.0"]'

# ONE_REG documents client/desktop only. The second directory must still block.
D=$(mkroot multidir1 "$ONE_REG" "$MULTI_DIR")
run "a multi-directory ignore is demanded for EVERY directory" "$D" 1 "jose \(in client/admin\)"

BOTH_REG='{"exceptions":[
  {"kind":"exact-pin","ecosystem":"npm","workspace":"client/desktop","package":"jose","pinned":"6.2.8","reason":"r","review_by":"2027-01-01"},
  {"kind":"exact-pin","ecosystem":"npm","workspace":"client/admin","package":"jose","pinned":"6.2.8","reason":"r","review_by":"2027-01-01"}
]}'
D=$(mkroot multidir2 "$BOTH_HELD" "$MULTI_DIR")
run "...and passes once every directory is documented" "$D" 0 "accounted for"

#     `labels:` and `patterns:` are `- "..."` lines too. Tracking the list
#     explicitly is what stops them being read as directories; a bare
#     list-item match would invent a "area: deps" workspace and demand it.
LIST_SIBLINGS='version: 2
updates:
  - package-ecosystem: "docker"
    directories:
      - "/client/desktop"
      - "/client/admin"
    labels:
      - "area: deps"
    groups:
      base-images:
        patterns:
          - "*"
    ignore:
      - dependency-name: "jose"
        versions: [">=7.0.0"]'
D=$(mkroot multidir3 "$BOTH_HELD" "$LIST_SIBLINGS")
run "a sibling labels/patterns list is not mistaken for a directory" "$D" 0 "accounted for"

# ── 12. AN IGNORE NEEDS kind=held-back, NOT JUST ANY ENTRY (PR #3199 review,
#     security-reviewer, MEDIUM). Matching on workspace+package alone let the
#     pre-existing exact-pin for @electron/fuses silently discharge a NEW
#     ignore's obligation: the register kept asserting "being at latest is
#     normal" while 2.1.3 had been refused since 2026-06-29, and the pin-shaped
#     review_by that would have surfaced it was a year out. Third instance of
#     the same fail-OPEN shape as 5 and 11 — the lint passing for the wrong
#     reason. An exact-pin cannot express a hold: no `available`, nothing decays.
D=$(mkroot pinvsignore "$ONE_REG" "$IGN_DESKTOP")
run "an exact-pin entry does not satisfy an ignore rule" "$D" 1 'not kind="held-back"'
run "...and says so, rather than reporting it missing"   "$D" 1 "entry exists but"
D=$(mkroot heldignore "$ONE_HELD" "$IGN_DESKTOP")
run "a held-back entry does satisfy an ignore rule"      "$D" 0 "accounted for"

# ── 13. THE #3292 DEFECT ITSELF: a covered workspace outside client/* and
#     services/*. Before WORKSPACES named the Cloudflare Workers, both exact-pinned
#     wrangler with no register entry and the lint reported "every pin and ignore
#     accounted for" — a claim about the repository that held only for three
#     directories. This asserts a worker manifest is now READ, which is the whole
#     fix; the two cases below assert it cannot silently stop being read.
WORKER_WS='[internal]workers/apple-sso-bridge'
D=$(mkroot workerpin "$EMPTY_REG" "$NO_IGNORES")
mkdir -p "$D/$WORKER_WS"
printf '%s\n' '{"name":"w","devDependencies":{"wrangler":"4.131.1"}}' > "$D/$WORKER_WS/package.json"
run "reads a worker manifest's exact pin"      "$D" 1 "exact pin\(s\) with no register entry"
run "names the worker pin, not just a count"   "$D" 1 "wrangler@4.131.1"

# ── 14. SCOPE DRIFT, both directions. This is the assertion that makes case 13
#     durable: without it, WORKSPACES and dependabot.yml can part company and the
#     lint goes on passing. `uncovered` is the #3292 shape — dependabot proposes
#     bumps for a directory the control never reads. `stale` is the inverse and is
#     easy to miss, because the pin loop's existsSync guard skips a mistyped path
#     silently, so a typo covers nothing and still exits 0.
DRIFT_EXTRA='version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/services/brand-new-thing"'
D=$(mkroot driftuncovered "$EMPTY_REG" "$DRIFT_EXTRA")
run "blocks an npm directory outside WORKSPACES" "$D" 1 "dependabot watches but this control does not read"
run "names the uncovered directory"             "$D" 1 "services/brand-new-thing"

# mkroot_raw writes dependabot.yml VERBATIM — no npm_blocks append — so a fixture
# can omit workspaces the checker covers and exercise the `stale` direction.
mkroot_raw() {
  local name="$1" reg="$2" dbot="$3"
  local d="$WORK/$name"
  mkdir -p "$d/.github"
  printf '%s\n' "$reg"  > "$d/.github/dependency-pin-exceptions.json"
  printf '%s\n' "$dbot" > "$d/.github/dependabot.yml"
  echo "$d"
}
D=$(mkroot_raw driftstale "$EMPTY_REG" "$NO_IGNORES")
run "blocks a WORKSPACES entry nothing watches" "$D" 1 "no npm update block watches"
run "names the unwatched workspace"             "$D" 1 "services/media-plane"

# ── 15. The drift parser reads a `directories:` LIST, not only a scalar
#     `directory:`. The docker blocks use the list form, so a parser that handled
#     only the scalar would report every listed npm directory as uncovered the
#     first time someone groups npm directories that way.
DRIFT_LIST='version: 2
updates:
  - package-ecosystem: "npm"
    directories:
      - "/client/desktop"
      - "/client/admin"
      - "/services/media-plane"
      - "/.github/tools/license-checker"
      - "/[internal]workers/apple-sso-bridge"
      - "/[internal]workers/invite-landing"
      - "/dev-tooling/concord-bridge"'
D=$(mkroot_raw driftlist "$EMPTY_REG" "$DRIFT_LIST")
run "accepts a directories: list as coverage"   "$D" 0 "accounted for"

# ── 16. A non-npm ecosystem's directory is NOT treated as npm coverage. Without
#     the ecosystem filter, the gomod and docker blocks would satisfy the drift
#     check for directories npm never watches.
DRIFT_GOMOD='version: 2
updates:
  - package-ecosystem: "gomod"
    directory: "/services/control-plane"'
D=$(mkroot_raw driftgomod "$EMPTY_REG" "$DRIFT_GOMOD")
run "a gomod block is not npm coverage"        "$D" 1 "no npm update block watches"

# ── 17. A MISSING dependabot.yml must fail closed, not pass. Gitar caught this on
#     PR #3325: `drift` was populated only inside `if (existsSync(dbPath))`, so an
#     absent config left the scope comparison unrun AND every ignore: obligation
#     unchecked, while the run still printed "every pin and ignore accounted for".
#     Absence was indistinguishable from a pass — the same false-green shape the
#     whole file exists to refuse. Symmetric with the missing-register case below.
D="$WORK/nodbot"; mkdir -p "$D/.github"
printf '%s\n' "$EMPTY_REG" > "$D/.github/dependency-pin-exceptions.json"
run "fails closed when dependabot.yml is absent" "$D" 2 "dependabot config not found"

# ── 18. `directories:` has two valid YAML spellings and Dependabot accepts both.
#     The parser matched only the block style, so a FLOW sequence parsed as
#     absent — not as an error — and every watched directory reported as drift.
#     A false exit 1 here blocks commits, which is why this is a case and not a
#     note (CodeRabbit, PR #3325).
DRIFT_FLOW='version: 2
updates:
  - package-ecosystem: "npm"
    directories: ["/client/desktop", "/client/admin", "/services/media-plane", "/.github/tools/license-checker", "/[internal]workers/apple-sso-bridge", "/[internal]workers/invite-landing", "/dev-tooling/concord-bridge"]'
D=$(mkroot_raw driftflow "$EMPTY_REG" "$DRIFT_FLOW")
run "accepts a flow-style directories list"    "$D" 0 "accounted for"

# ── 19. …and the same list spread over several lines, which is equally valid YAML
#     and is what a formatter produces once the single line grows too long.
DRIFT_FLOW_ML='version: 2
updates:
  - package-ecosystem: "npm"
    directories: [
      "/client/desktop",
      "/client/admin",
      "/services/media-plane",
      "/.github/tools/license-checker",
      "/[internal]workers/apple-sso-bridge",
      "/[internal]workers/invite-landing",
      "/dev-tooling/concord-bridge"
    ]'
D=$(mkroot_raw driftflowml "$EMPTY_REG" "$DRIFT_FLOW_ML")
run "accepts a multi-line flow sequence"       "$D" 0 "accounted for"

# ── 20. A shape the parser cannot read must exit 2, not 1. Exit 1 is this lint's
#     "findings" code, so returning [] for an unreadable config would report a
#     config it could not parse as a pin problem — and an uncaught throw would say
#     so with a stack trace. "No directories" and "I could not read the
#     directories" are the two claims this file exists to keep apart.
DRIFT_BAD='version: 2
updates:
  - package-ecosystem: "npm"
    directories: "/client/desktop"'
D=$(mkroot_raw driftbadshape "$EMPTY_REG" "$DRIFT_BAD")
run "an unreadable directories: shape exits 2" "$D" 2 "directories.*not a non-empty sequence"

# ── 21. QUOTED KEYS and FLOW MAPPINGS are valid YAML that Dependabot accepts.
#     The regex scanner matched only bare keys in block mappings, so neither form
#     identified the update block: the lint saw npm watching nothing and reported
#     every WORKSPACES entry as stale — exit 1, blocking the commit. Supporting
#     the flow SEQUENCE value form did not help, because the problem was the
#     MAPPING around it. That is what ended the hand-rolled parser (CodeRabbit).
DRIFT_QUOTED="version: 2
updates:
  - 'package-ecosystem': 'npm'
    'directories':
      - '/client/desktop'
      - '/client/admin'
      - '/services/media-plane'
      - '/.github/tools/license-checker'
      - '/[internal]workers/apple-sso-bridge'
      - '/[internal]workers/invite-landing'
      - '/dev-tooling/concord-bridge'"
D=$(mkroot_raw driftquoted "$EMPTY_REG" "$DRIFT_QUOTED")
run "quoted mapping keys are read"             "$D" 0 "accounted for"

# ── 22. The same update written as a flow mapping.
DRIFT_FLOWMAP='version: 2
updates:
  - { package-ecosystem: npm, directories: ["/client/desktop", "/client/admin", "/services/media-plane", "/.github/tools/license-checker", "/[internal]workers/apple-sso-bridge", "/[internal]workers/invite-landing", "/dev-tooling/concord-bridge"] }'
D=$(mkroot_raw driftflowmap "$EMPTY_REG" "$DRIFT_FLOWMAP")
run "a flow mapping update block is read"      "$D" 0 "accounted for"

# ── 23. THE FAIL-OPEN. `ignore: not-a-list` is valid YAML, so `Array.isArray`
#     was false, the ignore rule VANISHED, and this lint printed "every pin and
#     ignore accounted for" over an obligation it had silently dropped — exit 0,
#     the precise shape the whole file exists to refuse. PyYAML settles syntax,
#     not the shape these controls read (CodeRabbit, PR #3325).
IGN_NOTLIST='version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/client/desktop"
    ignore: not-a-list'
D=$(mkroot ignnotlist "$EMPTY_REG" "$IGN_NOTLIST")
run "a non-sequence ignore: exits 2, never 0"  "$D" 2 "\"ignore:\" is not a sequence"

# ── 24. …and an ignore ITEM that is not a mapping, which would have been read as
#     a rule carrying no dependency-name and skipped by the `if (!name) continue`.
IGN_BADITEM='version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/client/desktop"
    ignore:
      - left-pad'
D=$(mkroot ignbaditem "$EMPTY_REG" "$IGN_BADITEM")
run "a non-mapping ignore item exits 2"        "$D" 2 "is not a mapping"

# ── 25. THE FAIL-OPEN, ONE LEAF DEEPER. An ignore rule carrying no
#     `dependency-name` passed the container-shape validation, emitted NO
#     obligation, and the lint exited 0 with "every pin and ignore accounted for".
#     `dependency-name` is the field the register is matched on, so a rule without
#     one is an obligation this control cannot see (CodeRabbit, PR #3325).
IGN_NONAME='version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/client/desktop"
    ignore:
      - versions: [">=2.0.0"]'
D=$(mkroot ignnoname "$EMPTY_REG" "$IGN_NONAME")
run "an ignore rule with no dependency-name exits 2" "$D" 2 "dependency-name"

# ── 26. …and a non-string one, which produced an obligation keyed on a number
#     that no register entry could ever match.
IGN_NUMNAME='version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/client/desktop"
    ignore:
      - dependency-name: 123'
D=$(mkroot ignnumname "$EMPTY_REG" "$IGN_NUMNAME")
run "a non-string dependency-name exits 2"          "$D" 2 "dependency-name"

# ── 27. `directory` and `directories` are alternatives, and the parser returned
#     their UNION — a declared-coverage set Dependabot never promised. A block
#     naming both is refused rather than resolved, because picking one would be
#     guessing which Dependabot honours (CodeRabbit, PR #3325).
BOTH_KEYS='version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/client/desktop"
    directories: ["/client/admin"]'
D=$(mkroot_raw bothkeys "$EMPTY_REG" "$BOTH_KEYS")
run "a block naming both directory keys exits 2"    "$D" 2 "declares both"

# ── 28. …and an explicit `directory: null` counts as PRESENT, because value
#     truthiness is not key presence.
NULL_DIR='version: 2
updates:
  - package-ecosystem: "npm"
    directory: null
    directories: ["/client/desktop"]'
D=$(mkroot_raw nulldir "$EMPTY_REG" "$NULL_DIR")
run "an explicit null directory counts as present"  "$D" 2 "declares both"

echo
echo "check-pin-exceptions tests: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
