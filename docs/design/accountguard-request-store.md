# Dormant account-change request store

`internal/accountguard` has a private, transaction-bound component for pending
account-change requests and their credential origins. It loads the persisted
request-row subset of a `decide.Decide` snapshot, prepares request and
dispute-group IDs, and writes only request-row effects. Its caller supplies the
user ID, an existing SQL transaction, and a database timestamp captured after
the required locks. The component does not begin or complete a transaction and
has no production caller.

The pure `partitionRequestEffects` function separates request-row effects from
other effects. Its returned outcome retains every non-effect obligation,
including source, credential and epoch changes, continuation, permission
generation, enrolment class, and notices. An empty remainder effect list does
not mean the decision is complete. `applyTx` accepts only the private request
plan and reports only IDs needed to bind later notices; its success means that
those rows were written in the caller's still-open transaction. A later
coordinator must validate the complete outcome and all dependencies before any
request or material write, then own the transaction and commit decision.

The store rejects malformed request states and effect shapes before writes. It
preserves target material versions, origin ordering and digest, dispute timing,
and settled hold milliseconds. Completing or cancelling a request clears its
pending payload; qualifying recent proof or dispute-resolved cancellations
retain fingerprints for the reopen window. Fingerprints use versioned,
length-prefixed SHA-256 and HMAC-SHA-256 transcripts. The coarse digest excludes
address destinations and secrets; the keyed digest includes a normalized
destination for address changes.
Key-generation allocation and current-key binding belong to later work.

The independent PostgreSQL tests use two test-owned tables. There is no
production migration, complete-outcome executor, credential or address writer,
proof verification, notice state or delivery, route, or activation in this
unit. Those remain requirements before account-change flows can serve requests.
