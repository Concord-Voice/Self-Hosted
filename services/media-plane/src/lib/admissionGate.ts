/**
 * Pre-upgrade admission gate (#2032).
 *
 * Bounds what per-socket budgets structurally cannot: `socket.data` exists only
 * AFTER a socket is established, so buckets keyed there cannot limit connection
 * ESTABLISHMENT. This gate runs before the socket exists.
 *
 * Extracted from index.ts as a pure predicate for the same reason originGate.ts
 * was (`[internal]rules/media-plane.md`): logic inside the server-construction
 * block is not unit-testable.
 */

import { BlockList, isIPv4, isIPv6 } from 'node:net';
import * as ipaddr from 'ipaddr.js';

/** 60 handshakes per 10 s = 6/s sustained per source IP. */
export const HANDSHAKE_BURST = 60;
export const HANDSHAKE_WINDOW_MS = 10_000;
/** Hard cap on tracked IPs — the store must not become the DoS it prevents. */
export const MAX_TRACKED_IPS = 10_000;

export interface AdmissionRequest {
  headers: Record<string, string | string[] | undefined>;
  socket: { remoteAddress?: string };
}

export interface AdmissionGateOptions {
  now?: () => number;
  /**
   * Peer addresses or CIDR blocks whose forwarded headers may be trusted (the
   * nginx front). An EMPTY list makes the gate inert — see createAdmissionGate.
   */
  trustedProxies: readonly string[];
  /** Production requires a usable trusted-proxy rule for address attribution. */
  required?: boolean;
  /** PII-safe rejection hook — receives no address. */
  onReject?: () => void;
  /**
   * Sink for the one-shot configuration warnings emitted at construction.
   * Defaults to console.warn so the lib stays free of the winston/config import
   * chain; index.ts passes the service logger.
   */
  warn?: (message: string) => void;
}

/** A compiled trusted-proxy allowlist. `size === 0` means nothing is trusted. */
export interface TrustedProxies {
  /** Number of rules that parsed. Zero disables the gate entirely. */
  readonly size: number;
  /** True when `peer` falls inside a configured address or CIDR block. */
  readonly has: (peer: string) => boolean;
}

type IpFamily = 'ipv4' | 'ipv6';

const ipv4AddressSpace = new BlockList();
ipv4AddressSpace.addSubnet('0.0.0.0', 0, 'ipv4');

function ipFamily(address: string): IpFamily | null {
  if (isIPv4(address)) return 'ipv4';
  if (isIPv6(address)) return 'ipv6';
  return null;
}

function isIPv4MappedIPv6(address: string, family: IpFamily): boolean {
  return family === 'ipv6' && ipv4AddressSpace.check(address, 'ipv6');
}

/** Parse once for both the development matcher and production startup guard. */
export function parseTrustedProxyEntry(
  raw: string
): { address: string; prefix: number; family: IpFamily } | null {
  const entry = raw.trim();
  const slash = entry.lastIndexOf('/');
  const address = slash < 0 ? entry : entry.slice(0, slash);
  const family = ipFamily(address);
  if (!family || isIPv4MappedIPv6(address, family)) return null;
  if (slash < 0) return { address, prefix: family === 'ipv4' ? 32 : 128, family };

  const prefixText = entry.slice(slash + 1);
  if (!/^\d{1,3}$/.test(prefixText)) return null;
  const prefix = Number(prefixText);
  if (prefix === 0 || prefix > (family === 'ipv4' ? 32 : 128)) return null;
  return { address, prefix, family };
}

/** Invalid development entries are skipped; production rejects them on import. */
function addTrustedEntry(list: BlockList, entry: string): boolean {
  const parsed = parseTrustedProxyEntry(entry);
  if (!parsed) return false;
  try {
    if (entry.includes('/')) list.addSubnet(parsed.address, parsed.prefix, parsed.family);
    else list.addAddress(parsed.address, parsed.family);
    return true;
  } catch {
    return false;
  }
}

/**
 * Compile the configured trusted-proxy entries into a matcher.
 *
 * Production configuration admits only exact host routes. Development keeps
 * CIDR support and malformed-entry warnings to preserve local test topology.
 *
 * Accepts bare addresses (`10.0.0.9` → /32, `2001:db8::1` → /128) and CIDR
 * blocks, IPv4 and IPv6.
 */
export function compileTrustedProxies(
  entries: readonly string[],
  warn: (message: string) => void = defaultWarn
): TrustedProxies {
  const list = new BlockList();
  let size = 0;

  for (const [index, raw] of entries.entries()) {
    const entry = raw.trim();
    if (!entry) continue;
    if (addTrustedEntry(list, entry)) {
      size += 1;
    } else {
      // Report the POSITION, never the value. The entry is environment-derived
      // (TRUSTED_PROXIES), and echoing an env-derived string into a log sink is
      // what CodeQL's js/clear-text-logging flags: an operator who pastes the
      // wrong value into that variable would have it printed straight back. The
      // value is also an address, which `[internal]rules/observability.md` #2 asks
      // us to keep out of logs where avoidable — and echoing it would contradict
      // this gate's own posture, since it deliberately never logs a REJECTED
      // client IP. A 1-based position locates the typo in a comma-separated list
      // without reproducing anything the operator supplied.
      warn(
        `Ignoring invalid TRUSTED_PROXIES entry at position ${index + 1} — ` +
          'expected an IP address or a CIDR block.'
      );
    }
  }

  return {
    size,
    has: (peer: string) => {
      const address = peer.trim();
      const family = ipFamily(address);
      if (!family) return false;
      // The family MUST be passed: BlockList.check defaults to 'ipv4', which
      // would silently miss every IPv4-mapped IPv6 peer (`::ffff:172.18.0.5`,
      // the form docker commonly presents). Passing 'ipv6' makes node compare
      // such a peer against the IPv4 rules, as documented for net.BlockList.
      return list.check(address, family);
    },
  };
}

function defaultWarn(message: string): void {
  console.warn(message);
}

function firstHeader(value: string | string[] | undefined): string | undefined {
  if (Array.isArray(value)) return value[0];
  return value;
}

/**
 * Resolve the client address.
 *
 * nginx fronts the media-plane and sets X-Real-IP / X-Forwarded-For, so
 * `req.socket.remoteAddress` is the PROXY's address for every client. Trust the
 * forwarded headers only when the immediate peer is a known proxy — an
 * untrusted peer presenting X-Real-IP is spoofing.
 *
 * Returns null when nothing can be determined; callers MUST fail open on null.
 */
export function resolveClientIp(
  req: AdmissionRequest,
  trustedProxies: TrustedProxies
): string | null {
  const peer = req.socket?.remoteAddress;

  if (peer && trustedProxies.has(peer)) {
    // Reject malformed headers so they cannot become arbitrary counter keys.
    const realIp = firstHeader(req.headers['x-real-ip'])?.trim();
    if (realIp && ipFamily(realIp)) return realIp;

    const forwarded = firstHeader(req.headers['x-forwarded-for']);
    if (forwarded) {
      const hops = forwarded
        .split(',')
        .map((h) => h.trim())
        .filter(Boolean);
      // nginx overwrites XFF with the observed client address.
      const last = hops.at(-1);
      // Same parse guard as X-Real-IP above — validating only one of the two
      // headers would leave the identical arbitrary-key path open on the other.
      if (last && ipFamily(last)) return last;
    }

    // A verified proxy peer that omits usable forwarding headers still has an
    // attributable observed source. Count it rather than creating an unmetered
    // path; nginx normally overwrites these headers.
    return peer;
  }

  // Untrusted peer: it IS the client, so attribute the request to it directly.
  return peer ?? null;
}

// bucketClientIp returns canonical IPv4 /32 identities and masks native IPv6
// identities to /64. ipaddr.process also folds IPv4-mapped IPv6 into IPv4.
export function bucketClientIp(address: string): string {
  const parsed = ipaddr.process(address);
  if (parsed.kind() === 'ipv4') return parsed.toString();

  const bytes = parsed.toByteArray();
  bytes.fill(0, 8);
  return ipaddr.fromByteArray(bytes).toString();
}

interface WindowCounters {
  /** Alternating fixed-size windows — no unbounded timestamp arrays. */
  windowStartMs: number;
  current: number;
  previous: number;
}

/**
 * Callback shape of engine.io's `allowRequest` option
 * (`engine.io/build/server.d.ts`: `fn: (err: string | null | undefined,
 * success: boolean) => void`). The gate always passes `null` — on `success ===
 * false` engine.io answers FORBIDDEN and carries this value only as diagnostic
 * context, so it must not describe why the client was rejected.
 */
export type AdmissionCallback = (err: string | null | undefined, success: boolean) => void;

export interface AdmissionGate {
  (req: AdmissionRequest, callback: AdmissionCallback): void;
  trackedCount: () => number;
}

export function createAdmissionGate(options: AdmissionGateOptions): AdmissionGate {
  const now = options.now ?? Date.now;
  const warn = options.warn ?? defaultWarn;
  const trustedProxies = compileTrustedProxies(options.trustedProxies, warn);

  // Fail INERT, not fail-shared. With no trusted proxy configured every client
  // resolves to the nginx peer address, so a counting gate would put the whole
  // deployment in ONE bucket and deny voice service-wide on the first reconnect
  // storm. A misconfiguration must degrade to the pre-gate behaviour — the same
  // fail-open posture the design specifies for an unresolvable IP.
  if (trustedProxies.size === 0) {
    if (options.required) {
      throw new Error('Admission gate requires at least one valid trusted proxy.');
    }
    warn(
      'Admission gate INACTIVE: no trusted proxies configured (TRUSTED_PROXIES). ' +
        'Every client would otherwise be attributed to the reverse proxy and share ' +
        'one handshake budget, so the per-source-IP ceiling is disabled. Set ' +
        'TRUSTED_PROXIES to the exact peer the media-plane sees nginx on to enable it.'
    );
    const inert = ((_req, callback) => {
      callback(null, true);
    }) as AdmissionGate;
    inert.trackedCount = () => 0;
    return inert;
  }

  const counters = new Map<string, WindowCounters>();

  const gate = ((req, callback) => {
    const ip = resolveClientIp(req, trustedProxies);

    // A genuinely peerless request has no source identity and cannot enter an
    // admission budget.
    if (!ip) {
      options.onReject?.();
      callback(null, false);
      return;
    }
    const bucket = bucketClientIp(ip);

    const nowMs = now();
    let entry = counters.get(bucket);

    if (!entry) {
      // Bounded store: evict the oldest insertion (Map preserves insertion
      // order) rather than growing without limit under an IPv6-source flood.
      if (counters.size >= MAX_TRACKED_IPS) {
        const oldest = counters.keys().next();
        if (!oldest.done) counters.delete(oldest.value);
      }
      entry = { windowStartMs: nowMs, current: 0, previous: 0 };
      counters.set(bucket, entry);
    }

    const elapsed = nowMs - entry.windowStartMs;
    if (elapsed >= HANDSHAKE_WINDOW_MS * 2) {
      entry.previous = 0;
      entry.current = 0;
      entry.windowStartMs = nowMs;
    } else if (elapsed >= HANDSHAKE_WINDOW_MS) {
      entry.previous = entry.current;
      entry.current = 0;
      entry.windowStartMs = nowMs;
    }

    if (entry.previous + entry.current >= HANDSHAKE_BURST) {
      options.onReject?.();
      callback(null, false);
      return;
    }

    entry.current += 1;
    callback(null, true);
  }) as AdmissionGate;

  gate.trackedCount = () => counters.size;
  return gate;
}
