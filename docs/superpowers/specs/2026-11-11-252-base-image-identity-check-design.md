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
| `stage-mismatch` | the number of `FROM` lines, an alias, or scratch-versus-image differs from the manifest |
| `variable-reference` | an image reference contains `$` |
| `missing-tag` | an image reference has no tag |
| `missing-digest` | an image reference has no `@sha256:` digest of 64 hex characters |
| `identity-mismatch` | registry or repository differs from the manifest |
| `tag-mismatch` | the tag does not fully match the manifest pattern |
| `external-copy-source` | `COPY --from=` or `RUN --mount=…from=` names something that is not an earlier stage alias, an earlier stage index, or a declared build context |
| `syntax-directive` | a `# syntax=` directive is absent from the manifest, or differs from it |
| `escape-directive` | a `# escape=` directive is present |

Parsing choices, all of them fail-closed:

- Instructions are case-insensitive. A line that ends in `\` followed by optional spaces or tabs continues on the next line, as Docker does.
- Heredoc bodies are not skipped. A body line that looks like `FROM` is counted as a stage and fails `stage-mismatch`. The parser can see more instructions than Docker does, never fewer.
- `# escape=` is refused because a different escape character would make the continuation rule above wrong.
- A `FROM` that names an earlier stage is not supported. It fails `missing-tag`. No tracked Dockerfile does this.
- `--platform=` and other `FROM` flags are not image references. `$BUILDPLATFORM` there is allowed.

### 3. Where it runs

- **pre-commit:** one hook for the check (triggered by any Dockerfile, the manifest or the script) and one for its self-test.
- **CI, advisory:** a step in the `workflow-lint` job of `pr-ci.yml` runs the check over the whole tree, and the job's self-test step runs the suite. The `workflow_lint` relevance filter gains `.github/base-images.json`; `.github/scripts/**` is already there. The filter entry for every Dockerfile arrives with the docker Dependabot tier change (#251), so that change should merge first.
- **CI, merge-blocking:** a protected job in `required-pr-build.yml`. That workflow is loaded from protected `main`, so a pull request cannot test its own change to it, and a defect in a new job there blocks every pull request until an organization admin intervenes. Whether that job lands in this change or in a follow-up after the advisory step has run on real pull requests is the maintainer's decision; it is recorded on the issue. The design of that job, for either answer: no `needs`, no `if`, the checker read from the base commit, the manifest read from the pull request, and `result` requiring `success`.
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

## Deviation from the issue

The issue asked for a cross-check of the `os` values in the `container-image-scan.yml` matrix against the manifest. It is left out. The repository and tag pattern already pin the flavor statically, the post-build OS assertion still runs, and the cross-check would need a YAML parser or a fragile text match in a standard-library script.

## Testing

`.github/scripts/tests/check-base-images.test.sh`, hermetic, with one fixture directory per case under `.github/scripts/tests/fixtures/base-images/`. Fixture files are not named `Dockerfile*`.

Must fail, each with its own rule name: digest-only `FROM` with no tag; registry not in the manifest; missing digest; flavor swap under one image name (Alpine to Debian); `ARG`-templated reference; `COPY --from=` an external image; a Dockerfile absent from the manifest; a stale manifest entry; a changed `# syntax=` directive; an `# escape=` directive; an extra stage; an unanchored tag pattern (exit 2).

Must pass: the current tree; a digest-only bump; a same-flavor patch bump; `FROM scratch`; a `--platform=$BUILDPLATFORM` flag; a declared build context; a line continuation inside `FROM`.

Mutation check: disabling any one rule makes that rule's fixture test fail.

## Acceptance criteria

The issue's acceptance criteria apply, with two stated changes: the `os` cross-check is left out, and the "deliberately failing pull request shows the required build result red" criterion can only be met once the protected job is on `main`. This repository configures no quality gate in `.claude/repo-config.json`, so the gate is the existing pre-commit and PR check set.
