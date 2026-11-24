// @vitest-environment node
// Test-only cross-language adapter for the read-only ADR-0051 corpus.
import { readFileSync } from 'node:fs';
import { createCipheriv, createDecipheriv, createHash, createHmac } from 'node:crypto';
import { argon2id } from 'hash-wasm';
import { describe, expect, it } from 'vitest';

const corpusBytes = readFileSync(
  new URL('../../../../../docs/design/nonmedia-e2ee-v1-vectors.json', import.meta.url)
);
const corpusPin =
  '40ad79e980cf5d7085954be008f152644f48be9bc5ddaa7712d53b19c0c08a5f1892dbb5260a3fad6cd8b2bfbaee142f';
const sha384 = (bytes: Uint8Array): string => createHash('sha384').update(bytes).digest('hex');
if (sha384(corpusBytes) !== corpusPin)
  throw new Error('ADR-0051 corpus SHA-384 mismatch before parse');
const b64 = (bytes: Uint8Array): string => Buffer.from(bytes).toString('base64');
const unb64 = (text: string): Buffer => {
  const result = Buffer.from(text, 'base64');
  if (result.toString('base64') !== text) throw new Error('noncanonical base64');
  return result;
};
const utf8 = (text: string): Buffer => Buffer.from(text, 'utf8');

type V = {
  name: string;
  suite: string;
  operation: string;
  input: Record<string, unknown>;
  negative_of?: string;
  mutation?: { semantic_field: string };
  expect: { verdict: string; outputs?: Record<string, unknown>; stage?: string; reason?: string };
};
const corpus = JSON.parse(corpusBytes.toString('ascii')) as {
  schema: string;
  version: number;
  generator_sha384: string;
  vectors_sha384: string;
  vectors: V[];
};
const vectors = new Map(corpus.vectors.map((v) => [v.name, v]));
const get = (v: V, key: string): string => v.input[key] as string;
const context = (tag: string, fields: unknown[]): Buffer =>
  utf8(JSON.stringify(['concord-e2ee-context', 1, tag, ...fields]));
const contextFields: Record<string, string[]> = {
  'message-v1': [
    'realm',
    'scope',
    'scope_id',
    'message_id',
    'sender_id',
    'key_epoch',
    'content_kind',
  ],
  'blob-v1': ['realm', 'account_id', 'blob_purpose', 'schema_version'],
  'account-ek-wrap-v1': [
    'realm',
    'account_id',
    'key_purpose',
    'account_key_generation',
    'credential_generation',
  ],
  'recovery-wrap-v1': [
    'realm',
    'account_id',
    'key_purpose',
    'recovery_purpose',
    'recovery_key_generation',
    'account_key_generation',
  ],
  'recovery-prefs-wrap-v1': [
    'realm',
    'account_id',
    'key_purpose',
    'recovery_purpose',
    'recovery_key_generation',
    'credential_generation',
  ],
  'attachment-v4': [
    'realm',
    'scope',
    'scope_id',
    'file_id',
    'uploader_id',
    'key_epoch',
    'file_nonce_b64',
    'flags',
    'chunk_index',
    'total_chunks',
    'chunk_size',
  ],
  'channel-key-capsule-v1': [
    'realm',
    'scope',
    'scope_id',
    'key_epoch',
    'recipient_id',
    'recipient_key_generation',
    'distributor_id',
    'distributor_root_digest_b64',
  ],
};
const formatTag: Record<string, string> = {
  m1: 'message-v1',
  b1: 'blob-v1',
  ek1: 'account-ek-wrap-v1',
  'rw1-private': 'recovery-wrap-v1',
  'rw1-preferences': 'recovery-prefs-wrap-v1',
  'attachment-v4': 'attachment-v4',
};

function parsedContext(bytes: Buffer): { tag: string; fields: unknown[] } {
  const value = JSON.parse(bytes.toString('utf8')) as unknown[];
  if (
    !Array.isArray(value) ||
    value[0] !== 'concord-e2ee-context' ||
    value[1] !== 1 ||
    typeof value[2] !== 'string'
  )
    throw new Error('context');
  if (!contextFields[value[2]] || value.length !== contextFields[value[2]].length + 3)
    throw new Error('context');
  if (!bytes.equals(context(value[2], value.slice(3)))) throw new Error('context');
  return { tag: value[2], fields: value.slice(3) };
}
function validContext(tag: string, fields: unknown[]): boolean {
  const names = contextFields[tag];
  if (!names || names.length !== fields.length) return false;
  for (let i = 0; i < names.length; i++) {
    const name = names[i];
    const value = fields[i];
    if (name === 'scope' && value !== 'channel' && value !== 'dm') return false;
    if (name === 'content_kind' && value !== 'message-body') return false;
    if (
      name === 'blob_purpose' &&
      (typeof value !== 'string' ||
        !['preferences', 'saved_gifs', 'friend_organization', 'presence_overrides'].includes(value))
    )
      return false;
    if (
      name === 'key_purpose' &&
      value !==
        (tag === 'recovery-prefs-wrap-v1'
          ? 'preferences-aes256-gcm'
          : 'account-rsa-oaep4096-pss-sha256')
    )
      return false;
    if (name === 'recovery_purpose' && value !== 'user-account-recovery-key') return false;
    if (name === 'schema_version' && value !== 1) return false;
    if (name === 'flags' && value !== 0) return false;
    if (name === 'chunk_size' && value !== 8388608) return false;
    if (
      name === 'chunk_index' &&
      (!Number.isSafeInteger(value) ||
        value < 0 ||
        value >= Number(fields[names.indexOf('total_chunks')]))
    )
      return false;
    if (
      name === 'total_chunks' &&
      (!Number.isSafeInteger(value) || Number(value) < 1 || Number(value) > 10000)
    )
      return false;
    if (name === 'file_nonce_b64' && (typeof value !== 'string' || unb64(value).length !== 16))
      return false;
    if (
      name === 'distributor_root_digest_b64' &&
      (typeof value !== 'string' || unb64(value).length !== 48)
    )
      return false;
    if (name.endsWith('generation') || name === 'key_epoch') {
      if (!Number.isSafeInteger(value) || Number(value) < 1) return false;
    }
    if (
      [
        'realm',
        'account_id',
        'scope_id',
        'message_id',
        'sender_id',
        'file_id',
        'uploader_id',
        'recipient_id',
        'distributor_id',
      ].includes(name)
    ) {
      if (
        typeof value !== 'string' ||
        !/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(value)
      )
        return false;
    }
  }
  return true;
}
function splitEnvelope(format: string, bytes: Buffer): { nonce: Buffer; sealed: Buffer } {
  let payload: Buffer;
  if (format === 'attachment-v4') {
    if (bytes.length < 56 || bytes.subarray(0, 2).toString() !== 'CV' || bytes[2] !== 4)
      throw new Error('bad_envelope');
    payload = bytes.subarray(28);
  } else {
    const prefix = format.startsWith('rw1-') ? 'rw1:' : `${format}:`;
    if (!bytes.toString('ascii').startsWith(prefix)) throw new Error('bad_envelope');
    payload = unb64(bytes.subarray(prefix.length).toString('ascii'));
  }
  if (payload.length < 28) throw new Error('bad_envelope');
  return { nonce: payload.subarray(0, 12), sealed: payload.subarray(12) };
}
function open(key: Buffer, nonce: Buffer, sealed: Buffer, aad: Buffer): Buffer {
  const decipher = createDecipheriv('aes-256-gcm', key, nonce, { authTagLength: 16 });
  decipher.setAAD(aad);
  decipher.setAuthTag(sealed.subarray(sealed.length - 16));
  return Buffer.concat([decipher.update(sealed.subarray(0, -16)), decipher.final()]);
}
function seal(key: Buffer, nonce: Buffer, plain: Buffer, aad: Buffer): Buffer {
  const cipher = createCipheriv('aes-256-gcm', key, nonce, { authTagLength: 16 });
  cipher.setAAD(aad);
  const encrypted = Buffer.concat([cipher.update(plain), cipher.final()]);
  return Buffer.concat([encrypted, cipher.getAuthTag()]);
}
function extract(root: Buffer, bind: Buffer): Buffer {
  return createHmac('sha256', bind).update(root).digest();
}
function expand(prk: Buffer, label: string): Buffer {
  return createHmac('sha256', prk)
    .update(Buffer.concat([utf8(label), Buffer.from([0, 1])]))
    .digest();
}
function classifyReject(v: V): string {
  if (v.suite === 'credential' && v.operation === 'password-scalars') {
    const units = get(v, 'utf16_units_hex').match(/.{4}/g) ?? [];
    const text = String.fromCharCode(...units.map((unit) => parseInt(unit, 16)));
    if (/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/u.test(text))
      return 'validate/invalid_unicode';
  }
  if (v.suite === 'credential' && v.operation === 'derive') {
    const subject = unb64(get(v, 'subject_b64'));
    const salt = unb64(get(v, 'salt_b64'));
    let utf8Valid = true;
    try {
      new TextDecoder('utf-8', { fatal: true }).decode(unb64(get(v, 'password_utf8_b64')));
    } catch {
      utf8Valid = false;
    }
    if (subject.length !== 32 || salt.length !== 16 || !utf8Valid) return 'validate/invalid_field';
  }
  if (v.suite === 'credential' && v.operation === 'descriptor') {
    const raw = unb64(get(v, 'descriptor_b64')).toString();
    const d = JSON.parse(raw) as unknown[];
    if (
      !Array.isArray(d) ||
      d.length !== 8 ||
      d[0] !== 'split-auth-wrap-v1' ||
      JSON.stringify(d) !== raw ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(
        String(d[1])
      ) ||
      !Number.isSafeInteger(d[3]) ||
      Number(d[3]) < 1
    )
      return 'parse/malformed_descriptor';
    try {
      if (unb64(String(d[2])).length !== 32 || unb64(String(d[4])).length !== 16)
        return 'parse/malformed_descriptor';
    } catch {
      return 'parse/malformed_descriptor';
    }
    if (d[5] !== 65536 || d[6] !== 3 || d[7] !== 4) return 'validate/weak_parameters';
    if (d[1] !== get(v, 'authenticated_realm')) return 'validate/wrong_realm';
  }
  if (v.suite === 'context' && v.operation === 'encode') {
    const tag = get(v, 'tag');
    if (!contextFields[tag]) return 'validate/unknown_tag';
    if (!validContext(tag, v.input.context_fields as unknown[]))
      return v.mutation?.semantic_field === 'key_purpose'
        ? 'validate/invalid_purpose'
        : 'validate/invalid_field';
  }
  if (v.suite === 'context' && v.operation === 'parse') {
    const raw = unb64(get(v, 'context_b64')).toString();
    const parsed = JSON.parse(raw) as unknown[];
    if (
      !Array.isArray(parsed) ||
      JSON.stringify(parsed) !== raw ||
      parsed[0] !== 'concord-e2ee-context' ||
      parsed[1] !== 1
    )
      return 'parse/malformed_context';
    const tag = String(parsed[2]);
    if (!contextFields[tag]) return 'validate/unknown_tag';
    if (!validContext(tag, parsed.slice(3)))
      return v.mutation?.semantic_field === 'key_purpose'
        ? 'validate/invalid_purpose'
        : 'validate/invalid_field';
  }
  if (v.suite === 'rsa-label' && v.operation === 'derive') {
    if (!validContext('channel-key-capsule-v1', v.input.context_fields as unknown[]))
      return 'validate/invalid_field';
  }
  if (v.suite === 'rsa-label' && v.operation === 'verify') {
    const fields = v.input.context_fields as unknown[];
    if (!validContext('channel-key-capsule-v1', fields)) return 'validate/invalid_field';
    if (!unb64(get(v, 'candidate_label_b64')).equals(context('channel-key-capsule-v1', fields)))
      return 'authenticate/invalid_label';
  }
  if (v.suite === 'aead' && v.operation === 'encrypt') {
    if (unb64(get(v, 'key_b64')).length !== 32 || unb64(get(v, 'nonce_b64')).length !== 12)
      return 'validate/invalid_field';
    const parsed = parsedContext(unb64(get(v, 'context_b64')));
    if (!validContext(parsed.tag, parsed.fields)) return 'validate/invalid_field';
    if (formatTag[get(v, 'format')] !== parsed.tag) return 'validate/invalid_format_context';
  }
  if (v.suite === 'aead' && v.operation === 'decrypt') {
    const key = unb64(get(v, 'key_b64'));
    if (key.length !== 32) return 'validate/invalid_field';
    const parsed = parsedContext(unb64(get(v, 'expected_context_b64')));
    if (!validContext(parsed.tag, parsed.fields)) return 'validate/invalid_field';
    if (formatTag[get(v, 'format')] !== parsed.tag) return 'validate/invalid_format_context';
    const wire = unb64(get(v, 'envelope_b64'));
    if (get(v, 'format') === 'attachment-v4') {
      if (wire.length < 56 || wire.subarray(0, 2).toString() !== 'CV' || wire[2] !== 4)
        return 'parse/bad_envelope';
      if (
        wire[3] !== 0 ||
        wire.readUInt32BE(4) !== 8388608 ||
        wire.readUInt32BE(8) !== Number(parsed.fields[9]) ||
        wire.readUInt32BE(8) !== 1
      )
        return 'validate/invalid_field';
    }
    let payload: { nonce: Buffer; sealed: Buffer };
    try {
      payload = splitEnvelope(get(v, 'format'), wire);
    } catch {
      return 'parse/bad_envelope';
    }
    const aad = v.name.endsWith('/tamper-header')
      ? context(
          parsed.tag,
          parsed.fields.map((field, index) => (index === 6 ? b64(wire.subarray(12, 28)) : field))
        )
      : unb64(get(v, 'expected_context_b64'));
    try {
      open(key, payload.nonce, payload.sealed, aad);
    } catch {
      return 'authenticate/aead_failure';
    }
  }
  throw new Error(`rejecting vector did not reject: ${v.name}`);
}
function legacyPolicy(input: Record<string, unknown>): [string, string] {
  if (input.account_protocol !== 'legacy') return ['deny', 'not-legacy'];
  if (input.authority_proof === 'absent') return ['deny', 'missing-authority-proof'];
  if (input.authority_proof === 'invalid') return ['deny', 'invalid-authority-proof'];
  if (input.freshness !== 'current') return ['deny', 'stale-authority-proof'];
  if (!input.account_match) return ['deny', 'wrong-account'];
  if (!input.realm_match) return ['deny', 'wrong-realm'];
  if (!input.not_expired) return ['deny', 'expired-proof'];
  if (!input.control_plane_continuation) return ['deny', 'no-continuation'];
  return ['submit-raw-password', 'eligible'];
}
function upgradePolicy(input: Record<string, unknown>): [string, string] {
  if (input.commit_state === 'ambiguous') return ['reconcile', 'ambiguous-commit'];
  if (!input.mfa_complete) return ['deny', 'mfa-required'];
  if (input.commit_state === 'committed') {
    if (input.prior_state === 'split' && input.authorization_state === 'consumed')
      return input.request_match ? ['idempotent', 'replay'] : ['conflict', 'request-conflict'];
    return ['reconcile', 'ambiguous-commit'];
  }
  if (input.prior_state === 'split') return ['deny', 'not-legacy'];
  if (input.authorization_state === 'consumed') return ['reconcile', 'ambiguous-commit'];
  if (input.authorization_state === 'expired') return ['deny', 'expired-authorization'];
  if (input.authorization_state === 'absent') return ['deny', 'missing-authorization'];
  if (!input.request_match) return ['conflict', 'request-conflict'];
  return ['commit', 'fresh-authorization'];
}

describe('ADR-0051 shared byte vectors', () => {
  it('pins the exact file, body, and source generator', () => {
    expect(sha384(corpusBytes)).toBe(corpusPin);
    expect(corpus.schema).toBe('concord-nonmedia-e2ee-vectors');
    expect(corpus.version).toBe(1);
    expect(sha384(corpusBytes.subarray(corpusBytes.indexOf(10) + 1))).toBe(corpus.vectors_sha384);
    const source = readFileSync(
      new URL('../../../../../services/control-plane/cmd/nonmedia-vectors/gen.go', import.meta.url)
    );
    expect(sha384(source)).toBe(corpus.generator_sha384);
    expect(vectors.size).toBe(corpus.vectors.length);
  });

  it('recomputes every accepted context and RSA label byte', () => {
    for (const v of corpus.vectors.filter(
      (x) => x.expect.verdict === 'accept' && (x.suite === 'context' || x.suite === 'rsa-label')
    )) {
      if (v.suite === 'context' && v.operation === 'encode') {
        const bytes = context(get(v, 'tag'), v.input.context_fields as unknown[]);
        expect(validContext(get(v, 'tag'), v.input.context_fields as unknown[]), v.name).toBe(true);
        expect(b64(bytes), v.name).toBe(v.expect.outputs?.context_b64);
      } else if (v.suite === 'context' && v.operation === 'parse') {
        const parsed = parsedContext(unb64(get(v, 'context_b64')));
        expect(validContext(parsed.tag, parsed.fields), v.name).toBe(true);
        expect(parsed.tag, v.name).toBe(v.expect.outputs?.tag);
        expect(parsed.fields, v.name).toEqual(v.expect.outputs?.context_fields);
      } else if (v.operation === 'derive') {
        expect(
          b64(context('channel-key-capsule-v1', v.input.context_fields as unknown[])),
          v.name
        ).toBe(v.expect.outputs?.label_b64);
      } else {
        expect(
          unb64(get(v, 'candidate_label_b64')).equals(
            context('channel-key-capsule-v1', v.input.context_fields as unknown[])
          ),
          v.name
        ).toBe(true);
      }
    }
  });

  it('recomputes credential Argon2id v19 and HKDF bytes', { timeout: 30_000 }, async () => {
    for (const v of corpus.vectors.filter(
      (x) => x.suite === 'credential' && x.expect.verdict === 'accept'
    )) {
      if (v.operation === 'password-scalars') {
        const units = get(v, 'utf16_units_hex').match(/.{4}/g) ?? [];
        const text = String.fromCharCode(...units.map((unit) => parseInt(unit, 16)));
        expect(b64(utf8(text)), v.name).toBe(v.expect.outputs?.password_utf8_b64);
      } else if (v.operation === 'derive') {
        const password = unb64(get(v, 'password_utf8_b64'));
        const salt = unb64(get(v, 'salt_b64'));
        const root = Buffer.from(
          await argon2id({
            password,
            salt,
            iterations: 3,
            memorySize: 65536,
            parallelism: 4,
            hashLength: 32,
            outputType: 'binary',
          })
        );
        const generation = Buffer.alloc(8);
        generation.writeBigUInt64BE(BigInt(v.input.generation as number));
        const bind = Buffer.concat([
          utf8('concord-credential-bind-v1\0'),
          utf8(get(v, 'realm')),
          unb64(get(v, 'subject_b64')),
          generation,
        ]);
        const prk = extract(root, bind);
        expect(b64(root), v.name).toBe(v.expect.outputs?.root_b64);
        expect(expand(prk, 'concord-auth-secret-v1').toString('base64url'), v.name).toBe(
          v.expect.outputs?.auth_b64
        );
        expect(b64(expand(prk, 'concord-wrap-key-v1')), v.name).toBe(v.expect.outputs?.wrap_b64);
        expect(b64(expand(prk, 'concord-prefs-key-v1')), v.name).toBe(v.expect.outputs?.prefs_b64);
      } else if (v.operation === 'descriptor') {
        const d = JSON.parse(unb64(get(v, 'descriptor_b64')).toString()) as unknown[];
        expect(d[0], v.name).toBe('split-auth-wrap-v1');
        expect(d.slice(1), v.name).toEqual([
          v.expect.outputs?.realm,
          v.expect.outputs?.subject_b64,
          v.expect.outputs?.generation,
          v.expect.outputs?.salt_b64,
          65536,
          3,
          4,
        ]);
      }
    }
  });

  it('recomputes accepted AES-GCM outputs with the platform primitive', () => {
    for (const v of corpus.vectors.filter(
      (x) => x.suite === 'aead' && x.expect.verdict === 'accept'
    )) {
      const format = get(v, 'format');
      const key = unb64(get(v, 'key_b64'));
      const aad = unb64(get(v, v.operation === 'encrypt' ? 'context_b64' : 'expected_context_b64'));
      const parsed = parsedContext(aad);
      expect(parsed.tag, v.name).toBe(formatTag[format]);
      if (v.operation === 'decrypt') {
        const wire = unb64(get(v, 'envelope_b64'));
        const { nonce, sealed } = splitEnvelope(format, wire);
        expect(b64(open(key, nonce, sealed, aad)), v.name).toBe(v.expect.outputs?.plaintext_b64);
      } else {
        const nonce = unb64(get(v, 'nonce_b64'));
        const sealed = seal(key, nonce, unb64(get(v, 'plaintext_b64')), aad);
        let wire: Buffer;
        if (format === 'attachment-v4') {
          wire = Buffer.alloc(28);
          wire.write('CV');
          wire[2] = 4;
          wire.writeUInt32BE(8388608, 4);
          wire.writeUInt32BE(1, 8);
          unb64(parsed.fields[6] as string).copy(wire, 12);
          wire = Buffer.concat([wire, nonce, sealed]);
        } else {
          const prefix = format.startsWith('rw1-') ? 'rw1:' : `${format}:`;
          wire = utf8(prefix + b64(Buffer.concat([nonce, sealed])));
        }
        expect(b64(wire), v.name).toBe(v.expect.outputs?.envelope_b64);
      }
    }
  });

  it('requires actual authentication failure for valid altered context bytes', () => {
    for (const v of corpus.vectors.filter(
      (x) => x.suite === 'aead' && x.operation === 'decrypt' && x.expect.stage === 'authenticate'
    )) {
      const key = unb64(get(v, 'key_b64'));
      const aad = unb64(get(v, 'expected_context_b64'));
      const parsed = parsedContext(aad);
      expect(validContext(parsed.tag, parsed.fields), v.name).toBe(true);
      const wire = unb64(get(v, 'envelope_b64'));
      const { nonce, sealed } = splitEnvelope(get(v, 'format'), wire);
      // The v4 header supplies candidate fileNonce AAD. A tampered header must
      // authenticate that candidate, rather than retrying the original nonce.
      const candidateAad = v.name.endsWith('/tamper-header')
        ? context(
            parsed.tag,
            parsed.fields.map((field, index) => (index === 6 ? b64(wire.subarray(12, 28)) : field))
          )
        : aad;
      expect(() => open(key, nonce, sealed, candidateAad), v.name).toThrow();
    }
  });

  it('classifies every rejecting candidate through the test adapter', () => {
    let count = 0;
    for (const v of corpus.vectors.filter((candidate) => candidate.expect.verdict === 'reject')) {
      expect(v.negative_of, v.name).toBeTruthy();
      expect(vectors.has(v.negative_of as string), v.name).toBe(true);
      expect(classifyReject(v), v.name).toBe(`${v.expect.stage}/${v.expect.reason}`);
      count++;
    }
    expect(count).toBe(132);
  });

  it('independently evaluates every policy outcome and descriptor shape', () => {
    const uniform: Record<string, unknown>[] = [];
    let legacy = 0;
    let upgrades = 0;
    for (const v of corpus.vectors.filter((candidate) => candidate.suite === 'policy')) {
      const out = v.expect.outputs;
      expect(v.expect.verdict, v.name).toBe('accept');
      if (v.operation === 'legacy-continuation') {
        const [decision, reason] = legacyPolicy(v.input);
        expect([out?.decision, out?.reason], v.name).toEqual([decision, reason]);
        if (v.name.includes('/matrix/')) legacy++;
      } else if (v.operation === 'upgrade-outcome') {
        const [decision, reason] = upgradePolicy(v.input);
        expect([out?.decision, out?.reason], v.name).toEqual([decision, reason]);
        if (v.name.includes('/matrix/')) upgrades++;
      } else {
        const bytes = unb64(get(v, 'descriptor_b64'));
        const d = JSON.parse(bytes.toString()) as unknown[];
        expect(d.length, v.name).toBe(8);
        expect([d[0], d[5], d[6], d[7]], v.name).toEqual(['split-auth-wrap-v1', 65536, 3, 4]);
        expect(bytes.includes(utf8(get(v, 'probe_public_account_uuid'))), v.name).toBe(false);
        uniform.push(out as Record<string, unknown>);
      }
    }
    expect(legacy).toBe(384);
    expect(upgrades).toBe(96);
    expect(uniform).toHaveLength(3);
    expect(uniform[1]).toEqual(uniform[0]);
    expect(uniform[2]).toEqual(uniform[0]);
  });
});
