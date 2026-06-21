/**
 * #1742 DIAGNOSTIC (temporary — remove with the instrumentation)
 *
 * The E2EE worker owns its own MediaEncryption instance in a separate module
 * scope and cannot read localStorage, so the main thread forwards the opt-in
 * media-diagnostic flag via a `setDiag` message (Gitar finding on PR #1776).
 * This verifies the worker's onmessage handler relays that flag into its module
 * scope's setMediaDiagEnabled — without which the worker-path passthrough
 * counters / summary would be silently inert on the script-transform path.
 */
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

const { setMediaDiagEnabled } = vi.hoisted(() => ({ setMediaDiagEnabled: vi.fn() }));

vi.mock('@/renderer/services/mediaEncryption', () => ({
  MediaEncryption: class {
    initFromKey = vi.fn();
    addDecryptKeyDirect = vi.fn();
    rotateKeys = vi.fn().mockResolvedValue(undefined);
    getCurrentKeyId = vi.fn().mockReturnValue(0);
    catchUpToEpoch = vi.fn().mockResolvedValue(undefined);
    destroy = vi.fn();
    encryptFrame = vi.fn().mockResolvedValue(undefined);
    decryptFrame = vi.fn().mockResolvedValue(undefined);
  },
  setMediaDiagEnabled,
}));

type Handler = (e: { data: unknown }) => void;

describe('e2eeWorker setDiag (#1742 diagnostics, opt-in)', () => {
  beforeEach(() => {
    vi.resetModules();
    setMediaDiagEnabled.mockClear();
    // The worker calls self.postMessage(msg) with a single arg; stub it to
    // avoid jsdom's strict (message, targetOrigin) signature throwing.
    vi.stubGlobal('postMessage', vi.fn());
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    (self as unknown as { onmessage: Handler | null }).onmessage = null;
  });

  async function loadWorkerHandler(): Promise<Handler> {
    await import('@/renderer/workers/e2eeWorker');
    const handler = (self as unknown as { onmessage: Handler | null }).onmessage;
    if (!handler) throw new Error('worker did not register onmessage');
    return handler;
  }

  it('forwards enabled=true to the worker MediaEncryption module scope', async () => {
    const onmessage = await loadWorkerHandler();
    onmessage({ data: { type: 'setDiag', enabled: true } });
    expect(setMediaDiagEnabled).toHaveBeenCalledWith(true);
  });

  it('forwards enabled=false to the worker MediaEncryption module scope', async () => {
    const onmessage = await loadWorkerHandler();
    onmessage({ data: { type: 'setDiag', enabled: false } });
    expect(setMediaDiagEnabled).toHaveBeenCalledWith(false);
  });
});
