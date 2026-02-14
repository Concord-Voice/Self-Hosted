import express from 'express';
import { Server as SocketIOServer } from 'socket.io';
import { createServer } from 'http';
import { config } from './config/index.js';
import { logger } from './lib/logger.js';
import { MediasoupService } from './lib/mediasoup.js';

async function main() {
  // Create Express app
  const app = express();
  const httpServer = createServer(app);

  // Middleware
  app.use(express.json());

  // Health check endpoint
  app.get('/health', (req, res) => {
    res.json({
      status: 'healthy',
      service: 'media-plane',
    });
  });

  // Initialize mediasoup
  const mediasoupService = new MediasoupService();
  await mediasoupService.init();

  logger.info('Mediasoup initialized', {
    workers: mediasoupService.getWorkerCount(),
  });

  // Initialize Socket.IO
  const io = new SocketIOServer(httpServer, {
    cors: {
      origin: config.allowedOrigins,
      credentials: true,
    },
  });

  // Socket.IO connection handling
  io.on('connection', (socket) => {
    logger.info('Client connected', { socketId: socket.id });

    // Join room
    socket.on('join-room', async ({ roomId, userId }) => {
      try {
        logger.info('User joining room', { roomId, userId, socketId: socket.id });

        socket.join(roomId);
        socket.data.roomId = roomId;
        socket.data.userId = userId;

        // Get router for this room
        const router = await mediasoupService.getOrCreateRouter(roomId);

        // Send router RTP capabilities
        socket.emit('router-rtp-capabilities', {
          rtpCapabilities: router.rtpCapabilities,
        });

        // Notify others in room
        socket.to(roomId).emit('user-joined', { userId });
      } catch (error) {
        logger.error('Error joining room', { error });
        socket.emit('error', { message: 'Failed to join room' });
      }
    });

    // Create WebRTC transport
    socket.on('create-transport', async ({ roomId, direction }, callback) => {
      try {
        const transport = await mediasoupService.createWebRtcTransport(roomId, direction);

        callback({
          id: transport.id,
          iceParameters: transport.iceParameters,
          iceCandidates: transport.iceCandidates,
          dtlsParameters: transport.dtlsParameters,
        });
      } catch (error) {
        logger.error('Error creating transport', { error });
        callback({ error: 'Failed to create transport' });
      }
    });

    // Connect transport
    socket.on('connect-transport', async ({ transportId, dtlsParameters }, callback) => {
      try {
        await mediasoupService.connectTransport(transportId, dtlsParameters);
        callback({ success: true });
      } catch (error) {
        logger.error('Error connecting transport', { error });
        callback({ error: 'Failed to connect transport' });
      }
    });

    // Produce media
    socket.on('produce', async ({ transportId, kind, rtpParameters }, callback) => {
      try {
        const { producer, roomId } = await mediasoupService.produce(
          transportId,
          kind,
          rtpParameters,
          socket.data.userId
        );

        // Notify other users in the room
        socket.to(roomId).emit('new-producer', {
          producerId: producer.id,
          userId: socket.data.userId,
        });

        callback({ id: producer.id });
      } catch (error) {
        logger.error('Error producing', { error });
        callback({ error: 'Failed to produce' });
      }
    });

    // Consume media
    socket.on('consume', async ({ producerId, rtpCapabilities }, callback) => {
      try {
        const consumer = await mediasoupService.consume(
          socket.data.roomId,
          producerId,
          rtpCapabilities
        );

        if (!consumer) {
          callback({ error: 'Cannot consume' });
          return;
        }

        callback({
          id: consumer.id,
          producerId,
          kind: consumer.kind,
          rtpParameters: consumer.rtpParameters,
        });
      } catch (error) {
        logger.error('Error consuming', { error });
        callback({ error: 'Failed to consume' });
      }
    });

    // Leave room
    socket.on('leave-room', () => {
      const { roomId, userId } = socket.data;

      if (roomId) {
        socket.to(roomId).emit('user-left', { userId });
        socket.leave(roomId);
        logger.info('User left room', { roomId, userId });
      }
    });

    // Disconnect
    socket.on('disconnect', () => {
      const { roomId, userId } = socket.data;

      if (roomId) {
        socket.to(roomId).emit('user-left', { userId });
      }

      logger.info('Client disconnected', { socketId: socket.id, userId });
    });
  });

  // Start server
  const port = config.port;
  httpServer.listen(port, () => {
    logger.info(`Media Plane server started`, {
      port,
      environment: config.environment,
    });
  });

  // Graceful shutdown
  process.on('SIGTERM', async () => {
    logger.info('SIGTERM received, shutting down gracefully');

    await mediasoupService.close();

    httpServer.close(() => {
      logger.info('Server closed');
      process.exit(0);
    });
  });
}

// Start the application
main().catch((error) => {
  logger.error('Fatal error during startup', { error });
  process.exit(1);
});
