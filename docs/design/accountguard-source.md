# Account-change source marker

`internal/accountguard.SourceTx` supplies a dormant transaction-local provenance
helper for future account-change writers. It has no production callers. It does
not authorize a change, alter credentials, emit audit records, or activate an
account-recovery flow.

## Contract

`accountguard.Source` aliases the existing `decide.Source` type and re-exports its
six constants. `SourceTx(ctx, tx, source)` accepts exactly `user_session`,
`lockdown`, `break_glass`, and `recovery_key`. It rejects `operator`,
`unattributed`, empty, and unknown values before SQL, without echoing the input.
There is no normalization or fallback source. An operator path requires a
separate reviewed contract change.

The caller supplies a non-nil context and owns the SQL transaction. The helper
rejects a nil transaction and executes this parameterized statement:

```sql
SELECT set_config('concord.auth_change_source', $1, true)
```

The setting belongs to that transaction. A later accepted call in the same
transaction replaces it. The helper never begins, commits, or rolls back a
transaction, and never retries a failed statement. SQL and context errors retain
their identity through wrapping for `errors.Is`; exact error text is not an API.

Readers normalize PostgreSQL's empty custom-setting value:

```sql
SELECT NULLIF(current_setting('concord.auth_change_source', true), '')
```

Transaction completion removes the local value and restores any prior session
setting. With no prior source, the normalized read is NULL. The helper does not
establish a session-level source.

## Integration boundary

The future account-change runner is the sole intended production caller. It must
set the authoritative `Decide` outcome's source before giving a write callback
the transaction. Proof checks, authority, material effects, commit ownership and
activation remain separate obligations. The current source boundary test permits
no production callers or accountguard transaction owners; introducing the runner
requires a deliberate update to that guard.

Independent PostgreSQL tests cover accepted and rejected sources, same-session
commit/rollback lifetime, session isolation, replacement within a transaction,
and error identity. The source guard also checks aliased references, duplicate
marker owners and transaction ownership. These tests complement the existing
pure decision-policy and credential-provenance guards.
