# Weekly hygiene: Python code-quality findings — Design (ad-hoc, issueless)

Slug: `weekly-hygiene-python-quality`. Branch: `fix/weekly-hygiene-python-quality`. Base: `origin/main` 65646fc8.

## Problem

GitHub Code Quality (CodeQL) reports 78 open `py/*` findings in 17 Python files:

| Rule | Count |
|---|---|
| `py/empty-except` | 20 |
| `py/imprecise-assert` | 20 |
| `py/unused-import` | 17 |
| `py/mixed-returns` | 7 |
| `py/file-not-closed` | 6 |
| `py/mismatched-multiple-assignment` | 4 |
| `py/import-and-import-from` | 2 |
| `py/unused-local-variable` | 2 |

## Decision

Fix every finding in place. Reject a finding only with a technical reason, and record each rejection in `docs/ci/codeql-false-positive-register.md`.

Per-rule fix shape:

- `py/empty-except`: keep the deliberate swallow, narrow the exception type where the raising call allows it, and add a comment that says why the error is safe to ignore. Do not change control flow.
- `py/imprecise-assert`: `assertTrue(a == b)` becomes `assertEqual(a, b)`. Same truth condition, better failure message.
- `py/unused-import`, `py/unused-local-variable`: delete, after checking that no importer reaches the name through the module (for example `support.time`).
- `py/import-and-import-from`: use one import form per module.
- `py/mixed-returns`: add the explicit `return None` (or the value the callers expect) on the fall-through path. No behaviour change.
- `py/file-not-closed`: use a `with` block, or close in `finally`. Keep the file lifetime where a caller still needs the handle.
- `py/mismatched-multiple-assignment`: make the returned tuple and the unpack agree.

`.claude/skills/ste-writing/ste-lint.py` is a synced copy. The fix goes to its canonical source, `Agentic-Dev-Tooling/plugins/writing/skills/ste-writing/ste-lint.py`, in a companion PR. This repo gets the same byte-identical change so the copy stays in sync with what `sync-tooling` installs.

## Scope boundary

Edit only the 17 Python files, the CodeQL register, and the companion repository. Other sessions own `.github/workflows`, `scripts/*.sh`, lockfiles, TypeScript and Go.

## Acceptance criteria

1. Every one of the 78 findings is fixed or rejected with a reason in the register.
2. The Python tests that cover the changed files pass, with the same test count as the baseline on `origin/main`.
3. An adversarial check confirms that each changed test still exercises the same behaviour.
4. CodeQL on the PR head reports none of the fixed findings.
