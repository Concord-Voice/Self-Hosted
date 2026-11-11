#!/usr/bin/env bash
#
# check-base-images.test.sh — self-test for check-base-images.py.
#
# Hermetic: one directory per case under fixtures/base-images/, each with a
# manifest.json and one or more *.dockerfile files. None is named Dockerfile*,
# so `git ls-files -- '*Dockerfile*'` never picks a fixture up. A must-fail
# case has to produce exactly the rule it names and no other, so it cannot pass
# off a neighbouring rule. The cases that the repository's own hooks would
# rewrite or reject (CRLF, a BOM, trailing blanks, invalid JSON) are built in a
# temp directory below.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CHECKER="${REPO_ROOT}/.github/scripts/check-base-images.py"
FIXTURES="${SCRIPT_DIR}/fixtures/base-images"

pass=0
fail=0
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

ok() { echo "  ✓ $1"; pass=$(( pass + 1 )); }
ko() { echo "  ✗ $1" >&2; fail=$(( fail + 1 )); }

rc=0
out=""
err=""

# invoke ARGS... — run the checker; sets rc, out (stdout) and err (stderr).
invoke() {
  rc=0
  out=$(python3 "$CHECKER" "$@" 2> "$WORK/stderr") || rc=$?
  err=$(cat "$WORK/stderr")
}

# run CASE [FILE...] — check a case directory (a name under fixtures/base-images/,
# or an absolute path); FILE defaults to every *.dockerfile in it.
run() {
  local dir="$1" f
  shift
  [[ "$dir" == /* ]] || dir="$FIXTURES/$dir"
  local files=("$@")
  if [ "${#files[@]}" -eq 0 ]; then
    for f in "$dir"/*.dockerfile; do files+=("$(basename "$f")"); done
  fi
  invoke --root "$dir" --manifest "$dir/manifest.json" "${files[@]}"
}

# rules_of OUTPUT — the distinct rule names in "<file>:<line>: <rule>: <message>" lines.
rules_of() {
  sed -E 's/^[^:]+:[0-9]+: ([a-z-]+): .*$/\1/' <<<"$1" | sort -u | paste -sd, -
}

detail() { echo "rc=$rc, rules='$(rules_of "$out")', stderr: $(head -c 200 <<<"$err")"; }

# expect_fail DIR RULE DESC — exit 1, with RULE the only rule reported.
expect_fail() {
  run "$1"
  if [ "$rc" -eq 1 ] && [ "$(rules_of "$out")" = "$2" ] && [ -z "$err" ]; then
    ok "$3"
  else
    ko "$3 ($(detail))"
  fi
}

# expect_pass DIR DESC — exit 0 with no output at all.
expect_pass() {
  run "$1"
  if [ "$rc" -eq 0 ] && [ -z "$out" ] && [ -z "$err" ]; then ok "$2"; else ko "$2 ($(detail))"; fi
}

# expect_exit2 DESC FRAGMENT... — after run/invoke: exit 2, nothing on stdout,
# and every FRAGMENT on stderr.
expect_exit2() {
  local desc="$1" frag
  shift
  if [ "$rc" -ne 2 ] || [ -n "$out" ]; then ko "$desc ($(detail))"; return; fi
  for frag in "$@"; do
    if ! grep -qF -- "$frag" <<<"$err"; then ko "$desc (stderr lacks '$frag': $(detail))"; return; fi
  done
  ok "$desc"
}

DIGEST="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
IMAGE="golang:1.27.3-alpine@${DIGEST}"
MANIFEST='{"schema":1,"dockerfiles":{"app.dockerfile":{"stages":[{"alias":"builder","registry":"docker.io","repository":"library/golang","tag":"^1\\.27\\.[0-9]+-alpine$"}]}}}'

# dyn NAME TEXT — a case in $WORK with the standard manifest; TEXT goes through
# printf %b, so \n \r \t \\ and \xHH are escapes.
dyn() {
  mkdir "$WORK/$1"
  printf '%s\n' "$MANIFEST" > "$WORK/$1/manifest.json"
  printf '%b' "$2" > "$WORK/$1/app.dockerfile"
}

echo "── must fail (exit 1, one rule) ──"
expect_fail fail-missing-tag              missing-tag          "a digest-only FROM with no tag is refused"
expect_fail fail-from-earlier-stage       missing-tag          "a FROM naming an earlier stage is refused"
expect_fail fail-identity-registry        identity-mismatch    "a registry that is not in the manifest is refused"
expect_fail fail-identity-repository      identity-mismatch    "a repository that is not in the manifest is refused"
expect_fail fail-missing-digest           missing-digest       "a FROM with no digest is refused"
expect_fail fail-short-digest             missing-digest       "a digest of 63 hex characters is refused"
expect_fail fail-tag-flavor-swap          tag-mismatch         "alpine swapped for bookworm under one image name is refused"
expect_fail fail-variable-reference       variable-reference   "an ARG-templated FROM is refused"
expect_fail fail-copy-external-image      external-copy-source "COPY --from= an external image is refused"
expect_fail fail-copy-own-stage-index     external-copy-source "COPY --from= the current stage's own index is refused"
expect_fail fail-copy-own-alias           external-copy-source "COPY --from= the current stage's own alias is refused"
expect_fail fail-copy-undeclared-context  external-copy-source "COPY --from= an undeclared build context is refused"
expect_fail fail-mount-external-image     external-copy-source "RUN --mount=from= an external image is refused"
expect_fail fail-onbuild-copy-external    external-copy-source "ONBUILD COPY --from= an external image is refused"
expect_fail fail-unlisted-dockerfile      unlisted-dockerfile  "a Dockerfile with no manifest entry is refused"
expect_fail fail-stale-entry              stale-entry          "a manifest entry with no file is refused"
expect_fail fail-syntax-changed           syntax-directive     "a changed # syntax= directive is refused"
expect_fail fail-syntax-undeclared        syntax-directive     "an undeclared # syntax= directive is refused"
expect_fail fail-syntax-removed           syntax-directive     "a declared # syntax= directive that is gone is refused"
expect_fail fail-syntax-slash-form        syntax-directive     "a // syntax= directive, which BuildKit also honors, is refused"
expect_fail fail-escape-directive         escape-directive     "an # escape= directive is refused"
expect_fail fail-escape-after-comment     escape-directive     "an # escape= directive after other leading comments is refused"
expect_fail fail-stage-extra              stage-mismatch       "an extra stage is refused"
expect_fail fail-stage-tab-separated      stage-mismatch       "a FROM separated from its image by a tab is still a stage"
expect_fail fail-stage-fewer              stage-mismatch       "a missing stage is refused"
expect_fail fail-stage-alias              stage-mismatch       "a wrong alias is refused"
expect_fail fail-stage-alias-dropped      stage-mismatch       "a dropped alias is refused"
expect_fail fail-stage-alias-added        stage-mismatch       "an added alias is refused"
expect_fail fail-stage-image-for-scratch  stage-mismatch       "an image where the manifest says scratch is refused"
expect_fail fail-stage-scratch-for-image  stage-mismatch       "scratch where the manifest says an image is refused"

echo ""
echo "── must fail (exit 2, could not complete) ──"
run exit2-unanchored-start
expect_exit2 "a tag pattern with no ^ is refused" 'dockerfiles["app.dockerfile"]' 'must start with ^ and end with $'
run exit2-unanchored-end
expect_exit2 "a tag pattern with no \$ is refused" 'dockerfiles["app.dockerfile"]' 'must start with ^ and end with $'
run exit2-bad-regex
expect_exit2 "a tag pattern that does not compile is refused" 'dockerfiles["app.dockerfile"]' "does not compile"
run exit2-no-stages
expect_exit2 "an entry with no stages is refused" 'dockerfiles["app.dockerfile"]' '"stages"'
run exit2-scratch-and-registry
expect_exit2 "a stage that is both scratch and an image is refused" 'dockerfiles["app.dockerfile"].stages[0]' "scratch"

dyn not-json 'FROM scratch\n'
printf 'this is not json\n' > "$WORK/not-json/manifest.json"
run "$WORK/not-json"
expect_exit2 "a manifest that is not JSON is refused" "not valid JSON"

# Inputs that BuildKit reads differently from a naive line parser. Each one hid
# an image from an earlier version of the checker. Built here because the
# repository's own hooks would rewrite a CR or a control byte in a fixture, and
# the Semgrep Dockerfile parser reports a heredoc fixture as a scanner error.
EVIL="evil.example/base:1@${DIGEST}"
HEAD_LINE="FROM ${IMAGE} AS builder"
dyn hidden-double-backslash "${HEAD_LINE}"'\nENV PAD=x\\\\\nFROM '"${EVIL}"'\n'
expect_fail "$WORK/hidden-double-backslash" stage-mismatch       "a doubled backslash at line end does not continue the line, so the next FROM is a stage"
dyn hidden-lone-cr "${HEAD_LINE}"'\nRUN echo \\\r#c\nFROM '"${EVIL}"'\n'
expect_fail "$WORK/hidden-lone-cr"          unsupported-character "a lone CR is refused"
dyn hidden-control-blank "${HEAD_LINE}"'\nRUN echo a \\\n\x1f\nFROM '"${EVIL}"'\n'
expect_fail "$WORK/hidden-control-blank"    unsupported-character "a control byte that Python reads as a blank line is refused"
dyn heredoc-from "${HEAD_LINE}"'\nRUN <<EOF\nFROM '"${EVIL}"'\nEOF\n'
expect_fail "$WORK/heredoc-from"            heredoc,stage-mismatch "a FROM-looking line inside a heredoc body is a stage, and the heredoc is refused"
dyn hidden-heredoc "${HEAD_LINE}"'\nRUN <<\x27#END\x27\ntrue \\\n#END\nFROM '"${EVIL}"'\n'
expect_fail "$WORK/hidden-heredoc"          heredoc              "a heredoc is refused, whatever its body hides"
dyn flag-quoted "${HEAD_LINE}"'\nCOPY --fr"o"m='"${EVIL}"' /a /b\n'
expect_fail "$WORK/flag-quoted"             external-copy-source "a quoted --from flag is refused"
dyn flag-escaped "${HEAD_LINE}"'\nCOPY --fr\\om='"${EVIL}"' /a /b\n'
expect_fail "$WORK/flag-escaped"            external-copy-source "a backslash-escaped --from flag is refused"
# A quoted word can hold a space, so splitting on blanks would stop reading flags
# one word early and never reach the --from behind it.
dyn flag-behind-quoted-space "${HEAD_LINE}"'\nCOPY --chown="a b" --from='"${EVIL}"' /a /b\n'
expect_fail "$WORK/flag-behind-quoted-space" external-copy-source "a --from behind a quoted flag that holds a space is refused"
dyn mount-quoted-field "${HEAD_LINE}"'\nRUN --mount=type=bind,"from='"${EVIL}"'",target=/m true\n'
expect_fail "$WORK/mount-quoted-field"      external-copy-source "a quoted from= field in --mount is refused"
dyn flag-after-cr "${HEAD_LINE}"'\nCOPY\r--from='"${EVIL}"' /a /b\n'
expect_fail "$WORK/flag-after-cr"           unsupported-character "a CR used as a word separator is refused"
dyn flag-after-nbsp-byte "${HEAD_LINE}"'\nCOPY --exclude=x\xc3\xa0--from='"${EVIL}"' /a /b\n'
expect_fail "$WORK/flag-after-nbsp-byte"    unsupported-character "a non-ASCII character in an instruction is refused"
dyn slash-directive-joined '// escape=\\\n// syntax=evil.example/frontend:1\n'"${HEAD_LINE}"'\n'
expect_fail "$WORK/slash-directive-joined"  syntax-directive     "a // syntax= line hidden behind a // continuation is refused"


dyn unreadable 'FROM scratch\n'
rm "$WORK/unreadable/app.dockerfile"
run "$WORK/unreadable" app.dockerfile
expect_exit2 "a listed Dockerfile that cannot be read is refused" "cannot read" "app.dockerfile"

mkdir "$WORK/bin" "$WORK/no-git"
printf '#!/bin/sh\nexit 3\n' > "$WORK/bin/git"
chmod +x "$WORK/bin/git"
cp "$FIXTURES/pass-digest-bump/manifest.json" "$WORK/no-git/manifest.json"
PATH="$WORK/bin:$PATH" invoke --root "$WORK/no-git" --manifest "$WORK/no-git/manifest.json"
expect_exit2 "a failing git ls-files is refused" "git ls-files"

echo ""
echo "── must pass (exit 0, no output) ──"
expect_pass pass-digest-bump                "a digest-only bump needs no manifest edit"
expect_pass pass-patch-bump                 "a same-flavor patch bump needs no manifest edit"
expect_pass pass-scratch                    "FROM scratch, and COPY/mount from an earlier stage alias"
expect_pass pass-platform-flag              "--platform=\$BUILDPLATFORM is a flag, not the image"
expect_pass pass-build-context              "COPY --from= a declared build context"
expect_pass pass-copy-index                 "COPY --from=0 names an earlier stage"
expect_pass pass-continuation               "a FROM split over a continuation line, and a comment inside one"
expect_pass pass-registry-port              "a registry with a port, localhost, explicit docker.io and a Hub organisation"
expect_pass pass-lowercase                  "lower-case instructions"
expect_pass pass-directive-after-instruction "# syntax= and # escape= after the first instruction are comments"

dyn trailing-blank "FROM ${IMAGE} AS \\\\ \\t\\nbuilder\\n"
expect_pass "$WORK/trailing-blank"          "a continuation backslash followed by blanks continues the line"
dyn crlf "FROM ${IMAGE} \\\\\\r\\nAS builder\\r\\n"
expect_pass "$WORK/crlf"                    "CRLF line endings, including a continuation"
dyn comment-non-ascii '# caf\xc3\xa9 \xe2\x80\x94 a note\n'"${HEAD_LINE}"'\n'
expect_pass "$WORK/comment-non-ascii"       "non-ASCII text in a comment line is allowed"
dyn bom "\\xef\\xbb\\xbfFROM ${IMAGE} AS builder\\n"
expect_pass "$WORK/bom"                     "a UTF-8 byte order mark before the first instruction"

echo ""
echo "── the repository ──"
invoke --root "$REPO_ROOT"
if [ "$rc" -eq 0 ] && [ -z "$out" ] && [ -z "$err" ]; then
  ok "the tracked Dockerfiles match .github/base-images.json"
else
  ko "the tracked Dockerfiles do not match the manifest ($(detail))"
fi

echo ""
echo "─────────────────────────────────────────"
echo "$pass passed, $fail failed"
[[ "$fail" == "0" ]]
