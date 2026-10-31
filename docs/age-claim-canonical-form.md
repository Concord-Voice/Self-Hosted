# Age-Claim Canonical Signing Form v1 (the A↔B contract)

**Issue:** #1623 (child A of epic #272) · **ADR:** ADR-0025 · **Spec:** `2026-06-18-1623-age-verification-server-design.md` §4.2

This document is the **byte-exact contract** for the message that the client (child B,
WebCrypto) signs and the control-plane (Go) verifies. The server never parses an
attacker-supplied string — it **reconstructs** these bytes from the typed, authenticated
request fields and verifies the RSA-PSS signature against them.

The canonical Go implementation is `Claim.CanonicalBytes()` in
[`services/control-plane/internal/age/canonical.go`](../services/control-plane/internal/age/canonical.go).
The committed cross-impl fixture is
[`services/control-plane/internal/age/testdata/age-claim-canonical-v1.json`](../services/control-plane/internal/age/testdata/age-claim-canonical-v1.json).

## The signed bytes

Fixed-field, **ASCII-only**, joined by a single `\n` (U+000A), **no trailing newline**:

```
age-claim/v1
canonical_version=<int; MUST equal the v1 prefix's version, else reject>
user_id=<uuid lowercase RFC4122>
valid_age=<true|false>
nsfw_auth=<true|false>
jurisdiction_obligation=<int 0..2, decimal, no leading zeros>
nonce=<lowercase hex, exactly 64 chars = 32 bytes>
timestamp=<int, unix seconds, no leading zeros>
key_version=<int, decimal>
client_version=<charset [0-9A-Za-z.+\-], max 32 chars>
```

## Hard contract terms

- **`canonical_version` is inside the signed bytes.** The server selects which
  reconstruction to verify against **solely** from this signed field, and rejects a claim
  that disagrees with the `age-claim/v1` prefix. This authenticates the version and blocks
  a v1↔v2 downgrade. There is exactly one version today: `1`.
- **`user_id` is the authenticated user**, not a body field. The client signs over its
  own user id. The server reconstructs with `c.Get("user_id")`. A claim signed for X
  cannot be replayed by an authenticated Y (the reconstructed `user_id` differs →
  signature fails).
- **Charset/length are enforced.** Every string field is ASCII-only with a charset that
  excludes `\n` and `=`-ambiguity. The bounds are:
    - `nonce` — exactly 64 lowercase-hex chars
    - `client_version` — 1..32 chars from `[0-9A-Za-z.+\-]`
    - `jurisdiction_obligation` — in `[0,2]`
    - `timestamp` and `key_version` — positive

  Any out-of-charset byte (incl. embedded NUL) → HTTP `400`. **No Unicode
  normalization** (ASCII only).
- **Signature algorithm: RSA-PSS over SHA-256.** The signature is base64
  (`signature` field in the request body).
- **PSS salt length is pinned to 32** (= SHA-256 length) on **both** sides:
  `rsa.PSSSaltLengthEqualsHash` (Go) ⟷ `saltLength: 32` (WebCrypto). **`PSSSaltLengthAuto`
  (max salt on a 4096-bit key) is rejected by the verifier** — a verify-side test locks
  this. Child B MUST use salt length 32 or every claim fails.
- **Key:** the signature is verified against the user's **current** `public_keys` row
  (`ORDER BY key_version DESC LIMIT 1`), an RSA-4096 modulus (`BitLen() ≥ 4096`). A key
  rotation (including account recovery, which bumps `key_version`) invalidates outstanding
  claims — the client must re-sign under the current version.

## Child-B signing-key path (delivered in #1624)

The per-user key in `public_keys` is registered as an **RSA-OAEP encryption** key — the
client cannot `crypto.subtle.sign()` with it as-is. Child B (#1624) provides the RSA-PSS
`sign` path over the existing device key: `unwrapSigningKey` (`client/desktop/src/renderer/
utils/crypto/crypto.ts`) re-unwraps the same wrapped PKCS8 as a **non-extractable** RSA-PSS
`['sign']` handle, surfaced via `e2eeService.deriveSigningKey()` / `signAgeClaim()` (no new
keypair is minted). See the `.claude/rules/e2ee.md` "Age-claim signing — device-key reuse
for RSA-PSS" note for the full pattern. The **server side (child A) is version-agnostic to
how B gets the signing capability**. A verifies a PSS signature over the same modulus
regardless, and tests fully against fixtures signed with a test keypair (see the
fixture).

## Modulus-reuse caveat

The age signature shares the E2EE key's modulus, so it provides **no assurance independent
of device-key custody** — it is exactly as trustworthy as the E2EE key, not a second
factor. Downstream children (C/D) must not market it as one.

## Request / response

`PUT /api/v1/age/claim` (authenticated, verified-email, rate-limited 5/min). The body carries
only the signed-core fields + the base64 `signature`. `user_id` is **never** in the body,
and child A **does not accept** the forward-compat metadata columns. See spec §4.3 for
the full request shape and the `error_code` taxonomy.
