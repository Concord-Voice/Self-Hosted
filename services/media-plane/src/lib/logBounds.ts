import winston from 'winston';
import type { Logform } from 'winston';

/**
 * Bounds for every log line (VULN-157).
 *
 * Log calls pass client-supplied values (payload fields, handshake values,
 * error text) into meta. This format is the single place that bounds them, so
 * no call site has to remember to. It runs after `errors()` and before the
 * finalizing `json()` / `printf()` format.
 *
 * Invariant: the format never mutates the caller's object and never throws.
 * It builds a new info object from a copy-only traversal; any throw inside the
 * traversal (a throwing getter, a Proxy trap) yields a fallback line carrying
 * the level, the bounded message and `logBoundFailure: true`.
 *
 * Worst case for one line is about 100,000 characters before JSON escaping:
 * 256 nodes of at most 256 characters plus their keys, the message and the
 * stack.
 */
export const LOG_FIELD_MAX_CHARS = 256;
export const LOG_MESSAGE_MAX_CHARS = 2048;
export const LOG_STACK_MAX_CHARS = 8192;
export const LOG_MAX_DEPTH = 4;
export const LOG_MAX_ENTRIES = 32;
export const LOG_MAX_KEY_CHARS = 64;
export const LOG_MAX_NODES = 256;

const TRUNCATED = '[Truncated]';
const CIRCULAR = '[Circular]';
const UNSERIALIZABLE = '[Unserializable]';
const UNLOGGABLE_MESSAGE = '[Unloggable message]';

// triple-beam's LEVEL. The registry symbol is the one every logform format
// shares, so this module does not import the transitive package directly.
const LEVEL = Symbol.for('level');

// Keys the format handles on their own instead of as meta entries, so a line
// with many meta fields never loses its level, message, stack or timestamp.
const RESERVED_KEYS = new Set(['level', 'message', 'stack', 'timestamp']);

interface NodeBudget {
  nodes: number;
}

function isHighSurrogate(code: number): boolean {
  return code >= 0xd800 && code <= 0xdbff;
}

/** Cuts `value` to `max` code units, never splitting a surrogate pair. */
export function cutLogString(value: string, max: number): string {
  if (value.length <= max) return value;
  let end = max;
  if (isHighSurrogate(value.charCodeAt(end - 1))) end -= 1;
  return `${value.slice(0, end)}...[+${value.length - end} chars]`;
}

// defineProperty, not assignment: a `__proto__` key from parsed client JSON
// must stay an own data property instead of replacing the copy's prototype.
function setEntry(target: object, key: string | symbol, value: unknown): void {
  Object.defineProperty(target, key, { value, enumerable: true, writable: true, configurable: true });
}

function boundNode(value: unknown, budget: NodeBudget, depth: number, ancestors: Set<object>): unknown {
  if (budget.nodes <= 0) return TRUNCATED;
  budget.nodes -= 1;
  return boundCharged(value, budget, depth, ancestors, true);
}

// The node for `value` is already charged. `allowToJSON` is false for the
// value a `toJSON` call returned, matching JSON.stringify, which calls it once.
function boundCharged(
  value: unknown,
  budget: NodeBudget,
  depth: number,
  ancestors: Set<object>,
  allowToJSON: boolean
): unknown {
  if (typeof value === 'string') return cutLogString(value, LOG_FIELD_MAX_CHARS);
  if (value === null || typeof value !== 'object') return value;
  if (ancestors.has(value)) return CIRCULAR;

  if (allowToJSON) {
    const toJSON: unknown = (value as { toJSON?: unknown }).toJSON;
    if (typeof toJSON === 'function') {
      let replaced: unknown;
      try {
        replaced = toJSON.call(value);
      } catch {
        return UNSERIALIZABLE;
      }
      if (replaced === value) return boundCharged(value, budget, depth, ancestors, false);
      ancestors.add(value);
      try {
        return boundCharged(replaced, budget, depth, ancestors, false);
      } finally {
        ancestors.delete(value);
      }
    }
  }

  if (depth > LOG_MAX_DEPTH) return Array.isArray(value) ? '[Array]' : '[Object]';

  ancestors.add(value);
  try {
    if (Array.isArray(value)) {
      const count = Math.min(value.length, LOG_MAX_ENTRIES);
      const out: unknown[] = new Array<unknown>(count);
      for (let i = 0; i < count; i++) out[i] = boundNode(value[i], budget, depth + 1, ancestors);
      return out;
    }
    const out: Record<string, unknown> = {};
    const keys = Object.keys(value);
    const count = Math.min(keys.length, LOG_MAX_ENTRIES);
    for (let i = 0; i < count; i++) {
      const key = keys[i];
      const bounded = boundNode((value as Record<string, unknown>)[key], budget, depth + 1, ancestors);
      setEntry(out, cutLogString(key, LOG_MAX_KEY_CHARS), bounded);
    }
    return out;
  } finally {
    ancestors.delete(value);
  }
}

/**
 * Returns a bounded copy of `value`: strings cut at `LOG_FIELD_MAX_CHARS`,
 * keys at `LOG_MAX_KEY_CHARS`, `LOG_MAX_ENTRIES` entries per object or array,
 * objects past `LOG_MAX_DEPTH` replaced by `[Object]` / `[Array]`, ancestor
 * cycles by `[Circular]`, and every value past the node budget by
 * `[Truncated]`. Pass one budget to several calls to share it across a line.
 * May throw on a hostile value (a throwing getter or Proxy trap); the format
 * below turns that into a fallback line.
 */
export function boundLogValue(value: unknown, budget: NodeBudget = { nodes: LOG_MAX_NODES }): unknown {
  return boundNode(value, budget, 0, new Set<object>());
}

function boundReserved(key: string, value: unknown, budget: NodeBudget): unknown {
  if (key === 'level') return value;
  if (typeof value === 'string') {
    if (key === 'message') return cutLogString(value, LOG_MESSAGE_MAX_CHARS);
    if (key === 'stack') return cutLogString(value, LOG_STACK_MAX_CHARS);
  }
  return boundLogValue(value, budget);
}

function boundInfo(info: Logform.TransformableInfo): Logform.TransformableInfo {
  const budget: NodeBudget = { nodes: LOG_MAX_NODES };
  // Every own key, level and message included, is copied by the loop below.
  const out = {} as Logform.TransformableInfo;
  let entries = 0;
  for (const key of Object.keys(info)) {
    if (RESERVED_KEYS.has(key)) {
      setEntry(out, key, boundReserved(key, info[key], budget));
      continue;
    }
    if (entries >= LOG_MAX_ENTRIES) continue;
    entries += 1;
    setEntry(out, cutLogString(key, LOG_MAX_KEY_CHARS), boundLogValue(info[key], budget));
  }
  for (const symbol of Object.getOwnPropertySymbols(info)) {
    setEntry(out, symbol, (info as Record<symbol, unknown>)[symbol]);
  }
  return out;
}

function fallbackInfo(info: Logform.TransformableInfo): Logform.TransformableInfo {
  let message = UNLOGGABLE_MESSAGE;
  try {
    if (typeof info.message === 'string') message = cutLogString(info.message, LOG_MESSAGE_MAX_CHARS);
  } catch {
    // keep the fixed message
  }
  const out = { level: info.level, message, logBoundFailure: true } as Logform.TransformableInfo;
  setEntry(out, LEVEL, (info as Record<symbol, unknown>)[LEVEL]);
  return out;
}

/** The winston format that bounds every field of every log line. */
export function boundedFields(): Logform.Format {
  return winston.format((info) => {
    try {
      return boundInfo(info);
    } catch {
      return fallbackInfo(info);
    }
  })();
}
