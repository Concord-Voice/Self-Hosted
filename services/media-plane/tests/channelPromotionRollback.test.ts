import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

const SOURCE = readFileSync(join(__dirname, '..', 'src', 'lib', 'roomAdmissionCommit.ts'), 'utf8')
  .replace(/\/\*[\s\S]*?\*\//g, '')
  .replace(/\/\/[^\n]*/g, '');

describe('channel promotion rollback call-site contract', () => {
  it('clears socket state and leaves the room when membership commit fails', () => {
    const channelCommit = SOURCE.slice(
      SOURCE.indexOf('function commitChannelAdmission'),
      SOURCE.indexOf('function commitDMAdmission')
    );

    const promoteAt = channelCommit.indexOf('roomManager.promoteChannelParticipant(');
    const rollbackAt = channelCommit.indexOf('rollbackSocketMembership(socket, data, roomId);');
    const throwAt = channelCommit.indexOf('throw error;');

    // The behavioural promotion/rollback assertions live in
    // roomAdmissionCommit.test.ts; this pins the shape of the seam.
    expect(channelCommit).toContain('try {');
    expect(promoteAt).toBeGreaterThanOrEqual(0);
    expect(rollbackAt).toBeGreaterThan(promoteAt);
    expect(throwAt).toBeGreaterThan(rollbackAt);
  });
});
