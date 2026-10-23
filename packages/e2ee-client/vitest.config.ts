import path from 'node:path';
import { defineConfig } from 'vitest/config';

// Same `@` alias and layout as the desktop suite, so the moved tests run unchanged.
export default defineConfig({
  resolve: { alias: { '@': path.resolve(__dirname, './src') } },
  test: {
    globals: true,
    environment: 'jsdom',
    setupFiles: ['./tests/setup.ts'],
    include: ['tests/**/*.test.ts'],
  },
});
