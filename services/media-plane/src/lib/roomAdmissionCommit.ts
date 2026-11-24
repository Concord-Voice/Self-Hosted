import type { Socket } from 'socket.io';
import { logger } from './logger.js';
import type {
  ChannelParticipantPromotion,
  DMParticipantPromotion,
  JoinRoomResult,
  RoomManager,
} from './roomManager.js';
import type { AuthenticatedSocketData, resolveParticipantIdentity } from '../middleware/auth.js';

/**
 * The synchronous A2 commit for both room kinds (VULN-159).
 *
 * Invariant: after a same-account promotion replaces an admitted session, the
 * socket that held it is disconnected, for channels and DMs alike, so it stops
 * receiving `io.to(room)` broadcasts. Both commit functions follow one order:
 *
 *   1. capture the displaced socket id, synchronously, before promotion;
 *   2. promote; a throw rolls back this socket's membership and rethrows, and
 *      nothing is evicted;
 *   3. after the `try`, with no `await`, evict the displaced socket.
 *
 * Eviction sits outside the `try` so that a throw from the displaced socket's
 * disconnect listeners can never roll back a promotion RoomManager already
 * committed. It only disconnects: it does not `leave()` or clear `roomId`
 * first, because the displaced socket's own disconnect handler takes the
 * ownership-guard branch and releases its identities from that state.
 */

export type RoomJoinValue = {
  authorizedCallId: string | undefined;
  identity: ReturnType<typeof resolveParticipantIdentity>;
  promotion: ChannelParticipantPromotion | DMParticipantPromotion | undefined;
  result: JoinRoomResult;
};

export function rollbackSocketMembership(
  socket: Socket,
  data: AuthenticatedSocketData,
  roomId: string
): void {
  data.roomId = undefined;
  try {
    socket.leave(roomId);
  } catch (leaveError) {
    logger.error('Failed to roll back Socket.IO room membership', {
      error: leaveError,
      roomId,
      userId: data.userId,
    });
  }
}

/**
 * Disconnects the socket a promotion displaced, closing its underlying
 * connection. Returns `true` only when it called `disconnect(true)` and the
 * call did not throw. Never throws: a failure is logged with ids only.
 */
export function evictDisplacedSocket(
  sockets: ReadonlyMap<string, { disconnect(close?: boolean): unknown }>,
  displacedSocketId: string | undefined,
  currentSocketId: string,
  context: { roomId: string }
): boolean {
  if (displacedSocketId === undefined || displacedSocketId === currentSocketId) return false;
  const displaced = sockets.get(displacedSocketId);
  if (!displaced) return false;
  try {
    displaced.disconnect(true);
    return true;
  } catch {
    logger.error('Failed to disconnect displaced socket', {
      roomId: context.roomId,
      displacedSocketId,
    });
    return false;
  }
}

export function commitChannelAdmission(
  roomManager: RoomManager,
  socket: Socket,
  data: AuthenticatedSocketData,
  roomId: string,
  value: RoomJoinValue,
  commitSocketMembership: () => void
): RoomJoinValue {
  if (!value.promotion) {
    throw new Error('Room admission is missing its A2 promotion state');
  }
  if (!('permissions' in value.promotion)) {
    throw new Error('Channel admission is missing its A2 promotion state');
  }
  const displacedSocketId = roomManager.getParticipant(roomId, data.userId)?.socketId;
  try {
    value.result = roomManager.promoteChannelParticipant(
      roomId,
      data.userId,
      socket.id,
      value.promotion,
      commitSocketMembership
    );
  } catch (error) {
    // A custom/failed adapter must not strand Socket.IO membership or socket.data
    // when its synchronous join boundary throws.
    rollbackSocketMembership(socket, data, roomId);
    throw error;
  }
  evictDisplacedSocket(socket.nsp.sockets, displacedSocketId, socket.id, { roomId });
  return value;
}

export function commitDMAdmission(
  roomManager: RoomManager,
  socket: Socket,
  data: AuthenticatedSocketData,
  roomId: string,
  value: RoomJoinValue,
  commitSocketMembership: () => void
): RoomJoinValue {
  if (!value.promotion) {
    throw new Error('Room admission is missing its A2 promotion state');
  }
  if (!value.authorizedCallId || !('callId' in value.promotion)) {
    throw new Error('DM admission is missing its A2 promotion state');
  }
  const displacedSocketId = roomManager.getParticipant(roomId, data.userId)?.socketId;
  try {
    value.result = roomManager.promoteDMParticipant(
      roomId,
      data.userId,
      socket.id,
      value.authorizedCallId,
      value.promotion,
      commitSocketMembership
    );
  } catch (error) {
    // A custom/failed adapter must not strand Socket.IO membership or socket.data
    // when its synchronous join boundary throws.
    rollbackSocketMembership(socket, data, roomId);
    throw error;
  }
  evictDisplacedSocket(socket.nsp.sockets, displacedSocketId, socket.id, { roomId });
  return value;
}
