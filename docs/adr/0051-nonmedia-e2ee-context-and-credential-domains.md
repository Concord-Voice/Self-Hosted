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

`credential_generation_u64be` is the eight-byte, big-endian, zero-extended encoding of the descriptor's exact integer in `1..9007199254740991`. The server advances the generation monotonically, refuses rollover at that bound, and never reuses a generation for a different credential. The shared corpus includes the maximum generation and its eight encoded bytes.

`auth` is sent as unpadded base64url (43 ASCII characters) in a versioned proof field. The server stores its ordinary password verifier over that ASCII value. `wrap` and `prefs` are client-only key inputs. The authority realm UUID is the cryptographic realm identity; an API hostname is transport configuration and cannot substitute for it. Before deriving a new credential, the client verifies the descriptor realm against the selected profile's authenticated authority trust chain. First registration also requires that trusted realm bootstrap; an unpinned or unknown realm cannot activate this protocol.

The credential descriptor is one canonical UTF-8 JSON array, with no whitespace, escapes, objects, or trailing bytes:

```text
["split-auth-wrap-v1","<realm-uuid>","<subject-b64>",<generation>,"<salt-b64>",65536,3,4]
```

`realm-uuid` has the same lowercase 36-byte form as the authenticated profile realm. `subject-b64` is standard padded base64 of exactly 32 bytes (44 ASCII characters); `salt-b64` is standard padded base64 of exactly 16 bytes (24 ASCII characters). Decoding followed by re-encoding must reproduce each field byte for byte. `generation` is an unsigned decimal integer in `1..9007199254740991`, without leading zeroes. The complete descriptor is at most 256 bytes. A known split account receives its stored descriptor. Legacy and unknown identifiers receive deterministic synthetic descriptors from a dedicated, domain-separated server secret, with the same field shapes, allowed values, status, and rate-limit class. No pre-auth field reveals account state or its public UUID. The first login proof always uses `auth`. A failed proof gets an opaque continuation on the same response path for a wrong split proof, a legacy account, and an unknown account. The continuation is not permission to send the raw password: the client permits that only after verifying a current, anti-rollback legacy-status attestation under the independently authenticated account authority. Neither the control-plane response nor locally cached legacy state alone suffices, including on a fresh device. The attestation must bind the authority realm, authority-bound account identity, `credential_protocol = legacy`, authoritative credential generation, issuance and expiry, and a monotonic authority-chain freshness reference that prevents replay after upgrade. The exact signer and freshness mechanism remain to be frozen; no legacy fallback can activate until they are. Without that proof, including on a fresh device, the client refuses the raw-password path. The raw-password continuation succeeds only for a legacy verifier, under the normal MFA policy.

Clients reject any descriptor whose Argon2id values differ from exactly `(65536 KiB, 3, 4)`; changing these values requires a new protocol discriminator. These values match the current client derivation constants, but security-owner approval of the new credential protocol remains an acceptance gate. The vector corpus includes a lower-parameter descriptor that every client rejects before KDF work.

Every invalid proof before MFA, including unknown account, wrong split proof, and wrong legacy raw password, has the same status, response shape, error class, and attempt-budget charge. Unknown and split records take a fixed-cost synthetic verifier path during the raw continuation. A valid legacy password does not release legacy key material or an upgrade authorization until MFA succeeds. The implementation must test response and bounded timing equivalence across these cases; correct credentials necessarily proceed to the normal MFA state.

`credential_protocol` selects verifier semantics; `user_keys.key_derivation_alg` describes the private-key wrap format. The following inventory is the S0 classification gate. It identifies the password-bearing routes that must be audited before activation; it does not activate the split protocol on any route.

| Surface family | Classification | S0 requirement |
|---|---|---|
| Pending registration, registration, login, password recovery, sessions, ownership confirmation, user password change, and account-authenticated OAuth or SSO linking | Account credential | Must use the versioned account-credential contract before activation. |
| E2EE key reveal (`RevealMyKeys`, `current_password`) and account-security posture changes (`PatchSecurity`, `current_passphrase`) | Account credential | These direct password-verifier rails must use the versioned contract and each needs a migrated-account test before activation. |
| MFA, generic step-up, and the DM-purge step-up path | Factor verification | Must consume the account-credential verifier through the versioned contract; their second factors remain their own protocols. |
| Admin and bootstrap credentials | Separate administrator credential | Must remain separately classified and must not silently inherit `split-auth-wrap-v1` or the account private-key-wrap semantics. |

The implementation review must enumerate direct and adapted verifier callers within each family, including authority-mediated flows, and add a migrated-account test for every family before activation.

A successful legacy login may issue a 32-byte random, five-minute, single-use upgrade authorization after MFA. Only its digest is stored. It binds the account, login family, credential epoch and generation, legacy protocol, current account-key row version/digest, and new descriptor digest. The client must first unwrap the old key locally. One compare-and-swap transaction consumes the authorization and atomically replaces the verifier protocol, credential generation, and private-key wrap. It preserves the RSA public key, account-key generation, and decryptable history. An ordinary bearer token alone cannot authorize this change.

An upgrade has a stable idempotency key and a server-keyed request fingerprint; the server stores neither raw `auth` nor an unkeyed digest of it. An identical completed retry returns completion, while a different request conflicts. A timeout or ambiguous commit is reconciled from durable status before any retry. The committed transaction advances the credential epoch and revokes every pre-upgrade session family atomically with the credential and wrap change. Durable disconnect delivery follows the existing outbox contract. Exact SQL layout, replacement-session issuance, and outbox sequencing need implementation review. In-flight login, refresh, and upgrade races must be tested.

### Canonical non-authority context

New non-authority contexts use ASCII bytes of one positional JSON array:

```text
["concord-e2ee-context",1,"<registered-tag>",<registered-fields...>]
```

There is no whitespace, escape, object, optional field, or trailing byte. UUIDs are lowercase canonical 36-character strings; binary values use standard padded base64 with exact re-encoding; integers are `0` or unpadded decimal in `0..9007199254740991`. Each tag fixes field count, order, type, enum values, and per-field caps. The whole context is at most 4096 bytes. A reader parses and validates, re-encodes, and requires byte-for-byte equality. Authority-signed objects continue to use the existing, separately frozen authority protocol-v1 codec; this registry does not extend it.

The following field lists are positional. `realm`, `scope_id`, and account/object identifiers are canonical UUIDs; `scope` is `channel` or `dm`. `key_epoch`, `account_key_generation`, `recipient_key_generation`, `credential_generation`, and `recovery_key_generation` are integers in `1..9007199254740991`. The v1 enum registry is closed: a reader rejects an unknown value before decryption, and a writer cannot select a generic fallback.

| Tag | Fields after tag |
|---|---|
| `message-v1` | realm, scope, scope_id, message_id, sender_id, key_epoch, content_kind |
| `blob-v1` | realm, account_id, blob_purpose, schema_version |
| `account-ek-wrap-v1` | realm, account_id, key_purpose, account_key_generation, credential_generation |
| `recovery-wrap-v1` | realm, account_id, key_purpose, recovery_purpose, recovery_key_generation, account_key_generation |
| `recovery-prefs-wrap-v1` | realm, account_id, key_purpose, recovery_purpose, recovery_key_generation, credential_generation |
| `attachment-v4` | realm, scope, scope_id, file_id, uploader_id, key_epoch, file_nonce_b64, flags, chunk_index, total_chunks, chunk_size |
| `channel-key-capsule-v1` | realm, scope, scope_id, key_epoch, recipient_id, recipient_key_generation, distributor_id, distributor_root_digest_b64 |

For `attachment-v4`, `file_nonce_b64` decodes to exactly 16 bytes, `flags` is `0`, `chunk_size` is exactly `8388608`, `total_chunks` is an unsigned 32-bit wire field limited to `1..10000` by the existing multipart upload contract, and `chunk_index` is an unsigned 32-bit wire field in `0..total_chunks-1`. Readers reject out-of-range geometry before allocation or decryption. The binary header is exactly 28 bytes. Changing any of these wire constraints requires a new attachment tag and version.

`key_purpose` is valid only in the listed tag-purpose pairs. `account-rsa-oaep4096-pss-sha256` is the only value for both `account-ek-wrap-v1` and `recovery-wrap-v1`: the RSA account key pair may decrypt OAEP/SHA-256 account-key capsules and make its existing PSS/SHA-256 age claim. This does not designate the separate P-384 authority signing root. `preferences-aes256-gcm` is the only value for `recovery-prefs-wrap-v1`; it identifies the raw 32-byte AES-GCM preferences key derived for the named credential generation. Readers reject every unlisted tag-purpose pair before unwrap, and the corpus mutates each applicable wrap.

`account_key_generation` comes from protected account-key authority state and identifies the RSA account key being recovered. `credential_generation` is the exact generation from the authenticated `split-auth-wrap-v1` descriptor and identifies the preferences key derived from that credential. A legacy account without that authenticated split descriptor cannot emit `recovery-prefs-wrap-v1`; its existing preferences-key recovery ciphertext remains legacy-readable. `recovery_key_generation` is a monotonic per-account value reserved under authenticated step-up before client encryption and committed by compare-and-swap with the recovery-key row. The private-key and preferences-key wraps created by one enrollment or replacement carry the same generation. It is never inferred from ciphertext, cached client state, or a post-write database value. The current server-internal `user_recovery_keys.material_version` assignment is not this pre-encryption reservation contract; that contract must exist before either new recovery writer activates.

| Enum | Allowed v1 values | Rule |
|---|---|---|
| `content_kind` | `message-body` | The existing encrypted message body. A distinct plaintext semantic needs a new registered value and vector set. |
| `blob_purpose` | `preferences`, `saved_gifs`, `friend_organization`, `presence_overrides` | Each existing preference-domain producer uses its own value. A blob cannot be accepted under another purpose. |
| `(blob_purpose, schema_version)` | `(preferences, 1)`, `(saved_gifs, 1)`, `(friend_organization, 1)`, `(presence_overrides, 1)` | These are the only v1 blob schema pairs. Both writer and reader select the expected pair locally; every other pair fails before decryption. |
| `recovery_purpose` | `user-account-recovery-key` | The current user-held account recovery key only. Trusted-device recovery v2 and future social recovery remain separate protocols. |
| `attachment-v4.flags` | `0` | Every nonzero value is reserved and rejected. |

Expected context comes from authenticated or local sources, never from unauthenticated server metadata alone:

| Field | Required provenance |
|---|---|
| `realm` | Authenticated authority profile. |
| `scope`, `scope_id` | The selected channel or DM route. |
| `account_id` | Authenticated account identity from the protected session or account authority. |
| `message_id` | On write, client allocation before encryption and idempotent server persistence. On read, the candidate persisted ID from the selected route's message record; the GCM tag must authenticate its equality before plaintext release. The record alone is not trusted. |
| `sender_id`, `uploader_id` | On write, the authenticated local session. On read, the candidate server-derived author in the selected route's message record and, for uploads, the verified enclosing message and authenticated attachment reference. The GCM tag must authenticate the declared identity; caller-supplied identity or ciphertext metadata alone is not trusted. |
| `key_epoch` | On write, the current authorized epoch in verified conversation-key state. On read, the candidate historical message epoch must resolve to verified key state for that exact epoch and authenticate under its GCM tag; an attachment read inherits the epoch from its verified enclosing message. The current epoch is never substituted for a historical one. |
| `content_kind`, `blob_purpose` | The closed local producer registry above. |
| `schema_version` | The reader's locally expected version for the closed `blob_purpose` pair; never an unauthenticated server-selected schema. |
| `key_purpose`, `recovery_purpose` | The closed tag-specific local registry and protected key or recovery handle. |
| `file_id` | Upload initialization, cross-checked against the authenticated message reference. |
| `file_nonce_b64` | Writer-generated 16-byte cryptographic random value carried in the v4 header. The reader validates its canonical encoding and length, uses it only as candidate AAD, and releases no chunk until its tag authenticates that value. It is not the attachment identity. |
| `flags`, `chunk_index`, `total_chunks`, `chunk_size` | Fixed zero flags and a bounded local chunk plan on write; on read, the expected ordinal and authenticated v4 header geometry, checked against actual chunk boundaries before plaintext release. |
| Account and key generations | Protected authority or credential state, with the pre-encryption recovery-key reservation and compare-and-swap rule above. |
| Capsule recipient and distributor fields | Protected handles and authority state, never raw server key JSON. |
| `distributor_root_digest_b64` | Standard padded base64 of exactly 48 bytes, equal to `active_root_digest` in independently verified account-authority status for the distributor. It is not a product-root `delegation_gen`, a control-plane key object, or a caller-supplied value. |

For a new message, the client allocates `message_id` before encryption and the server persists that ID. An identical retry by the same sender returns the existing message; reuse with different protected fields conflicts. For an attachment, upload initialization already returns `file_id`; the client retains it and binds it to every v4 chunk and final authenticated message reference. The server checks that session, committed file, and message metadata name the same ID. A v4 reader derives `realm` from its authenticated authority profile, `scope` and `scope_id` from the selected channel or DM route, and `file_id`, `uploader_id`, and `key_epoch` from the verified enclosing message context and its authenticated attachment reference. It rejects any disagreement with server file metadata before constructing per-chunk AAD; server metadata alone is never the expected context. The v4 reader therefore depends on the verified new message context and cannot activate first. `fileNonce` remains random nonce material, never the attachment identity.

AES-GCM uses a fresh 12-byte nonce from the platform cryptographic random source (Web Crypto `getRandomValues` or the native equivalent), a 16-byte tag, and the exact context bytes as `additionalData`. A test-only random-source seam must prove nonce generation and reject reuse; production never accepts caller-supplied nonces. New text envelopes have distinct non-base64 prefixes (`m1:`, `b1:`, `ek1:`, `rw1:`) followed by standard padded base64 of `nonce || ciphertext || tag`; readers dispatch by prefix before decryption. The attachment v4 binary header retains the v3 28-byte geometry with version byte 4, and each chunk authenticates its expected `attachment-v4` context. The writer must retain v3's uniform multipart geometry. A missing or mismatched expected context fails; it never triggers trial decryption with an older format.

Both recovery-wrap tags may use `rw1:` because their existing storage slots are distinct and typed. The reader chooses exactly one expected tag from its local recovery operation and target key purpose, never from a server-supplied slot label; it never tries the other recovery tag after an authentication failure.

### Channel-key distribution

Only a protected verified recipient-key handle may enter a new wrap. The RSA-OAEP-4096/SHA-256 `label` is exactly the canonical `channel-key-capsule-v1` context bytes. A complete distribution must bind the capsule digest, recipient and distributor identities, protected key generations, scope, and epoch to a distributor proof from its authority-bound P-384 root. The recipient must verify that proof, protected account-to-key bindings, a trusted control-plane authorization receipt, and equality of all envelope and expected-context fields before RSA decryption.

This ADR does **not** register `distribution-intent-v1` or `distribution-authz-v1` as wire formats or authorize a distribution writer. The follow-on channel-distribution decision must freeze the distributor's signed bytes and signature encoding, the control plane's delegated-CAK signer and trust-generation reference, the receipt's signed bytes, signature bytes, storage representation, and verification inputs. It must also prove transaction-fenced membership, VIEW permission, distributor key possession, recipient key generation, and channel or DM rotation rules before a receipt can be issued. An incomplete wrap is not readable as complete until its receipt is durable. A removal prevents later receipts; it does not expire a historical receipt or destroy previously decryptable ciphertext. These are required invariants under the stated control-plane membership trust model, not yet an interoperable receipt format.

### Migration and activation

New readers explicitly dispatch by stored format. Legacy messages, whole-file attachments, chunked attachment v2/v3, and old account wraps remain readable during migration; new-format verification failure never falls back to a legacy reader. Generic untyped blob encryption and unauthenticated shared-secret helpers are not new-protocol writers. Trusted-device recovery v2 remains its own existing protocol.

S0-A freezes this document's registries, provenance rules, and required vector categories, not generated bytes or a corpus serialization. S0-B first reviews and freezes the corpus serialization, then creates the shared, read-only conformance corpus; it must contain a positive context for every tag including `recovery-prefs-wrap-v1`, exact canonical bytes, Argon2/HKDF outputs including invalid-Unicode rejection, AES-GCM AAD mutations including tampered attachment metadata, RSA label, distributor-root-digest, and cross-tag key-purpose mismatches, descriptor uniformity, a forged continuation offered to a split account that never causes raw-password submission, stale post-upgrade, wrong-account, wrong-realm, and expired legacy attestations, fresh-device refusal without proof, upgrade replay/race outcomes, and one altered-context case per bound field. The same corpus must pass in Go, shared TypeScript, Desktop, Swift, and Kotlin. Its generator and contents receive SHA-384 pins before this ADR is Accepted. Distributor and CAK signature vectors remain in the follow-on channel-distribution decision; S0 only vectors the `channel-key-capsule-v1` context and label.

The S0 migration limitation is explicit: a fresh legacy device without independently authenticated, anti-rollback authority evidence refuses the raw-password continuation. Cached state and a control-plane response never substitute for that evidence, and no fingerprint-comparison fallback is introduced. Existing-device migration also waits for independent authority proof; the proof design for both existing and fresh devices is later account-authority work.

**Acceptance requires:** security-owner sign-off on the pinned credential parameters and this fresh-device limitation; freeze the exact tag-scoped `key_purpose`, `recovery_purpose`, `content_kind`, `(blob_purpose, schema_version)`, attachment flag, and attachment geometry registries; every producer's expected-context provenance; and the shared byte vectors. A separate channel-distribution decision must freeze its proof and receipt format before that writer can activate. The Proposed layouts are not an implementation license until those reviews pass.

Writer activation, old-client exclusion, future encryption material, rollback, and legacy-reader retirement require a separate client-cutover decision and cross-client evidence. Protected recipient/root handles require the account-key authority work. The final social-recovery transcript waits for verified contacts and Recovery Circle share policy. Raw wrap-key persistence has a separate owner. This ADR does not decide those interfaces or enable a new writer.

## Rejected shortcuts

- A renderer signing root or first-contact pin duplicates the account-key authority and leaves first contact unauthenticated.
- An API-origin KDF binding changes credential identity when transport hostnames move.
- A client-selected account UUID transfers identity allocation to an unauthenticated client.
- An expiring distribution receipt strands historical or offline ciphertext.
- A universal one-byte envelope prefix can collide with legacy binary formats; each producer gets explicit dispatch.
