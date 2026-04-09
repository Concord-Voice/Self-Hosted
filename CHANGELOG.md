# Changelog

All notable changes to Concord Voice will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

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
