/**
 * Bus MAC purpose registry: the TS mirror of
 * `services/control-plane/pkg/busmac/registry.go`.
 *
 * A string has exactly one kind. A family is an enveloped wire format
 * (sign, open, mac, verifyMac); a raw purpose is for detached MACs over
 * caller-supplied lines (rawMac, rawVerify). Keeping the two disjoint is what
 * stops a raw caller from minting or accepting a family MAC by passing that
 * family's own lines. Security#175 adds its raw purposes here and in the Go
 * table together; the shared vectors pin the family set on both sides.
 */
export const FAMILY_LIFECYCLE = 'mp>cp/lifecycle';
export const FAMILY_TELEMETRY = 'mp>cp/telemetry';

export type BusMacPurposeKind = 'family' | 'raw';

export const BUS_MAC_PURPOSES: ReadonlyMap<string, BusMacPurposeKind> = new Map<
  string,
  BusMacPurposeKind
>([
  [FAMILY_LIFECYCLE, 'family'],
  [FAMILY_TELEMETRY, 'family'],
]);
