import dotenv from 'dotenv';

dotenv.config();

export const config = {
  environment: process.env.ENVIRONMENT || 'development',
  port: parseInt(process.env.PORT || '3000', 10),

  // Redis
  redisUrl: process.env.REDIS_URL || 'redis://localhost:6379',

  // Control plane
  controlPlaneUrl: process.env.CONTROL_PLANE_URL || 'http://localhost:8080',

  // WebRTC settings
  rtc: {
    // Announced IP (for Docker/cloud deployment)
    announcedIp: process.env.ANNOUNCED_IP || '127.0.0.1',

    // Port range for RTC
    minPort: parseInt(process.env.RTC_MIN_PORT || '40000', 10),
    maxPort: parseInt(process.env.RTC_MAX_PORT || '49999', 10),

    // Initial available port
    initialAvailablePort: undefined as number | undefined,
  },

  // Mediasoup settings
  mediasoup: {
    // Number of workers (CPU cores)
    numWorkers: parseInt(process.env.NUM_WORKERS || '4', 10),

    worker: {
      logLevel: (process.env.MEDIASOUP_LOG_LEVEL || 'warn') as any,
      logTags: [
        'info',
        'ice',
        'dtls',
        'rtp',
        'srtp',
        'rtcp',
      ],
      rtcMinPort: parseInt(process.env.RTC_MIN_PORT || '40000', 10),
      rtcMaxPort: parseInt(process.env.RTC_MAX_PORT || '49999', 10),
    },

    router: {
      mediaCodecs: [
        {
          kind: 'audio',
          mimeType: 'audio/opus',
          clockRate: 48000,
          channels: 2,
        },
      ],
    },

    webRtcTransport: {
      listenIps: [
        {
          ip: '0.0.0.0',
          announcedIp: process.env.ANNOUNCED_IP || '127.0.0.1',
        },
      ],
      enableUdp: true,
      enableTcp: true,
      preferUdp: true,
    },
  },

  // CORS
  allowedOrigins: [
    'http://localhost:3001',
    'http://localhost:3002',
  ],
};
