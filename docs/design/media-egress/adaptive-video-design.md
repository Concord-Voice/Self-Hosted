# Adaptive Video / Egress-Reduction Design

**Date:** 2026-06-15 · **Last reconciled with live media code:** 2026-07-15 · **Method:** multi-agent workflow. `feature-dev` explorers mapped the live pipeline (backend SFU, frontend, E2EE, IGNIS). A `system-design`/`architecture` lens produced two designs. An adversarial pass verified every claim against the code. Corrections applied inline.
**Companion to:** `cost-scale-model.md` — video egress is Concord's #1 cost/capacity driver on flat-bandwidth infra.

---

## 0. Bottom line

- **Simulcast is E2EE-safe in this codebase** (verified). #291 ("simulcast breaks AES-GCM") was surmountable, not fundamental. The SFU selects whole SSRCs/frames rather than decrypting payloads. RID stays outside the encrypted frame. H.264 and VP8 use a fresh random IV for every encoded frame and layer. RTP routing metadata never enters AAD. H.264 authenticates only its clear first-slice parser prefix.
- **Caps ≠ egress reduction.** Bitrate/producer caps protect *ingress* and stop abuse. Only **receiver-driven layer-selection** + **visibility-pause** cut the SFU→receiver fan-out that is the actual bill.
- **Delivery status:** visibility pause, server caps, codec-preserving layered publishing,
  receiver-driven selection, audio last-N, and the IGNIS recovery arm have shipped. The original
  ordering below remains useful as the dependency rationale.
- **Previously surfaced crypto drift is now resolved:** live media uses **AES-256-GCM media crypto v5**. AV1 is per-OBU, H.264 is Annex-B/NAL-aware encrypted-suffix, and VP9/VP8/Opus are whole-frame. `join-room.mediaFrameCryptoVersion` accepts `{3, 4, 5}` for rollout but requires an exact occupied-room match. No live-room version ratchet occurs. The load-bearing layering invariants remain codec-specific IV uniqueness, no RTP routing metadata in AAD, and SFU selection without payload decryption.

## 1. Current state (verified against live code)

- **Camera layering is gate-driven.** `voiceService.ts produceVideo()` starts from a **single-layer** default/fallback path, then re-produces into AV1/VP9 SVC or H264/VP8 simulcast when the media-plane gate enables layered publishing. When the gate is disabled, streams remain flat single-layer.
- **IGNIS now samples real decoder intervals and has guarded recovery.** Per-consumer
  `ΔtotalDecodeTime / ΔframesDecoded` histories produce a bounded nearest-rank p95 and interval FPS.
  Missing, stale, paused, reset, first, no-progress, and non-finite derived-metric observations are
  unknown rather than green.
  Layered consumers step down before pausing. After three usable green cycles, recovery releases one
  IGNIS pause while preserving any visibility/manual pause reason. A cycle with no usable sample
  changes neither health nor recovery state.
- **Caps:** a producing transport receives the participant's tier-derived
  `maxManualBitrateBps`. Receive transports retain the global default. This is cooperative WebRTC
  congestion-control feedback, not a hard policer against a patched RTP sender, and worker failure
  is observable fail-open. Room camera/screen counts are also tier-resolved and server-enforced.
- **Screenshare is a separate `source:'screen'` producer**, opt-in (`requiresOptIn`). Consumption is user-controlled via symmetric per-stream and global Tune In / Tune Out controls, with an off-by-default "Automatically tune in to screen shares" preference. Tune-out closes the SFU-side consumer via `close-consumer` (#2088). Good — already separable from webcam.
- **Consumers** are created `paused:true` and resumed by the client. Audio and video ride **split recv transports**, which avoids a BUNDLE payload-type collision under E2EE. `pauseConsumer`/`resumeConsumer` are client- and SFU-callable.
- **E2EE media:** encoded transform on the `RTCRtpSender`, using **AES-256-GCM media crypto v5**. AV1 encrypts coded OBU payloads while preserving parseable OBU structure. H.264 preserves and authenticates its first-VCL header/three-field slice prefix, encrypts the terminal access-unit suffix once, and emulation-prevention-stuffs it for RFC 6184 packetization. VP9/VP8/Opus use whole-frame v5. Whole-frame and AV1 have no AAD. H.264's clear parser prefix is AAD. RID remains an RTP extension, never a crypto input. The media plane accepts rollout versions `{3, 4, 5}` but requires every participant in an occupied room to advertise the same format. The stamped channel `keyVersion` lets receivers derive the required key across mid-session CSK rotations.
- **Project IGNIS** = internally-authored decoder-budget heuristic (`FPS_safe = 0.8×(1000/T_decode_p95)`, `rho = T_d×FPS/1000`) + state machine. **No Sunshine/GPL source** anywhere in the tree (verified grep + the dependency-license audit shows "Strong copyleft: None detected"). The licensing concern is unfounded. Optionally add a provenance note.

## 2. What actually cuts egress (the ingress/egress ledger)

| Mechanism | Cuts SFU→receiver egress? | Notes |
|---|---|---|
| **Receiver-driven layered video select** | ✅ **YES — the big one** | 9-tile gallery: 9×3.5 Mbps → 9×150 kbps *per viewer*. Requires a layered camera producer. |
| **Visibility-pause** (hidden tile/window → pause consumer) | ✅ YES | No E2EE interaction, no publisher cost. Highest ROI; ship first. *(Shipped #1541 — grew from "~20 lines" into a reason-based pause-coordination refactor once #1540's IGNIS pause shared the same `consumer.paused` flag.)* |
| **Audio last-N** (pause non-active speakers' consumers) | ✅ YES *(if prerequisite holds)* | mediasoup `consumer.pause()` *does* signal the SFU to stop forwarding — real egress, **but** gated on §5 verification. |
| Server-side per-tier **bitrate cap** | ❌ caps **ingress** | Stops a free user uploading 16 Mbps; does *not* reduce normal-room fan-out. Abuse/safety, not bill. |
| Publisher/producer-count caps | ❌ ingress/abuse | Bounds the pathological room; protects the box. |

## 3. The receiver-driven selection policy (your design, validated)

Client computes the desired layer per consumer, in precedence order, and signals a requested layer to the SFU (a new `set-preferred-layers {consumerId, spatial, temporal}` Socket.IO event → SFU `consumer.setPreferredLayers`):

1. **Render size** → map tile dimensions to the nearest layer. **Use device pixels** (`tileWidth × devicePixelRatio`) — a 240 CSS-px tile on a 2× display is 480 device px (HiDPI bug if you use CSS px). Pick between the two adjacent layers when in-between, with a **±band + asymmetric debounce** (up-delay > down-delay) to stop resize-flapping.
2. **Focus state** → unfocused/thumbnail downgrades, focused/pinned upgrades.
3. **Network congestion** → overrides both. Deprioritize video for audio and force a lower layer. Per-consumer priority **audio > screenshare > webcam** via `consumer.setPriority()`.

**Keyframe thrash (both designs under-specified this):** a spatial-layer switch needs a keyframe (PLI) on the target SSRC → a visible ~GOP stall on *every* focus change. For active-speaker-driven focus this flashes on every speaker change. Mitigation: **linger the just-stopped speaker on a high layer** (speaker-churn hysteresis — a *different* axis than resize hysteresis), and consume at a **mid layer** on join to avoid a full-bitrate spike. PLI-on-resume after a visibility un-hide too.

## 4. The simulcast tax + the mandatory small-room gate

Simulcast = 3 independent encoder passes **+ 3× per-frame AES-GCM on the sender** (the E2EE transform runs on every frame of every encoding). The byte overhead is ~30%. The **CPU/battery tax is the real cost** (3× encode + 3× encrypt), badly understated in the system design.

**Therefore — gate it:** layered publishing is server-enabled only when at least one active receiver benefits from a lower layer (small/focused-window cap, unfocused tile, congestion, or decoder pressure). SVC (AV1/VP9) can use this benefit gate directly. H264/VP8 simulcast also requires enough camera fan-out (at least three camera producers) to justify the multi-encode tax. No gate = net-negative deployment on small calls (the common case).

**Layered camera publishing and receiver selection must ship together.** Producer layering alone adds cost and delivers little or zero egress reduction — the worst possible half-step.

## 5. P0 prerequisite — VERIFIED (#1543 / S5a, 2026-06-18)

**Result: `ssrc-audio-level` IS negotiated → audio last-N (S5b / #1544) is viable under E2EE.** The SFU's `AudioLevelObserver` works only if the **RFC 6464 audio-level RTP header extension** (`urn:ietf:params:rtp-hdrext:ssrc-audio-level`) is negotiated — under E2EE the audio *payload* is encrypted, so the SFU reads loudness **only from that cleartext header**. It was "not visible" in the router/transport config because mediasoup advertises header extensions **implicitly** via `getSupportedRtpCapabilities()`. Concord declares only `mediaCodecs` and does not strip header extensions, so the audio-level extension passes through to `router.rtpCapabilities.headerExtensions`. Confirmed deterministically by a real-worker test (`services/media-plane/tests/audioLevelHeaderExtension.test.ts`) that boots a real mediasoup router from the production `mediaCodecs` and asserts the recv-capable audio-level extension is advertised (observed `preferredId: 6, direction: 'sendrecv'`). The green media-plane CI check is the standing evidence.

**Accepted-risk note (metadata):** the audio-level header is cleartext by RFC 6464 design, so the SFU operator can observe per-stream *loudness / active-speaker timing* (speaking cadence) — never audio *content*, which stays E2EE-encrypted (AES per-frame). This is inherent to server-side active-speaker detection — the `AudioLevelObserver` already runs — so it is a pre-existing, accepted metadata surface, **not** introduced by this spike or by last-N. Recorded here because #1544 makes the system *rely* on this header.

**Second prerequisite (RESOLVED — shipped in #1544):** the `AudioLevelObserver` was configured `maxEntries: 1` ("Report top 1 speaker"). It is now sized to the resolved last-N cap (`resolveAudioLastN` → free 8) so it surfaces the ranked top-N. The two changes this section anticipated — RFC-6464 confirmation (#1543) **and** the `maxEntries` bump (#1544) — are both done. Audio last-N is **server-authoritative**: the SFU pauses each subscriber's non-top-N mic consumers and a `resumeConsumer` guard makes the cap unbypassable by a misbehaving client (it also composes with server-deafen so moderation is not defeated). See `.claude/rules/domains/media-plane.md` § "Per-room audio last-N".

## 6. Sequenced plan

**A. Now / pre-Jun-21 (cheap, real, low-risk):**
1. **Visibility-pause** — pause a consumer when its tile is `IntersectionObserver`-hidden or `document.hidden`. PLI-on-resume. *Real egress, ship first.* **Shipped in #1541** as a reason-based pause coordinator (`consumerPauseCoordinator.ts`) — the "~20 lines" estimate was obsoleted by #1540's IGNIS decoder pause colliding on `consumer.paused`. See `docs/superpowers/specs/2026-06-15-1541-visibility-pause-coordination-design.md`.
2. **Server-enforced per-tier bitrate ceiling** (free = 1080p30/720p60 envelope on the least-efficient codec) + **tier-aware producer-count caps** (webcam 8 free / 25 paid, screenshare 1 free / 2–3 paid) at `produce`/`/voice/join`. *Ingress/abuse protection — not egress, but stops the box-saturation case.*
3. **IGNIS recovery arm** — **shipped.** It resumes one IGNIS-paused consumer after three usable
   green cycles, and its interval sampler prevents absent/stale counters from manufacturing green.
   Independent of simulcast.
4. **Verify RFC 6464** (§5, #1543 ✅). Audio **last-N** (free 8, with paid 16 deferred behind #1294) via SFU-signaled consumer pause with ~2.5 s hysteresis — **shipped in #1544** (server-authoritative, `resumeConsumer`-guarded, server-deafen-composed).

**B. Next coherent block (the real egress win — ONE PR):**
5. **Codec-preserving layered camera producer (AV1/VP9 SVC, H264/VP8 simulcast, same-codec single-stream fallback) + receiver-driven `setPreferredLayers`**, server-gated (§4), device-pixel thresholds, focus/window caps, pressure step-down, and dual hysteresis (resize + speaker-churn). This is the only thing that turns a gallery from ~31 Mbps/viewer into ~1.3.

**C. Later / product-gated:** large paid galleries (25 active + thumbnails) depend on B. The IGNIS hardware-encode abstraction (AV1 efficiency, per-platform encoders) is a client-efficiency lever exposed under Advanced.

## 7. ADR-worthy decisions + crypto drifts

- **ADR (simulcast-under-E2EE):** record the *correct* safety invariant — H.264/VP8 use a fresh 96-bit random IV per encrypted frame and layer, and every encrypted AV1 OBU uses its own fresh full 96-bit random IV. The SFU selects whole SSRCs/frames, and SSRC/RID stay out of AAD. Do *not* restore session-reset counter prefixes as a nonce domain.
- **Crypto drift #1 (resolved):** live media E2EE now uses **AES-256-GCM v5**, matching `.claude/rules/e2ee.md`. The admission gate accepts `{3, 4, 5}` during rollout but requires an exact occupied-room match in both directions, so a room never silently mixes incompatible formats. The simulcast ADR's load-bearing invariants remain codec-specific IV uniqueness, no RTP routing metadata in AAD, and SFU selection of encrypted frames/packets.
- **Licensing:** IGNIS is original Concord code, with no GPL contamination. Optional provenance note ("heuristic independently derived, not adapted from GPL sources").

## 8. Connection to the gating/cost work

- The **per-tier bitrate ceiling + producer caps** are the media half of the gating epic (#1300 audio/video tier, #601 voice-quality gating) — this analysis reframes them as **capacity/cost controls**, not just monetization, and adds the **free-tier video *concurrency* cap** (net-new, not yet ticketed).
- **1080p/30 free + 720p/60 secondary** stays (beats competitors), made sustainable by the caps + (eventually) layer-selection — *not* by nerfing resolution.
- **The viewport is the fair-use limit — there are no "HD slot" caps.** Because the SFU forwards only what each receiver actually views, at the size it views it (last-N + simulcast layer-select + visibility-pause), egress scales with **attention**, not room population. That is why "native res for a single sub" is affordable without an artificial concurrent-HD cap: *production* is uncapped up to the per-tier bitrate ceiling, while *forwarding* is viewport-bounded and invisible to users. The per-tier bitrate ceilings are defined in the canonical entitlement matrix (Sonic ≤5/2.5 Mbps stream/webcam, Mach server-tier **screen-share** floors ≤5 / ≤9 / ≤16 Mbps for Mach 1/2/3 — **the server never lifts webcam, which stays on the personal tier** — Supersonic native ≤20/6; bitrate is the enforced unit, resolution/fps the label) and enforced server-side by #1300/#1542.
- Storage (#1523) remains immaterial next to video egress.

*Full designs + maps are in the workflow transcript. This record now annotates the original
blueprint with shipped-state corrections verified on 2026-07-15.*
