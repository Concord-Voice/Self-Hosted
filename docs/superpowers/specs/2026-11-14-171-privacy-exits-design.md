# Security #171 Privacy Exit Design

## Goal

An invisible member must be indistinguishable from an offline member in counts, last-seen data, and group-DM ringing. Profile reads and notification recipients must obey the audience rules that already govern search and relationship state.

## Decisions

- The Hub owns visible-online status. Snapshot connected users and `hiddenPresence` under its read lock; release the lock before one Redis MGET of `presence:<id>`. Admit only explicit online or DND status. Missing, unknown, invisible, or Redis error is offline.
- Both server-list counts and group-DM ringing use filtered Hub accessors. The ring uses the same filtered recipient slice for invitations, response code, and `ringing_user_ids`; an invisible callee is not invited.
- Member-list `last_seen` hydration reads status and timestamp together. Another user's invisible timestamp is omitted; the viewer may see their own. A failed batch read omits timestamps. On final disconnect, capture invisibility before the Hub clears it and suppress the timestamp write if invisible.
- A direct profile read uses the search audience: self is allowed; otherwise the target must be searchable by username and neither party may have blocked the other. One SQL statement fetches the allowed target. Missing or excluded targets return the same 404; malformed IDs return 400.
- `ownership_transfer_initiated` with `to_user_id` goes only to the source owner and target. No bystander variant is needed because no current client consumes that event.
- A block transaction reads and locks the prior friendship status before changing it. The target receives `friend_removed` only when the prior status was accepted. The blocker's key-revocation event remains unconditional.

## Boundaries

No schema, route, configuration, entitlement, frontend, or media-plane change. Preserve transaction and lock ordering. Do not hold the Hub lock across Redis. Do not reveal an excluded profile through a distinct error or status. Keep the existing HTTP responses for ring and block except for the privacy-driven recipient/status cases.

Self-Hosted and Hosted implement the same control-plane behavior. Their `CHANGELOG.md`, OpenAPI, and architecture docs must be rebased after #166 merges. Ship Self-Hosted first, then Hosted. Separate approval is required for each merge.

## Proof

- Replay the five reported exploit tests for VULN-043, 050, 204, and 120 against baseline and fixed code; the attacker-goal assertions must fail after the fix.
- Add positive and negative tests for visible/DND/invisible/missing/Redis-error presence, offline-equivalent server counts and group rings, self/other last-seen, and final invisible disconnect.
- Add profile self/searchable/opt-out/two-direction block tests; live party and bystander WebSocket observers for transfer; stranger/pending/accepted-friend block-frame tests.
- Run focused package suites against isolated PostgreSQL and Redis, then the exact-head quality gate and Phase-8 security review on each PR.
