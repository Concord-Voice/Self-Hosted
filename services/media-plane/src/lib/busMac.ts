/**
 * Bus MAC: the TS half of `services/control-plane/pkg/busmac`. Both modules
 * replay `pkg/busmac/testdata/vectors.json`, so they agree on every MAC, every
 * envelope byte and every refusal code.
 *
 * Construction (see the Go package doc for the invariant and what breaks it):
 *
 *   subkey = HMAC-SHA256(root[kid], "concord-bus-mac/v1/" + purpose)
 *   mac    = HMAC-SHA256(subkey, "concord-bus-mac/v1\n" + purpose + "\n" + kid + "\n" + lines.join("\n"))
 *
 * A family MAC uses the lines [subject, ts, hex(sha256(body))]. No purpose,
 * kid or line may contain a newline or NUL, the line list is never empty, and
 * every field must be well-formed UTF-16: a lone surrogate encodes to EF BF BD,
 * so two different strings would otherwise hash to the same bytes.
 *
 * Every refusal throws a `BusMacError` whose `code` matches the Go sentinel;
 * the verify methods return false instead. Messages carry the code only, never
 * key material, a kid, a MAC or a subject.
 */
import { createHash, createHmac, timingSafeEqual } from 'node:crypto';
import {
  BUS_MAC_PURPOSES,
  FAMILY_LIFECYCLE,
  FAMILY_TELEMETRY,
  type BusMacPurposeKind,
} from './busMacRegistry.js';

export { FAMILY_LIFECYCLE, FAMILY_TELEMETRY };

export const MIN_KEY_BYTES = 32;
export const MAX_PAST_SKEW_MS = 60_000;
export const MAX_FUTURE_SKEW_MS = 5_000;

const MAC_VERSION = 'concord-bus-mac/v1';
const MAC_BYTES = 32;
const KID_PATTERN = /^[a-z0-9]{1,16}$/;
const BASE64URL_PATTERN = /^[A-Za-z0-9_-]*$/;
const INTEGER_LITERAL = /^(0|[1-9][0-9]*)$/;
const ENVELOPE_MEMBERS: ReadonlySet<string> = new Set(['v', 'kid', 'ts', 'mac', 'body']);

export type BusMacErrorCode =
  | 'empty_keyring'
  | 'malformed_keyring'
  | 'short_key'
  | 'duplicate_kid'
  | 'bad_envelope'
  | 'unknown_kid'
  | 'unregistered_family'
  | 'reserved_purpose'
  | 'bad_field'
  | 'skew'
  | 'bad_mac';

export class BusMacError extends Error {
  readonly code: BusMacErrorCode;

  constructor(code: BusMacErrorCode) {
    super(`bus MAC refused: ${code}`);
    this.name = 'BusMacError';
    this.code = code;
  }
}

export interface BusMacKeyring {
  /** The kid that signs: the first keyring entry. */
  activeKid(): string;
  /** Detached MAC over caller lines under a registered raw purpose. No time window. */
  rawMac(purpose: string, lines: readonly string[]): { kid: string; mac: Buffer };
  rawVerify(purpose: string, kid: string, lines: readonly string[], mac: Uint8Array): boolean;
  /** Detached family MAC over subject, tsMillis and body. */
  mac(
    family: string,
    subject: string,
    tsMillis: number,
    body: Uint8Array
  ): { kid: string; mac: Buffer };
  verifyMac(
    family: string,
    kid: string,
    subject: string,
    tsMillis: number,
    body: Uint8Array,
    mac: Uint8Array
  ): boolean;
  /** Envelope text for body, signed for subject at nowMs. */
  sign(family: string, subject: string, body: string, nowMs: number): string;
  /**
   * The body an envelope received on subject at receivedAtMs authenticates.
   * receivedAtMs is the NATS callback time, taken once per message.
   */
  open(family: string, subject: string, envelope: string, receivedAtMs: number): string;
}

/** Whether family is a registered (enveloped) family. */
export function isRegisteredFamily(family: string): boolean {
  return BUS_MAC_PURPOSES.get(family) === 'family';
}

function wellFormed(s: string): boolean {
  return Buffer.from(s, 'utf8').toString('utf8') === s;
}

function validField(s: unknown): s is string {
  return typeof s === 'string' && !s.includes('\n') && !s.includes('\0') && wellFormed(s);
}

function purposeRefusal(purpose: string, want: BusMacPurposeKind): BusMacErrorCode | undefined {
  if (!validField(purpose)) return 'bad_field';
  const kind = BUS_MAC_PURPOSES.get(purpose);
  if (kind === want) return undefined;
  return kind === 'family' ? 'reserved_purpose' : 'unregistered_family';
}

function linesValid(lines: readonly string[]): boolean {
  return Array.isArray(lines) && lines.length > 0 && lines.every(validField);
}

function familyLines(subject: string, tsMillis: number, body: Uint8Array): string[] | undefined {
  if (subject === '' || !validField(subject)) return undefined;
  if (!Number.isSafeInteger(tsMillis) || tsMillis < 0) return undefined;
  const digest = createHash('sha256').update(body).digest('hex');
  return [subject, String(tsMillis), digest];
}

function compute(root: Buffer, purpose: string, kid: string, lines: readonly string[]): Buffer {
  const subkey = createHmac('sha256', root).update(`${MAC_VERSION}/${purpose}`, 'utf8').digest();
  return createHmac('sha256', subkey)
    .update(`${MAC_VERSION}\n${purpose}\n${kid}\n${lines.join('\n')}`, 'utf8')
    .digest();
}

/**
 * The one unpadded base64url spelling of a byte string. Buffer.from ignores
 * characters outside the alphabet and tolerates non-zero trailing bits, so the
 * alphabet check and the re-encode comparison are what make it unique.
 */
function decodeCanonical(s: string): Buffer | undefined {
  if (!BASE64URL_PATTERN.test(s)) return undefined;
  const decoded = Buffer.from(s, 'base64url');
  return decoded.toString('base64url') === s ? decoded : undefined;
}

class Keyring implements BusMacKeyring {
  readonly #keys: ReadonlyMap<string, Buffer>;
  readonly #activeKid: string;

  constructor(activeKid: string, keys: ReadonlyMap<string, Buffer>) {
    this.#activeKid = activeKid;
    this.#keys = keys;
  }

  activeKid(): string {
    return this.#activeKid;
  }

  /** The root key for kid; a short key is treated as absent. */
  #root(kid: string): Buffer | undefined {
    const key = this.#keys.get(kid);
    return key !== undefined && key.length >= MIN_KEY_BYTES ? key : undefined;
  }

  #signer(): { kid: string; root: Buffer } {
    const root = this.#root(this.#activeKid);
    if (root === undefined) throw new BusMacError('empty_keyring');
    return { kid: this.#activeKid, root };
  }

  #verify(purpose: string, kid: string, lines: readonly string[], mac: Uint8Array): boolean {
    if (!validField(kid) || !(mac instanceof Uint8Array) || mac.length !== MAC_BYTES) return false;
    const root = this.#root(kid);
    return root !== undefined && timingSafeEqual(compute(root, purpose, kid, lines), mac);
  }

  rawMac(purpose: string, lines: readonly string[]): { kid: string; mac: Buffer } {
    const { kid, root } = this.#signer();
    const refusal = purposeRefusal(purpose, 'raw');
    if (refusal !== undefined) throw new BusMacError(refusal);
    if (!linesValid(lines)) throw new BusMacError('bad_field');
    return { kid, mac: compute(root, purpose, kid, lines) };
  }

  rawVerify(purpose: string, kid: string, lines: readonly string[], mac: Uint8Array): boolean {
    if (purposeRefusal(purpose, 'raw') !== undefined || !linesValid(lines)) return false;
    return this.#verify(purpose, kid, lines, mac);
  }

  mac(
    family: string,
    subject: string,
    tsMillis: number,
    body: Uint8Array
  ): { kid: string; mac: Buffer } {
    const { kid, root } = this.#signer();
    const refusal = purposeRefusal(family, 'family');
    if (refusal !== undefined) throw new BusMacError(refusal);
    const lines = familyLines(subject, tsMillis, body);
    if (lines === undefined) throw new BusMacError('bad_field');
    return { kid, mac: compute(root, family, kid, lines) };
  }

  verifyMac(
    family: string,
    kid: string,
    subject: string,
    tsMillis: number,
    body: Uint8Array,
    mac: Uint8Array
  ): boolean {
    if (purposeRefusal(family, 'family') !== undefined) return false;
    const lines = familyLines(subject, tsMillis, body);
    return lines !== undefined && this.#verify(family, kid, lines, mac);
  }

  sign(family: string, subject: string, body: string, nowMs: number): string {
    const { kid, mac } = this.mac(family, subject, nowMs, Buffer.from(body, 'utf8'));
    if (!wellFormed(body)) throw new BusMacError('bad_field');
    // Key order is the wire order; the Go encoder writes the same bytes.
    return JSON.stringify({ v: 1, kid, ts: nowMs, mac: mac.toString('base64url'), body });
  }

  open(family: string, subject: string, envelope: string, receivedAtMs: number): string {
    this.#signer();
    const refusal = purposeRefusal(family, 'family');
    if (refusal !== undefined) throw new BusMacError(refusal);
    if (subject === '' || !validField(subject)) throw new BusMacError('bad_field');
    const env = decodeEnvelope(envelope);
    if (!validField(env.kid)) throw new BusMacError('bad_field');
    if (this.#root(env.kid) === undefined) throw new BusMacError('unknown_kid');
    if (
      !Number.isFinite(receivedAtMs) ||
      env.ts < receivedAtMs - MAX_PAST_SKEW_MS ||
      env.ts > receivedAtMs + MAX_FUTURE_SKEW_MS
    ) {
      throw new BusMacError('skew');
    }
    const body = Buffer.from(env.body, 'utf8');
    if (!this.verifyMac(family, env.kid, subject, env.ts, body, env.mac)) {
      throw new BusMacError('bad_mac');
    }
    return env.body;
  }
}

/**
 * Parses `<kid>:<base64url key>[,<kid>:<base64url key>...]`. Nothing is
 * trimmed: whitespace, a trailing comma or a line ending is refused.
 */
export function parseKeyring(spec: string): BusMacKeyring {
  if (spec === '') throw new BusMacError('empty_keyring');
  const keys = new Map<string, Buffer>();
  let activeKid = '';
  for (const part of spec.split(',')) {
    const colon = part.indexOf(':');
    const kid = colon < 0 ? '' : part.slice(0, colon);
    if (!KID_PATTERN.test(kid)) throw new BusMacError('malformed_keyring');
    const key = decodeCanonical(part.slice(colon + 1));
    if (key === undefined) throw new BusMacError('malformed_keyring');
    if (key.length < MIN_KEY_BYTES) throw new BusMacError('short_key');
    if (keys.has(kid)) throw new BusMacError('duplicate_kid');
    keys.set(kid, key);
    if (activeKid === '') activeKid = kid;
  }
  return new Keyring(activeKid, keys);
}

interface Envelope {
  kid: string;
  ts: number;
  mac: Buffer;
  body: string;
}

/**
 * Parses the one JSON object a signed frame may be. JSON.parse validates the
 * syntax but keeps the LAST of two duplicate members, so the members are read
 * from the text itself; the Go decoder walks its tokens for the same reason.
 * Decoded strings are normalised through UTF-8, as Go's decoder does, so an
 * escaped lone surrogate reads as U+FFFD on both sides.
 */
function decodeEnvelope(text: string): Envelope {
  const bad = new BusMacError('bad_envelope');
  if (!wellFormed(text)) throw bad;
  try {
    JSON.parse(text);
  } catch {
    throw bad;
  }
  const members = topLevelMembers(text);
  if (members === undefined) throw bad;
  const raw = new Map<string, string>();
  for (const [name, value] of members) {
    if (!ENVELOPE_MEMBERS.has(name) || raw.has(name)) throw bad;
    raw.set(name, value);
  }
  if (raw.size !== ENVELOPE_MEMBERS.size || raw.get('v') !== '1') throw bad;
  const tsText = raw.get('ts') ?? '';
  const ts = Number(tsText);
  if (!INTEGER_LITERAL.test(tsText) || !Number.isSafeInteger(ts)) throw bad;
  const kid = jsonString(raw.get('kid'));
  const macText = jsonString(raw.get('mac'));
  const body = jsonString(raw.get('body'));
  if (kid === undefined || macText === undefined || body === undefined) throw bad;
  const mac = decodeCanonical(macText);
  if (mac === undefined || mac.length !== MAC_BYTES) throw bad;
  return { kid, ts, mac, body };
}

function jsonString(raw: string | undefined): string | undefined {
  if (raw === undefined || !raw.startsWith('"')) return undefined;
  const value: unknown = JSON.parse(raw);
  return typeof value === 'string' ? Buffer.from(value, 'utf8').toString('utf8') : undefined;
}

const isJsonWhitespace = (c: string): boolean =>
  c === ' ' || c === '\t' || c === '\n' || c === '\r';

function skipWhitespace(text: string, from: number): number {
  let i = from;
  while (i < text.length && isJsonWhitespace(text[i])) i++;
  return i;
}

/** Index just past the string literal opening at `from`, or -1. */
function endOfString(text: string, from: number): number {
  for (let i = from + 1; i < text.length; i++) {
    if (text[i] === '\\') i++;
    else if (text[i] === '"') return i + 1;
  }
  return -1;
}

/** Index just past the value starting at `from`, or -1. */
function endOfValue(text: string, from: number): number {
  let depth = 0;
  let i = from;
  while (i < text.length) {
    const c = text[i];
    if (c === '"') {
      i = endOfString(text, i);
      if (i < 0) return -1;
      if (depth === 0) return i;
      continue;
    }
    if (c === '{' || c === '[') {
      depth++;
    } else if (c === '}' || c === ']') {
      if (depth === 0) return i;
      depth--;
      if (depth === 0) return i + 1;
    } else if (depth === 0 && (c === ',' || isJsonWhitespace(c))) {
      return i;
    }
    i++;
  }
  return depth === 0 ? i : -1;
}

/**
 * The top-level members of a JSON object as [decoded name, raw value text],
 * in order and with duplicates kept. Runs only on text JSON.parse accepted.
 */
function topLevelMembers(text: string): Array<[string, string]> | undefined {
  let i = skipWhitespace(text, 0);
  if (text[i] !== '{') return undefined;
  const members: Array<[string, string]> = [];
  i = skipWhitespace(text, i + 1);
  if (text[i] === '}') return skipWhitespace(text, i + 1) === text.length ? members : undefined;
  for (;;) {
    if (text[i] !== '"') return undefined;
    const nameEnd = endOfString(text, i);
    if (nameEnd < 0) return undefined;
    const name: unknown = JSON.parse(text.slice(i, nameEnd));
    if (typeof name !== 'string') return undefined;
    i = skipWhitespace(text, nameEnd);
    if (text[i] !== ':') return undefined;
    const valueStart = skipWhitespace(text, i + 1);
    const valueEnd = endOfValue(text, valueStart);
    if (valueEnd < 0) return undefined;
    members.push([name, text.slice(valueStart, valueEnd)]);
    i = skipWhitespace(text, valueEnd);
    if (text[i] === ',') {
      i = skipWhitespace(text, i + 1);
      continue;
    }
    if (text[i] !== '}') return undefined;
    return skipWhitespace(text, i + 1) === text.length ? members : undefined;
  }
}
