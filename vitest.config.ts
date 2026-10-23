import { defineConfig } from 'vitest/config';

// One run over every package, so CI can shard and merge coverage the way the
// desktop suite did. Each package keeps its own vitest.config.ts (alias, jsdom,
// setup file); this file only lists them and owns coverage.
export default defineConfig({
  test: {
    projects: ['packages/*'],
    coverage: {
      provider: 'istanbul',
      reporter: ['text', 'html', 'lcov'],
      include: ['packages/*/src/**/*.ts'],
      exclude: ['**/*.d.ts', '**/*.test.ts'],
      // Coverage thresholds enforced by SonarQube Quality Gate, not here.
    },
  },
});
