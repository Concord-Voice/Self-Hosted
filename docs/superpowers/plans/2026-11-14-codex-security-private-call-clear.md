# Private-call clear port plan

1. Track each connection's delivered Rich Presence projections in Self-Hosted's WebSocket code.
2. Port the clear and bootstrap tests, fix the staticcheck complaint, and make the stale-DM caller test prove the viewer held the badge before expecting a clear.
3. Update the prerelease changelog and run focused WebSocket tests and Go lint.
4. Publish a draft Self-Hosted PR for isolated CI, review, and security validation. Merge only after the normal gates and explicit approval.
