import { describe, it, expect, vi, beforeEach } from 'vitest';

const { mockWithScope, mockCaptureException, mockScope } = vi.hoisted(() => {
  const mockScope = {
    setTag: vi.fn(),
    setLevel: vi.fn(),
  };
  const mockCaptureException = vi.fn();
  const mockWithScope = vi.fn((cb: (scope: typeof mockScope) => void) => {
    cb(mockScope);
  });
  return { mockWithScope, mockCaptureException, mockScope };
});

vi.mock('@sentry/node', () => ({
  withScope: mockWithScope,
  captureException: mockCaptureException,
}));

import { captureWithTag } from '../src/lib/sentry-helpers.js';

describe('captureWithTag', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('calls withScope and sets the tag', () => {
    const err = new Error('test');
    captureWithTag(err, 'socket.event', 'join-room');

    expect(mockWithScope).toHaveBeenCalledOnce();
    expect(mockScope.setTag).toHaveBeenCalledWith('socket.event', 'join-room');
    expect(mockCaptureException).toHaveBeenCalledWith(err);
  });

  it('sets level when provided', () => {
    const err = new Error('fatal');
    captureWithTag(err, 'mediasoup.event', 'worker-died', 'fatal');

    expect(mockScope.setLevel).toHaveBeenCalledWith('fatal');
    expect(mockScope.setTag).toHaveBeenCalledWith('mediasoup.event', 'worker-died');
  });

  it('does not set level when not provided', () => {
    captureWithTag(new Error('x'), 'redis.operation', 'addParticipant');

    expect(mockScope.setLevel).not.toHaveBeenCalled();
  });

  it('accepts nats.subject category', () => {
    captureWithTag(new Error('x'), 'nats.subject', 'voice.joined');

    expect(mockScope.setTag).toHaveBeenCalledWith('nats.subject', 'voice.joined');
  });
});
