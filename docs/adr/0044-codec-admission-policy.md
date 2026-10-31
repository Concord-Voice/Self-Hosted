# 0044 — Codec admission and licensing policy for the native media engine

**Status:** Accepted
**Date:** 2026-09-10
**Deciders:** project lead (licensing determination); security reviewer (E2EE coupling)
**Issue:** #3240 · **Epic:** #2319

## Context

Epic #2319's native media engine pins its pipeline to NVENC H.264 and AV1. Nothing in the
repository records **which codecs Concord may use, by which implementation path, or why** — so
every codec question in that epic's children re-derives the answer, and two of them (#3241
routing, #3242 E2EE) were about to spend engineering time on HEVC with no written position.

The one place the repository did state a position, `services/media-plane/src/config/index.ts`,
asserted two premises that are now false:

- *"mediasoup lacks support (as of 3.13.x)"* — the pin is `^3.28.0`
  (`services/media-plane/package.json:29`). The version was stale; the **capability claim still
  holds**: mediasoup 3.26.0's (re-checked unchanged at 3.28.0, 2026-10-14) `supportedRtpCapabilities.js` enumerates exactly four video
  codecs — `video/AV1`, `video/H264`, `video/VP8`, `video/VP9` — and no file under `node/lib/`
  mentions H265 or HEVC. There is no worker codec class to negotiate.
- *"Chrome/Firefox refuse to ship it in WebRTC"* — Chrome has shipped **hardware** H.265 in
  WebRTC since M136, with no software fallback. Electron 44 inherits those flags compiled ON.

Three research passes (2026-09-09/10, adversarially verified, merged as PR #3244) established the
licensing landscape. This ADR turns that research into citable policy.

## Decision

**Concord supports HEVC where the hardware already provides it, and never implements it.**

Invoking an OS- or GPU-provided codec is use of an already-licensed capability, not the
distribution of a licensable product. The patent pools license products that *incorporate* an
encoder or decoder; on the hardware path, the device is that product and its vendor is the
licensee. Concord ships no encoder, so Concord incorporates none.

Concord **never ships, vendors, or writes a software HEVC codec.**

### Codec admission table

| Codec | Software implementation | Hardware via OS/GPU API | Licence gate | E2EE scheme (v5) | Status |
|---|---|---|---|---|---|
| Opus | Yes — libopus (BSD); the only audio codec | n/a | none (RFC 6716 grants) | whole-frame | shipped |
| VP8 / VP9 | Yes — libvpx (BSD); WebRTC MTI | where available | none | whole-frame | shipped |
| AV1 | Yes — SVT-AV1 encode, dav1d decode (BSD + AOM patent licence). HW encode on NVENC Ada+, Intel Arc/Lunar+, AMD RDNA4, Apple M4 Ultra / M5 Pro / M5 Max | yes | none from AOM; Sisvel pool noted as residual, industry ships regardless | per-OBU | shipped |
| H.264 | **No software encoder in the sidecar** — Cisco's royalty coverage attaches to its *binary* OpenH264, and libwebrtc compiles OpenH264 from source | yes, universal | hardware-only in the sidecar | Annex-B/NAL access-unit | shipped (browser path) |
| **HEVC** | **Never** | **yes — VideoToolbox, Media Foundation, NVENC/NVDEC, QSV, AMF, VA-API** | none required for platform invocation (see Decision) | **none yet — #3242 owns the arm** | **admitted in principle; gated on #3241 + #3242** |
| VVC | Never | none exists on desktop (Lunar Lake decode only) | same posture as HEVC once hardware encode arrives | none | deferred — watch |
| AV2 | not yet — reference encoder only | none | AOM pattern presumed | none | watch |
| AAC / FLAC | out of scope for the RTP path | — | — | — | rejected for the media path |

Two table entries need their reasoning stated so they are not read as contradictions:

- **H.264 "no software encoder in the sidecar" vs. spec §5.1.** The candidate ladder in
  `docs/superpowers/specs/2026-07-15-native-custom-media-engine-design.md` §5.1 lists `H.264-SW`
  and `VP8-SW` as live tiers. That ladder spans **both** paths — the existing Electron/WebRTC
  path may supply software candidates from libwebrtc's own build, which is not something the
  sidecar ships. This row constrains **the sidecar**, not the ladder.
- **AAC / FLAC.** AAC is pool-encumbered and inferior to Opus for real-time conversational audio;
  FLAC has no packet-loss concealment, no low-delay mode, and no RTP payload format. Neither is a
  candidate, and neither needs a licensing analysis to be excluded.

### Software-codec admission rule

- **Royalty-free codecs** — AV1, VP8, VP9, Opus — may be implemented in software from permissive
  libraries.
- **Patent-encumbered codecs** — HEVC today, VVC when desktop hardware encode exists — are
  **hardware-only**. Concord invokes a platform API; it does not carry an implementation.
- **No GPL, AGPL, or LGPL codec library at runtime** — x265, xin26x, and copyleft FFmpeg builds
  are excluded on the copyright axis independently of any patent question.

**How this rule is actually enforced — and where it is not.** `.github/workflows/license-check.yml`
allowlists licences on the npm and Go dependency axes (npm `--onlyAllow`; Go
`--disallowed_types=restricted,forbidden`, where `restricted` covers GPL/AGPL/LGPL). Two limits
matter and must not be overstated:

1. **It is advisory, not gating.** Its own header states that the Default Branch Ruleset requires
   only `Gitar` and `SonarCloud Code Analysis`, so a red result there is visible, not blocking.
2. **It does not reach the surface this rule governs.** Its path filters cover npm
   manifests/lockfiles, `go.mod`/`go.sum`, `NOTICE.md`, the regenerator, and itself. A vendored
   C/C++ codec linked into a native addon or the media-engine sidecar lives under
   `client/desktop/native/**` and matches no filter and no job. x265 is neither an npm package nor
   a Go module.

**The native/vendored axis is therefore currently unenforced by CI.** This rule is upheld by
review, not by a gate. Closing that gap is out of scope here; #217 owns the dependency-licence
axis, and `docs/legal/dependency-license-audit.md:477` already records that no formal
patent-licensing review was performed. This ADR fills the patent axis for codecs specifically.

### Electron feature-flag posture

`WebRtcAllowH265Send` / `WebRtcAllowH265Receive` and `PlatformHEVCEncoderSupport` /
`PlatformHEVCDecoderSupport` stay at their **compiled default (ON at Chromium 150)**. Concord does
not add a kill switch.

Admission is gated at two places that Concord already owns:

- the **router codec list** (`services/media-plane/src/config/index.ts`), which today contains no
  HEVC entry at all; and
- the **client capability resolver**
  (`client/desktop/src/renderer/services/voice/voiceCodecSelection.ts`), where HEVC is present but
  `selectable: false`.

`--disable-features=WebRtcAllowH265Send,WebRtcAllowH265Receive` remains available as a reversible
off switch if a reason to use one ever appears. None exists today: with no router entry and no
selectable client entry, the flags cannot produce a negotiation.

### libopus 1.5+ — no action for GA

DRED, Deep PLC, and OSCE are **compile-time** features of libopus 1.5. Concord consumes Opus
through Chromium's bundled copy and cannot reach those build flags. Two further facts argue
against pursuing them now: Opus 1.6 broke DRED bitstream compatibility, and DRED is still
unstandardised.

**Revisit trigger:** the native audio path (#3243), which is the first point at which Concord
would control an Opus build.

### AV2 — watch item

The AV2 specification reached v1.0.0 on 2026-05-28. There is a reference encoder and nothing else.

**Re-evaluation trigger — both required:** an RTP payload format, **and** either a real-time
software encoder or hardware encode.

## Options considered

**A. Hardware-only HEVC, capability-negotiated — SELECTED.** Complexity medium (two children).
Cost is engineering only. Matches the posture Chromium, major comms platforms, and OBS's hardware paths already
take. Fails closed: both ends must report hardware, otherwise the session falls back to
AV1 / VP9 / H.264 / VP8.

**B. No HEVC at all.** Complexity low. Leaves the 8-bit AV1 ceiling and the HDR gap unaddressed on
hardware where HEVC Main 10 is the only accelerated 10-bit path — Apple Silicon before M4 Ultra
has no AV1 encode at all. Rejected: it forgoes the capability the native engine exists to unlock.

**C. Software HEVC under a pool licence.** Complexity high, recurring cost, and every available
software HEVC library is GPL-family (x265) or otherwise licence-poisoned. Rejected outright — it
is the one option that would make Concord an incorporating product.

## Consequences

**Easier.** Every codec question in #2319 cites one document instead of re-deriving it. The router
note stops asserting two false premises.

**Harder.** HEVC is not real until two children land, and they are sequenced: #3241 forks mediasoup
with an H265 codec class behind a router flag, and #3242 designs the HEVC arm of media frame crypto
v5. Until both merge, HEVC stays `selectable: false` on the client — which is **already its state
today**, so this ADR records current behaviour rather than requesting a change.

**Revisit when:** VVC desktop hardware encode ships · AV2 gets an RTP payload format and a
real-time encoder · the native audio path (#3243) lands and Concord controls an Opus build.

## Security considerations

**Media frame crypto v5 dispatches per codec, and an unrecognised codec is admitted to a scheme
that was not designed for it rather than refused.** `.claude/rules/e2ee.md:404` routes VP9, VP8,
Opus, **and unknown byte-transparent codecs** to whole-frame v5, and the dispatch is an
if/if/fall-through rather than an exhaustive switch (`workers/e2eeProtocol.ts` —
`codecFamilyFromMimeType` returns `undefined` for anything outside its five literals, and
`selectCryptoScheme(undefined)` yields `whole-frame`). Nothing in the repository enforces "no
scheme entry, therefore reject".

**State the consequence precisely, because this document will be cited.** Whole-frame v5 encrypts
the entire payload less a static short header, so an unscheme'd codec routed there leaks **no
plaintext** — it is if anything more opaque than the H.264 arm, which leaves SPS/PPS in the clear
by design. The failure is one of **framing and decode correctness**, not confidentiality: this is
CWE-693 (protection-mechanism failure), not a cryptographic-disclosure finding.

For HEVC that fall-through is **not benign.** HEVC is Annex-B/NAL, like H.264 — and the H.264 arm
(`.claude/rules/e2ee.md:425-440`) exists precisely because whole-frame encryption of an Annex-B
stream can synthesise a false `00 00 01` start code (hence its reversible emulation-prevention
stuffing) and conceals the SPS/PPS that libwebrtc must parse. Admitting HEVC without its own
scheme entry routes a NAL codec down the byte-transparent path that the H.264 work already
demonstrated is wrong for it.

**This ADR therefore imposes a forward requirement, and binds it to #3241 rather than #3242.**
An admitted codec is not negotiable until it has its own v5 scheme entry — and the issue that
must honour that is the one which *arms* the hazard, not the one which closes it. **The router
`mediaCodecs` list is the admission boundary for both directions.** #3241 must not merge an
`video/H265` entry — **even flag-default-off** — before #3242 has registered the HEVC arm.
#3242 owns writing that arm; the enforcement shape it should adopt is converting the
`encryptFrame`/`decryptFrame` fall-through into an exhaustive switch with a `never` default, so
the next codec cannot be added silently.

**The margin today is ONE gate, not three — do not read the client resolver as a backstop.**
Three restraints exist, and only one of them is load-bearing:

- The router `mediaCodecs` list has no HEVC entry, **and** mediasoup 3.26.0 (and 3.28.0) has no H265 worker
  codec class. These are **not independent**: mediasoup's `ortc.js` throws
  `UnsupportedError('media codec not supported')` at router creation for any `mediaCodecs` entry
  absent from `supportedRtpCapabilities`, so the list cannot be opened without the codec class.
  #3241 opens both in a single change.
- HEVC is `selectable: false` in the client resolver (`voiceCodecSelection.ts:157,168`, enforced
  at `:240`, with `:291` returning false). **This governs the local send path only.** The decrypt
  path resolves the *sender's* codec from `consumer.rtpParameters`
  (`services/voice/voiceService.ts:8530`) and never consults the resolver — so once the router
  admits H265, an honest receiver whole-frame-decrypts a peer's HEVC regardless of its own
  setting. There is no receive-side gate.

So #3241 alone is sufficient to arm this. That is why the requirement above binds #3241.

No new dependency is introduced. Relevant to OWASP A06 (vulnerable and outdated components).

## Evidence pack

The licensing research itself lives in the four reports under `docs/superpowers/reports/` that
this ADR's References section links; it is not restated here. What follows is only what a reader
*citing this decision* needs and cannot get from the sections above.

- **The licensee roster is a premise, not a verified fact.** Apple, Microsoft, and Google appear
  on the Via LA / VCL Advance roster. **NVIDIA, Intel, and AMD licensee status is unverified** —
  the Access Advance roster was unreachable during research, and the project lead's premise is
  that per-unit hardware royalties make GPU vendors licensees. Since 2025-12-15 Access Advance
  also operates the former Via LA HEVC/VVC pool, so a written enquiry has one counterparty; it
  remains available at the project lead's discretion and is not a precondition for this ADR.
- **JSF++ does not reach a media-engine sidecar.** `.claude/rules/native-audio.md` binds JSF AV
  C++ Coding Standards Rev C to the `rt/` real-time core of the audiocap addon
  (`client/desktop/native/concord-audiocap/rt/`); `napi/` is explicitly outside it. No rule yet
  covers a media-engine sidecar, which does not exist. A future one needs its own decision, not
  an assumed inheritance.
- **One stale artefact contradicts this ADR's premise if read in isolation.**
  `client/desktop/src/renderer/services/e2ee/mediaEncryption.ts:110` carries an `h265: 2` entry in
  `UNENCRYPTED_BYTES`. It is dead — `getUnencryptedBytes` returns a static value and nothing reads
  it — but a reader checking "does HEVC already have a v5 entry?" against the crypto module will
  find one. #3242 should delete it alongside writing the real arm.

## Related

- #2319 — Epic: native custom media engine (outbound screen sidecar)
- #3241 — Spike: fork mediasoup with an H265 codec class behind a router flag — **gate 1 for HEVC**
- #3242 — Design: HEVC arm of media frame crypto v5 — **gate 2 for HEVC**
- #3243 — Design: full-pipeline native media engine phasing — libopus revisit trigger
- #2320 / #2321 / #2322 — sibling de-risking children
- #2209 — explicit codec preference during screen layering (current Chromium pipeline; not absorbed here)
- #217 — dependency licence audit (copyright axis; this ADR owns the patent axis for codecs)
- ADR-0043 — the JSF++-constrained native addon this ADR's scope note refers to

## References

**Internal** — `.claude/rules/e2ee.md` § Media frame crypto v5 · `.claude/rules/media-plane.md`
§ Profile-aware room codec floor · `.claude/rules/native-audio.md` · `docs/legal/dependency-license-audit.md`
· `.github/workflows/license-check.yml` · `docs/superpowers/specs/2026-07-15-native-custom-media-engine-design.md` §5

**Research** (merged in PR #3244, under `docs/superpowers/reports/`) —
`2026-09-09-2319-codec-license-fact-sheet.md` · `2026-09-10-2319-codec-deep-research.md` ·
`2026-09-10-2319-codec-research-addendum.md` · `2026-09-10-2319-codec-research-final-pass.md`

**External** — Access Advance HEVC Advance programme and FAQ · Via LA HEVC/VVC licensee roster ·
Chromium blink-dev Intent to Ship WebRTC H.265 (M136) · Chromium `media/webrtc/webrtc_features.cc`
@ 150.0.7871.250 · RFC 7798 (HEVC RTP) · RFC 9628 (VP9 RTP) · RFC 6716 / RFC 7587 (Opus) · AV1 RTP
payload spec v1.0.0 · AV2 spec v1.0.0 (2026-05-28) · libopus 1.5 release notes · JSF AV C++ Coding
Standards Rev C
