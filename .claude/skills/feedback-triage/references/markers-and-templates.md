# Markers & Templates

This file defines the **single state surface** the routine owns on each public issue — the *managed status comment* — plus every comment template. All idempotency depends on these being byte-stable: the routine finds its own comment by the sentinel, parses the marker, and **edits that same comment in place**. Never post a second managed comment.

## Why a managed comment, not a body edit

The public repo is source-available; the reporter's words are theirs. The routine never rewrites the issue body. Instead it owns exactly one comment per issue (created on first touch, edited thereafter) that carries both the human-readable status and a machine-readable marker. The 👍 seed lives on the issue itself (for reaction-sorting), not in the comment.

## The managed status comment

Sentinel (first line, exact) lets the routine locate its comment among all comments:

```markdown
<!-- feedback-triage:status v1 -->
### 🛰️ Feedback triage status

👥 **Reported by 3 user(s)** · 📌 Tracked as `example-repo-private#1234` · 🗓️ Tracked for **an upcoming Beta update**

_Consolidated reports: #18, #27. Last updated by automated triage on 2026-06-20._

<!-- ft-marker
schema: 1
reports: 3
consolidated: 18,27
alpha: example-repo-private#1234
state: tracked
release: v0.2.0-Beta
fixed_in:
last_synced: 2026-06-20
-->
```

### Marker field reference

| Field | Meaning | Example |
|---|---|---|
| `schema` | marker version, for future migration | `1` |
| `reports` | total user reports folded into this survivor (Σ of members) | `3` |
| `consolidated` | comma-list of dupe issue numbers closed into this one | `18,27` |
| `alpha` | the tracker issue, `owner/repo#N` form (empty until Phase 4) | `example-repo-private#1234` |
| `state` | `untriaged \| tracked \| in-progress \| fixed \| declined` | `tracked` |
| `release` | release window string, or `future` | `v0.2.0-Beta` |
| `fixed_in` | version the fix shipped in (only when `state: fixed`) | `v0.2.1` |
| `last_synced` | UTC date of the last marker write | `2026-06-20` |

### Locate + parse (idempotency primitive)

```bash
# List comments via the REST API; find the managed one by sentinel. Each REST comment
# carries an INTEGER `id` (the database id). Do NOT use `gh issue view --json comments`:
# that returns the GraphQL NODE id (`IC_...`), which the REST PATCH below rejects with 404.
gh api "repos/$PUBLIC/issues/$N/comments" --paginate \
  --jq '.[] | select(.body | startswith("<!-- feedback-triage:status v1 -->")) | {id, body}'
```

Parse the `ft-marker` block (the lines between `<!-- ft-marker` and `-->`) into key/values. **Absent ⇒ treat as `state: untriaged`, `reports: 1` (the issue itself counts as one report).** Then **edit that comment in place** — regenerate the whole block (visible text + marker together, so they never drift) into a file and PATCH it by its integer `id`:

```bash
# $COMMENT_ID is the integer `id` from the locate step (NOT the GraphQL node id).
gh api --method PATCH "repos/$PUBLIC/issues/comments/$COMMENT_ID" -F body=@/tmp/ft-comment.md
```

(ID-type contract — pick one API and keep the id matched to it: REST `PATCH /issues/comments/{id}` takes the **integer** id shown above; GraphQL `updateIssueComment(input: { id: <nodeId>, body: … })` takes the **node** id. Mixing them 404s — the founding bug this note prevents.)

## Comment templates (all user-facing copy = warm maintainer voice)

### Consolidation comment (posted on a dupe before closing)

```markdown
Thanks for flagging this! It's the same underlying report as #<survivor>, so we're
consolidating there to keep everything in one place — every report still counts toward
prioritization. Follow #<survivor> for updates. 🙌

📋 This is on our public Known Issues list (we're aware and tracking it): <KI_URL>
```

### Tracking note (rolled into the survivor's managed status comment — Phase 5)

- With a version: `📌 Tracked internally as \`example-repo-private#NNN\` — tracked for **<release_display>**.` (render `<release_display>` via the normalization table above — e.g. `an upcoming Beta update`, NOT the raw `v0.2.0-Beta` token)
- Without: `📌 Tracked internally as \`example-repo-private#NNN\` — tracked for **a future release**.`
- Always append the Known Issues back-reference: `📋 This is on our public Known Issues list (we're aware and tracking it): <KI_URL>`

### Fix-shipped comment (Phase 6, then close)

```markdown
✅ Fixed in **<version>** (shipped <YYYY-MM-DD>). Thanks for reporting — update to the
latest build and it should be resolved. Reopen or file a fresh report if you still see it.

📋 This was on our public Known Issues list and is now resolved: <KI_URL>
```

### Fix-in-progress note (Phase 6, rolled into the managed comment)

```markdown
🔧 A fix for this is in progress, targeting **<version>**.
```

### Declined / not-planned comment (Phase 6, then close)

```markdown
After review we've decided not to pursue this for now. Rationale: <one-line tracker reason>.
Thanks for taking the time to report it — context like this still helps us prioritize.
```

> **Redaction caveat:** `<one-line tracker reason>` is the ONLY private→public *prose* channel in the routine. It MUST be a public-safe summary the routine composes — never the verbatim tracker issue text. Strip internal issue refs, customer/user names, and infra detail before posting; when in doubt, omit the rationale rather than risk leaking private-tracker prose.

> **`<KI_URL>`** in every template above is the pinned **Known Issues** issue URL, resolved in Phase 0 (see [`known-issues.md`](known-issues.md)) — or shown as an unresolved placeholder in a first dry-run, before the issue is created. Every routine comment that informs a reporter carries it, so a user always has a one-click path to the full list — that's the "we're already aware" signal the maintainer asked for.

## Public release-window display (normalization)

The marker's `release` field stores a stable internal token (`v0.2.0-Beta`, `v1.0.0`, `future`, …) — it is the idempotency comparator for "skip if `alpha`+`release` unchanged", so it MUST NOT be rewritten for cosmetic reasons. But that token is **not** the public-facing string: `v0.2.0-Beta` reads like a specific patch that should already have shipped (this project ships rolling Beta patches — e.g. v0.2.1 — so a fix lands in *a future Beta build*, not in a version called "v0.2.0-Beta"). Every render of `release` to a user therefore passes through this display normalization (`release_display`):

| stored `release` | public `release_display` |
|---|---|
| `v0.2.0-Beta` (or `beta`) | `an upcoming Beta update` |
| `v1.0.0` | `the v1.0 release` |
| `v1.1.0` / `v1.2.0` / `v1.3.0` | verbatim (clearly a future major — not mistaken for shipped) |
| `future` | `a future release` |
| anything else | verbatim |

Apply `release_display` at **every** user-facing render of `release`: the Known Issues list entry (`known-issues.md`), the status-comment visible line (below), and the Phase-5 tracking note. The stored marker value stays the token; only the displayed text is normalized — so the no-op-on-unchanged-inbox invariant holds (the comparator is the stored token, which doesn't move). Founding incident: the first auto run rendered the raw `v0.2.0-Beta` token on the public list, which reads as an overdue patch.

## Visible-line regeneration rule

Always rebuild the visible line from the marker so they can't diverge (rendering `release` via `release_display` above):

```
👥 **Reported by {reports} user(s)**
  · 📌 Tracked as `{alpha}`                  (omit the whole clause if alpha empty)
  · 🗓️ Tracked for **{release_display}**     (e.g. "an upcoming Beta update"; "a future release" when release=future)
  · ✅ Fixed in **{fixed_in}**                (only when state=fixed)
```

## Hard rules

- **One managed comment per issue, ever.** Locate-then-edit; if the locate step errors, fail the issue (digest it) rather than risk a duplicate post.
- **Marker and visible text are regenerated together** from field values — never hand-edit one side.
- **Internal reporter IDs never leave the body.** The routine reports *counts*, never the opaque IDs, in any new text it writes (no new PII surface).
- Backtick-fence the `alpha` ref (`` `example-repo-private#NNN` ``) so it doesn't render as a broken cross-repo autolink for anonymous viewers.
