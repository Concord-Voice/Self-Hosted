import { describe, it, expect } from 'vitest';
import {
  scrubEvent,
  scrubBreadcrumb,
  redactSensitivePatterns,
  ALLOWED_CONTEXTS,
  ALLOWED_TAG_KEYS,
} from '../src/lib/sentry-scrubbers.js';

describe('scrubEvent', () => {
  it('returns null for null input', () => {
    expect(scrubEvent(null)).toBeNull();
  });

  it('strips unknown contexts', () => {
    const event = {
      contexts: { os: { name: 'Linux' }, electron: { version: '41' }, customThing: {} },
    };
    const result = scrubEvent(event);
    expect(result.contexts).toHaveProperty('os');
    expect(result.contexts).not.toHaveProperty('electron');
    expect(result.contexts).not.toHaveProperty('customThing');
  });

  it('preserves all allowed contexts', () => {
    const event = {
      contexts: { os: {}, runtime: {}, app: {} },
    };
    const result = scrubEvent(event);
    expect(Object.keys(result.contexts)).toHaveLength(3);
  });

  it('strips unknown tags', () => {
    const event = {
      tags: { environment: 'production', release: 'v1', customTag: 'leaked' },
    };
    const result = scrubEvent(event);
    expect(result.tags).toHaveProperty('environment');
    expect(result.tags).toHaveProperty('release');
    expect(result.tags).not.toHaveProperty('customTag');
  });

  it('preserves all allowed tag keys', () => {
    const tags: Record<string, string> = {};
    for (const key of ALLOWED_TAG_KEYS) tags[key] = 'test';
    const event = { tags };
    const result = scrubEvent(event);
    expect(Object.keys(result.tags)).toHaveLength(ALLOWED_TAG_KEYS.size);
  });

  it('ALWAYS strips room.id tag (regression)', () => {
    const event = { tags: { environment: 'prod', 'room.id': 'abc123' } };
    const result = scrubEvent(event);
    expect(result.tags).not.toHaveProperty('room.id');
  });

  it('ALWAYS strips user.id tag (regression)', () => {
    const event = { tags: { environment: 'prod', 'user.id': 'abc123' } };
    const result = scrubEvent(event);
    expect(result.tags).not.toHaveProperty('user.id');
  });

  it('ALWAYS strips channel.id tag (regression)', () => {
    const event = { tags: { environment: 'prod', 'channel.id': 'abc123' } };
    const result = scrubEvent(event);
    expect(result.tags).not.toHaveProperty('channel.id');
  });

  it('strips extra completely', () => {
    const event = { extra: { sensitiveState: 'leaked' } };
    const result = scrubEvent(event);
    expect(result.extra).toBeUndefined();
  });

  it('strips request completely', () => {
    const event = {
      request: { url: '/api/v1/channels', headers: { authorization: 'Bearer xxx' } },
    };
    const result = scrubEvent(event);
    expect(result.request).toBeUndefined();
  });

  it('strips breadcrumbs completely', () => {
    const event = {
      breadcrumbs: [{ category: 'http', message: 'GET /health' }],
    };
    const result = scrubEvent(event);
    expect(result.breadcrumbs).toBeUndefined();
  });

  it('strips user fields except id', () => {
    const event = {
      user: { id: 'anon-123', email: 'user@example.com', ip_address: '1.2.3.4' },
    };
    const result = scrubEvent(event);
    expect(result.user).toEqual({ id: 'anon-123' });
  });

  it('redacts sensitive patterns in user.id', () => {
    const event = { user: { id: 'alice@example.com' } };
    const result = scrubEvent(event);
    expect(result.user.id).toContain('[REDACTED]');
  });

  it('handles event with no user field', () => {
    const event = { extra: { data: 'value' } };
    const result = scrubEvent(event);
    expect(result.user).toBeUndefined();
  });

  it('redacts JWTs in exception messages', () => {
    const fakeJwt = `${btoa('{"alg":"HS256"}').replace(/=/g, '')}.${btoa('{"sub":"test"}').replace(/=/g, '')}`;
    const event = {
      exception: { values: [{ value: `Error: token ${fakeJwt} expired` }] },
    };
    const result = scrubEvent(event);
    expect(result.exception.values[0].value).not.toContain('eyJ');
    expect(result.exception.values[0].value).toContain('[REDACTED]');
  });
});

describe('redactSensitivePatterns', () => {
  it('catches base64 blobs over 64 chars', () => {
    const blob = 'A'.repeat(70);
    expect(redactSensitivePatterns(`Key: ${blob}`)).toContain('[REDACTED]');
  });

  it('catches hex hashes', () => {
    const hex = 'a'.repeat(64);
    expect(redactSensitivePatterns(`Hash: ${hex}`)).toContain('[REDACTED]');
  });

  it('catches email addresses', () => {
    expect(redactSensitivePatterns('User alice@example.com failed')).toContain('[REDACTED]');
  });

  it('passes through clean messages', () => {
    const msg = 'TypeError: Cannot read properties of undefined';
    expect(redactSensitivePatterns(msg)).toBe(msg);
  });

  it('handles non-string input', () => {
    expect(redactSensitivePatterns(undefined as any)).toBe('');
    expect(redactSensitivePatterns(null as any)).toBe('');
    expect(redactSensitivePatterns(42 as any)).toBe('42');
  });
});

describe('scrubBreadcrumb', () => {
  it('drops ALL breadcrumbs', () => {
    expect(scrubBreadcrumb({ category: 'http', message: 'GET /health' })).toBeNull();
    expect(scrubBreadcrumb({ category: 'console', message: 'debug' })).toBeNull();
    expect(scrubBreadcrumb({ category: 'navigation' })).toBeNull();
  });

  it('handles null input', () => {
    expect(scrubBreadcrumb(null)).toBeNull();
  });
});
