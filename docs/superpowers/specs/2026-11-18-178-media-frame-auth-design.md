# Media frame authentication and keyId bounds (frame crypto v6)

**Date:** 2026-11-18 · **Status:** Design approved by the project lead. The AV1 and H.264 AAD
element lists stay **provisional** until the rig capture (§ Rig capture) passes.
**Findings:** VULN-181 (High), VULN-199, VULN-200, VULN-182, VULN-179 (Low), and the media-frame
part of VULN-190 (the whole-frame header and trailer outside AAD, the same defect as VULN-200).
**Surfaces:** `packages/e2ee-client` (frame crypto and worker), `services/media-plane` (version
admission), ADR 0022, ADR 0044, and docs. The Desktop copy follows in Concord-Voice/Desktop.

## Intent

The SFU sits between every sender and receiver. Today it can change cleartext frame bytes that
the receiver hands to a decoder without any key. For AV1 it can even inject frames that bypass
decryption completely. This change gives every non-empty frame a decrypted payload plus an AAD
binding for each cleartext element that reaches the decoder. Each frame carries its own
authenticated codec scheme. Unauthenticated keyIds can no longer grow the key cache or drive
unbounded key requests. It all ships as one frame-format bump, v5 to v6.

## Current state (anchors verified on `main` at fe675c2f)

| Area | Behaviour today | Anchor |
|---|---|---|
| Version | `MEDIA_E2EE_FRAME_CRYPTO_VERSION = 5`; whole-frame trailer 22 B with version at `n-3` | `services/e2ee/mediaEncryption.ts:84-90` |
| Dispatch | `encryptFrame`/`decryptFrame` branch on a per-transform `codecFamily` from the first media codec in SFU-supplied `rtpParameters`; `CodecFamily` is declared twice | `mediaEncryption.ts:526-539`, `:864-878`, `:99`; `workers/e2eeProtocol.ts:63`, `:125-134` |
| AV1 | Only `OBU_FRAME`/`OBU_TILE_GROUP` payloads are encrypted, with no AAD. Every other OBU is copied clear. A frame with no tile OBU passes through with no crypto (VULN-181) | `:660-702`, `:736-806`; passthrough rationale `:773-781` |
| H.264 | AAD = `data[firstVcl.nalOffset .. clearEnd)`; SPS/PPS/SEI/AUD before it are unauthenticated (VULN-199) | `:220-240`, `:616-629`, `:881-913` |
| Whole-frame | VP8/VP9/Opus GCM with no `additionalData`; the 1–10 B header and the trailer are unauthenticated (VULN-200) | `:542-605`, `:924-993` (GCM `:986`) |
| Mini-header | 22 B `[magic 2][IV 12][keyId 2][keyVersion 4][reserved 2 = 0]`; reserved bytes are not read on decode | `services/e2ee/mediaFrameMiniHeader.ts:4-16`, `:47-76` |
| Ratchet | `deriveRatchetedKey` writes every intermediate key into `decryptKeys` before any tag check; cap is 100 steps (comment says 10); `decryptKeys` has no size cap (VULN-182) | `:808-855` (insert `:846`), `:284` |
| Key requests | `frameKeyRequests` dedups per `sender:keyVersion:keyId`, backoff 350 ms, burst 8, global LRU cap 512, no per-sender bound (VULN-179) | `workers/e2eeWorker.ts:65-69`, `:283-311`, called `:406` |
| SFU admission | `SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION = 5`, `ACCEPTED = {3,4,5}`, exact match per occupied room | `services/media-plane/src/lib/roomManager.ts:761`, `:767`, `:951-960` |

## Goals

1. Every byte that the receiver hands to a decoder is either GCM plaintext or a deterministic
   re-serialization of AAD-bound elements plus constant framing. No wire byte that isn't
   authenticated reaches a decoder.
2. Every frame authenticates its frame version and codec scheme. A scheme that differs from the
   receive branch, or any version other than 6, is rejected before key lookup.
3. A frame that hasn't authenticated never adds a key-cache entry. The cache has a global cap,
   and key requests are bounded per sender.
4. v5 receivers fail closed on v6 frames, and v6 receivers reject every v5 frame.

## Non-goals and anti-goals

- **Non-goals:** the ratchet key-derivation redesign, tracked separately as the extractable-key
  finding. Media replay protection: the SFU can already replay or drop whole frames. Dropping
  versions 3, 4 and 5 from admission, which is a later change. HEVC, which keeps ADR 0044's
  forward requirement. The native Rust port (Concord-Voice/Desktop#149), which consumes this
  spec's vectors.
- **Anti-goals (a failure even if the tests pass):** binding raw wire framing (leb128 sizes,
  `has_size`, temporal delimiters, start-code lengths) into AAD, because the transport rewrites
  those bytes and every frame would fail. Binding RTP routing metadata (SSRC, RID, Dependency
  Descriptor) into AAD, which ADR 0022 forbids. Building AAD from receiver-local state such as
  `frame.type`, which breaks BUNDLE-misrouting tolerance. Any decode fallback to v5. Any
  passthrough of a non-empty frame.

## Approaches

| | A (amended): canonical AAD per codec, v6 | B: whole-frame for every codec | C: drop-only hardening |
|---|---|---|---|
| What | Keep the per-codec schemes. AAD covers a canonical form of all cleartext structure, plus the version and scheme stamped in the mini-header or trailer | Encrypt everything after one small authenticated header | Drop AV1 frames that have no tile OBU and reject unexpected pre-VCL NALs. No format change |
| Closes | 181, 199, 200, 190 (media) | All, on paper | 181 only. 199 and 200 stay open |
| Cost | Format bump plus a rig capture to freeze the element sets | Format bump. **Breaks transport:** packetizers parse in-frame AV1/H.264 structure, and an earlier whole-frame AV1 format (v3) corrupted every AV1 frame | Small |
| Verdict | **Chosen** | Rejected | Rejected (interim only, and an interim isn't needed) |

## Design

### Frame format v6

- `MEDIA_E2EE_FRAME_CRYPTO_VERSION = 6`. The v5 constants and the v5 decode path are removed.
- **Scheme ids**, single-sourced in `e2eeProtocol.ts` next to `selectCryptoScheme`:
  `0x01` whole-frame (VP8, VP9, Opus, unknown), `0x02` AV1 per-OBU, `0x03` H.264 NAL-aware. Any
  other value is invalid. The duplicate `CodecFamily` at `mediaEncryption.ts:99` becomes an
  import. Encrypt and decrypt dispatch become an exhaustive `switch` over the scheme, with a
  `never` default (the shape ADR 0044 asks for).
- **Mini-header (AV1, H.264), still 22 bytes:** `[0..2) magic 0xDE 0xAD · [2..14) IV ·
  [14..16) keyId BE · [16..20) keyVersion BE · [20] version = 6 · [21] scheme`.
  `decodeMediaFrameMiniHeader` returns `version` and `scheme`. A v5 header (zeros at [20..22))
  therefore decodes as version 0 and is rejected.
- **Whole-frame trailer, now 23 bytes:** `[headerBytes 1][keyId 2 BE][keyVersion 4 BE][IV 12]
  [scheme 1 = 0x01][version 1 = 6][magic 2]`. The version byte stays at `n-3`, so both
  directions fail on the explicit version check. The minimum frame is 1 + 16 + 23 bytes. (Putting
  the scheme in the trailer is the architect's call: the whole-frame format has no mini-header.)
- **Sender identity** is bound by the per-sender frame key (`HKDF(CSK, info = senderUserId)`) and
  isn't repeated in AAD. The worker's encrypt path doesn't reliably know the local userId
  (`initFromKey`). "Sender context" in AAD means the cleartext fields the sender wrote: header
  bytes, `headerBytes`, keyId, keyVersion, scheme and version.

### AAD per codec

Encoding notation: `||` concatenates. `u16be`/`u32be` are big-endian. Every receiver builds its
AAD from parsed wire elements only.

**Whole-frame (scheme 0x01, final):** `AAD = data[0 .. headerBytes) || data[n-23 .. n)`. That is
every cleartext byte of the frame in wire order. Output is unchanged:
`header || plaintext`. A misrouted frame still decrypts, as in v5, because nothing comes from
`frame.type`.

**AV1 (scheme 0x02, PROVISIONAL pending the rig):**
- The canonical OBU list `L` is the parsed OBUs in wire order, excluding temporal delimiter (2) and
  padding (15). The transport strips and reinserts those two types.
- OBU classes: **encrypted payload:** frame header (3), tile group (4), metadata (5), frame (6),
  redundant frame header (7). **Clear and AAD-bound:** sequence header (1). **Forbidden:** 0,
  tile list (8), reserved 9–14. The sender throws, so the frame is dropped. The receiver rejects.
- `canonHeader_j = (obu_type << 3) | (obu_extension_flag << 2)`. The forbidden, `has_size` and
  reserved bits are excluded.
- `C = ||_j ( canonHeader_j || ext_j [only when the flag is set] || cls_j || body_j )`. `cls_j` is
  `0x00` for clear and `0x01` for encrypted. `body_j` is `u32be(len) || payload` for clear OBUs
  and empty for encrypted ones. Each ciphertext is covered by its own tag.
- For the encrypted OBU at canonical index `k`: `AAD_k = mini_k (22 B as on wire) || u16be(k) ||
  C`. The index stops two encrypted OBUs from being swapped.
- **show_existing_frame (decided):** a header-only temporal unit (a frame-header OBU and no tile
  OBU) is legitimate. Frame headers are in the encrypted class, so that unit still carries an
  authenticated element: its frame-header payload is encrypted behind a mini-header. This is
  consistent with v5, which already encrypts the frame header inside `OBU_FRAME`. The receive
  rule is **at least one encrypted OBU per non-empty temporal unit**, not "at least one tile OBU".
- **Receiver output:** `[0x12 0x00 if the first received OBU is a TD]`, then for each `j` in `L`,
  `(canonHeader_j | 0x02) || ext_j? || leb128_min(len'_j) || payload'_j`. Padding OBUs and TDs
  that aren't leading are dropped. The TD emission rule is provisional.

**H.264 (scheme 0x03, PROVISIONAL pending the rig):**
- `P` is the NAL units before the first VCL NAL. `body(u)` runs from the NAL header byte to the
  end of the NAL, with trailing `0x00` bytes stripped (pre-VCL RBSPs end in a stop bit, so a real
  body never ends in zero). Start codes are excluded.
- `C = ||_{u∈P} ( u32be(|body(u)|) || body(u) ) || u32be(clearEnd − nalOffset_vcl) ||
  data[nalOffset_vcl .. clearEnd)`. `AAD = mini (22 B, unstuffed) || C`. This extends the v5 AAD
  back over every byte before `aadStart`, in canonical form.
- **Receiver output:** `||_{u∈P}(00 00 00 01 || body(u)) || 00 00 00 01 ||
  data[nalOffset_vcl .. clearEnd) || plaintext`. Stuffing still keys off the trailing zeros of the
  slice prefix, so it doesn't change.

### Decrypt-side checks and failure behaviour (every scheme, in this order)

1. A 0-byte frame passes. It carries no bytes. This is unchanged.
2. Locate the stamp (whole-frame trailer, the first encrypted OBU's mini-header, or the H.264
   region's mini-header). If it's absent, reject with reason `unencrypted`.
3. `version ≠ 6`: reject with reason `version`. `scheme ≠ branch scheme`: reject with reason
   `scheme`. Both happen **before** any key lookup, derivation or key request.
4. Structure: AV1 rejects on a forbidden type, on zero encrypted OBUs, or when mini-header bytes
   [14..22) differ across the unit's OBUs (one key per frame). For H.264, the existing
   first-VCL-is-terminal rule stays.
5. Key: a cache hit, or a locally derived **candidate** that isn't inserted. If there's no base,
   raise `FrameKeyMissError`, which takes the per-sender-bounded request path.
6. GCM-decrypt each operation with its AAD. On failure, drop through the existing
   `OperationError` / `dropCount` path.
7. Only when every operation in the frame succeeds: commit the candidate, re-serialize, enqueue.

Steps 2–4 throw a new typed `MediaFrameRejectedError` carrying a closed `reason` enum. The worker
counts these in a separate `rejectCount` and logs at the 1st and every 100th occurrence with the
reason, never with frame bytes or IVs. They **don't** feed `dropCount` or `attemptRecovery`: a
key re-request can't fix a format reject, and forged frames must not drive key-recovery traffic.
No non-empty frame is ever enqueued without passing step 7.

### Key cache and key requests (VULN-182, VULN-179; minimal)

- `deriveRatchetedKey` stops writing to `decryptKeys`. It returns a candidate `{ mapKey, key }`.
  The caller commits **only the target keyId**, with an expiry of `now + 3_600_000`, after step 7.
  Intermediate keys are never cached.
- Each frame does at most one candidate derivation (the uniform-key rule in step 4). The step
  cap stays at 100; correct the comment that says 10. Residual: a forged frame can cost at most
  100 ratchet steps of CPU and persists nothing. The full fix is the separate redesign.
- `MAX_DECRYPT_KEYS = 1024`. Every insert (`addDecryptKeyDirect`, `addDecryptKeyAtVersion`,
  `rotateKeysOnce`, commit) goes through one helper. At the cap, the helper sweeps expired
  entries, then evicts the smallest `expires`. It never evicts the entry being inserted.
- `FRAME_KEY_MAX_PER_SENDER = 4` live tracked keys per sender. An entry is live when it is
  younger than `FRAME_KEY_RETRY_RESET_MS` (15 s). A request for a 5th distinct
  `(keyVersion, keyId)` from that sender is suppressed: not posted and not tracked. Backoff
  (350 ms), burst (8) and the global cap (512) stay as they are.

### SFU admission

`roomManager.ts:761` goes from 5 to 6, and `:767` becomes `{3, 4, 5, 6}`. The exact-match room
rule doesn't change, so v5 and v6 never share a room. The same change lands in the mirrored
deployment's media plane. The SFU stays blind: it has no frame-crypto code.

## Rig capture (gates freezing the AV1 and H.264 element lists)

**Setup:** two local desktop clients on one local media plane, running **current v5 `main`**
with a throwaway instrumentation patch that is never committed. For each of AV1, H.264, VP8 and
VP9, force the codec and run camera and screen share, layered on and off. Opus runs with DTX on.
Scenario per codec: at least 60 s steady, a mid-call keyframe request, a resolution change, both
send directions, and about 5% simulated loss (exercises NACK/RTX). Local traps: set the announced
IP, avoid the TURN UDP port collision, and keep windows unoccluded.

**What to log** (JSON lines, one per frame per side). Never log keys, IVs or encrypted-payload
plaintext. Clear elements appear only as length plus SHA-256.
- *Join key, both sides:* SHA-256 of the first encrypted region: the AV1 mini-header plus
  ciphertext, the stuffed H.264 region, or the whole-frame trailer plus ciphertext. A random IV
  makes it unique per frame.
- *AV1 sender (encoder output, before encrypt) and receiver (after depacketize, before decrypt):*
  the ordered OBU list. For each OBU: type, extension flag and byte, `has_size`, reserved bit,
  size-field byte count, payload length, and SHA-256 of the payload for clear types. Also count
  header-only temporal units and record which forbidden or metadata types appear.
- *H.264 sender and receiver:* the ordered NAL list. For each NAL: start-code length, `nal_unit_type`,
  `nal_ref_idc`, body length, trailing-zero count, and SHA-256 of the body for pre-VCL NALs. Also
  SHA-256 of `data[nalOffset_vcl .. clearEnd)`.
- *VP8/VP9/Opus:* SHA-256 and length of the whole encrypted frame on both sides.

**Compare:** join on the key with a small script and diff the canonical fields per pair.
Report the non-canonical fields that differ (expected: `has_size`, leb128 length, TD presence,
start-code length, trailing zeros) as evidence that excluding them is necessary.

**Pass criteria (all required):** at least 1,000 frames and 5 keyframes per video codec, and at
least 3,000 Opus frames. Every receiver frame joins a sender frame. For **100%** of joined pairs,
the canonical form of `L` (AV1) and `P` plus the slice prefix (H.264) is identical. No
receiver-only element appears, such as an SPS/PPS that the receive side prepends or rewrites.
Whole-frame frames are byte-identical.

**Values it fixes:** the AV1 class table and excluded types; whether the reserved bit and the
extension byte survive; the TD emission rule; whether metadata or header-only units occur (if
none occur, unit vectors alone cover them, and the spec records that). For H.264: the bound
pre-VCL NAL set and the start-code and trailing-zero normalisation. On pass, remove the
PROVISIONAL markers in this spec and the ADR text. **On any canonical mismatch, the spec isn't
frozen:** take the diff back to the project lead as a design gap. Don't exclude the differing
element on the spot.

## Rollout

1. **Media plane admits 6:** the server change in this repo plus the same change in the mirrored
   deployment's media plane, deployed wherever they run, **before** any v6 client ships.
2. **Rig capture** (can run in parallel with step 1), then freeze the AV1 and H.264 lists.
3. **Client v6:** `packages/e2ee-client` here; nothing in this repo consumes it at runtime.
   Then the Desktop copy (`mediaEncryption.ts`, `e2eeProtocol.ts`, `e2eeWorker.ts`,
   `mediaFrameMiniHeader.ts`, `av1ObuParser.ts`) and the advertised version in `voiceService`.
   Desktop ships **v6-only**, with no v5 encode or decode.
4. **Later, separate:** drop 3, 4 and 5 from `ACCEPTED`.

**Consequences:** a v6 Desktop can't join a room seeded by a v5 client. It can't join voice at
all on a media plane that hasn't done step 1, because the join is rejected as an unsupported
version. Self-hosted operators must upgrade before their users' Desktop upgrades. The Desktop
release notes state the minimum server version, and the Desktop PR maps that rejection to an
actionable "server needs an update" message.

**Build order:** T1 admission, T2 (v6 stamps, version and scheme checks, whole-frame AAD,
`MediaFrameRejectedError`), T3 (key cache and request bounds) and T4 (the rig) are independent,
so run them in parallel. T5 (AV1 and H.264 canonical AAD and re-serialization) waits on T2 and T4.
T6 (docs and ADRs) rides with T5. T7 (the Desktop copy) follows the merge.

## Tests (acceptance)

In `packages/e2ee-client/tests/unit/` and `services/media-plane/tests/`.
| # | Criterion |
|---|---|
| 1 | **Tamper each AAD-bound field causes decrypt failure.** Whole-frame: each header byte, `headerBytes`, keyId, keyVersion, scheme. AV1: each OBU's type, extension byte, each sequence-header payload byte, swapped encrypted OBUs, an inserted or removed clear OBU, and each mini-header field. H.264: each SPS/PPS/SEI/AUD body byte, an inserted or removed pre-VCL NAL, each slice-prefix byte |
| 2 | **Framing-only tamper leaves output byte-identical** to the untampered decode: non-minimal leb128, a toggled `has_size` on the last OBU, a leading TD added or removed, an inserted padding OBU, 3-byte vs 4-byte start codes, added trailing zeros |
| 3 | **Scheme or version mismatch is rejected before key work:** AV1, H.264 and whole-frame branches each fed the other two schemes, and version 5 or 7 stamps. Each throws `MediaFrameRejectedError` with no candidate derivation and no `requestFrameKey` post |
| 4 | **VULN-181 regression:** the ported exploit (an AV1 frame with no encrypted OBU, plus an AV1 branch fed a clear VP8 frame) is dropped. A forbidden OBU type is dropped. A show-existing unit with an encrypted frame header decrypts, and the same unit with a clear header is dropped |
| 5 | **The cache never holds an unverified key:** after N forged frames with random keyIds inside the step cap, cache size and contents are unchanged. A valid ratcheted frame inserts exactly the target key |
| 6 | **Cap enforced:** 1,025 inserts leave a size of 1,024. The smallest-`expires` entry is evicted, and the newly inserted entry is never the victim |
| 7 | **Per-sender request bound:** one sender cycling 100 random keyIds produces at most 4 tracked keys, and posts stop for new keys. A second sender's request still posts |
| 8 | **A v6 client rejects v5 frames:** v5 whole-frame, AV1 and H.264 fixtures are each rejected with reason `version` |
| 9 | **SFU admits 6:** `parseMediaFrameCryptoVersion(6)` is accepted and `7` is rejected. A room seeded at 6 rejects a v5 joiner, and the reverse holds too |
| 10 | **Conformance vectors:** a fixed key and IV give the expected frame bytes and AAD per scheme, committed as JSON fixtures for the Desktop port. Existing version assertions (`mediaEncryption.test.ts:953`, `e2eeWorker.test.ts:10`) move to 6 |

**Quality gate:** 0 new blocker or critical issues, 0 new high-confidence security hotspots, and
≥ 80% new-code coverage.

## Docs to update (they ship with the code)

- `docs/adr/0022-simulcast-under-e2ee.md`: `:11` (verified mechanics), `:22` (IV per v6 scheme),
  `:23` (replace "use no AAD" with the v6 canonical AAD; the ban on RTP routing metadata stays),
  `:25` (accepted set `{3,4,5,6}`).
- `docs/adr/0044-codec-admission-policy.md` `:156-184`: the scheme is now authenticated per frame.
  An unknown codec gets scheme `0x01` and the exhaustive switch. The HEVC forward requirement
  becomes "its own v6 scheme id".
- `docs/architecture.md` `:1098-1099`; `.claude/rules/domains/media-plane.md` `:468`;
  `docs/design/media-egress/adaptive-video-design.md` `:15`, `:33`, `:90`.
- Code doc comments: `mediaEncryption.ts:1-58`, `mediaFrameMiniHeader.ts:1-17`, `:55-56`.
  `CHANGELOG.md` `[Prerelease]`: the media plane admits frame crypto v6.

## Left to the builder

The module layout (a `mediaFrameAad.ts` for the canonical builders is suggested), helper and
type names, the per-sender index structure for `frameKeyRequests`, fixture file format, and log
wording. Expect a rebase onto the open media-plane PR that also edits `roomManager.ts`.
