# Weekly hygiene: code smells — Design (ad-hoc, issueless, 2026-11-18)

Scope: every OPEN SonarCloud issue on `Concord-Voice_Self-Hosted` (27 at base `65646fc8`) and the
GitHub Code Quality (CodeQL) JS/Go findings #1–#9 and #88. Dependencies, lockfiles, Python and
workflow files are out of scope (other sweeps own them).

## Decisions

| Group | Findings | Approach |
|---|---|---|
| Regex backtracking (`typescript:S8786`) | `client/admin/src/webauthn.ts`, `packages/e2ee-client/.../base64url.ts` | Replace `/=+$/g` with a linear trailing-`=` trim. Output stays byte-identical for every input. A unit test pins it. |
| `self` → `globalThis` (`typescript:S7764`) | `e2eeWorker.ts` ×3 | As built: false positive under calibration `typescript:S7764` (a). The `globalThis` change was tried and reverted, because CodeQL then raised `js/missing-origin-check` on the worker's message handler. |
| Dedicated matchers (`typescript:S5906`) | 16 test assertions | Use `toHaveLength` / `toBeNull`. The two `mediaEncryption.test.ts` cases assert `AesKeyAlgorithm.length`, a key-size field; the agent decides if `toHaveLength` is correct or if a property assertion is better. Either result fixes the smell. |
| Go smells | `godre:S8196`, `go:S3776` | Rename the single-method interface to the `-er` form. Extract helpers to get cognitive complexity to 15 or less, with no behaviour change. |
| Calibrated FPs | `docker:S8431` ×4, `go:S1313` ×1 | Existing register calibrations hold (tag = Dependabot lineage anchor; RFC1918 dev-only fallback). Flip per instance and append rows (renumbered #92–#96 after a merge from main). |
| CodeQL | #1–#9, #88 (as built: #9 won't fix. The hash-pinned generator forces an 8.25 MB vector redraw, and the required custom Semgrep scan times out on it.) | Fix each one (dead assignment, duplicate object-literal key, unused variable, impossible null comparison). For `js/missing-await` in `redis.ts` / `nats.ts`, find out if the un-awaited promise is a deliberate fire-and-forget. If it is, use `void` with an attached error handler. If it is not, await it. |

## Acceptance criteria

- Each finding is fixed, or rejected with a technical reason and a register row.
- Typecheck, lint and tests pass for e2ee-client, media-plane, admin, and control-plane (`go vet`, `go test` on the touched packages).
- The base64url change has a pinning test.
- `e2ee-reviewer` reviews the e2ee-client changes at Phase 8.
