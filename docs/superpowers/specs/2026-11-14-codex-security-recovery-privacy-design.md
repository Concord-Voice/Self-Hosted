# Recovery timing and log privacy port

Port [Hosted #4435](https://github.com/Concord-Voice/Hosted/pull/4435) and [Hosted #4436](https://github.com/Concord-Voice/Hosted/pull/4436) together because both change recovery initiation. The accepted response must return before account lookup, Redis storage, or SMTP delivery, with the same response for known and unknown addresses. A bounded queue and four workers drain during router shutdown; a saturated queue drops work without changing the response. SMTP has a ten-second connection deadline.

Registration, recovery, refresh, login, and ownership-transfer logs must omit email addresses, client IP addresses, client machine IDs, and SMTP error strings that can include those values. The prior Self-Hosted recovery success log also contained the email address and is removed. Existing recovery code hashing, expiration, attempt limits, and generic response remain.

The port reaches the auth handler and tests, ownership handler and test, router shutdown, email delivery, OpenAPI, and changelog. Database-backed tests run in isolated PR CI.
