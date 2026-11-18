import { describe, it, expect, vi } from 'vitest';
import { LEVEL, MESSAGE, SPLAT } from 'triple-beam';
import type { Logform } from 'winston';

// logger.ts reads the environment from config at import; the format factory
// takes the environment as an argument, so a stub is enough here.
vi.mock('../src/config/index.js', () => ({ config: { environment: 'test' } }));

import {
  LOG_FIELD_MAX_CHARS,
  LOG_MAX_DEPTH,
  LOG_MAX_ENTRIES,
  LOG_MAX_KEY_CHARS,
  LOG_MAX_NODES,
  LOG_MESSAGE_MAX_CHARS,
  LOG_STACK_MAX_CHARS,
  boundLogValue,
  boundedFields,
} from '../src/lib/logBounds.js';
import { createLogFormat } from '../src/lib/logger.js';

type Info = Logform.TransformableInfo;

function runFormat(format: Logform.Format, info: Info): Info {
  const out = format.transform(info, format.options);
  if (typeof out !== 'object' || out === null) throw new Error('format dropped the info object');
  return out;
}

function nested(depth: number): Record<string, unknown> {
  let value: Record<string, unknown> = { leaf: 1 };
  for (let i = 0; i < depth; i++) value = { child: value };
  return value;
}

const LONE_HIGH_SURROGATE = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])/;

describe('log bound constants (VULN-157)', () => {
  it('match the reviewed limits', () => {
    expect({
      LOG_FIELD_MAX_CHARS,
      LOG_MESSAGE_MAX_CHARS,
      LOG_STACK_MAX_CHARS,
      LOG_MAX_DEPTH,
      LOG_MAX_ENTRIES,
      LOG_MAX_KEY_CHARS,
      LOG_MAX_NODES,
    }).toEqual({
      LOG_FIELD_MAX_CHARS: 256,
      LOG_MESSAGE_MAX_CHARS: 2048,
      LOG_STACK_MAX_CHARS: 8192,
      LOG_MAX_DEPTH: 4,
      LOG_MAX_ENTRIES: 32,
      LOG_MAX_KEY_CHARS: 64,
      LOG_MAX_NODES: 256,
    });
  });
});

describe('boundLogValue (VULN-157)', () => {
  it('cuts long strings with a dropped-length marker', () => {
    const out = boundLogValue('x'.repeat(300)) as string;
    expect(out.startsWith('x'.repeat(256))).toBe(true);
    expect(out.startsWith('x'.repeat(257))).toBe(false);
    expect(out).toContain('44'); // marker states the dropped length
  });

  it('leaves a string at the limit and non-string primitives unchanged', () => {
    const atLimit = 'y'.repeat(LOG_FIELD_MAX_CHARS);
    expect(boundLogValue(atLimit)).toBe(atLimit);
    expect(boundLogValue(42)).toBe(42);
    expect(boundLogValue(true)).toBe(true);
    expect(boundLogValue(null)).toBe(null);
    expect(boundLogValue(undefined)).toBe(undefined);
  });

  it('cuts keys at 64 chars', () => {
    const longKey = 'k'.repeat(100);
    const out = boundLogValue({ [longKey]: 'v', short: 's' }) as Record<string, unknown>;
    const keys = Object.keys(out);
    expect(keys).toContain('short');
    const cut = keys.find((k) => k.startsWith('k'));
    expect(cut).toBeDefined();
    expect(cut?.startsWith('k'.repeat(64))).toBe(true);
    expect(cut?.startsWith('k'.repeat(65))).toBe(false);
    expect(cut).toContain('36');
    expect(out[cut as string]).toBe('v');
  });

  it('replaces values past depth 4 with [Object] / [Array]', () => {
    // depth 0 is the value itself; the object at depth 4 is the last one expanded
    const obj = boundLogValue({ a: { b: { c: { d: { e: { f: 1 }, list: [1] } } } } });
    expect(obj).toEqual({ a: { b: { c: { d: { e: '[Object]', list: '[Array]' } } } } });
    const atLimit = boundLogValue(nested(LOG_MAX_DEPTH));
    expect(JSON.stringify(atLimit)).toContain('"leaf":1');
    expect(JSON.stringify(boundLogValue(nested(LOG_MAX_DEPTH + 1)))).not.toContain('leaf');
  });

  it('keeps 32 entries per object or array and drops the rest', () => {
    const arr = boundLogValue(Array.from({ length: 50 }, (_, i) => i)) as unknown[];
    expect(arr).toEqual(Array.from({ length: 32 }, (_, i) => i));
    const wide: Record<string, number> = {};
    for (let i = 0; i < 50; i++) wide[`k${i}`] = i;
    const obj = boundLogValue(wide) as Record<string, number>;
    expect(Object.keys(obj)).toEqual(Array.from({ length: 32 }, (_, i) => `k${i}`));
  });

  it('replaces values past 256 nodes with [Truncated]', () => {
    const grid: Record<string, Record<string, string>> = {};
    for (let i = 0; i < 32; i++) {
      grid[`r${i}`] = {};
      for (let j = 0; j < 32; j++) grid[`r${i}`][`c${j}`] = 'v';
    }
    const out = boundLogValue(grid) as Record<string, unknown>;
    const json = JSON.stringify(out);
    const kept = (json.match(/"v"/g) ?? []).length;
    const truncated = (json.match(/"\[Truncated\]"/g) ?? []).length;
    expect(kept).toBeGreaterThan(0);
    // every value (objects and leaves) costs one node, so fewer than 256 leaves survive
    expect(kept).toBeLessThan(LOG_MAX_NODES);
    expect(truncated).toBeGreaterThan(0);
    // the first row is intact and the last row is gone
    expect(out.r0).toEqual(grid.r0);
    expect(out.r31).toBe('[Truncated]');
  });

  it('shares one node budget across calls when given one', () => {
    const budget = { nodes: 3 };
    expect(boundLogValue('a', budget)).toBe('a');
    expect(boundLogValue({ x: 1 }, budget)).toEqual({ x: 1 });
    expect(boundLogValue('b', budget)).toBe('[Truncated]');
  });

  it('replaces ancestor cycles with [Circular]', () => {
    const a: Record<string, unknown> = { name: 'a' };
    a.self = a;
    const list: unknown[] = [1];
    list.push(list);
    expect(boundLogValue(a)).toEqual({ name: 'a', self: '[Circular]' });
    expect(boundLogValue(list)).toEqual([1, '[Circular]']);
  });

  it('keeps a repeated non-ancestor reference', () => {
    const o = { v: 1 };
    const out = boundLogValue({ x: o, y: o }) as Record<string, unknown>;
    expect(out).toEqual({ x: { v: 1 }, y: { v: 1 } });
    expect(out.x).not.toBe(o);
    expect(out.y).not.toBe(o);
  });

  it('calls toJSON so Date and Buffer output is unchanged', () => {
    const date = new Date('2026-11-18T12:00:00.000Z');
    const buffer = Buffer.from('concord');
    expect(boundLogValue(date)).toEqual(JSON.parse(JSON.stringify(date)));
    expect(boundLogValue(buffer)).toEqual(JSON.parse(JSON.stringify(buffer)));
    expect(boundLogValue({ at: date })).toEqual({ at: '2026-11-18T12:00:00.000Z' });
  });

  it('bounds what toJSON returns', () => {
    const big = { toJSON: () => 'z'.repeat(1000) };
    const out = boundLogValue(big) as string;
    expect(out.startsWith('z'.repeat(256))).toBe(true);
    expect(out.length).toBeLessThan(300);
  });

  it('keeps Error meta as enumerable own properties only', () => {
    const err = Object.assign(new Error('boom secret detail'), { code: 'E_TEST' });
    expect(boundLogValue(err)).toEqual(JSON.parse(JSON.stringify(err)));
    expect(boundLogValue({ error: err })).toEqual({ error: { code: 'E_TEST' } });
  });

  it('does not mutate the value it bounds', () => {
    const input = { s: 'x'.repeat(500), deep: nested(8), list: Array.from({ length: 40 }, () => 'q') };
    const snapshot = JSON.stringify(input);
    boundLogValue(input);
    expect(JSON.stringify(input)).toBe(snapshot);
  });

  it('never leaves a lone surrogate when cutting at 256', () => {
    const out = boundLogValue('a'.repeat(255) + '😀' + 'b'.repeat(10)) as string;
    expect(LONE_HIGH_SURROGATE.test(out)).toBe(false);
    expect(out.startsWith('a'.repeat(255))).toBe(true);
    // a pair that ends exactly at the limit is kept whole
    const whole = boundLogValue('a'.repeat(254) + '😀' + 'b'.repeat(10)) as string;
    expect(whole.startsWith('a'.repeat(254) + '😀')).toBe(true);
  });

  it('never leaves a lone surrogate when cutting a key at 64', () => {
    const key = 'k'.repeat(63) + '😀' + 'k'.repeat(10);
    const out = boundLogValue({ [key]: 1 }) as Record<string, unknown>;
    for (const k of Object.keys(out)) expect(LONE_HIGH_SURROGATE.test(k)).toBe(false);
  });
});

describe('boundedFields format (VULN-157)', () => {
  const format = boundedFields();

  it('cuts message at 2048 and stack at 8192', () => {
    const out = runFormat(format, {
      level: 'error',
      message: 'm'.repeat(5000),
      stack: 's'.repeat(20_000),
      [LEVEL]: 'error',
    });
    const message = out.message as string;
    const stack = out.stack as string;
    expect(message.startsWith('m'.repeat(LOG_MESSAGE_MAX_CHARS))).toBe(true);
    expect(message.startsWith('m'.repeat(LOG_MESSAGE_MAX_CHARS + 1))).toBe(false);
    expect(message).toContain(String(5000 - LOG_MESSAGE_MAX_CHARS));
    expect(stack.startsWith('s'.repeat(LOG_STACK_MAX_CHARS))).toBe(true);
    expect(stack.startsWith('s'.repeat(LOG_STACK_MAX_CHARS + 1))).toBe(false);
    expect(stack).toContain(String(20_000 - LOG_STACK_MAX_CHARS));
  });

  it('bounds every other field at the field limit', () => {
    const out = runFormat(format, {
      level: 'info',
      message: 'ok',
      field: 'f'.repeat(1000),
      [LEVEL]: 'info',
    });
    expect((out.field as string).startsWith('f'.repeat(LOG_FIELD_MAX_CHARS + 1))).toBe(false);
    expect(out.message).toBe('ok');
    expect(out.level).toBe('info');
  });

  it('bounds the number and length of top-level meta keys, keeping level, message and timestamp', () => {
    const info: Info = { level: 'info', message: 'ok', [LEVEL]: 'info' };
    for (let i = 0; i < 100; i++) info[`meta${i}`] = i;
    info['x'.repeat(200)] = 'long-key';
    info.timestamp = '2026-11-18T12:00:00.000Z';
    const out = runFormat(format, info);
    const keys = Object.keys(out);
    expect(keys).toEqual(expect.arrayContaining(['level', 'message', 'timestamp']));
    expect(keys.filter((k) => !['level', 'message', 'timestamp'].includes(k))).toHaveLength(LOG_MAX_ENTRIES);
    for (const k of keys) expect(k.length).toBeLessThanOrEqual(LOG_MAX_KEY_CHARS + 32);
  });

  it('uses one node budget for the whole line', () => {
    const info: Info = { level: 'info', message: 'ok', [LEVEL]: 'info' };
    for (let i = 0; i < 30; i++) info[`f${i}`] = Array.from({ length: 32 }, () => 'v');
    const out = runFormat(format, info);
    const leaves = (JSON.stringify(out).match(/"v"/g) ?? []).length;
    expect(leaves).toBeLessThan(LOG_MAX_NODES);
    expect(JSON.stringify(out)).toContain('[Truncated]');
  });

  it('does not mutate the caller object and copies symbol keys', () => {
    const splat = [{ raw: 'payload' }];
    const meta = { nested: { s: 'x'.repeat(400) } };
    const info: Info = {
      level: 'warn',
      message: 'm'.repeat(3000),
      meta,
      [LEVEL]: 'warn',
      [SPLAT]: splat,
    };
    const before = { message: info.message, nested: meta.nested.s };
    const out = runFormat(format, info);
    expect(out).not.toBe(info);
    expect(info.message).toBe(before.message);
    expect(meta.nested.s).toBe(before.nested);
    expect(info.meta).toBe(meta);
    expect(out[LEVEL]).toBe('warn');
    expect(out[SPLAT]).toBe(splat);
  });

  it('returns level, bounded message and logBoundFailure on a throwing getter', () => {
    const hostile = {
      get secret(): string {
        throw new Error('getter exploded');
      },
    };
    const info: Info = { level: 'info', message: 'm'.repeat(3000), hostile, [LEVEL]: 'info' };
    let out: Info | undefined;
    expect(() => {
      out = runFormat(format, info);
    }).not.toThrow();
    expect(out?.logBoundFailure).toBe(true);
    expect(out?.level).toBe('info');
    expect(out?.[LEVEL]).toBe('info');
    expect((out?.message as string).startsWith('m'.repeat(LOG_MESSAGE_MAX_CHARS + 1))).toBe(false);
    expect(Object.keys(out ?? {}).sort()).toEqual(['level', 'logBoundFailure', 'message']);
  });

  it('survives a throwing toJSON and a Proxy whose ownKeys throws', () => {
    const badJson = {
      toJSON() {
        throw new Error('toJSON exploded');
      },
    };
    const jsonOut = runFormat(format, { level: 'info', message: 'a', badJson, [LEVEL]: 'info' });
    expect(jsonOut.logBoundFailure).toBeUndefined();
    expect(typeof jsonOut.badJson).toBe('string');
    expect(JSON.stringify(jsonOut)).not.toContain('exploded');

    const proxy = new Proxy(
      {},
      {
        ownKeys() {
          throw new Error('ownKeys exploded');
        },
      }
    );
    let proxyOut: Info | undefined;
    expect(() => {
      proxyOut = runFormat(format, { level: 'info', message: 'b', proxy, [LEVEL]: 'info' });
    }).not.toThrow();
    expect(proxyOut?.logBoundFailure).toBe(true);
    expect(proxyOut?.message).toBe('b');
  });

  it('bounds a non-string message as a value', () => {
    const out = runFormat(format, {
      level: 'info',
      message: { text: 't'.repeat(1000) } as unknown as string,
      [LEVEL]: 'info',
    });
    expect(((out.message as { text: string }).text).length).toBeLessThan(300);
  });
});

describe('createLogFormat end to end (VULN-157)', () => {
  const longInfo = (): Info => ({ level: 'info', message: 'm', [LEVEL]: 'info', f: 'x'.repeat(10_000) });

  it('bounds production format end to end', () => {
    const out = runFormat(createLogFormat('production'), longInfo());
    const line = out[MESSAGE];
    expect(typeof line).toBe('string');
    const parsed = JSON.parse(line as string) as { f: string; message: string; timestamp: string };
    expect(parsed.f.length).toBeLessThan(300);
    expect(parsed.message).toBe('m');
    expect(typeof parsed.timestamp).toBe('string');
  });

  it('bounds dev format end to end', () => {
    const out = runFormat(createLogFormat('development'), longInfo());
    const line = out[MESSAGE];
    expect(typeof line).toBe('string');
    expect((line as string).length).toBeLessThan(2_000);
    expect(line as string).toContain('xxxx');
  });

  it('bounds an Error message and stack logged through errors()', () => {
    const err = new Error('e'.repeat(5000));
    const out = runFormat(createLogFormat('production'), {
      level: 'error',
      message: err as unknown as string,
      [LEVEL]: 'error',
    });
    const parsed = JSON.parse(out[MESSAGE] as string) as { message: string; stack: string };
    expect(parsed.message.length).toBeLessThan(LOG_MESSAGE_MAX_CHARS + 32);
    expect(parsed.stack.length).toBeLessThan(LOG_STACK_MAX_CHARS + 32);
  });
});
