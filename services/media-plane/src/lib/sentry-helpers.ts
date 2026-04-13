import * as Sentry from '@sentry/node';

type TagCategory = 'socket.event' | 'nats.subject' | 'redis.operation' | 'mediasoup.event';

/**
 * Capture an exception with a categorical tag for triage.
 * Uses Sentry.withScope to isolate the tag to this single event.
 */
export function captureWithTag(
  error: unknown,
  category: TagCategory,
  value: string,
  level?: Sentry.SeverityLevel
): void {
  try {
    Sentry.withScope((scope) => {
      scope.setTag(category, value);
      if (level) scope.setLevel(level);
      Sentry.captureException(error);
    });
  } catch {
    // Sentry SDK errors must never propagate into application catch blocks.
    // The original error is still logged by Winston at the call site.
  }
}
