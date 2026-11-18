# Media Frame Authentication Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship media frame crypto v6. Every cleartext byte a receiver hands to a decoder is authenticated, every frame carries an authenticated version and codec scheme, and unauthenticated keyIds cannot grow the key cache or drive unbounded key requests.

**Architecture:** Gate first: a local two-client rig capture on v5 `main` freezes the AV1 and H.264 canonical element sets, and nothing else starts until it passes. Then the client gets a typed reject path, the v6 stamps (mini-header bytes 20 and 21, a 23-byte whole-frame trailer), verify-then-insert key caching, and canonical per-codec AAD builders in a new `mediaFrameAad.ts`. The media plane admits 6. Everything lands in one squash-merged PR.

**Tech Stack:** TypeScript, WebCrypto AES-256-GCM and HKDF, WebRTC encoded transforms, vitest with jsdom (`packages/e2ee-client`), vitest (`services/media-plane`).

**Spec:** `docs/superpowers/specs/2026-11-18-178-media-frame-auth-design.md`

## Global Constraints

- `MEDIA_E2EE_FRAME_CRYPTO_VERSION = 6`. The v5 encode and decode paths are removed. Any version other than 6 is rejected with reason `version`.
- Scheme ids are single-sourced in `workers/e2eeProtocol.ts`: `0x01` whole-frame (VP8, VP9, Opus, unknown), `0x02` AV1 per-OBU, `0x03` H.264 NAL-aware. Any other value is rejected with reason `scheme`.
- Mini-header, 22 B: `[0..2) 0xDE 0xAD · [2..14) IV · [14..16) keyId BE · [16..20) keyVersion BE · [20] version = 6 · [21] scheme`. Bytes `[20..22)` were reserved zero in v5, so a v5 header decodes as version 0.
- Whole-frame trailer, 23 B: `[headerBytes 1][keyId 2 BE][keyVersion 4 BE][IV 12][scheme 1 = 0x01][version 1 = 6][magic 2]`. Version sits at `n-3` and scheme at `n-4`. The minimum frame is 40 B (1 + 16 + 23).
- Decrypt order: a 0-byte frame passes, then stamp, version, scheme and structure checks run, and only then key lookup. The checks throw `MediaFrameRejectedError` before any key lookup, derivation or `requestFrameKey`. They never feed `dropCount` or `attemptRecovery`. Logs fire at the 1st and every 100th reject, carry the reason only, and never include frame bytes or IVs.
- Key cache: `MAX_DECRYPT_KEYS = 1024`, with one insert helper. Only the target keyId is committed, at `now + 3_600_000`, after every operation in the frame succeeds. The ratchet step cap stays at 100.
- Key requests: `FRAME_KEY_MAX_PER_SENDER = 4` live keys, where live means `lastAttempt` is younger than 15_000 ms. Backoff stays at 350 ms, burst at 8, and the global cap at 512.
- Sender identity is bound by the per-sender frame key `HKDF(CSK, info = senderUserId)`, not by AAD.
- AAD never contains raw framing (leb128 sizes, `has_size`, TDs, start-code lengths), RTP routing metadata (SSRC, RID, Dependency Descriptor), or receiver-local state (`frame.type`).
- Media plane: `SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION = 6` and `ACCEPTED = {3, 4, 5, 6}`. The exact-match room rule is unchanged, and the SFU gets no frame-crypto code.
- **No partial v6 on `main`.** Tasks 2–9 land as one squash-merged PR, and only after Task 1 passes. Intermediate branch commits may be partial, but each one must be green.
- A rig mismatch, or a codec that could not be captured, blocks every task, including admission.
- This is a public repo. Commits, PR text and comments cite VULN IDs and behaviour only. Never name the mirrored deployment's repository. Backtick `@names`.
- Quality gate: 0 new blocker or critical issues, 0 new high-confidence security hotspots, and at least 80% coverage of new code.
- Commits use bare `type:` subjects and end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Do not commit the rig patch.
- Client tests run with `cd packages/e2ee-client && npx vitest run <file>`. The full gate is `npm test && npm run typecheck` in that directory. Media-plane tests run with `cd services/media-plane && npx vitest run <file>`.

## Review Focus

1. **Malformed SFU input.** Truncated stamps, a bad leb128, `headerBytes` that runs past the tag, a non-terminal first VCL, or bad stuffing must each be a typed reject. None may become a generic error that reaches `dropCount` and then `attemptRecovery` (Task 3, `rejects malformed input on every branch with a typed reason`).
2. **A non-empty AV1 frame whose canonical list is empty.** A TD-only, padding-only, or TD-plus-padding frame is rejected as `unencrypted`, and the transform never emits a bare `0x12 0x00` (Task 6, `rejects TD-only and padding-only units`).
3. **Partial authentication.** In a ratcheted two-OBU AV1 frame, OBU 0 can pass while OBU 1's tag fails. Nothing may be committed to the cache in that case (Task 6, `partial authentication commits no key`).
4. **Layered AV1.** OBUs with extension bytes arrive as one frame per spatial layer, and the SFU may forward only the base layer. Each layer frame must decrypt on its own (Task 6, `each spatial-layer frame authenticates alone`).
5. **Per-sender slot recovery.** Once a tracked key is delivered or idles for 15 s, that sender's next distinct request posts, so forged churn cannot starve a real rotation past the window (Task 4, `per-sender slots free on clear and after 15 s idle`).

---

### Task 1: Rig capture (STOP gate, never committed)

**Files:**
- Scratch: `/private/tmp/claude-501/-Users-markd--claude-spares-spare-RphBci/b1728b81-06d2-47ef-b02f-bb20f392990c/scratchpad/rig178/` (`instrumentation.patch`, `sender-<codec>.jsonl`, `receiver-<codec>.jsonl`, `compare.py`, `report.md`).
- Patch targets in a fresh Concord-Voice/Desktop worktree at `origin/main` (v5): `desktop/src/renderer/workers/e2eeWorker.ts` (the encrypt and decrypt transforms), plus a codec-forcing hook if Desktop has no dev setting for it.
- Media plane: one local instance from this repo's `main` (v5).

**Interfaces:**
- Produces: `rig178/report.md`. It holds a per-codec table (frames, keyframes, joined %, canonical-identical %, receiver-only elements, observed non-canonical diffs), the frozen values, and a `PASS` or `STOP` verdict. Tasks 6, 7 and 9 consume the frozen values.

- [ ] **Step 1: Tell the lead** that the rig is about to open two Electron client windows (spec decision 3). Do not launch until that message has gone out.
- [ ] **Step 2: Write `instrumentation.patch`.** The worker emits one `RIG178 <json>` console line per frame per side, with no keys, IVs or encrypted plaintext. The sender logs encoder output before encrypt and the receiver logs after depacketize, before decrypt.
  - Join key on both sides: SHA-256 of the first encrypted region (AV1 mini-header plus ciphertext, the stuffed H.264 region, or the whole-frame trailer plus ciphertext). The sender computes it after encrypt.
  - AV1: the ordered OBU list. For each OBU, log type, extension flag and byte, `has_size`, the reserved bit, size-field byte count, and payload length, plus SHA-256 of the payload for clear types. Count header-only units, and record any forbidden (0, 8, 9–14) or metadata types.
  - H.264: the ordered NAL list. For each NAL, log start-code length, `nal_unit_type`, `nal_ref_idc`, body length and trailing-zero count, plus SHA-256 of the body for pre-VCL NALs. Also log SHA-256 of `data[nalOffset_vcl .. clearEnd)`.
  - VP8, VP9 and Opus: SHA-256 and length of the whole encrypted frame.
- [ ] **Step 3: Run the scenario** for each of AV1, H.264, VP8 and VP9, with camera and screen share and layering on and off. Run Opus with DTX on. Each codec gets at least 60 s steady, a mid-call keyframe request, a resolution change, both send directions, and about 5% loss.
  - Set the announced IP, avoid the TURN UDP port collision, and keep the windows unoccluded (see the shared memory on local two-client media traps).
  - Use `dnctl`/`pfctl` for the loss only if `sudo -n` works. Otherwise record "loss not simulated" and raise it in the report.
- [ ] **Step 4: Write `compare.py` (run with `python3 -I`).** It joins on the key, diffs the canonical fields per pair, and lists the non-canonical fields that differ (expected: `has_size`, leb128 length, TD presence, start-code length, trailing zeros).
- [ ] **Step 5: Check the pass criteria (all required).** Each video codec needs at least 1,000 frames and 5 keyframes, and Opus needs at least 3,000 frames. Every receiver frame must join a sender frame. 100% of joined pairs must have an identical canonical `L` (AV1) and identical `P` plus slice prefix (H.264). No receiver-only element may appear, such as a prepended or rewritten SPS/PPS. Whole-frame frames must be byte-identical.
- [ ] **Step 6: Record the frozen values in `report.md`.**
  - AV1: the class table and excluded types, whether the reserved bit and extension byte survive, the TD emission rule, and whether metadata or header-only units occurred (if none did, say "covered by unit vectors only").
  - H.264: the bound pre-VCL NAL set and the start-code and trailing-zero normalisation.
- [ ] **Step 7: Tear down.** Revert the patch, confirm `git status --porcelain` is empty and `git log origin/main..HEAD` is empty in the Desktop worktree, then remove the worktree. The raw JSONL stays in the scratchpad, which is wiped after about 3 days. The report summary goes into the PR body.
- [ ] **STOP GATE:** If any canonical element differs, a codec cannot be captured, or a pass criterion fails, stop and send `report.md` to the lead. Do not exclude the differing element on the spot; per decisions 2 and 3, it is a design gap. No later task starts until the lead accepts a `PASS` report.

### Task 2: Typed frame rejection that never drives key recovery

**Files:**
- Modify: `packages/e2ee-client/src/renderer/services/e2ee/mediaEncryption.ts` (next to `FrameKeyMissError`, around line 262).
- Modify: `packages/e2ee-client/src/renderer/workers/e2eeWorker.ts` (decrypt transform, around lines 352–413).
- Test: `packages/e2ee-client/tests/unit/workers/e2eeWorker.test.ts` (new `describe('e2eeWorker typed frame rejection')`).

**Interfaces:**
- Produces: `export type MediaFrameRejectReason = 'unencrypted' | 'version' | 'scheme' | 'malformed' | 'forbidden-obu' | 'mixed-key'`. This is a closed enum, and the last three are the architect's names for the spec's step-4 cases.
- Produces: `export class MediaFrameRejectedError extends Error { override name = 'MediaFrameRejectedError'; constructor(public readonly reason: MediaFrameRejectReason) }`. The message is `` `E2EE: media frame rejected (${reason})` ``.
- Produces (worker): `function isFrameRejected(err: unknown): err is MediaFrameRejectedError`. It matches on `name`, the same way as `isFrameKeyMiss`. Each decrypt transform also gets a local `rejectCount`.

- [ ] **Step 1: Write the failing tests.**
  - `a rejected frame is never enqueued and never requests keys or recovery`: stub `MediaEncryption.prototype.decryptFrame` to throw `new MediaFrameRejectedError('scheme')` 600 times. Assert 0 enqueued frames, 0 posts of type `requestFrameKey` and 0 of type `requestRecovery` (600 is past both the 50 and 500 thresholds).
  - `logs rejects at the 1st and every 100th with the reason only`: 250 rejects give exactly 3 warn logs. Each one's context equals `{ reason: 'scheme', rejected: n }` for n in {1, 100, 200}, and no key is named `iv` or `data`.
  - `rejects do not advance the drop counter`: send 3 `OperationError`s interleaved with 200 rejects, then 1 good frame. The recovery info log reads `(3 drops, 0 key misses)`.
- [ ] **Step 2: Run** `npx vitest run tests/unit/workers/e2eeWorker.test.ts`. Expect FAIL, because `MediaFrameRejectedError` is not exported yet.
- [ ] **Step 3: Implement.** Add the error class. In the transform's `catch`, check `isFrameRejected` first, before `isFrameKeyMiss`. That branch increments `rejectCount`, logs at 1 and every 100th, and returns.
- [ ] **Step 4: Run again** and expect PASS, then run `npm test` and expect it all green.
- [ ] **Step 5: Commit** `fix: drop rejected media frames without driving key recovery`.

### Task 3: Frame format v6 stamps and pre-key checks

**Files:**
- Modify: `packages/e2ee-client/src/renderer/workers/e2eeProtocol.ts` (`CodecFamily` around line 63, `selectCryptoScheme` around line 89).
- Modify: `packages/e2ee-client/src/renderer/services/e2ee/mediaFrameMiniHeader.ts` (the whole file, including the doc comments at lines 1–17 and 55–56).
- Modify: `packages/e2ee-client/src/renderer/services/e2ee/mediaEncryption.ts` (constants at lines 84–90; the duplicate `CodecFamily` at line 99 becomes an import with `export type { CodecFamily }`; `encryptFrame` and `decryptFrame` at lines 526–539 and 864–878; the whole-frame, AV1 and H.264 decrypt entry checks).
- Create: `packages/e2ee-client/tests/fixtures/media-frame-v6/v5-frames.json`.
- Test: `tests/unit/services/mediaFrameV6.stamp.test.ts` (new), `tests/unit/services/mediaFrameMiniHeader.test.ts`, `tests/unit/workers/e2eeProtocol.test.ts`. Also update `mediaEncryption.test.ts` (line 953 changes to 6; v5-compat blocks are rewritten to v6 or deleted) and `e2eeWorker.test.ts` (line 10's mock constant changes to 6).

**Interfaces:**
- Consumes: `MediaFrameRejectedError` from Task 2.
- Produces (`e2eeProtocol.ts`):
  - `export type CryptoScheme = 'whole-frame' | 'per-obu' | 'nal-aware'`.
  - `export const CRYPTO_SCHEME_ID = { 'whole-frame': 0x01, 'per-obu': 0x02, 'nal-aware': 0x03 } as const satisfies Record<CryptoScheme, number>`.
  - `selectCryptoScheme(codec): CryptoScheme`, with unchanged mapping.
  - `export function assertNeverScheme(s: never): never`.
- Produces (`mediaFrameMiniHeader.ts`): `MediaFrameMiniHeader` gains `version: number; scheme: number`. Encode writes them to `[20]` and `[21]`. Decode returns the raw values and returns `null` only for a short buffer or bad magic.
- Produces (`mediaEncryption.ts`): `MEDIA_E2EE_FRAME_CRYPTO_VERSION = 6`, `WHOLE_FRAME_TRAILER_SIZE = 23`, `MIN_WHOLE_FRAME_SIZE = 40`, and `private static assertStamp(version: number, scheme: number, expected: CryptoScheme): void`, which checks version first and then scheme.
  - Both dispatchers become `switch (selectCryptoScheme(codec))` with a `default: assertNeverScheme(...)`.

- [ ] **Step 1: Capture the v5 fixtures from the current code before editing anything.** Use a scratchpad script with a fixed key and fixed IVs to produce one v5 frame each for whole-frame (Opus), AV1 (sequence header plus `OBU_FRAME`) and H.264 (SPS, PPS, IDR). Write them, with their key, sender and keyId data, to `v5-frames.json`.
- [ ] **Step 2: Write the failing tests.**
  - `advertises crypto version 6`: the version constant is 6.
  - `mini-header carries version at byte 20 and scheme at byte 21`: encoding `{…, version: 6, scheme: 0x02}` gives `[20] === 6` and `[21] === 2`. Decoding 22 bytes with zeros at `[20..22)` returns `version: 0, scheme: 0`.
  - `whole-frame v6 trailer layout`: the output length equals input + 16 + 23, `out[n-4] === 0x01`, `out[n-3] === 6`, and `out[n-2..n)` is `DE AD`.
  - `AV1 and H.264 stamps carry version 6 and their scheme`: the decoded mini-header has `version 6` and scheme `0x02` or `0x03`.
  - `version or scheme mismatch is rejected before key work` (criterion 3). The receiver has **no keys installed**. For each branch, restamp a valid frame with version 5 and then 7, and expect reason `version`. Restamp it with each of the other two scheme ids, and expect reason `scheme`. Also feed each branch real v6 frames of the other two schemes, and expect a `MediaFrameRejectedError` of any reason. Every case must throw that error and never `FrameKeyMissError`. A `vi.spyOn(crypto.subtle, 'deriveKey')` must record 0 calls.
  - `v6 rejects every v5 frame` (criterion 8): each `v5-frames.json` entry is rejected with reason `version`.
  - `rejects malformed input on every branch with a typed reason` (Review Focus 1). Each case expects `MediaFrameRejectedError` and never a `RangeError` or generic `Error`:
    - whole-frame with valid magic, version and scheme but `n = 39`: `malformed`.
    - `headerBytes = 255` on a 60-byte frame: `malformed`.
    - AV1 with a truncated leb128: `malformed`.
    - AV1 with an encrypted-class OBU whose payload is under 22 B: `unencrypted`.
    - H.264 whose first VCL is not terminal: `malformed`.
    - H.264 with bad stuffing: `malformed`.
    - H.264 with no VCL: `malformed`.
- [ ] **Step 3: Run** `npx vitest run tests/unit/services/mediaFrameV6.stamp.test.ts tests/unit/services/mediaFrameMiniHeader.test.ts`. Expect FAIL.
- [ ] **Step 4: Implement.**
  - Whole-frame decrypt checks run in this order: `n === 0` passes; `n < 4` or bad magic gives `unencrypted`; `data[n-3] !== 6` gives `version`; `data[n-4] !== 0x01` gives `scheme`; then `n < 40` or `headerBytes > n - 40` gives `malformed`.
  - AV1 and H.264 throw `malformed` or `unencrypted` for the parse and boundary failures listed in Step 2. They then call `assertStamp` on the located mini-header.
  - Encrypt paths stamp version 6 and their scheme id.
  - Rewrite the doc comments in `mediaFrameMiniHeader.ts` to the v6 layout.
- [ ] **Step 5: Run** `npm test && npm run typecheck`, expecting green. Then run `grep -n 'V5\|= 5;' src/renderer/services/e2ee/mediaEncryption.ts`, which must find no v5 constant.
- [ ] **Step 6: Commit** `feat: stamp media frame crypto v6 version and scheme and reject mismatches before key work`.

### Task 4: Verify-then-insert key cache, cap, and per-sender request bound (VULN-182, VULN-179)

**Files:**
- Modify: `mediaEncryption.ts`: `addDecryptKeyDirect` (around line 335), `addDecryptKeyAtVersion` (around line 383), `rotateKeysOnce` (around line 413), `requireDecryptKey` (around line 704), `deriveRatchetedKey` (around line 814), and the three decrypt paths.
- Modify: `workers/e2eeWorker.ts`: `requestFrameKeyOnce` (around line 283) and its constants (around lines 65–69).
- Test: `tests/unit/services/mediaEncryption.keyCache.test.ts` (new), and the `requestFrameKey retry policy` block of `tests/unit/workers/e2eeWorker.test.ts`.

**Interfaces:**
- Produces:
  - `export const MAX_DECRYPT_KEYS = 1024`.
  - `interface KeyCandidate { mapKey: string; key: CryptoKey }`.
  - `private insertDecryptKey(mapKey: string, key: CryptoKey, expires: number): void`. At the cap and for a new `mapKey`, it first sweeps entries with `expires <= now`, then evicts the smallest `expires` other than `mapKey`.
  - `private async deriveRatchetedKey(senderUserId: string, keyVersion: number, targetKeyId: number): Promise<KeyCandidate | null>`. It never writes to the cache, keeps the cap at 100 steps, and its comment is corrected from 10 to 100.
  - `private async requireDecryptKey(senderUserId: string, keyVersion: number, keyId: number): Promise<{ key: CryptoKey; candidate: KeyCandidate | null }>`.
  - `private commitCandidate(c: KeyCandidate | null): void`. It calls `insertDecryptKey(c.mapKey, c.key, Date.now() + 3_600_000)`.
- Each decrypt path calls `requireDecryptKey` once per frame and calls `commitCandidate` only after every GCM operation succeeds.
- Produces (worker): `export const FRAME_KEY_MAX_PER_SENDER = 4`.

- [ ] **Step 1: Write the failing tests.** Read the private map through a typed cast in the tests, and add no public API.
  - `forged frames never grow the cache` (criterion 5): give the receiver a base key at keyId 0. Send 50 v6 whole-frame frames with random keyIds in 1..100 and random ciphertext. Each one throws, and the map's key set is identical before and after.
  - `a valid ratcheted frame inserts exactly the target key`: the sender is at keyId 5 and the receiver's base is at 0. Decrypt succeeds, the map gains exactly `sender:v:5`, and there are no entries for 1–4.
  - `cap holds at 1024 and evicts the smallest expires` (criterion 6): under fake timers that advance 1 ms per insert, make 1,025 distinct inserts. The size is then 1024, the first insert is gone, and the 1,025th is present.
  - `re-inserting an existing key at the cap evicts nothing`: the size stays 1024 and all other keys stay present.
  - `expired entries are swept before any live eviction`: fill to the cap, advance past 1 h for the first 512 entries, then insert one more. The size is 513 and every live entry survives.
  - `one candidate derivation per frame`: a 3-OBU AV1 frame that ratchets 4 steps should cost 4 ratchet steps, not 12. Count `crypto.subtle.deriveKey` calls (`ratchetKey` runs one per step).
  - Worker, `one sender cycling 100 random keyIds is bounded to 4` (criterion 7): there are exactly 4 `requestFrameKey` posts, and the tracked keys with prefix `s1:` number 4 or fewer. A following `requestFrameKeyOnce('s2', 1, 1)` posts.
  - Worker, `per-sender slots free on clear and after 15 s idle` (Review Focus 5): calling `clearFrameKeyRequest` on one of `s1`'s keys lets a new distinct key post. With all 4 slots live, advance 15_000 ms, and a new distinct key posts.
  - Update `caps the tracking map against a sender churning unique keys` to use 600 senders with 1 key each, so it still exercises the global 512 cap.
- [ ] **Step 2: Run both files** and expect FAIL.
- [ ] **Step 3: Implement.** The per-sender count is a scan of `frameKeyRequests` for live entries with prefix `${sender}:`, which is acceptable at 512 entries or fewer. Suppressed requests are neither posted nor tracked.
- [ ] **Step 4: Run** `npm test`, expecting green. Then run `grep -n 'decryptKeys.set' src/renderer/services/e2ee/mediaEncryption.ts`, which must find exactly one hit, inside `insertDecryptKey`.
- [ ] **Step 5: Commit** `fix: cache only verified media frame keys and bound key requests per sender`.

### Task 5: Whole-frame AAD (VULN-200, media part of VULN-190)

**Files:**
- Create: `packages/e2ee-client/src/renderer/services/e2ee/mediaFrameAad.ts`.
- Modify: `mediaEncryption.ts` (`encryptWholeFrame` around line 542, `decryptWholeFrame` around line 924).
- Create: `tests/fixtures/media-frame-v6/whole-frame.json`.
- Test: `tests/unit/services/mediaEncryption.wholeFrameV6.test.ts` and `tests/unit/services/mediaFrameV6.vectors.test.ts` (both new).

**Interfaces:**
- Produces: `export function wholeFrameAad(header: Uint8Array, trailer: Uint8Array): Uint8Array`, which returns `header || trailer` (the trailer is the full 23 B). The sender builds the trailer before it encrypts.
- Vector fixture fields:
  - `scheme`, `version`, `cskHex` (`000102…1f`), `senderUserId` (`00000000-0000-4000-8000-000000000001`).
  - `frameKeyHex`, `keyId` (7), `keyVersion` (3), `ivsHex[]` (starting at `a0a1…ab`).
  - `inputHex`, `aadHex[]`, `wireHex`, `outputHex`.
- Generate the vectors once with a scratchpad script and commit them. The test only reads them and stubs `crypto.getRandomValues` to return `ivsHex` in order.

- [ ] **Step 1: Write the failing tests.**
  - `round-trips Opus, VP8 and VP9 under v6`: the output equals the input byte for byte.
  - `every cleartext byte is authenticated` (criterion 1): for each index in `[0, headerBytes) ∪ [n-23, n)`, flip one bit and expect the decrypt to reject. Cover Opus (1 header byte) and VP8 (2).
  - `a BUNDLE-misrouted frame still decrypts`: encrypt on an audio-frame object, then decrypt on a video-frame object (`type: 'delta'`, codec `vp8`). The plaintext must match.
  - `whole-frame conformance vector`: encrypt with stubbed IVs and expect `wireHex`. `wholeFrameAad` must equal `aadHex[0]`, and decrypt must equal `outputHex`.
- [ ] **Step 2: Run** both files and expect FAIL, because there is no AAD yet and a tampered header still decrypts.
- [ ] **Step 3: Implement.** Pass `additionalData: wholeFrameAad(header, trailer)` on both sides. The output stays `header || plaintext`.
- [ ] **Step 4: Run** and expect PASS, then run `npm test`.
- [ ] **Step 5: Commit** `fix: authenticate whole-frame media headers and trailer as AAD`.

### Task 6: Canonical AV1 AAD and receiver re-serialization (VULN-181)

**Files:**
- Modify: `packages/e2ee-client/src/renderer/services/e2ee/av1ObuParser.ts` (`ParsedObu` around line 42, the parse loop around lines 110–160).
- Modify: `mediaFrameAad.ts`.
- Modify: `mediaEncryption.ts` (`encryptAv1PerObu` around line 660, `decryptOneObu` and `decryptAv1PerObu` around lines 736–806). Delete the passthrough rationale at lines 773–781.
- Create: `tests/fixtures/media-frame-v6/av1.json`.
- Test: `tests/unit/services/mediaEncryption.av1V6.test.ts` (new), `tests/unit/services/mediaFrameAad.test.ts` (new), `tests/unit/services/av1ObuParser.test.ts`, and the vectors test.
- Read-only source to port: the scan's `exploit_tests/test_vuln_181_av1_passthrough.test.ts`. Never copy the scan folder into the repo.

**Interfaces:**
- Consumes: Task 3's `assertStamp` and `CRYPTO_SCHEME_ID['per-obu']`, and Task 4's `requireDecryptKey` and `commitCandidate`.
- Produces (`av1ObuParser.ts`): `ParsedObu` gains `headerOffset: number; extensionByte: number | null; hasSizeField: 0 | 1`. Also export `AV1_OBU_REDUNDANT_FRAME_HEADER = 7` and `AV1_OBU_TILE_LIST = 8`.
- Produces (`mediaFrameAad.ts`):
  - `export function av1ObuClass(t: number): 'clear' | 'encrypted' | 'excluded' | 'forbidden'`. Type 1 is clear; 3, 4, 5, 6 and 7 are encrypted; 2 and 15 are excluded; 0, 8 and 9–14 are forbidden.
  - `export interface CanonicalObu { obuType: number; extensionByte: number | null; payload: Uint8Array }`.
  - `export function av1CanonicalC(list: readonly CanonicalObu[]): Uint8Array`.
  - `export function av1ObuAad(mini: Uint8Array, k: number, c: Uint8Array): Uint8Array`.
  - `export function leb128Min(n: number): Uint8Array`.
  - `export function av1Reserialize(leadingTd: boolean, list: readonly CanonicalObu[]): Uint8Array`, where each `payload` is plaintext for encrypted OBUs.

The canonical algorithm (this is the part the tests do not determine):

```
L = parsed OBUs in wire order, minus types 2 (TD) and 15 (padding)
canon(o) = (o.obuType << 3) | (o.extensionByte !== null ? 0x04 : 0)   // forbidden, has_size, reserved bits excluded
C = ||_{o in L} canon(o) || [o.extensionByte if present] || cls || (cls == 0x00 ? u32be(len) || payload : <empty>)
    where cls = 0x00 for clear, 0x01 for encrypted
AAD_k = mini_k (22 B, as on wire) || u16be(k) || C    // k = index of the encrypted OBU within L
receiver output = (parsed[0].obuType == 2 ? 0x12 0x00 : <empty>)
                  || ||_{o in L} (canon(o) | 0x02) || [ext] || leb128Min(len') || payload'
```

- Sender: compute `C` from the encoder output. A forbidden type throws a plain `Error`, so the frame is dropped on the encrypt side. Each encrypted OBU gets its own IV and a mini-header with version 6 and scheme `0x02`. TD and padding are copied as they are.
- Receiver order:
  1. A parse failure gives `malformed`.
  2. No encrypted-class OBU in `L`, or a first one that lacks a mini-header, gives `unencrypted`.
  3. Run `assertStamp`.
  4. Any forbidden type gives `forbidden-obu`.
  5. For each encrypted OBU: no magic gives `unencrypted`, a payload under 38 B gives `malformed`, and mini bytes `[14..22)` that differ from the first OBU's give `mixed-key`.
  6. Call `requireDecryptKey` once and run GCM on each OBU with `AAD_k`.
  7. Call `commitCandidate`, then write `frame.data = av1Reserialize(...)`.

- [ ] **Step 1: Write the failing tests.**
  - `round-trips a canonical unit byte-identically`: a TD, a sequence header, and an `OBU_FRAME` with extension byte `0x28`. Every OBU has `has_size = 1`, minimal leb128 and reserved bit 0.
  - `round-trips a header-only show-existing unit`: a TD plus a frame-header OBU. Its clear-header twin is rejected with reason `unencrypted` (criterion 4).
  - `ported VULN-181 exploit is dropped`: a tile-less unit with no encrypted OBU, and the AV1 branch fed a clear VP8 frame, both reject with `MediaFrameRejectedError`.
  - `forbidden types throw on encrypt and reject on decrypt`: for t in {0, 8, 9, …, 14}, encrypt throws. Inserting t beside a valid encrypted OBU gives reason `forbidden-obu`.
  - `tamper of any bound element fails` (criterion 1), as a table, where every row rejects:
    - each OBU's type;
    - the extension byte;
    - each sequence-header payload byte (loop over all of them);
    - swapping the frame-header and tile-group OBUs;
    - inserting a second sequence header;
    - removing the sequence header;
    - each of the 22 bytes of each mini-header.
  - `framing-only changes leave output byte-identical` (criterion 2): the sequence header's size re-encoded as a 3-byte leb128 (for example `0x8A 0x80 0x00` for 10), `has_size = 0` on the last OBU, an inserted padding OBU, or an inserted mid-unit TD. In each case the decrypt output equals the untampered decode.
  - `leading TD added or removed`: decrypt succeeds, the output after the optional leading `0x12 0x00` is byte-identical, and the TD's presence follows the receive rule. (Spec gap: criterion 2 says this case is byte-identical, but the receive rule emits a TD only when one was received. A TD carries no payload, so this test pins the rule.)
  - `mixed mini-header keys reject` with reason `mixed-key`: restamp bytes `[14..16)` of the second OBU.
  - `rejects TD-only and padding-only units` (Review Focus 2): `12 00`, `7A 01 00`, and `12 00 7A 01 00` each give reason `unencrypted`, and `frame.data` is unchanged.
  - `partial authentication commits no key` (Review Focus 3): the receiver's base is at 0 and the sender is at 3. Flip one ciphertext byte of OBU 1. Expect `OperationError`, and the map must not contain `sender:v:3`.
  - `each spatial-layer frame authenticates alone` (Review Focus 4): encrypt the base-layer and enhancement-layer frames separately (extension bytes `0x00` and `0x08`). Each decrypts alone, and moving the enhancement tile OBU into the base frame rejects.
  - `mediaFrameAad` unit tests:
    - `av1ObuClass` over all 16 types.
    - `leb128Min` for 0, 127, 128 and 16383, giving `[0]`, `[0x7f]`, `[0x80, 0x01]` and `[0xff, 0x7f]`.
    - `av1CanonicalC` against a hand-built byte array.
  - Parser tests: the new fields are correct for OBUs with and without extensions.
  - The AV1 conformance vector (sequence header, frame header and tile group; 2 IVs; 2 entries in `aadHex`).
- [ ] **Step 2: Run** the AV1, AAD and parser files. Expect FAIL.
- [ ] **Step 3: Implement** as specified above. Apply the Task 1 frozen values: if the report narrowed the class table or the TD rule, follow the report and update these tests to match.
- [ ] **Step 4: Run** `npm test && npm run typecheck` and expect green.
- [ ] **Step 5: Commit** `fix: bind canonical AV1 OBU structure into AAD and reject unencrypted units`.

### Task 7: H.264 canonical AAD (VULN-199)

**Files:**
- Modify: `mediaEncryption.ts`. `locateH264AccessUnitBoundary` (around lines 216–242) also returns `units` and `firstVclIndex`. `encryptH264AccessUnit` (around line 608) and `decryptH264AccessUnit` (around line 881) change. Leave `h264NalUnit.ts` unchanged unless a helper is needed, and if one is, list it for the Desktop copy.
- Modify: `mediaFrameAad.ts`.
- Create: `tests/fixtures/media-frame-v6/h264.json`.
- Test: `tests/unit/services/mediaEncryption.h264V6.test.ts` (new), `mediaFrameAad.test.ts`, and the vectors test.

**Interfaces:**
- Produces:
  - `export function h264PreVclBodies(data: Uint8Array, units: readonly ParsedH264NalUnit[], firstVclIndex: number): Uint8Array[]`. Each body runs from the NAL header byte to the end of the NAL, with trailing `0x00` bytes stripped.
  - `export function h264CanonicalC(bodies: readonly Uint8Array[], slicePrefix: Uint8Array): Uint8Array`. It returns `||(u32be(|b|) || b) || u32be(|prefix|) || prefix`.
  - `export function h264Aad(mini: Uint8Array, c: Uint8Array): Uint8Array`. It returns `mini (22 B, unstuffed) || C`.
  - `export function h264Reserialize(bodies: readonly Uint8Array[], slicePrefix: Uint8Array, plaintext: Uint8Array): Uint8Array`. It returns `||(00 00 00 01 || b) || 00 00 00 01 || prefix || plaintext`.
- `slicePrefix = data[units[firstVclIndex].nalOffset .. clearEnd)`. Stuffing still keys off the trailing zero run of the slice prefix.
- All pre-VCL NALs are bound, whatever their type, unless Task 1 froze a narrower set.

- [ ] **Step 1: Write the failing tests.**
  - `round-trips AUD+SPS+PPS+SEI+IDR byte-identically`, using 4-byte start codes and no trailing zeros.
  - `round-trips a P-slice unit with no pre-VCL NALs`.
  - `round-trips a slice prefix ending in zero bytes`, which checks that stuffing is unchanged.
  - `tamper of any bound element fails` (criterion 1): each body byte of SPS, PPS, SEI and AUD (in a loop), an inserted SEI, the removed PPS, and each slice-prefix byte (in a loop). Every case rejects.
  - `framing-only changes leave output byte-identical` (criterion 2): 3-byte start codes on any NAL, and trailing `00 00` after the SPS. In both cases the output equals the untampered decode.
  - `mediaFrameAad` unit tests: `h264PreVclBodies` strips trailing zeros and never the NAL header byte, and `h264CanonicalC` matches a hand-built array.
  - The H.264 conformance vector.
- [ ] **Step 2: Run** and expect FAIL.
- [ ] **Step 3: Implement.** The sender builds `C` from its own parse of the encoder output. The receiver builds it from its parse of the wire, never from sender output. After `commitCandidate`, set `frame.data = h264Reserialize(...)`.
- [ ] **Step 4: Run** `npm test && npm run typecheck` and expect green.
- [ ] **Step 5: Commit** `fix: bind every pre-VCL H.264 NAL into the access-unit AAD`.

### Task 8: Media plane admits frame crypto v6

**Files:**
- Modify: `services/media-plane/src/lib/roomManager.ts` (lines 761 and 767). Expect a rebase onto the open media-plane PR that also edits this file.
- Test: `services/media-plane/tests/roomManager.test.ts` (around lines 1520–1660).
- Docs:
  - `CHANGELOG.md` `## [Prerelease]` → `### Changed`.
  - `.claude/rules/domains/media-plane.md` line 468: v6 is current, the accepted set is `{3, 4, 5, 6}`, and the dangling `.claude/rules/e2ee.md` pointer goes to ADR 0022.
  - `docs/architecture.md` line 1098.

**Interfaces:**
- Produces: `SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION = 6` and `ACCEPTED_MEDIA_FRAME_CRYPTO_VERSIONS = new Set([3, 4, 5, 6])`.

- [ ] **Step 1: Write the failing tests** (criterion 9).
  - `parseMediaFrameCryptoVersion(5)` returns 5 and `parseMediaFrameCryptoVersion(6)` returns 6.
  - `parseMediaFrameCryptoVersion(7)` throws `/Unsupported media frame crypto version 7; expected one of 3, 4, 5, 6/`.
  - `a room seeded at 6 rejects a v5 joiner` throws `CryptoVersionMismatchError` with `{ roomVersion: 6, joinVersion: 5 }`, and the reverse case gives `{ roomVersion: 5, joinVersion: 6 }`.
- [ ] **Step 2: Run** `cd services/media-plane && npx vitest run tests/roomManager.test.ts` and expect FAIL.
- [ ] **Step 3: Change the two constants.** Then run `npx vitest run tests/roomManager.test.ts tests/socketJoinLifecycle.test.ts tests/voiceAuthJoinCapacity.test.ts` and expect PASS.
- [ ] **Step 4: Add the CHANGELOG line** `**Voice:** The media plane admits media frame crypto version 6 alongside 3, 4 and 5. Upgrade servers before Desktop clients that send version 6.`, then edit the two docs lines.
- [ ] **Step 5: Commit** `feat: media plane admits media frame crypto v6`.

### Task 9: ADR, architecture and code-comment amendments; freeze the element lists

**Files:**
- `docs/adr/0022-simulcast-under-e2ee.md`:
  - line 11: the verified mechanics.
  - line 22: the IV rule for each v6 scheme.
  - line 23: "use no AAD" becomes the v6 canonical AAD, and the ban on RTP routing metadata stays.
  - line 25: the accepted set becomes `{3, 4, 5, 6}`.
- `docs/adr/0044-codec-admission-policy.md` lines 156–184: the scheme is now authenticated per frame, an unknown codec gets `0x01` through the exhaustive switch, and the HEVC requirement becomes "its own v6 scheme id".
- `docs/architecture.md` lines 1099 and 1106, `.claude/rules/domains/media-plane.md` line 842, and `docs/design/media-egress/adaptive-video-design.md` lines 15, 33 and 90.
- `services/media-plane/src/config/index.ts` line 253: the comment changes to "media frame crypto v6 scheme id".
- The `mediaEncryption.ts` module comment at lines 1–58.
- In the spec, delete the PROVISIONAL markers.
- `CHANGELOG.md` `### Security`.

- [ ] **Step 1: Edit the files.** Every description must match the code as built in Tasks 3–7 and the frozen values in Task 1's report, with no aspirational text. Add the Security line `**Voice:** Media frame crypto v6 authenticates every cleartext byte a receiver decodes, rejects frames that carry no encrypted payload, and bounds the frame-key cache and per-sender key requests.`
- [ ] **Step 2: Verify** with `grep -rn 'whole-frame v5\|crypto v5\|{3, 4, 5}\|PROVISIONAL' docs .claude/rules services/media-plane/src packages/e2ee-client/src`. The only expected hits are historical ADR context that names v5 as the predecessor.
- [ ] **Step 3: Commit** `docs: record media frame crypto v6 in ADRs 0022 and 0044 and the architecture`.

---

### Task 10: v6 rig re-capture (closes the v5-only rig gap)

The Task 1 capture runs on v5, so it cannot show how the transport treats the OBUs v6 newly encrypts: frame header, metadata and redundant frame header. This task re-runs the same rig on the finished v6 build before the pull request is marked ready. It is the last task, and nothing merges without it.

**Files:** none committed. The capture report goes in `/private/tmp/claude-501/-Users-markd--claude-spares-spare-RphBci/b1728b81-06d2-47ef-b02f-bb20f392990c/scratchpad/rig178/v6-report.md`.

- [ ] **Step 1:** Tell the lead before the client windows open, as in Task 1.
- [ ] **Step 2:** Build two Desktop clients from Desktop `main` with this branch's client files copied in, uncommitted. Point both at a local media plane that has Task 8 applied.
- [ ] **Step 3:** For AV1, H.264, VP8, VP9 and Opus, run the Task 1 scenarios at 0% and 5% loss: camera, screen share, keyframe request, and a show_existing_frame source.
- [ ] **Step 4:** Pass criteria:
  - every received frame decrypts, with zero `MediaFrameRejectedError` on honest traffic;
  - for each codec, the receiver's rebuilt frame is byte-identical to the sender's canonical form;
  - video renders, and audio plays without gaps beyond what loss explains.
- [ ] **Step 5:** On any failure, STOP. Report the diff to the lead under the spec's "rig mismatch blocks everything" decision. Do not patch the design in place.
- [ ] **Step 6:** Put the report's summary (codecs, frame counts, results) in the pull request body.

## Companion: mirrored deployment's media-plane admission

The same builder applies Task 8 to the mirrored deployment's media plane: the same two constants in its `roomManager.ts`, the same four tests, and a CHANGELOG line in that repo's own format. It ships as a separate PR. Both media planes must be **deployed** before any v6 client merges. This repo's PR text does not name that repository.

## Companion: Desktop v6 copy (merges only after both media planes deploy)

1. Before copying, `diff` each Desktop file against this repo's pre-change package file on `main`. Hand-merge any pre-existing drift; never overwrite it.
2. Copy every client file this plan changed: `desktop/src/renderer/services/e2ee/{mediaEncryption,mediaFrameMiniHeader,av1ObuParser,mediaFrameAad}.ts`, plus `h264NalUnit.ts` if Task 7 touched it, and `desktop/src/renderer/workers/{e2eeProtocol,e2eeWorker}.ts`. Copy the matching tests and `tests/fixtures/media-frame-v6/`.
3. `desktop/src/renderer/services/voice/voiceService.ts` (around lines 4772–4775) already sends `MEDIA_E2EE_FRAME_CRYPTO_VERSION`, so it now advertises 6.
4. Map the join rejection `Unsupported media frame crypto version 6; expected one of …` to the actionable message "This server needs an update to support voice in this version of Concord." This is a different message from the direction-neutral `CryptoVersionMismatchError` banner.
5. Tests: a `join-room` ack carrying that error shows the update message, and a mismatch ack still shows the existing banner.
6. Desktop ships v6-only, with no v5 fallback. Its release notes give the minimum server version: the first release of this repository that contains Task 8. Add a Desktop CHANGELOG line.
