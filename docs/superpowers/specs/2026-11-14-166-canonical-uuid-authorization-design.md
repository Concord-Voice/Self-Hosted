# Security #166: Canonical UUID Authorization Design

## Intent

PostgreSQL treats alternate spellings of a UUID as one value but returns canonical lowercase text. Authorization code currently uses caller-supplied text as map keys or compares it to database text. Six confirmed exploit paths use that mismatch to bypass channel DENYs or owner/creator protection, or to purge keys from a member who remains authorized.

## Goals and constraints

- Every affected authorization decision treats canonical, uppercase, braced, URN, and raw-hex spellings of one UUID identically. Preserve the valid input forms accepted by the pinned `google/uuid` parser.
- Malformed route and body IDs return the route's existing HTTP 400 before a write. Resolver and internal paths fail closed on malformed IDs.
- SQL arguments, cache keys, response IDs, audit IDs, and WebSocket payloads from affected paths use lowercase hyphenated UUID text.
- Keep existing permission precedence, ownership rules, transaction and lock ordering, route shapes, and error privacy.
- A group-DM creator cannot be demoted even if a preflight check races; the mutation statement enforces the rule.
- No schema, dependency, configuration, or client protocol change.

## Approaches

1. **Chosen: normalize at the affected HTTP boundaries and in shared authorization code.** Parse once at each boundary with a small `internal/requestid` helper. The resolver, hierarchy check, and key-revocation comparison independently normalize internal inputs. This closes current routes and non-HTTP callers without changing their string APIs.
2. Convert every resolver API and caller to `uuid.UUID`. This gives a strong type guarantee but changes many critical voice and E2EE call sites for the same outcome.
3. Normalize only in handlers. This leaves internal callers and database-originated IDs able to trigger the same class of mismatch.

## Design

`requestid.Canonical(raw)` returns `uuid.Parse(raw).String()` or an error. `requestid.Param(c, name)` applies it to a Gin path parameter without writing a response; each handler keeps its current error body. Gin v1.12 `ShouldBindJSON` validates fields but does not rewrite them, so body IDs must be normalized after binding. The helper has no configurable policy or new accepted input forms.

The RBAC resolver canonicalizes textual server, user, and channel IDs before cache use or SQL. `ResolveEffectivePermissionsForChannelsFresh` parses all IDs before starting a transaction, deduplicates channel IDs by UUID value, seeds and updates the permission map with canonical keys, and returns canonical keys only. Its two production consumers are the single-channel adapter and server purge; the adapter indexes by its canonical channel ID, and server purge receives channel IDs from PostgreSQL. A malformed ID fails the whole call.

`checkHierarchy` compares parsed actor, target, and owner UUID values; it refuses malformed server, actor, target, or database owner values. `RevokeDeniedChannelKeyCandidatesTx` compares visible and candidate users as UUID values and emits canonical denied IDs. These rules apply inside the caller's existing transaction.

Affected purge, channel-key, role-assign/unassign, group-DM role, message-send, and voice-moderation handlers carry one canonical local ID from validation through authorization, write, response, and event emission. The group-DM role update also adds a creator predicate to the `UPDATE` itself; zero changed rows follow the existing unavailable/refusal path.

## Verification

- The six scan exploit tests VULN-077, 064, 099, 195, 196, and 205 first reproduce on Self-Hosted `origin/main` at `7e66479b`; after the fix each must fail because the attacker goal is denied, not because its source-shape assertion or setup fails.
- Focused regression tests assert authorization and output equivalence for accepted spellings, malformed-ID refusal before writes, no false key revocation, creator protection at the SQL write, and canonical response/event fields.
- Run focused Go packages and quality checks in Self-Hosted, then port and rerun the same proofs in Hosted. PostgreSQL integration tests require an isolated `DATABASE_URL`.

## Dependency references

- Gin v1.12.0: `/gin-gonic/gin/v1.12.0`, [binding and path parameters](https://github.com/gin-gonic/gin/blob/v1.12.0/docs/doc.md).
- Google UUID v1.6.0: `/google/uuid/v1.6.0`; Context7 returned no matching Parse/String excerpt. The pinned local `uuid.go` source confirms accepted forms and canonical `String()` output.
