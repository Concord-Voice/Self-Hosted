/**
 * Media E2EE v6 for Insertable Streams. AES-256-GCM encrypts codec payloads;
 * every clear byte emitted to a decoder is authenticated as frame AAD.
 *
 * Opus/VP8/VP9 keep a clear codec prefix and use a 23-byte trailer. H.264
 * keeps the complete Annex-B prefix through the first slice header and a
 * stuffed 22-byte mini-header. AV1 encrypts every frame/tile-group payload
 * and authenticates one ordered manifest of all OBUs under every tile tag.
 * The only transport normalization measured in the two-client v5 rig is one
 * exact empty leading AV1 temporal delimiter (12 00), which is discarded and
 * never reaches the decoder. Unknown codecs and older frame versions fail closed.
 */

import {
  parseAv1Obus,
  AV1_OBU_FRAME,
  AV1_OBU_TILE_GROUP,
  AV1_OBU_SEQUENCE_HEADER,
  AV1_OBU_FRAME_HEADER,
  AV1_OBU_PADDING,
  AV1_OBU_TEMPORAL_DELIMITER,
  type ParsedObu,
} from './av1ObuParser';
import {
  MEDIA_FRAME_MINI_HEADER_SIZE,
  MEDIA_FRAME_VERSION,
  encodeMediaFrameMiniHeader,
  decodeMediaFrameMiniHeader,
  buildObuIv,
} from './mediaFrameMiniHeader';
import {
  findH264SlicePrefixLength,
  parseH264AnnexB,
  stuffH264Bytes,
  unstuffH264Bytes,
} from './h264NalUnit';

const SALT = new TextEncoder().encode('concord-voice-e2ee');
const RATCHET_INFO = new TextEncoder().encode('concord-e2ee-ratchet');

// Magic trailer bytes appended to every encrypted frame so the receiver can
// distinguish encrypted frames from unencrypted ones.
const MAGIC_0 = 0xde;
const MAGIC_1 = 0xad;
export { MEDIA_FRAME_VERSION as MEDIA_E2EE_FRAME_CRYPTO_VERSION } from './mediaFrameMiniHeader';
// v6 adds a per-frame authenticated scheme byte to the whole-frame trailer.
const TRAILER_SIZE_V6 = 23;
const FRAME_SCHEMES: Record<CodecFamily, number> = {
  opus: 1,
  vp8: 2,
  vp9: 3,
  av1: 4,
  h264: 5,
};
const FRAME_AAD_DOMAIN = new TextEncoder().encode('concord-voice/media-frame\0');
const MAX_DECRYPT_KEYS = 2048;
const RATCHET_WORK_WINDOW_MS = 15_000;
const RATCHET_KEY_BACKOFF_MS = 350;
const MAX_RATCHET_STEPS_PER_SENDER = 10;
const MAX_RATCHET_STEPS_GLOBAL = 64;
const MAX_RATCHET_WORK_TRACKED = 2048;
const MAX_RATCHET_KEY_ATTEMPTS_TRACKED = 512;
// Minimum encrypted frame: 1 header byte + 16-byte GCM tag (empty plaintext) + trailer.
const MIN_GCM_OVERHEAD = 17;

/**
 * Codec family for frame-crypto dispatch (spec §6.1 / §6.2).
 * AV1 uses per-OBU payload encryption, H.264 uses NAL-aware suffix encryption,
 * and Opus/VP8/VP9 use whole-frame v6. Other codecs are rejected.
 * This union is kept in sync with the worker protocol's codec resolver.
 */
export type CodecFamily = 'opus' | 'vp8' | 'vp9' | 'av1' | 'h264';

/**
 * Determine how many leading bytes of the frame payload must be left
 * unencrypted. The unencrypted prefix preserves codec framing so the
 * decoder can identify keyframes, Opus modes, etc.
 *
 * The sender encodes this value in the frame trailer (1-byte headerBytes
 * field), so the receiver reads it directly instead of recomputing it.
 * The authenticated codec scheme makes BUNDLE frame misrouting fail closed.
 *
 * Using 1 byte for audio (Opus TOC) and 2 bytes for video (covers VP8
 * partition flag + VP9 frame header) is the conservative minimum.
 */
function getUnencryptedBytes(codec: CodecFamily): number {
  return codec === 'opus' ? 1 : 2;
}

/** Derive an AES-256-GCM key from a channel CSK + sender userId via HKDF */
export async function deriveFrameKey(
  channelCSK: CryptoKey,
  senderUserId: string
): Promise<CryptoKey> {
  // Export the CSK to raw bytes for use as HKDF input key material
  const cskBytes = await crypto.subtle.exportKey('raw', channelCSK);

  // Import as HKDF key
  const hkdfKey = await crypto.subtle.importKey('raw', cskBytes, 'HKDF', false, ['deriveKey']);

  // Derive AES-256-GCM frame key (extractable: true so it can be ratcheted on epoch rotation)
  return crypto.subtle.deriveKey(
    {
      name: 'HKDF',
      hash: 'SHA-256',
      salt: SALT,
      info: new TextEncoder().encode(senderUserId),
    },
    hkdfKey,
    { name: 'AES-GCM', length: 256 },
    true,
    ['encrypt', 'decrypt']
  );
}

/** Ratchet a key forward (for epoch rotation) */
export async function ratchetKey(currentKey: CryptoKey): Promise<CryptoKey> {
  const keyBytes = await crypto.subtle.exportKey('raw', currentKey);
  const hkdfKey = await crypto.subtle.importKey('raw', keyBytes, 'HKDF', false, ['deriveKey']);
  return crypto.subtle.deriveKey(
    {
      name: 'HKDF',
      hash: 'SHA-256',
      salt: SALT,
      info: RATCHET_INFO,
    },
    hkdfKey,
    { name: 'AES-GCM', length: 256 },
    true,
    ['encrypt', 'decrypt']
  );
}

// ---------------------------------------------------------------------------
// AV1 per-OBU helpers (#1895)
// ---------------------------------------------------------------------------

/**
 * Encode a non-negative integer as AV1 leb128 (little-endian base-128). Uses
 * integer division (not `<<`) so values above 2^28 don't overflow the 32-bit
 * shift; bounded by the caller (OBU payloads are size-capped by the parser).
 */
function encodeLeb128(value: number): Uint8Array {
  const out: number[] = [];
  let v = value >>> 0;
  do {
    let b = v & 0x7f;
    v = Math.floor(v / 128);
    if (v !== 0) b |= 0x80;
    out.push(b);
  } while (v !== 0);
  return new Uint8Array(out);
}

/**
 * Concatenate a list of byte chunks into a single contiguous Uint8Array backed
 * by a plain ArrayBuffer (so `.buffer` is assignable to `frame.data`).
 */
function concatBytes(parts: Uint8Array[]): Uint8Array<ArrayBuffer> {
  let total = 0;
  for (const p of parts) total += p.length;
  const out = new Uint8Array(new ArrayBuffer(total));
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
}

function uint16be(value: number): Uint8Array {
  const bytes = new Uint8Array(2);
  new DataView(bytes.buffer).setUint16(0, value, false);
  return bytes;
}

function uint32be(value: number): Uint8Array {
  const bytes = new Uint8Array(4);
  new DataView(bytes.buffer).setUint32(0, value, false);
  return bytes;
}

interface Av1CanonicalEntry {
  type: number;
  head: Uint8Array;
  payload: Uint8Array;
  plainLength: number;
  mini?: Uint8Array;
  iv?: Uint8Array;
}

function av1Manifest(entries: Av1CanonicalEntry[]): Uint8Array<ArrayBuffer> {
  const parts: Uint8Array[] = [uint16be(entries.length)];
  for (let i = 0; i < entries.length; i++) {
    const entry = entries[i];
    parts.push(
      uint16be(i),
      new Uint8Array([entry.type, entry.head.length]),
      entry.head,
      new Uint8Array([entry.mini ? 1 : 0]),
      uint32be(entry.plainLength),
      entry.mini ?? entry.payload
    );
  }
  return concatBytes(parts);
}

function av1Aad(
  manifestLength: number,
  manifestDigest: Uint8Array,
  ordinal: number
): Uint8Array<ArrayBuffer> {
  return concatBytes([
    FRAME_AAD_DOMAIN,
    new Uint8Array([MEDIA_FRAME_VERSION, FRAME_SCHEMES.av1]),
    uint32be(manifestLength),
    manifestDigest,
    uint16be(ordinal),
  ]);
}

interface H264AccessUnitBoundary {
  clearEnd: number;
  firstVclIsTerminal: boolean;
}

/** Locate the clear/AAD boundary independently on each side of the transform. */
function locateH264AccessUnitBoundary(data: Uint8Array): H264AccessUnitBoundary {
  if (data.length === 0) throw new Error('E2EE: empty H.264 access unit');
  const units = parseH264AnnexB(data);
  if (units === null) throw new Error('E2EE: malformed or unsupported H.264 access unit');
  const firstVclIndex = units.findIndex((unit) => unit.kind === 'slice');
  const firstVcl = units[firstVclIndex];
  if (!firstVcl) throw new Error('E2EE: H.264 access unit has no supported VCL NAL');

  const slicePayloadStart = firstVcl.nalOffset + 1;
  const slicePrefixLength = findH264SlicePrefixLength(
    data.subarray(slicePayloadStart, firstVcl.nalOffset + firstVcl.nalLength)
  );
  if (slicePrefixLength === null) {
    throw new Error('E2EE: malformed H.264 first-slice prefix');
  }
  return {
    clearEnd: slicePayloadStart + slicePrefixLength,
    firstVclIsTerminal: firstVclIndex === units.length - 1,
  };
}

function trailingZeroRun(data: Uint8Array): number {
  let count = 0;
  for (let offset = data.length - 1; offset >= 0 && count < 2 && data[offset] === 0; offset--) {
    count++;
  }
  return count;
}

// ---------------------------------------------------------------------------
// FrameKeyMissError
// ---------------------------------------------------------------------------

/**
 * #1878: thrown when `decryptFrame` has no key for the frame's exact
 * (senderUserId, keyVersion, keyId) and cannot ratchet to it. Distinct from a
 * WebCrypto `OperationError` (GCM auth failure on a wrong-base key) so the
 * worker can route a typed miss to on-demand key provisioning (`requestFrameKey`)
 * while leaving the OperationError/persistent-failure self-heal path untouched.
 * The message still contains "no decrypt key" so existing regex assertions hold.
 */
export class FrameKeyMissError extends Error {
  override name = 'FrameKeyMissError';
  constructor(
    public senderUserId: string,
    public keyVersion: number,
    public keyId: number,
    message: string
  ) {
    super(message);
  }
}

// ---------------------------------------------------------------------------
// MediaEncryption class
// ---------------------------------------------------------------------------

export class MediaEncryption {
  private destroyed = false;
  private currentKeyId = 0;
  // #1878: the channel-key (CSK) version bound to the encrypt key. Stamped into
  // every outgoing frame's trailer or codec mini-header so receivers select the exact key.
  private currentKeyVersion = 0;
  private encryptKey: CryptoKey | null = null;
  private readonly decryptKeys: Map<string, { key: CryptoKey; expires: number }> = new Map();
  private readonly ratchetWorkBySender = new Map<string, { windowStart: number; steps: number }>();
  private ratchetWorkGlobal = { windowStart: 0, steps: 0 };
  private readonly ratchetKeyAttempts = new Map<string, number>();
  private rotationChain: Promise<void> = Promise.resolve();

  /**
   * Compose the decrypt-key map key. #1878: keyed by senderId:keyVersion:keyId
   * (was senderId:keyId — collided across CSK versions). Sender userIds are
   * UUIDs (no embedded colons), so the 3-part split is unambiguous.
   */
  private static mapKey(senderUserId: string, keyVersion: number, keyId: number): string {
    return `${senderUserId}:${keyVersion}:${keyId}`;
  }

  private rememberDecryptKey(
    senderUserId: string,
    keyVersion: number,
    keyId: number,
    key: CryptoKey
  ): void {
    if (this.destroyed) throw new Error('E2EE: media encryption destroyed');
    const now = Date.now();
    if (keyVersion < this.currentKeyVersion) {
      throw new Error('E2EE: cannot install a retired decrypt key version');
    }
    for (const [id, entry] of this.decryptKeys) {
      if (entry.expires <= now) this.decryptKeys.delete(id);
    }
    const id = MediaEncryption.mapKey(senderUserId, keyVersion, keyId);
    for (const [heldId, entry] of this.decryptKeys) {
      const [heldSender, heldVersion, heldKeyId] = heldId.split(':');
      if (heldSender !== senderUserId || Number(heldVersion) !== keyVersion) continue;
      if (Number(heldKeyId) > keyId) {
        throw new Error('E2EE: cannot reinstall a retired decrypt key epoch');
      }
      if (Number(heldKeyId) < keyId) entry.expires = Math.min(entry.expires, now + 10_000);
    }
    this.ratchetKeyAttempts.delete(id);
    this.decryptKeys.delete(id);
    // Active, authority-provisioned keys have no wall-clock cutoff. Only a
    // newer version/epoch retires them; replayed old media cannot extend that.
    this.decryptKeys.set(id, { key, expires: Number.POSITIVE_INFINITY });
    while (this.decryptKeys.size > MAX_DECRYPT_KEYS) {
      const oldest = this.decryptKeys.keys().next().value;
      if (oldest !== undefined) this.decryptKeys.delete(oldest);
    }
  }

  // Debounced key rotation — collapses rapid join/leave bursts into a single
  // rotation to avoid O(N²) HKDF cost when many users join simultaneously.
  private rotationTimer: ReturnType<typeof setTimeout> | null = null;
  private rotationPending = false;
  private rotationDeadline = 0; // Max-cap absolute deadline (0 = none)
  private static readonly DEBOUNCE_MS = 2000;
  private static readonly MAX_CAP_MS = 5000;

  /** Initialize encryption for a channel with the given CSK and local userId */
  async init(channelCSK: CryptoKey, localUserId: string): Promise<void> {
    const key = await deriveFrameKey(channelCSK, localUserId);
    if (this.destroyed) throw new Error('E2EE: media encryption destroyed');
    this.encryptKey = key;
    this.currentKeyId = 0;
  }

  /** Initialize encryption with a pre-derived key (used by Worker) */
  initFromKey(key: CryptoKey, keyId: number): void {
    if (this.destroyed) throw new Error('E2EE: media encryption destroyed');
    this.encryptKey = key;
    this.currentKeyId = keyId;
  }

  /** Update the current epoch ID without changing the encrypt key (main-thread sync) */
  setCurrentKeyId(keyId: number): void {
    this.currentKeyId = keyId;
  }

  /** Bind the encrypt key's channel-key version (the authoritative CSK version). */
  setKeyVersion(keyVersion: number): void {
    if (keyVersion < this.currentKeyVersion) throw new Error('E2EE: key version downgrade');
    if (keyVersion > this.currentKeyVersion) {
      const retireAt = Date.now() + 10_000;
      for (const [id, entry] of this.decryptKeys) {
        const version = Number(id.split(':')[1]);
        if (version < keyVersion) entry.expires = Math.min(entry.expires, retireAt);
      }
    }
    this.currentKeyVersion = keyVersion;
  }

  getKeyVersion(): number {
    return this.currentKeyVersion;
  }

  /**
   * Legacy 2-arg worker-path adder — defaults keyVersion to the encrypt
   * version. Thin wrapper over addDecryptKeyDirectV3; remaining call sites
   * pass version explicitly in a later task (#1878 Task 5).
   */
  addDecryptKeyDirect(senderUserId: string, keyId: number, key: CryptoKey): void {
    this.addDecryptKeyDirectV3(senderUserId, this.currentKeyVersion, keyId, key);
  }

  /** Worker-path: add a pre-derived decrypt key at an explicit (version, keyId). */
  addDecryptKeyDirectV3(
    senderUserId: string,
    keyVersion: number,
    keyId: number,
    key: CryptoKey
  ): void {
    this.rememberDecryptKey(senderUserId, keyVersion, keyId, key);
  }

  /**
   * Legacy 2-arg adder — defaults keyVersion to the encrypt version. Thin
   * wrapper over addDecryptKeyAtVersion (#1878 Task 5 removes remaining
   * version-blind call sites).
   */
  async addDecryptKey(channelCSK: CryptoKey, senderUserId: string, keyId = 0): Promise<CryptoKey> {
    return this.addDecryptKeyAtVersion(channelCSK, senderUserId, this.currentKeyVersion, keyId);
  }

  /**
   * Legacy adder pre-ratcheted to a target epoch — defaults keyVersion to the
   * encrypt version. Thin wrapper over addDecryptKeyAtVersion.
   */
  async addDecryptKeyAtEpoch(
    channelCSK: CryptoKey,
    senderUserId: string,
    targetEpoch: number
  ): Promise<CryptoKey> {
    return this.addDecryptKeyAtVersion(
      channelCSK,
      senderUserId,
      this.currentKeyVersion,
      targetEpoch
    );
  }

  /**
   * Main-path: derive + ratchet a decrypt key for an explicit (version, keyId).
   * Avoids the expensive per-frame self-healing ratchet when a decrypt key is
   * added mid-session (e.g., during tuneIn or late new-producer events).
   */
  async addDecryptKeyAtVersion(
    channelCSK: CryptoKey,
    senderUserId: string,
    keyVersion: number,
    keyId = 0
  ): Promise<CryptoKey> {
    if (keyId > 100) {
      throw new Error('E2EE epoch gap too large (' + keyId + '), rejoin required');
    }
    let key = await deriveFrameKey(channelCSK, senderUserId);
    for (let i = 0; i < keyId; i++) {
      key = await ratchetKey(key);
    }
    this.rememberDecryptKey(senderUserId, keyVersion, keyId, key);
    return key;
  }

  /** Rotate keys (new epoch) — ratchet forward, keep old keys for overlap */
  async rotateKeys(): Promise<void> {
    const run = this.rotationChain.then(
      () => this.rotateKeysOnce(),
      () => this.rotateKeysOnce()
    );
    this.rotationChain = run.catch(() => {});
    return run;
  }

  private async rotateKeysOnce(): Promise<void> {
    if (!this.encryptKey) return;

    const oldKeyId = this.currentKeyId;
    const newKeyId = oldKeyId + 1;
    const currentEncryptKey = this.encryptKey;
    const newEncryptKey = await ratchetKey(currentEncryptKey);
    if (this.encryptKey !== currentEncryptKey || this.currentKeyId !== oldKeyId) return;
    this.currentKeyId = newKeyId;
    this.encryptKey = newEncryptKey;

    // Ratchet ALL decrypt keys at oldKeyId → newKeyId so receivers stay in
    // sync with senders.  Without this, decrypt keys stay at keyId 0 while
    // senders advance on every join/leave, causing "No decrypt key" errors
    // once the gap exceeds the self-healing ratchet limit.
    // #1878: the map key is now senderId:keyVersion:keyId (3-part). Match the
    // keyId field by parsing, not endsWith(':oldKeyId') — and re-insert at the
    // SAME keyVersion so each version's ratchet chain stays independent.
    const toRatchet: {
      senderUserId: string;
      keyVersion: number;
      entry: { key: CryptoKey; expires: number };
    }[] = [];
    for (const [id, entry] of this.decryptKeys) {
      const parts = id.split(':');
      if (parts.length !== 3) continue; // sender UUIDs have no colons
      const [senderUserId, ver, kid] = parts;
      if (
        Number(kid) === oldKeyId &&
        Number(ver) >= this.currentKeyVersion &&
        entry.expires > Date.now()
      ) {
        toRatchet.push({ senderUserId, keyVersion: Number(ver), entry });
      }
    }
    for (const { senderUserId, keyVersion, entry } of toRatchet) {
      const newKey = await ratchetKey(entry.key);
      this.rememberDecryptKey(senderUserId, keyVersion, newKeyId, newKey);
      // Keep the old keyId for a 10s overlap so in-flight frames from before the
      // ratchet still decrypt. This is a delivery-overlap window, NOT a forward-
      // secrecy boundary (intra-version FS is intentionally dropped — #1878
      // Decision B; FS is enforced by CSK key_version rotation on membership
      // change).
      entry.expires = Date.now() + 10_000;
    }

    // Clean expired keys
    const now = Date.now();
    for (const [id, entry] of this.decryptKeys) {
      if (entry.expires < now) {
        this.decryptKeys.delete(id);
      }
    }
  }

  /** Current epoch number (for sync checks) */
  getCurrentKeyId(): number {
    return this.currentKeyId;
  }

  /**
   * Debounced key rotation: collapses rapid join/leave bursts into a single
   * rotation. Waits 2s of silence, but never delays more than 5s from the
   * first pending event (max cap prevents indefinite deferral during a
   * sustained trickle of joins).
   */
  debouncedRotateKeys(): void {
    this.rotationPending = true;

    // Set the max-cap deadline on the first pending event
    if (!this.rotationDeadline) {
      this.rotationDeadline = Date.now() + MediaEncryption.MAX_CAP_MS;
    }

    if (this.rotationTimer) clearTimeout(this.rotationTimer);

    // If we've already exceeded the max cap, rotate immediately
    const remaining = this.rotationDeadline - Date.now();
    const delay = remaining <= 0 ? 0 : Math.min(MediaEncryption.DEBOUNCE_MS, remaining);

    this.rotationTimer = setTimeout(async () => {
      this.rotationTimer = null;
      this.rotationDeadline = 0;
      if (!this.rotationPending) return;
      this.rotationPending = false;
      await this.rotateKeys();
    }, delay);
  }

  /**
   * Catch up to a server-reported epoch by ratcheting forward.
   * Used by the periodic epoch-sync mechanism to recover from missed events.
   */
  async catchUpToEpoch(targetEpoch: number): Promise<void> {
    while (this.currentKeyId < targetEpoch) {
      const steps = targetEpoch - this.currentKeyId;
      if (steps > 100) {
        throw new Error('E2EE epoch gap too large (' + steps + '), rejoin required');
      }
      const before = this.currentKeyId;
      await this.rotateKeys();
      await this.rotationChain;
      if (this.currentKeyId <= before) {
        throw new Error('E2EE epoch catch-up stalled, rejoin required');
      }
    }
  }

  /**
   * Encrypt a media frame, dispatching by the LOCAL send codec (spec §6.1):
   * AV1 → per-OBU payload encryption; H.264 → NAL-aware encrypted suffix;
   * VP9/VP8/Opus → whole-frame v6. Unknown codecs fail closed.
   * Throws on failure — the caller drops the frame.
   */
  async encryptFrame(
    frame: RTCEncodedAudioFrame | RTCEncodedVideoFrame,
    codec?: CodecFamily
  ): Promise<void> {
    if (!codec || !Object.hasOwn(FRAME_SCHEMES, codec))
      throw new Error('E2EE: unsupported media codec');
    if (codec === 'av1') {
      await this.encryptAv1PerObu(frame);
      return;
    }
    if (codec === 'h264') {
      await this.encryptH264AccessUnit(frame);
      return;
    }
    await this.encryptWholeFrame(frame, codec);
  }

  /** Whole-frame v6 encrypt for VP9/VP8/Opus. */
  private async encryptWholeFrame(
    frame: RTCEncodedAudioFrame | RTCEncodedVideoFrame,
    codec: 'opus' | 'vp8' | 'vp9'
  ): Promise<void> {
    if (!this.encryptKey) throw new Error('E2EE: no encrypt key');
    const encryptKey = this.encryptKey;
    const keyId = this.currentKeyId;
    const keyVersion = this.currentKeyVersion;
    const data = new Uint8Array(frame.data);

    // #1742 root cause: an empty (0-byte) DTX frame would otherwise encrypt to
    // a small fixed blob (0-byte header + 16-byte GCM tag over an empty payload
    // + trailer) that falls just below the decrypt side's "too small to be
    // encrypted" guard. Such a frame would be misclassified as unencrypted and
    // fed to the Opus decoder UNDECIPHERED — the receiver-side garble-during-silence.
    // An empty frame carries no audio content, so pass it through unchanged.
    // Do NOT instead lower the decrypt threshold.
    if (data.length === 0) return;

    // Keep the same static clear prefix for every frame of this codec.
    const headerBytes = getUnencryptedBytes(codec);

    // Split: header (unencrypted for SFU routing) + payload (encrypted)
    const header = data.slice(0, headerBytes);
    const payload = data.slice(headerBytes);

    // Generate IV: full 96-bit random nonce per GCM operation.
    const iv = new Uint8Array(12);
    crypto.getRandomValues(iv);

    const trailer = new Uint8Array(TRAILER_SIZE_V6);
    const view = new DataView(trailer.buffer);
    trailer[0] = header.length;
    view.setUint16(1, keyId & 0xffff, false);
    view.setUint32(3, keyVersion >>> 0, false);
    trailer.set(iv, 7);
    trailer[19] = MEDIA_FRAME_VERSION;
    trailer[20] = FRAME_SCHEMES[codec];
    trailer[21] = MAGIC_0;
    trailer[22] = MAGIC_1;
    const aad = concatBytes([FRAME_AAD_DOMAIN, trailer, header]);
    const ciphertext = await crypto.subtle.encrypt(
      { name: 'AES-GCM', iv, additionalData: aad },
      encryptKey,
      payload
    );
    frame.data = concatBytes([header, new Uint8Array(ciphertext), trailer]).buffer;
  }

  /** Encrypt one complete H.264 Annex-B access-unit suffix with one AES-GCM operation. */
  private async encryptH264AccessUnit(
    frame: RTCEncodedVideoFrame | RTCEncodedAudioFrame
  ): Promise<void> {
    if (!this.encryptKey) throw new Error('E2EE: no encrypt key');
    const encryptKey = this.encryptKey;
    const keyId = this.currentKeyId;
    const keyVersion = this.currentKeyVersion;
    const data: Uint8Array<ArrayBuffer> = new Uint8Array(frame.data);
    const { clearEnd } = locateH264AccessUnitBoundary(data);
    const clearPrefix = data.slice(0, clearEnd);
    const suffix = data.slice(clearEnd);
    const iv = new Uint8Array(12);
    crypto.getRandomValues(iv);
    const mini = encodeMediaFrameMiniHeader({ iv, keyId, keyVersion, scheme: FRAME_SCHEMES.h264 });
    const aad = concatBytes([FRAME_AAD_DOMAIN, mini, uint32be(clearPrefix.length), clearPrefix]);
    const ciphertext = new Uint8Array(
      await crypto.subtle.encrypt({ name: 'AES-GCM', iv, additionalData: aad }, encryptKey, suffix)
    );
    const region = concatBytes([mini, ciphertext]);
    const stuffed = stuffH264Bytes(region, trailingZeroRun(clearPrefix));
    if (stuffed === null) throw new Error('E2EE: H.264 stuffing failed');
    frame.data = concatBytes([clearPrefix, stuffed]).buffer;
  }

  /**
   * Recover the byte offset of an OBU's header (the first header byte) from the
   * parser's `payloadOffset`. The parser emits OBUs in contiguous bitstream
   * order with `nextStart = payloadOffset + payloadLen`, so the i-th OBU's start
   * is the (i-1)-th OBU's end, and the first starts at 0. This recovers each
   * OBU's `[header(+ext)+size]` span without trusting any sender-stamped offset
   * (spec §9.10): the boundaries come purely from the wire leb128 sizes that the
   * hardened parser already validated.
   */
  private static obuStarts(obus: ParsedObu[]): number[] {
    const starts: number[] = [];
    let next = 0;
    for (const o of obus) {
      starts.push(next);
      next = o.payloadOffset + o.payloadLen;
    }
    return starts;
  }

  private static av1Entry(
    data: Uint8Array,
    obu: ParsedObu,
    start: number,
    index: number
  ): Av1CanonicalEntry | null {
    if (obu.obuType === AV1_OBU_TEMPORAL_DELIMITER) {
      if (
        index === 0 &&
        obu.payloadLen === 0 &&
        obu.payloadOffset === 2 &&
        data[0] === 0x12 &&
        data[1] === 0x00
      )
        return null;
      throw new Error('E2EE: AV1 unexpected temporal delimiter');
    }
    const encrypted = obu.obuType === AV1_OBU_FRAME || obu.obuType === AV1_OBU_TILE_GROUP;
    if (
      !encrypted &&
      ![AV1_OBU_SEQUENCE_HEADER, AV1_OBU_FRAME_HEADER, AV1_OBU_PADDING].includes(obu.obuType)
    )
      throw new Error('E2EE: AV1 disallowed OBU type');
    const first = data[start];
    if ((first & 1) !== 0) throw new Error('E2EE: AV1 reserved header bit set');
    if ((first & 2) === 0) throw new Error('E2EE: AV1 OBU size field required');
    const extension = (first & 4) !== 0 ? data[start + 1] : undefined;
    if (extension !== undefined && (extension & 7) !== 0) {
      throw new Error('E2EE: AV1 reserved extension bits set');
    }
    const head =
      extension === undefined
        ? new Uint8Array([first | 2])
        : new Uint8Array([first | 2, extension]);
    const payload = data.subarray(obu.payloadOffset, obu.payloadOffset + obu.payloadLen);
    return { type: obu.obuType, head, payload, plainLength: payload.length };
  }

  private static av1Entries(data: Uint8Array): Av1CanonicalEntry[] {
    const obus = parseAv1Obus(data);
    if (obus === null) throw new Error('E2EE: AV1 parse failed — dropping frame');
    const starts = MediaEncryption.obuStarts(obus);
    const entries: Av1CanonicalEntry[] = [];
    for (let i = 0; i < obus.length; i++) {
      const entry = MediaEncryption.av1Entry(data, obus[i], starts[i], i);
      if (entry) entries.push(entry);
    }
    if (
      !entries.some((entry) => entry.type === AV1_OBU_FRAME || entry.type === AV1_OBU_TILE_GROUP)
    ) {
      throw new Error('E2EE: AV1 frame has no encrypted tile data');
    }
    return entries;
  }

  /**
   * AV1 per-OBU payload encryption (spec §4.1). Walks the OBU list and replaces
   * the payload of each OBU_FRAME(6) / OBU_TILE_GROUP(4) with
   *   [mini-header 22B][AES-256-GCM(payload)][tag 16B]
   * re-encoding the cleartext leb128 size to cover the new payload length. All
   * clear allow-listed OBUs are authenticated through the shared manifest.
   *
   * IV uniqueness: every encrypted OBU receives a fresh full 96-bit CSPRNG IV.
   */
  private async encryptAv1PerObu(
    frame: RTCEncodedVideoFrame | RTCEncodedAudioFrame
  ): Promise<void> {
    if (!this.encryptKey) throw new Error('E2EE: no encrypt key');
    const encryptKey = this.encryptKey;
    const keyId = this.currentKeyId;
    const keyVersion = this.currentKeyVersion;
    const data: Uint8Array<ArrayBuffer> = new Uint8Array(frame.data);
    if (data.length === 0) throw new Error('E2EE: empty AV1 frame');

    const entries = MediaEncryption.av1Entries(data);
    for (const entry of entries) {
      if (entry.type !== AV1_OBU_FRAME && entry.type !== AV1_OBU_TILE_GROUP) continue;
      const iv: Uint8Array<ArrayBuffer> = new Uint8Array(buildObuIv());
      entry.iv = iv;
      entry.mini = encodeMediaFrameMiniHeader({ iv, keyId, keyVersion, scheme: FRAME_SCHEMES.av1 });
    }
    const manifest = av1Manifest(entries);
    // Hash the complete canonical clear structure once. Every protected OBU
    // binds this digest, avoiding O(tile count × manifest size) GCM AAD work.
    const manifestDigest = new Uint8Array(await crypto.subtle.digest('SHA-256', manifest));
    const parts: Uint8Array[] = [];
    for (let i = 0; i < entries.length; i++) {
      const entry = entries[i];
      let payload = entry.payload;
      if (entry.mini && entry.iv) {
        const iv: Uint8Array<ArrayBuffer> = new Uint8Array(entry.iv);
        const ciphertext = new Uint8Array(
          await crypto.subtle.encrypt(
            { name: 'AES-GCM', iv, additionalData: av1Aad(manifest.length, manifestDigest, i) },
            encryptKey,
            new Uint8Array(payload)
          )
        );
        payload = concatBytes([entry.mini, ciphertext]);
      }
      parts.push(entry.head, encodeLeb128(payload.length), payload);
    }
    frame.data = concatBytes(parts).buffer;
  }

  private async requireDecryptKey(
    senderUserId: string,
    keyVersion: number,
    keyId: number
  ): Promise<{ key: CryptoKey; derived: boolean }> {
    const id = MediaEncryption.mapKey(senderUserId, keyVersion, keyId);
    const entry = this.decryptKeys.get(id);
    if (entry && entry.expires > Date.now()) return { key: entry.key, derived: false };
    if (entry) this.decryptKeys.delete(id);
    const derived = await this.deriveRatchetedKey(senderUserId, keyVersion, keyId);
    if (derived) return { key: derived, derived: true };

    let availableCount = 0;
    for (const id of this.decryptKeys.keys()) {
      if (id.startsWith(`${senderUserId}:`)) availableCount++;
    }
    throw new FrameKeyMissError(
      senderUserId,
      keyVersion,
      keyId,
      `E2EE: no decrypt key for sender=${senderUserId} v=${keyVersion} keyId=${keyId} (${availableCount} keys held for sender)`
    );
  }

  /** Authenticate the complete canonical OBU list before emitting any decoder bytes. */
  private async decryptAv1PerObu(
    frame: RTCEncodedVideoFrame | RTCEncodedAudioFrame,
    senderUserId: string
  ): Promise<void> {
    const data: Uint8Array<ArrayBuffer> = new Uint8Array(frame.data);
    const entries = MediaEncryption.av1Entries(data);
    const { keyVersion, keyId } = MediaEncryption.prepareAv1DecryptEntries(entries);
    const manifest = av1Manifest(entries);
    const manifestDigest = new Uint8Array(await crypto.subtle.digest('SHA-256', manifest));
    const candidate = await this.requireDecryptKey(senderUserId, keyVersion, keyId);
    const parts: Uint8Array[] = [];
    for (let i = 0; i < entries.length; i++) {
      const entry = entries[i];
      let payload = entry.payload;
      if (entry.mini && entry.iv) {
        const iv: Uint8Array<ArrayBuffer> = new Uint8Array(entry.iv);
        payload = new Uint8Array(
          await crypto.subtle.decrypt(
            { name: 'AES-GCM', iv, additionalData: av1Aad(manifest.length, manifestDigest, i) },
            candidate.key,
            new Uint8Array(entry.payload.subarray(MEDIA_FRAME_MINI_HEADER_SIZE))
          )
        );
      }
      parts.push(entry.head, encodeLeb128(payload.length), payload);
    }
    if (candidate.derived) this.rememberDecryptKey(senderUserId, keyVersion, keyId, candidate.key);
    frame.data = concatBytes(parts).buffer;
  }

  private static prepareAv1DecryptEntries(entries: Av1CanonicalEntry[]): {
    keyVersion: number;
    keyId: number;
  } {
    let keyVersion: number | undefined;
    let keyId: number | undefined;
    const seenIvs = new Set<string>();
    for (const entry of entries) {
      if (entry.type !== AV1_OBU_FRAME && entry.type !== AV1_OBU_TILE_GROUP) continue;
      const mini = MediaEncryption.inspectAv1Ciphertext(entry);
      if (keyVersion !== undefined && (mini.keyVersion !== keyVersion || mini.keyId !== keyId)) {
        throw new Error('E2EE: AV1 tile-data key mismatch');
      }
      const ivKey = Array.from(mini.iv).join(',');
      if (seenIvs.has(ivKey)) throw new Error('E2EE: AV1 repeated IV');
      seenIvs.add(ivKey);
      keyVersion = mini.keyVersion;
      keyId = mini.keyId;
      entry.mini = entry.payload.subarray(0, MEDIA_FRAME_MINI_HEADER_SIZE);
      entry.iv = mini.iv;
      entry.plainLength = entry.payload.length - MEDIA_FRAME_MINI_HEADER_SIZE - 16;
    }
    if (keyVersion === undefined || keyId === undefined)
      throw new Error('E2EE: AV1 frame has no encrypted tile data');
    return { keyVersion, keyId };
  }

  private static inspectAv1Ciphertext(entry: Av1CanonicalEntry) {
    const mini = decodeMediaFrameMiniHeader(entry.payload);
    if (!mini) throw new Error('E2EE: AV1 tile-data OBU missing v6 mini-header');
    if (mini.scheme !== FRAME_SCHEMES.av1) throw new Error('E2EE: media codec scheme mismatch');
    if (entry.payload.length < MEDIA_FRAME_MINI_HEADER_SIZE + 16) {
      throw new Error('E2EE: AV1 OBU ciphertext too small');
    }
    return mini;
  }

  /**
   * Self-healing key derivation: when we receive a frame with a keyId we don't
   * have, ratchet forward from the nearest known key for that sender.
   * This handles the race where sender rotated keys before we learned the new epoch.
   * Max 10 ratchets to prevent abuse.
   */
  private async deriveRatchetedKey(
    senderUserId: string,
    keyVersion: number,
    targetKeyId: number
  ): Promise<CryptoKey | null> {
    let bestKeyId = -1;
    let bestEntry: { key: CryptoKey; expires: number } | null = null;

    // #1878: parse the 3-part key (senderId:keyVersion:keyId) by field, never
    // positional index — and ONLY ratchet entries of the SAME keyVersion. A
    // v(M) key must never ratchet toward a v(N) target.
    for (const [id, entry] of this.decryptKeys) {
      const parts = id.split(':');
      if (parts.length !== 3) continue; // sender UUIDs have no colons
      const [sid, ver, kid] = parts;
      if (sid !== senderUserId || entry.expires <= Date.now()) continue;
      if (Number(ver) !== keyVersion) continue;
      const kidNum = Number.parseInt(kid, 10);
      if (kidNum < targetKeyId && kidNum > bestKeyId) {
        bestKeyId = kidNum;
        bestEntry = entry;
      }
    }

    if (!bestEntry || bestKeyId < 0) return null;

    const steps = targetKeyId - bestKeyId;
    if (steps > 10) return null;
    this.reserveRatchetWork(senderUserId, keyVersion, targetKeyId, steps);

    let currentKey = bestEntry.key;
    for (let i = bestKeyId; i < targetKeyId; i++) {
      currentKey = await ratchetKey(currentKey);
    }
    return currentKey;
  }

  /** Bound HKDF work triggered by frame-controlled key IDs before tag verification. */
  private reserveRatchetWork(
    senderUserId: string,
    keyVersion: number,
    keyId: number,
    steps: number
  ): void {
    const now = Date.now();
    const id = MediaEncryption.mapKey(senderUserId, keyVersion, keyId);
    const lastAttempt = this.ratchetKeyAttempts.get(id);
    if (lastAttempt !== undefined && now - lastAttempt < RATCHET_KEY_BACKOFF_MS) {
      throw new FrameKeyMissError(
        senderUserId,
        keyVersion,
        keyId,
        'E2EE: ratchet work throttled; request exact decrypt key'
      );
    }
    const previousSender = this.ratchetWorkBySender.get(senderUserId);
    const senderSteps =
      previousSender && now - previousSender.windowStart < RATCHET_WORK_WINDOW_MS
        ? previousSender.steps
        : 0;
    const globalSteps =
      now - this.ratchetWorkGlobal.windowStart < RATCHET_WORK_WINDOW_MS
        ? this.ratchetWorkGlobal.steps
        : 0;
    if (
      senderSteps + steps > MAX_RATCHET_STEPS_PER_SENDER ||
      globalSteps + steps > MAX_RATCHET_STEPS_GLOBAL
    ) {
      throw new FrameKeyMissError(
        senderUserId,
        keyVersion,
        keyId,
        'E2EE: ratchet work budget exhausted; request exact decrypt key'
      );
    }
    this.ratchetWorkBySender.delete(senderUserId);
    this.ratchetWorkBySender.set(senderUserId, {
      windowStart: senderSteps ? (previousSender?.windowStart ?? now) : now,
      steps: senderSteps + steps,
    });
    this.ratchetWorkGlobal = {
      windowStart: globalSteps ? this.ratchetWorkGlobal.windowStart : now,
      steps: globalSteps + steps,
    };
    this.ratchetKeyAttempts.delete(id);
    this.ratchetKeyAttempts.set(id, now);
    while (this.ratchetWorkBySender.size > MAX_RATCHET_WORK_TRACKED) {
      const oldest = this.ratchetWorkBySender.keys().next().value;
      if (oldest !== undefined) this.ratchetWorkBySender.delete(oldest);
    }
    while (this.ratchetKeyAttempts.size > MAX_RATCHET_KEY_ATTEMPTS_TRACKED) {
      const oldest = this.ratchetKeyAttempts.keys().next().value;
      if (oldest !== undefined) this.ratchetKeyAttempts.delete(oldest);
    }
  }

  /**
   * Decrypt a media frame, dispatching by the RECEIVED codec (spec §6.2):
   * AV1 → per-OBU payload decryption; H.264 → NAL-aware encrypted suffix;
   * VP9/VP8/Opus → whole-frame v6. Unknown codecs fail closed.
   * Returns normally if decryption succeeds or the frame is empty (DTX).
   * Throws for any non-empty frame that fails — caller should DROP the frame.
   */
  async decryptFrame(
    frame: RTCEncodedAudioFrame | RTCEncodedVideoFrame,
    senderUserId: string,
    codec?: CodecFamily
  ): Promise<void> {
    if (!codec || !Object.hasOwn(FRAME_SCHEMES, codec))
      throw new Error('E2EE: unsupported media codec');
    if (codec === 'av1') {
      await this.decryptAv1PerObu(frame, senderUserId);
      return;
    }
    if (codec === 'h264') {
      await this.decryptH264AccessUnit(frame, senderUserId);
      return;
    }
    await this.decryptWholeFrame(frame, senderUserId, codec);
  }

  /** Decrypt the stuffed terminal H.264 region and restore the original access-unit suffix. */
  private async decryptH264AccessUnit(
    frame: RTCEncodedVideoFrame | RTCEncodedAudioFrame,
    senderUserId: string
  ): Promise<void> {
    const data: Uint8Array<ArrayBuffer> = new Uint8Array(frame.data);
    const { clearEnd, firstVclIsTerminal } = locateH264AccessUnitBoundary(data);
    if (!firstVclIsTerminal) {
      throw new Error('E2EE: H.264 access unit missing media mini-header');
    }
    const clearPrefix = data.slice(0, clearEnd);
    const region = unstuffH264Bytes(data.subarray(clearEnd), trailingZeroRun(clearPrefix));
    if (region === null) throw new Error('E2EE: malformed H.264 stuffing');
    const mini = decodeMediaFrameMiniHeader(region);
    if (mini === null) throw new Error('E2EE: H.264 access unit missing media mini-header');
    if (mini.scheme !== FRAME_SCHEMES.h264) throw new Error('E2EE: media codec scheme mismatch');
    const ciphertext: Uint8Array<ArrayBuffer> = new Uint8Array(
      region.subarray(MEDIA_FRAME_MINI_HEADER_SIZE)
    );
    if (ciphertext.byteLength < 16) {
      throw new Error('E2EE: H.264 ciphertext too small');
    }

    const candidate = await this.requireDecryptKey(senderUserId, mini.keyVersion, mini.keyId);
    const iv: Uint8Array<ArrayBuffer> = new Uint8Array(mini.iv);
    const aad = concatBytes([
      FRAME_AAD_DOMAIN,
      region.subarray(0, MEDIA_FRAME_MINI_HEADER_SIZE),
      uint32be(clearPrefix.length),
      clearPrefix,
    ]);
    const suffix = new Uint8Array(
      await crypto.subtle.decrypt(
        { name: 'AES-GCM', iv, additionalData: aad },
        candidate.key,
        ciphertext
      )
    );
    if (candidate.derived)
      this.rememberDecryptKey(senderUserId, mini.keyVersion, mini.keyId, candidate.key);
    frame.data = concatBytes([clearPrefix, suffix]).buffer;
  }

  /**
   * Whole-frame v6 decrypt for VP9/VP8/Opus.
   *
   * Returns normally if decryption succeeds or the frame is empty (e.g. DTX).
   * Throws for any non-empty frame without the E2EE magic trailer or any frame
   * that carries the trailer but cannot be decrypted. The caller should DROP
   * those frames to avoid feeding plaintext-policy violations or ciphertext
   * into the audio/video decoder.
   */
  private async decryptWholeFrame(
    frame: RTCEncodedAudioFrame | RTCEncodedVideoFrame,
    senderUserId: string,
    codec: 'opus' | 'vp8' | 'vp9'
  ): Promise<void> {
    const data = new Uint8Array(frame.data);

    // Empty frames can appear for DTX/control cases and carry no plaintext media.
    if (data.length === 0) {
      return;
    }

    const hasMagic = data.length >= 2 && data.at(-1) === MAGIC_1 && data.at(-2) === MAGIC_0;
    if (!hasMagic) {
      throw new Error('E2EE: unencrypted media frame received');
    }

    // Minimum: 1 header + 16 GCM tag + 23 v6 trailer.
    if (data.length < TRAILER_SIZE_V6 + MIN_GCM_OVERHEAD) {
      throw new Error('E2EE: malformed encrypted frame too small');
    }

    const n = data.length;
    const trailer = data.slice(n - TRAILER_SIZE_V6);
    if (trailer[19] !== MEDIA_FRAME_VERSION) {
      throw new Error('E2EE: unexpected frame crypto version');
    }
    if (trailer[20] !== FRAME_SCHEMES[codec]) throw new Error('E2EE: media codec scheme mismatch');

    const view = new DataView(trailer.buffer);
    const iv = trailer.slice(7, 19);
    const keyVersion = view.getUint32(3, false);
    const keyId = view.getUint16(1, false);
    const headerBytes = trailer[0];

    // The sender permits only one or two clear bytes for these codecs.
    if (headerBytes < 1 || headerBytes > 2 || headerBytes >= data.length - TRAILER_SIZE_V6) {
      throw new Error('E2EE: malformed encrypted frame trailer');
    }

    const header = data.slice(0, headerBytes);
    const ciphertext = data.slice(headerBytes, data.length - TRAILER_SIZE_V6);

    // AES-GCM ciphertext must be at least 16 bytes (the auth tag alone)
    if (ciphertext.byteLength < 16) {
      throw new Error('E2EE: malformed encrypted frame ciphertext');
    }

    const candidate = await this.requireDecryptKey(senderUserId, keyVersion, keyId);
    const aad = concatBytes([FRAME_AAD_DOMAIN, trailer, header]);
    const plaintext = await crypto.subtle.decrypt(
      { name: 'AES-GCM', iv, additionalData: aad },
      candidate.key,
      ciphertext
    );
    if (candidate.derived) this.rememberDecryptKey(senderUserId, keyVersion, keyId, candidate.key);

    // Reconstruct: [header][plaintext]
    const result = new Uint8Array(header.length + plaintext.byteLength);
    result.set(header);
    result.set(new Uint8Array(plaintext), header.length);
    frame.data = result.buffer;
  }

  /** Clean up */
  destroy(): void {
    this.destroyed = true;
    if (this.rotationTimer) {
      clearTimeout(this.rotationTimer);
      this.rotationTimer = null;
    }
    this.rotationPending = false;
    this.rotationDeadline = 0;
    this.rotationChain = Promise.resolve();
    this.encryptKey = null;
    this.decryptKeys.clear();
    this.ratchetWorkBySender.clear();
    this.ratchetKeyAttempts.clear();
    this.ratchetWorkGlobal = { windowStart: 0, steps: 0 };
  }
}
