# MFA policy notification audience

Hosted [#4424](https://github.com/Concord-Voice/Hosted/pull/4424) found that a server-wide `server_permissions_changed` event discloses when the protected MFA policy changes to ordinary members. Self-Hosted has the same handler and read boundary.

The event must reach only the owner and users with the raw Administrator bit on a role belonging to this server, matching `GET /servers/:id/mfa-enforcement`. Keep the server-wide permission-generation bump before notification so every member's later permission read is fresh. Recipient lookup errors must fail closed; keep the existing event payload and one-second send bound. No schema or client change is needed.

The handler, event integration tests, backend rule, OpenAPI contract, and prerelease changelog are in scope. A stale or cross-server role must not widen the notification audience. Merge remains subject to the normal PR review and CI gates.
