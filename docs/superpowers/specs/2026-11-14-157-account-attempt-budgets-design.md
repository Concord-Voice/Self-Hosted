# Security #157: Account attempt budgets

## Intent

Close the seven findings in [Security #157](https://github.com/Concord-Voice/Security/issues/157). A caller must not gain fresh code guesses by changing IP address, starting recovery again, changing a pending registration's email, or choosing a different MFA step-up route. A legitimate user must still be able to distinguish a temporary service fault from an exhausted budget.

## Scope and constraints

- Self-Hosted owns the control-plane implementation. Hosted imports `services/**` and `docs/api/**` from Self-Hosted through its reviewed upstream mirror PR. The two quality gates and merge approvals remain separate.
- Keep request bodies and successful response shapes. Recovery begin remains an account-enumeration-resistant generic `200`.
- Charge a submitted MFA code before factor verification. TOTP disable must first confirm enrollment under its existing SQL lock, so a no-factor request cannot exhaust the shared budget; other routes charge before opening their protected transaction. Keep transactional factor consumption with the protected mutation. Clear a budget only after the relevant proof and protected mutation have committed; a correct password does not clear the MFA budget.
- Preserve the existing `stepup.MFASettingsBudgetPrefix` key spelling, the existing registration pending-ID key, and existing Redis/SQL validation and error handling. Do not add a dependency, migration, environment variable, or new client UI.
- Use an isolated `DATABASE_URL` and `REDIS_URL` for integration tests. Never run destructive tests against the shared `concord` database.

## Approaches

| Approach | Benefit | Cost | Decision |
|---|---|---|---|
| Add `stepup.Charge` at each missing step-up call site, with a production caller census | Reuses the existing fail-closed budget; most routes keep Redis outside SQL transactions | Every direct verifier call needs an explicit classification; TOTP disable charges after its locked enrollment check | **Chosen** |
| Charge inside `VerifyCode` / `VerifyCodeTx` | One apparent enforcement point | `VerifyCodeTx` would call Redis while holding SQL; login MFA would inherit the dangerous-action policy; existing charged callers could double-charge | Rejected |
| Add an opaque reservation-handle type | A stricter helper interface | Broad signature/fake changes while direct verifier calls remain callable | Rejected |
| Lengthen recovery codes and add notifications | Defense in depth | Changes client input and email delivery without closing the reset bug | Deferred |

The issue's default design calls for structural coverage. The census is the guard for this narrower implementation: every production `VerifyCode`, `VerifyCodeTx`, `VerifyMFAFactor`, and `VerifyMFAFactorTx` call must be budgeted or have a named non-step-up exemption. A pattern-only grep that silently misses a call is insufficient.

## MFA step-up

Use `stepup.Charge(ctx, stepup.DangerousActionBudget(redis), userID, mfaCode)` once for every submitted MFA code that reaches factor verification on session revoke, revoke-all, revocation-mode, TOTP disable, backup-code regeneration, password change, E2EE key reset, ownership transfer/reversal, and DM/group purge. A no-TOTP TOTP-disable request reaches no factor verifier and is not charged. Existing guarded routes retain one charge. Route handlers map exhaustion to the shared `429` body and Redis/unwired failure to the shared `503` body; neither outcome reaches the protected mutation. An absent code does not spend the budget. Wrong codes across routes spend the same user budget. A verified committed action clears it; rollback or uncertain commit does not.

When MFA is enrolled, session revoke, revoke-all, and revocation-mode require both password and MFA. Without MFA enrollment, password remains required when the session policy calls for authentication. The MFA code cannot substitute for the password.

## Recovery

Use a separate fixed `recovery_fail:<normalized-email>` budget: 10 failed attempts in 24 hours, independent of source IP and the ten-minute code record. Preserve the existing per-code `Attempts` and WATCH redemption as a second bound. Charge before code comparison; a wrong code retains the charge. A correctly consumed email code clears this failure budget. Exhaustion returns `429` with `Retry-After`; Redis failure returns `503`.

`RecoveryBegin` installs a new code record with `SET NX` and the existing ten-minute TTL. Only the winning request sends an email. A live record remains unchanged, including its `Attempts`, and no second email is sent. An email-send failure conditionally removes only the record installed by that request. Every begin response remains the existing generic `200`. Begin never clears `recovery_fail:`.

## Pending registration

Keep the attempt key attached to the pending registration ID for the pending record's lifetime. Resend and change-email rotate code material without deleting attempt state. `UpdateEmail` preserves `resend_count` and `last_resend_at`; change-email consumes the same row-locked resend cooldown and cap as ordinary resend. Terminal promotion or abandonment may remove the attempt key. Preserve the existing email/code binding; Security #160 separately hardens the final locked promotion comparison.

## API and client behavior

| Result | HTTP | Client contract |
|---|---:|---|
| Missing required factor | 403 | Existing opaque required-factor response; no budget charge |
| Wrong factor with budget remaining | 403 | Existing opaque invalid-factor response |
| Budget exhausted | 429 | `step_up_budget_exhausted: true` and `Retry-After` |
| Budget unavailable | 503 | `step_up_budget_unavailable: true`; no invalid-code claim |

Desktop and Mobile can keep their existing forms. Their API adapters should preserve form state on `503` and use `Retry-After` to pause repeat submissions on `429`; any client-specific copy change belongs in those repositories. The server must not reveal whether a recovery account or active record exists on begin.

## Acceptance criteria

- [ ] Recovery guesses stop at the account limit across repeated begin calls and source IPs; concurrent begins leave one code and one email.
- [ ] A live recovery record retains its code and `Attempts`; failed email cleanup cannot delete a later record.
- [ ] Registration resend and change-email preserve attempts and the durable resend cooldown/cap.
- [ ] Wrong MFA codes on every listed route spend one shared per-user budget; a correct password cannot reset it; success clears only after commit.
- [ ] Every listed route returns `429` on exhaustion and `503` on Redis failure, with the response flags and `Retry-After` contract above.
- [ ] MFA-enrolled session revocation rejects password-only and MFA-only requests.
- [ ] The VULN-016, VULN-015, VULN-024, and VULN-039 exploit reproductions fail against the fixed code and retained regression tests pass.
- [ ] Update `docs/design/authenticated-step-up-budget.md`, ADR-0046, relevant OpenAPI responses, and `CHANGELOG.md` with the code. Both repositories pass their quality gates after the Hosted import.
- [ ] New-code coverage is at least 80%, with zero new blocker/critical issues and zero new high-confidence security hotspots.

## Review focus

Check double charges on already guarded routes, budget clearing before a commit, direct verifier calls omitted from the census, recovery begin/send races, registration rotation using terminal cleanup, and Hosted import conflicts. Security #82 overlaps the shared MFA budget and Security #118 overlaps recovery/session policy; close or rescope them only after comparing their remaining acceptance criteria against merged proof.
