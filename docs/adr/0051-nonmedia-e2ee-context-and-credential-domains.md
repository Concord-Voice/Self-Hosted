# ADR-0051: Non-media E2EE context and credential domains

**Status:** Proposed. Wire vectors and specialist review are required before this becomes Accepted. No writer activation is authorized by this ADR.
**Scope:** Account-key wrapping, non-media ciphertext, and channel-key distribution across the TypeScript, Desktop, Swift, and Kotlin clients. Media frames use their own versioned protocol.

## Context and trust boundary

The server currently receives the same password from which the client derives the RSA private-key wrapping key. Several non-media AES-GCM writers omit the object context from additional authenticated data (AAD). A channel-key recipient and distributor also need authenticated bindings. These are separate protocol problems and must share a migration plan so a new reader never falls back to an old format after a new-format verification failure.

The control plane remains authoritative for membership and roles. An operator who grants a real attacker account membership can still authorize that account to receive future group keys. A captured authentication output permits offline password guesses; a successful guess also derives the wrapping output. Those are accepted limits of this incremental design. The account signing root stays in its protected authority boundary and is never password-wrapped, recovery-wrapped, or sent to the renderer or control plane.

## Decision

### Credential domain separation

The protocol discriminator is `split-auth-wrap-v1`. The password input is its exact UTF-8 encoding; clients do not normalize it. Every client rejects an unpaired UTF-16 surrogate or other invalid Unicode scalar sequence before derivation, rather than silently replacing it. The server generates a stable, random 32-byte `credential_subject` and reserves the final account UUID before the client wraps the account key. Pending-registration promotion uses that same UUID.

The client derives:

```text
root = Argon2id_v19(UTF8(password), salt[16], m=65536 KiB, t=3, p=4, out=32)
bind = ASCII("concord-credential-bind-v1\0")
    || ASCII(lowercase-canonical-authority-realm-UUID)
    || credential_subject[32]
    || credential_generation_u64be
prk = HKDF-Extract-SHA256(salt=bind, IKM=root)
auth = HKDF-Expand-SHA256(prk, ASCII("concord-auth-secret-v1\0"), 32)
wrap = HKDF-Expand-SHA256(prk, ASCII("concord-wrap-key-v1\0"), 32)
prefs = HKDF-Expand-SHA256(prk, ASCII("concord-prefs-key-v1\0"), 32)
```

`auth` is sent as unpadded base64url (43 ASCII characters) in a versioned proof field. The server stores its ordinary password verifier over that ASCII value. `wrap` and `prefs` are client-only key inputs. The authority realm UUID is the cryptographic realm identity; an API hostname is transport configuration and cannot substitute for it. Before deriving a new credential, the client verifies the descriptor realm against the selected profile's authenticated authority trust chain. First registration also requires that trusted realm bootstrap; an unpinned or unknown realm cannot activate this protocol.

The credential descriptor is one canonical UTF-8 JSON array, with no whitespace, escapes, objects, or trailing bytes:

```text
["split-auth-wrap-v1","<realm-uuid>","<subject-b64>",<generation>,"<salt-b64>",65536,3,4]
```

`realm-uuid` has the same lowercase 36-byte form as the authenticated profile realm. `subject-b64` is standard padded base64 of exactly 32 bytes (44 ASCII characters); `salt-b64` is standard padded base64 of exactly 16 bytes (24 ASCII characters). Decoding followed by re-encoding must reproduce each field byte for byte. `generation` is an unsigned decimal integer in `1..9007199254740991`, without leading zeroes. The complete descriptor is at most 256 bytes. A known split account receives its stored descriptor. Legacy and unknown identifiers receive deterministic synthetic descriptors from a dedicated, domain-separated server secret, with the same field shapes, allowed values, status, and rate-limit class. No pre-auth field reveals account state or its public UUID. The first login proof always uses `auth`. A failed proof gets an opaque continuation on the same response path for a wrong split proof, a legacy account, and an unknown account. The raw-password continuation succeeds only for a legacy verifier, under the normal MFA policy.

Every invalid proof before MFA, including unknown account, wrong split proof, and wrong legacy raw password, has the same status, response shape, error class, and attempt-budget charge. Unknown and split records take a fixed-cost synthetic verifier path during the raw continuation. A valid legacy password does not release legacy key material or an upgrade authorization until MFA succeeds. The implementation must test response and bounded timing equivalence across these cases; correct credentials necessarily proceed to the normal MFA state.

`credential_protocol` selects verifier semantics; `user_keys.key_derivation_alg` describes the private-key wrap format. Every password proof surface, including step-up, recovery, password change, and SSO linking, must use the versioned contract before activation.

A successful legacy login may issue a 32-byte random, five-minute, single-use upgrade authorization after MFA. Only its digest is stored. It binds the account, login family, credential epoch and generation, legacy protocol, current account-key row version/digest, and new descriptor digest. The client must first unwrap the old key locally. One compare-and-swap transaction consumes the authorization and atomically replaces the verifier protocol, credential generation, and private-key wrap. It preserves the RSA public key, account-key generation, and decryptable history. An ordinary bearer token alone cannot authorize this change.

An upgrade has a stable idempotency key and a server-keyed request fingerprint; the server stores neither raw `auth` nor an unkeyed digest of it. An identical completed retry returns completion, while a different request conflicts. A timeout or ambiguous commit is reconciled from durable status before any retry. The committed transaction advances the credential epoch and revokes every pre-upgrade session family atomically with the credential and wrap change. Durable disconnect delivery follows the existing outbox contract. Exact SQL layout, replacement-session issuance, and outbox sequencing need implementation review. In-flight login, refresh, and upgrade races must be tested.

### Canonical non-authority context

New non-authority contexts use ASCII bytes of one positional JSON array:

```text
["concord-e2ee-context",1,"<registered-tag>",<registered-fields...>]
```

There is no whitespace, escape, object, optional field, or trailing byte. UUIDs are lowercase canonical 36-character strings; binary values use standard padded base64 with exact re-encoding; integers are `0` or unpadded decimal in `0..9007199254740991`. Each tag fixes field count, order, type, enum values, and per-field caps. The whole context is at most 4096 bytes. A reader parses and validates, re-encodes, and requires byte-for-byte equality. Authority-signed objects continue to use the existing, separately frozen authority protocol-v1 codec; this registry does not extend it.

The following field lists are positional. `realm`, `scope_id`, and account/object identifiers are canonical UUIDs; `scope` is `channel` or `dm`. An epoch or generation is a nonnegative integer. The implementation and vector corpus must freeze each remaining purpose/content-kind enum before writers activate.

| Tag | Fields after tag |
|---|---|
| `message-v1` | realm, scope, scope_id, message_id, sender_id, key_epoch, content_kind |
| `blob-v1` | realm, account_id, purpose, schema_version |
| `account-ek-wrap-v1` | realm, account_id, key_purpose, account_key_generation, credential_generation |
| `recovery-wrap-v1` | realm, account_id, key_purpose, recovery_purpose, recovery_key_generation, account_key_generation |
| `attachment-v4` | realm, scope, scope_id, file_id, uploader_id, key_epoch, file_nonce_b64, flags, chunk_index, total_chunks, chunk_size |
| `channel-key-capsule-v1` | realm, scope, scope_id, key_epoch, recipient_id, recipient_key_generation, distributor_id, distributor_root_generation |

`key_purpose` has the single v1 value `account-rsa-oaep4096-pss-sha256`: the existing RSA account key pair may decrypt OAEP/SHA-256 account-key capsules and make its existing PSS/SHA-256 age claim. This does not designate the separate P-384 authority signing root. A wrong purpose or algorithm fails before unwrap; the corpus must mutate it in both password and recovery wraps. `recovery_purpose` and other producer-specific enums remain to be frozen before acceptance.

For a new message, the client allocates `message_id` before encryption and the server persists that ID. An identical retry by the same sender returns the existing message; reuse with different protected fields conflicts. For an attachment, upload initialization already returns `file_id`; the client retains it and binds it to every v4 chunk and final authenticated message reference. The server checks that session, committed file, and message metadata name the same ID. A v4 reader derives `realm` from its authenticated authority profile, `scope` and `scope_id` from the selected channel or DM route, and `file_id`, `uploader_id`, and `key_epoch` from the verified enclosing message context and its authenticated attachment reference. It rejects any disagreement with server file metadata before constructing per-chunk AAD; server metadata alone is never the expected context. The v4 reader therefore depends on the verified new message context and cannot activate first. `fileNonce` remains random nonce material, never the attachment identity.

AES-GCM uses a fresh 12-byte nonce from the platform cryptographic random source (Web Crypto `getRandomValues` or the native equivalent), a 16-byte tag, and the exact context bytes as `additionalData`. A test-only random-source seam must prove nonce generation and reject reuse; production never accepts caller-supplied nonces. New text envelopes have distinct non-base64 prefixes (`m1:`, `b1:`, `ek1:`, `rw1:`) followed by standard padded base64 of `nonce || ciphertext || tag`; readers dispatch by prefix before decryption. The attachment v4 binary header retains the v3 28-byte geometry with version byte 4, and each chunk authenticates its expected `attachment-v4` context. The writer must retain v3's uniform multipart geometry. A missing or mismatched expected context fails; it never triggers trial decryption with an older format.

### Channel-key distribution

Only a protected verified recipient-key handle may enter a new wrap. The RSA-OAEP-4096/SHA-256 `label` is exactly the canonical `channel-key-capsule-v1` context bytes. A complete distribution must bind the capsule digest, recipient and distributor identities, protected key generations, scope, and epoch to a distributor proof from its authority-bound P-384 root. The recipient must verify that proof, protected account-to-key bindings, a trusted control-plane authorization receipt, and equality of all envelope and expected-context fields before RSA decryption.

This ADR does **not** register `distribution-intent-v1` or `distribution-authz-v1` as wire formats or authorize a distribution writer. The follow-on channel-distribution decision must freeze the distributor's signed bytes and signature encoding, the control plane's delegated-CAK signer and trust-generation reference, the receipt's signed bytes, signature bytes, storage representation, and verification inputs. It must also prove transaction-fenced membership, VIEW permission, distributor key possession, recipient key generation, and channel or DM rotation rules before a receipt can be issued. An incomplete wrap is not readable as complete until its receipt is durable. A removal prevents later receipts; it does not expire a historical receipt or destroy previously decryptable ciphertext. These are required invariants under the stated control-plane membership trust model, not yet an interoperable receipt format.

### Migration and activation

New readers explicitly dispatch by stored format. Legacy messages, whole-file attachments, chunked attachment v2/v3, and old account wraps remain readable during migration; new-format verification failure never falls back to a legacy reader. Generic untyped blob encryption and unauthenticated shared-secret helpers are not new-protocol writers. Trusted-device recovery v2 remains its own existing protocol.

The shared vector corpus must contain exact canonical bytes, Argon2/HKDF outputs including invalid-Unicode rejection, AES-GCM AAD mutations including tampered attachment metadata, RSA label and key-purpose mismatches, descriptor uniformity, upgrade replay/race outcomes, and one altered-context case per bound field. The same corpus must pass in Go, shared TypeScript, Desktop, Swift, and Kotlin. Its generator and contents receive SHA-384 pins before this ADR is Accepted. Distributor and CAK signature vectors belong to the follow-on channel-distribution decision.

**Acceptance requires:** freeze the exact `recovery_purpose`, `content_kind`, blob purpose, and attachment flag registries; every producer's expected-context provenance; and the shared byte vectors. A separate channel-distribution decision must freeze its proof and receipt format before that writer can activate. The Proposed layouts are not an implementation license until those reviews pass.

Writer activation, old-client exclusion, future encryption material, rollback, and legacy-reader retirement require a separate client-cutover decision and cross-client evidence. Protected recipient/root handles require the account-key authority work. The final social-recovery transcript waits for verified contacts and Recovery Circle share policy. Raw wrap-key persistence has a separate owner. This ADR does not decide those interfaces or enable a new writer.

## Rejected shortcuts

- A renderer signing root or first-contact pin duplicates the account-key authority and leaves first contact unauthenticated.
- An API-origin KDF binding changes credential identity when transport hostnames move.
- A client-selected account UUID transfers identity allocation to an unauthenticated client.
- An expiring distribution receipt strands historical or offline ciphertext.
- A universal one-byte envelope prefix can collide with legacy binary formats; each producer gets explicit dispatch.
