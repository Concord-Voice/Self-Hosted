import * as mediasoup from 'mediasoup';
import { config } from '../config/index.js';
import { logger } from './logger.js';
import type {
  Worker,
  Router,
  WebRtcTransport,
  Producer,
  Consumer,
  RtpCapabilities,
  RtpParameters,
  DtlsParameters,
} from 'mediasoup/node/lib/types.js';

export class MediasoupService {
  private workers: Worker[] = [];
  private nextWorkerIdx = 0;
  private routers: Map<string, Router> = new Map();
  private transports: Map<string, { transport: WebRtcTransport; roomId: string }> = new Map();
  private producers: Map<string, { producer: Producer; roomId: string; userId: string }> = new Map();
  private consumers: Map<string, Consumer> = new Map();

  async init() {
    logger.info('Initializing mediasoup workers', {
      numWorkers: config.mediasoup.numWorkers,
    });

    for (let i = 0; i < config.mediasoup.numWorkers; i++) {
      const worker = await mediasoup.createWorker({
        logLevel: config.mediasoup.worker.logLevel,
        logTags: config.mediasoup.worker.logTags as any,
        rtcMinPort: config.mediasoup.worker.rtcMinPort,
        rtcMaxPort: config.mediasoup.worker.rtcMaxPort,
      });

      worker.on('died', () => {
        logger.error('Mediasoup worker died', { pid: worker.pid });
        process.exit(1);
      });

      this.workers.push(worker);

      logger.debug('Mediasoup worker created', {
        pid: worker.pid,
        index: i,
      });
    }
  }

  private getNextWorker(): Worker {
    const worker = this.workers[this.nextWorkerIdx];
    this.nextWorkerIdx = (this.nextWorkerIdx + 1) % this.workers.length;
    return worker;
  }

  async getOrCreateRouter(roomId: string): Promise<Router> {
    let router = this.routers.get(roomId);

    // Evict stale closed routers (room was destroyed and user is rejoining)
    if (router && router.closed) {
      this.routers.delete(roomId);
      router = undefined;
    }

    if (!router) {
      const worker = this.getNextWorker();
      router = await worker.createRouter({
        mediaCodecs: config.mediasoup.router.mediaCodecs as any,
      });

      this.routers.set(roomId, router);

      logger.info('Created router for room', { roomId, workerId: worker.pid });
    }

    return router;
  }

  /** Remove a cached router (called when a room is destroyed) */
  removeRouter(roomId: string): void {
    this.routers.delete(roomId);
  }

  async createWebRtcTransport(roomId: string, direction: 'send' | 'recv'): Promise<WebRtcTransport> {
    const router = await this.getOrCreateRouter(roomId);

    const transport = await router.createWebRtcTransport({
      listenIps: config.mediasoup.webRtcTransport.listenIps,
      enableUdp: config.mediasoup.webRtcTransport.enableUdp,
      enableTcp: config.mediasoup.webRtcTransport.enableTcp,
      preferUdp: config.mediasoup.webRtcTransport.preferUdp,
    });

    this.transports.set(transport.id, { transport, roomId });

    logger.debug('Created WebRTC transport', {
      roomId,
      transportId: transport.id,
      direction,
    });

    return transport;
  }

  async connectTransport(transportId: string, dtlsParameters: DtlsParameters): Promise<void> {
    const entry = this.transports.get(transportId);
    if (!entry) {
      throw new Error('Transport not found');
    }

    await entry.transport.connect({ dtlsParameters });

    logger.debug('Transport connected', { transportId });
  }

  async produce(
    transportId: string,
    kind: 'audio' | 'video',
    rtpParameters: RtpParameters,
    userId: string
  ): Promise<{ producer: Producer; roomId: string }> {
    const entry = this.transports.get(transportId);
    if (!entry) {
      throw new Error('Transport not found');
    }

    const producer = await entry.transport.produce({
      kind,
      rtpParameters,
    });

    this.producers.set(producer.id, {
      producer,
      roomId: entry.roomId,
      userId,
    });

    logger.info('Producer created', {
      producerId: producer.id,
      roomId: entry.roomId,
      userId,
      kind,
    });

    return { producer, roomId: entry.roomId };
  }

  async consume(
    roomId: string,
    producerId: string,
    rtpCapabilities: RtpCapabilities
  ): Promise<Consumer | null> {
    const router = this.routers.get(roomId);
    if (!router) {
      throw new Error('Router not found for room');
    }

    const producerEntry = this.producers.get(producerId);
    if (!producerEntry) {
      throw new Error('Producer not found');
    }

    if (!router.canConsume({ producerId, rtpCapabilities })) {
      logger.warn('Cannot consume', { producerId, roomId });
      return null;
    }

    // Find a receive transport for this room (simplified - in production, manage per-user)
    const recvTransport = Array.from(this.transports.values()).find(
      (t) => t.roomId === roomId
    )?.transport;

    if (!recvTransport) {
      throw new Error('No receive transport available');
    }

    const consumer = await recvTransport.consume({
      producerId,
      rtpCapabilities,
      paused: false,
    });

    this.consumers.set(consumer.id, consumer);

    logger.info('Consumer created', {
      consumerId: consumer.id,
      producerId,
      roomId,
    });

    return consumer;
  }

  getWorkerCount(): number {
    return this.workers.length;
  }

  async close() {
    logger.info('Closing mediasoup service');

    for (const worker of this.workers) {
      worker.close();
    }

    this.workers = [];
    this.routers.clear();
    this.transports.clear();
    this.producers.clear();
    this.consumers.clear();
  }
}
