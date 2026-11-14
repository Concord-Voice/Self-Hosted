# Base-image identity check — Design (Issue #252)

## Understanding

**Intended outcome.** A pull request that changes which image a Dockerfile stage is built from fails a static check, unless the same pull request changes the declared identity of that stage. The declared identity lives in one reviewed file.

**What is wrong today.** Every `FROM` line pins a tag and a digest, but nothing checks that the image named is the intended one. A change of registry, repository or flavor is well-formed and digest-pinned, so it looks routine in review. The only base-image check runs after a build, on four final stages, and only when a path filter selects them.

**Success looks like.** The check passes on the current tree. Each bad fixture fails with its own message. A digest-only bump and a same-flavor patch bump pass with no manifest edit.

**Constraints.** No Docker daemon, no network, Python standard library only. This is a public repository.

## Decision

Option 1 from the issue's Design Dossier: identity declared in a manifest, shape rules applied to every reference.

Rejected: an inline registry allowlist (cannot fail a flavor swap), and post-build inspection (slow, path-gated, never sees the reference as written).

## Design

### 1. Manifest: `.github/base-images.json`

```json
{
  "schema": 1,
  "dockerfiles": {
    "services/control-plane/Dockerfile.ops-agent": {
      "stages": [
        {"alias": "builder", "registry": "docker.io", "repository": "library/golang", "tag": "^1\\.27\\.[0-9]+-alpine$"},
        {"scratch": true}
      ]
    }
  }
}
```

Per Dockerfile:

- `stages` — one entry per `FROM`, in file order. An image stage has `registry`, `repository` and `tag` (a regular expression that must start with `^` and end with `$`), plus `alias` when the `FROM` has `AS <name>`. A `{"scratch": true}` stage matches `FROM scratch`.
- `build_contexts` — optional list of named build contexts that `COPY --from=` may use (for example `admin_ui`).
- `syntax` — optional. The exact value of a `# syntax=` parser directive.

The key set of `dockerfiles` must equal the set of tracked Dockerfiles.

Tag patterns admit routine bumps and nothing else: a floating tag is matched exactly (`^26-alpine$`), a patch-pinned tag admits any patch (`^1\.27\.[0-9]+-alpine$`). A new minor, major or flavor needs a manifest edit in the same pull request.

### 2. Checker: `.github/scripts/check-base-images.py`

```
check-base-images.py [--root DIR] [--manifest PATH] [FILE...]
```

With no `FILE`, the tracked set is `git ls-files -- '*Dockerfile*'` under `--root` (default: the repository root). With `FILE` arguments, those paths are the tracked set; the test suite uses this. `--manifest` defaults to `<root>/.github/base-images.json`.

Exit codes: `0` pass, `1` one or more violations, `2` the check could not complete (unreadable file, malformed or unanchored manifest, `git` failure). One line per violation: `<file>:<line>: <rule>: <message>`.

Image references are normalized before comparison: the first path component is a registry when it contains `.` or `:` or is `localhost`; otherwise the registry is `docker.io`, and a single-component name gains the `library/` prefix.

Rules:

| Rule | Fails when |
|---|---|
| `unlisted-dockerfile` | a tracked Dockerfile has no manifest entry |
| `stale-entry` | a manifest entry names a file that is not tracked |
| `stage-mismatch` | the number of `FROM` lines, an alias, or scratch-versus-image differs from the manifest; or the arguments of a `FROM` cannot be parsed (a quote or a backslash among them included) |
| `variable-reference` | an image reference contains `$` |
| `missing-tag` | an image reference has no tag |
| `missing-digest` | an image reference has no `@sha256:` digest of 64 hex characters |
| `identity-mismatch` | registry or repository differs from the manifest |
| `tag-mismatch` | the tag does not fully match the manifest pattern |
| `external-copy-source` | `COPY --from=`, `ADD --from=` or `RUN --mount=…from=` names something that is not an earlier stage alias, an earlier stage index (`COPY` and `ADD` only) or a declared build context; any such source in an `ONBUILD` instruction; a quoted or escaped flag word |
| `syntax-directive` | a `# syntax=` directive is absent from the manifest, differs from it, is declared but missing from the file, or is not at the top of the file where BuildKit reads it; a line that starts with `//` |
| `escape-directive` | a `# escape=` directive is present |
| `heredoc` | an instruction contains `<<` |
| `unsupported-character` | the file holds a control character (a lone CR included), or a non-ASCII character outside a comment line |

Parsing choices. The parser splits lines, continuations and flag words the way BuildKit does, and refuses an input it cannot split that way:

- Instructions are case-insensitive. A line that ends in `\` followed by optional spaces or tabs continues on the next line, unless another `\` precedes that backslash. This is BuildKit's rule.
- The file is read as bytes. CRLF is a line end; a lone CR is a control character. Control characters are refused, and so is a non-ASCII character outside a comment line, because Python and BuildKit disagree on which of them are blank or end a word.
- A heredoc is refused. Its body is opaque to a line parser and can hide an instruction or forge one. No tracked Dockerfile uses one.
- A flag word is a word that starts with `--` after quotes and backslashes are removed, which is how BuildKit reads it. A flag word that carries a quote or a backslash is refused under `external-copy-source`.
- A line that starts with `//` fails `syntax-directive`: BuildKit also reads the syntax directive from that form.
- `ONBUILD` is stripped before an instruction is read, so `ONBUILD COPY --from=` is checked.
- `# escape=` is refused because a different escape character would make the continuation rule above wrong.
- A `FROM` that names an earlier stage is not supported. It fails `missing-tag`. No tracked Dockerfile does this.
- `--platform=` and other `FROM` flags are not image references. `$BUILDPLATFORM` there is allowed.

### 3. Where it runs

- **pre-commit:** one hook for the check and one for its self-test. Added in a follow-up pull request after the CI wiring merged. The check hook runs on every commit, because pre-commit leaves deleted files out of its file list and a trigger would miss a deleted or renamed Dockerfile.
- **CI, advisory:** a step in the `workflow-lint` job of `pr-ci.yml` runs the check over the whole tree, and the job's self-test step runs the suite. The `workflow_lint` relevance filter gains `.github/base-images.json`; `.github/scripts/**` is already there. The filter entry for every Dockerfile arrives with the docker Dependabot tier change (#251), so that change should merge first.
- **CI, merge-blocking:** a protected job in `required-pr-build.yml`. That workflow is loaded from protected `main`, so a pull request cannot test its own change to it, and a defect in a new job there blocks every pull request until an organization admin intervenes. It landed in a follow-up pull request, after the advisory step and the pre-commit hooks had run on real pull requests. The job, `base-image-lint`, has no `needs` and no `if`, reads the checker from the protected revision that defines the workflow (`job.workflow_sha`, as `protected-sonar` does, rather than trusting `HEAD^1` to be the base) and runs it with `python3 -I` against the pull request's Dockerfiles and manifest. `result` requires `success`; unlike `migration-lint`, `skipped` blocks.
- **CODEOWNERS:** `/.github/base-images.json` is added, so a manifest edit needs code-owner review. Review of the manifest diff is the control when a pull request changes a Dockerfile and the manifest together.

### 4. Findings recorded by the first manifest

`infrastructure/docker/buildtools/Dockerfile` starts with `# syntax=docker/dockerfile:1.7`. That is an external frontend image with a tag and no digest. The manifest records the directive exactly, so a change to it fails the check. Pinning its digest changes how the buildtools image is built and is left as follow-up work.

### 5. Docs

`docs/development.md`, `docs/runbooks/refresh-buildtools-base.md`, `docs/runbooks/refresh-minio-image.md` and `.github/CONTRIBUTING.md` each say how to change a base image: edit the Dockerfile and the manifest entry in the same pull request.

## Not covered

- A malicious image published under a tag that matches the pattern. There is no signature or provenance check.
- The contents of an image.
- `image:` references in compose files, and images pulled by scripts.
- A pull request that edits the manifest and a Dockerfile together. Code-owner review of the manifest is the control.
- Whether a digest belongs to its tag. A digest-only edit can point a stage at another flavor of the same repository while the tag text stays the same. The check reads text; it does not query a registry.
- A build context supplied from outside the Dockerfile. Compose `additional_contexts` and `docker build --build-context` can point a named context, a stage name or an image name at another image.
- A build from a file whose path does not contain `Dockerfile`, and a Dockerfile written inline by a script.
- `ADD` from a URL or a git repository.

## Amended in the final review

- **Which files are checked.** The tracked set is every tracked path that contains `dockerfile` once compatibility forms, letter case and accents are folded. A case-sensitive listing missed `dockerfile`, which a case-insensitive checkout (macOS, Windows) writes over the real `Dockerfile`. A symbolic link or a submodule entry among those paths ends the check with exit 2. The CI path filter matches any ASCII letter case of the name.
- **Stage index in a mount.** BuildKit accepts a stage index for `COPY` and `ADD` only. In `RUN --mount=…,from=0` the value is an image name, so it is refused.
- **`ONBUILD`.** A trigger runs in the child build and is resolved against the child's stages. Every image source in an `ONBUILD` instruction is refused.
- **`# syntax=` position.** BuildKit reads the directive only while every line above it is a directive (a shebang on line 1 excepted) and the line is unindented. Elsewhere in the leading comment block it is refused, because the build would silently use the built-in frontend.
- **Quoting in `FROM`.** A quote or a backslash in the arguments of `FROM` is refused under `stage-mismatch`.
- **Flag words** are read for `COPY`, `ADD` and `RUN` only.
- **Fixtures.** A case with the standard manifest is one line in the suite. Fixture files end in `.df`, and no fixture path contains `dockerfile`.
- **Not covered, added:** a Compose `build:` entry with a remote `context`, `dockerfile_inline`, or a `dockerfile:` path that does not contain `dockerfile`; a declared build context that the build command does not supply.

## Deviation from the issue

The issue asked for a cross-check of the `os` values in the `container-image-scan.yml` matrix against the manifest. It is left out. The repository and tag pattern already pin the flavor statically, the post-build OS assertion still runs, and the cross-check would need a YAML parser or a fragile text match in a standard-library script.

## Testing

`.github/scripts/tests/check-base-images.test.sh`, hermetic, with one fixture directory per case under `.github/scripts/tests/fixtures/base-images/`. Fixture files are not named `Dockerfile*`.

Must fail, each with its own rule name: digest-only `FROM` with no tag; registry not in the manifest; missing digest; flavor swap under one image name (Alpine to Debian); `ARG`-templated reference; `COPY --from=` an external image; a Dockerfile absent from the manifest; a stale manifest entry; a changed `# syntax=` directive; an `# escape=` directive; an extra stage; an unanchored tag pattern (exit 2).

Must fail, added after the branch review, each an input BuildKit reads differently from a naive line parser: a doubled backslash at line end before a `FROM`; a lone CR; a control byte on a line of its own inside a continuation; a heredoc; a quoted `--from`; a backslash-escaped `--from`; a quoted `from=` field in `--mount`; a `--from` behind a quoted flag that holds a space; a CR or a non-ASCII byte used as a word separator; a `// syntax=` line behind a `//` continuation.

Must pass: the current tree; a digest-only bump; a same-flavor patch bump; `FROM scratch`; a `--platform=$BUILDPLATFORM` flag; a declared build context; a `COPY --from=<index>`; a registry with a port; lower-case instructions; a `# syntax=` after the first instruction; a matching `# syntax=`, also below a shebang; a line continuation inside `FROM`. The suite is the complete list.

Mutation check: disabling any one rule makes that rule's fixture test fail.

## Acceptance criteria

The issue's acceptance criteria apply, with two stated changes: the `os` cross-check is left out, and the "deliberately failing pull request shows the required build result red" criterion can only be met once the protected job is on `main`. This repository configures no quality gate in `.claude/repo-config.json`, so the gate is the existing pre-commit and PR check set.
