import { webcrypto } from 'node:crypto';

// jsdom lacks crypto.subtle; use Node's Web Crypto, as the desktop setup does.
if (!globalThis.crypto?.subtle) {
  Object.defineProperty(globalThis, 'crypto', { value: webcrypto });
}
