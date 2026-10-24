import { defineConfig } from 'vitest/config';
import { fileURLToPath } from 'node:url';

// Alpha ran these cross-language vector tests in its desktop Vitest suite.
export default defineConfig({
  root: fileURLToPath(new URL('../..', import.meta.url)),
  test: {
    name: 'authority-vectors',
    environment: 'node',
    include: ['scripts/authority-vectors/*.test.ts'],
  },
});
