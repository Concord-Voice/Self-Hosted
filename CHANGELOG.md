# Changelog

All notable changes to Concord Voice are recorded in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
This file names the pending section `[Prerelease]`, not `[Unreleased]`, after
the release channel that ships it first.

To add an entry, write one line under the matching `###` category in
`[Prerelease]`: Added, Changed, Deprecated, Removed, Fixed, or Security. Start
the line with a bold area tag, such as `**Voice:**`. Mark a breaking change with
`**BREAKING**` after the tag. The category sets the recommended version bump:
Added, Deprecated and Removed suggest a minor, and the rest suggest a patch. A
**BREAKING** entry suggests a major (a minor before 1.0). Keep one summary of 1-3
sentences above the categories. The post-update dialog shows only that summary.
Do not add a paragraph for each change. Edit the summary only when your change
belongs in it, and write it if the section has none. Do not edit the
`**N Total Changes**` line. The promoter recounts it when it cuts a release.

## [Prerelease]

Paid plans are ready: Supersonic and Wing subscriptions, a 30-day Supersonic
trial, plan changes, and code credit. Servers can require MFA for dangerous
actions, purges and message timers can keep pinned messages, and the microphone
gate can learn from your voice.

**107 Total Changes** | 14 Added | 23 Changed | 48 Fixed | 22 Security

### Added

- **Tray:** The tray icon now shows both stars of the Concord Voice mark, and it reflects your status. It shows when you are in voice, muted, deafened, sharing your screen, reconnecting, or have unread messages or mentions. On macOS and Windows, click the icon to open a menu with Mute, Deafen, Leave voice, Stop sharing, your presence, and a shortcut to your newest unread conversation. On Linux, click the icon to open the window; the menu comes from your desktop's tray host.
- **Settings:** Settings ▸ Appearance has a new Tray Icon setting (Menu Bar Icon on macOS). Choose Automatic, Light, Dark, or Colour to match your taskbar or menu bar.
- **Billing:** Settings ▸ Subscription lists each plan with its monthly and yearly price and opens a secure Stripe page to subscribe or manage billing.
- **Billing:** New subscribers get a 30-day free trial of Supersonic. The card is collected first, and billing starts when the trial ends. Wing has no trial.
- **Billing:** Subscribers can change plans in Settings, see the amount due before an immediate switch, and withdraw a change that is booked for renewal.
- **Billing:** A Wing subscription can share Supersonic with one friend. Invite the friend from Settings ▸ Subscription. You can change your +1 once every 30 days.
- **Billing:** A premium code redeemed on a paid subscription becomes account credit that your next renewals use first. A code for a different plan shows the credit and asks you to confirm.
- **Self-hosting:** Operators can set separate minimum app versions for iOS and Android with `CLIENT_MIN_VERSION_IOS` and `CLIENT_MIN_VERSION_ANDROID`. `CLIENT_MIN_VERSION` still sets the desktop minimum. API: mobile clients send `X-Concord-Client-Platform` (`ios` or `android`), or `client_platform` on the WebSocket; any other value gets a 400.
- **Servers:** Owners can require MFA for dangerous actions from Server Settings. Members then confirm each dangerous action with a code or security key ([#3454](internal development reference omitted), [#3456](internal development reference omitted), [#3675](internal development reference omitted)). API: send `mfa_code` on a retry after a 403 `mfa_required`. `GET /api/v1/server/capabilities` reports `features.mfaEnforcedDangerousActions`.
- **Servers:** Delete Role asks you to confirm and names the role first ([#3675](internal development reference omitted)).
- **Messages:** Every purge has an "Include pinned messages" option, off by default ([#3458](internal development reference omitted), [#3552](internal development reference omitted)). API: `include_pinned` and `features.purgeKeepsPinned`.
- **Messages:** Message expiration timers can keep pinned messages, and new timers keep them by default ([#3459](internal development reference omitted)). API: `include_pinned` and `features.expirationPinnedPolicySupported`.
- **Messages:** Up Arrow in an empty message box edits your last message ([#1959](internal development reference omitted)).
- **Voice:** Auto Calibrate and Manual Calibrate can suggest a fixed microphone gate threshold from a room-and-voice check. Manual Calibrate also shows a live meter ([#4118](internal development reference omitted), [#3637](internal development reference omitted)).
- **Operations:** `provision-secrets.yml` now carries `APP_ATTEST_APP_IDS`, so production can turn on iOS App Attest from a GitHub variable (ADR-0010 D17).
- **Operations:** Blue/green media deployment can move new voice rooms to a second instance while existing rooms drain. It is off by default ([#2752](internal development reference omitted)).
- **Operations:** A separate West staging deployment path is available. It stays off until its secrets, access, and checks are complete. West is not a production site ([#2751](internal development reference omitted)).
- **Operations:** Groundwork, off by default: managed ingress verification ([#3667](internal development reference omitted)), managed egress ([#4048](internal development reference omitted)), Redis credential rotation ([#3930](internal development reference omitted)), TURN credential rotation ([#3931](internal development reference omitted)), and shared media listeners. Each needs host checks and approval before activation.

### Changed

- **Self-hosting:** Self-hosted deployments now ship from the dedicated Self-Hosted repository, with the service, admin, and package checks running in its own CI.
- **Voice:** The desktop call library handles mixed audio and video RTP header extensions with mediasoup-client 3.25.3.
- **Voice:** Updated server voice transport dependencies.
- **Accessibility:** The desktop app follows the system's reduced-motion setting, hides decorative icons from screen readers, and shows text while bundled fonts load ([#4181](internal development reference omitted)).
- **Desktop:** Updated runtime and updater dependencies.
- **Plans:** Every account can create any number of servers, search as far back as a subscriber, and customize theme colors. One account can create up to 10 servers in 24 hours ([#3861](internal development reference omitted)).
- **Desktop:** Updated navigation dependencies.
- **Mobile:** The in-development mobile client, not yet released, now builds on React Native 0.88.0.
- **Web:** Opening the web address in a browser shows a link to download the desktop app, not an "Update Required" prompt.
- **Desktop:** Updated installer tooling.
- **Search:** Message search looks back 90 days or 600 messages at a time, loads older results as you scroll, and says when it is paused or stopped ([#3875](internal development reference omitted)).
- **Search:** Jump reaches messages that are not loaded or are in another channel, from search results, pins, and reply quotes ([#3894](internal development reference omitted)).
- **Settings:** Subscription is under Account, protection settings are under Security, and device permissions are under About & Updates ([#3780](internal development reference omitted)).
- **Settings:** Sections start collapsed and remember what you expand while you use Settings and for 30 minutes after you leave ([#2365](internal development reference omitted), [#3651](internal development reference omitted)).
- **Settings:** Audio and video device controls sit with their settings. An app-only Microphone Level (0–200%) shows when automatic gain control (AGC) is off ([#3635](internal development reference omitted)).
- **Security:** Purge, purge protection, DM Clear, and Security settings prompts ask only for the methods your account uses. A passkey or security key works in one step ([#3660](internal development reference omitted), [#3673](internal development reference omitted)).
- **Privacy:** New accounts start with stricter defaults for friend requests, voice details, call presence, and notification previews. Existing choices do not change.
- **Friends:** Under Mutual Servers, people who share a server or a mutual friend with you, or who have your friend code, can send you a request. Username changes wait six months on Free and three on Premium.
- **Recovery:** Recovery Circle recovery is temporarily paused. Your saved contacts stay in Settings. Use another recovery method to regain access.
- **Voice:** The Noise Gate has Dynamic, Auto Calibrate, Manual Calibrate, and Off modes. Dynamic adapts during a call or Microphone Test ([#3657](internal development reference omitted)).
- **Voice:** With AGC on, calls and Microphone Test limit outgoing microphone sample peaks to −6 dBFS through one shared processing path ([#3653](internal development reference omitted)).
- **Voice:** Turning DTX off in Advanced settings disables it for every quality preset.
- **Voice:** Voice, camera, and screen sends use separate plan-based bitrate ceilings. If video use stays over the limit, the server pauses video for a cooldown and voice continues.
- **Voice:** When voice is paused for server maintenance, joining a call says so and suggests that you try again in a few minutes.
- **Voice:** When operators enable blue/green mode, calls recover after the old media instance stops at its two-hour drain cutoff ([#2752](internal development reference omitted)).
- **Video:** Enable HDR and Prioritize HDR Codecs are separate settings. Neither setting guarantees HDR capture or display.
- **Video:** Webcam speaker frames keep a fixed size and show a steady highlight ([#3595](internal development reference omitted), [#3596](internal development reference omitted)).
- **GIFs:** GIF personalization sends KLIPY an ID derived from your Personalization ID. With personalization off, GIF browsing uses a temporary ID that changes about every 30 minutes.
- **Bug reports:** Screenshots have storage allowances and expire after 72 hours if the report is abandoned.
- **Desktop:** Electron 44.5.0 ([#3599](internal development reference omitted)), Electron Forge 8 packaging ([#3794](internal development reference omitted)), lucide-react 1.48.0 ([#3645](internal development reference omitted)), mediasoup-client 3.24.1, and socket.io-client 4.8.4 ([#3602](internal development reference omitted)).
- **Server:** The control plane builds with Go 1.27.2 ([#3823](internal development reference omitted)).

### Fixed

- **Connections:** WebSocket input waiting on pacing or a full server queue now stops when its connection is canceled or the server shuts down.
- **Voice:** The media server now forwards your app's attestation when it checks your voice access, a step toward letting servers require an attested app ([#4211](internal development reference omitted)).
- **Sign-in:** A signed-out session no longer retries its rejected refresh token every 10 seconds in the background ([#4205](internal development reference omitted)).
- **Voice:** Calling someone back in a DM joins the call still open there instead of silently stopping, a failed call start now shows an error, your camera comes back after a brief connection drop, and 1:1 DMs show Join voice call during a call ([#4179](internal development reference omitted)).
- **Voice:** Microphone processing failures now notify the existing error handler and release the failed audio node once ([#4147](internal development reference omitted)).
- **Billing:** Blocking someone ends a Wing seat between you, whichever side blocks. Unfriending or blocking also cancels a pending seat invite at once; unfriending alone keeps an active seat ([#4172](internal development reference omitted)).
- **Billing:** Switching from a monthly to a yearly plan works again. It credits the unused part of the month and keeps your billing date.
- **Billing:** Subscribing twice at the same time no longer creates a second subscription.
- **Billing:** Opening billing no longer withdraws a booked plan change. Cancellation keeps access through the paid period, and the plan card shows your actual price.
- **Billing:** Redeeming two codes at the same time no longer fails ([#4019](internal development reference omitted)).
- **Billing:** Subscription updates apply in order, so an older downgrade cannot follow a newer upgrade ([#4079](internal development reference omitted)).
- **Billing:** Recovery changes only subscriptions that its environment owns, and canceled seat requests still notify members ([#4110](internal development reference omitted)).
- **Chat:** The desktop chat connection no longer drops and reconnects every 14 minutes.
- **Chat:** Chat no longer drops for everyone every 30 seconds when someone sits in a voice channel that none of their friends can see.
- **Chat:** Changing activity sharing settings in large servers no longer disconnects unrelated users.
- **Chat:** Chat stays responsive under excessive connection traffic ([#3897](internal development reference omitted)).
- **Messages:** Failed channel messages keep their text and retry controls after a brief disconnect ([#4079](internal development reference omitted)).
- **Messages:** History no longer skips messages that share a timestamp, in channels or DMs ([#3875](internal development reference omitted), [#3989](internal development reference omitted)).
- **Messages:** An edit is held to the character limit ([#3857](internal development reference omitted), [#3873](internal development reference omitted)).
- **Messages:** A delayed failed edit no longer closes an edit in another chat panel or loses its text.
- **Messages:** Attachments upload and send in call text chat ([#3985](internal development reference omitted)).
- **Messages:** Attachments that scroll out of view release their decrypted data ([#4086](internal development reference omitted)).
- **Messages:** A server purge refreshes only the channels it changed ([#4052](internal development reference omitted)).
- **Messages:** Reading a server's channels clears its unread indicator ([#3777](internal development reference omitted)).
- **Invites:** Invite links keep their previews and open the selected invite after sign-in. Profile and server images load through the signed-in account ([#3942](internal development reference omitted)).
- **Friends:** Copying a friend code no longer leaves a timer running after you leave Add Friend.
- **Roles:** Failed role and override saves show an error. A purge refused for MFA asks for a code or links to setup ([#3675](internal development reference omitted)).
- **Voice:** Calls keep your mute and deafen choices through recovery, and DM calls keep their call type ([#4086](internal development reference omitted)).
- **Voice:** Dynamic gating releases stale thresholds and keeps phrase evidence through pauses. Invalid saved thresholds reset to the default ([#4113](internal development reference omitted)).
- **Voice:** The microphone resumes after a settings change with the Dynamic noise gate ([#3670](internal development reference omitted)).
- **Voice:** If AGC fails, the microphone stays paused ([#3658](internal development reference omitted)).
- **Voice:** Joins capture the selected microphone once and explain how to recover if capture fails ([#3619](internal development reference omitted)).
- **Voice:** Audio menus show one Default option that follows the operating system ([#3619](internal development reference omitted)).
- **Voice:** Join errors show in a small centered prompt and no longer push the layout down.
- **Voice:** Removing someone from voice during their join waits for their connection to close ([#3870](internal development reference omitted)).
- **Voice:** Disconnects wait for media cleanup before the session is released ([#3778](internal development reference omitted), [#2624](internal development reference omitted)).
- **Voice:** Voice moderation and group DM removals survive broker outages ([#3818](internal development reference omitted), [#2623](internal development reference omitted)).
- **Voice:** Voice startup waits for enforcement. If enforcement fails, new joins stop and calls close within 30 seconds ([#3852](internal development reference omitted)).
- **Voice:** Blue/green retries keep call and room ownership ([#3941](internal development reference omitted)).
- **Video:** Video stays on when someone with incompatible video support joins ([#4051](internal development reference omitted)).
- **Video:** H.264 video no longer goes black after another camera turns on ([#3985](internal development reference omitted)).
- **Video:** Camera swaps keep local previews, and late media from an earlier session is closed ([#3650](internal development reference omitted)).
- **Video:** Camera recovery uses codec and HDR settings that you save during a restart ([#4079](internal development reference omitted)).
- **Sign-in:** Email-code sign-in sends the code ([#3672](internal development reference omitted)).
- **Sign-in:** Sign-in offers backup codes only when they can work ([#3567](internal development reference omitted)).
- **Sign-in:** Session checks can use your security key ([#3663](internal development reference omitted)).
- **Settings:** Verification setup no longer loses unsaved settings or applies a stale confirmation ([#3452](internal development reference omitted), [#3711](internal development reference omitted), [#3717](internal development reference omitted)).
- **Accessibility:** Settings uses a native microphone level meter and respects reduced motion ([#4113](internal development reference omitted)). Screen readers skip decorative security icons ([#3869](internal development reference omitted)).
- **Desktop:** Pilot builds can connect alongside Stable builds ([#3905](internal development reference omitted)).
- **Desktop:** Windows updates recheck the installer before restart, including saved downloads ([#2455](internal development reference omitted)).
- **Desktop:** Group DM icons load through authenticated requests ([#3771](internal development reference omitted)).
- **Server:** Animated image uploads use less memory ([#3981](internal development reference omitted)).
- **Self-hosting:** Calls through the bundled TURN relay work when the relay and media server share one public address. An IPv6 loopback rule no longer blocks IPv4 peers.
- **Self-hosting:** The offline ingress verifier refuses ambiguous NAT address and source-port pools instead of reading them as fixed translations ([#4180](internal development reference omitted)).
- **Operations:** A production deploy keeps running on the server when its SSH tunnel drops mid-rollout, and the deploy reconnects to that run instead of failing ([#4178](internal development reference omitted)).
- **Voice:** Signing out or switching accounts while a DM call is starting no longer lets that call start for the next account.

### Security

- **Accounts:** Your encrypted key is only sent to a device that has just signed in, or that confirms your current password, so a stolen session can no longer download it. Changing your password asks for your current password as before.
- **Voice:** One account can no longer fill the voice server's connection capacity with idle connections and lock other users out of voice.
- **Accounts:** A device signed out while its account-deletion request was still processing can no longer delete the account ([#4192](internal development reference omitted)).
- **Accounts:** Signing out a device or revoking a session cuts it off everywhere at once. Its open connection and any voice call end before the change is confirmed, and a signed-out device can no longer act on your account.
- **Attestation:** iOS App Attest binds each attested key to the account that attested it, ties the attestation session header to the signed-in session, and caps the verify request size (ADR-0010 D17).
- **Attestation:** The server no longer accepts an attestation record that is not tied to an account, so such a record cannot stand in for another user ([#4211](internal development reference omitted)).
- **Dependencies:** Updated golang.org/x/net and fast-uri for security fixes ([#4181](internal development reference omitted)).
- **Desktop:** Updated the URI parser security floor to address upstream normalization advisories.
- **Accounts:** Sign-in protection accepts a key re-encryption only from the maintenance run that started it. It refuses to roll back while its state is unknown ([#4137](internal development reference omitted)).
- **Accounts:** An account with suspicious session use stays blocked until session revocation finishes.
- **Accounts:** Account recovery asks for the authenticator you reserved for recovery ([#3868](internal development reference omitted)).
- **Accounts:** Trusted-device recovery requires matching fingerprints on both devices ([#3582](internal development reference omitted)). Update both devices and start a new request.
- **Accounts:** Security-key confirmations go only to the server that asked ([#3690](internal development reference omitted)).
- **Accounts:** Session refresh checks the device identifier ([#3865](internal development reference omitted)). Concurrent sign-ins cannot reuse a recovery code ([#3860](internal development reference omitted)).
- **Desktop:** The desktop rechecks its saved web app when the signature changes and refuses an invalid one.
- **Desktop:** Electron 44.9.1 blocks compromised eMudhra intermediate certificates ([#4071](internal development reference omitted), [#4074](internal development reference omitted)).
- **Real time:** Control connections close when their access token expires ([#3895](internal development reference omitted)).
- **Real time:** An account can keep up to 8 WebSocket connections and a process accepts up to 10,000 ([#3892](internal development reference omitted)).
- **Real time:** Profile changes reach only people with a current relationship ([#3891](internal development reference omitted)). Account deletion clears status only for its prior viewers ([#4055](internal development reference omitted)).
- **Messages:** DMs store participant messages only as encrypted messages ([#3864](internal development reference omitted)).
- **Messages:** The server requires an explicit key version on every message. Upgrade all control-plane writers before you apply the migration ([#2844](internal development reference omitted), [#3935](internal development reference omitted)).
- **Messages:** Attachment deletion requires current access to the chat. Group DM icon uploads and reads require current admin role or membership ([#2625](internal development reference omitted), [#3779](internal development reference omitted)).
- **Messages:** Erased feedback screenshots no longer load from the image endpoint ([#3982](internal development reference omitted)).
- **Plans:** Animated avatars, banners, and server icons follow the tier rules on every route ([#3856](internal development reference omitted), [#3874](internal development reference omitted)).
- **Plans:** Voice refuses microphone streams that do not declare packet time or bitrate ([#3858](internal development reference omitted), [#3872](internal development reference omitted)). Repeated over-limit requests are flagged and resync your plan limits ([#3877](internal development reference omitted)).
- **Self-hosting:** Disabled feedback refuses submissions, and the installer rejects unsafe settings and ambiguous retained data. Release publication verifies proof and attestations.
- **Operations:** Redis health checks keep passwords out of Docker event commands. Existing deployments need the updated container configuration.
- **Operations:** CI fails on fixable high or critical vulnerabilities in server images ([#3987](internal development reference omitted)).
- **Dependencies:** Go 1.27.3 ([GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809)), Go 1.26.9 and `golang.org/x/net` 0.60.0 ([#3623](internal development reference omitted)), and Vite 8.3.3 ([GHSA-rq7h-c2jc-7f22](https://github.com/vitejs/vite/security/advisories/GHSA-rq7h-c2jc-7f22), [GHSA-vfpm-58rq-9qcg](https://github.com/vitejs/vite/security/advisories/GHSA-vfpm-58rq-9qcg), [GHSA-9jrq-w75r-8gcw](https://github.com/vitejs/vite/security/advisories/GHSA-9jrq-w75r-8gcw)).
- **Dependencies:** `source-map-js` 1.2.2 ([CVE-2026-93749](https://github.com/advisories/GHSA-68fv-2mgg-jv7q)), `shell-quote` 1.12.0 ([CVE-2026-102422](https://github.com/advisories/GHSA-pqg4-j6r4-53mv)), media-plane forwarded-address parser ([#3620](internal development reference omitted)), and `sprintf-js` removed ([#3776](internal development reference omitted)).

## [0.2.48] - 2026-10-04

Screen sharing stays steady when a capture changes, and voice pauses a stream
that sends far more than your plan allows. Security prompts ask for the right
proof in the right places, never a code your account cannot provide and never
your password twice.

**88 Total Changes** | 2 Added | 16 Changed | 57 Fixed | 13 Security

### Added

- **Bug reports:** The bug-report form accepts up to four screenshots (PNG, JPEG, or WebP, 5 MB each). Re-encoding removes camera metadata such as GPS location ([#1747](internal development reference omitted)).
- **Messages:** Deleting more than 15 messages in 30 seconds, or 100 in 24 hours, asks you to confirm it is you. API: optional `{"mfa_code", "step_up_token"}` body and new `POST /api/v1/auth/step-up/password` (migration 000162) ([#3455](internal development reference omitted), [#3509](internal development reference omitted)).

### Changed

- **Voice:** The voice server pauses a microphone, camera, or screen share that keeps sending far above your plan. A second time within an hour removes you from the call for 15 minutes ([#3413](internal development reference omitted), [#2153](internal development reference omitted)).
- **Voice:** Screen sharing with sound sends at your plan's audio quality (96 kbps on Free) ([#3413](internal development reference omitted)).
- **Messages:** API clients can hide a DM or group from one participant's list, or clear that participant's history before a server-stamped cutoff ([#3306](internal development reference omitted)).
- **Messages:** DMs offer separate Hide, Clear history for me, and Leave group controls ([#3434](internal development reference omitted)).
- **Messages:** Purge Messages uses one range menu and says first that a DM purge removes only your own messages ([#3468](internal development reference omitted)).
- **Messages:** The Hide confirmation no longer uses warning styling, and Leave group has its own section in Group Info ([#3468](internal development reference omitted)).
- **Servers:** Owners and Administrators can require MFA for dangerous actions through `PUT /api/v1/servers/{id}/mfa-enforcement`. Email and text codes do not count ([#3464](internal development reference omitted), [#3453](internal development reference omitted)).
- **Appearance:** Font Size moved to Appearance ▸ Application Font and stays usable with Dyslexic Support on ([#3401](internal development reference omitted), [#2367](internal development reference omitted)).
- **Accessibility:** UI Scale runs from 50% to 200% and zooms the whole interface. Above 100%, the window width limits the zoom ([#3414](internal development reference omitted), [#2367](internal development reference omitted)).
- **Appearance:** Application Font has two modes. One Font applies your pick everywhere, and Font by Area sets fonts for Messages, Headings, Navigation, and Interface ([#2366](internal development reference omitted), [#3429](internal development reference omitted)).
- **Appearance:** Application Font highlights your mode. Theme Default and Concord Voice Default each have a description ([#3457](internal development reference omitted)).
- **Security:** After you add an authenticator app, Concord says whether it created, kept, or failed to create your recovery key ([#3433](internal development reference omitted)).
- **Server:** DM-block and credential-epoch cleanup records retryable voice ejections and fences stale callbacks ([#3140](internal development reference omitted)).
- **Server:** DM writes recheck current membership before they store keys or uploads ([#3141](internal development reference omitted)).
- **Server:** Server changes serialize authority and membership at their write boundary, so stale operations fail closed ([#3142](internal development reference omitted)).
- **Voice:** Voice joins verify admission before they open a session, so an old disconnect cannot remove a newer join ([#3143](internal development reference omitted)).

### Fixed

- **Bug reports:** Screenshots removed while logs are collected are no longer sent with the report.
- **Messages:** Messages and attachments always carry the version of the key that encrypted them ([#3565](internal development reference omitted)).
- **Messages:** A purge target names the same author at admission and deletion. A malformed `target_user_id` returns 400 ([#3553](internal development reference omitted)).
- **Messages:** A hidden conversation comes back as soon as someone messages it ([#3468](internal development reference omitted)).
- **Messages:** Group messages from before someone left are readable again ([#3472](internal development reference omitted)).
- **Messages:** Purging a DM stops after 10 seconds, and removed messages disappear for others at once ([#3442](internal development reference omitted)).
- **Messages:** Empty conversations that everyone has hidden are cleaned up, and the last group member can leave ([#3402](internal development reference omitted), [#2821](internal development reference omitted)).
- **Messages:** Return to Latest stays at the latest message while older history loads.
- **Messages:** A saved chat position waits for the history page that holds it.
- **Self-hosting:** Self-hosted servers keep Google and Apple sign-in unavailable. Password sign-in still works ([#1619](internal development reference omitted), [#3546](internal development reference omitted)).
- **Sign-in:** Remember Me keeps your saved sign-in when the keychain is briefly unavailable ([#3560](internal development reference omitted)).
- **Sign-in:** Two-factor sign-in no longer hangs when your encryption keys cannot be unlocked ([#3466](internal development reference omitted), [#3453](internal development reference omitted)).
- **Security:** Email MFA can be turned on again, and SMS shows as coming soon ([#3519](internal development reference omitted)).
- **Security:** Regenerating backup codes from Settings works again ([#3466](internal development reference omitted), [#3453](internal development reference omitted)).
- **Security:** Try again on the recovery-key screen counts each attempt ([#3433](internal development reference omitted)).
- **Security:** Reset TOTP accepts a typed backup code ([#3433](internal development reference omitted)).
- **Security:** Turning on two-factor authentication no longer asks for your code twice ([#3437](internal development reference omitted)).
- **Security:** A damaged two-factor record no longer crashes sign-in checks or the `mfa-rekey` tool ([#3420](internal development reference omitted)).
- **Security:** Accounts with only email or text codes are not asked for a code they cannot check ([#3433](internal development reference omitted)).
- **Security:** Removing your last authenticator or security key while email or text codes are on is refused, with a reason ([#3433](internal development reference omitted)).
- **Security:** Purging messages no longer asks for your password twice ([#3433](internal development reference omitted)).
- **Security:** A wrong password or code marks the field that was wrong, and screen readers announce it ([#3433](internal development reference omitted)).
- **Security:** Retrying a lost recovery-key save resends the same key ([#3433](internal development reference omitted)).
- **Security:** A temporarily unavailable verification check says so instead of "too many attempts" ([#3433](internal development reference omitted)).
- **Security:** Account-security actions report a failure instead of claiming success ([#3461](internal development reference omitted)).
- **Security:** The "Verify Your Identity" prompt opens on top of Settings and takes the keyboard ([#3423](internal development reference omitted)).
- **Servers:** Requests to a server that is deleted mid-request answer as for a non-member, not with a server error ([#3508](internal development reference omitted)).
- **Servers:** Channel and category permission overrides save again. API: permission bitfields now cross JSON as decimal strings (breaking change) ([#3473](internal development reference omitted), [#3406](internal development reference omitted)).
- **Servers:** Server owners can mention @all and @here again ([#3464](internal development reference omitted), [#3453](internal development reference omitted)).
- **Servers:** Text-only channel changes work on busy voice servers ([#3480](internal development reference omitted)).
- **Invites:** Inviting someone to a server they are already in is greyed out ([#2372](internal development reference omitted)).
- **Invites:** The invite button in a DM lists every server you can invite to ([#2372](internal development reference omitted)).
- **Invites:** An invite to a server you are already in says "Joined" ([#2372](internal development reference omitted)).
- **Voice:** A failed voice join no longer removes someone who joined the next call ([#3479](internal development reference omitted), [#3480](internal development reference omitted)).
- **Voice:** Voice access cleanup no longer sends an in-flight update to someone whose access was revoked ([#3480](internal development reference omitted)).
- **Voice:** Voice activity clears before a privacy change reconnects the app ([#3412](internal development reference omitted)).
- **Voice:** Moving between voice channels on different servers no longer loses the old-channel leave ([#3415](internal development reference omitted)).
- **Voice:** Voice channel lists recover after stale sessions without wrong counts or repeated leave sounds ([#3304](internal development reference omitted), [#3298](internal development reference omitted), [#3370](internal development reference omitted)).
- **Presence:** Rich Presence sharing settings save every time ([#3444](internal development reference omitted)).
- **Screen share:** Free accounts start screen sharing at 1080p, not a Premium resolution ([#3436](internal development reference omitted)).
- **Screen share:** Sharing an app with sound says when the sound could not be captured ([#3405](internal development reference omitted)).
- **Screen share:** App audio captures helper processes and says when the sound stops ([#3445](internal development reference omitted), [#3394](internal development reference omitted)).
- **Screen share:** Switching share sources no longer drops or leaks per-app audio ([#3369](internal development reference omitted)).
- **Desktop:** A malformed server response no longer replaces the desktop's working configuration ([#3481](internal development reference omitted), [#2240](internal development reference omitted)).
- **Desktop:** Update, reconnection, and what's-new windows work while Settings is open ([#3424](internal development reference omitted)).
- **Desktop:** Links opened from a picture-in-picture window open in your browser ([#3416](internal development reference omitted)).
- **Desktop:** Right-click menus no longer open partly off-screen ([#3414](internal development reference omitted), [#2367](internal development reference omitted)).
- **Appearance:** Brand-colored buttons and avatars meet 4.5:1 contrast in every color scheme ([#3514](internal development reference omitted)).
- **Appearance:** Premium labels and Admin badges are readable in every color scheme ([#3439](internal development reference omitted)).
- **Appearance:** The sign-in logo is readable in light themes.
- **Appearance:** Dyslexic Support changes every piece of text ([#2366](internal development reference omitted)).
- **Appearance:** Your app font reaches text boxes, buttons, and menus ([#2366](internal development reference omitted)).
- **Accessibility:** Error text in code and security-key prompts is easier to read ([#3455](internal development reference omitted), [#3509](internal development reference omitted)).
- **Accessibility:** The Basic / Advanced switch in Audio and Video Configuration works with a keyboard and screen reader ([#3428](internal development reference omitted)).
- **Accessibility:** The audio quality labels work as buttons for keyboard and screen-reader users ([#3430](internal development reference omitted)).
- **Accessibility:** Keyboard focus rings are visible in every theme and no longer look like a selection ([#3438](internal development reference omitted), [#798](internal development reference omitted)).
- **Operations:** A dirty database migration stops control-plane startup until an operator recovers it ([#3138](internal development reference omitted)).

### Security

- **Accounts:** A backup code no longer signs you in past an authenticator app set to recovery only ([#3563](internal development reference omitted)).
- **Accounts:** An authenticator-app code works only once ([#3466](internal development reference omitted), [#3453](internal development reference omitted)).
- **Accounts:** A security-key confirmation counts only for the action you confirmed ([#3467](internal development reference omitted), [#3453](internal development reference omitted)).
- **Accounts:** Weakening your account's recovery asks you to prove it is you ([#3433](internal development reference omitted)).
- **Accounts:** Two-factor sign-in keeps its attempt limits, and your code, through a server fault ([#3460](internal development reference omitted)).
- **Accounts:** Account safety checks and moderation agree on one spelling of your account's identifier ([#3362](internal development reference omitted)).
- **Messages:** Once every member has cleared a DM or group's history, the server deletes those messages and attachments ([#3462](internal development reference omitted)).
- **Presence:** A dropped voice connection no longer leaves your activity visible to people you later hide it from ([#3446](internal development reference omitted)).
- **Operations:** The control plane treats an unset `ENVIRONMENT` as production, and refuses `CONCORD_ENV=test` outside development or test ([#3519](internal development reference omitted)).
- **Operations:** Identity-anomaly audit records keep the request's final authorization result ([#3369](internal development reference omitted)).
- **Dependencies:** The voice server's connection library closes GHSA-2GC4-CQFQ-P2GV ([#3507](internal development reference omitted)).
- **Dependencies:** Electron 44.4.3 carries current Chromium, V8, and PDFium security fixes ([#3132](internal development reference omitted)).
- **Dependencies:** macOS installer builds no longer pull the legacy DMG dependency chain ([#2727](internal development reference omitted), [#3396](internal development reference omitted)).

---

## [0.2.47] - 2026-09-18

Screen sharing gets tabs, a sound switch, per-app audio, and in-place source
switching. Shared chats can set message timers, and Rich Presence shows what
is shared and who may receive it.

**62 Total Changes** | 13 Added | 5 Changed | 37 Fixed | 7 Security

### Added

- **Messages:** Changing the message timer leaves a note in the conversation that says who changed it and to what ([#1351](internal development reference omitted)).
- **Messages:** The server stores message expiry policy for channels, DMs, and group DMs, and applies it in resumable batches ([#3144](internal development reference omitted), [#2195](internal development reference omitted)).
- **Messages:** Eligible chats can set message expiration to Off, 1 hour, 24 hours, 7 days, or 30 days, for new or existing messages ([#2197](internal development reference omitted)).
- **Messages:** Opening a busy channel or DM drops you at your first unread message, and messages you watch arrive count as read ([#3289](internal development reference omitted), [#3329](internal development reference omitted), [#2006](internal development reference omitted)).
- **Presence:** Rich Presence activity shows in member rows and profiles, and your panel shows its eligible audience ([#2235](internal development reference omitted)).
- **Screen share:** The share picker has Screens and Windows tabs with thumbnail grids ([#3145](internal development reference omitted), [#3204](internal development reference omitted)).
- **Screen share:** Screen sharing carries your computer's sound by default, with a switch to turn it off. Linux does not support it yet ([#3145](internal development reference omitted), [#3198](internal development reference omitted)).
- **Screen share:** Sharing one window can send only that app's sound, where the operating system supports it ([#3198](internal development reference omitted)).
- **Screen share:** A Switch button changes what you share without dropping viewers ([#3145](internal development reference omitted)).
- **Voice:** Each person's tile has a one-click mute that only you hear ([#3204](internal development reference omitted)).
- **Voice:** Voice participants show their profile photo in the channel sidebar ([#2360](internal development reference omitted)).
- **GIFs:** GIFs stop playing when you switch away from Concord, including animated WebP and PNG ([#3291](internal development reference omitted), [#2369](internal development reference omitted)).
- **GIFs:** Settings ▸ Accessibility ▸ Display has a GIF Playback control: Auto, Always, or Hover only ([#3291](internal development reference omitted), [#2369](internal development reference omitted)).

### Changed

- **Messages:** Message expiration and purging are separate controls. The timer shows next to the encryption notice, and Purge has its own header button ([#1351](internal development reference omitted)).
- **Messages:** Expired messages leave chats and pinned views together, in five-minute sweeps ([#2196](internal development reference omitted)).
- **Voice:** Call buttons are grouped by what they do, and only Leave is red ([#3204](internal development reference omitted)).
- **Voice:** Picture-in-Picture and the stay-active setting moved into a More menu ([#3204](internal development reference omitted)).
- **Voice:** Busy servers give each new voice room to the least busy CPU core ([#3157](internal development reference omitted), [#3149](internal development reference omitted)).

### Fixed

- **Voice:** Per-person volume, master output volume, quiet-user boost, and output device choice work again ([#3351](internal development reference omitted)).
- **Voice:** The chat button is back in DM calls ([#3351](internal development reference omitted)).
- **Voice:** Right-clicking someone in a DM call opens their menu ([#3351](internal development reference omitted)).
- **Voice:** Voice no longer drops people who are still connected when a media server's clock runs ahead ([#3187](internal development reference omitted)).
- **Voice:** A media server with a badly wrong clock can no longer freeze the rooms it serves ([#3205](internal development reference omitted)).
- **Video:** Turning your camera on in a call works again ([#3348](internal development reference omitted)).
- **Video:** Video drops to a smaller size before it freezes when your computer is struggling ([#3268](internal development reference omitted), [#3279](internal development reference omitted), [#3094](internal development reference omitted)).
- **Video:** Joining a call late no longer leaves your camera stuck at one quality ([#3275](internal development reference omitted)).
- **Video:** The controls on a video can be reached with a keyboard ([#3204](internal development reference omitted)).
- **Screen share:** Screen sharing respects your quality setting again ([#3348](internal development reference omitted)).
- **Screen share:** Changing screen-share quality mid-share reaches everyone watching ([#3260](internal development reference omitted), [#2208](internal development reference omitted)).
- **Screen share:** Screen sharing on macOS 14.2 and later carries sound. macOS 12 and earlier share picture only ([#3145](internal development reference omitted)).
- **Screen share:** The share window keeps keyboard focus until you choose or close it ([#3204](internal development reference omitted)).
- **Screen share:** The screen-share volume slider shows only when you point at the video ([#3351](internal development reference omitted)).
- **Messages:** Rotating a DM's encryption key no longer locks everyone out of the conversation ([#3343](internal development reference omitted)).
- **Messages:** A DM with only a photo or file says what it is in your DM list, not "Encrypted message" ([#2364](internal development reference omitted)).
- **Messages:** Chats come back to where you left them, and Return to Latest stays at the bottom ([#2006](internal development reference omitted), [#3282](internal development reference omitted)).
- **Notifications:** The unread count on the app icon clears when you read the messages ([#2403](internal development reference omitted)).
- **Notifications:** The app icon stops bouncing after a notification ([#2403](internal development reference omitted)).
- **Presence:** If you drop offline, you come back on your own ([#3332](internal development reference omitted)).
- **Presence:** You no longer drop offline or lose your connection while Concord sits in the background ([#3328](internal development reference omitted)).
- **Presence:** Going invisible no longer knocks everyone else offline ([#3238](internal development reference omitted)).
- **Presence:** "Eligible audience" shows only when you point at your user area ([#3351](internal development reference omitted)).
- **Sign-in:** SSO sessions survive refresh when optional session metadata is missing ([#3290](internal development reference omitted)).
- **GIFs:** The GIF and emoji pickers sit against their buttons with an arrow, and are about 15% larger ([#3278](internal development reference omitted), [#2370](internal development reference omitted)).
- **GIFs:** Saving a GIF no longer sends you back to the top ([#3266](internal development reference omitted), [#2370](internal development reference omitted)).
- **Appearance:** Secondary text throughout the app is readable, including in custom themes ([#3341](internal development reference omitted)).
- **Appearance:** Midnight Sky's secondary text is readable on raised panels ([#3335](internal development reference omitted)).
- **Appearance:** The DM voice-call record is readable in every theme ([#3334](internal development reference omitted)).
- **Appearance:** 23 fixed colors now follow your theme in light themes ([#3270](internal development reference omitted)).
- **Appearance:** Accent-colored buttons keep a readable label, High Contrast included ([#3285](internal development reference omitted)).
- **Appearance:** Text on red and green buttons is legible in every theme ([#3285](internal development reference omitted)).
- **Appearance:** Server-enforced mutes show amber again, and missing hover highlights are back ([#3270](internal development reference omitted)).
- **Appearance:** The documents in Settings ▸ About have their dividers and boxes ([#3270](internal development reference omitted)).
- **Appearance:** Call buttons and badges follow your theme ([#3204](internal development reference omitted)).
- **Desktop:** The DM profile window opens in the middle of the screen ([#3351](internal development reference omitted)).
- **Desktop:** The installed app is about 37 MB smaller and no longer ships its source code or an internal test config ([#3156](internal development reference omitted)).

### Security

- **Messages:** Unread counts no longer leak from channels you can see but cannot read ([#2403](internal development reference omitted)).
- **Desktop:** Screen-share audio loads its native helper directly and refuses a substituted JavaScript module ([#3300](internal development reference omitted)).
- **Voice:** Voice rooms no longer stay full after everyone has left ([#3116](internal development reference omitted)).
- **Operations:** The control plane and media plane emit privacy-safe security events for Nightwatch ingestion ([#2273](internal development reference omitted)).
- **Sign-in:** Release-attestation tokens accept only the one signature algorithm GitHub uses ([#3207](internal development reference omitted)).
- **Sign-in:** Google and Apple sign-in accept only the exact signature type those providers use ([#3207](internal development reference omitted)).
- **Dependencies:** Desktop and Admin resolve patched Undici releases, and Worker deployments use the audited lockfile's Wrangler ([#3305](internal development reference omitted), [#3133](internal development reference omitted)).

---

## [0.2.46] - 2026-09-07

Voice connects on networks that block UDP, and Picture-in-Picture voice windows
get a private line and a one-time credential. Server updates no longer cut your
connection short, and a broken update can no longer pass as a success.

**9 Total Changes** | 7 Fixed | 2 Security

### Fixed

- **Voice:** Voice works on networks that block UDP, through the TCP and TLS relays the server offers ([#3104](internal development reference omitted)).
- **Voice:** Calls no longer spend setup time on a TCP route that no server accepts. Operators can still enable it ([#3105](internal development reference omitted)).
- **Voice:** Closing Picture-in-Picture voice during startup disconnects it cleanly ([#3128](internal development reference omitted)).
- **Presence:** Your voice and call status no longer disappears every hour ([#3102](internal development reference omitted)).
- **Server:** Server updates no longer cut your connection short ([#3106](internal development reference omitted)).
- **Operations:** An update that cannot reach its database fails its health check instead of passing ([#3106](internal development reference omitted)).
- **Messages:** Deleting a message retires its unshared attachment ([#3100](internal development reference omitted)).

### Security

- **Voice:** Picture-in-Picture voice windows get their own one-time credential and private channel ([#3104](internal development reference omitted)).
- **Messages:** Group deletion rechecks the administrator under lock ([#3100](internal development reference omitted)).

## [0.2.45] - 2026-09-02

Voice channels and private calls can be joined again. The desktop runtime moves
to Electron 43.5.1, and sign-in rejects extension data that Concord did not
request.

**8 Total Changes** | 5 Changed | 1 Fixed | 2 Security

### Changed

- **Desktop:** Electron 43.5.1 brings upstream Chromium, Node.js, and V8 fixes ([#3087](internal development reference omitted)).
- **Messages:** Large encrypted attachments can use consistently sized upload pieces on compatible clients ([#3086](internal development reference omitted)).
- **Video:** Video forwarding keeps temporal quality at or below the requested layer ([#3055](internal development reference omitted)).
- **Dependencies:** `lucide-react` and the media-plane `qs` dependency moved to reviewed releases ([#3073](internal development reference omitted), [#3098](internal development reference omitted)).
- **Dependencies:** Development and CI tooling was refreshed without runtime changes ([#3099](internal development reference omitted), [#3097](internal development reference omitted), [#3096](internal development reference omitted), [#3065](internal development reference omitted), [#3074](internal development reference omitted), [#3071](internal development reference omitted), [#3070](internal development reference omitted), [#3085](internal development reference omitted)).

### Fixed

- **Voice:** Voice channels and calls can be joined again. The minimum-version check no longer applies to internal service calls ([#3088](internal development reference omitted)).

### Security

- **Sign-in:** WebAuthn sign-in and enrollment reject client extension output that Concord did not request ([#3090](internal development reference omitted)).
- **Dependencies:** The control plane drops the advisory-affected MongoDB driver release and updates `quic-go` and `sonic` ([#3091](internal development reference omitted)).

## [0.2.44] - 2026-09-01

You can choose who sees your voice-channel and private-call activity. The
attachment stack is ready for the hosted storage cutover, and deletions keep
the media and activity cleanup they trigger.

**17 Total Changes** | 2 Added | 8 Changed | 7 Fixed

### Added

- **Presence:** Settings ▸ Rich Presence has audience and detail controls for server voice and private calls ([#3042](internal development reference omitted)).
- **Server:** The server can require a minimum desktop version before attachment format changes ([#3053](internal development reference omitted)).

### Changed

- **Presence:** The desktop keeps voice and call activity current internally ([#3038](internal development reference omitted)).
- **Messages:** Encrypted attachment uploads negotiate their storage format with the server ([#3044](internal development reference omitted)).
- **Operations:** Production validates the R2 attachment destination before cutover ([#3050](internal development reference omitted)).
- **Desktop:** Desktop utilities are organized by concern, with no behavior change ([#3045](internal development reference omitted)).
- **Dependencies:** `jose`, `lucide-react`, and the build-only `xmldom` resolution moved to reviewed releases ([#3069](internal development reference omitted), [#3062](internal development reference omitted), [#3078](internal development reference omitted)).
- **Dependencies:** Admin, desktop, and media-plane development tooling was refreshed ([#3068](internal development reference omitted), [#3063](internal development reference omitted), [#3064](internal development reference omitted), [#3066](internal development reference omitted), [#3067](internal development reference omitted), [#3059](internal development reference omitted), [#3056](internal development reference omitted), [#3060](internal development reference omitted)).
- **Dependencies:** Browser compatibility metadata no longer resolves to advisory-affected releases ([#3080](internal development reference omitted)).
- **Operations:** Weekly dependency triage fails closed on missing PR CI ([#3081](internal development reference omitted)).

### Fixed

- **Presence:** Deleting a server no longer leaves its voice activity visible after a restart ([#3054](internal development reference omitted)).
- **Presence:** Removing someone from a private conversation, or deleting its creator, no longer leaves stale voice activity ([#3052](internal development reference omitted)).
- **Messages:** Removing someone from a private conversation no longer deadlocks with a concurrent account deletion ([#3079](internal development reference omitted)).
- **Messages:** Encrypted attachments stay on the compatible storage format during rollouts ([#3051](internal development reference omitted)).
- **Messages:** Profile-media cleanup survives storage failures ([#3043](internal development reference omitted)).
- **Messages:** Large encrypted attachments are prepared for consistently sized upload pieces ([#3039](internal development reference omitted)).
- **Invites:** An invite preview no longer shows details from a different invite while it loads ([#3037](internal development reference omitted)).

## [0.2.43] - 2026-08-30

The app can reach the servers again after a certificate change locked every
installed copy out. Deleting your account removes the files it uploaded.

**5 Total Changes** | 1 Changed | 1 Fixed | 3 Security

### Changed

- **Desktop:** Desktop state is grouped by what it controls, with no behavior change ([#3033](internal development reference omitted)).

### Fixed

- **Desktop:** The app checks server certificates the way a browser does, so a provider certificate change no longer locks everyone out.

### Security

- **Desktop:** Switching between the app and the web version no longer leaves one unable to load your servers ([#3032](internal development reference omitted)).
- **Accounts:** Deleting your account removes the files you uploaded ([#3019](internal development reference omitted)).
- **Servers:** Changing a server's owner rechecks active voice access at once ([#2991](internal development reference omitted)).

## [0.2.42] - 2026-08-29

Interface fixes reach you again without an app update. Invite links arrive,
open, and show the server, and the GIF picker's Categories and Recent tabs
work.

**9 Total Changes** | 7 Fixed | 2 Security

### Fixed

- **Desktop:** Interface fixes reach you again without an app update ([#3014](internal development reference omitted)).
- **Invites:** "Open in Concord" on an invite link works ([#2363](internal development reference omitted)).
- **Invites:** An invite that arrived while you were offline appears when you return ([#2363](internal development reference omitted)).
- **Invites:** A server you join from a link appears in the sidebar at once ([#2363](internal development reference omitted)).
- **GIFs:** The GIF picker's Categories and Recent tabs show content ([#2976](internal development reference omitted)).
- **GIFs:** The GIF picker keeps its size when a tab is empty ([#2976](internal development reference omitted)).
- **Chat:** A burst of reconnections no longer holds up messages ([#2975](internal development reference omitted)).

### Security

- **Presence:** Someone who has just lost access can no longer be shown that you are online ([#3010](internal development reference omitted)).
- **Desktop:** Screen-capture protection reports itself as off where it cannot be enforced ([#2990](internal development reference omitted)).

## [0.2.41] - 2026-08-27

Attachments can be as large as your plan says, you can choose who may send you
a friend request, and server roles can be reordered. Encrypted voice and video
are no longer garbled on recent engine builds.

**12 Total Changes** | 3 Added | 1 Changed | 6 Fixed | 2 Security

### Added

- **Messages:** Attachments up to 256 MiB upload in encrypted pieces ([#2157](internal development reference omitted)).
- **Friends:** Settings ▸ Privacy sets who may send you a friend request ([#2888](internal development reference omitted), [#2911](internal development reference omitted)).
- **Servers:** Server roles can be reordered by drag, keyboard, or toolbar, with Apply Order ([#2359](internal development reference omitted)).

### Changed

- **Desktop:** Concord trusts a list of interface-signing keys, so a key change does not disable the cache ([#2958](internal development reference omitted)).

### Fixed

- **Voice:** Voice and video are no longer garbled on recent engine builds ([#2866](internal development reference omitted)).
- **Voice:** Picture-in-picture windows decrypt what they show ([#2870](internal development reference omitted)).
- **Chat:** The connection no longer drops every few minutes ([#2873](internal development reference omitted)).
- **Voice:** A voice server queue overflow no longer disconnects everyone on that machine ([#2868](internal development reference omitted)).
- **Messages:** Mentions in DMs show names again ([#2368](internal development reference omitted)).
- **Presence:** A restart no longer leaves your activity showing after you leave ([#2910](internal development reference omitted)).

### Security

- **Friends:** A storage fault no longer reveals which friend codes exist ([#2860](internal development reference omitted)).
- **Desktop:** An opt-in setting on macOS and Windows keeps Concord windows out of screen captures ([#2977](internal development reference omitted)).

---

## [0.2.40] - 2026-08-20

Friend codes have a web page of their own. It shows who the code belongs to and
offers to open Concord, and it looks the same whether the code is live or not.

**8 Total Changes** | 1 Added | 1 Changed | 5 Fixed | 1 Security

### Added

- **Friends:** Friend code links open a page with the owner's name and avatar, and a button that opens Concord. Dead codes show a neutral page of the same shape ([#945](internal development reference omitted)).

### Changed

- **Presence:** A friend or privacy change can delay a Custom Status save for up to 30 seconds, with a `Retry-After` header ([#2823](internal development reference omitted)).

### Fixed

- **Friends:** Several friend or invite links clicked quickly all open, in order ([#945](internal development reference omitted)).
- **Friends:** A brief server problem no longer reports a working friend code as dead ([#945](internal development reference omitted)).
- **Friends:** Revoking a friend code says when it fails ([#945](internal development reference omitted)).
- **Presence:** Leaving, or being kicked or banned from, a server stops your activity showing there at once ([#2447](internal development reference omitted)).
- **Presence:** Deleting a server or your account no longer leaves your activity visible to others ([#2447](internal development reference omitted)).

### Security

- **Friends:** A friend link clicked before sign-in no longer follows you into someone else's account ([#945](internal development reference omitted)).

## [0.2.39] - 2026-07-31

A self-hosted server address is checked when the app connects, not only when
you type it. A server on your own network needs your approval in a dialog that
the web layer cannot draw or click.

**1 Total Changes** | 1 Security

### Security

- **Self-hosting:** A compromised web layer can no longer make the app read your private network ([#2668](internal development reference omitted)).

## [0.2.38] - 2026-07-30

Encrypted channel keys reach every member of a busy server without stalling or
handing out a revoked key. The DM and server sidebars share one adaptive layout.

**14 Total Changes** | 2 Changed | 3 Fixed | 9 Security

### Changed

- **Desktop:** The DM and server sidebars share one adaptive layout ([#2510](internal development reference omitted)).
- **Dependencies:** mediasoup 3.22.0 and the current React release ([#2513](internal development reference omitted), [#2515](internal development reference omitted), [#2517](internal development reference omitted)).

### Fixed

- **Encryption:** Channel keys are delivered in batches ([#2503](internal development reference omitted)).
- **Encryption:** Key distribution can be cancelled while it waits to retry ([#2541](internal development reference omitted)).
- **Encryption:** Key recovery resumes after a repeated rotation ([#2539](internal development reference omitted)).

### Security

- **Sign-in:** Backing out of a social sign-in no longer costs a later password sign-up its encryption keys ([#2655](internal development reference omitted)).
- **Sign-in:** Two-step verification codes are compared in constant time ([#2654](internal development reference omitted)).
- **Encryption:** The server refuses key distribution into a revoked epoch ([#2534](internal development reference omitted)).
- **Encryption:** Manual channel rotation is fenced to the current credentials ([#2540](internal development reference omitted)).
- **Encryption:** A channel's active key epoch survives concurrent writes ([#2542](internal development reference omitted)).
- **Sign-in:** Signing out other sessions keeps the one you are using ([#2501](internal development reference omitted)).
- **Presence:** Rich Presence suppression fails closed ([#2522](internal development reference omitted)).
- **Presence:** Returning from invisible does not replay a stale suppression ([#2502](internal development reference omitted)).
- **Sign-in:** Static-analysis findings in authentication code are resolved ([#2525](internal development reference omitted)).

## [0.2.37] - 2026-07-27

A stolen or superseded token can no longer outlive the reset meant to end it,
and a disabled account is disconnected at once. Recovery keys use P-384.

**8 Total Changes** | 3 Fixed | 5 Security

### Fixed

- **Desktop:** Electron 43.1.1 fixes crashes when the app archive is replaced while Concord runs ([#2492](internal development reference omitted)).
- **Encryption:** The app bounds how many key requests it sends at once ([#2482](internal development reference omitted)).
- **Messages:** Bulk message removal records its progress, so an interrupted purge can finish ([#2483](internal development reference omitted)).

### Security

- **Sign-in:** Linking a social account is bound to the password you just verified ([#2458](internal development reference omitted)).
- **Sign-in:** Token-theft revocation and session creation cannot interleave ([#2460](internal development reference omitted), [#2457](internal development reference omitted)).
- **Sign-in:** A disabled account is disconnected from an open connection ([#2480](internal development reference omitted)).
- **Encryption:** Recovery keys use P-384, and weaker curves are rejected ([#2355](internal development reference omitted)).
- **Presence:** Going invisible or offline clears activity that was already published ([#2461](internal development reference omitted)).

## [0.2.36] - 2026-07-26

Desktop actions that touch your camera, clipboard, updates, or permissions
verify that the request came from Concord Voice itself.

**1 Total Changes** | 1 Security

### Security

- **Desktop:** Screen capture, clipboard, update, permission, lifecycle, and developer controls act only for a permitted Concord frame ([#2351](internal development reference omitted)).

## [0.2.35] - 2026-07-25

Windows updates install when you click Restart.

**1 Total Changes** | 1 Fixed

### Fixed

- **Desktop:** Windows updates install from the Restart button, and leftover installer files are cleaned up later ([#2402](internal development reference omitted)).

## [0.2.34] - 2026-07-25

This release carries everything from 0.2.33, which was never published. Google
and Apple sign-in are bound to the current account state, and the "can't reach
Concord servers" warning clears itself.

**4 Total Changes** | 1 Fixed | 3 Security

### Fixed

- **Desktop:** The "can't reach Concord servers" warning clears itself once the app connects ([#2401](internal development reference omitted)).

### Security

- **Sign-in:** Social sign-in is bound to your current account state ([#2453](internal development reference omitted)).
- **Dependencies:** The desktop router update closes GHSA-QWWW-VCR4-C8H2 ([#2442](internal development reference omitted)).
- **Encryption:** Key self-healing checks the recipient's current key version ([#2440](internal development reference omitted)).

## [0.2.33] - 2026-07-24

This version was never published, and its contents shipped in 0.2.34.
Sessions, encryption keys, and password changes are tied to the exact
credentials that authorized them.

**14 Total Changes** | 6 Fixed | 8 Security

### Fixed

- **Encryption:** Key recovery stays reachable after a password sign-in ([#2435](internal development reference omitted)).
- **Presence:** Custom Status visibility settles consistently ([#2419](internal development reference omitted)).
- **Presence:** Rich Presence cleanup resumes after an interruption ([#2408](internal development reference omitted)).
- **Voice:** Private calls appear only once both sides are admitted ([#2407](internal development reference omitted)).
- **Presence:** Hidden presence stays hidden across servers ([#2404](internal development reference omitted), [#2405](internal development reference omitted), [#2232](internal development reference omitted)).
- **Sign-in:** Sign-in and voice lifecycle code is simpler, with no behavior change ([#2421](internal development reference omitted)).

### Security

- **Sign-in:** Two-step verification during a social sign-in completes inside the protected process ([#2436](internal development reference omitted)).
- **Encryption:** Encryption keys are committed by the sign-in that created them ([#2434](internal development reference omitted)).
- **Sign-in:** A password change that succeeded is never reported as a failure ([#2431](internal development reference omitted)).
- **Encryption:** Key delivery checks the recipient against a concurrent key reset ([#2430](internal development reference omitted)).
- **Sign-in:** A slow rejected request cannot end a rotated session ([#2432](internal development reference omitted)).
- **Sign-in:** Grace-period session refresh follows an exact token lineage ([#2428](internal development reference omitted)).
- **Sign-in:** Every session is bound to the credentials that authorized it ([#2427](internal development reference omitted), [#2397](internal development reference omitted)).
- **Encryption:** A password change re-encrypts every encrypted setting in one transaction ([#2395](internal development reference omitted)).

---

## [0.2.32] - 2026-07-22

Sign-in and session refresh stay with the account and server you chose, even
when two sign-ins overlap or you switch servers.

**2 Total Changes** | 1 Changed | 1 Fixed

### Changed

- **Dependencies:** mediasoup, Vite, the icon set, and the media server's request parser moved to current releases ([#2379](internal development reference omitted), [#2384](internal development reference omitted), [#2380](internal development reference omitted), [#2389](internal development reference omitted)).

### Fixed

- **Sign-in:** Sign-in and refresh stay with the right account and server, and a superseded sign-in clears its keys from memory ([#2374](internal development reference omitted)).

## [0.2.31] - 2026-07-22

Groundwork for showing what your friends are doing. The server can publish
voice and call activity, and the desktop validates it without showing it yet.

**1 Total Changes** | 1 Changed

### Changed

- **Presence:** The server can publish authorized voice and call activity, and the desktop validates the new format ([#2391](internal development reference omitted)).

## [0.2.30] - 2026-07-18

Reconnecting after a network drop restores what changed while you were away.
Moderators can remove a member's messages when they kick or ban them, and Apple
sign-in works on desktop.

**8 Total Changes** | 3 Added | 2 Fixed | 3 Security

### Added

- **Servers:** Kicking or banning a member can also purge their messages in that server ([#2342](internal development reference omitted)).
- **Operations:** Operators see aggregate account activity in an Admin workspace, as counts only ([#2338](internal development reference omitted)).
- **Operations:** Operators can rotate the two-step verification encryption key safely ([#2345](internal development reference omitted)).

### Fixed

- **Chat:** Reconnecting refreshes your memberships and back-fills the open conversation, and keeps drafts and queued messages ([#2327](internal development reference omitted), [#2358](internal development reference omitted)).
- **Sign-in:** Apple sign-in completes on desktop through the hosted bridge ([#2323](internal development reference omitted)).

### Security

- **Encryption:** A password change that loses your encryption keys signs you out ([#2357](internal development reference omitted)).
- **Encryption:** A sign-in that has ended cannot clear the keys of the one that replaced it ([#2337](internal development reference omitted)).
- **Server:** Security headers are sent once, from one place ([#2328](internal development reference omitted)).

## [0.2.29] - 2026-07-15

Moderators can remove a member's messages in bulk. The codec in Settings is the
one in use, and H.264 calls are encrypted per access unit. Operators get an
Admin console.

**8 Total Changes** | 3 Added | 3 Fixed | 2 Security

### Added

- **Servers:** Moderators can delete a member's messages across a channel, server, or conversation. The audit record keeps counts only ([#2290](internal development reference omitted)).
- **Operations:** The Admin Portal console shows instance health and aggregate metrics ([#2268](internal development reference omitted)).
- **Presence:** Rich Presence category settings persist across restarts ([#2275](internal development reference omitted)).

### Fixed

- **Video:** The codec shown in Settings is the one in use ([#2242](internal development reference omitted)).
- **Video:** H.264 video is encrypted per access unit, and HDR target selection is corrected ([#2298](internal development reference omitted)).
- **Operations:** Admin metrics stay in bounds, are labelled, and include host telemetry ([#2286](internal development reference omitted), [#2287](internal development reference omitted), [#2281](internal development reference omitted)).

### Security

- **Operations:** Desktop release signing is isolated from pull requests ([#2296](internal development reference omitted)).
- **Presence:** The server authorizes voice activity before it publishes it ([#2282](internal development reference omitted)).

## [0.2.28] - 2026-07-14

DM call previews say what happened, and Custom Status counts characters the way
you would.

**3 Total Changes** | 1 Changed | 2 Fixed

### Changed

- **Dependencies:** Desktop build and test tooling was refreshed ([#2265](internal development reference omitted), [#2264](internal development reference omitted), [#2261](internal development reference omitted)).

### Fixed

- **Messages:** A DM call entry describes its outcome ([#2245](internal development reference omitted)).
- **Presence:** Custom Status counts characters, not bytes ([#2250](internal development reference omitted)).

## [0.2.27] - 2026-07-14

Activity History arrives: an opt-in, self-only record of your own voice and
call time. A call that drops to software encoding re-selects its codec.

**7 Total Changes** | 2 Added | 4 Fixed | 1 Security

### Added

- **Privacy:** Activity History is an opt-in, self-only record of your voice and call intervals, pruned on your schedule ([#1235](internal development reference omitted)).
- **Operations:** A read-only admin metrics API serves aggregate counts ([#2228](internal development reference omitted), [#2218](internal development reference omitted)).

### Fixed

- **Video:** Video re-selects a hardware-capable codec mid-call after a software fallback ([#2187](internal development reference omitted)).
- **Chat:** A late layout change no longer scrolls you away from the newest message ([#2227](internal development reference omitted)).
- **Messages:** Edited encrypted messages display correctly ([#2217](internal development reference omitted)).
- **Desktop:** The outgoing-call and What's New dialogs no longer appear behind other windows ([#2225](internal development reference omitted)).

### Security

- **Desktop:** The updater's parser no longer spends excessive CPU on crafted recovery-feed YAML ([#2247](internal development reference omitted)).

## [0.2.26] - 2026-07-12

Everyone can share their screen at once, each stream has its own volume and
mute, and viewers get the resolution their window needs. Codec selection checks
the live call for hardware encoding.

**19 Total Changes** | 4 Added | 2 Changed | 8 Fixed | 5 Security

### Added

- **Screen share:** Everyone can share at once, and each viewer gets the quality that fits their window ([#2160](internal development reference omitted), [#2185](internal development reference omitted)).
- **Screen share:** Each shared screen has its own volume and mute ([#2169](internal development reference omitted)).
- **Voice:** A voice tile view fills the available space ([#2177](internal development reference omitted)).
- **Presence:** You can hide your Custom Status from specific people ([#2191](internal development reference omitted)).

### Changed

- **Billing:** Subscriptions return to the free tier on schedule when a plan period ends ([#2168](internal development reference omitted)).
- **Settings:** Settings ▸ Sounds and Notifications is now Notifications ([#2188](internal development reference omitted)).

### Fixed

- **Video:** Hardware video encoding is confirmed from the live call ([#2184](internal development reference omitted), [#2189](internal development reference omitted)).
- **Voice:** Joining during a brief server blip retries instead of failing ([#2181](internal development reference omitted)).
- **Voice:** Picture-in-picture windows close promptly when a call ends ([#2215](internal development reference omitted)).
- **Screen share:** System audio is captured only for whole-screen shares ([#2165](internal development reference omitted)).
- **Screen share:** Tuning into a second screen keeps the focused one ([#2156](internal development reference omitted)).
- **Screen share:** Quality demand no longer flaps for AV1 and VP9 shares ([#2212](internal development reference omitted)).
- **Desktop:** Interface and app update states show separately ([#2183](internal development reference omitted)).
- **Notifications:** Follow-up fixes from the desktop notification work ([#2167](internal development reference omitted)).

### Security

- **Voice:** The media server checks permission to speak or share ([#2140](internal development reference omitted)).
- **Voice:** An empty server-supplied voice identity fails closed ([#2152](internal development reference omitted)).
- **Video:** Channel video limits follow the server's plan, not the owner's ([#2175](internal development reference omitted)).
- **Screen share:** Screen-share resolution and frame rate follow your plan as a combined pixel rate ([#2172](internal development reference omitted)).
- **Server:** Shutdown drains in-flight requests before closing live connections ([#2203](internal development reference omitted), [#2149](internal development reference omitted), [#2214](internal development reference omitted)).

## [0.2.25] - 2026-07-09

A full audit of channel and message permissions checks visibility on every path
that can reveal it. The composer adds emoji autocomplete and picker shortcuts.

**18 Total Changes** | 4 Added | 4 Fixed | 10 Security

### Added

- **Messages:** Type a `:shortcode:` for inline emoji suggestions ([#2081](internal development reference omitted)).
- **Messages:** Ctrl+E opens emoji and Ctrl+G opens GIFs from the composer ([#2079](internal development reference omitted)).
- **Screen share:** You choose which shared screens to watch ([#2126](internal development reference omitted)).
- **Bug reports:** A preview shows the redacted diagnostic log before you send a report ([#2086](internal development reference omitted), [#2082](internal development reference omitted)).

### Fixed

- **Messages:** The composer no longer jumps when a picker first opens ([#2075](internal development reference omitted), [#2085](internal development reference omitted)).
- **Messages:** Emoji-only messages render large again ([#2073](internal development reference omitted)).
- **Settings:** Privacy and Security settings no longer sign you out ([#2067](internal development reference omitted)).
- **Accessibility:** Dialogs trap focus and announce themselves to screen readers ([#2089](internal development reference omitted)).

### Security

- **Servers:** Channel metadata, unread counts, attachments, keys, broadcasts, and voice lists require permission to view the channel ([#2134](internal development reference omitted), [#2136](internal development reference omitted), [#2138](internal development reference omitted), [#2132](internal development reference omitted), [#2130](internal development reference omitted)).
- **Servers:** Kicked or banned members are evicted from live connections ([#2133](internal development reference omitted), [#2135](internal development reference omitted)).
- **Privacy:** Group conversation creation respects your privacy settings ([#2127](internal development reference omitted)).
- **Servers:** The database refuses to bind a channel to another server's category ([#2131](internal development reference omitted)).
- **Sign-in:** SSO state and one-time tokens are consumed atomically, and attestation tokens are bound to their account ([#2129](internal development reference omitted), [#2139](internal development reference omitted)).
- **Voice:** Voice display names and avatars come from the server ([#2143](internal development reference omitted)).
- **Operations:** Admin enrollment links keep their token out of the URL ([#2137](internal development reference omitted)).
- **Server:** The server refuses a wildcard origin in production ([#2128](internal development reference omitted)).
- **Dependencies:** The tar library update blocks malicious archives in build tooling ([#2142](internal development reference omitted)).
- **Sign-in:** Running out of verification attempts returns the same shape as other failures ([#2083](internal development reference omitted)).

## [0.2.24] - 2026-07-05

Username search, the friend-request sound, password-reset email, and the
verification-code window work again. Voice adds a stage that shows who is
talking.

**11 Total Changes** | 2 Added | 8 Fixed | 1 Security

### Added

- **Voice:** A voice stage puts the active speaker in front and marks the call as encrypted ([#2059](internal development reference omitted), [#2062](internal development reference omitted)).
- **Sign-in:** The sign-in screen has a show-password toggle ([#2061](internal development reference omitted)).

### Fixed

- **Encryption:** The app notices when your keychain cannot save your keys for next launch ([#2068](internal development reference omitted)).
- **Friends:** Searching for a user by name works again ([#2057](internal development reference omitted)).
- **Friends:** Friend requests make a sound again ([#2054](internal development reference omitted)).
- **Sign-in:** Password-reset email is delivered ([#2058](internal development reference omitted)).
- **Sign-in:** The email verification code window is long enough to use ([#2055](internal development reference omitted)).
- **Voice:** One-to-one calls show a single bottom bar ([#2056](internal development reference omitted)).
- **Settings:** Settings and help overlays can be closed without the keyboard ([#2064](internal development reference omitted)).
- **GIFs:** A GIF that fails to load no longer signs you out ([#2051](internal development reference omitted)).

### Security

- **Server:** The server refuses test-mode configuration in production ([#2060](internal development reference omitted)).

---

## [0.2.23] - 2026-07-04

You can see and manage your subscription in the app, and call quality, upload
limits, and server features follow your plan. When bandwidth is tight, Concord
drops webcam first, then screen share, so voice stays clear.

**6 Total Changes** | 4 Added | 1 Changed | 1 Fixed

### Added

- **Billing:** Settings ▸ Subscription shows your plan and status and redeems codes ([#2043](internal development reference omitted)).
- **Billing:** Call quality, per-room camera and screen-share limits, and server features follow your plan. Webcam drops first, then screen share, before audio ([#2042](internal development reference omitted), [#2039](internal development reference omitted), [#2038](internal development reference omitted)).
- **Billing:** Server-creation and search-depth limits scale with your subscription ([#2040](internal development reference omitted)).
- **GIFs:** The GIF pipeline keeps animation and guards against oversized files ([#2041](internal development reference omitted)).

### Changed

- **Operations:** Release workflows use tighter least privilege, and the build toolchain and dependencies were refreshed ([#2049](internal development reference omitted), [#2046](internal development reference omitted), [#2036](internal development reference omitted), [#2035](internal development reference omitted)).

### Fixed

- **Sign-in:** SSO sign-up gets a 15-minute token window and recovers if it expires ([#2047](internal development reference omitted)).

## [0.2.22] - 2026-07-02

After you update, the next launch tells you what changed. This file became the
public record of every release, and CI refuses a version without an entry.

**2 Total Changes** | 2 Added

### Added

- **Desktop:** A "What's new" dialog covers every version since the one you had. It shows once and works offline ([#2034](internal development reference omitted)).
- **Desktop:** This changelog is the canonical public record, and CI refuses a release without its entry ([#2034](internal development reference omitted)).

## [0.2.21] - 2026-07-02

Windows verifies an update's signature before it installs, and public releases
carry build provenance you can check. Pinning works again, and scroll stays
where you left it.

**12 Total Changes** | 1 Added | 1 Changed | 7 Fixed | 3 Security

### Added

- **Billing:** Avatar, banner, and server-icon size limits scale with your plan ([#2010](internal development reference omitted)).

### Changed

- **Operations:** Tooling, CI, and documentation upkeep ([#2016](internal development reference omitted), [#2017](internal development reference omitted), [#2019](internal development reference omitted), [#2025](internal development reference omitted)).

### Fixed

- **Messages:** Server message pinning works again, and pinned media stays in its panel ([#2011](internal development reference omitted)).
- **Messages:** Switching channels quickly no longer snaps you to a stale position ([#2008](internal development reference omitted)).
- **Notifications:** DM and notification previews say "Photo" instead of a placeholder ([#2015](internal development reference omitted)).
- **Messages:** The composer menu renders above the placeholder text ([#2014](internal development reference omitted)).
- **Server:** The client no longer makes permission checks it was never entitled to make ([#1990](internal development reference omitted), [#2009](internal development reference omitted)).
- **Desktop:** UI and app update checks share one path ([#2013](internal development reference omitted)).
- **Sign-in:** The suggested identity at sign-up no longer carries a stray "@concordvoice.chat" suffix ([#2018](internal development reference omitted)).

### Security

- **Desktop:** Windows checks the code signature of each update against the expected publisher ([#2023](internal development reference omitted)).
- **Operations:** Every publicly mirrored desktop release asset carries verifiable build provenance ([#1988](internal development reference omitted)).
- **Operations:** A misconfigured attestation deployment fails loudly ([#2024](internal development reference omitted)).

## [0.2.20] - 2026-06-30

Self-hosted servers get every feature with no plan limits. The licence, privacy
policy, and terms are one click away in About.

**2 Total Changes** | 2 Added

### Added

- **Self-hosting:** Self-hosted servers unlock every premium entitlement with no subscription ([#1985](internal development reference omitted)).
- **Settings:** Settings ▸ About shows the licence, terms, and other notices ([#1987](internal development reference omitted)).

## [0.2.19] - 2026-06-30

You can sign in to a Concord Voice server that you run yourself. A client stuck
on an old interface can recover without a reinstall.

**3 Total Changes** | 1 Added | 1 Changed | 1 Fixed

### Added

- **Self-hosting:** Desktop sign-in can route to an operator-run server ([#1982](internal development reference omitted)).

### Changed

- **Operations:** Deployment and tooling upkeep.

### Fixed

- **Desktop:** The app checks the public release feed for updates, so a stranded client can recover ([#1984](internal development reference omitted)).

## [0.2.18] - 2026-06-29

Certificate pinning is back on for connections to Concord Voice servers.

**1 Total Changes** | 1 Security

### Security

- **Desktop:** Certificate pinning for connections to Concord services is restored after it lapsed ([#1983](internal development reference omitted)).

## [0.2.17] - 2026-06-29

You can react to DMs with emoji. The app connects reliably at launch, and a
certificate rotation no longer locks you out.

**4 Total Changes** | 1 Added | 1 Changed | 2 Fixed

### Added

- **Messages:** DMs take emoji reactions ([#1976](internal development reference omitted)).

### Changed

- **Operations:** Tooling, CI, and dependency upkeep ([#1979](internal development reference omitted)).

### Fixed

- **Chat:** A race in the WebSocket sign-in handshake no longer leaves you disconnected after launch ([#1978](internal development reference omitted)).
- **Desktop:** The client trusts the rotated API certificate pin ([#1980](internal development reference omitted)).

## [0.2.16] - 2026-06-29

You choose how much a notification shows on screen. Custom themes are free, and
opening DMs no longer interrupts your call.

**5 Total Changes** | 2 Added | 1 Changed | 2 Fixed

### Added

- **Notifications:** A setting controls how much message content desktop notifications show ([#1970](internal development reference omitted)).
- **Appearance:** Custom themes are free for everyone ([#1974](internal development reference omitted)).

### Changed

- **Operations:** A legacy automation credential was retired, and the supply-chain list refresh is guarded ([#1968](internal development reference omitted), [#1969](internal development reference omitted)).

### Fixed

- **Voice:** Call audio keeps playing when you open DMs ([#1972](internal development reference omitted)).
- **Desktop:** macOS relaunches after an update installs ([#1973](internal development reference omitted)).

## [0.2.15] - 2026-06-29

Moderators can time a member out instead of removing them. Passkey registration
is stricter, and erasing your account revokes the session that asked for it.

**5 Total Changes** | 2 Added | 1 Changed | 2 Security

### Added

- **Servers:** Moderators can time a member out for a set duration ([#1967](internal development reference omitted)).
- **Desktop:** On first launch outside /Applications, macOS offers to move the app there ([#1966](internal development reference omitted)).

### Changed

- **Operations:** Tooling, CI, and deployment upkeep.

### Security

- **Sign-in:** Passkey registration requires platform-authenticator options ([#1963](internal development reference omitted)).
- **Accounts:** Erasing your account revokes your current access token ([#1965](internal development reference omitted)).

## [0.2.14] - 2026-06-28

A voice channel can set one audio quality for everyone in it. Usernames are
case-insensitive everywhere, and self-hosted certificate failures are reported.

**10 Total Changes** | 3 Added | 2 Changed | 3 Fixed | 2 Security

### Added

- **Voice:** A channel can set one audio quality tier for every member, bounded by the server's tier ([#1946](internal development reference omitted)).
- **Video:** Settings ▸ Audio & Video has SVC and simulcast toggles ([#1929](internal development reference omitted)).
- **Self-hosting:** One script provisions a self-signed, Let's Encrypt, or bring-your-own certificate ([#1919](internal development reference omitted)).

### Changed

- **Settings:** My Profile moved into Settings ▸ Account, and the SSO controls were polished ([#1930](internal development reference omitted), [#1951](internal development reference omitted)).
- **Operations:** Tooling, CI, and dependency upkeep.

### Fixed

- **Accounts:** Usernames are case-insensitive everywhere ([#1936](internal development reference omitted)).
- **Notifications:** Notification settings match the macOS permission state ([#1960](internal development reference omitted)).
- **Self-hosting:** Self-hosted certificate provisioning errors are reported ([#1964](internal development reference omitted)).

### Security

- **Sign-in:** Email MFA setup is hardened for production ([#1961](internal development reference omitted)).
- **Desktop:** The cached-UI origin is on the server allowlists before the cache activates ([#1928](internal development reference omitted)).

## [0.2.13] - 2026-06-28

Invite bubbles name the person who invited you. Linux updates are verified
before they install, and the signed offline interface cache is live.

**5 Total Changes** | 1 Changed | 2 Fixed | 2 Security

### Changed

- **Operations:** A retired sign-in configuration value was removed ([#1918](internal development reference omitted), [#1920](internal development reference omitted)).

### Fixed

- **Invites:** Invite bubbles name the person who sent the invite ([#1909](internal development reference omitted)).
- **Settings:** System Permissions settings are easier to find and understand ([#1910](internal development reference omitted)).

### Security

- **Desktop:** Linux AppImage, deb, and rpm updates carry Ed25519 signatures, and an unverified update does not install ([#1923](internal development reference omitted)).
- **Desktop:** The signed offline UI cache is live and verified before it runs ([#1907](internal development reference omitted), [#1908](internal development reference omitted)).

## [0.2.12] - 2026-06-27

Your camera and screen share stay live when the call switches video quality
layers.

**3 Total Changes** | 1 Changed | 2 Fixed

### Changed

- **Operations:** Engineering write-ups, storage-config groundwork, and static-analysis cleanup ([#1900](internal development reference omitted), [#1901](internal development reference omitted), [#1906](internal development reference omitted)).

### Fixed

- **Video:** Camera and screen share stay live when video layers switch ([#1903](internal development reference omitted)).
- **Accounts:** Age-verification status survives a reload ([#1904](internal development reference omitted)).

## [0.2.11] - 2026-06-26

Hardware video encoding is offered only when your graphics hardware supports
it.

**1 Total Changes** | 1 Fixed

### Fixed

- **Video:** Hardware encoding uses only codecs your GPU accelerates ([#1879](internal development reference omitted)).

## [0.2.10] - 2026-06-26

You can run your own Concord Voice instance. AV1 video no longer goes black in
encrypted calls.

**3 Total Changes** | 1 Added | 1 Changed | 1 Fixed

### Added

- **Self-hosting:** A guided installer sets up a self-hosted instance, and `concord-selfhost` manages it ([#1892](internal development reference omitted)).

### Changed

- **Operations:** Supply-chain indicators refreshed, and macOS builds retry a transient DMG flake ([#1877](internal development reference omitted), [#1894](internal development reference omitted)).

### Fixed

- **Video:** AV1 video decrypts reliably in encrypted calls (frame crypto v4) ([#1896](internal development reference omitted)).

## [0.2.9] - 2026-06-24

Video no longer goes black after a key rotates mid-call, and the offline
interface cache is signed.

**6 Total Changes** | 2 Added | 1 Changed | 2 Fixed | 1 Security

### Added

- **Desktop:** The app keeps a signed last-known-good copy of the interface ([#1880](internal development reference omitted)).
- **Server:** `GET /api/v1/server/capabilities` tells a client what a server supports ([#1883](internal development reference omitted)).

### Changed

- **Operations:** Tooling and workflow upkeep ([#1884](internal development reference omitted), [#1888](internal development reference omitted)).

### Fixed

- **Video:** Video frames carry their key version, so video survives a mid-call key rotation ([#1885](internal development reference omitted)).
- **Operations:** Public release mirroring triggers only on real releases ([#1887](internal development reference omitted)).

### Security

- **Desktop:** Packaged builds accept only the official API address or a verified self-hosted server ([#1882](internal development reference omitted)).

## [0.2.8] - 2026-06-24

Audio devices switch mid-call, DM calls open with sound, and encrypted video
keeps up with layered streams. Older DM attachments decrypt again.

**9 Total Changes** | 2 Changed | 7 Fixed

### Changed

- **Invites:** The invite landing page is restyled ([#1869](internal development reference omitted)).
- **Operations:** Tooling, CI, and test upkeep, including automated contrast checks ([#1864](internal development reference omitted), [#1871](internal development reference omitted), [#1875](internal development reference omitted)).

### Fixed

- **Desktop:** The client retries the latest UI after a cold-start network problem, and session-only sign-in survives a reload ([#1872](internal development reference omitted)).
- **Video:** Encrypted video stays in sync with layered and simulcast streams ([#1865](internal development reference omitted)).
- **Voice:** You can switch audio devices mid-call ([#1866](internal development reference omitted)).
- **Voice:** DM calls open the call view and play remote audio ([#1876](internal development reference omitted)).
- **Messages:** Older DM attachments decrypt correctly ([#1867](internal development reference omitted)).
- **Presence:** Your DM list shows accurate online status ([#1874](internal development reference omitted)).
- **Messages:** Enlarged images are centered ([#1868](internal development reference omitted)).

## [0.2.7] - 2026-06-23

> Versions 0.2.4 to 0.2.6 never shipped. Everything they carried ships here.

Images open in a lightbox, and invite links can be public. Camera quality adapts
in group calls, and video recovers once a key catches up.

**10 Total Changes** | 3 Added | 1 Changed | 5 Fixed | 1 Security

### Added

- **Messages:** Click an image to see it full size, zoom, and save it ([#1813](internal development reference omitted)).
- **Invites:** Public invite links open a landing page ([#1821](internal development reference omitted)).
- **Video:** SVC-first camera layers send each viewer the resolution they need ([#1831](internal development reference omitted)).

### Changed

- **Dependencies:** Electron 42.4.1, mediasoup 3.20.9, and release-pipeline upkeep.

### Fixed

- **Video:** Encrypted video recovers after a key epoch desync ([#1784](internal development reference omitted)).
- **Voice:** Output device changes apply mid-call ([#1823](internal development reference omitted)).
- **Messages:** A batch of chat and UI fixes for DMs, mentions, lists, code, menus, and voice frames ([#1798](internal development reference omitted) to [#1836](internal development reference omitted)).
- **Desktop:** Linux packages launch reliably, show menu icons, and keep RPM sandbox permissions ([#1801](internal development reference omitted), [#1825](internal development reference omitted), [#1863](internal development reference omitted)).
- **Server:** Media uploads return "service unavailable" when storage is disabled ([#1826](internal development reference omitted)).

### Security

- **Servers:** Channel permissions are enforced on the WebSocket path as well as REST ([#1811](internal development reference omitted)).

## [0.2.3] - 2026-06-22

The composer shows how many characters you have left and keeps what you typed
when you go over.

**2 Total Changes** | 1 Added | 1 Changed

### Added

- **Messages:** A live character counter flags over-limit text before you send ([#1709](internal development reference omitted)).

### Changed

- **Operations:** Build, scanning, and documentation upkeep ([#1780](internal development reference omitted), [#1781](internal development reference omitted), [#1764](internal development reference omitted), [#1783](internal development reference omitted)).

## [0.2.2] - 2026-06-22

Premium features show what they unlock and can be redeemed with a code. Voice
crackle is fixed, and the app reconnects after a server deploy.

**8 Total Changes** | 5 Added | 1 Changed | 2 Fixed

### Added

- **Billing:** Premium lock badges, redemption codes, and server-enforced quality limits per plan ([#1724](internal development reference omitted), [#1730](internal development reference omitted), [#1737](internal development reference omitted)).
- **Feedback:** A public Known Issues list is linked from the feedback dialog ([#1736](internal development reference omitted), [#1769](internal development reference omitted)).
- **Desktop:** Settings ▸ About has a "Load latest UI" button, and launch retries it ([#1779](internal development reference omitted)).
- **Operations:** Operator console sign-in needs a password and a WebAuthn hardware key ([#1703](internal development reference omitted)).
- **GIFs:** New accounts load KLIPY GIFs automatically. Existing choices do not change ([#1774](internal development reference omitted)).

### Changed

- **Operations:** Feedback triage, mirror fixes, and opt-in audio diagnostics ([#1731](internal development reference omitted), [#1733](internal development reference omitted), [#1762](internal development reference omitted), [#1771](internal development reference omitted), [#1772](internal development reference omitted), [#1775](internal development reference omitted), [#1776](internal development reference omitted)).

### Fixed

- **Voice:** Small calls no longer clip or garble the first words of speech ([#1765](internal development reference omitted), [#1777](internal development reference omitted)).
- **Chat:** The app reconnects after a server deploy, and a sign-in blip keeps Remember Me ([#1770](internal development reference omitted)).

---

## [0.2.1] - 2026-06-20

> Patch release. v0.2.0-Beta shipped the macOS client as a `.zip` only. This
> release attaches the signed and notarized `.dmg` installer as well.

Friend categories arrive: group your DMs with your own emoji and colors. The
grouping is encrypted with your key, so the server cannot read it.

**2 Total Changes** | 1 Added | 1 Fixed

### Added

- **Friends:** Friend categories group your DMs, encrypted with your key ([#1704](internal development reference omitted)).

### Fixed

- **Desktop:** Releases attach the macOS `.dmg` installer as `ConcordVoice-<version>-macos-<arch>.dmg`. Auto-update stays on the `.zip` ([#1722](internal development reference omitted)).

## [0.2.0-Beta] - 2026-06-20

> Release-level rollup of Phase 2A and Phase 2B. Per-revision detail is in the
> 0.1.12 to 0.1.18 entries below.

The Beta release. Sign in with Google or Apple, add two-step verification or a
passkey, and recover a locked account. Roles and permissions arrive with GIFs,
server moderation, and end-to-end encrypted attachments.

**101 Total Changes** | 38 Added | 8 Changed | 3 Removed | 30 Fixed | 22 Security

### Added

- **Sign-in:** Google sign-in and registration ([#808](internal development reference omitted)).
- **Sign-in:** Sign in with Apple, aware of Apple's private relay ([#824](internal development reference omitted)).
- **Security:** TOTP, WebAuthn/FIDO2, backup codes, recovery circles, and trusted devices ([#202](internal development reference omitted)).
- **Accounts:** `POST /api/v1/privacy/erase-account` erases an account across every linked table ([#717](internal development reference omitted)).
- **Accounts:** Zero-knowledge account key recovery ([#328](internal development reference omitted)).
- **Servers:** Role- and server-based access control with audit logging ([#242](internal development reference omitted), [#548](internal development reference omitted)).
- **Server:** User images moved from PostgreSQL to S3-compatible storage with two-tier access ([#325](internal development reference omitted)).
- **Servers:** Server ownership transfer with MFA, email confirmation, and reversal tokens ([#351](internal development reference omitted)).
- **Sign-in:** Email verification on registration, later sent through Resend ([#273](internal development reference omitted)).
- **Sign-in:** Registrations stay pending for 15 minutes until verified ([#688](internal development reference omitted)).
- **Messages:** Reactions, replies, pinning, encrypted search, attachments, drafts, notifications, shortcuts, group DMs, and extended Markdown ([#459](internal development reference omitted), [#463](internal development reference omitted), [#465](internal development reference omitted), [#468](internal development reference omitted), [#470](internal development reference omitted), [#477](internal development reference omitted), [#478](internal development reference omitted), [#479](internal development reference omitted), [#472](internal development reference omitted), [#711](internal development reference omitted)).
- **GIFs:** KLIPY GIF search and picker through a privacy proxy ([#557](internal development reference omitted)).
- **Voice:** Server-enforced mute and deafen, applied at the media server ([#546](internal development reference omitted)).
- **Notifications:** @mention routing in encrypted channels without decryption ([#310](internal development reference omitted)).
- **Encryption:** DM key-epoch enforcement and revocation ([#298](internal development reference omitted)).
- **Encryption:** Channel keys rotate when a member is removed.
- **Desktop:** Auto-updater with rollback, splash screen, progress, and error states ([#155](internal development reference omitted)).
- **Desktop:** Server-proxied desktop updates with no per-client telemetry ([#264](internal development reference omitted)).
- **Desktop:** Hot-update SPA pipeline with a bundle-hash deploy contract ([#429](internal development reference omitted), [#773](internal development reference omitted)).
- **Desktop:** The desktop falls back to the bundled SPA when a hot update fails ([#831](internal development reference omitted), [#832](internal development reference omitted), [#835](internal development reference omitted)).
- **Chat:** WebSocket reconnect race fix and subscribe-barrier protocol ([#769](internal development reference omitted)).
- **Self-hosting:** Self-hosted coturn STUN/TURN ([#124](internal development reference omitted), [#577](internal development reference omitted)).
- **Server:** Public media proxy for public assets ([#570](internal development reference omitted)).
- **Sign-in:** Token theft detection binds sessions to machine ID and IP ([#89](internal development reference omitted), [#702](internal development reference omitted)).
- **Sign-in:** The main process refreshes the access token before expiry ([#329](internal development reference omitted)).
- **Appearance:** Per-user profile theming, profile cards, and avatar theming ([#251](internal development reference omitted), [#252](internal development reference omitted)).
- **Appearance:** Image crop editors for profile and server images ([#357](internal development reference omitted)).
- **Accounts:** Username rules, period support, and a yearly change cooldown ([#330](internal development reference omitted)).
- **Desktop:** OS-level permission management ([#321](internal development reference omitted)).
- **Friends:** Friend requests with accept and decline ([#203](internal development reference omitted)).
- **Notifications:** Notification sounds, per-category volumes, and looping DM call sounds ([#375](internal development reference omitted), [#394](internal development reference omitted), [#743](internal development reference omitted), [#554](internal development reference omitted)).
- **Desktop:** A Developer Mode setting enables DevTools in Alpha and Beta builds ([#567](internal development reference omitted)).
- **Desktop:** macOS builds are signed and notarized ([#641](internal development reference omitted)).
- **Desktop:** Windows `Setup.exe` is signed ([#649](internal development reference omitted)).
- **Operations:** Docker network segmentation, request-ID propagation, and Redis auth ([#442](internal development reference omitted)).
- **Operations:** GitHub Actions CI with tests, coverage, SonarQube, and Semgrep ([#128](internal development reference omitted), [#130](internal development reference omitted), [#457](internal development reference omitted)).
- **Operations:** A supply-chain IOC scanner ([#722](internal development reference omitted), [#781](internal development reference omitted)).
- **Operations:** An AI-generated code governance framework ([#454](internal development reference omitted) to [#458](internal development reference omitted), [#500](internal development reference omitted) to [#522](internal development reference omitted)).

### Changed

- **Dependencies:** React 19.2.4, react-router-dom 7.13.1, Zustand 5.0.12, ESLint 10 flat config, and Vite 8.0.0 ([#181](internal development reference omitted), [#182](internal development reference omitted), [#183](internal development reference omitted), [#184](internal development reference omitted), [#185](internal development reference omitted), [#186](internal development reference omitted), [#287](internal development reference omitted)).
- **Dependencies:** Go 1.26.1 with `govulncheck` ([#193](internal development reference omitted)).
- **Dependencies:** Electron 41, mediasoup 3.19.18, mediasoup-client 3.18.7, and TypeScript 6.0.2.
- **Encryption:** The client derives the password key with Argon2id instead of PBKDF2 ([#117](internal development reference omitted)).
- **Operations:** macOS notarization uses an App Store Connect API key ([#826](internal development reference omitted)).
- **Operations:** Lower cognitive complexity across Go handlers and TypeScript code ([#418](internal development reference omitted), [#419](internal development reference omitted), [#498](internal development reference omitted), [#505](internal development reference omitted), [#550](internal development reference omitted) to [#553](internal development reference omitted)).
- **Operations:** Documentation audit for the Beta release gate ([#214](internal development reference omitted), [#823](internal development reference omitted)).
- **Operations:** MCP server config split into per-host files ([#838](internal development reference omitted)).

### Removed

- **Operations:** Sentry telemetry removed from all three services, and its MCP config removed ([#770](internal development reference omitted), [#780](internal development reference omitted), [#793](internal development reference omitted), [#796](internal development reference omitted), [#838](internal development reference omitted)).
- **Operations:** The unused Postgres MCP server config ([#838](internal development reference omitted)).
- **Desktop:** The deprecated `WebSocketMessage` type alias. `WebSocketEvent` is the canonical name ([#1185](internal development reference omitted), [#1184](internal development reference omitted)).

### Fixed

- **Encryption:** Channel-key OperationError reports a structured diagnostic ([#765](internal development reference omitted)).
- **Encryption:** WebRTC BUNDLE no longer misroutes encrypted voice and video between consumers ([#291](internal development reference omitted), [#292](internal development reference omitted), [#293](internal development reference omitted)).
- **Encryption:** Media encryption moved from Insertable Streams to RTCRtpScriptTransform ([#355](internal development reference omitted)).
- **Encryption:** A session-scoped cache stops the key request flood ([#241](internal development reference omitted)).
- **Encryption:** Frame decryption recovers, and CSK rotation is hardened ([#232](internal development reference omitted), [#284](internal development reference omitted)).
- **Encryption:** DM key epochs, presence sync, and key distribution are fixed ([#298](internal development reference omitted)).
- **Messages:** Replied-to messages decrypt on REST fetch ([#542](internal development reference omitted)).
- **Messages:** DM threads update their preview and order in real time ([#541](internal development reference omitted)).
- **Messages:** Message editing works in encrypted channels and DMs ([#220](internal development reference omitted)).
- **Voice:** Black-screen recovery, codec selection, screen-share audio, and audio that persists across navigation ([#198](internal development reference omitted), [#227](internal development reference omitted), [#295](internal development reference omitted), [#299](internal development reference omitted), [#396](internal development reference omitted)).
- **Server:** Hub goroutine races and flaky voice tests are resolved ([#476](internal development reference omitted), [#544](internal development reference omitted)).
- **Chat:** A lost connection reconnects the WebSocket instead of reloading the page ([#194](internal development reference omitted), [#439](internal development reference omitted)).
- **Presence:** You no longer show as Offline while connected ([#1535](internal development reference omitted)).
- **Server:** The Postgres "invalid length of startup packet" flood is fixed ([#779](internal development reference omitted)).
- **Voice:** The PiP window loads the SPA route ([#815](internal development reference omitted)).
- **Voice:** PiP signaling and local user identification are fixed ([#415](internal development reference omitted)).
- **Desktop:** Login on the bundled SPA fallback uses the `app://` scheme ([#832](internal development reference omitted)).
- **Sign-in:** The `/mfa/verify` response parses `access_token` ([#814](internal development reference omitted)).
- **Desktop:** The updater trust path is hardened on macOS, Windows, and CI ([#655](internal development reference omitted)).
- **Operations:** CI verifies ASAR integrity ([#686](internal development reference omitted)).
- **Desktop:** The preload script is bundled with esbuild for the sandbox ([#678](internal development reference omitted)).
- **Operations:** Docker Compose passes the MFA encryption key ([#225](internal development reference omitted)).
- **GIFs:** The KLIPY media proxy no longer returns 401, and its routes sit under `/gifs` ([#687](internal development reference omitted), [#580](internal development reference omitted)).
- **GIFs:** KLIPY GIFs render on every decrypt path ([#566](internal development reference omitted)).
- **Operations:** NATS config and coturn TLS and external IP are fixed ([#576](internal development reference omitted), [#577](internal development reference omitted)).
- **Desktop:** Escape closes only the top modal ([#480](internal development reference omitted)).
- **Messages:** Server role styling no longer affects DMs ([#543](internal development reference omitted)).
- **Accessibility:** Semantic HTML, keyboard navigation, and ARIA pass ([#380](internal development reference omitted), [#427](internal development reference omitted), [#482](internal development reference omitted)).
- **Messages:** A Markdown syntax help dialog and help icon ([#748](internal development reference omitted)).
- **Operations:** Beacon verification, the `/spa/` nginx route, and a defensive sentinel ([#766](internal development reference omitted)).

### Security

- **Desktop:** Main-process logs no longer pass raw `Error` objects ([#714](internal development reference omitted)).
- **Sign-in:** Ten token-suffix leaks are removed from the token manager ([#704](internal development reference omitted)).
- **Desktop:** Passive navigation allows only `https:`. Clicked links still allow `http:`, `https:`, and `mailto:` ([#774](internal development reference omitted)).
- **Desktop:** The updater pins the TLS certificate for `api.concordvoice.chat` ([#719](internal development reference omitted)).
- **Operations:** nginx closes H2C smuggling and Host header injection ([#525](internal development reference omitted)).
- **Operations:** A GitHub Actions shell injection is resolved ([#523](internal development reference omitted)).
- **Server:** CORS rejects null or empty origins and validates custom headers ([#259](internal development reference omitted)).
- **Server:** Hardcoded dev credentials are removed, and production guards are added ([#260](internal development reference omitted)).
- **Operations:** Infrastructure resists brute-force probes and vulnerability scanners ([#153](internal development reference omitted), [#189](internal development reference omitted)).
- **Dependencies:** npm overrides fix transitive vulnerabilities ([#289](internal development reference omitted), [#314](internal development reference omitted)).

**Known issues** (not counted as changes):

- The "No Internet" Retry button can stay stuck after the network returns. Quit and relaunch ([#817](internal development reference omitted)).
- Chat Markdown has heading size, code block, and spacing errors ([#807](internal development reference omitted)).
- The Pinned Messages panel drops media and shows GIFs as raw text ([#805](internal development reference omitted)).
- The GIF picker can hit a rate limit while you scroll ([#804](internal development reference omitted)).
- The Member List "+ Add Role" dropdown is clipped inside its card ([#799](internal development reference omitted)).
- The profile editor can lose unsaved edits when your user record refreshes ([#707](internal development reference omitted)).

**Upgrading from v0.1.0-Alpha** (not counted as changes):

- No end-user breaking changes. New features are additive.
- Migrations 000054 to 000061 apply on first server run. Migration 000059 makes `refresh_tokens.user_id` cascade on delete.
- The client re-wraps legacy PBKDF2 keys with Argon2id at next login (migration 000034) ([#117](internal development reference omitted)).
- Passive navigation allows only `https:` ([#774](internal development reference omitted), [#1172](internal development reference omitted)).
- A password change revokes refresh tokens, so other devices sign out at their next refresh.
- Self-hosted operators who want Apple sign-in must set the Apple credentials in the control-plane `.env`.

---

## [0.1.41] - 2026-05-20

Windows and Linux get native window buttons, and the app gets a branded title
bar. You can now choose what the close and minimize buttons do.

**7 Total Changes** | 4 Added | 3 Changed

### Added

- **Desktop:** Windows and Linux show native close, minimize, and maximize buttons. macOS keeps its traffic lights ([#806](internal development reference omitted)).
- **Desktop:** A branded title bar shows the app name, the version, and the active SPA build, and updates when the SPA updates.
- **Desktop:** The window remembers its size, position, and maximized state, and checks the saved position against your current displays.
- **Desktop:** Settings ▸ Appearance ▸ Client Behavior lets you send the close and minimize buttons to the tray, the taskbar or dock, or quit. Invalid combinations are greyed out with a reason. The tray icon itself ships with ([#1099](internal development reference omitted)).

### Changed

- **Desktop:** Four new window IPC channels and one SPA version event. Each handler validates its input.
- **Desktop:** Picture-in-picture windows no longer draw an OS shadow on macOS.
- **Appearance:** The Windows and Linux title bar follows your theme, including the system light or dark setting.

---

## [0.1.40] - 2026-05-20

v0.1.39 never published because the Linux build failed. This release carries
all of v0.1.39 plus the Linux build fix.

**2 Total Changes** | 1 Changed | 1 Fixed

### Fixed

- **Desktop:** Linux builds package again. A display-name change had renamed the Linux binary to one with a space, so the packager could not find it. Linux keeps `concord-voice`, and macOS and Windows keep "Concord Voice".

### Changed

- **Desktop:** The packaging test checks the binary name per platform.

---

## [0.1.39] - 2026-05-20

The release workflow publishes again after it skipped every release since
v0.1.34. This release also carries the unshipped v0.1.36 to v0.1.38 changes.

**8 Total Changes** | 6 Changed | 2 Fixed

### Changed

- **Desktop:** The product name is "Concord Voice" everywhere, including the Dock, the Windows registry, and the Linux desktop entry.
- **Desktop:** The company name is "Concord Voice LLC" in the license and About screen.
- **Notifications:** You can mute messages and voice separately for each server and DM ([#985](internal development reference omitted), [#84](internal development reference omitted)).
- **Messages:** The server no longer sends an `is_encrypted` field, because every room is encrypted. WebSocket messages without `key_version` 1 or higher are refused with close code 4400 ([#1042](internal development reference omitted), [#201](internal development reference omitted)).
- **Messages:** The desktop no longer reads `is_encrypted` ([#1031](internal development reference omitted)).
- **Voice:** The media server drops `is_encrypted` and documents that SRTP is always on ([#1032](internal development reference omitted)).

### Fixed

- **Desktop:** The release workflow no longer skips publishing on a push to main.
- **Desktop:** The first WebSocket connection drop no longer logs noise, and the auth ticket no longer shows in the console ([#1046](internal development reference omitted)).

---

## [0.1.18] - 2026-04-09

The desktop reports crashes and errors, and scrubs private data before it
sends them.

**3 Total Changes** | 2 Added | 1 Changed

### Added

- **Desktop:** Error tracking for the main and renderer processes. It fails closed and drops key material, personal data, and console logs before it sends a report ([#586](internal development reference omitted)).
- **Development:** A documentation-drift reviewer for pull requests.

### Changed

- **Development:** The project guides and architecture docs describe the current state of the app.

---

## [0.1.17] - 2026-04-09

A quality pass fixes GIF routes and hardens the voice relay servers.

**6 Total Changes** | 1 Added | 1 Changed | 4 Fixed

### Added

- **Development:** A script keeps the internal project counts current ([#581](internal development reference omitted)).

### Changed

- **Development:** Tool server settings use one naming scheme ([#581](internal development reference omitted)).

### Fixed

- **GIFs:** GIF requests reach the right server route ([#580](internal development reference omitted)).
- **Development:** Review comments from an earlier pull request are addressed ([#579](internal development reference omitted)).
- **Voice:** The voice relay server uses its own TLS certificate, covers its hostname, and renews it automatically.
- **Operations:** The message bus and voice relay settings are correct for a single server.

---

## [0.1.16] - 2026-04-08

Public images load without sign-in, and a quality pass fixes many small bugs.

**3 Total Changes** | 1 Added | 2 Fixed

### Added

- **Servers:** Public images load without sign-in ([#570](internal development reference omitted)).

### Fixed

- **Appearance:** Avatars keep a fixed 40 by 40 pixel size, so the layout no longer shifts ([#570](internal development reference omitted)).
- **Desktop:** A broad sweep fixes app, server, and infrastructure bugs ([#571](internal development reference omitted)).

---

## [0.1.15] - 2026-04-07

GIF search arrives, and the server fetches GIFs for you so the GIF provider
never sees your address.

**9 Total Changes** | 2 Added | 1 Changed | 6 Fixed

### Added

- **GIFs:** GIF search and a GIF picker. The server fetches GIFs on your behalf, and the About screen credits the provider ([#483](internal development reference omitted), [#557](internal development reference omitted)).
- **Desktop:** Alpha builds have a Developer Mode switch that opens DevTools ([#567](internal development reference omitted)).

### Changed

- **Dependencies:** Routine updates to build, test, and sign-in libraries ([#559](internal development reference omitted), [#560](internal development reference omitted), [#561](internal development reference omitted), [#564](internal development reference omitted), [#565](internal development reference omitted)).

### Fixed

- **Desktop:** Windows builds work again ([#569](internal development reference omitted)).
- **Messages:** Chat messages, GIFs, image crops, and SPA updates display correctly ([#566](internal development reference omitted)).
- **GIFs:** The GIF provider logo follows your theme.
- **GIFs:** GIFs decrypt in history as well as in live chat, and every image size loads.
- **Desktop:** The SPA keeps its strict content security policy on every response.
- **GIFs:** The GIF picker and its settings work with a keyboard and screen reader.

---

## [0.1.14] - 2026-04-06

Server roles now control the right-click moderation actions. Voice code is
split into smaller parts.

**2 Total Changes** | 1 Changed | 1 Fixed

### Changed

- **Voice:** Voice settings, the voice service, encryption helpers, and codec choice are split into smaller parts ([#550](internal development reference omitted), [#551](internal development reference omitted), [#552](internal development reference omitted), [#553](internal development reference omitted)).

### Fixed

- **Servers:** Right-click kick, ban, and mute follow your server role ([#548](internal development reference omitted)).

---

## [0.1.13] - 2026-04-04

Chat gets reactions, replies, pins, search, attachments, and group DMs.
Servers can mute and deafen members, and the voice server enforces it.

**34 Total Changes** | 15 Added | 7 Changed | 10 Fixed | 2 Security

### Added

- **Voice:** Server mute and deafen, enforced by the voice server ([#488](internal development reference omitted)).
- **Messages:** Emoji reactions that sync live ([#169](internal development reference omitted), [#459](internal development reference omitted)).
- **Messages:** Replies that quote the original message ([#170](internal development reference omitted), [#463](internal development reference omitted)).
- **Messages:** Pinned messages with a pin list for each channel ([#171](internal development reference omitted), [#465](internal development reference omitted)).
- **Messages:** Encrypted search that decrypts and searches history on your device ([#172](internal development reference omitted), [#468](internal development reference omitted)).
- **Messages:** File and image attachments with preview and download ([#178](internal development reference omitted), [#470](internal development reference omitted)).
- **Messages:** Group DMs with admin controls ([#208](internal development reference omitted)).
- **Messages:** Unsent drafts are saved for each channel ([#174](internal development reference omitted), [#477](internal development reference omitted)).
- **Notifications:** Desktop notifications for mentions and new DMs ([#175](internal development reference omitted), [#478](internal development reference omitted)).
- **Desktop:** Configurable keyboard shortcuts with a help overlay ([#176](internal development reference omitted), [#479](internal development reference omitted)).
- **Desktop:** One right-click menu across the app, with copy and role assignment ([#446](internal development reference omitted)).
- **Desktop:** The SPA can update without a new desktop release ([#429](internal development reference omitted)).
- **Development:** Shared chat logic for fetch, paging, decryption, and send ([#545](internal development reference omitted)).
- **Operations:** Services run on separate networks, requests carry an ID, and Redis requires auth ([#442](internal development reference omitted)).
- **Development:** An AI-generated code policy, code owners, and static analysis ([#454](internal development reference omitted), [#458](internal development reference omitted), [#500](internal development reference omitted), [#504](internal development reference omitted), [#507](internal development reference omitted), [#518](internal development reference omitted), [#522](internal development reference omitted)).

### Changed

- **Messages:** The message component is split into smaller parts ([#451](internal development reference omitted)).
- **Messages:** The DM list updates live with the latest message ([#541](internal development reference omitted)).
- **Dependencies:** TypeScript 6, ESLint 10, mediasoup, Electron, and other updates ([#411](internal development reference omitted), [#414](internal development reference omitted), [#511](internal development reference omitted), [#528](internal development reference omitted), [#529](internal development reference omitted), [#532](internal development reference omitted), [#536](internal development reference omitted), [#538](internal development reference omitted), [#539](internal development reference omitted)).
- **Accounts:** Account email uses a new provider with branded templates.
- **Development:** CI adds static analysis, quality gates, test sharding, and caching ([#457](internal development reference omitted), [#471](internal development reference omitted)).
- **Development:** Server and app code is simpler to read ([#418](internal development reference omitted), [#419](internal development reference omitted), [#498](internal development reference omitted)).
- **Desktop:** Modals share one set of panels ([#365](internal development reference omitted), [#481](internal development reference omitted)).

### Fixed

- **Messages:** Replied-to messages decrypt when history loads, not only live ([#542](internal development reference omitted)).
- **Messages:** Server role colors no longer appear in DMs ([#543](internal development reference omitted)).
- **Development:** WebSocket and voice tests no longer depend on timing ([#476](internal development reference omitted), [#544](internal development reference omitted)).
- **Servers:** Assigning a role from the right-click menu reports failures ([#447](internal development reference omitted)).
- **Desktop:** Escape closes only the top modal ([#480](internal development reference omitted)).
- **Video:** Video fills the voice area ([#443](internal development reference omitted)).
- **Desktop:** A lost connection reconnects without a page reload ([#194](internal development reference omitted), [#439](internal development reference omitted)).
- **Accessibility:** Semantic markup, keyboard navigation, and screen-reader labels across the app ([#380](internal development reference omitted), [#427](internal development reference omitted)).
- **Development:** Safe database query warnings are marked as reviewed ([#524](internal development reference omitted)).
- **Voice:** The media server reaches Redis.

### Security

- **Operations:** The web server blocks request smuggling and Host header injection ([#525](internal development reference omitted)).
- **Development:** A CI workflow no longer allows shell injection ([#523](internal development reference omitted)).

---

## [0.1.12] - 2026-03-27

Accounts get two-factor sign-in, security keys, and email verification.
Servers get roles and permissions, and the desktop updates itself.

**36 Total Changes** | 16 Added | 6 Changed | 10 Fixed | 4 Security

### Added

- **Accounts:** Two-factor sign-in with authenticator apps, security keys, backup codes, recovery contacts, and trusted devices ([#89](internal development reference omitted)).
- **Servers:** Roles and permissions with an audit log ([#82](internal development reference omitted)).
- **Accounts:** Email verification at sign-up ([#269](internal development reference omitted)).
- **Servers:** Images are kept in object storage, not the database ([#166](internal development reference omitted)).
- **Servers:** Ownership transfer with two-factor confirmation, email confirmation, and a way to reverse it ([#244](internal development reference omitted)).
- **Desktop:** Automatic updates with rollback, a splash screen, and progress ([#155](internal development reference omitted), [#381](internal development reference omitted), [#387](internal development reference omitted)).
- **Accounts:** A stolen sign-in token is revoked when it appears on another device or address ([#89](internal development reference omitted)).
- **Accounts:** Sign-in tokens refresh before they expire ([#240](internal development reference omitted), [#254](internal development reference omitted)).
- **Messages:** Removing a member rotates the channel key ([#96](internal development reference omitted)).
- **Messages:** Revoked DM keys are refused on the live connection ([#122](internal development reference omitted)).
- **Messages:** Mentions reach people without the server reading the message ([#118](internal development reference omitted)).
- **Desktop:** The app asks for OS permissions and respects them ([#197](internal development reference omitted)).
- **Voice:** A self-hosted relay helps calls connect through strict networks ([#124](internal development reference omitted)).
- **Development:** CI runs tests, coverage, and code quality checks in parallel ([#128](internal development reference omitted), [#130](internal development reference omitted)).
- **Development:** Many new tests toward the 80% coverage gate.
- **Desktop:** Install and update steps are logged to a file ([#383](internal development reference omitted)).

### Changed

- **Dependencies:** React 19, React Router 7, Zustand 5, Vite 8, Go 1.26, Electron 41, and mediasoup 3.19 ([#181](internal development reference omitted), [#182](internal development reference omitted), [#183](internal development reference omitted), [#287](internal development reference omitted), [#193](internal development reference omitted)).
- **Development:** ESLint 10 flat config and Redis client 5 ([#184](internal development reference omitted), [#185](internal development reference omitted), [#186](internal development reference omitted), [#187](internal development reference omitted)).
- **Messages:** Encryption keys are derived with Argon2id instead of PBKDF2 ([#117](internal development reference omitted)).
- **Messages:** Shared message fetch, decrypt, and paging logic ([#177](internal development reference omitted)).
- **Desktop:** The app is named "Concord Voice" on every platform, with the correct version, icon, and publisher ([#385](internal development reference omitted), [#386](internal development reference omitted)).
- **Desktop:** The updater no longer shares files with other Electron apps ([#382](internal development reference omitted)).

### Fixed

- **Voice:** Encrypted voice and video no longer mix up streams ([#291](internal development reference omitted)).
- **Voice:** The microphone works in encrypted voice on macOS ([#295](internal development reference omitted)).
- **Video:** Codec choice and screen-share sound work ([#299](internal development reference omitted)).
- **Voice:** Audio continues when you move to another view ([#396](internal development reference omitted)).
- **Video:** Black screens and missing audio in calls are fixed ([#227](internal development reference omitted)).
- **Messages:** Edits save ([#161](internal development reference omitted)).
- **Appearance:** Profiles use the owner's color scheme ([#165](internal development reference omitted)).
- **Desktop:** Clicking a popover's button again closes it ([#167](internal development reference omitted)).
- **Screen share:** Screen sharing uses your video settings ([#198](internal development reference omitted)).
- **Servers:** Changing a role no longer crashes the app ([#249](internal development reference omitted)).

### Security

- **Operations:** The server refuses empty or null origins and checks custom headers ([#259](internal development reference omitted)).
- **Operations:** Development credentials are removed from the code, with production guards ([#260](internal development reference omitted)).
- **Operations:** The infrastructure blocks brute-force scans ([#153](internal development reference omitted)).
- **Dependencies:** Vulnerable indirect packages are pinned to fixed versions ([#289](internal development reference omitted)).

---

## [0.1.0-alpha] - 2026-03-03

The first alpha: encrypted accounts, servers and text chat, voice and video,
and the desktop app.

**4 Total Changes** | 4 Added

### Added

- **Accounts:** Sign-up, sign-in, sessions, Argon2id passwords, and rate limits, with end-to-end encryption using RSA-OAEP 4096 and AES-256-GCM.
- **Servers:** Servers and channels, live encrypted messaging, presence, 12 color schemes, and an OpenAPI 3.0 reference.
- **Voice:** Voice, video, and screen sharing with 7 audio quality tiers, VP9, AV1, VP8, and H.264, channel categories, DMs, an emoji picker, a theme builder, and a mic test.
- **Operations:** Docker Compose for development, staging, and production, a relay server, cross-platform desktop builds, and pre-commit hooks.
