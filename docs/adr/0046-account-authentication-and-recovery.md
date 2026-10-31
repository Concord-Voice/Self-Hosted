# 0046 — Account authentication and recovery: seniority, not supremacy

**Status:** Proposed. The lead answered the Phase-2 gate on 2026-10-07 (spec §9.5). It becomes Accepted with C11 (action item 5), after #3587 PR-A freezes Δ1, Δ9 and Δ13.
**Created:** 2026-10-07.
**Decision owner:** project lead. Design panel: architect, system-engineer, platform-engineer, frontend-designer.
**Scope:** #3662 (parent #696). Couples to the #2553 account-key authority (ADR-0045, not published in this repository).
**Design:** `docs/superpowers/specs/2026-10-07-3662-account-auth-recovery-design.md`. This ADR records the decisions; the spec holds the rules, the table and the evidence.

## Context

Concord treats every credential as interchangeable, and the weakest can rebuild the rest. One emailed 6-digit code mints a 25-hour recovery token with no credential epoch, and on an MFA account that token resets the password without asking for MFA. The password alone removes security keys; on an MFA account the password alone, or the MFA code alone, signs every session out. No session or credential outranks another, and nothing waits. In the other direction, honest owners are trapped: a lost authenticator is permanent, the backup email does nothing, the primary email cannot change, and both key-recovery paths are unusable. The desktop stores the password-derived wrapping key beside the key it wraps.

**Current implementation note (2026-10-15):** the ad-hoc `recovery-only-challenge` patch closes this immediate recovery-policy bypass (Codex Security finding `csf_13551019f403b6ab218b09ef`) on this branch, pending deployment. A recovery email is atomically consumed and, where current raw policy reserves an actually enrolled TOTP or WebAuthn factor, a distinct five-minute recovery challenge must prove one before final recovery authority is issued. The final `recovery_v2` token carries signed server-verified factor provenance and every reset, device, and social-recovery consumer rechecks current policy. The patch does not implement this ADR's broader proposed hardening: universal MFA accounts, the 15-minute recovery-token TTL, credential-epoch recovery binding, recovery-key authority, or the seniority/hold model remain proposed work.

#2553 assumes CP user authentication is sound (ADR-0045 decision-table row D21, `0045:672`) and freezes its protocol-v1 contract at #3587 PR-A, so any factor this design needs must be named first.

Two hard constraints pull against each other. An attacker who changed the password or email must not lock the owner out or stop the owner's recovery. And an intimate-partner abuser who knows the old password or shares the mailbox must be cut off quickly, with no delay that shields the abuser. When owner and attacker hold the same credentials, no rule over credentials alone can separate them: the two cases are mirror images.

## Decision

1. **Access is fast; capability is slow unless outranked.** Anyone who meets the login floor (DoR H) may at once sign every other session out (with an epoch rotation) and rotate the password. Removing a factor, address or recovery channel is immediate only for a strictly stronger proof; otherwise it is a request that completes after Δ_cp (72 h) unless a credential of at least the opener's class cancels it.
2. **Seniority, not supremacy.** Every new credential counts for nothing until it matures (72 h). Maturity governs rank, never sign-in. A credential never acts on a request opened before it matured. Requests are bound to neither the epoch nor their origin credential.
3. **At the CP, one class per proof set, and paper is not stronger than possession.** C1: one knowledge or channel credential, a backup code, or one anchor. C2: knowledge + possession, a non-paper possession factor alone, an anchor + knowledge, an anchor + a backup code, or two anchors. C3: an anchor + a mature, non-backup-eligible possession factor. The same class applies to opening and cancelling. The one exception is decision 5's recovery key at the authority.
4. **Ties are a state.** Conflicting requests, and a reopen within 30 days of a cancel by an equal class, go to `disputed`, which never completes on its own. It exits by a C3 proof, or expires after 30 days to no change, because no support path exists at GA (decision 9).
5. **Renames and verdicts.** The lead's "Bastion Host" becomes the opt-in **Recovery device** (`keeper`): an anchor credential (a P-384 main-process key, password-wrapped, unlocked at approval), never an irrevocable session. An account may hold up to 10 (#2553 MAX_DEVICES). A **matured** one (past Δ_cp at the CP, uncontested, and mature at the authority) can lift a user suspend from its Devices list (#2553 LD-40, lead 2026-10-07); a freshly designated one cannot, so a phisher cannot designate a device and undo the owner's panic suspend. The "master recovery passkey" becomes the **Recovery key**. It is a burn-on-use 256-bit offline secret whose P-384 public key is **bound at the authority** (Option B). With a password or address code from the last 30 days, it runs `recover`, which replaces the root and resets the CP account after a hold of Δ + G (72 h 15 min) that **only the key itself can cancel**. Alone it does nothing at the authority. It escrows the EK; no root is ever escrowed. Binding it, or replacing or removing it (unbind, lead 2026-10-08) without the old sheet, waits 30 d, 72 h or nothing with one, two or three mature factors (#2553 LD-41, LD-43; the password and SSO count once together, LD-48; addresses count by mailbox, and one the SSO provider hosts counts as SSO, LD-51; on an MFA account the immediate tier needs a mature real MFA method, LD-50). A pending replacement is cancelled by the sheet plus the knowledge leg, and only a password or address that matured before it was retired counts toward that leg (LD-44). Every saved copy (paper, file, image or password manager) carries the binding proof (LD-42, LD-45). Backup email becomes verified, maturing, and removable at once by a stronger proof; no long-lived key is ever emailed. Passkeys become a UV second factor; passwordless accounts are deferred.
6. **MFA at sign-up (lead, 2026-10-07).** Every account created after C12 enrols a possession factor before it is usable; legacy accounts are exempt. Email verification stays a login convenience that never counts as a factor here, and its target may be a mature verified backup address. A verified address belongs to at most one account.
7. **Fix the live takeover now.** Recovery tokens carry the credential epoch and live 15 minutes; MFA accounts need an inline factor to recover; revocation from a session needs the login floor (the one sessionless exception: a mature authenticator alone may sign every session out, changing nothing else); Recovery Circle share serving is paused. The Circle is kept and fixed, so that a contact sees a share only after a 72 h request the owner could cancel.
8. **#2553 integration.** Before PR-A merges:
   - reserve the `cp-evidence` factor values (Δ1: four, plus `email-backup` from #2553 LD-41);
   - add the recovery-key ops `bind_recovery` and `recover`, a `recovery-key` signature role and the bound-key positions (Δ9);
   - cap `recover`'s assurance by the segment where the key was bound (Δ13).

   D-152 is unchanged: the owner's own escape is `rotate_root`, preceded in the same CP transaction by the CP cut-off. `revoke_root` stays operator-only. Two Δ windows run in sequence (CP, then authority).
9. **No human support at GA (lead, 2026-10-07).** No operator resolves a tie. `disputed` resolves only by a strong proof, or expires after 30 days to no change, and a held request cancels at its cap. #2553's operator exits are unavailable.
10. **Boundary.** These CP rules are **not** a #2553 boundary control. A CP-database writer can forge maturity, requests and notices; DoR §7 is the whole claim against it.

## Options considered

| Option | Summary | Why not chosen |
|---|---|---|
| A — ranks by seniority at the CP; a recovery key only at the CP | Smaller PR-A | **Rejected at the gate.** With no operator path, a thief who holds the root and meets S keeps the encryption identity permanently. |
| **B — A's CP ranks, plus a recovery key that outranks the root at the authority** | **Chosen** (lead, 2026-10-07) | Cost accepted: Class-F additions on PR-A's critical path; an abuser who finds the sheet and knows a recent password beats a victim holding the device. |
| C — a uniform delay with no ranks | Smallest build | An IPV victim with a strong factor cannot cut a shared mailbox at once. **Rejected at the gate** (the lead chose B). |

## Consequences

- **Easier:** an IPV victim holding their own factor cuts an abuser off in one ceremony; a takeover cannot remove a mature possession factor or stop the owner's break-glass; lost MFA has an exit for the first time.
- **Harder:** someone who finds the recovery-key sheet and knows a password from the last 30 days takes the account, and a device holder cannot stop it (accepted with Option B); an owner who has lost everything but the mailbox waits about six days and ends `unverified` at the authority; on a no-MFA account a credential stuffer keeps access until the owner's break-glass completes (new accounts cannot be no-MFA: they enrol a possession factor at sign-up, spec §6.16); with no support path, a genuine tie reverts to no change after 30 days, so a *prepared* stuffer on a **legacy** account with no MFA and no bound recovery key keeps it (the IPV mirror; spec LQ-5); a griefer who keeps a guessable cancel budget locked can cancel a request at its cap, but only one that no mature non-guessable credential could cancel: a passkey protects requests opened at C1 or C2, a recovery device only C1-origin ones (spec LQ-6, §6.2); a thief who counts three mature factors replaces a bound recovery key at once (#2553 LD-41), a narrowing of Option B's rescue the lead accepted as a residual (#2553 LD-47); a stolen copy of the sheet alone cancels the owner's pending `recover` and spends its budgets, so an owner holding only another copy and no root has no exit (spec §6.12).
- **Scope boundary (lead, 2026-10-08; #2553 LD-55).** An adversary holding three or more of the owner's independent ways in (the mailbox, the password, an MFA method or backup codes, the recovery key, a root-holding device) is out of scope. Residuals that need that many get no mechanism; adversaries holding one or two stay in scope. Option B, decided before the boundary, stays as designed; nothing further is built for such an adversary (lead, 2026-10-08).
- **Delivery becomes a security control.** Notices go through a durable outbox, and completion holds while they are undelivered.
- **To revisit:** passwordless accounts (needs a floor branch and PRF); Touch ID for the recovery device (needs a signed-build entitlements spike); a hardware security key used only for recovery.

## Action items

1. [x] Lead answers Tier 1 of the spec's §9 before #3587 PR-A merges (2026-10-07, spec §9.5).
2. [ ] C1 lands Δ1, Δ2, Δ3, Δ5, Δ7 (the §7 wording), Δ9 and Δ13 inside PR-A, and records Δ4, Δ10 and Δ12 there, after a PR-A design pass on Δ9's bytes.
3. [ ] C10a and C2a ship from the gate; C2b ships with C5.
4. [x] Lead answers spec §9.6 (2026-10-07: LQ-5 MFA at sign-up, LQ-6 narrow plus a known-device budget, X6 derive with guardrails, ruled not custom cryptography).
5. [ ] This ADR moves to Accepted with C11.
