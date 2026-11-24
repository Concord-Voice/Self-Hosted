// ADR-0051's closed, positional context grammar. This module does not select
// context from ciphertext or server metadata; callers supply trusted values.
type Context<Tag extends string, Fields extends readonly unknown[]> = Readonly<{
  tag: Tag;
  fields: Fields;
}>;

type Scope = 'channel' | 'dm';
type BlobPurpose = 'preferences' | 'saved_gifs' | 'friend_organization' | 'presence_overrides';
type AccountKeyPurpose = 'account-rsa-oaep4096-pss-sha256';
type RecoveryPurpose = 'user-account-recovery-key';

export type NonmediaContext =
  | Context<'message-v1', readonly [string, Scope, string, string, string, number, 'message-body']>
  | Context<'blob-v1', readonly [string, string, BlobPurpose, 1]>
  | Context<'account-ek-wrap-v1', readonly [string, string, AccountKeyPurpose, number, number]>
  | Context<
      'recovery-wrap-v1',
      readonly [string, string, AccountKeyPurpose, RecoveryPurpose, number, number]
    >
  | Context<
      'recovery-prefs-wrap-v1',
      readonly [string, string, 'preferences-aes256-gcm', RecoveryPurpose, number, number]
    >
  | Context<
      'attachment-v4',
      readonly [string, Scope, string, string, string, number, string, 0, number, number, 8388608]
    >
  | Context<
      'channel-key-capsule-v1',
      readonly [string, Scope, string, number, string, number, string, string]
    >;

const INVALID_CONTEXT = 'Invalid non-media context';
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const isUuid = (value: unknown): value is string => typeof value === 'string' && uuid.test(value);
const isScope = (value: unknown): value is Scope => value === 'channel' || value === 'dm';
const positive = (value: unknown): value is number =>
  Number.isSafeInteger(value) && Number(value) >= 1;
const boundedBase64 = (value: unknown, length: number): boolean => {
  if (
    typeof value !== 'string' ||
    value.length !== Math.ceil(length / 3) * 4 ||
    !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(value)
  )
    return false;
  try {
    const bytes = atob(value);
    return bytes.length === length && btoa(bytes) === value;
  } catch {
    return false;
  }
};

function validFields(tag: string, f: readonly unknown[]): boolean {
  switch (tag) {
    case 'message-v1':
      return (
        f.length === 7 &&
        isUuid(f[0]) &&
        isScope(f[1]) &&
        isUuid(f[2]) &&
        isUuid(f[3]) &&
        isUuid(f[4]) &&
        positive(f[5]) &&
        f[6] === 'message-body'
      );
    case 'blob-v1':
      return (
        f.length === 4 &&
        isUuid(f[0]) &&
        isUuid(f[1]) &&
        (f[2] === 'preferences' ||
          f[2] === 'saved_gifs' ||
          f[2] === 'friend_organization' ||
          f[2] === 'presence_overrides') &&
        f[3] === 1
      );
    case 'account-ek-wrap-v1':
      return (
        f.length === 5 &&
        isUuid(f[0]) &&
        isUuid(f[1]) &&
        f[2] === 'account-rsa-oaep4096-pss-sha256' &&
        positive(f[3]) &&
        positive(f[4])
      );
    case 'recovery-wrap-v1':
      return (
        f.length === 6 &&
        isUuid(f[0]) &&
        isUuid(f[1]) &&
        f[2] === 'account-rsa-oaep4096-pss-sha256' &&
        f[3] === 'user-account-recovery-key' &&
        positive(f[4]) &&
        positive(f[5])
      );
    case 'recovery-prefs-wrap-v1':
      return (
        f.length === 6 &&
        isUuid(f[0]) &&
        isUuid(f[1]) &&
        f[2] === 'preferences-aes256-gcm' &&
        f[3] === 'user-account-recovery-key' &&
        positive(f[4]) &&
        positive(f[5])
      );
    case 'attachment-v4':
      return (
        f.length === 11 &&
        isUuid(f[0]) &&
        isScope(f[1]) &&
        isUuid(f[2]) &&
        isUuid(f[3]) &&
        isUuid(f[4]) &&
        positive(f[5]) &&
        boundedBase64(f[6], 16) &&
        f[7] === 0 &&
        Number.isSafeInteger(f[9]) &&
        Number(f[9]) >= 1 &&
        Number(f[9]) <= 10000 &&
        Number.isSafeInteger(f[8]) &&
        Number(f[8]) >= 0 &&
        Number(f[8]) < Number(f[9]) &&
        f[10] === 8388608
      );
    case 'channel-key-capsule-v1':
      return (
        f.length === 8 &&
        isUuid(f[0]) &&
        isScope(f[1]) &&
        isUuid(f[2]) &&
        positive(f[3]) &&
        isUuid(f[4]) &&
        positive(f[5]) &&
        isUuid(f[6]) &&
        boundedBase64(f[7], 48)
      );
    default:
      return false;
  }
}

export function encodeNonmediaContext(context: NonmediaContext): Uint8Array {
  if (!context || !validFields(context.tag, context.fields)) throw new Error(INVALID_CONTEXT);
  const bytes = new TextEncoder().encode(
    JSON.stringify(['concord-e2ee-context', 1, context.tag, ...context.fields])
  );
  if (bytes.length > 4096) throw new Error(INVALID_CONTEXT);
  return bytes;
}

export function parseNonmediaContext(bytes: Uint8Array): NonmediaContext {
  if (!(bytes instanceof Uint8Array) || bytes.length > 4096) throw new Error(INVALID_CONTEXT);
  try {
    const raw = new TextDecoder('utf-8', { fatal: true }).decode(bytes);
    const parsed: unknown = JSON.parse(raw);
    if (
      !Array.isArray(parsed) ||
      parsed[0] !== 'concord-e2ee-context' ||
      parsed[1] !== 1 ||
      typeof parsed[2] !== 'string'
    )
      throw new Error('shape');
    const context = { tag: parsed[2], fields: parsed.slice(3) } as unknown as NonmediaContext;
    const canonical = encodeNonmediaContext(context);
    if (
      canonical.length !== bytes.length ||
      canonical.some((value, index) => value !== bytes[index])
    )
      throw new Error('canonical');
    return context;
  } catch {
    throw new Error(INVALID_CONTEXT);
  }
}
