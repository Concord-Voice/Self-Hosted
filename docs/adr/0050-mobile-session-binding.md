# ADR-0050: Mobile session binding — Secure Enclave P-256 for device-possession signatures only

**Status:** Accepted (Mark, 2026-10-30). The decision is in force; the proof itself (D) is not built yet.
**Spec:** mobile refresh-token design panel, 2026-10-30 | **Related:** ADR-0010 (client attestation) D17, [ADR-0047](https://github.com/Concord-Voice/Mobile/blob/main/docs/adr/0047-mobile-client-framework-and-native-boundaries.md)

## Context

The iOS app stores its rotating refresh token in the Keychain as `AfterFirstUnlockThisDeviceOnly`, so background work (CallKit, APNs token re-registration, sync) can read it while the phone is locked. A Face ID seal on the token ([Mobile#11](https://github.com/Concord-Voice/Mobile/pull/11)) was reverted because it blocked every locked background path. That leaves a token that can be copied off a seized phone or by malware and replayed elsewhere (threats T1 and T3 in the spec).

The fix (option D) binds a session to a key that cannot leave the phone: each refresh carries a DPoP-shaped proof signed by a Secure Enclave key. The Secure Enclave supports only NIST P-256. The project minimum is P-384 for key agreement (the E2EE crypto rule), so D needs a recorded exception.

## Decision

1. **P-256 is allowed for exactly one use:** ECDSA (ES256) signatures by a hardware-bound, non-exportable key proving that a request comes from the device that holds it. On iOS that key lives in the Secure Enclave. App Attest (ADR-0010 D17) is the precedent: Apple's attestation keys are P-256 for the same reason.
2. **P-256 is never used for encryption or key agreement.** No ECDH, ECIES or key wrapping on P-256, hardware-backed or not. Message, media and identity-key cryptography stay on AES-256-GCM, RSA-OAEP 4096 and P-384+. A software P-256 key is not covered by this exception.
3. **One key per server and sign-in.** The app generates a fresh Secure Enclave key at sign-in for each server. It is deleted on sign-out and never reused across servers, so a proof cannot be replayed against another origin even though the server checks only the path.
4. **The proof authenticates possession, not identity.** The refresh token and the user's credentials remain the authentication. A missing or invalid proof on a bound session is a 401 that leaves the session live; it never triggers the machine-ID theft path.
5. **Binding is one-way.** An unbound session may become bound at a refresh; a bound session never becomes unbound.

## Consequences

- The P-384 rule in the E2EE crypto rule file gains a pointer to this ADR. Reviewers should treat any other P-256 use as a violation.
- Bound sessions get window-free replay recovery (spec §2 item 2), since a valid Secure Enclave proof is stronger evidence than IP and User-Agent.
- **Open risk U2:** App Attest in a locked background launch is unverified. It must be shown to work before `REQUIRE_CLIENT_ATTESTATION` is turned on, independent of D.
- **Rollback floor:** once the D server release ships, the control plane may not be rolled back below it, because bound rows would lose their enforcement. The D PR's release notes record the floor for operators.
- Android, when built, uses StrongBox or TEE keys under the same rule: signatures only, hardware-bound only.

## Rejected alternatives

- **Software P-384 key in the Keychain.** Meets the curve rule but can be copied with the token, so it defeats the purpose.
- **ECIES wrap of the token with a Secure Enclave key (option H).** Uses ECDH on P-256, which breaks the key-agreement rule outright, and adds nothing once D exists.
- **App Attest assertions on every refresh (option E).** Apple does not support App Attest key generation in app extensions, and development builds could not refresh.
