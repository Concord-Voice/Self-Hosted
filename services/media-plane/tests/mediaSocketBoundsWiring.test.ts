/**
 * Wiring pins for issue 176 (media socket bounds). `src/index.ts` is the
 * entrypoint: it is excluded from coverage and cannot be imported, so every
 * rule lives in `src/lib` or `src/middleware` and these pins assert that
 * `index.ts` only calls them.
 */
import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

const indexSrc = readFileSync(new URL('../src/index.ts', import.meta.url), 'utf8');

describe('media socket bounds wiring in src/index.ts', () => {
  it('the Client connected log has no username key', () => {
    const start = indexSrc.indexOf("logger.info('Client connected'");
    expect(start).toBeGreaterThanOrEqual(0);
    const meta = indexSrc.slice(start, indexSrc.indexOf('});', start));
    expect(meta).toContain('socketId');
    expect(meta).not.toMatch(/username/);
  });

  it('the consume handler calls parseConsumeRequest before its first logger call', () => {
    const start = indexSrc.indexOf("withRateLimit(socket, 'consume'");
    expect(start).toBeGreaterThanOrEqual(0);
    const handler = indexSrc.slice(start, indexSrc.indexOf('withRateLimit(socket,', start + 1));
    const parseAt = handler.indexOf('parseConsumeRequest(');
    const firstLogAt = handler.indexOf('logger.');
    expect(parseAt).toBeGreaterThanOrEqual(0);
    expect(firstLogAt).toBeGreaterThan(parseAt);
    expect(handler).toContain("{ error: 'invalid_payload' }");
    // The raw payload is never destructured into the handler's parameters.
    expect(handler).not.toMatch(/'consume',\s*async\s*\(\s*\{/);
    expect(indexSrc).toMatch(/import \{[^}]*\bparseConsumeRequest\b[^}]*\} from '\.\/lib\/wireBounds\.js'/);
  });

  it('imports the commit functions from lib/roomAdmissionCommit.js and does not define them', () => {
    expect(indexSrc).toMatch(/from '\.\/lib\/roomAdmissionCommit\.js'/);
    expect(indexSrc).not.toMatch(/function commit(Channel|DM)Admission\(/);
    expect(indexSrc).not.toMatch(/function rollbackSocketMembership\(/);
    // Both room kinds still commit through the imported functions.
    expect(indexSrc).toContain(
      'commitChannelAdmission(roomManager, socket, data, roomId, value, commitSocketMembership)'
    );
    expect(indexSrc).toContain('commitDMAdmission(roomManager, socket, data, roomId, value, commitSocketMembership)');
  });
});
