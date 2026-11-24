/**
 * Wire checks for client-supplied ids and handshake text (VULN-157, VULN-147).
 *
 * A value that fails these checks is refused before any log line or room
 * lookup sees it, so neither its type nor its length reaches a sink. The log
 * format (`logBounds.ts`) still bounds every line; these checks keep the raw
 * value out of the line in the first place.
 */

/** Ids the media plane hands out are UUIDs; 64 characters leaves headroom. */
export const WIRE_ID_PATTERN = /^[A-Za-z0-9_-]{1,64}$/;

/** The product maximum is 32 and registration accepts up to 50. */
export const HANDSHAKE_USERNAME_MAX_CHARS = 64;

export function isBoundedWireId(value: unknown): value is string {
  return typeof value === 'string' && WIRE_ID_PATTERN.test(value);
}

export interface ConsumeRequest {
  producerId: string;
  transportId?: string;
}

/**
 * Parses a `consume` payload. `producerId` must be a bounded wire id;
 * `transportId` must be absent or one too (the resolved receive transport is
 * authoritative, so the optional hint is never trusted beyond its shape).
 * Returns `undefined` for anything else, including a non-object payload and a
 * `null` transportId.
 */
export function parseConsumeRequest(payload: unknown): ConsumeRequest | undefined {
  if (typeof payload !== 'object' || payload === null || Array.isArray(payload)) return undefined;
  const { producerId, transportId } = payload as Record<string, unknown>;
  if (!isBoundedWireId(producerId)) return undefined;
  if (transportId === undefined) return { producerId };
  if (!isBoundedWireId(transportId)) return undefined;
  return { producerId, transportId };
}
