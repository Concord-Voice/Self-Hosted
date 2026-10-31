# ADR-0022: Re-introduce codec-preserving layered video under E2EE

**Status:** Accepted (design) — revised 2026-07-14 for codec-preserving layering; implementation introduced by #1545 (v1.0) and corrected by #2242 (v0.2.0-Beta), under epic #1538.
**Date:** 2026-06-15
**Design basis:** [`../design/media-egress/adaptive-video-design.md`](../design/media-egress/adaptive-video-design.md) + `../design/media-egress/cost-scale-model.md`, verified against live code by a multi-agent review.

## Context

- **Real-time video egress is Concord's #1 cost/capacity driver.** On flat-bandwidth infra (per #1504) the binding constraint is **Gbps line-rate, not $/GB**, and SFU fan-out is superlinear (`N·(N−1)·B`). A 9-tile gallery costs ~31 Mbps/viewer un-optimized.
- **Layered streaming was removed** (VP9 SVC L3T3, issue #291) under the E2EE-everywhere posture; #1545 reintroduces server-gated camera layering while keeping **single-layer** camera publishing as the default/fallback path. When the gate is disabled, every active, unpaused video consumer receives the sole full-resolution encode for its tile.
- **Verified E2EE mechanics** (`client/desktop/src/renderer/services/e2ee/mediaEncryption.ts`): codec-aware AES-256-GCM via encoded transforms on the `RTCRtpSender`; AV1 encrypts `OBU_FRAME` / `OBU_TILE_GROUP` payloads behind a per-OBU mini-header, H.264 uses Annex-B/NAL-aware encrypted-suffix v5, and VP9/VP8/Opus use whole-frame v5. AV1 structure and RTP header extensions such as RID / Dependency Descriptor remain clear so the SFU can route without decrypting payloads. H.264 authenticates only its parser-visible first-VCL header and three-field slice prefix as AAD.

## Decision

1. **Re-introduce layered camera publishing without changing the selected codec:** AV1/VP9 use SVC when enabled; H264/VP8 use simulcast when enabled and server-authorized. When that codec's layering mode is disabled, the same codec publishes one stream.
2. **Receiver-driven layer selection:** the client reports render-window size (device pixels), focus/window state, and pressure signals; the server clamps the final layer and calls `consumer.setPreferredLayers(...)`.
3. **Server gate + egress governor (mandatory):** server state decides whether layered publishing is useful, then caps each consumer's effective layer. SVC can enable when layered publishing is useful; H264/VP8 simulcast additionally requires enough camera fan-out to justify the multi-encode tax.
4. **Producer layering and receiver selection ship as ONE unit.**

## E2EE invariants (load-bearing — do not violate)

- **IV uniqueness is codec-specific:** VP9/VP8/Opus whole-frame v5 and H.264's one-AEAD-per-access-unit v5 use a fresh 96-bit CSPRNG IV per encoded frame and layer. Every encrypted AV1 OBU also uses a fresh full 96-bit CSPRNG IV; no deterministic prefix resets with a recreated session. v5's random-IV construction plus existing key rotations are operational controls, not a false process-local proof of a global cross-session/device invocation bound. Do not bind RID/SSRC into an IV.
- **Keep RTP routing metadata out of AAD.** Do **NOT** bind SSRC, RID, or Dependency Descriptor into AAD: layer selection needs those fields to remain routing metadata. Whole-frame and AV1 v5 use no AAD; H.264's only AAD is its first VCL NAL header plus clear three-field slice prefix.
- The SFU forwards selected encoded frames/packets without decrypting or rewriting encrypted payload bytes.
- **Media-frame version admission is explicit.** `join-room.mediaFrameCryptoVersion` is checked before authoritative participant storage or epoch mutation. The accepted rollout set is `{3, 4, 5}`; an empty channel room seeds from its first accepted joiner, while an empty DM seeds only when an A2-approved candidate is successfully promoted. Every later authoritative join must exactly match that occupied room. Both mismatch directions receive the same typed `CryptoVersionMismatchError`. Never ratchet a live room to a newer joiner: existing participants would remain on mutually undecryptable formats. v5 retains the v3 key schedule and `keyVersion` while changing H.264 framing. Source of truth: `.claude/rules/media-plane.md` § admission gate.

## Consequences

- **Positive:** receiver-driven selection materially cuts SFU→receiver video egress for still-visible consumers (9-tile gallery ~31 Mbps/viewer → ~1.3); visibility pause and audio last-N also cut aggregate egress by pausing consumers. This unlocks a credible large **paid** gallery (25 active + low-res thumbnails, the model major comms platforms use) while fully preserving E2EE.
- **Cost:** SVC has lower publisher cost than simulcast but still adds encoder complexity and can hit software paths on some clients. H264/VP8 simulcast = multiple encoder passes + multiple E2EE frame-encryption passes, so it remains more tightly gated. Layer switches can need keyframes — mitigated by speaker-churn hysteresis. Use **device pixels** for the size→layer map (HiDPI).
- **Sequencing trap:** layered publishing without receiver selection adds tax for little or zero egress benefit — never ship half.

## Alternatives considered

- **RID-only VP8/H264 simulcast first** — rejected as the primary path: it conflicts with Concord's codec cascade and makes the least-preferred codec the default layered implementation. Use H264/VP8 simulcast only when one of those families is selected; disabling SVC must not replace AV1/VP9.
- **Stay single-layer (status quo)** — rejected: no layer-based bitrate reduction for active, visible consumers; large galleries unaffordable.
- **Cloud-egress hosting** — rejected: flat-bandwidth hosting is the cost basis (#1504); metered cloud egress makes the model non-viable.

## Related

- Epic #1538 (media egress optimization & capacity); #1545 (this decision's implementation); #1300 (per-tier bitrate enforcement, a dependency); #291 (removed layered streaming — **this is net-new, not a reopen**).
- Crypto note: the live media path uses **AES-256-GCM**, matching `.claude/rules/e2ee.md` and message/CSK encryption policy. The load-bearing invariants above remain codec-specific IV uniqueness, no RTP routing metadata in AAD, SFU layer selection without payload decryption, and exact occupied-room version admission; ADR-0022 should not be cited to change them.
