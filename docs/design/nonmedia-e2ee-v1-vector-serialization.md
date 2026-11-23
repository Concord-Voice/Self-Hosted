# ADR-0051 S0-B0 vector corpus serialization

This document specifies a test-only carrier for non-media E2EE conformance cases. It does not define a production message, activate a writer or verifier, or grant legacy raw-password fallback. Corpus generation starts only after the S0-A registry PR merges and this serialization has passed separate review.

## File and byte grammar

The canonical file is `docs/design/nonmedia-e2ee-v1-vectors.json`. It is UTF-8 containing ASCII only, LF line endings, no BOM, no CR, no indentation, no trailing spaces, and exactly one final LF. It is at most 1 MiB. Object key order is fixed. No duplicate object keys, duplicate vector names, unknown keys, extra fields, or JSON values outside the forms below are accepted.

```text
{"schema":"concord-nonmedia-e2ee-vectors","version":1,"generator_sha384":"<96 lowercase hex>","vectors_sha384":"<96 lowercase hex>","vectors":[
{"name":"...","suite":"...","operation":"...","cite":"...","input":{},"expect":{}},
{"name":"...","suite":"...","operation":"...","cite":"...","input":{},"negative_of":"...","mutation":{},"expect":{}}
]}
```

The displayed line breaks are literal LF bytes; the final `]}` line also ends in LF. The examples show the two allowed case shapes, not two required corpus cases.

`generator_sha384` is SHA-384 over the exact `gen.go` bytes; all generation logic must live in that file. `vectors_sha384` is the body hash: SHA-384 over the exact file suffix after the first LF through the final LF. It excludes the entire header line, including `schema`, `version`, `generator_sha384`, and `vectors_sha384`. The whole-file SHA-384 is pinned in ADR-0051 and in each language's test adapter; it is never embedded in this file. The source repository verifies the generator hash against `gen.go`. Every consumer verifies the body and whole-file hashes before interpreting cases and compares the stored generator pin with the approved pin.

Every vector has required keys in this order: `name`, `suite`, `operation`, `cite`, `input`, then, for a rejecting case, `negative_of` and `mutation`, and finally `expect`. `name` is a stable ASCII identifier matching `[a-z0-9][a-z0-9._/-]{0,127}`. `cite` names a specific ADR-0051 section and is 1..120 visible ASCII bytes excluding `"` and `\`. `suite` is one of `credential`, `context`, `aead`, `rsa-label`, `policy`. Byte strings use standard padded base64 with decode-and-re-encode equality, except the ADR's 43-character unpadded base64url auth proof. Numeric values are JSON integers within the ADR's per-field bounds; no floating point, exponent, sign, or leading zero syntax. Malformed production numeric spellings are carried inside base64-encoded candidate bytes, not as malformed corpus JSON.

The S0-B0 operation registry is closed. Names in braces below are object keys in the listed order; no extra key is allowed. `context_fields` is the positional array defined by the selected ADR-0051 tag. `b64` always means canonical standard padded base64. Each `outputs` object is present only on an accepted case.

`utf16_units_hex` is concatenated four-digit lowercase hexadecimal UTF-16 code units in source order (for example, `0041d800`). The empty string is a valid input and encodes an empty password. A lone surrogate is rejected before Argon2; malformed hex is invalid corpus data. The corpus must include the ADR's positive `recovery-prefs-wrap-v1` context, `distributor_root_digest_b64` binding, and cross-tag `key_purpose` rejection cases; a generic context vector cannot replace those named coverage requirements.

| Suite / operation | `input` keys in order | Accepted `outputs` keys in order |
|---|---|---|
| `credential` / `password-scalars` | `utf16_units_hex` | `password_utf8_b64` |
| `credential` / `derive` | `password_utf8_b64`, `realm`, `subject_b64`, `generation`, `salt_b64` | `root_b64`, `auth_b64`, `wrap_b64`, `prefs_b64` |
| `credential` / `descriptor` | `authenticated_realm`, `descriptor_b64` | `realm`, `subject_b64`, `generation`, `salt_b64`, `m_kib`, `t`, `p` |
| `context` / `encode` | `tag`, `context_fields` | `context_b64` |
| `context` / `parse` | `context_b64` | `tag`, `context_fields` |
| `aead` / `encrypt` | `format`, `key_b64`, `nonce_b64`, `context_b64`, `plaintext_b64` | `envelope_b64` |
| `aead` / `decrypt` | `format`, `key_b64`, `expected_context_b64`, `envelope_b64` | `plaintext_b64` |
| `rsa-label` / `derive` | `context_fields` | `label_b64` |
| `rsa-label` / `verify` | `context_fields`, `candidate_label_b64` | no keys (`{}`) |
| `policy` / `descriptor-uniformity` | `account_class`, `probe_public_account_uuid`, `descriptor_b64`, `proof_verdict` | `descriptor_shape`, `failed_proof_status`, `failed_proof_shape`, `failed_proof_error_class`, `attempt_charge` |
| `policy` / `legacy-continuation` | `device_state`, `account_protocol`, `authority_proof`, `realm_match`, `account_match`, `freshness`, `not_expired`, `control_plane_continuation` | `decision`, `reason` |
| `policy` / `upgrade-outcome` | `prior_state`, `authorization_state`, `request_match`, `mfa_complete`, `commit_state` | `decision`, `reason` |

`format` is one of `m1`, `b1`, `ek1`, `rw1-private`, `rw1-preferences`, `attachment-v4`; the two `rw1` values are typed test operations that share the same on-wire prefix. `key_b64` decodes to exactly 32 bytes, and test-only `nonce_b64` to exactly 12 bytes. The encrypt adapter rejects either wrong length before AES-GCM work; the decrypt adapter rejects a wrong key length before AES-GCM work. `envelope_b64` is base64 of the complete wire representation: the ASCII prefix plus encoded payload for text formats, or the complete v4 header and encrypted chunk for attachments. Each text payload is parsed as exactly `nonce[12] || ciphertext || tag[16]` after its prefix; fewer than 28 decoded bytes is `parse/bad_envelope`. The v4 binary form is exactly `header[28] || nonce[12] || encrypted_chunk || tag[16]` for the single-chunk corpus cases, with the chunk length checked against its authenticated header geometry before AES-GCM. These formats have no variable-length nonce field: a longer inserted nonce in a text payload is parsed as a fixed 12-byte nonce and shifted ciphertext, then fails tag authentication. The parser must never pass an 8- or 16-byte IV to AES-GCM or retry an alternate nonce split. `rsa-label` implicitly and exclusively uses `channel-key-capsule-v1` as its tag. `account_class` is `split`, `legacy`, or `unknown`; `proof_verdict` is `wrong`, the only pre-MFA outcome this uniformity operation covers. `probe_public_account_uuid` is a canonical, public synthetic UUID for the fixture, including the unknown-account fixture; it is never sent to a production descriptor endpoint. Its `descriptor_shape`, `failed_proof_shape`, and `failed_proof_error_class` are stable ASCII identifiers, `failed_proof_status` is an HTTP status integer, and `attempt_charge` is an integer count. The corpus must have one case for each account class and require all five outputs to be equal across those cases; descriptor **bytes** may differ but must share the ADR's size, field, and parameter shape. The source coverage test rejects a descriptor containing the probe UUID as text, its 16 raw bytes, or either canonical base64 encoding in any decoded descriptor field. This detects direct fixture-identity leakage; it does not prove that a live descriptor is uncorrelated with an account, which remains an activation privacy review and live test. This test-only comparison does not claim live handler timing equivalence, which remains an activation test. `device_state` is `fresh` or `existing`; `account_protocol` is `legacy` or `split`; `authority_proof` is `verified`, `absent`, or `invalid`; `freshness` is `current` or `stale`; `prior_state` is `legacy` or `split`; `authorization_state` is `fresh`, `consumed`, `expired`, or `absent`; `commit_state` is `none`, `committed`, or `ambiguous`. Boolean fields are JSON `true` or `false`. Policy decisions are `submit-raw-password` or `deny` for legacy continuation and `commit`, `idempotent`, `conflict`, `deny`, or `reconcile` for upgrade outcome. A valid policy evaluation that denies an operation is an **accepted** vector with `outputs.decision:"deny"`, not a rejected vector. `outputs.reason` is a closed policy reason: `eligible`, `not-legacy`, `missing-authority-proof`, `invalid-authority-proof`, `stale-authority-proof`, `wrong-account`, `wrong-realm`, `expired-proof`, `no-continuation`, `fresh-authorization`, `replay`, `request-conflict`, `mfa-required`, `ambiguous-commit`, `expired-authorization`, `missing-authorization`. In `upgrade-outcome`, an expired authorization maps to `deny/expired-authorization` and an absent one to `deny/missing-authorization`; neither is an authority-proof failure. `commit_state:"ambiguous"` maps to `decision:"reconcile"` and `reason:"ambiguous-commit"`; it never authorizes a blind retry. These policy cases describe abstract decisions only; they do not imply a frozen legacy-attestation or upgrade-request wire format. Required policy matrices are pinned by case name; they do not use `negative_of` or `mutation`.

An AEAD `format` is valid only with its exact canonical context tag: `m1` → `message-v1`, `b1` → `blob-v1`, `ek1` → `account-ek-wrap-v1`, `rw1-private` → `recovery-wrap-v1`, `rw1-preferences` → `recovery-prefs-wrap-v1`, and `attachment-v4` → `attachment-v4`. Both encrypt and decrypt test adapters parse the context, reject every other format/tag pair at `validate/invalid_format_context` before AEAD work, and never try another tag or prefix after a failure. The corpus includes a negative prefix/context-pair case for every text format and the binary attachment format.

Accepted `expect` has exactly `{"verdict":"accept","outputs":{...}}`; rejected `expect` has exactly `{"verdict":"reject","stage":"...","reason":"..."}`. The stage/reason registry is closed:

| Operation | Allowed rejected stage/reason pairs |
|---|---|
| `password-scalars` | `validate/invalid_unicode` |
| `derive` | `validate/invalid_field` |
| `descriptor` | `parse/malformed_descriptor`, `validate/weak_parameters`, `validate/wrong_realm` |
| `context.encode` | `validate/unknown_tag`, `validate/invalid_field`, `validate/invalid_purpose` |
| `context.parse` | `parse/malformed_context`, `validate/unknown_tag`, `validate/invalid_field`, `validate/invalid_purpose` |
| `aead.encrypt` | `validate/invalid_field`, `validate/invalid_purpose`, `validate/invalid_format_context` |
| `aead.decrypt` | `parse/bad_envelope`, `validate/invalid_field`, `validate/invalid_format_context`, `authenticate/aad_mismatch`, `authenticate/bad_tag` |
| `rsa-label.derive` | `validate/invalid_field` |
| `rsa-label.verify` | `validate/invalid_field`, `authenticate/invalid_label` |
| All policy operations | No rejected cases; malformed policy input is a corpus schema error. |

Tests compare these categories, not platform error messages.

Every rejecting case contains the complete input it actually consumes. `negative_of` names a positive vector in the same suite and operation. `mutation` has exactly `{"kind":"replace|remove|insert|reorder|truncate","path":"/input/...","semantic_field":"..."}`; `path` is an RFC 6901 JSON Pointer from the vector root. For `remove`, it resolves against the positive parent; for `insert`, against the negative case; otherwise against both. `semantic_field` names the one ADR bound field or operation input changed, including a field carried inside base64 candidate bytes. The test never executes a mutation engine: it evaluates the supplied complete `input`. A source-repository coverage test decodes candidate bytes as needed and checks that the declared mutation changes exactly that one semantic field from the named positive case. Policy matrices use named complete cases instead of mutation links, because deny, conflict, and reconciliation are successful policy evaluations.

The generator is deterministic and never reads time, randomness, locale, or environment to choose case bytes. Synthetic keys, salts, nonces, UUIDs, and passwords are public test inputs. A fixed nonce in the corpus is only a test vector; no production API accepts caller-supplied nonces. RSA coverage pins the exact capsule context and OAEP label bytes; it does not include private keys or randomized OAEP ciphertext. The policy suite may express abstract legacy-attestation validity and upgrade state, but it cannot claim byte-level attestation or CAK/distributor signature conformance before those wire formats are separately frozen.

## Required coverage manifest

The source coverage test expands the following **closed required-case manifest** before it evaluates any outputs. Names are literal and case-sensitive. It rejects a missing or duplicate required name, a wrong suite/operation/tag/format, a wrong `negative_of`, a mutation of the wrong `semantic_field`, or a wrong verdict or rejection category. Additional cases are permitted only if they satisfy the same schema and mutation rules. This manifest is checked independently of the generator; generating both the corpus and its coverage list from `gen.go` would make an omission self-attesting.

For each row below, require `context/encode/<tag>/base` and `context/parse/<tag>/base` as accepting cases. For each listed field, require `<operation>/<tag>/alter-<field>` as a rejecting case whose `negative_of` is `<operation>/<tag>/base` and whose `mutation.semantic_field` is exactly that field. The operation is `aead/decrypt` for the six AEAD tags and `rsa-label/verify` for the capsule tag. Require the corresponding accepting operation base case, plus `aead/encrypt/<tag>/base` for every AEAD tag and `rsa-label/derive/<tag>/base` for the capsule. The AEAD `format` is the tag's fixed mapping above. An altered field with another valid value rejects at `authenticate/aad_mismatch` (or `authenticate/invalid_label` for the capsule). A field marked `*` has no second valid v1 value: substitute an invalid value and reject at `validate/invalid_field`; the source test checks that the case changes no other semantic field. Context encoder and parser cases for these fields separately prove the closed enum and shape rules.

| Tag | Required altered fields |
|---|---|
| `message-v1` | `realm`, `scope`, `scope_id`, `message_id`, `sender_id`, `key_epoch`, `content_kind*` |
| `blob-v1` | `realm`, `account_id`, `blob_purpose`, `schema_version*` |
| `account-ek-wrap-v1` | `realm`, `account_id`, `key_purpose*`, `account_key_generation`, `credential_generation` |
| `recovery-wrap-v1` | `realm`, `account_id`, `key_purpose*`, `recovery_purpose*`, `recovery_key_generation`, `account_key_generation` |
| `recovery-prefs-wrap-v1` | `realm`, `account_id`, `key_purpose*`, `recovery_purpose*`, `recovery_key_generation`, `credential_generation` |
| `attachment-v4` | `realm`, `scope`, `scope_id`, `file_id`, `uploader_id`, `key_epoch`, `file_nonce_b64`, `flags*`, `chunk_index`, `total_chunks`, `chunk_size*` |
| `channel-key-capsule-v1` | `realm`, `scope`, `scope_id`, `key_epoch`, `recipient_id`, `recipient_key_generation`, `distributor_id`, `distributor_root_digest_b64` |

In addition to those generated names, the following names and mappings are mandatory. Each `reject` row has a complete negative input and a same-operation positive parent. `accept` policy rows have no mutation link. The source test pins each row's named input predicate and expected output decision, not only its name.

| Required name or name expansion | Suite / operation | Required outcome |
|---|---|---|
| `credential/password-scalars/invalid-surrogate` | `credential` / `password-scalars` | reject `validate/invalid_unicode` before Argon2 |
| `credential/derive/max-generation` | `credential` / `derive` | accept generation `9007199254740991` and its eight-byte big-endian encoding |
| `credential/descriptor/weak-parameters` | `credential` / `descriptor` | reject `validate/weak_parameters` before KDF work |
| `credential/descriptor/wrong-realm` | `credential` / `descriptor` | reject `validate/wrong_realm` |
| `context/encode/blob-v1/<purpose>` for each of `preferences`, `saved_gifs`, `friend_organization`, `presence_overrides` | `context` / `encode` | accept exactly the matching `(blob_purpose, schema_version=1)` pair |
| `context/encode/<tag>/wrong-key-purpose` for `account-ek-wrap-v1`, `recovery-wrap-v1`, `recovery-prefs-wrap-v1` | `context` / `encode` | reject `validate/invalid_purpose` using another tag's purpose |
| `aead/encrypt/<tag>/<length-case>` for every AEAD tag and `length-case` in `short-key`, `long-key`, `short-nonce`, `long-nonce` | `aead` / `encrypt` | reject `validate/invalid_field` before AEAD work; mutate only the 32-byte key or 12-byte nonce named by the case |
| `aead/decrypt/<tag>/<length-case>` for every AEAD tag and `length-case` in `short-key`, `long-key` | `aead` / `decrypt` | reject `validate/invalid_field` before AEAD work |
| `aead/decrypt/<tag>/<length-case>` for every AEAD tag and `length-case` in `short-nonce`, `long-nonce` | `aead` / `decrypt` | reject an envelope made with the named wrong nonce length; `short-nonce` uses empty plaintext and rejects at `parse/bad_envelope`, while `long-nonce` rejects at `authenticate/bad_tag` for text or `parse/bad_envelope` for v4 geometry; never pass a non-12-byte IV to AES-GCM |
| `aead/decrypt/<tag>/wrong-format` for every AEAD tag above | `aead` / `decrypt` | reject `validate/invalid_format_context` before AEAD work |
| `aead/decrypt/attachment-v4/tamper-header` | `aead` / `decrypt` | reject `authenticate/aad_mismatch` after changing one authenticated v4 header byte |
| `policy/descriptor-uniformity/<class>` for `split`, `legacy`, `unknown` | `policy` / `descriptor-uniformity` | accept with identical five outputs, fixed descriptor shape, and no direct fixture-UUID leakage |
| `policy/legacy-continuation/<case>` for `eligible`, `forged-split`, `stale-post-upgrade`, `wrong-account`, `wrong-realm`, `expired-proof`, `fresh-device-no-proof` | `policy` / `legacy-continuation` | accept with the exact input predicate and output pair below |
| `policy/upgrade-outcome/<case>` for `fresh-authorization`, `replay`, `request-conflict`, `mfa-required`, `ambiguous-commit`, `expired-authorization`, `missing-authorization` | `policy` / `upgrade-outcome` | accept with the exact input predicate and output pair below |

For the legacy-continuation matrix, start with `device_state:"existing"`, `account_protocol:"legacy"`, `authority_proof:"verified"`, both matches true, `freshness:"current"`, `not_expired:true`, and `control_plane_continuation:true`. Each row changes only the listed predicate; the output pair is exact. `forged-split` deliberately retains a control-plane continuation, which cannot override the split account's protocol. `fresh-device-no-proof` has no independently authenticated proof or trusted cached state.

| Case | Changed predicate | `decision` / `reason` |
|---|---|---|
| `eligible` | none | `submit-raw-password` / `eligible` |
| `forged-split` | `account_protocol:"split"` | `deny` / `not-legacy` |
| `stale-post-upgrade` | `freshness:"stale"` | `deny` / `stale-authority-proof` |
| `wrong-account` | `account_match:false` | `deny` / `wrong-account` |
| `wrong-realm` | `realm_match:false` | `deny` / `wrong-realm` |
| `expired-proof` | `not_expired:false` | `deny` / `expired-proof` |
| `fresh-device-no-proof` | `device_state:"fresh"`, `authority_proof:"absent"` | `deny` / `missing-authority-proof` |

For the upgrade-outcome matrix, start with `prior_state:"legacy"`, `authorization_state:"fresh"`, `request_match:true`, `mfa_complete:true`, and `commit_state:"none"`. The changed predicates and output pairs below are exact. `request_match` models the server-keyed request fingerprint; an ambiguous commit requires durable reconciliation before another attempt.

| Case | Changed predicate | `decision` / `reason` |
|---|---|---|
| `fresh-authorization` | none | `commit` / `fresh-authorization` |
| `replay` | `authorization_state:"consumed"`, `commit_state:"committed"` | `idempotent` / `replay` |
| `request-conflict` | `authorization_state:"consumed"`, `request_match:false`, `commit_state:"committed"` | `conflict` / `request-conflict` |
| `mfa-required` | `mfa_complete:false` | `deny` / `mfa-required` |
| `ambiguous-commit` | `commit_state:"ambiguous"` | `reconcile` / `ambiguous-commit` |
| `expired-authorization` | `authorization_state:"expired"` | `deny` / `expired-authorization` |
| `missing-authorization` | `authorization_state:"absent"` | `deny` / `missing-authorization` |

## Consumer contract

Go, shared TypeScript, Desktop, Swift, and Kotlin each load the same byte-identical corpus and assert the hardcoded whole-file SHA-384 before parsing. Source-repository generator/hygiene tests enforce the lexical byte grammar, object key order, and duplicate-key ban; downstream adapters enforce the semantic schema and compare the stored pins. Each adapter recomputes positive cryptographic outputs with its actual platform primitive or a test-only adapter using that primitive, and rejects negative inputs at the declared stage/category. Production writer/reader conformance is a later activation gate. No adapter may mark a case passed solely because the generator's expected bytes are self-consistent. The Kotlin pass runs against the real Android/JNI crypto path on an emulator; a fake JVM KDF does not count.

The generator has a `--check` mode that regenerates the entire file in memory, compares exact bytes, and verifies both embedded hashes. CI checks the committed file; it never rewrites it. Final corpus and generator pins, all five language verdicts, and security-owner credential-parameter signoff are necessary but not sufficient for ADR-0051 acceptance: independent authority-attestation signer/freshness, channel-distribution proof/receipt, and client-cutover gates remain.
