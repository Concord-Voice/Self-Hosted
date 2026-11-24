import { encodeNonmediaContext, type NonmediaContext } from './nonmediaContext';
import { decryptNonmediaAesGcm } from './crypto';

// Stored format is chosen by trusted provenance at a future call site. This
// dormant reader deliberately has no legacy trial-decryption path.
export type NonmediaTextFormat = 'm1' | 'b1' | 'ek1' | 'rw1-private' | 'rw1-preferences';
type ContextTag<F extends NonmediaTextFormat> = F extends 'm1'
  ? 'message-v1'
  : F extends 'b1'
    ? 'blob-v1'
    : F extends 'ek1'
      ? 'account-ek-wrap-v1'
      : F extends 'rw1-private'
        ? 'recovery-wrap-v1'
        : 'recovery-prefs-wrap-v1';
type Expected<F extends NonmediaTextFormat> = Extract<NonmediaContext, { tag: ContextTag<F> }>;

const tagByFormat: Record<NonmediaTextFormat, NonmediaContext['tag']> = {
  m1: 'message-v1',
  b1: 'blob-v1',
  ek1: 'account-ek-wrap-v1',
  'rw1-private': 'recovery-wrap-v1',
  'rw1-preferences': 'recovery-prefs-wrap-v1',
};

const INVALID_ENVELOPE = 'Invalid non-media envelope';

function decodePayload(text: string): Uint8Array {
  if (!/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(text))
    throw new Error(INVALID_ENVELOPE);
  try {
    const binary = atob(text);
    if (btoa(binary) !== text) throw new Error('noncanonical');
    return Uint8Array.from(binary, (char) => {
      const codePoint = char.codePointAt(0);
      if (codePoint === undefined) throw new Error(INVALID_ENVELOPE);
      return codePoint;
    });
  } catch {
    throw new Error(INVALID_ENVELOPE);
  }
}

export async function openNonmediaTextEnvelope<F extends NonmediaTextFormat>(
  storedFormat: F,
  envelope: string,
  key: CryptoKey,
  expected: Expected<F>
): Promise<Uint8Array> {
  const tag = tagByFormat[storedFormat];
  if (!tag || expected?.tag !== tag || typeof envelope !== 'string')
    throw new Error(INVALID_ENVELOPE);
  // Validate and snapshot the caller's context before the first await.
  const aad = encodeNonmediaContext(expected);
  const prefix = storedFormat.startsWith('rw1-') ? 'rw1:' : `${storedFormat}:`;
  if (!envelope.startsWith(prefix)) throw new Error(INVALID_ENVELOPE);
  const payload = decodePayload(envelope.slice(prefix.length));
  if (
    payload.length < 28 ||
    key.algorithm.name !== 'AES-GCM' ||
    (key.algorithm as AesKeyAlgorithm).length !== 256 ||
    !key.usages.includes('decrypt')
  )
    throw new Error(INVALID_ENVELOPE);
  try {
    const plaintext = await decryptNonmediaAesGcm(key, payload, aad);
    return new Uint8Array(plaintext);
  } catch {
    throw new Error('Invalid non-media ciphertext');
  }
}

export function channelKeyCapsuleLabel(
  context: Extract<NonmediaContext, { tag: 'channel-key-capsule-v1' }>
): Uint8Array {
  return encodeNonmediaContext(context);
}
