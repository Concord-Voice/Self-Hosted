# Self-Hosted media consumer limits

An authenticated participant can consume each room producer on up to four receive transports. Existing deduplication only rejects a repeated producer on the same transport, so one participant can multiply SFU consumer objects and egress. The vulnerable path is `RoomManager.consume`.

## Behavior

- Reserve capacity synchronously before `recvTransport.consume`, counting live and in-flight consumers for the participant.
- Permit one primary consumer per producer and at most three cross-transport duplicates, allowing one PiP duplicate on each secondary transport.
- Cap total participant consumers at 1,060, the existing room/source ceilings plus those three duplicates.
- Keep same-transport retries idempotent. Release reservations on failure and committed capacity through existing consumer close paths.

The change is local to media allocation. It adds no configuration or wire event. A participant exceeding either cap receives a consume error; other participants retain their capacity.

## Verification

Cover race and close-path regressions in the media suite. Run the focused suite, typecheck, lint, changelog guard, CI, and security review on the final head.
