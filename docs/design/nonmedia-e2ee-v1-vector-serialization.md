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
| `policy` / `descriptor-uniformity` | `account_class`, `descriptor_b64`, `proof_verdict` | `descriptor_shape`, `failed_proof_status`, `failed_proof_shape`, `failed_proof_error_class`, `attempt_charge` |
| `policy` / `legacy-continuation` | `account_protocol`, `authority_proof`, `realm_match`, `account_match`, `freshness`, `not_expired`, `control_plane_continuation` | `decision`, `reason` |
| `policy` / `upgrade-outcome` | `prior_state`, `authorization_state`, `request_match`, `mfa_complete`, `commit_state` | `decision`, `reason` |

`format` is one of `m1`, `b1`, `ek1`, `rw1-private`, `rw1-preferences`, `attachment-v4`; the two `rw1` values are typed test operations that share the same on-wire prefix. `envelope_b64` is base64 of the complete wire representation: the ASCII prefix plus encoded payload for text formats, or the complete v4 header and encrypted chunk for attachments. `rsa-label` implicitly and exclusively uses `channel-key-capsule-v1` as its tag. `account_class` is `split`, `legacy`, or `unknown`; `proof_verdict` is `wrong`, the only pre-MFA outcome this uniformity operation covers. Its `descriptor_shape`, `failed_proof_shape`, and `failed_proof_error_class` are stable ASCII identifiers, `failed_proof_status` is an HTTP status integer, and `attempt_charge` is an integer count. The corpus must have one case for each account class and require all five outputs to be equal across those cases; descriptor **bytes** may differ but must share the ADR's size, field, and parameter shape and reveal no account UUID. This test-only comparison does not claim live handler timing equivalence, which remains an activation test. `account_protocol` is `legacy` or `split`; `authority_proof` is `verified`, `absent`, or `invalid`; `freshness` is `current` or `stale`; `prior_state` is `legacy` or `split`; `authorization_state` is `fresh`, `consumed`, `expired`, or `absent`; `commit_state` is `none`, `committed`, or `ambiguous`. Boolean fields are JSON `true` or `false`. Policy decisions are `submit-raw-password` or `deny` for legacy continuation and `commit`, `idempotent`, `conflict`, `deny`, or `reconcile` for upgrade outcome. A valid policy evaluation that denies an operation is an **accepted** vector with `outputs.decision:"deny"`, not a rejected vector. `outputs.reason` is a closed policy reason: `eligible`, `not-legacy`, `missing-authority-proof`, `invalid-authority-proof`, `stale-authority-proof`, `wrong-account`, `wrong-realm`, `expired-proof`, `no-continuation`, `fresh-authorization`, `replay`, `request-conflict`, `mfa-required`, `ambiguous-commit`, `expired-authorization`, `missing-authorization`. In `upgrade-outcome`, an expired authorization maps to `deny/expired-authorization` and an absent one to `deny/missing-authorization`; neither is an authority-proof failure. `commit_state:"ambiguous"` maps to `decision:"reconcile"` and `reason:"ambiguous-commit"`; it never authorizes a blind retry. These policy cases describe abstract decisions only; they do not imply a frozen legacy-attestation or upgrade-request wire format. Required policy matrices are pinned by case name; they do not use `negative_of` or `mutation`.

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
| `aead.decrypt` | `parse/bad_envelope`, `validate/invalid_field`, `validate/invalid_format_context`, `authenticate/aad_mismatch` |
| `rsa-label.derive` | `validate/invalid_field` |
| `rsa-label.verify` | `validate/invalid_field`, `authenticate/invalid_label` |
| All policy operations | No rejected cases; malformed policy input is a corpus schema error. |

Tests compare these categories, not platform error messages.

Every rejecting case contains the complete input it actually consumes. `negative_of` names a positive vector in the same suite and operation. `mutation` has exactly `{"kind":"replace|remove|insert|reorder|truncate","path":"/input/...","semantic_field":"..."}`; `path` is an RFC 6901 JSON Pointer from the vector root. For `remove`, it resolves against the positive parent; for `insert`, against the negative case; otherwise against both. `semantic_field` names the one ADR bound field or operation input changed, including a field carried inside base64 candidate bytes. The test never executes a mutation engine: it evaluates the supplied complete `input`. A source-repository coverage test decodes candidate bytes as needed and checks that the declared mutation changes exactly that one semantic field from the named positive case. Policy matrices use named complete cases instead of mutation links, because deny, conflict, and reconciliation are successful policy evaluations.

The generator is deterministic and never reads time, randomness, locale, or environment to choose case bytes. Synthetic keys, salts, nonces, UUIDs, and passwords are public test inputs. A fixed nonce in the corpus is only a test vector; no production API accepts caller-supplied nonces. RSA coverage pins the exact capsule context and OAEP label bytes; it does not include private keys or randomized OAEP ciphertext. The policy suite may express abstract legacy-attestation validity and upgrade state, but it cannot claim byte-level attestation or CAK/distributor signature conformance before those wire formats are separately frozen.

## Consumer contract

Go, shared TypeScript, Desktop, Swift, and Kotlin each load the same byte-identical corpus and assert the hardcoded whole-file SHA-384 before parsing. Source-repository generator/hygiene tests enforce the lexical byte grammar, object key order, and duplicate-key ban; downstream adapters enforce the semantic schema and compare the stored pins. Each adapter recomputes positive cryptographic outputs with its actual platform primitive or a test-only adapter using that primitive, and rejects negative inputs at the declared stage/category. Production writer/reader conformance is a later activation gate. No adapter may mark a case passed solely because the generator's expected bytes are self-consistent. The Kotlin pass runs against the real Android/JNI crypto path on an emulator; a fake JVM KDF does not count.

The generator has a `--check` mode that regenerates the entire file in memory, compares exact bytes, and verifies both embedded hashes. CI checks the committed file; it never rewrites it. Final corpus and generator pins, all five language verdicts, and security-owner credential-parameter signoff are necessary but not sufficient for ADR-0051 acceptance: independent authority-attestation signer/freshness, channel-distribution proof/receipt, and client-cutover gates remain.
