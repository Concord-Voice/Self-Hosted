# Changelog

All notable changes to Concord Voice will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [0.2.0-Beta] — 2026-XX-XX (Phase 2 — Beta release)

> Release-level rollup of Phase 2A + Phase 2B work. Per-revision detail lives in the `[0.1.12]`–`[0.1.18]` entries below; this entry surfaces the user-visible themes that close the v0.2.0-Beta milestone.

### Added

- **Federated identity — Google SSO** ([#808](internal development reference omitted)) — backend OAuth flow for Google sign-in and registration; desktop client integration shipped alongside Apple SSO in [#824](internal development reference omitted) (`client/desktop/src/main/ssoLoopback.ts`, `Login.tsx` / `Register.tsx`).
- **Federated identity — Apple Sign in with Apple** ([#824](internal development reference omitted)) — privacy-relay-aware Apple SSO alongside Google.
- **MFA / WebAuthn authentication** ([#202](internal development reference omitted)) — TOTP, WebAuthn/FIDO2, backup codes, recovery circles, trusted devices; closes #89.
- **Account erasure (GDPR right to be forgotten)** ([#717](internal development reference omitted)) — `POST /api/v1/privacy/erase-account` wired to a transactional account-deletion service that cascades across all linked tables; `refresh_tokens.user_id` cascades atomically.
- **Account recovery** ([#328](internal development reference omitted)) — zero-knowledge key recovery flow.
- **RBAC / SBAC permissions system** ([#242](internal development reference omitted)) — granular role-based access control with audit logging; closes #82. Context menu wired to roles in [#548](internal development reference omitted).
- **Object storage on MinIO** ([#325](internal development reference omitted)) — user image assets migrated from PostgreSQL to S3-compatible storage with two-tier media access; closes #166.
- **Server ownership transfer** ([#351](internal development reference omitted)) — full lifecycle with MFA, email confirmation, reversal tokens; closes #244.
- **Email verification on registration** ([#273](internal development reference omitted)) — SMTP-based verification; later migrated from Proton SMTP to Resend with branded templates and `verify.concordvoice.chat` subdomain.
- **Pending registrations** ([#688](internal development reference omitted)) — registration creates `pending_registrations` with a 15-minute TTL; closes #527 and #621.
- **Chat enhancements (#168 series)** — message reactions ([#459](internal development reference omitted)), reply / quote ([#463](internal development reference omitted)), pinning ([#465](internal development reference omitted)), E2EE-native search ([#468](internal development reference omitted)), file & image attachments ([#470](internal development reference omitted)), draft persistence ([#477](internal development reference omitted)), desktop notifications ([#478](internal development reference omitted)), keyboard shortcuts ([#479](internal development reference omitted)), group DMs ([#472](internal development reference omitted)), extended Markdown rendering ([#711](internal development reference omitted)).
- **Klipy GIF integration** ([#557](internal development reference omitted)) — GIF search, picker, and privacy proxy through the control-plane; theme-aware logos and disclaimers.
- **Server-enforced mute and deafen** ([#546](internal development reference omitted)) — server-side mute/deafen state propagated to the mediasoup SFU; consumers paused/resumed at SFU level.
- **@mention notification routing in E2EE channels** ([#310](internal development reference omitted)) — server-side mention detection without decryption.
- **DM key-epoch enforcement** ([#298](internal development reference omitted)) — key revocation table and epoch checking in the WebSocket path; closes #122.
- **Channel-key rotation on member removal** — E2EE forward secrecy for channel keys; closes #96.
- **Desktop auto-updater** ([#155](internal development reference omitted), [#381](internal development reference omitted)–[#387](internal development reference omitted)) — safe updates with rollback, branded splash screen, fill-progress, error states, position memory, and structured logging.
- **Server-proxied desktop updates** ([#264](internal development reference omitted)) — privacy-first update delivery with no per-client telemetry.
- **SPA deployment pipeline** ([#429](internal development reference omitted)) — file server, versioning, and GitHub Actions workflow for hot-update SPA bundles. SPA deploy contract added in [#773](internal development reference omitted) coupling bundle-hash and handler-path.
- **Bundled-SPA fallback (Option C)** ([#831](internal development reference omitted), [#832](internal development reference omitted), [#835](internal development reference omitted)) — desktop client falls back to the bundled SPA on hot-update failure with the `app://` scheme, an Option C user-facing overlay, and IPC v9.
- **WebSocket reconnect race fix + subscribe-barrier protocol** ([#769](internal development reference omitted)) — closes #752.
- **Self-hosted coturn STUN/TURN** ([#124](internal development reference omitted)) — infrastructure for NAT traversal; cert isolation and `turn.concordvoice.chat` SAN added in [#577](internal development reference omitted).
- **Public Tier-1 media proxy** ([#570](internal development reference omitted)) — unauthenticated access for public media assets.
- **Token theft detection** ([#89](internal development reference omitted)) — machine ID + IP binding with automatic revocation. Sessions capture real client IP via trusted-proxy CIDR allowlist in [#702](internal development reference omitted).
- **Proactive token refresh** ([#329](internal development reference omitted)) — main-process JWT refresh before expiry; closes #240 and #254.
- **Profile and identity asset theming** ([#251](internal development reference omitted), [#252](internal development reference omitted)) — per-user theming, profile cards, DM sidebar cards, avatar theming.
- **Image crop editors for profile and server images** ([#357](internal development reference omitted)).
- **Username restrictions, period support, yearly change cooldown** ([#330](internal development reference omitted)).
- **OS-level permission management** ([#321](internal development reference omitted)) — request, check, and enforce system permissions; closes #197.
- **Friend requests** ([#203](internal development reference omitted)) — accept/decline UI with context menu.
- **Notification sounds** for chat and voice events ([#375](internal development reference omitted), [#394](internal development reference omitted)); per-category sound volumes ([#743](internal development reference omitted)); DM call sounds with looping ([#554](internal development reference omitted)).
- **Developer Mode toggle** ([#567](internal development reference omitted)) — DevTools accessible in Alpha/Beta builds only via the developer mode setting.
- **Code signing — macOS** ([#641](internal development reference omitted)) — Developer ID Application cert wired to sign and notarize macOS builds.
- **Code signing — Windows** ([#649](internal development reference omitted)) — Microsoft Artifact Signing for `Setup.exe`; closes #404.
- **Docker network segmentation** ([#442](internal development reference omitted)) — service isolation, request-ID propagation, Redis auth bans.
- **CI/CD pipeline** ([#128](internal development reference omitted), [#130](internal development reference omitted)) — GitHub Actions `build.yml` with parallel test, coverage, and SonarQube; later hardened with Semgrep SAST in [#457](internal development reference omitted).
- **Shai-Hulud 2.0 supply-chain IOC scanner** ([#722](internal development reference omitted)) — closes #715; IOC list refreshed in [#781](internal development reference omitted).
- **AI governance framework** ([#454](internal development reference omitted)–[#458](internal development reference omitted), [#500](internal development reference omitted)–[#522](internal development reference omitted)) — AI-generated code policy, CODEOWNERS, agentic controls, Semgrep SAST, path-scoped [internal] rules, Claude Code skills, custom agents, Copilot prompt templates, MCP project config.

### Changed

- **React 18 → 19.2.4** ([#181](internal development reference omitted)).
- **react-router-dom 6 → 7.13.1** ([#182](internal development reference omitted)).
- **Zustand 4 → 5.0.12** ([#183](internal development reference omitted)).
- **ESLint → 10 flat config** ([#184](internal development reference omitted), [#185](internal development reference omitted), [#186](internal development reference omitted)).
- **Vite 7 → 8.0.0** ([#287](internal development reference omitted)) plus `@vitejs/plugin-react` 4 → 6.
- **Go 1.24 → 1.26.1** ([#193](internal development reference omitted)) with `govulncheck` hardening.
- **Electron 33 → 41.x**, **mediasoup 3.13 → 3.19.18**, **mediasoup-client → 3.18.7**, **TypeScript 6.0.2**, **typescript-eslint 8.58**.
- **E2EE password-derived key** — PBKDF2 → Argon2id client-side ([#117](internal development reference omitted)).
- **macOS notarization** switched to App Store Connect API key ([#826](internal development reference omitted)).
- **Cognitive complexity reduction** across Go control-plane handlers and TypeScript frontend / media-plane ([#418](internal development reference omitted), [#419](internal development reference omitted), [#498](internal development reference omitted), [#505](internal development reference omitted), [#550](internal development reference omitted)–[#553](internal development reference omitted)).
- **Documentation audit** completed for the v0.2.0-Beta release gate ([#214](internal development reference omitted)) — drift inventory at `[internal]2026-05-01-214-doc-audit-drift-inventory.md`; PR-1 verification merged in [#823](internal development reference omitted).
- **MCP server deployment refactored into per-host configs** ([#838](internal development reference omitted)) — closes #778. Splits into `.mcp.json` (Claude Code CLI / App via `launchctl setenv`) and `.vscode/mcp.json` (VS Code native MCP via `${input:VAR}` → secret store). Eliminates `launchctl setenv` exposure for VS Code native MCP — OWASP A02 (Security Misconfiguration) win. Policy doc (`docs/policies/mcp-server-policy.md`) rewritten with the three-surface credential taxonomy.

### Removed

- **Sentry telemetry** stripped from all three services ([#770](internal development reference omitted) control-plane, [#780](internal development reference omitted) media-plane, [#793](internal development reference omitted) Electron client) plus a closing sweep through MCP, CI, and rules ([#796](internal development reference omitted)). The Sentry MCP server config was finally removed in the per-host MCP cleanup ([#838](internal development reference omitted), closing the MCP-config dimension). Closes #610, #614, #672. The integration that landed earlier in the cycle (#586, #622, #623, #668, #682) was reversed once telemetry surfaced zero production logs and forced re-consent friction; project memory `Sentry — being removed` documents the decision.
- **Postgres MCP server config** removed alongside Sentry MCP ([#838](internal development reference omitted)) — unused operationally; final state is 6 servers in `.mcp.json` + 6 in `.vscode/mcp.json`.
- **Deprecated `WebSocketMessage` source-compat type alias** removed from `client/desktop/src/renderer/services/websocketService.ts` ([#1185](internal development reference omitted)) — the discriminated-union migration ([#709](internal development reference omitted) / PR [#1184](internal development reference omitted)) made the shim unused. `WebSocketEvent` remains the canonical name.

### Fixed

- **E2EE channel-key OperationError** with structured diagnostic envelope ([#765](internal development reference omitted)).
- **E2EE voice / video codec collision** — WebRTC BUNDLE misrouting between consumers ([#291](internal development reference omitted), [#292](internal development reference omitted), [#293](internal development reference omitted)).
- **E2EE Insertable Streams → RTCRtpScriptTransform** migration ([#355](internal development reference omitted)).
- **E2EE key request flood** — session-scoped cache plus CI hardening ([#241](internal development reference omitted)).
- **E2EE frame decryption** recovery and CSK rotation hardening ([#232](internal development reference omitted), [#284](internal development reference omitted)).
- **DM key-epoch enforcement, presence sync, and key distribution** ([#298](internal development reference omitted)).
- **Replied-to message decryption** on REST message fetch (not only WebSocket) ([#542](internal development reference omitted)).
- **DM thread real-time preview and reorder** ([#541](internal development reference omitted)) — closes #486.
- **Message editing** in E2EE channels and DMs ([#220](internal development reference omitted)).
- **Voice and video** — black-screen recovery, codec selection, screen-share audio, audio persistence on navigation ([#198](internal development reference omitted), [#227](internal development reference omitted), [#295](internal development reference omitted), [#299](internal development reference omitted), [#396](internal development reference omitted)).
- **Hub goroutine races** with test cleanup — flaky voice tests resolved ([#476](internal development reference omitted)); deterministic channel-based sync replaced `time.Sleep` in WebSocket hub tests ([#544](internal development reference omitted)).
- **WebSocket reconnect** — connection-lost handler replaced page-reload with direct WS reconnect ([#194](internal development reference omitted), [#439](internal development reference omitted)).
- **Postgres "invalid length of startup packet" flood** ([#779](internal development reference omitted)) — closes #755.
- **PiP child window** loads SPA route, not marketing site ([#815](internal development reference omitted)) — closes #802.
- **PiP window signaling and local user identification** ([#415](internal development reference omitted)).
- **Desktop login on bundled SPA fallback** uses `app://` scheme ([#832](internal development reference omitted)) — closes #830.
- **MFA verify response** parsing — `access_token` extracted from `/mfa/verify` ([#814](internal development reference omitted)).
- **electron-updater trust path** hardening on macOS / Windows / CI ([#655](internal development reference omitted)).
- **Build-desktop CI ASAR integrity** verification ([#686](internal development reference omitted)) — closes #683.
- **Preload bundling with esbuild** for sandbox compatibility ([#678](internal development reference omitted)).
- **MFA encryption key wiring** through Docker Compose ([#225](internal development reference omitted)).
- **Klipy GIF media proxy 401** — webRequest auth injection ([#687](internal development reference omitted)); proxy routes nested under `/gifs` ([#580](internal development reference omitted)).
- **Klipy GIF rendering** — envelope unwrapped on every decrypt path; nested rendition shape parsed correctly ([#566](internal development reference omitted)).
- **NATS server config and coturn TLS / external-IP** ([#576](internal development reference omitted), [#577](internal development reference omitted)).
- **Modal nested Escape handler** firing on all stacked modal instances ([#480](internal development reference omitted)).
- **Server role styling** isolated from DM message rendering ([#543](internal development reference omitted)).
- **Accessibility pass** — semantic HTML, keyboard navigation, ARIA, UI polish ([#380](internal development reference omitted), [#427](internal development reference omitted), [#482](internal development reference omitted)).
- **Theme markdown syntax help modal and help icon** ([#748](internal development reference omitted)).
- **Cloudflare beacon verification** + `/spa/` nginx route + defensive sentinel ([#766](internal development reference omitted)) — closes #750.

### Security

- **`Error.cause` propagation closed** ([#714](internal development reference omitted)) — `console.error` / `console.warn` no longer pass raw `Error` arguments through main-process logs; ESLint enforcement and a Vitest regression test added.
- **Token-fingerprint leaks removed** ([#704](internal development reference omitted)) — 10 token-suffix leaks in `tokenManager.ts` removed; ESLint warnings remediated and security rules promoted to error.
- **External-link scheme tightened** ([#774](internal development reference omitted)) — `setWindowOpenHandler` and `will-navigate` restricted to `https:`-only with ESLint drift defense; user-initiated `open-external` IPC retains the broader `http:` / `https:` / `mailto:` policy. Closes #754.
- **electron-updater TLS certificate pinning** on `api.concordvoice.chat` ([#719](internal development reference omitted)).
- **nginx hardening** — H2C smuggling vector closed; Host header injection blocked ([#525](internal development reference omitted)).
- **GitHub Actions shell injection** resolved (Semgrep) ([#523](internal development reference omitted)).
- **CORS hardening** — null/empty origin rejection, custom header validation ([#259](internal development reference omitted)).
- **Hardcoded dev credentials removed**, production guards added ([#260](internal development reference omitted)).
- **Scanner hardening** — brute-force probe mitigation at infrastructure level ([#153](internal development reference omitted)); production infrastructure hardened against vulnerability scanners ([#189](internal development reference omitted)).
- **Dependabot vulnerability fixes** — npm overrides for transitive vulnerabilities ([#289](internal development reference omitted), [#314](internal development reference omitted)).

### Known Issues

Tracking at time of v0.2.0-Beta release. For the full open-issue list, see [issues](internal development reference omitted).

- **[#817](internal development reference omitted) — "No Internet" Retry button stuck after network restore** — after the desktop client trips the no-internet dialog, the Retry button does not visibly progress once connectivity is back; the user must use Exit App to break out and relaunch. Workaround: quit and relaunch the desktop client once the network is restored.
- **[#807](internal development reference omitted) — Markdown rendering correctness in chat** — H1 renders smaller than H2, fenced code blocks parse incorrectly in some inputs, and vertical spacing is heavier than expected. Cosmetic only; message content is preserved. Workaround: none required for delivery; fix tracked for v0.2.x.
- **[#805](internal development reference omitted) — Pinned Messages panel drops media and shows GIFs as raw JSON** — pinned messages with image attachments lose the image; pinned KLIPY GIFs render as the raw envelope text. The original message in the main chat is unaffected. Workaround: scroll to the source message in the channel for the full rendering.
- **[#804](internal development reference omitted) — KLIPY GIF picker hits 429 during normal scrolling** — the shared rate limiter for the GIF media proxy and the API endpoint is too aggressive when ~30 picker tiles fan out simultaneously. Workaround: pause briefly between scrolls in the GIF picker; the limiter resets quickly.
- **[#803](internal development reference omitted) — Member List shows the viewer as Offline** — in Server Settings → Members, the logged-in user appears as Offline (grey dot, "Status: Offline" in the popover) despite an active WebSocket connection. Other members' presence is unaffected. Workaround: none; the viewer's connection is functional, only the self-status badge is misleading.
- **[#799](internal development reference omitted) — Member List "+ Add Role" dropdown clipped inside the card** — the role-picker dropdown is constrained by its container and adds an inner scrollbar instead of overflowing the card. Workaround: scroll inside the Member List card to access roles below the fold.
- **[#707](internal development reference omitted) — Profile editor loses in-progress edits on background user updates** — `ProfileInfoForm`'s reset effect re-runs on any user object mutation, which can wipe unsaved field edits if the underlying user object refreshes mid-edit. Workaround: save profile changes promptly; avoid leaving the editor open while presence or other user-object events fire.

### Migration from v0.1.0-Alpha

**No end-user breaking changes.** New features (Apple/Google SSO, MFA/WebAuthn, channel-key revocation, server mute/deafen, Klipy GIF integration, DM message pinning, account erasure, bundled-SPA fallback) are additive — existing v0.1.0-Alpha installations continue to work without manual intervention.

**Automatic migrations applied on first connection / first server run:**

- Database schema additions (migrations 000054–000061): server mute/deafen state, Klipy GIF customer IDs, DM message pinning index, pending registrations TTL, account-deletion cascade, removed `sentry_delete_attempted` column, and SSO identities (`is_relay_email` for Apple privacy-relay handling).
- Refresh-token cascade (migration 000059): `refresh_tokens.user_id` becomes `ON DELETE CASCADE`, which strengthens the atomic-revocation invariant for account erasure (per `[internal]rules/backend.md`).
- E2EE key derivation: PBKDF2 → Argon2id ([#117](internal development reference omitted)) migrates transparently on next login — legacy keys are unwrapped with PBKDF2, re-wrapped with Argon2id, and uploaded to the server. No user action required; migration 000034 adds `key_derivation_alg` tracking.
- Renderer migrations: existing local data migrates client-side via the standard Zustand `persist` migration path on first launch; no user-visible state loss.

**Behavior changes that may surprise:**

- **External link policy** ([#774](internal development reference omitted), [#775](internal development reference omitted)): the Electron client's `setWindowOpenHandler` and `will-navigate` now restrict to `https:` only — passive navigation (redirects, programmatic `window.open`) cannot escape to the OS browser for non-`https:` schemes. User-clicked links in `UserProfileModal` and the Markdown pipeline (`SafeLink.tsx`) route through the `open-external` IPC handler, which accepts `{http, https, mailto}` because the explicit click is consent. Legacy `http://` profile links continue to work via this path.
- **Token revocation on password change**: refresh tokens are now atomically revoked when a user changes their password (was a known gap in Alpha). Active sessions on other devices will be logged out on the next refresh attempt. This is the intended behavior; surfaced here because v0.1.0-Alpha did not enforce it.
- **Sentry telemetry removed**: zero behavior change to end users (telemetry was opt-in and zero events were captured in production); flagged for transparency.

**For self-hosted operators:**

- Review the migration set `services/control-plane/migrations/` (000054 onward represents the Alpha→Beta delta) for any deployment-specific actions.
- The MCP server cleanup ([#838](internal development reference omitted)) is dev-environment-only — affects contributor tooling, not production deployment. No action required for self-hosted operators.
- For Apple sign-in support, configure the Apple Sign In credentials per Apple Developer documentation: Team ID, Key ID (with the corresponding `.p8` private key), Services ID, and the loopback redirect URI. Set the corresponding env vars in your control-plane `.env` (see `services/control-plane/internal/oauth/` for the variable names; the source-of-truth is `apple_clientsecret.go`). Without these, the Apple SSO button will be visible but sign-in attempts will fail.
- macOS code signing now uses App Store Connect API key ([#826](internal development reference omitted)) — only relevant if you build your own signed binaries; the public release builds remain Apple Developer ID signed.

---

## [0.1.41] — 2026-05-20 (#806 cross-platform window chrome)

### Added

- **Cross-platform native window controls** — Windows + Linux now show native close / minimize / maximize buttons in the top-right via Electron's `titleBarOverlay` API; macOS retains its native traffic lights via `titleBarStyle: 'hiddenInset'`. The per-platform branching lives in `client/desktop/src/main/browserWindowConfig.ts` ([#806](internal development reference omitted)).
- **Branded Titlebar** — new `<Titlebar />` component renders centered `CONCORD VOICE` in BaronNeue.woff with the running version + active SPA hash (`v0.1.41-abc123`). The version line updates live when an SPA hot-update lands via the new `spa:versionChanged` IPC event.
- **Window state persistence** — size + position + maximized state save to `window-state.json` under `app.getPath('userData')` with 500ms debounce on resize/move and synchronous write on close. Restore validates the saved bounds against the current display layout (4 safety checks: NaN/missing, display intersection, min/max size, negative-coords). Wayland sessions omit x/y per compositor-controlled placement.
- **Client Behavior settings** — new Settings → Appearance subsection lets users assign the `[×]` close and `[—]` minimize buttons to system tray, OS taskbar/dock, or graceful quit. Mutex + coverage rules visualize invalid configs as greyed-out segmented-control cards with explanatory `title=""` tooltips. Dynamic explanation panel reads the current configuration and renders 3 "How do I X" paragraphs.
- **Coordinated-pair sibling [#1099](internal development reference omitted)** — system tray icon. v0.1.41 ships the Client Behavior surface; #1099 ships the tray icon. Both required for the `[X] → tray` and `[-] → tray` paths to be user-visible.

### Changed

- **IPC surface widened** — 4 new `window:*` channels (`setClientBehavior`, `quit`, `setTitleBarOverlayColor`, `getVersionString`) + 1 new send-only event (`spa:versionChanged`). All 4 handlers carry runtime input validators at the IPC trust boundary; sender-frame validation is intentionally omitted per the new "Low-stakes UI-state IPC" exception class codified in [`[internal]rules/electron.md`]([internal]rules/electron.md).
- **PiP windows opt out of OS-drawn shadow** — `hasShadow: false` on PiP `BrowserWindow` construction for the lightweight floating-glass aesthetic. macOS drops the standard window shadow; no-op on Wayland/X11/Windows.
- **Theme-color sync** — settingsStore subscribes to `appearance.theme` and IPC-pushes resolved overlay colors via `window:setTitleBarOverlayColor` on every theme change, including the OS-driven `prefers-color-scheme` listener for `theme: 'system'`. macOS ignores the IPC (uses native traffic lights); other platforms get the dark / light overlay treatment.

### Documentation

- **Developer handoff spec** for the Client Behavior section landed at `[internal]handoffs/2026-05-20-806-client-behavior-section-handoff.md` — first entry in a new `handoffs/` directory paralleling `specs/`, `plans/`, `reports/`.
- **`[internal]rules/electron.md`** documents the low-stakes-IPC sender-frame exception class with conditions, current accepted-exception handlers, and an explicit list of categories that MUST validate.

### Deferred / follow-up

- Plan Task 20 (cross-platform manual verification on Windows 11 + Ubuntu GNOME) requires real hardware. macOS verification will land before merge.

---

## [0.1.40] — 2026-05-20 (Linux build hotfix)

> v0.1.39 was bumped in `package.json` on `main` but never received a GitHub
> Release: the `build-desktop.yml` build matrix had Linux build failures (see
> "Fixed" below), so the `release:` job correctly skipped per ADR-0004
> Invariant 1 (`needs.build.result == 'success'` gate). v0.1.40 carries the
> full v0.1.39 release content plus the Linux build fix.

### Fixed

- **Linux build no longer fails at `@reforged/maker-appimage` packaging** — commit [21accce5](internal development reference omitted) ("Change company name to 'Concord Voice LLC'") silently bundled an unrelated `packagerConfig.executableName` rename from `'concord-voice'` to `'Concord Voice'` alongside its legitimate company-name updates. With executableName changed to `'Concord Voice'`, the packaged Linux binary became `Concord-Voice-linux-<arch>/Concord Voice` (with a space), but the Linux makers' `bin: 'concord-voice'` option still performed a literal-string file lookup for `concord-voice`, failing with `"Could not find executable 'concord-voice' in packaged application"` ([run 26148494680](internal development reference omitted)). The bug sat latent on `main` for ~6 hours while the cascade-skip regression (since-fixed by [#1077](internal development reference omitted)) was still suppressing the build matrix, and surfaced on the first `push:main` after the cascade-skip fix shipped — exactly the verification path documented in [ADR-0004]([internal]0004-desktop-release-contract.md) Invariant 3. Fixed by making `executableName` per-platform: Linux falls back to kebab-case `'concord-voice'` (matching the maker `bin:` lookup and debian-policy §5.6.7), while macOS / Windows retain the proper-name format `'Concord Voice'` (visible in Activity Monitor, Task Manager, and crash reports).

### Changed

- **Test guard for the Linux ↔ display-name asymmetry is now platform-conditional** — `packagingIdentity.test.ts` updates the `executableName` literal-value assertion and the `Linux maker bin intentionally diverges from executableName` asymmetry-guard to branch on `process.platform`. On Linux test runners (CI's ubuntu-latest), the asymmetry guard returns early because the per-platform conditional in `forge.config.ts` makes Linux's `executableName` legitimately equal to `bin`; on macOS/Windows, the guard still asserts the deliberate divergence.

---

## [0.1.39] — 2026-05-20 (Release-pipeline fix + v0.1.36–v0.1.38 catch-up)

> First desktop release published via the workflow since v0.1.34 (2026-05-02).
> v0.1.35 was published manually by the operator on 2026-05-09; v0.1.36, v0.1.37,
> and v0.1.38 were bumped in `package.json` on `main` but never received GitHub
> Releases due to a workflow regression — see "Fixed" below. `gh release list`
> confirms no tags exist for those three versions. v0.1.39 bundles the accumulated
> v0.1.36–v0.1.38 content with the workflow fix that ships it.

### Fixed

- **Desktop release workflow no longer cascade-skips on `push:main`** — PR #889 (merged 2026-05-08) introduced a `pr-paths-filter` job to support PR smoke-testing. That job carries `if: github.event_name == 'pull_request'` and is `skipped` on push events. The downstream `release` job did not opt out of GHA's transitive-skip semantics with `always()`, causing every release-bearing push to main since 2026-05-08 to skip the `Create release` step despite all six platform builds succeeding. Fixed by adding `always()` with explicit `.result == 'success'` checks on the direct upstream needs, plus the original `should_release == 'true'` gate.

### Changed

- **Product display name normalized to "Concord Voice"** (was "ConcordVoice"; commit 72b98f1a) — affects the macOS Dock label, Windows registry display name, and Linux desktop-entry name in the packaged builds.
- **Company name normalized to "Concord Voice LLC"** (commit 21accce5) — License + About-screen attribution; aligns with the Windows Authenticode signer CN already pinned in `eslint.config.mjs` and the Windows signature verification step.
- **Per-target notification mute preferences** ([#985](internal development reference omitted)) — closes [#84](internal development reference omitted); independent mute toggles for messages vs voice on a per-server/per-DM basis.

### Changed (carried over from unshipped v0.1.36–v0.1.38)

- **Backend stops emitting `is_encrypted` field across API and WebSocket envelopes** ([#1042](internal development reference omitted)) — the field is now structural (every room/channel is encrypted under [#201](internal development reference omitted)). Inbound WebSocket envelopes lacking `key_version >= 1` are rejected via close frame 4400 `missing_or_invalid_key_version`. Landed on main in the v0.1.37 bump; first released here.
- **Frontend stops reading `is_encrypted`** ([#1031](internal development reference omitted)) — Child A of the #201 epic; landed on main in the v0.1.36 bump; first released here.
- **Media-plane removes `is_encrypted` field + documents SRTP-mandatory** ([#1032](internal development reference omitted)) — Child D of #201; landed on main in the v0.1.37 bump; first released here.

### Fixed (carried over from unshipped v0.1.36–v0.1.38)

- **WebSocket first-attempt drop is now silent + WS auth ticket redacted from console** ([#1046](internal development reference omitted)) — noisy reconnect log on the very first connection has been quieted; auth ticket no longer surfaces in DevTools console. Landed on main in the v0.1.38 bump; first released here.

---

## [0.1.18] - 2026-04-09 (Phase 2B — Sentry Error Tracking)

### Added

- **Sentry Electron integration** — Error tracking for main and renderer processes with fail-closed privacy model; `beforeSend` scrubber drops key material, PII, and console breadcrumbs before transmission (#586)
- **Docs-reviewer subagent** — Claude Code subagent for automated documentation drift detection with tier classification and `/review-pr` skill dispatch

### Changed

- **Documentation refresh** — AGENTS.md, REVIEW.md, README.md, and docs/architecture.md updated to reflect current Phase 2B state

---

## [0.1.17] - 2026-04-09 (Phase 2B — QA Pass & Infrastructure Hardening)

### Added

- **[internal] counts automation** — `./scripts/update-claude-md-counts.sh` for keeping project stats in sync (#581)

### Changed

- **MCP env var standardization** — Environment variable naming aligned across MCP server configs (#581)

### Fixed

- **KLIPY GIF proxy routes** — Nested `/gifs` path prefix added to match client-side route expectations (#580)
- **Copilot review feedback** — Addressed review comments from PR #577 (#579)
- **coturn TLS hardening** — Certificate isolation, `turn.concordvoice.chat` SAN added, certbot deploy hook wired
- **NATS single-node config** — Corrected NATS configuration; coturn TLS and external-IP wiring fixed

---

## [0.1.16] - 2026-04-08 (Phase 2B — Media Proxy & QA)

### Added

- **Public Tier 1 media proxy** — Unauthenticated access for public media assets (#570)

### Fixed

- **Avatar slot pinning** — Avatar elements pinned to 40×40 px to prevent layout shift (#570)
- **QA bug pass** — Broad regression sweep covering UI, API, and infrastructure issues (#571)

---

## [0.1.15] - 2026-04-07 (Phase 2B — Klipy GIF Integration)

### Added

- **Klipy GIF integration** — GIF search, picker, and privacy proxy through the control-plane; API key injected via environment variable; disclaimers and branding in About section (#483, #557)
- **Developer Mode toggle** — DevTools accessible in Alpha builds only via developer mode setting (#567)

### Changed

- **Dependency bumps** — `dotenv`, `@vitest/coverage-istanbul`, `go-webauthn/webauthn`, `@types/node`, build-tooling group (4 packages), testing group (2 packages) (#559, #560, #561, #564, #565)

### Fixed

- **Windows desktop build** — `matrix.arch` substituted directly in `electron-forge make` command to fix cross-platform CI build (#569)
- **Chat and GIF rendering** — Chat message rendering, GIF display via Klipy, MinIO crop upload, and SPA hot-reload all corrected (#566)
- **Theme-aware Klipy logos** — Logos respond to active color scheme; public/branding layout restructured
- **GIF envelope decryption** — Envelope unwrapped on every decrypt path, not only realtime; nested `file.{hd,md,sm}.<format>` rendition shape parsed correctly
- **SPA CSP headers** — Strict default CSP no longer dropped on every response, only on success paths; SPA bundle directory mounted into control-plane container
- **GIF picker UX** — Picker interaction, settings accessibility regressions, and test assertions updated

---

## [0.1.14] - 2026-04-06 (Phase 2B — Voice Refactors & RBAC Wiring)

### Changed

- **VoiceAudioSection split** — Component decomposed into focused sub-components for maintainability (#553)
- **voiceService complexity reduction** — Remaining high-complexity functions in `voiceService` refactored (#552)
- **E2EE transforms extraction** — E2EE transform and produce helpers extracted to reduce cognitive complexity (#551)
- **Codec cascade extraction** — Codec cascade selection logic extracted into a dedicated helper (#550)

### Fixed

- **RBAC context menu wiring** — Context menu actions wired to RBAC roles; moderation actions (kick, ban, mute) enabled (#548)

---

## [0.1.13] - 2026-04-04 (Phase 2B — Chat Features, Server Mute/Deafen, Infrastructure)

### Added

- **Server mute/deafen with SFU enforcement** — Server-side mute and deafen state propagated to mediasoup SFU; consumers paused/resumed on enforcement (#488)
- **Shared `useChatController` hook** — Unified chat container logic (fetch, paginate, decrypt, send) extracted into a reusable hook (#545)
- **Message reactions** — Emoji reaction add/remove on messages with real-time sync (#169, #459)
- **Reply/quote messages** — Threaded reply rendering with quoted message preview (#170, #463)
- **Message pinning** — Pin/unpin messages in channels with pin feed (#171, #465)
- **E2EE-native message search** — Client-side decrypted search across channel message history (#172, #468)
- **File and image attachments** — Upload, preview, and download for chat attachments (#178, #470)
- **Group DM creation and management** — Multi-participant DM groups with admin controls (#208)
- **Desktop notifications** — System notifications for @mentions and new DMs (#175, #478)
- **Draft message persistence** — Unsent drafts saved per-channel and restored on revisit (#174, #477)
- **Keyboard shortcuts system** — Configurable keyboard shortcuts with help overlay (#176, #479)
- **Global context menu** — Unified context menu system with clipboard support and role assignment (#446)
- **SPA deployment pipeline** — File server, versioning, and GitHub Actions workflow for hot-update SPA bundles (#429)
- **Docker network segmentation** — Service isolation, request ID propagation, Redis auth bans (#442)
- **AI governance framework** — AI-generated code policy, CODEOWNERS, agentic controls, Semgrep SAST, path-scoped [internal] rules, Claude Code skills, custom agents, Copilot prompt templates, MCP project config (#454–#458, #500–#504, #507–#518, #522)

### Changed

- **Message component decomposition** — `Message` component decomposed; shared types extracted for the Phase 2B chat rewrite (#451)
- **DM thread list real-time updates** — `last_message` included in `dm_unread_notify` events for live thread list refresh (#541)
- **Dependency bumps** — TypeScript 6.0.2, typescript-eslint 8.58, eslint 10.1, mediasoup, lucide-react 1.7.0, Electron, `@types/node`, `@playwright/test`, Go module group, Actions group, media-plane dev-tooling (#411, #414, #511, #528–#529, #532–#536, #538–#539)
- **Email infrastructure** — Transactional email migrated from Proton SMTP to Resend; branded templates; `verify.concordvoice.chat` subdomain
- **CI pipeline hardening** — Semgrep SAST added; quality gates formalized; CI performance optimized with `sync.Once` migrations, test sharding, and caching (#457, #471)
- **Go and TypeScript complexity reduction** — Cognitive complexity reduced across control-plane handlers and frontend/media-plane code (#418, #419, #498)
- **DRY modal components** — Shared modal panels extracted to eliminate duplication (#365, #481)

### Fixed

- **DM thread real-time updates** — Last message propagated in unread notify payload to refresh thread list (#541)
- **Replied-to message decryption** — `replied_to` content decrypted on REST message fetch, not only via WebSocket (#542)
- **Server role styling isolation** — Server role badge styles no longer bleed into DM message rendering (#543)
- **Hub test determinism** — `time.Sleep` replaced with deterministic channel-based sync in WebSocket hub tests (#544)
- **Context menu role assignment** — Role assignment via context menu no longer fails silently (#447)
- **Voice flaky tests** — Hub goroutine races with test cleanup resolved (#476)
- **Modal Escape handler** — Nested Escape key handler no longer fires on all stacked modal instances (#480)
- **Video frame scaling** — Video frames dynamically scale to fill voice chat area (#443)
- **WebSocket reconnect** — Connection-lost handler replaced page reload with direct WS reconnect (#194, #439)
- **Accessibility pass** — Semantic HTML, keyboard navigation, ARIA attributes, and UI polish across the app (#380, #427)
- **nginx security** — H2C smuggling and Host header injection mitigated; shell injection in GitHub Actions CI fixed (#523, #525)
- **Semgrep findings** — Verified-safe `sql-sprintf` findings suppressed with inline annotations (#524)
- **Media-plane Redis URL** — Media-plane added to data network with correct Redis URL configured

### Security

- **Nginx hardening** — H2C smuggling vector closed; Host header injection blocked (#525)
- **GitHub Actions shell injection** — Semgrep-identified injection in CI workflow resolved (#523)

---

## [0.1.12] - 2026-03-27 (Phase 2A — Foundations & Security)

### Added

- **MFA/WebAuthn authentication** — TOTP, WebAuthn/FIDO2, backup codes, recovery circles, trusted devices (#89)
- **RBAC/SBAC permission system** — Granular role-based access control with audit logging (#82)
- **Email verification** — SMTP-based verification on registration (#269)
- **Object storage (MinIO)** — User image assets migrated from PostgreSQL to S3-compatible storage (#166)
- **Server ownership transfer** — Full lifecycle with MFA, email confirmation, reversal tokens (#244)
- **Desktop auto-updater** — Safe updates with rollback, splash screen, progress tracking (#155, #381-#387)
- **Token theft detection** — Machine ID + IP binding with automatic revocation (#89)
- **Proactive token refresh** — JWT refresh before expiry with rate limiting (#240, #254)
- **CSK rotation on member removal** — E2EE forward secrecy for channel keys (#96)
- **DM key-epoch enforcement** — Key revocation table + epoch checking in WebSocket path (#122)
- **@mention routing in E2EE** — Server-side mention detection without decryption (#118)
- **OS permission management** — System-level permission requests and enforcement (#197)
- **Self-hosted coturn STUN/TURN** — Infrastructure for NAT traversal (#124)
- **CI/CD pipeline** — GitHub Actions build.yml with parallel test + coverage + SonarQube (#128, #130)
- **Test coverage push** — 70 Go test files, 195 frontend test files toward 80% Quality Gate
- **Custom branded splash screen** — Install/update progress with Concord Voice branding (#387)
- **Install/update logging** — Structured file-based logging for troubleshooting (#383)

### Changed

- **React 18 → 19.2.4** (#181)
- **react-router-dom 6 → 7.13.1** (#182)
- **Zustand 4 → 5.0.12** (#183)
- **ESLint → 10 flat config** (client + media-plane) (#184, #185, #186)
- **Redis client 4 → 5.0.0** (media-plane) (#187)
- **Vite 7 → 8.0.0** + @vitejs/plugin-react 4 → 6 (#287)
- **Go 1.24 → 1.26.1** + govulncheck hardening (#193)
- **Electron 33 → 41.0.2** (desktop client)
- **mediasoup 3.13 → 3.19.18** (server), mediasoup-client → 3.18.7
- **E2EE key derivation** — PBKDF2 → Argon2id client-side (#117)
- **useMessageFetch hook** — Extracted shared fetch/decrypt/paginate logic (#177)
- **Desktop bundle naming** — Unified to "Concord Voice" across all platforms (#385)
- **OS-level app metadata** — Correct version, icon, publisher on all platforms (#386)
- **Shared updater resources eliminated** — Prevents conflicts with other Electron apps (#382)

### Security

- **CORS hardening** — Null/empty origin rejection, custom header validation (#259)
- **Credential extraction** — Hardcoded dev credentials removed, production guards added (#260)
- **Scanner hardening** — Brute-force probe mitigation at infrastructure level (#153)
- **Dependabot vulnerability fixes** — npm overrides for transitive vulnerabilities (#289)

### Fixed

- **E2EE voice/video codec collision** — WebRTC BUNDLE misrouting between consumers (#291)
- **Mac microphone in E2EE voice** — Audio production fixed in encrypted channels (#295)
- **Video streaming quality** — Codec selection and screen share audio (#299)
- **Voice audio persistence** — Audio no longer cuts out when navigating away (#396)
- **Voice/video black screens** — Audio output and video rendering restored (#227)
- **Message editing** — Edit submit no longer silently dropped (#161)
- **Identity asset theming** — Uses viewed user's color scheme, not viewer's (#165)
- **Popover toggle** — Clicking again dismisses instead of respawning (#167)
- **Screen share defaults** — Respects Video Configuration settings (#198)
- **UpdateRole response** — Handler now returns role body, preventing crash (#249)

## [0.1.0-alpha] - 2026-03-03 (Phase 1 — Core Platform)

### Added

- **Phase 1A: Authentication & E2EE** — User registration, JWT + refresh tokens, E2EE (RSA-OAEP 4096 + AES-256-GCM), session management, Argon2id password hashing, rate limiting
- **Phase 1B: Channels & Text Chat** — Server/channel CRUD, WebSocket messaging, E2EE encryption/decryption, presence system, 12 color schemes (dark/light), security hardening, API docs (OpenAPI 3.0)
- **Phase 1C: Voice, Media & Desktop** — mediasoup SFU (voice, video, screen share), 7 audio quality tiers, video codec selection (VP9/AV1/VP8/H.264), channel groups/categories, Electron safeStorage, emoji picker, DM framework, custom theme builder, mic test with loopback
- **Infrastructure** — Docker Compose (dev/staging/production), coturn STUN, cross-platform Electron build, pre-commit hooks, Dependabot
