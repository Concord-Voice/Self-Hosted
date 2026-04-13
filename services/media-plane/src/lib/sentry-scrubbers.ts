/**
 * Sentry event scrubbers for media-plane — fail-closed allowlist model.
 *
 * SERVER-SIDE SPECIFIC: unlike the desktop client scrubber, this version:
 *   - Does not allow electron, device, or browser contexts
 *   - Adds categorical tags for triage (socket.event, nats.subject, etc.)
 *   - NEVER allows room IDs or user IDs as tags
 *   - Strips event.request entirely
 *   - Drops ALL breadcrumbs (Winston is the source of truth)
 *
 * See [internal]rules/observability.md for governance.
 * See parent issue #610 for the global scrubbing standard.
 */

/* eslint-disable @typescript-eslint/no-explicit-any */

export const ALLOWED_CONTEXTS = new Set(['os', 'runtime', 'app']);

export const ALLOWED_TAG_KEYS = new Set([
  'environment',
  'release',
  'level',
  'transaction',
  'handled',
  // Categorical tags for server-side triage — intentionally NOT including
  // any identifier tags (no user.id, no room.id, no channel.id).
  'socket.event',
  'nats.subject',
  'redis.operation',
  'mediasoup.event',
]);

const SENSITIVE_PATTERNS: RegExp[] = [
  // JWT tokens — all 3 segments
  /eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)?/g,
  // Base64/base64url blobs ≥64 chars
  /(?<![A-Za-z0-9+/=_-])[A-Za-z0-9+/=_-]{64,}(?![A-Za-z0-9+/=_-])/g,
  // Hex hashes (SHA-256 etc.)
  /\b[a-f0-9]{64}\b/gi,
  // Email addresses
  /\b[\w.+-]+@[\w-]+\.[\w.]+\b/g,
];

export function redactSensitivePatterns(message: string): string {
  if (typeof message !== 'string') return String(message ?? '');
  let scrubbed = message;
  for (const pattern of SENSITIVE_PATTERNS) {
    pattern.lastIndex = 0;
    scrubbed = scrubbed.replace(pattern, '[REDACTED]');
  }
  return scrubbed;
}

function stripUnknownContexts(event: any): void {
  if (!event.contexts) return;
  for (const key of Object.keys(event.contexts)) {
    if (!ALLOWED_CONTEXTS.has(key)) delete event.contexts[key];
  }
}

function stripUnknownTags(event: any): void {
  if (!event.tags) return;
  for (const key of Object.keys(event.tags)) {
    if (!ALLOWED_TAG_KEYS.has(key)) delete event.tags[key];
  }
}

function scrubExceptionValues(event: any): void {
  if (!event.exception?.values) return;
  for (const exc of event.exception.values) {
    if (exc.value) exc.value = redactSensitivePatterns(exc.value);
  }
}

export function scrubEvent(event: any): any {
  if (!event) return null;

  stripUnknownContexts(event);
  event.extra = undefined;
  event.request = undefined;
  event.breadcrumbs = undefined;

  if (event.user) {
    const id = event.user.id;
    event.user = { id: id ? redactSensitivePatterns(String(id)) : undefined };
  }

  stripUnknownTags(event);
  scrubExceptionValues(event);

  return event;
}

export function scrubBreadcrumb(_breadcrumb: any): any {
  // Drop ALL breadcrumbs — Winston is the logging authority for the media-plane.
  return null;
}

/* eslint-enable @typescript-eslint/no-explicit-any */
