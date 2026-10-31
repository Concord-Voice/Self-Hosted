# 0003 — End-to-end encryption is structural, not conditional

## Status

Accepted (2026-05-17) — **delivered**. Implemented under epic #201 with children #1024 (frontend), #1025 (backend), #1026 (migration `000062`), #1027 (media-plane); all closed COMPLETED. The per-server and per-user opt-outs were subsequently removed by migrations `000068` and `000073`.

## Context

Concord shipped alpha and beta with an `is_encrypted` boolean on `channels` and `dm_conversations`. The original intent was developer-visibility scaffolding during alpha — if message delivery worked but E2EE didn't, the team could compare encrypted vs unencrypted channels to isolate which layer was broken.

By v0.2.0-Beta, E2EE has stabilized. The `is_encrypted` flag is now:
- A liability (every message-send code path branches on it; missing the gate leaks plaintext)
- Dead code at the public API level (no channel-creation surface ever exposes the toggle in the UI; it's always `true` in practice)
- An attack surface increment (the flag is one of the things an attacker could plausibly force-flip via a forged WS message)

For a privacy-first product, "encryption is opt-in" is the wrong default. The decision is to remove the flag at every layer.

## Decision

E2EE is structural at v0.2.0-Beta and beyond:

1. **Frontend (#1024):** every send path unconditionally calls `e2eeService.encryptForChannel`. Every receive path unconditionally calls `e2eeService.decryptForChannel`. No per-channel toggle, no conditional fallback to plaintext, no UI for opting out. Fail-closed render-as-error on decryption failure.
2. **Backend (#1025):** WebSocket hub rejects any incoming message lacking `key_version >= 1`. The `is_encrypted` field is dropped from API request/response shapes. Models drop the column references.
3. **Database (#1026):** the `is_encrypted` column is dropped from `channels`, `dm_conversations`, and related tables after a brief data-normalization migration that sets remaining `false` rows to `true`.
4. **Media plane (#1027):** mediasoup's DTLS-SRTP remains structurally mandatory for voice/video; the auth.ts read of channel `is_encrypted` is removed since it was already informational.

> **As built (#3565):** item 1's `encryptForChannel` no longer exists. It returned ciphertext without the epoch that sealed it, and the send paths paired it with a separate version read that could disagree. Every send path now calls `encryptForChannelWithVersion`, and attachment uploads call `getChannelKeyMaterial`; both return the key and its epoch from one call. The decision itself (no per-channel toggle, no plaintext fallback) is unchanged.

Client DM sends always persist and broadcast as `type = 'user'`, regardless of a supplied type. History exempts only schema-valid `call_event` and `expiration_event` metadata with empty content from decryption. Other labels and malformed metadata follow ordinary message authentication, including reply previews; decryption failure never exposes their content. Existing encrypted messages with legacy labels remain readable.

The four changes ship sequentially with an adoption gate between #1024 and #1025: after the v0.1.37 desktop release goes out (frontend stops sending the field), wait ~24-48h for electron-updater propagation before merging the backend change. Otherwise active v0.1.36 clients would silently fail to decrypt.

## Consequences

**Positive:**
- One less attack surface — no toggleable "send plaintext" path
- Code simpler at every layer (gate-collapse removes ~40+ conditional branches in the desktop client alone)
- Privacy-first default aligns with the product positioning

**Negative:**
- Loss of the alpha-era debugging tool (can't compare "this channel encrypted, that one not" anymore to isolate E2EE issues)
- Loss of opt-out for users who might want plaintext for a specific channel (none have asked; #918 startup-race remains the only legitimate uncontrolled-decryption case)
- Brief adoption-gate window where active v0.1.36 clients can fail to decrypt if v0.1.37 hasn't propagated before backend ships — mitigated by the 24-48h electron-updater wait

**Migration:**
- See `docs/superpowers/specs/2026-05-15-201-enforce-e2ee-everywhere-design.md` for the full design including merge ordering, adoption gates, and per-child scope.
- The CLAUDE.md "Phase 3 deferred" line for #201 has been corrected to reflect the v0.2.0-Beta target.

## Related

- Parent epic: #201
- Children: #1024 (this PR), #1025, #1026, #1027
- Sibling carve-out: #1030 (notification content privacy preference; layers on top of the post-#1024 service)
- Design spec: `docs/superpowers/specs/2026-05-15-201-enforce-e2ee-everywhere-design.md`
- Plan (this PR): `docs/superpowers/plans/2026-05-17-1024-remove-is-encrypted-frontend.md`
- Prior art: ADR-0001 (SPA deploy contract), ADR-0002 (Actions-as-SoT deploys)
