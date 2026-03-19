import type {
  Router,
  WebRtcTransport,
  Producer,
  Consumer,
  RtpCapabilities,
  RtpParameters,
  DtlsParameters,
  AudioLevelObserver,
} from 'mediasoup/node/lib/types.js';
import type { MediaKind } from 'mediasoup/node/lib/rtpParametersTypes.js';
import { config } from '../config/index.js';
import { logger } from './logger.js';
import { MediasoupService } from './mediasoup.js';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/** Media source identifier — matches client-side appData.source */
export type MediaSource = 'mic' | 'camera' | 'screen' | 'screen-audio';

export interface Participant {
  userId: string;
  socketId: string;
  username: string;
  displayName?: string;
  avatarUrl?: string;
  sendTransport: WebRtcTransport | null;
  /** Multiple recv transports (keyed by transport ID) — supports PiP windows */
  recvTransports: Map<string, WebRtcTransport>;
  producers: Map<string, { producer: Producer; source: MediaSource; kind: MediaKind }>;
  consumers: Map<string, Consumer>;
  rtpCapabilities: RtpCapabilities | null;
  joinedAt: Date;
}

export interface Room {
  id: string;
  router: Router;
  audioLevelObserver: AudioLevelObserver | null;
  participants: Map<string, Participant>;
  createdAt: Date;
  isEncrypted: boolean;
  e2eeEpoch: number;
}

export interface ProducerInfo {
  producerId: string;
  userId: string;
  kind: MediaKind;
  source: MediaSource;
}

export interface TransportOptions {
  id: string;
  iceParameters: unknown;
  iceCandidates: unknown;
  dtlsParameters: unknown;
}

// ---------------------------------------------------------------------------
// Events emitted by RoomManager for NATS/Redis integration (A5-A7)
// ---------------------------------------------------------------------------

export type RoomEvent =
  | { type: 'user-joined'; roomId: string; userId: string; username: string; displayName?: string }
  | { type: 'user-left'; roomId: string; userId: string }
  | { type: 'room-empty'; roomId: string }
  | { type: 'producer-added'; roomId: string; userId: string; producerId: string; kind: MediaKind; source: MediaSource }
  | { type: 'producer-removed'; roomId: string; userId: string; producerId: string; kind: MediaKind; source: MediaSource }
  | { type: 'active-speaker'; roomId: string; userId: string; volume: number };

export type RoomEventHandler = (event: RoomEvent) => void;

// ---------------------------------------------------------------------------
// RoomManager
//
// Manages the lifecycle of voice/video rooms (one per voice channel):
//   - Router creation (1 per room, assigned round-robin to mediasoup workers)
//   - Participant join/leave with proper transport cleanup
//   - Per-participant send/recv transports (fixes the consumer transport bug)
//   - Producer/consumer management with source tracking (mic/camera/screen)
//   - AudioLevelObserver for active speaker detection
//   - Event emission for NATS/Redis integration
// ---------------------------------------------------------------------------

export class RoomManager {
  private rooms: Map<string, Room> = new Map();
  private mediasoup: MediasoupService;
  private eventHandlers: RoomEventHandler[] = [];

  constructor(mediasoup: MediasoupService) {
    this.mediasoup = mediasoup;
  }

  /** Register an event handler for room events (used by NATS/Redis integration) */
  onEvent(handler: RoomEventHandler): void {
    this.eventHandlers.push(handler);
  }

  private emitEvent(event: RoomEvent): void {
    for (const handler of this.eventHandlers) {
      try {
        handler(event);
      } catch (err) {
        logger.error('Room event handler error', { event: event.type, error: err });
      }
    }
  }

  // ─── Room lifecycle ──────────────────────────────────────────────────

  /** Get or create a room for the given channel ID */
  private async getOrCreateRoom(roomId: string): Promise<Room> {
    let room = this.rooms.get(roomId);
    if (room) {
      // Defensive: if the router was closed (e.g. race between leave and rejoin),
      // discard the stale room and recreate it below.
      if (room.router.closed) {
        logger.warn('Room has closed router, discarding stale room', { roomId });
        this.rooms.delete(roomId);
        room = undefined;
      } else {
        return room;
      }
    }

    const router = await this.mediasoup.getOrCreateRouter(roomId);

    // Create AudioLevelObserver for active speaker detection
    let audioLevelObserver: AudioLevelObserver | null = null;
    try {
      audioLevelObserver = await router.createAudioLevelObserver({
        maxEntries: config.audioLevelObserver.maxEntries,
        threshold: config.audioLevelObserver.threshold,
        interval: config.audioLevelObserver.interval,
      });
    } catch (err) {
      logger.warn('Failed to create AudioLevelObserver', { roomId, error: err });
    }

    room = {
      id: roomId,
      router,
      audioLevelObserver,
      participants: new Map(),
      createdAt: new Date(),
      isEncrypted: false,
      e2eeEpoch: 0,
    };

    // Wire up active speaker events
    if (audioLevelObserver) {
      audioLevelObserver.on('volumes', (volumes: Array<{ producer: Producer; volume: number }>) => {
        if (volumes.length === 0) return;
        const { producer, volume } = volumes[0];
        // Find the participant who owns this producer
        for (const [, participant] of room!.participants) {
          for (const [, entry] of participant.producers) {
            if (entry.producer.id === producer.id) {
              this.emitEvent({
                type: 'active-speaker',
                roomId,
                userId: participant.userId,
                volume,
              });
              return;
            }
          }
        }
      });

      // Clear active speaker when everyone stops talking
      audioLevelObserver.on('silence', () => {
        this.emitEvent({
          type: 'active-speaker',
          roomId,
          userId: '',
          volume: -Infinity,
        });
      });
    }

    this.rooms.set(roomId, room);

    logger.info('Room created', { roomId });
    return room;
  }

  /** Close and remove a room (called when last participant leaves) */
  private async closeRoom(roomId: string): Promise<void> {
    const room = this.rooms.get(roomId);
    if (!room) return;

    // Close AudioLevelObserver
    if (room.audioLevelObserver && !room.audioLevelObserver.closed) {
      room.audioLevelObserver.close();
    }

    // Close the router (closes all transports, producers, consumers)
    if (!room.router.closed) {
      room.router.close();
    }

    this.rooms.delete(roomId);

    // Remove the stale router from the mediasoup cache so rejoin creates a fresh one
    this.mediasoup.removeRouter(roomId);

    logger.info('Room closed', { roomId });
    this.emitEvent({ type: 'room-empty', roomId });
  }

  // ─── Participant management ──────────────────────────────────────────

  /** Add a participant to a room. Returns the router's RTP capabilities and existing producers. */
  async joinRoom(
    roomId: string,
    userId: string,
    socketId: string,
    username: string,
    displayName?: string,
    avatarUrl?: string,
    rtpCapabilities?: RtpCapabilities,
    isEncrypted = false
  ): Promise<{
    rtpCapabilities: RtpCapabilities;
    existingProducers: ProducerInfo[];
    participants: Array<{ userId: string; username: string; displayName?: string; avatarUrl?: string }>;
    e2eeEpoch: number;
  }> {
    let room = await this.getOrCreateRoom(roomId);

    // Check if user is already in this room (reconnect scenario)
    const existing = room.participants.get(userId);
    if (existing) {
      logger.warn('User already in room, cleaning up old session', {
        roomId,
        userId,
        oldSocketId: existing.socketId,
        newSocketId: socketId,
      });
      await this.leaveRoom(roomId, userId);
      // Room may have been closed if user was the only participant — re-create
      room = await this.getOrCreateRoom(roomId);
    }

    const participant: Participant = {
      userId,
      socketId,
      username,
      displayName,
      avatarUrl,
      sendTransport: null,
      recvTransports: new Map(),
      producers: new Map(),
      consumers: new Map(),
      rtpCapabilities: rtpCapabilities ?? null,
      joinedAt: new Date(),
    };

    room.participants.set(userId, participant);

    // E2EE epoch tracking: mark room as encrypted on first join, increment epoch
    if (isEncrypted) room.isEncrypted = true;
    if (room.isEncrypted) room.e2eeEpoch++;

    logger.info('Participant joined room', {
      roomId,
      userId,
      username,
      participantCount: room.participants.size,
    });

    this.emitEvent({
      type: 'user-joined',
      roomId,
      userId,
      username,
      displayName,
    });

    // Collect existing producers for the new joiner to consume
    const existingProducers: ProducerInfo[] = [];
    for (const [, p] of room.participants) {
      if (p.userId === userId) continue;
      for (const [, entry] of p.producers) {
        existingProducers.push({
          producerId: entry.producer.id,
          userId: p.userId,
          kind: entry.kind,
          source: entry.source,
        });
      }
    }

    // Collect participant list
    const participants = Array.from(room.participants.values()).map((p) => ({
      userId: p.userId,
      username: p.username,
      displayName: p.displayName,
      avatarUrl: p.avatarUrl,
    }));

    return {
      rtpCapabilities: room.router.rtpCapabilities,
      existingProducers,
      participants,
      e2eeEpoch: room.e2eeEpoch,
    };
  }

  /** Remove a participant from a room. Cleans up all their transports, producers, consumers. */
  async leaveRoom(roomId: string, userId: string): Promise<void> {
    const room = this.rooms.get(roomId);
    if (!room) return;

    const participant = room.participants.get(userId);
    if (!participant) return;

    // Close all consumers
    for (const [, consumer] of participant.consumers) {
      if (!consumer.closed) consumer.close();
    }
    participant.consumers.clear();

    // Close all producers (also removes from AudioLevelObserver)
    for (const [producerId, entry] of participant.producers) {
      if (!entry.producer.closed) entry.producer.close();
      this.emitEvent({
        type: 'producer-removed',
        roomId,
        userId,
        producerId,
        kind: entry.kind,
        source: entry.source,
      });
    }
    participant.producers.clear();

    // Close transports
    if (participant.sendTransport && !participant.sendTransport.closed) {
      participant.sendTransport.close();
    }
    for (const [, transport] of participant.recvTransports) {
      if (!transport.closed) transport.close();
    }
    participant.recvTransports.clear();

    room.participants.delete(userId);

    // E2EE: increment epoch on leave (forward secrecy)
    if (room.isEncrypted) room.e2eeEpoch++;

    logger.info('Participant left room', {
      roomId,
      userId,
      remainingParticipants: room.participants.size,
    });

    this.emitEvent({ type: 'user-left', roomId, userId });

    // Tear down room if empty
    if (room.participants.size === 0) {
      await this.closeRoom(roomId);
    }
  }

  // ─── Transport management ────────────────────────────────────────────

  /** Create a WebRTC transport for a participant */
  async createTransport(
    roomId: string,
    userId: string,
    direction: 'send' | 'recv'
  ): Promise<TransportOptions> {
    const room = this.rooms.get(roomId);
    if (!room) throw new Error('Room not found');

    const participant = room.participants.get(userId);
    if (!participant) throw new Error('Participant not found in room');

    const transport = await room.router.createWebRtcTransport({
      listenIps: config.mediasoup.webRtcTransport.listenIps,
      enableUdp: config.mediasoup.webRtcTransport.enableUdp,
      enableTcp: config.mediasoup.webRtcTransport.enableTcp,
      preferUdp: config.mediasoup.webRtcTransport.preferUdp,
      initialAvailableOutgoingBitrate:
        config.mediasoup.webRtcTransport.initialAvailableOutgoingBitrate,
    });

    // Set max incoming bitrate
    if (config.mediasoup.webRtcTransport.maxIncomingBitrate) {
      try {
        await transport.setMaxIncomingBitrate(config.mediasoup.webRtcTransport.maxIncomingBitrate);
      } catch {
        // Some transports don't support this
      }
    }

    // Store on participant
    if (direction === 'send') {
      if (participant.sendTransport && !participant.sendTransport.closed) {
        participant.sendTransport.close();
      }
      participant.sendTransport = transport;
    } else {
      // Multiple recv transports are allowed (main window + PiP windows)
      participant.recvTransports.set(transport.id, transport);
      // Clean up map entry when router closes the transport
      transport.on('routerclose', () => {
        participant.recvTransports.delete(transport.id);
      });
    }

    // Log ICE/DTLS state transitions for diagnostics
    transport.on('icestatechange', (iceState) => {
      const level = iceState === 'completed' ? 'info' : iceState === 'disconnected' || iceState === 'closed' ? 'warn' : 'debug';
      logger.log(level, 'Transport ICE state change', {
        transportId: transport.id, roomId, userId, direction, iceState,
      });
    });
    transport.on('dtlsstatechange', (dtlsState) => {
      const level = dtlsState === 'connected' ? 'info' : dtlsState === 'failed' ? 'error' : dtlsState === 'closed' ? 'warn' : 'debug';
      logger.log(level, 'Transport DTLS state change', {
        transportId: transport.id, roomId, userId, direction, dtlsState,
      });
    });

    // Clean up on transport close
    transport.on('routerclose', () => {
      logger.debug('Transport router closed', { transportId: transport.id, roomId, userId });
    });

    logger.debug('Transport created', {
      transportId: transport.id,
      roomId,
      userId,
      direction,
    });

    return {
      id: transport.id,
      iceParameters: transport.iceParameters,
      iceCandidates: transport.iceCandidates,
      dtlsParameters: transport.dtlsParameters,
    };
  }

  /** Connect a transport's DTLS */
  async connectTransport(
    roomId: string,
    userId: string,
    transportId: string,
    dtlsParameters: DtlsParameters
  ): Promise<void> {
    const transport = this.findParticipantTransport(roomId, userId, transportId);
    if (!transport) throw new Error('Transport not found');

    await transport.connect({ dtlsParameters });

    logger.debug('Transport connected', { transportId, roomId, userId });
  }

  // ─── Producer management ─────────────────────────────────────────────

  /** Create a producer on the participant's send transport */
  async produce(
    roomId: string,
    userId: string,
    transportId: string,
    kind: MediaKind,
    rtpParameters: RtpParameters,
    source: MediaSource
  ): Promise<ProducerInfo> {
    const room = this.rooms.get(roomId);
    if (!room) throw new Error('Room not found');

    const participant = room.participants.get(userId);
    if (!participant) throw new Error('Participant not found');

    if (!participant.sendTransport || participant.sendTransport.id !== transportId) {
      throw new Error('Send transport not found or mismatch');
    }

    // Enforce stream limits
    if (source === 'camera') {
      let cameraCount = 0;
      for (const [, p] of room.participants) {
        for (const [, info] of p.producers) {
          if (info.source === 'camera') cameraCount++;
        }
      }
      if (cameraCount >= 25) {
        throw new Error('Video participant limit reached (max 25)');
      }
    }

    if (source === 'screen') {
      let screenCount = 0;
      for (const [, p] of room.participants) {
        for (const [, info] of p.producers) {
          if (info.source === 'screen') screenCount++;
        }
      }
      if (screenCount >= 5) {
        throw new Error('Screen share limit reached (max 5)');
      }
    }

    // Validate screen-audio: must be audio kind and participant must have an active screen producer
    if (source === 'screen-audio') {
      if (kind !== 'audio') {
        throw new Error('screen-audio source must be audio kind');
      }
      const hasScreenProducer = [...participant.producers.values()].some(
        (info) => info.source === 'screen'
      );
      if (!hasScreenProducer) {
        throw new Error('screen-audio requires an active screen producer');
      }
    }

    const producer = await participant.sendTransport.produce({
      kind,
      rtpParameters,
      appData: { source, userId },
    });

    participant.producers.set(producer.id, { producer, source, kind });

    // Add mic audio producers to the AudioLevelObserver (skip screen-audio to
    // avoid false active-speaker detection from system audio)
    if (kind === 'audio' && source !== 'screen-audio' && room.audioLevelObserver && !room.audioLevelObserver.closed) {
      try {
        await room.audioLevelObserver.addProducer({ producerId: producer.id });
      } catch (err) {
        logger.warn('Failed to add producer to AudioLevelObserver', {
          producerId: producer.id,
          error: err,
        });
      }
    }

    // Clean up when producer closes
    producer.on('transportclose', () => {
      participant.producers.delete(producer.id);
    });

    const info: ProducerInfo = { producerId: producer.id, userId, kind, source };

    logger.info('Producer created', {
      producerId: producer.id,
      roomId,
      userId,
      kind,
      source,
    });

    this.emitEvent({
      type: 'producer-added',
      roomId,
      userId,
      producerId: producer.id,
      kind,
      source,
    });

    return info;
  }

  /** Pause a producer (mute without removing) */
  async pauseProducer(roomId: string, userId: string, producerId: string): Promise<void> {
    const entry = this.findProducer(roomId, userId, producerId);
    if (!entry) throw new Error('Producer not found');

    await entry.producer.pause();
    logger.debug('Producer paused', { producerId, roomId, userId });
  }

  /** Resume a paused producer */
  async resumeProducer(roomId: string, userId: string, producerId: string): Promise<void> {
    const entry = this.findProducer(roomId, userId, producerId);
    if (!entry) throw new Error('Producer not found');

    await entry.producer.resume();
    logger.debug('Producer resumed', { producerId, roomId, userId });
  }

  /** Close and remove a producer */
  async closeProducer(roomId: string, userId: string, producerId: string): Promise<MediaSource | null> {
    const room = this.rooms.get(roomId);
    if (!room) return null;

    const participant = room.participants.get(userId);
    if (!participant) return null;

    const entry = participant.producers.get(producerId);
    if (!entry) return null;

    if (!entry.producer.closed) entry.producer.close();
    participant.producers.delete(producerId);

    this.emitEvent({
      type: 'producer-removed',
      roomId,
      userId,
      producerId,
      kind: entry.kind,
      source: entry.source,
    });

    logger.info('Producer closed', { producerId, roomId, userId, kind: entry.kind, source: entry.source });
    return entry.source;
  }

  // ─── Consumer management ─────────────────────────────────────────────

  /**
   * Create a consumer on the requesting user's recv transport for a given producer.
   * This fixes the bug in the original mediasoup.ts where consume() picked an
   * arbitrary transport from the room instead of the specific user's recv transport.
   */
  async consume(
    roomId: string,
    consumerUserId: string,
    producerId: string,
    transportId?: string
  ): Promise<{
    id: string;
    producerId: string;
    kind: MediaKind;
    rtpParameters: RtpParameters;
    producerUserId: string;
    source: MediaSource;
  } | null> {
    const room = this.rooms.get(roomId);
    if (!room) throw new Error('Room not found');

    const consumer = room.participants.get(consumerUserId);
    if (!consumer) throw new Error('Consumer participant not found');

    // Find the recv transport: use specified ID, or fall back to first available
    const recvTransport = transportId
      ? consumer.recvTransports.get(transportId)
      : consumer.recvTransports.values().next().value;

    logger.debug('Consume transport lookup', {
      roomId,
      consumerUserId,
      producerId,
      requestedTransportId: transportId,
      recvTransportCount: consumer.recvTransports.size,
      hasRecvTransport: !!recvTransport,
      hasRtpCapabilities: !!consumer.rtpCapabilities,
    });

    if (!recvTransport) {
      throw new Error('No receive transport — create one first');
    }

    if (!consumer.rtpCapabilities) {
      throw new Error('RTP capabilities not set — provide them on join');
    }

    // Find the producer and its owner
    let producerEntry: { producer: Producer; source: MediaSource; kind: MediaKind } | null = null;
    let producerUserId = '';
    for (const [, participant] of room.participants) {
      const entry = participant.producers.get(producerId);
      if (entry) {
        producerEntry = entry;
        producerUserId = participant.userId;
        break;
      }
    }

    if (!producerEntry) {
      throw new Error('Producer not found in room');
    }

    // Check if the router can route this producer to this consumer
    if (!room.router.canConsume({ producerId, rtpCapabilities: consumer.rtpCapabilities })) {
      logger.warn('Cannot consume — incompatible codecs', {
        producerId,
        consumerUserId,
        roomId,
      });
      return null;
    }

    // Create consumer on the specified (or default) recv transport
    const newConsumer = await recvTransport.consume({
      producerId,
      rtpCapabilities: consumer.rtpCapabilities,
      paused: true, // Start paused — client resumes after setup
    });

    consumer.consumers.set(newConsumer.id, newConsumer);

    // Clean up on close
    newConsumer.on('transportclose', () => {
      consumer.consumers.delete(newConsumer.id);
    });
    newConsumer.on('producerclose', () => {
      consumer.consumers.delete(newConsumer.id);
    });

    logger.info('Consumer created with transport state', {
      consumerId: newConsumer.id,
      producerId,
      consumerUserId,
      producerUserId,
      roomId,
      kind: newConsumer.kind,
      recvTransportId: recvTransport.id,
      iceState: recvTransport.iceState,
      dtlsState: recvTransport.dtlsState,
    });

    return {
      id: newConsumer.id,
      producerId,
      kind: newConsumer.kind as MediaKind,
      rtpParameters: newConsumer.rtpParameters,
      producerUserId,
      source: producerEntry.source,
    };
  }

  /** Resume a consumer (called after client has set up its decoder) */
  async resumeConsumer(roomId: string, userId: string, consumerId: string): Promise<void> {
    const room = this.rooms.get(roomId);
    if (!room) throw new Error('Room not found');

    const participant = room.participants.get(userId);
    if (!participant) throw new Error('Participant not found');

    const consumer = participant.consumers.get(consumerId);
    if (!consumer) throw new Error('Consumer not found');

    await consumer.resume();
    logger.debug('Consumer resumed', { consumerId, roomId, userId });
  }

  /** Pause a consumer (stops SFU from forwarding RTP, saving bandwidth) */
  async pauseConsumer(roomId: string, userId: string, consumerId: string): Promise<void> {
    const room = this.rooms.get(roomId);
    if (!room) throw new Error('Room not found');

    const participant = room.participants.get(userId);
    if (!participant) throw new Error('Participant not found');

    const consumer = participant.consumers.get(consumerId);
    if (!consumer) throw new Error('Consumer not found');

    await consumer.pause();
    logger.debug('Consumer paused', { consumerId, roomId, userId });
  }

  // ─── Query methods ───────────────────────────────────────────────────

  /** Get a room by ID */
  getRoom(roomId: string): Room | undefined {
    return this.rooms.get(roomId);
  }

  /** Get all active room IDs */
  getActiveRoomIds(): string[] {
    return Array.from(this.rooms.keys());
  }

  /** Get a participant by userId in a specific room */
  /** Update a participant's RTP capabilities (after device.load on client) */
  updateRtpCapabilities(roomId: string, userId: string, rtpCapabilities: RtpCapabilities): void {
    const participant = this.rooms.get(roomId)?.participants.get(userId);
    if (participant) {
      participant.rtpCapabilities = rtpCapabilities;
    }
  }

  /**
   * Compute the "codec floor" — the intersection of all participants' video
   * decode capabilities. Returns lowercase mimeTypes that every participant
   * with non-null rtpCapabilities can decode.
   * Returns null if <2 participants have capabilities (no constraint needed).
   *
   * Floor operates at mimeType level (e.g., "video/h264") — not profile level.
   * This is intentional: canConsume() handles profile matching, and all modern
   * browsers decode all profiles within a codec family. Profile-level floor
   * can be added if a real-world decode failure is observed.
   */
  computeCodecFloor(roomId: string): string[] | null {
    const room = this.rooms.get(roomId);
    if (!room) return null;

    const capableParts: RtpCapabilities[] = [];
    for (const [, p] of room.participants) {
      if (p.rtpCapabilities) capableParts.push(p.rtpCapabilities);
    }

    if (capableParts.length < 2) return null;

    let floor: Set<string> | null = null;
    for (const caps of capableParts) {
      const videoMimes = new Set<string>();
      for (const c of caps.codecs ?? []) {
        if (c.kind === 'video') videoMimes.add(c.mimeType.toLowerCase());
      }
      if (!floor) {
        floor = videoMimes;
      } else {
        for (const m of floor) {
          if (!videoMimes.has(m)) floor.delete(m);
        }
      }
    }

    return floor ? Array.from(floor) : [];
  }

  getParticipant(roomId: string, userId: string): Participant | undefined {
    return this.rooms.get(roomId)?.participants.get(userId);
  }

  /** Find which room a user is in (by socketId) */
  findRoomBySocketId(socketId: string): { roomId: string; userId: string } | null {
    for (const [roomId, room] of this.rooms) {
      for (const [userId, participant] of room.participants) {
        if (participant.socketId === socketId) {
          return { roomId, userId };
        }
      }
    }
    return null;
  }

  /** Get all socket IDs in a room (for broadcasting) */
  getRoomSocketIds(roomId: string): string[] {
    const room = this.rooms.get(roomId);
    if (!room) return [];
    return Array.from(room.participants.values()).map((p) => p.socketId);
  }

  /** Get aggregate stats for health endpoint */
  getStats(): {
    activeRooms: number;
    totalParticipants: number;
    totalProducers: number;
    totalConsumers: number;
  } {
    let totalParticipants = 0;
    let totalProducers = 0;
    let totalConsumers = 0;

    for (const [, room] of this.rooms) {
      totalParticipants += room.participants.size;
      for (const [, p] of room.participants) {
        totalProducers += p.producers.size;
        totalConsumers += p.consumers.size;
      }
    }

    return {
      activeRooms: this.rooms.size,
      totalParticipants,
      totalProducers,
      totalConsumers,
    };
  }

  /** Close all rooms (graceful shutdown) */
  async closeAll(): Promise<void> {
    logger.info('Closing all rooms', { count: this.rooms.size });
    const roomIds = Array.from(this.rooms.keys());
    for (const roomId of roomIds) {
      await this.closeRoom(roomId);
    }
  }

  // ─── Private helpers ─────────────────────────────────────────────────

  private findParticipantTransport(
    roomId: string,
    userId: string,
    transportId: string
  ): WebRtcTransport | null {
    const room = this.rooms.get(roomId);
    if (!room) return null;

    const participant = room.participants.get(userId);
    if (!participant) return null;

    if (participant.sendTransport?.id === transportId) return participant.sendTransport;
    const recvTransport = participant.recvTransports.get(transportId);
    if (recvTransport) return recvTransport;

    return null;
  }

  private findProducer(
    roomId: string,
    userId: string,
    producerId: string
  ): { producer: Producer; source: MediaSource; kind: MediaKind } | null {
    const room = this.rooms.get(roomId);
    if (!room) return null;

    const participant = room.participants.get(userId);
    if (!participant) return null;

    return participant.producers.get(producerId) ?? null;
  }
}
