// This file is loaded via Node's --import flag BEFORE any application code.
// @sentry/node uses require-hook patching to auto-instrument Express, HTTP,
// and other modules — they must be loaded AFTER Sentry.init() runs.
//
// Per Sentry ESM docs:
//   https://docs.sentry.io/platforms/javascript/guides/express/install/esm/

import * as Sentry from '@sentry/node';
import { scrubEvent, scrubBreadcrumb } from './lib/sentry-scrubbers.js';
import { readFileSync } from 'node:fs';

const pkg = JSON.parse(readFileSync(new URL('../package.json', import.meta.url), 'utf-8'));

const environment = process.env.ENVIRONMENT || 'development';
const dsn = process.env.SENTRY_DSN_MEDIA;

const sentryEnabled =
  process.env.SENTRY_ENABLED == null
    ? Boolean(dsn) && environment === 'production'
    : process.env.SENTRY_ENABLED.toLowerCase() === 'true';

Sentry.init({
  dsn,
  environment,
  enabled: sentryEnabled,
  release: `media-plane@${pkg.version}`,
  sampleRate: 1,
  // Tracing disabled until a beforeSendTransaction scrubber is implemented.
  // See parent #610 for rationale.
  tracesSampleRate: 0,
  // ═══════════════════════════════════════════════════════════════════════
  // CRITICAL PRIVACY SETTING — DO NOT CHANGE WITHOUT SECURITY REVIEW
  //
  // sendDefaultPii: false prevents @sentry/node from auto-collecting:
  //   - User IP addresses
  //   - Request headers (including Cookie, Authorization)
  //   - Request bodies
  //   - URL query strings
  //   - Cookies
  //
  // Sentry's default onboarding example sets this to true. That is
  // INCORRECT for Concord Voice. Setting it to true would undo every
  // privacy protection in the scrubber and ship PII to a third party.
  // ═══════════════════════════════════════════════════════════════════════
  sendDefaultPii: false,
  beforeSend: scrubEvent,
  beforeBreadcrumb: scrubBreadcrumb,
  integrations: [
    Sentry.httpIntegration(),
    Sentry.expressIntegration(),
    Sentry.requestDataIntegration({
      include: {
        cookies: false,
        data: false,
        headers: false,
        ip: false,
        query_string: false,
        url: true,
      },
    }),
  ],
});

// Production safety guard — mirrors the JWT_SECRET guard in config/index.ts.
if (environment === 'production' && sentryEnabled && !dsn) {
  console.error(
    'FATAL: SENTRY_ENABLED=true in production but SENTRY_DSN_MEDIA is unset. ' +
      'Refusing to start with a silent-inert observability pipeline.'
  );
  process.exit(1);
}
