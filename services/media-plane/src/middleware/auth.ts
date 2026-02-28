import jwt from 'jsonwebtoken';
import type { Socket, ExtendedError } from 'socket.io';
import { config } from '../config/index.js';
import { logger } from '../lib/logger.js';

// ---------------------------------------------------------------------------
// JWT claims — mirrors the Go control plane's auth.Claims struct.
// The control plane signs JWTs with HS256 using the shared jwtSecret.
// ---------------------------------------------------------------------------
interface JwtClaims {
  user_id: string;
  jti?: string;
  iss?: string;
  iat?: number;
  exp?: number;
  nbf?: number;
}

// ---------------------------------------------------------------------------
// Augment socket.data with authenticated user fields.
// These are populated by the auth middleware and available in all handlers.
// ---------------------------------------------------------------------------
export interface AuthenticatedSocketData {
  userId: string;
  username: string;
  displayName?: string;
  avatarUrl?: string;
  roomId?: string;
  rtpCapabilities?: unknown;
}

// ---------------------------------------------------------------------------
// Socket.IO authentication middleware
//
// Validates the JWT from socket.handshake.auth.token, verifies the HMAC-SHA256
// signature against the shared secret, and attaches user metadata to socket.data.
//
// The client must provide:
//   auth.token      — JWT access token (required)
//   auth.username   — display username (required, since JWT only has user_id)
//   auth.displayName — optional display name
//   auth.avatarUrl  — optional avatar URL
// ---------------------------------------------------------------------------
export function createAuthMiddleware() {
  return (socket: Socket, next: (err?: ExtendedError) => void) => {
    const { token, username, displayName, avatarUrl } = socket.handshake.auth;

    // Require token
    if (!token || typeof token !== 'string') {
      logger.warn('Socket connection rejected: missing token', {
        socketId: socket.id,
        address: socket.handshake.address,
      });
      return next(new Error('Authentication required'));
    }

    // Require username (JWT only carries user_id)
    if (!username || typeof username !== 'string') {
      logger.warn('Socket connection rejected: missing username', {
        socketId: socket.id,
      });
      return next(new Error('Username required'));
    }

    try {
      // Verify JWT signature and expiry
      const decoded = jwt.verify(token, config.jwtSecret, {
        algorithms: ['HS256'],
        issuer: 'concordvoice-control-plane',
      }) as JwtClaims;

      if (!decoded.user_id) {
        logger.warn('Socket connection rejected: missing user_id in token', {
          socketId: socket.id,
        });
        return next(new Error('Invalid token: missing user_id'));
      }

      // Attach authenticated user data to socket
      const socketData = socket.data as AuthenticatedSocketData;
      socketData.userId = decoded.user_id;
      socketData.username = username;
      socketData.displayName = typeof displayName === 'string' ? displayName : undefined;
      socketData.avatarUrl = typeof avatarUrl === 'string' ? avatarUrl : undefined;

      logger.debug('Socket authenticated', {
        socketId: socket.id,
        userId: decoded.user_id,
        username,
      });

      next();
    } catch (err) {
      if (err instanceof jwt.TokenExpiredError) {
        logger.warn('Socket connection rejected: token expired', {
          socketId: socket.id,
        });
        return next(new Error('Token expired'));
      }

      if (err instanceof jwt.JsonWebTokenError) {
        logger.warn('Socket connection rejected: invalid token', {
          socketId: socket.id,
          error: (err as Error).message,
        });
        return next(new Error('Invalid token'));
      }

      logger.error('Socket auth unexpected error', {
        socketId: socket.id,
        error: err,
      });
      return next(new Error('Authentication failed'));
    }
  };
}

// ---------------------------------------------------------------------------
// Channel access validation
//
// Called on join-room to verify the user has permission to access the channel.
// Queries the control plane API to check server membership — the channel
// belongs to a server, and the user must be a member of that server.
//
// Returns channel metadata including encryption status for E2EE setup.
// ---------------------------------------------------------------------------
export interface ChannelAccessResult {
  allowed: boolean;
  channelId: string;
  serverId: string;
  channelName: string;
  isEncrypted: boolean;
  error?: string;
}

export async function validateChannelAccess(
  userId: string,
  channelId: string,
  token: string
): Promise<ChannelAccessResult> {
  try {
    // Fetch channel details from control plane
    const channelRes = await fetch(
      `${config.controlPlaneUrl}/api/v1/channels/${channelId}`,
      {
        headers: {
          'Authorization': `Bearer ${token}`,
          'Content-Type': 'application/json',
        },
      }
    );

    if (!channelRes.ok) {
      if (channelRes.status === 401 || channelRes.status === 403) {
        return {
          allowed: false,
          channelId,
          serverId: '',
          channelName: '',
          isEncrypted: false,
          error: 'Not authorized to access this channel',
        };
      }
      if (channelRes.status === 404) {
        return {
          allowed: false,
          channelId,
          serverId: '',
          channelName: '',
          isEncrypted: false,
          error: 'Channel not found',
        };
      }
      return {
        allowed: false,
        channelId,
        serverId: '',
        channelName: '',
        isEncrypted: false,
        error: `Control plane returned ${channelRes.status}`,
      };
    }

    const data = await channelRes.json() as {
      channel: {
        id: string;
        server_id: string;
        name: string;
        type: string;
        is_encrypted: boolean;
      };
    };

    const channel = data.channel;

    // Verify this is a voice channel
    if (channel.type !== 'voice') {
      return {
        allowed: false,
        channelId,
        serverId: channel.server_id,
        channelName: channel.name,
        isEncrypted: channel.is_encrypted ?? false,
        error: 'Not a voice channel',
      };
    }

    // The control plane's AuthRequired middleware already validates server membership
    // via the JWT + channel ownership. If we got a 200, the user has access.
    return {
      allowed: true,
      channelId: channel.id,
      serverId: channel.server_id,
      channelName: channel.name,
      isEncrypted: channel.is_encrypted ?? false,
    };
  } catch (err) {
    logger.error('Failed to validate channel access', {
      userId,
      channelId,
      error: err,
    });
    return {
      allowed: false,
      channelId,
      serverId: '',
      channelName: '',
      isEncrypted: false,
      error: 'Failed to validate channel access',
    };
  }
}
