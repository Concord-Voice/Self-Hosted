# Session revoke concurrency fix plan

1. Reserve the free revoke atomically in Redis before the database effect, with a random non-integer score per request.
2. Release the reservation only on a conclusive no-effect failure; keep it on an unknown commit.
3. Cover the concurrent endpoint race, the window boundary, exact release, unknown-commit retention, and the Redis-down fallback with tests.
4. Clear CI and specialist review findings, then submit the ready PR for the required merge approval.
