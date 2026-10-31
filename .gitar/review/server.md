# Self-Hosted review guidance

This repository is public. It holds the server stack: the Go control plane (`services/control-plane`), the mediasoup media plane (`services/media-plane`), the admin portal (`client/admin`) and the self-host installer (`infrastructure/deploy`). Desktop and Mobile clients consume its HTTP API, WebSocket events and Socket.IO events.

## Control plane (Go)

- Parameterized SQL only (`$1`, `$2`); never build SQL with `fmt.Sprintf` or string concatenation. Inside a transaction use `tx.Exec` / `tx.QueryRow`, not `db`.
- `defer rows.Close()` after every `Query`, and check `rows.Err()` after the loop.
- Every error is checked. Flag `_ =` or `_, _ =` discards of an error from a database, crypto or auth call.
- Use `crypto/subtle.ConstantTimeCompare` for secrets, tokens and hashes. No `unsafe`.
- Enforce authorization server-side for the action and the specific resource.
- Preserve API routes, WebSocket events and payload fields that released clients still consume. Removing or renaming one is a breaking change and needs a compatibility plan.

## Migrations (`services/control-plane/migrations`)

- `NNNNNN_name.up.sql` with a matching `.down.sql`, numbered sequentially.
- No `DROP TABLE` / `DROP COLUMN` without a data migration. `NOT NULL` additions need a `DEFAULT`. Use `TIMESTAMPTZ`.
- A file with more than one statement cannot use `CREATE INDEX CONCURRENTLY`.
- Encrypted columns are `BYTEA`. No plaintext secrets in SQL. RBAC migrations need security review.

## Media plane (Node.js + mediasoup)

- All Socket.IO input is untrusted. Every client event is registered through `withRateLimit` (`src/lib/rateLimit.ts`); a bare `socket.on` for a client event is a finding.
- Room and user identity come from authenticated socket state, never from the payload.
- DTLS-SRTP is structural. Flag any per-room or per-message encryption flag or branch.
- The origin gate must reject `null` and `file://` origins and must not allow `*` in production.

## Self-host installer and compose files

- The installer supports fresh installs only and writes `.env` atomically at mode `0600` without overwriting.
- Never print credentials in installer output or logs.

## E2EE

The server stores and relays ciphertext only. Flag any code that would let the server read message, attachment or media plaintext, or any fallback path for unencrypted content.
