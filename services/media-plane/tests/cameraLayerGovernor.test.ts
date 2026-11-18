import { describe, expect, it } from 'vitest';
import {
  CAMERA_GATE_MEMBER_FLIP_LIMIT,
  CAMERA_GATE_MEMBER_FLIP_WINDOW_MS,
  chargeFlip,
  clampCameraLayerDemand,
  computeCameraLayeringGate,
  flipBudgetDelayMs,
  parseCameraLayerDemand,
  storedDemand,
} from '../src/lib/cameraLayerGovernor.js';

const validRawDemand = {
  consumerId: 'c1',
  spatialLayer: 2,
  temporalLayer: 2,
  visible: true,
  cssWidth: 1280,
  cssHeight: 720,
  devicePixelRatio: 1,
  role: 'focus',
  focusedWindow: true,
  pressureStepDown: false,
} as const;

describe('parseCameraLayerDemand', () => {
  it('accepts valid render demand', () => {
    expect(
      parseCameraLayerDemand({
        consumerId: 'c1',
        spatialLayer: 2,
        temporalLayer: 2,
        visible: true,
        cssWidth: 1280,
        cssHeight: 720,
        devicePixelRatio: 1,
        role: 'focus',
        focusedWindow: true,
        pressureStepDown: false,
      })
    ).toMatchObject({ ok: true });
  });

  it('rejects malformed render demand', () => {
    expect(
      parseCameraLayerDemand({
        consumerId: 'c1',
        spatialLayer: 5,
        temporalLayer: 2,
        visible: true,
        cssWidth: 1280,
        cssHeight: 720,
        devicePixelRatio: 1,
        role: 'focus',
        focusedWindow: true,
        pressureStepDown: false,
      })
    ).toEqual({ ok: false, error: 'invalid-layer' });
  });

  it('rejects blank consumer ids', () => {
    expect(
      parseCameraLayerDemand({
        consumerId: '   ',
        spatialLayer: 1,
        temporalLayer: 1,
        visible: true,
        cssWidth: 640,
        cssHeight: 360,
        devicePixelRatio: 1,
        role: 'grid',
        focusedWindow: true,
        pressureStepDown: false,
      })
    ).toEqual({ ok: false, error: 'invalid-consumer-id' });
  });

  it('rejects whitespace-padded consumer ids', () => {
    expect(parseCameraLayerDemand({ ...validRawDemand, consumerId: ' c1 ' })).toEqual({
      ok: false,
      error: 'invalid-consumer-id',
    });
  });

  it('rejects invalid render dimensions', () => {
    expect(
      parseCameraLayerDemand({
        consumerId: 'c1',
        spatialLayer: 1,
        temporalLayer: 1,
        visible: true,
        cssWidth: -1,
        cssHeight: 360,
        devicePixelRatio: 1,
        role: 'grid',
        focusedWindow: true,
        pressureStepDown: false,
      })
    ).toEqual({ ok: false, error: 'invalid-render-state' });
  });

  it.each([
    ['null payload', null],
    ['string payload', 'not-demand'],
    ['number payload', 42],
    ['NaN cssWidth', { ...validRawDemand, cssWidth: NaN }],
    ['infinite cssHeight', { ...validRawDemand, cssHeight: Infinity }],
  ])('rejects %s', (_name, raw) => {
    expect(parseCameraLayerDemand(raw)).toEqual({ ok: false, error: 'invalid-render-state' });
  });
});

describe('clampCameraLayerDemand', () => {
  it('caps 720p focus to mid layer', () => {
    const demand = parseCameraLayerDemand({
      consumerId: 'c1',
      spatialLayer: 2,
      temporalLayer: 2,
      visible: true,
      cssWidth: 1280,
      cssHeight: 720,
      devicePixelRatio: 1,
      role: 'focus',
      focusedWindow: true,
      pressureStepDown: false,
    });
    if (!demand.ok) throw new Error('unexpected invalid demand');
    expect(clampCameraLayerDemand(demand.value)).toEqual({ spatialLayer: 1, temporalLayer: 2 });
  });

  it('caps hidden demand to the lowest spatial layer', () => {
    const demand = parseCameraLayerDemand({
      consumerId: 'c1',
      spatialLayer: 2,
      temporalLayer: 2,
      visible: false,
      cssWidth: 1280,
      cssHeight: 720,
      devicePixelRatio: 1,
      role: 'focus',
      focusedWindow: true,
      pressureStepDown: false,
    });
    if (!demand.ok) throw new Error('unexpected invalid demand');
    expect(clampCameraLayerDemand(demand.value)).toEqual({ spatialLayer: 0, temporalLayer: 1 });
  });

  it('applies a server-authoritative spatial layer cap', () => {
    const demand = parseCameraLayerDemand({
      consumerId: 'c1',
      spatialLayer: 2,
      temporalLayer: 2,
      visible: true,
      cssWidth: 1920,
      cssHeight: 1080,
      devicePixelRatio: 1,
      role: 'focus',
      focusedWindow: true,
      pressureStepDown: false,
    });
    if (!demand.ok) throw new Error('unexpected invalid demand');
    expect(clampCameraLayerDemand(demand.value, 1)).toEqual({ spatialLayer: 1, temporalLayer: 2 });
  });

  it('uses physical pixels from css size and devicePixelRatio', () => {
    const demand = parseCameraLayerDemand({
      consumerId: 'c1',
      spatialLayer: 2,
      temporalLayer: 2,
      visible: true,
      cssWidth: 700,
      cssHeight: 400,
      devicePixelRatio: 2,
      role: 'focus',
      focusedWindow: true,
      pressureStepDown: false,
    });
    if (!demand.ok) throw new Error('unexpected invalid demand');
    expect(clampCameraLayerDemand(demand.value)).toEqual({ spatialLayer: 2, temporalLayer: 2 });
  });
});

describe('storedDemand', () => {
  it('stores the render-derived max useful spatial layer', () => {
    const demand = parseCameraLayerDemand({
      consumerId: 'c1',
      spatialLayer: 2,
      temporalLayer: 2,
      visible: true,
      cssWidth: 1280,
      cssHeight: 720,
      devicePixelRatio: 1,
      role: 'focus',
      focusedWindow: true,
      pressureStepDown: true,
    });
    if (!demand.ok) throw new Error('unexpected invalid demand');
    expect(storedDemand(demand.value)).toEqual({
      consumerId: 'c1',
      visible: true,
      maxUsefulSpatialLayer: 0,
      pressureStepDown: true,
    });
  });

  it('stores the server-capped max useful spatial layer', () => {
    const demand = parseCameraLayerDemand({
      consumerId: 'c1',
      spatialLayer: 2,
      temporalLayer: 2,
      visible: true,
      cssWidth: 1920,
      cssHeight: 1080,
      devicePixelRatio: 1,
      role: 'focus',
      focusedWindow: true,
      pressureStepDown: false,
    });
    if (!demand.ok) throw new Error('unexpected invalid demand');
    expect(storedDemand(demand.value, 1)).toMatchObject({ maxUsefulSpatialLayer: 1 });
  });
});

describe('computeCameraLayeringGate', () => {
  it('keeps the gate off during a last-producer replacement gap with stale demands', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'svc',
        cameraProducerCount: 0,
        demands: [
          {
            consumerId: 'c1',
            userId: 'v1',
            visible: true,
            maxUsefulSpatialLayer: 1,
            pressureStepDown: false,
          },
          {
            consumerId: 'c2',
            userId: 'v2',
            visible: true,
            maxUsefulSpatialLayer: 2,
            pressureStepDown: false,
          },
        ],
        previouslyEnabled: true,
      })
    ).toBe(false);
  });

  it('enables SVC when two visible consumers include lower-layer benefit', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'svc',
        cameraProducerCount: 2,
        demands: [
          {
            consumerId: 'c1',
            userId: 'v1',
            visible: true,
            maxUsefulSpatialLayer: 1,
            pressureStepDown: false,
          },
          {
            consumerId: 'c2',
            userId: 'v2',
            visible: true,
            maxUsefulSpatialLayer: 2,
            pressureStepDown: false,
          },
        ],
      })
    ).toBe(true);
  });

  it('keeps simulcast fallback off until there are at least three camera producers', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'simulcast',
        cameraProducerCount: 2,
        demands: [
          {
            consumerId: 'c1',
            userId: 'v1',
            visible: true,
            maxUsefulSpatialLayer: 1,
            pressureStepDown: false,
          },
          {
            consumerId: 'c2',
            userId: 'v2',
            visible: true,
            maxUsefulSpatialLayer: 2,
            pressureStepDown: false,
          },
        ],
      })
    ).toBe(false);
  });

  it('enables simulcast fallback with at least three camera producers and layer benefit', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'simulcast',
        cameraProducerCount: 3,
        demands: [
          {
            consumerId: 'c1',
            userId: 'v1',
            visible: true,
            maxUsefulSpatialLayer: 1,
            pressureStepDown: false,
          },
          {
            consumerId: 'c2',
            userId: 'v2',
            visible: true,
            maxUsefulSpatialLayer: 2,
            pressureStepDown: false,
          },
        ],
      })
    ).toBe(true);
  });

  it('enables SVC when visible top-layer consumers include pressure-only benefit', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'svc',
        cameraProducerCount: 2,
        demands: [
          {
            consumerId: 'c1',
            userId: 'v1',
            visible: true,
            maxUsefulSpatialLayer: 2,
            pressureStepDown: true,
          },
          {
            consumerId: 'c2',
            userId: 'v2',
            visible: true,
            maxUsefulSpatialLayer: 2,
            pressureStepDown: false,
          },
        ],
      })
    ).toBe(true);
  });

  it('ignores hidden lower-layer and pressure demand when computing benefit', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'svc',
        cameraProducerCount: 2,
        demands: [
          {
            consumerId: 'c1',
            userId: 'v1',
            visible: true,
            maxUsefulSpatialLayer: 2,
            pressureStepDown: false,
          },
          {
            consumerId: 'c2',
            userId: 'v2',
            visible: false,
            maxUsefulSpatialLayer: 1,
            pressureStepDown: true,
          },
        ],
      })
    ).toBe(false);
  });

  it('keeps layering off when only one consumer is visible', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'svc',
        cameraProducerCount: 3,
        demands: [
          {
            consumerId: 'c1',
            userId: 'v1',
            visible: true,
            maxUsefulSpatialLayer: 1,
            pressureStepDown: false,
          },
          {
            consumerId: 'c2',
            userId: 'v2',
            visible: false,
            maxUsefulSpatialLayer: 2,
            pressureStepDown: false,
          },
        ],
      })
    ).toBe(false);
  });

  it('keeps SVC enabled at the one-visible-consumer hysteresis floor', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'svc',
        cameraProducerCount: 2,
        demands: [
          {
            consumerId: 'c1',
            userId: 'v1',
            visible: true,
            maxUsefulSpatialLayer: 1,
            pressureStepDown: false,
          },
        ],
        previouslyEnabled: true,
      })
    ).toBe(true);
  });

  it('keeps simulcast enabled at the two-producer hysteresis floor', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'simulcast',
        cameraProducerCount: 2,
        demands: [
          {
            consumerId: 'c1',
            userId: 'v1',
            visible: true,
            maxUsefulSpatialLayer: 1,
            pressureStepDown: false,
          },
          {
            consumerId: 'c2',
            userId: 'v2',
            visible: true,
            maxUsefulSpatialLayer: 2,
            pressureStepDown: false,
          },
        ],
        previouslyEnabled: true,
      })
    ).toBe(true);
  });

  // VULN-158: the threshold counts distinct viewers, not consumer entries. One
  // client opening many camera consumers is still one viewer.
  describe('distinct viewers (VULN-158)', () => {
    const demand = (consumerId: string, userId: string, maxUsefulSpatialLayer: 0 | 1 | 2) => ({
      consumerId,
      userId,
      visible: true,
      maxUsefulSpatialLayer,
      pressureStepDown: false,
    });

    it('one viewer with many visible consumers does not enable the gate', () => {
      expect(
        computeCameraLayeringGate({
          codecKind: 'svc',
          cameraProducerCount: 3,
          demands: [demand('c1', 'v1', 1), demand('c2', 'v1', 2), demand('c3', 'v1', 0)],
        })
      ).toBe(false);
    });

    it('two distinct viewers enable the gate', () => {
      expect(
        computeCameraLayeringGate({
          codecKind: 'svc',
          cameraProducerCount: 3,
          demands: [demand('c1', 'v1', 1), demand('c2', 'v2', 2)],
        })
      ).toBe(true);
    });

    it('one viewer keeps an enabled gate on', () => {
      expect(
        computeCameraLayeringGate({
          codecKind: 'svc',
          cameraProducerCount: 3,
          demands: [demand('c1', 'v1', 1), demand('c2', 'v1', 2)],
          previouslyEnabled: true,
        })
      ).toBe(true);
    });
  });
});

// VULN-158: each member may move the camera gate a bounded number of times per
// window; the room keeps the per-member history.
describe('camera gate flip budget (VULN-158)', () => {
  const t = 1_000_000;

  it('allows two flips per member per 30 seconds', () => {
    expect(CAMERA_GATE_MEMBER_FLIP_LIMIT).toBe(2);
    expect(CAMERA_GATE_MEMBER_FLIP_WINDOW_MS).toBe(30_000);
  });

  it('flipBudgetDelayMs is 0 for a member with no history', () => {
    expect(flipBudgetDelayMs(undefined, t)).toBe(0);
  });

  it('flipBudgetDelayMs is 0 below the limit inside the window', () => {
    expect(flipBudgetDelayMs([t], t + 1)).toBe(0);
  });

  it('flipBudgetDelayMs waits until the oldest flip leaves the window', () => {
    expect(flipBudgetDelayMs([t, t + 1], t + 2)).toBe(CAMERA_GATE_MEMBER_FLIP_WINDOW_MS - 2);
  });

  it('flipBudgetDelayMs ignores flips outside the window', () => {
    expect(flipBudgetDelayMs([t, t + 1], t + 1 + CAMERA_GATE_MEMBER_FLIP_WINDOW_MS)).toBe(0);
  });

  it('chargeFlip prunes an expired key and removes it from the map', () => {
    const now = t + CAMERA_GATE_MEMBER_FLIP_WINDOW_MS;
    const flips = new Map<string, number[]>([
      ['expired', [t]],
      ['kept', [now - 1]],
    ]);

    chargeFlip(flips, 'charged', now);

    expect([...flips.entries()]).toEqual([
      ['kept', [now - 1]],
      ['charged', [now]],
    ]);
  });
});
