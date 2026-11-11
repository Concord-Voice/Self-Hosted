#!/usr/bin/env bash
#
# check-base-images.test.sh — self-test for check-base-images.py.
#
# Hermetic. A case with the standard one-stage manifest is one `dyn` line, built
# in a temp directory. A case that needs its own manifest or several files is a
# directory under fixtures/base-images/ with a manifest.json and *.df files. No
# fixture path contains "dockerfile" in any letter case, because the checker
# treats every such tracked path as a Dockerfile. A must-fail case has to
# produce exactly the rule it names and no other; where several branches share
# a rule name, the case also names a fragment of the one message it expects.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CHECKER="${REPO_ROOT}/.github/scripts/check-base-images.py"
FIXTURES="${SCRIPT_DIR}/fixtures/base-images"

# Git exports these to the hooks it runs. Left set, the scratch repositories
# below would resolve to the surrounding repository.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
      GIT_COMMON_DIR GIT_PREFIX GIT_CONFIG_PARAMETERS 2>/dev/null || true

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
# or an absolute path); FILE defaults to every *.df in it.
run() {
  local dir="$1" f
  shift
  [[ "$dir" == /* ]] || dir="$FIXTURES/$dir"
  local files=("$@")
  if [ "${#files[@]}" -eq 0 ]; then
    for f in "$dir"/*.df; do files+=("$(basename "$f")"); done
  fi
  invoke --root "$dir" --manifest "$dir/manifest.json" "${files[@]}"
}

# rules_of OUTPUT — the distinct rule names in "<file>:<line>: <rule>: <message>" lines.
rules_of() {
  sed -E 's/^[^:]+:[0-9]+: ([a-z-]+): .*$/\1/' <<<"$1" | sort -u | paste -sd, -
}

detail() { echo "rc=$rc, rules='$(rules_of "$out")', stderr: $(head -c 200 <<<"$err")"; }

# assert_fail RULE DESC [FRAGMENT] — after run/invoke: exit 1, RULE the only rule
# reported, nothing on stderr, and FRAGMENT (a fixed string) in the output.
assert_fail() {
  if [ "$rc" -ne 1 ] || [ "$(rules_of "$out")" != "$1" ] || [ -n "$err" ]; then
    ko "$2 ($(detail))"
  elif [ -n "${3:-}" ] && ! grep -qF -- "$3" <<<"$out"; then
    ko "$2 (output lacks '$3': $out)"
  else
    ok "$2"
  fi
}

# expect_fail DIR RULE DESC [FRAGMENT]
expect_fail() {
  run "$1"
  assert_fail "$2" "$3" "${4:-}"
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
MANIFEST='{"schema":1,"dockerfiles":{"app.df":{"stages":[{"alias":"builder","registry":"docker.io","repository":"library/golang","tag":"^1\\.27\\.[0-9]+-alpine$"}]}}}'

# One stage plus a scratch stage, and one stage with a declared # syntax=.
MANIFEST2='{"schema":1,"dockerfiles":{"app.df":{"stages":[{"alias":"builder","registry":"docker.io","repository":"library/golang","tag":"^1\\.27\\.[0-9]+-alpine$"},{"scratch":true}]}}}'
MANIFEST_SYNTAX='{"schema":1,"dockerfiles":{"app.df":{"syntax":"docker/dockerfile:1.7","stages":[{"alias":"builder","registry":"docker.io","repository":"library/golang","tag":"^1\\.27\\.[0-9]+-alpine$"}]}}}'

# dyn NAME TEXT [MANIFEST] — a case in $WORK, with the standard manifest unless
# one is given; TEXT goes through printf %b, so \n \r \t \\ and \xHH are escapes.
dyn() {
  mkdir "$WORK/$1"
  printf '%s\n' "${3:-$MANIFEST}" > "$WORK/$1/manifest.json"
  printf '%b' "$2" > "$WORK/$1/app.df"
}

echo "── must fail (exit 1, one rule) ──"
dyn fail-missing-tag 'FROM golang@'"${DIGEST}"' AS builder\n'
expect_fail "$WORK/fail-missing-tag"              missing-tag          "a digest-only FROM with no tag is refused"
expect_fail fail-from-earlier-stage       missing-tag          "a FROM naming an earlier stage is refused"
dyn fail-identity-registry 'FROM quay.io/library/golang:1.27.3-alpine@'"${DIGEST}"' AS builder\n'
expect_fail "$WORK/fail-identity-registry"        identity-mismatch    "a registry that is not in the manifest is refused"
dyn fail-identity-repository 'FROM someorg/golang:1.27.3-alpine@'"${DIGEST}"' AS builder\n'
expect_fail "$WORK/fail-identity-repository"      identity-mismatch    "a repository that is not in the manifest is refused"
dyn fail-missing-digest 'FROM golang:1.27.3-alpine AS builder\n'
expect_fail "$WORK/fail-missing-digest"           missing-digest       "a FROM with no digest is refused"
dyn fail-short-digest 'FROM golang:1.27.3-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa AS builder\n'
expect_fail "$WORK/fail-short-digest"             missing-digest       "a digest of 63 hex characters is refused"
dyn fail-tag-flavor-swap 'FROM golang:1.27.3-bookworm@'"${DIGEST}"' AS builder\n'
expect_fail "$WORK/fail-tag-flavor-swap"          tag-mismatch         "alpine swapped for bookworm under one image name is refused"
dyn fail-variable-reference 'ARG BASE=golang:1.27.3-alpine@'"${DIGEST}"'\nFROM ${BASE} AS builder\n'
expect_fail "$WORK/fail-variable-reference"       variable-reference   "an ARG-templated FROM is refused"
dyn fail-copy-external-image 'FROM golang:1.27.3-alpine@'"${DIGEST}"' AS builder\nCOPY --from=busybox:1.36 /bin/busybox /busybox\n'
expect_fail "$WORK/fail-copy-external-image"      external-copy-source "COPY --from= an external image is refused" "app.df:2: external-copy-source: COPY source busybox:1.36 is not an earlier stage"
expect_fail fail-copy-own-stage-index     external-copy-source "COPY --from= the current stage's own index is refused" "COPY source 1 is not an earlier stage"
dyn fail-copy-own-alias 'FROM golang:1.27.3-alpine@'"${DIGEST}"' AS builder\nCOPY --from=builder /a /b\n'
expect_fail "$WORK/fail-copy-own-alias"           external-copy-source "COPY --from= the current stage's own alias is refused" "COPY source builder is not an earlier stage"
dyn fail-copy-undeclared-context 'FROM golang:1.27.3-alpine@'"${DIGEST}"' AS builder\nCOPY --from=admin_ui . .\n'
expect_fail "$WORK/fail-copy-undeclared-context"  external-copy-source "COPY --from= an undeclared build context is refused" "COPY source admin_ui is not an earlier stage"
dyn fail-mount-external-image 'FROM golang:1.27.3-alpine@'"${DIGEST}"' AS builder\nRUN --mount=type=bind,from=busybox:1.36,target=/x true\n'
expect_fail "$WORK/fail-mount-external-image"     external-copy-source "RUN --mount=from= an external image is refused" "RUN source busybox:1.36 is not an earlier stage"
dyn fail-onbuild-copy-external 'FROM golang:1.27.3-alpine@'"${DIGEST}"' AS builder\nONBUILD COPY --from=busybox:1.36@'"${DIGEST}"' /bin/sh /sh\n'
expect_fail "$WORK/fail-onbuild-copy-external"    external-copy-source "ONBUILD COPY --from= an external image is refused" "is resolved by the child build"
expect_fail fail-unlisted                 unlisted-dockerfile  "a Dockerfile with no manifest entry is refused"
expect_fail fail-stale-entry              stale-entry          "a manifest entry with no file is refused"
expect_fail fail-syntax-changed           syntax-directive     "a changed # syntax= directive is refused" "differs from the manifest value"
dyn fail-syntax-undeclared '# syntax=docker/dockerfile:1.7\nFROM golang:1.27.3-alpine@'"${DIGEST}"' AS builder\n'
expect_fail "$WORK/fail-syntax-undeclared"        syntax-directive     "an undeclared # syntax= directive is refused" "is not declared in the manifest"
expect_fail fail-syntax-removed           syntax-directive     "a declared # syntax= directive that is gone is refused" "but the file has none"
dyn fail-syntax-slash-form '// syntax=example.invalid/frontend:1\nFROM golang:1.27.3-alpine@'"${DIGEST}"' AS builder\n'
expect_fail "$WORK/fail-syntax-slash-form"        syntax-directive     "a // syntax= directive, which BuildKit also honors, is refused" "a line that starts with //"
dyn fail-escape-directive '# escape=`\nFROM golang:1.27.3-alpine@'"${DIGEST}"' AS builder\n'
expect_fail "$WORK/fail-escape-directive"         escape-directive     "an # escape= directive is refused"
dyn fail-escape-after-comment '# note\n\n# escape=`\nFROM golang:1.27.3-alpine@'"${DIGEST}"' AS builder\n'
expect_fail "$WORK/fail-escape-after-comment"     escape-directive     "an # escape= directive after other leading comments is refused"
dyn fail-stage-extra 'FROM golang:1.27.3-alpine@'"${DIGEST}"' AS builder\nFROM alpine:3.24@'"${DIGEST}"'\n'
expect_fail "$WORK/fail-stage-extra"              stage-mismatch       "an extra stage is refused" "app.df:2: stage-mismatch: the file has 2 FROM lines"
dyn fail-stage-tab-separated 'FROM golang:1.27.3-alpine@'"${DIGEST}"' AS builder\nFROM\tbusybox:1.36@'"${DIGEST}"'\n'
expect_fail "$WORK/fail-stage-tab-separated"      stage-mismatch       "a FROM separated from its image by a tab is still a stage" "the file has 2 FROM lines"
expect_fail fail-stage-fewer              stage-mismatch       "a missing stage is refused" "the file has 1 FROM lines and the manifest declares 2"
dyn fail-stage-alias 'FROM golang:1.27.3-alpine@'"${DIGEST}"' AS build\n'
expect_fail "$WORK/fail-stage-alias"              stage-mismatch       "a wrong alias is refused" "alias build differs from the manifest alias builder"
dyn fail-stage-alias-dropped 'FROM golang:1.27.3-alpine@'"${DIGEST}"'\n'
expect_fail "$WORK/fail-stage-alias-dropped"      stage-mismatch       "a dropped alias is refused" "alias (none) differs"
expect_fail fail-stage-alias-added        stage-mismatch       "an added alias is refused" "differs from the manifest alias (none)"
expect_fail fail-stage-image-for-scratch  stage-mismatch       "an image where the manifest says scratch is refused" "an image, but the manifest declares scratch"
expect_fail fail-stage-scratch-for-image  stage-mismatch       "scratch where the manifest says an image is refused" "FROM scratch, but the manifest declares an image"

echo ""
echo "── must fail (exit 2, could not complete) ──"
run exit2-unanchored-start
expect_exit2 "a tag pattern with no ^ is refused" 'dockerfiles["app.df"]' 'must start with ^ and end with $'
run exit2-unanchored-end
expect_exit2 "a tag pattern with no \$ is refused" 'dockerfiles["app.df"]' 'must start with ^ and end with $'
run exit2-bad-regex
expect_exit2 "a tag pattern that does not compile is refused" 'dockerfiles["app.df"]' "does not compile"
run exit2-no-stages
expect_exit2 "an entry with no stages is refused" 'dockerfiles["app.df"]' '"stages"'
run exit2-scratch-and-registry
expect_exit2 "a stage that is both scratch and an image is refused" 'dockerfiles["app.df"].stages[0]' "scratch"

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
expect_fail "$WORK/hidden-double-backslash" stage-mismatch       "a doubled backslash at line end does not continue the line, so the next FROM is a stage" "app.df:3: stage-mismatch: the file has 2 FROM lines"
dyn hidden-lone-cr "${HEAD_LINE}"'\nRUN echo \\\r#c\nFROM '"${EVIL}"'\n'
expect_fail "$WORK/hidden-lone-cr"          unsupported-character "a lone CR is refused"
dyn hidden-control-blank "${HEAD_LINE}"'\nRUN echo a \\\n\x1f\nFROM '"${EVIL}"'\n'
expect_fail "$WORK/hidden-control-blank"    unsupported-character "a control byte that Python reads as a blank line is refused"
dyn heredoc-from "${HEAD_LINE}"'\nRUN <<EOF\nFROM '"${EVIL}"'\nEOF\n'
expect_fail "$WORK/heredoc-from"            heredoc,stage-mismatch "a FROM-looking line inside a heredoc body is a stage, and the heredoc is refused"
dyn hidden-heredoc "${HEAD_LINE}"'\nRUN <<\x27#END\x27\ntrue \\\n#END\nFROM '"${EVIL}"'\n'
expect_fail "$WORK/hidden-heredoc"          heredoc              "a heredoc is refused, whatever its body hides"
dyn flag-quoted "${HEAD_LINE}"'\nCOPY --fr"o"m='"${EVIL}"' /a /b\n'
expect_fail "$WORK/flag-quoted"             external-copy-source "a quoted --from flag is refused" "is quoted or escaped"
dyn flag-escaped "${HEAD_LINE}"'\nCOPY --fr\\om='"${EVIL}"' /a /b\n'
expect_fail "$WORK/flag-escaped"            external-copy-source "a backslash-escaped --from flag is refused" "is quoted or escaped"
# A quoted word can hold a space, so the words after it cannot be trusted. The
# quoted --chown word is refused; the --from behind it is never read.
dyn flag-behind-quoted-space "${HEAD_LINE}"'\nCOPY --chown="a b" --from='"${EVIL}"' /a /b\n'
expect_fail "$WORK/flag-behind-quoted-space" external-copy-source "a quoted flag word is refused before the --from behind it is read" 'flag --chown="a is quoted or escaped'
dyn mount-quoted-field "${HEAD_LINE}"'\nRUN --mount=type=bind,"from='"${EVIL}"'",target=/m true\n'
expect_fail "$WORK/mount-quoted-field"      external-copy-source "a quoted from= field in --mount is refused" "is quoted or escaped"
dyn flag-after-cr "${HEAD_LINE}"'\nCOPY\r--from='"${EVIL}"' /a /b\n'
expect_fail "$WORK/flag-after-cr"           unsupported-character "a CR used as a word separator is refused"
dyn flag-after-nbsp-byte "${HEAD_LINE}"'\nCOPY --exclude=x\xc3\xa0--from='"${EVIL}"' /a /b\n'
expect_fail "$WORK/flag-after-nbsp-byte"    unsupported-character "a non-ASCII character in an instruction is refused"
dyn slash-directive-joined '// escape=\\\n// syntax=evil.example/frontend:1\n'"${HEAD_LINE}"'\n'
expect_fail "$WORK/slash-directive-joined"  syntax-directive     "every // line is refused, so a // syntax= behind a // continuation cannot hide" "app.df:2: syntax-directive"

# Source flags. BuildKit lower-cases nothing here, but the checker folds case so
# that an upper-case flag is refused rather than skipped.
TWO="${HEAD_LINE}"'\nFROM scratch'
dyn flag-uppercase "${HEAD_LINE}"'\nCOPY --FROM='"${EVIL}"' /a /b\n'
expect_fail "$WORK/flag-uppercase"          external-copy-source "an upper-case --FROM flag is still read" "is not an earlier stage"
dyn mount-key-uppercase "${HEAD_LINE}"'\nRUN --mount=type=bind,FROM='"${EVIL}"',target=/m true\n'
expect_fail "$WORK/mount-key-uppercase"     external-copy-source "an upper-case FROM= mount field is still read" "RUN source evil.example"
dyn add-from "${HEAD_LINE}"'\nADD --from='"${EVIL}"' /a /b\n'
expect_fail "$WORK/add-from"                external-copy-source "ADD --from= an external image is refused" "ADD source evil.example"
# Only the quoting refusal can fire here: without the quotes the value is a
# legitimate earlier stage.
dyn quoted-own-stage "${TWO}"'\nCOPY --fr"o"m=builder /a /b\n' "$MANIFEST2"
expect_fail "$WORK/quoted-own-stage"        external-copy-source "a quoted --from is refused even when it names an earlier stage" "is quoted or escaped"
# BuildKit accepts a stage index for COPY and ADD only. In a mount, from=0 is the
# image docker.io/library/0.
dyn mount-from-index "${TWO}"'\nRUN --mount=type=bind,from=0,target=/m true\n' "$MANIFEST2"
expect_fail "$WORK/mount-from-index"        external-copy-source "a stage index in RUN --mount is an image name, and is refused" "RUN source 0 is not an earlier stage"
# An ONBUILD trigger runs in the child build, against the child's stages.
dyn onbuild-own-stage "${TWO}"'\nONBUILD COPY --from=builder /a /b\n' "$MANIFEST2"
expect_fail "$WORK/onbuild-own-stage"       external-copy-source "ONBUILD COPY --from= an earlier stage of this file is refused" "is resolved by the child build"
dyn copy-continued "${HEAD_LINE}"'\nCOPY \\\n  --from='"${EVIL}"' /a /b\n'
expect_fail "$WORK/copy-continued"          external-copy-source "a violation in a continued instruction is reported at its first line" "app.df:2: external-copy-source"

# FROM arguments and directives.
dyn from-escaped-flag 'FROM --platform=x\\ '"${IMAGE}"' AS builder\n'
expect_fail "$WORK/from-escaped-flag"       stage-mismatch       "a quoted or escaped word in FROM is refused" "cannot parse the arguments of FROM"
dyn no-from '# only a comment\n'
expect_fail "$WORK/no-from"                 stage-mismatch       "a file with no FROM is refused at line 1" "app.df:1: stage-mismatch: the file has 0 FROM lines"
dyn syntax-mixed-case '# Syntax=other.example/frontend:1\n'"${HEAD_LINE}"'\n'
expect_fail "$WORK/syntax-mixed-case"       syntax-directive     "a # Syntax= directive in another letter case is still read" "is not declared in the manifest"
dyn escape-upper-case '# ESCAPE=`\n'"${HEAD_LINE}"'\n'
expect_fail "$WORK/escape-upper-case"       escape-directive     "an # ESCAPE= directive in upper case is still read"
# BuildKit reads # syntax= only while every line above it is a directive. Below
# a comment, a blank line or leading blanks it is an ordinary comment, and the
# build silently uses the built-in frontend.
dyn syntax-after-comment '# a note\n# syntax=docker/dockerfile:1.7\n'"${HEAD_LINE}"'\n' "$MANIFEST_SYNTAX"
expect_fail "$WORK/syntax-after-comment"    syntax-directive     "a # syntax= line below a comment is refused" "app.df:2: syntax-directive: BuildKit reads # syntax= only"
dyn syntax-after-blank '\n# syntax=docker/dockerfile:1.7\n'"${HEAD_LINE}"'\n' "$MANIFEST_SYNTAX"
expect_fail "$WORK/syntax-after-blank"      syntax-directive     "a # syntax= line below a blank line is refused" "app.df:2: syntax-directive: BuildKit reads # syntax= only"
dyn syntax-indented '  # syntax=docker/dockerfile:1.7\n'"${HEAD_LINE}"'\n' "$MANIFEST_SYNTAX"
expect_fail "$WORK/syntax-indented"         syntax-directive     "an indented # syntax= line is refused" "app.df:1: syntax-directive: BuildKit reads # syntax= only"


dyn unreadable 'FROM scratch\n'
rm "$WORK/unreadable/app.df"
run "$WORK/unreadable" app.df
expect_exit2 "a listed Dockerfile that cannot be read is refused" "cannot read" "app.df"

mkdir "$WORK/bin" "$WORK/no-git"
printf '#!/bin/sh\nexit 3\n' > "$WORK/bin/git"
chmod +x "$WORK/bin/git"
printf '%s\n' "$MANIFEST" > "$WORK/no-git/manifest.json"
PATH="$WORK/bin:$PATH" invoke --root "$WORK/no-git" --manifest "$WORK/no-git/manifest.json"
expect_exit2 "a failing git ls-files is refused" "git ls-files"

run "$WORK/unreadable" app.df --manifest "$WORK/unreadable/absent.json"
expect_exit2 "a missing manifest is refused" "cannot read manifest"
dyn schema-two 'FROM scratch\n' '{"schema":2,"dockerfiles":{}}'
run "$WORK/schema-two"
expect_exit2 "a manifest with another schema number is refused" '"schema"' "must be 1"
dyn entry-unknown-key 'FROM scratch\n' '{"schema":1,"dockerfiles":{"app.df":{"stages":[{"scratch":true}],"contexts":[]}}}'
run "$WORK/entry-unknown-key"
expect_exit2 "an entry with an unknown key is refused" 'dockerfiles["app.df"]' 'must be an object with "stages"'
dyn stage-no-tag 'FROM scratch\n' '{"schema":1,"dockerfiles":{"app.df":{"stages":[{"registry":"docker.io","repository":"library/golang"}]}}}'
run "$WORK/stage-no-tag"
expect_exit2 "a stage with no tag pattern is refused" 'stages[0]' '"registry", "repository" and "tag"'
dyn not-utf8 "${HEAD_LINE}"'\n# \xff\n'
run "$WORK/not-utf8"
expect_exit2 "a Dockerfile that is not UTF-8 is refused" "cannot read" "app.df"

echo ""
echo "── which tracked files are Dockerfiles ──"
# repo NAME — a scratch git repository whose root Dockerfile matches the standard
# manifest. The checker lists the index, so nothing needs a commit.
repo() {
  mkdir -p "$WORK/$1"
  git -C "$WORK/$1" init -q
  printf '%s\n' "${MANIFEST/app.df/Dockerfile}" > "$WORK/$1/manifest.json"
  printf '%b' "${HEAD_LINE}"'\n' > "$WORK/$1/Dockerfile"
}
listed() { git -C "$WORK/$1" add -A; invoke --root "$WORK/$1" --manifest "$WORK/$1/manifest.json"; }
# On a case-insensitive checkout (macOS, Windows) a second path that differs
# only in letter case replaces the real Dockerfile. It must be listed, and so
# refused, on every platform.
repo case-variant
mkdir "$WORK/case-variant/sub"
printf 'FROM %s\n' "$EVIL" > "$WORK/case-variant/sub/dockerfile"
listed case-variant
assert_fail unlisted-dockerfile "a lower-case dockerfile is listed and refused" "sub/dockerfile:1: unlisted-dockerfile"
repo fold-variant
mkdir "$WORK/fold-variant/sub"
printf 'FROM %s\n' "$EVIL" > "$WORK/fold-variant/sub/$(printf 'Doc\xe2\x84\xaaerfile')"
listed fold-variant
assert_fail unlisted-dockerfile "a name that folds to Dockerfile (KELVIN SIGN for K) is listed and refused" "sub/"
repo listed-only
listed listed-only
if [ "$rc" -eq 0 ] && [ -z "$out" ] && [ -z "$err" ]; then
  ok "a repository whose only Dockerfile is declared passes"
else
  ko "a repository whose only Dockerfile is declared passes ($(detail))"
fi
repo symlinked
mv "$WORK/symlinked/Dockerfile" "$WORK/symlinked/real.txt"
ln -s real.txt "$WORK/symlinked/Dockerfile"
listed symlinked
expect_exit2 "a Dockerfile that is a symbolic link is refused" "Dockerfile" "not a regular file"

echo ""
echo "── must pass (exit 0, no output) ──"
dyn pass-digest-bump 'FROM golang:1.27.3-alpine@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb AS builder\n'
expect_pass "$WORK/pass-digest-bump"                "a digest-only bump needs no manifest edit"
dyn pass-patch-bump 'FROM golang:1.27.99-alpine@'"${DIGEST}"' AS builder\n'
expect_pass "$WORK/pass-patch-bump"                 "a same-flavor patch bump needs no manifest edit"
expect_pass pass-scratch                    "FROM scratch, and COPY/mount from an earlier stage alias"
dyn pass-platform-flag 'FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@'"${DIGEST}"' AS builder\n'
expect_pass "$WORK/pass-platform-flag"              "--platform=\$BUILDPLATFORM is a flag, not the image"
expect_pass pass-build-context              "COPY --from= a declared build context"
expect_pass pass-copy-index                 "COPY --from=0 names an earlier stage"
dyn pass-continuation 'FROM golang:1.27.3-alpine@'"${DIGEST}"' \\\n    AS builder\nRUN echo a \\\n    # a comment inside the continuation\n    b\n'
expect_pass "$WORK/pass-continuation"               "a FROM split over a continuation line, and a comment inside one"
expect_pass pass-registry-port              "a registry with a port, localhost, explicit docker.io and a Hub organisation"
expect_pass pass-lowercase                  "lower-case instructions"
dyn pass-directive-after-instruction 'FROM golang:1.27.3-alpine@'"${DIGEST}"' AS builder\n# syntax=docker/dockerfile:9\n# escape=`\n'
expect_pass "$WORK/pass-directive-after-instruction" "# syntax= and # escape= after the first instruction are comments"

dyn syntax-matches '# syntax=docker/dockerfile:1.7\n'"${HEAD_LINE}"'\n' "$MANIFEST_SYNTAX"
expect_pass "$WORK/syntax-matches"          "a # syntax= directive that matches the manifest"
dyn syntax-after-shebang '#!/usr/bin/env -S docker build -f\n# syntax=docker/dockerfile:1.7\n'"${HEAD_LINE}"'\n' "$MANIFEST_SYNTAX"
expect_pass "$WORK/syntax-after-shebang"    "a # syntax= directive below a shebang, where BuildKit still reads it"
dyn lone-backslash "${HEAD_LINE}"'\n\\\n'
expect_pass "$WORK/lone-backslash"          "a file that ends in a lone continuation backslash"
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
