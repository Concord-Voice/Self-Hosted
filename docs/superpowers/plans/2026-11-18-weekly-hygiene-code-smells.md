# Weekly hygiene: code smells — Plan (2026-11-18)

Spec: `docs/superpowers/specs/2026-11-18-weekly-hygiene-code-smells-design.md`.

Execution: a Workflow. It has four file-disjoint fix groups and one adversarial verify pass. The
orchestrator owns the registers and the Sonar status flips. Fix agents do not edit the registers.

1. **e2ee-client**: `base64url.ts` (S8786, plus a pinning test), `e2eeWorker.ts` (S7764 ×3), and the S5906 assertions in `mediaEncryption.test.ts`, `crypto.test.ts` and `shamir.test.ts`. Verify: e2ee-client typecheck and vitest.
2. **admin + media-plane tests**: `webauthn.ts` (S8786, plus a pinning test). S5906 in `cameraLayerPolicyParity`, `roomManager` and `voiceAuthWorkloadEvidence`. CodeQL #4–#8 (overwritten properties, unused variable). Verify: admin and media-plane typecheck, lint and tests.
3. **media-plane src + scripts**: CodeQL #1, #2 (missing-await), #3 (`voiceAuthReceiver` null compare), #9 (`gen.mjs`). Verify: media-plane typecheck, lint and tests. As built: `gen.mjs` pins its own hash, so `vectors.json` is regenerated and checked with `--check`. #1–#3 are false positives (CodeQL register).
4. **Go**: `admin/middleware.go` (S8196), `websocket/handler.go` (S3776), and `dm/handlers.go` (CodeQL #88). Verify: gofmt, `go vet`, and `go test` on the touched packages.
5. **Adversarial verify**: one agent checks every diff hunk for behaviour drift and incomplete fixes.
6. **Orchestrator**: flips Sonar S8431 ×4 and S1313 ×1 to false positive and appends rows #92–#96 (renumbered after a merge from main), plus #97–#99 for S7764. It records any rejection that the fix groups return.
