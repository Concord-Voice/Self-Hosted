export type CameraLayerRole = 'thumbnail' | 'grid' | 'focus';
export type LayeredCodecKind = 'svc' | 'simulcast';
export type LayerValue = 0 | 1 | 2;

export interface CameraLayerDemand {
  consumerId: string;
  spatialLayer: LayerValue;
  temporalLayer: LayerValue;
  visible: boolean;
  cssWidth: number;
  cssHeight: number;
  devicePixelRatio: number;
  role: CameraLayerRole;
  focusedWindow: boolean;
  pressureStepDown: boolean;
}

export interface StoredCameraLayerDemand {
  consumerId: string;
  visible: boolean;
  maxUsefulSpatialLayer: LayerValue;
  pressureStepDown: boolean;
}

/**
 * Stored demand plus the owning viewer's `userId` (VULN-158). The layering
 * gates count DISTINCT viewers, not consumer entries: one participant can own
 * many consumers of one producer, so counting entries let a single client
 * force every publisher into layered egress.
 */
export interface ViewerCameraLayerDemand extends StoredCameraLayerDemand {
  userId: string;
}

export type ParseDemandResult =
  | { ok: true; value: CameraLayerDemand }
  | { ok: false; error: 'invalid-consumer-id' | 'invalid-layer' | 'invalid-render-state' };

function isRecord(raw: unknown): raw is Record<string, unknown> {
  return typeof raw === 'object' && raw !== null;
}

function isLayer(n: unknown): n is LayerValue {
  return n === 0 || n === 1 || n === 2;
}

function isRole(role: unknown): role is CameraLayerRole {
  return role === 'thumbnail' || role === 'grid' || role === 'focus';
}

function isValidCssDimension(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0;
}

function isValidDevicePixelRatio(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value > 0 && value <= 8;
}

export function parseCameraLayerDemand(raw: unknown): ParseDemandResult {
  if (!isRecord(raw)) return { ok: false, error: 'invalid-render-state' };

  if (
    typeof raw.consumerId !== 'string' ||
    raw.consumerId.length === 0 ||
    raw.consumerId !== raw.consumerId.trim()
  ) {
    return { ok: false, error: 'invalid-consumer-id' };
  }
  if (!isLayer(raw.spatialLayer) || !isLayer(raw.temporalLayer)) {
    return { ok: false, error: 'invalid-layer' };
  }
  if (typeof raw.visible !== 'boolean') return { ok: false, error: 'invalid-render-state' };
  if (!isValidCssDimension(raw.cssWidth) || !isValidCssDimension(raw.cssHeight)) {
    return { ok: false, error: 'invalid-render-state' };
  }
  if (!isValidDevicePixelRatio(raw.devicePixelRatio)) {
    return { ok: false, error: 'invalid-render-state' };
  }
  if (!isRole(raw.role)) return { ok: false, error: 'invalid-render-state' };
  if (typeof raw.focusedWindow !== 'boolean' || typeof raw.pressureStepDown !== 'boolean') {
    return { ok: false, error: 'invalid-render-state' };
  }

  return {
    ok: true,
    value: {
      consumerId: raw.consumerId,
      spatialLayer: raw.spatialLayer,
      temporalLayer: raw.temporalLayer,
      visible: raw.visible,
      cssWidth: raw.cssWidth,
      cssHeight: raw.cssHeight,
      devicePixelRatio: raw.devicePixelRatio,
      role: raw.role,
      focusedWindow: raw.focusedWindow,
      pressureStepDown: raw.pressureStepDown,
    },
  };
}

function layerForRender(demand: CameraLayerDemand): LayerValue {
  if (!demand.visible) return 0;

  const maxEdge = Math.max(demand.cssWidth, demand.cssHeight) * demand.devicePixelRatio;
  let layer: number = layerForMaxEdge(maxEdge);

  if (demand.role === 'thumbnail') layer = Math.min(layer, 0);
  if (demand.role === 'grid') layer = Math.min(layer, 1);
  if (!demand.focusedWindow) layer -= 1;
  if (demand.pressureStepDown) layer -= 1;

  if (layer <= 0) return 0;
  if (layer >= 2) return 2;
  return 1;
}

function layerForMaxEdge(maxEdge: number): LayerValue {
  if (maxEdge <= 540) return 0;
  if (maxEdge <= 1280) return 1;
  return 2;
}

export function clampCameraLayerDemand(
  demand: CameraLayerDemand,
  maxSpatialLayer: LayerValue = 2
): {
  spatialLayer: LayerValue;
  temporalLayer: LayerValue;
} {
  const spatialLayer = Math.min(
    demand.spatialLayer,
    layerForRender(demand),
    maxSpatialLayer
  ) as LayerValue;
  const temporalLayer = Math.min(demand.temporalLayer, spatialLayer === 0 ? 1 : 2) as LayerValue;
  return { spatialLayer, temporalLayer };
}

export function storedDemand(
  demand: CameraLayerDemand,
  maxSpatialLayer: LayerValue = 2
): StoredCameraLayerDemand {
  return {
    consumerId: demand.consumerId,
    visible: demand.visible,
    maxUsefulSpatialLayer: Math.min(layerForRender(demand), maxSpatialLayer) as LayerValue,
    pressureStepDown: demand.pressureStepDown,
  };
}

export function computeCameraLayeringGate(input: {
  codecKind: LayeredCodecKind;
  cameraProducerCount: number;
  demands: ViewerCameraLayerDemand[];
  previouslyEnabled?: boolean;
}): boolean {
  // Closing the last producer precedes asynchronous consumer-demand cleanup.
  // Stale demands must not arm an SVC gate while no camera can provide layers.
  if (input.cameraProducerCount === 0) return false;
  const visible = input.demands.filter((demand) => demand.visible);
  const distinctViewers = new Set(visible.map((demand) => demand.userId)).size;
  const viewerThreshold = input.previouslyEnabled ? 1 : 2;
  const hasLayerBenefit =
    distinctViewers >= viewerThreshold &&
    visible.some((demand) => demand.maxUsefulSpatialLayer < 2 || demand.pressureStepDown);

  if (!hasLayerBenefit) return false;
  if (input.codecKind === 'svc') return true;
  return input.cameraProducerCount >= (input.previouslyEnabled ? 2 : 3);
}

/**
 * Per-member camera gate flip budget (VULN-158). `set-preferred-layers` allows
 * a burst of 200 events, so the event budget does not bound how often one
 * member can move the room gate, and every applied flip makes every camera
 * publisher re-encode. A member may cause at most
 * `CAMERA_GATE_MEMBER_FLIP_LIMIT` applied transitions per
 * `CAMERA_GATE_MEMBER_FLIP_WINDOW_MS`; a further one is deferred, not refused.
 */
export const CAMERA_GATE_MEMBER_FLIP_LIMIT = 2;
export const CAMERA_GATE_MEMBER_FLIP_WINDOW_MS = 30_000;

function insideFlipWindow(time: number, now: number): boolean {
  return now - time < CAMERA_GATE_MEMBER_FLIP_WINDOW_MS;
}

/**
 * Milliseconds until the member may cause another applied transition: 0 while
 * fewer than `CAMERA_GATE_MEMBER_FLIP_LIMIT` of its flips are inside the window,
 * otherwise the time until enough of the oldest leave it.
 */
export function flipBudgetDelayMs(times: readonly number[] | undefined, now: number): number {
  const recent = (times ?? []).filter((time) => insideFlipWindow(time, now));
  if (recent.length < CAMERA_GATE_MEMBER_FLIP_LIMIT) return 0;
  const blocking = recent[recent.length - CAMERA_GATE_MEMBER_FLIP_LIMIT];
  return blocking + CAMERA_GATE_MEMBER_FLIP_WINDOW_MS - now;
}

/**
 * Record an applied transition against `userId`, then prune every member's
 * history to the window and drop empty entries, so the map is bounded by the
 * members who flipped recently rather than by the room's lifetime.
 */
export function chargeFlip(flips: Map<string, number[]>, userId: string, now: number): void {
  flips.set(userId, [...(flips.get(userId) ?? []), now]);
  for (const [member, times] of flips) {
    const recent = times.filter((time) => insideFlipWindow(time, now));
    if (recent.length === 0) flips.delete(member);
    else flips.set(member, recent);
  }
}
