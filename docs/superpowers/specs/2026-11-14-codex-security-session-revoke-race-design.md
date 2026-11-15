# Session revoke concurrency fix

Concurrent requests for one account must reserve the single free revoke in Redis before either reaches the database mutation. A conclusive no-effect failure releases its own reservation; an unknown commit keeps it so another free revoke cannot slip through. Existing step-up, simple-mode, ownership, and response behavior stays intact.

Each reservation gets a random non-integer score inside the request's second. Non-free revokes keep integer scores, so the exact-score release cannot remove them, and a stale or retried release cannot remove another request's reservation. A release also requires that no login family was committed.

The change reaches the session handler, its direct test caller, the Redis reservation regressions, and the prerelease changelog. It adds no schema, API, or deployment setting. The managed Redis ACL manifest already grants every command the change uses.
