# Session revoke concurrency port

Port [Hosted #4434](https://github.com/Concord-Voice/Hosted/pull/4434) to the matching Self-Hosted session handler. Concurrent requests for one account must reserve the single free revoke in Redis before either reaches the database mutation. A conclusive no-effect failure releases its reservation; an unknown commit keeps it so another free revoke cannot slip through. Existing step-up, simple-mode, ownership, and response behavior stays intact.

The change reaches the session handler, its direct test caller, the Redis reservation regressions, and the prerelease changelog. It adds no schema, API, or deployment setting. Redis script access already exists in the managed ACL manifest. Database-backed tests require isolated CI infrastructure.
