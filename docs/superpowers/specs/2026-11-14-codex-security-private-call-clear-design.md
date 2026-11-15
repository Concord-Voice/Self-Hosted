# Private-call clear audience

Hosted [#4425](https://github.com/Concord-Voice/Hosted/pull/4425) found that a private-call activity clear was broadcast to every local Rich Presence client, disclosing the participant identifier and call termination timing outside the authorized audience. Self-Hosted has the same delivery and bootstrap paths.

Record each sender/category projection actually delivered to a connection, including the bootstrap snapshot and ordered live updates. An active-category terminal may be queued only for a connection holding that projection; it consumes the receipt only after successful enqueue. Preserve the existing bounded queue and privacy-critical disconnect behavior for live updates. A failed or canceled bootstrap must discard its staged projection.

Scope is the WebSocket client delivery, bootstrap, clear path, and focused tests. No wire-format, schema, or client change is required. Validate absence of identifier disclosure, clear delivery to authorized viewers, and bootstrap race ordering before review.
