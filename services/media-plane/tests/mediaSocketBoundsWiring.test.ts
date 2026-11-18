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
