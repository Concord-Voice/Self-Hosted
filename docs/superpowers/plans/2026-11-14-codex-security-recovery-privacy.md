# Recovery timing and log privacy port plan

1. Port independent log redaction and verify all changed callers and test fixtures.
2. Move recovery work behind the bounded queue, wire shutdown before database and Redis closure, and bound SMTP IO.
3. Adapt integration tests to observe the async response and eventual cleanup; assert that failure logs omit recipient data.
4. Compile changed packages, run queue and router tests locally, then open a draft PR for isolated database CI and final review.
