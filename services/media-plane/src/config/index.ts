import dotenv from 'dotenv';
import type { RouterRtpCodecCapability } from 'mediasoup/node/lib/rtpParametersTypes.js';

dotenv.config();

// ---------------------------------------------------------------------------
// Audio quality tiers — enforced client-side via producer codecOptions.
// The router advertises the maximum (510 kbps stereo); each producer selects
// its tier by setting maxAverageBitrate, DTX, FEC, and stereo in SDP fmtp.
// ---------------------------------------------------------------------------
export const AUDIO_QUALITY_TIERS = {
  voice: {
    label: 'Voice',
    maxBitrate: 24_000,
    opusDtx: true,
    opusFec: true,
    opusStereo: false,
    description: 'Optimized for voice chat, minimal bandwidth',
    premium: false,
  },
  standard: {
    label: 'Standard',
    maxBitrate: 64_000,
    opusDtx: false,
    opusFec: true,
    opusStereo: true,
    description: 'Good quality stereo, music-friendly',
    premium: false,
  },
  high: {
    label: 'High Quality',
    maxBitrate: 128_000,
    opusDtx: false,
    opusFec: false,
    opusStereo: true,
    description: 'High quality stereo, transparent for most content',
    premium: true,
  },
  hifi: {
    label: 'Hi-Fi',
    maxBitrate: 320_000,
    opusDtx: false,
    opusFec: false,
    opusStereo: true,
    description: 'Near-transparent quality, perceptually lossless',
    premium: true,
  },
  studio: {
    label: 'Studio',
    maxBitrate: 510_000,
    opusDtx: false,
    opusFec: false,
    opusStereo: true,
    description: 'Maximum Opus quality (510 kbps stereo)',
    premium: true,
  },
} as const;

export type AudioQualityTier = keyof typeof AUDIO_QUALITY_TIERS;

// ---------------------------------------------------------------------------
// RTCP feedback common to all video codecs
// ---------------------------------------------------------------------------
const VIDEO_RTCP_FEEDBACK = [
  { type: 'nack' as const },
  { type: 'nack' as const, parameter: 'pli' },
  { type: 'ccm' as const, parameter: 'fir' },
  { type: 'goog-remb' as const },
  { type: 'transport-cc' as const },
];

// ---------------------------------------------------------------------------
// Router media codecs — full codec suite for voice, video & screenshare.
//
// Audio:
//   Opus       — Primary codec, quality controlled per-producer (24-510 kbps)
//   Multiopus  — 5.1 / 7.1 surround (premium)
//
// Video (all with NACK, PLI, FIR, REMB, transport-cc):
//   VP8        — Universal fallback, wide HW decode support
//   VP9        — Default for video calls (SVC L3T3 capable), HDR via Profile 2
//   H264       — Constrained Baseline (compat), Main (5.0), High (5.2 = 4K60)
//   AV1        — Best compression, SVC capable, royalty-free, HDR via Main profile
//
// Strategy (codec preference enforced client-side in voiceService.ts):
//   Camera video     → VP9 SVC L3T3 (3 spatial + 3 temporal layers)
//   Camera fallback  → H264 simulcast (180p / 360p / full) → VP8 simulcast
//   Screen sharing   → AV1 SVC L3T3 (best compression for static content)
//   Screen fallback  → VP9 SVC → H264
//   Premium 4K60     → H264 High Level 5.2 (universal HW), AV1 SVC (better compression)
//   HDR              → VP9 Profile 2 (10-bit), AV1 Main (10-bit capable)
//
// IGNIS insight: 4:2:0 subsampling is sufficient for most use cases. Only
// text-heavy screenshare benefits from 4:4:4. Default to 4:2:0.
// ---------------------------------------------------------------------------
const mediaCodecs: RouterRtpCodecCapability[] = [
  // ── AUDIO ──────────────────────────────────────────────────────────────

  // Opus stereo — router advertises max capability; per-producer narrows
  {
    kind: 'audio',
    mimeType: 'audio/opus',
    clockRate: 48000,
    channels: 2,
    parameters: {
      useinbandfec: 1,        // Forward Error Correction (critical for voice)
      usedtx: 0,              // DTX off by default; voice tier enables per-producer
      stereo: 1,
      'sprop-stereo': 1,
      maxplaybackrate: 48000, // Full-band (20 kHz effective)
      maxaveragebitrate: 510000,
      minptime: 10,           // 10 ms minimum packet time
    },
  },

  // Multiopus 5.1 surround (premium)
  {
    kind: 'audio',
    mimeType: 'audio/multiopus',
    clockRate: 48000,
    channels: 6,
    parameters: {
      channel_mapping: '0,4,1,2,3,5',
      num_streams: 4,
      coupled_streams: 2,
    },
  },

  // Multiopus 7.1 surround (premium)
  {
    kind: 'audio',
    mimeType: 'audio/multiopus',
    clockRate: 48000,
    channels: 8,
    parameters: {
      channel_mapping: '0,6,1,2,3,4,5,7',
      num_streams: 5,
      coupled_streams: 3,
    },
  },

  // ── VIDEO: VP8 — Universal fallback ────────────────────────────────────
  {
    kind: 'video',
    mimeType: 'video/VP8',
    clockRate: 90000,
    parameters: {},
    rtcpFeedback: VIDEO_RTCP_FEEDBACK,
  },

  // ── VIDEO: VP9 Profile 0 — Default (SVC L3T3, 8-bit 4:2:0) ───────────
  {
    kind: 'video',
    mimeType: 'video/VP9',
    clockRate: 90000,
    parameters: {
      'profile-id': 0,
    },
    rtcpFeedback: VIDEO_RTCP_FEEDBACK,
  },

  // ── VIDEO: VP9 Profile 2 — HDR capable (10-bit 4:2:0) ─────────────────
  {
    kind: 'video',
    mimeType: 'video/VP9',
    clockRate: 90000,
    parameters: {
      'profile-id': 2,
    },
    rtcpFeedback: VIDEO_RTCP_FEEDBACK,
  },

  // ── VIDEO: H264 Constrained Baseline — Max compatibility ───────────────
  {
    kind: 'video',
    mimeType: 'video/H264',
    clockRate: 90000,
    parameters: {
      'packetization-mode': 1,
      'profile-level-id': '42e01f', // Constrained Baseline, Level 3.1
      'level-asymmetry-allowed': 1,
    },
    rtcpFeedback: VIDEO_RTCP_FEEDBACK,
  },

  // ── VIDEO: H264 Main — Better compression ─────────────────────────────
  {
    kind: 'video',
    mimeType: 'video/H264',
    clockRate: 90000,
    parameters: {
      'packetization-mode': 1,
      'profile-level-id': '4d0032', // Main profile, Level 5.0
      'level-asymmetry-allowed': 1,
    },
    rtcpFeedback: VIDEO_RTCP_FEEDBACK,
  },

  // ── VIDEO: H264 High — 4K60 capable (Level 5.2) ───────────────────────
  {
    kind: 'video',
    mimeType: 'video/H264',
    clockRate: 90000,
    parameters: {
      'packetization-mode': 1,
      'profile-level-id': '640034', // High profile, Level 5.2
      'level-asymmetry-allowed': 1,
    },
    rtcpFeedback: VIDEO_RTCP_FEEDBACK,
  },

  // NOTE: H265/HEVC is NOT yet in mediasoup's supportedRtpCapabilities
  // (as of 3.13.x). When mediasoup adds it, uncomment and re-enable.
  //
  // {
  //   kind: 'video',
  //   mimeType: 'video/H265',
  //   clockRate: 90000,
  //   parameters: { 'profile-id': 1, 'level-id': 153 },
  //   rtcpFeedback: VIDEO_RTCP_FEEDBACK,
  // },

  // ── VIDEO: AV1 — Best compression, SVC capable ────────────────────────
  // Chrome 90+ encode (software libaom), Chrome 113+ improved real-time.
  // HW encode: NVIDIA RTX 40+, Intel Arc, AMD RX 7000+.
  // Ideal for screen sharing (mostly static, huge compression gains).
  {
    kind: 'video',
    mimeType: 'video/AV1',
    clockRate: 90000,
    parameters: {
      'profile': 0, // Main profile (8-bit)
    },
    rtcpFeedback: VIDEO_RTCP_FEEDBACK,
  },
];

// ---------------------------------------------------------------------------
// Main configuration
// ---------------------------------------------------------------------------
export const config = {
  environment: process.env.ENVIRONMENT || 'development',
  port: parseInt(process.env.PORT || '3000', 10),

  // Redis — room state, cross-instance coordination
  // Default includes dev password to match docker-compose and control plane defaults
  redisUrl: process.env.REDIS_URL || 'redis://:concord_dev_redis@localhost:6379',

  // NATS — inter-service messaging (control plane ↔ media plane)
  natsUrl: process.env.NATS_URL || 'nats://localhost:4222',

  // Control plane — authorization checks, voice join validation
  controlPlaneUrl: process.env.CONTROL_PLANE_URL || 'http://localhost:8080',

  // JWT — shared secret for Socket.IO auth (must match control plane)
  jwtSecret: process.env.JWT_SECRET || 'dev_jwt_secret_change_in_production',

  // ICE servers — returned to clients via control plane /voice/join
  iceServers: [
    { urls: 'stun:stun.l.google.com:19302' },
    { urls: 'stun:stun1.l.google.com:19302' },
    // Production TURN (coturn or Cloudflare):
    // { urls: 'turn:turn.concordvoice.chat:3478', username: '...', credential: '...' },
  ],

  // WebRTC settings
  rtc: {
    announcedIp: process.env.ANNOUNCED_IP || '127.0.0.1',
    minPort: parseInt(process.env.RTC_MIN_PORT || '40000', 10),
    maxPort: parseInt(process.env.RTC_MAX_PORT || '49999', 10),
  },

  // mediasoup settings
  mediasoup: {
    numWorkers: parseInt(process.env.NUM_WORKERS || '4', 10),

    worker: {
      logLevel: (process.env.MEDIASOUP_LOG_LEVEL || 'warn') as mediasoup.types.WorkerLogLevel,
      logTags: [
        'info',
        'ice',
        'dtls',
        'rtp',
        'srtp',
        'rtcp',
      ] as mediasoup.types.WorkerLogTag[],
      rtcMinPort: parseInt(process.env.RTC_MIN_PORT || '40000', 10),
      rtcMaxPort: parseInt(process.env.RTC_MAX_PORT || '49999', 10),
    },

    router: {
      mediaCodecs,
    },

    webRtcTransport: {
      listenIps: [
        {
          ip: '0.0.0.0',
          announcedIp: process.env.ANNOUNCED_IP || '127.0.0.1',
        },
      ],
      // IGNIS insight: generous bitrate headroom — let congestion control find optimal rate
      initialAvailableOutgoingBitrate: 1_000_000, // 1 Mbps initial estimate
      maxIncomingBitrate: 50_000_000, // 50 Mbps cap (supports 4K60)
      enableUdp: true,
      enableTcp: true,  // TCP fallback for restrictive firewalls
      preferUdp: true,  // UDP preferred for lower latency
    },
  },

  // CORS — allowed origins for Socket.IO (env-configurable for LAN/staging)
  allowedOrigins: (process.env.ALLOWED_ORIGINS || 'http://localhost:3001,http://localhost:3002')
    .split(',').map((s: string) => s.trim()).filter(Boolean),

  // Active speaker detection
  audioLevelObserver: {
    maxEntries: 1,       // Report top 1 speaker
    threshold: -60,      // dBV threshold (lower = more sensitive)
    interval: 300,       // Check every 300ms for snappier feedback
  },
};

// Re-export mediasoup types for convenience
import * as mediasoup from 'mediasoup';
export type { mediasoup };
