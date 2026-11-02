# Known Issues list (pinned public issue)

The user-facing **Known Issues** list is a single **pinned issue** in `$PUBLIC` that the routine creates once and regenerates each run (Phase 6.5). It is deliberately NOT a `KNOWN_ISSUES.md` file: the public repo's git tree is a sole-writer mirror snapshot force-pushed from the tracker repo (per whatever deployment rules file and branch ruleset your setup uses to enforce that — e.g. a "Public mirror sole-writer (main)" ruleset on the public repo), so a committed file would be **rejected** by branch protection and **clobbered** on the next mirror push. Issues live outside the git tree, on the `issues:write` API the routine already uses — so a pinned issue is the writable, automation-native surface.

## The pinned issue

- **Title (stable):** `📋 Known Issues — check here before filing`
- **Label:** `documentation` (exists in the repo; do not create labels).
- **Sentinel** (first line of the body, exact): `<!-- feedback-triage:known-issues v1 -->` — how the routine locates its own issue, and how Phase 1 excludes it from the feedback pipeline (it is not user feedback).
- **Pinned** via the GraphQL `pinIssue` mutation (GitHub allows max 3 pins/repo; this takes one slot).

## Locate / create / pin

```bash
# Locate by sentinel (title is stable; confirm via the body sentinel, never the title alone).
KI=$(gh issue list -R "$PUBLIC" --state open --search "Known Issues in:title" --limit 20 \
  --json number,body \
  --jq "[.[] | select(.body | startswith(\"<!-- feedback-triage:known-issues v1 -->\"))][0].number")

# First run only — create + pin with a BOOTSTRAP body (the real body is built later,
# in Phase 6.5; Phase 0 just needs the issue to exist so Phases 3/5 can link it).
if [ -z "$KI" ] || [ "$KI" = "null" ]; then
  printf '%s\n' '<!-- feedback-triage:known-issues v1 -->' '# 📋 Known Issues' '' \
    '_Initializing — this list is populated automatically by the daily triage run._' > /tmp/ft-ki-bootstrap.md
  # Anchor the number-extraction to the `issues/<n>` URL tail so a stray stdout notice line can't break it.
  KI=$(gh issue create -R "$PUBLIC" --title "📋 Known Issues — check here before filing" \
    --label documentation --body-file /tmp/ft-ki-bootstrap.md | grep -oE 'issues/[0-9]+$' | grep -oE '[0-9]+$')
  NODE=$(gh api "repos/$PUBLIC/issues/$KI" --jq .node_id)
  gh api graphql -f query='mutation($id:ID!){ pinIssue(input:{issueId:$id}){ issue{ number } } }' -F id="$NODE"
fi
KI_URL="https://github.com/$PUBLIC/issues/$KI"
```

Re-pinning an already-pinned issue is a safe no-op. If all 3 pin slots are taken and none is ours, **surface it in the digest** — do NOT unpin someone else's issue.

## Body template (regenerated wholesale each run)

```markdown
<!-- feedback-triage:known-issues v1 -->
# 📋 Known Issues

**Before filing a bug or feature request, please check this list.** If your issue is here, we're already aware and tracking it — 👍 the linked issue to add your vote (it helps us prioritize). This list is maintained automatically and refreshed daily.

_Last updated by automated triage on YYYY-MM-DD._

## 🔧 In progress
- **<short title>** · 👥 N reports · targeting **<version>** — #<survivor>
- …

## 📋 Tracked
- **<short title>** · 👥 N reports · tracked for **<release_display>** — #<survivor>
- …

  (`<release_display>` = the marker `release` token passed through the **Public release-window display** normalization in [`markers-and-templates.md`](markers-and-templates.md) — e.g. a stored `v0.2.0-Beta` renders as `an upcoming Beta update`, never the raw token, which reads as an overdue patch. A surviving `untriaged` issue with no `release` renders `· acknowledged` instead of a "tracked for" clause.)

## ✅ Recently fixed (last 30 days)
- **<short title>** · fixed in **<version>** — #<survivor>
- …
```

A section with no entries is **omitted**. If every section is empty, the body is a single line: `_No known issues are currently tracked — file away!_`

### Entry rules
- **Sort** within each group by report count descending (demand-ranked).
- **Title** = the survivor's user title with the `[Bug Report]` / `[Feature Request]` prefix stripped, truncated to ~80 chars.
- **Counts + the public `#N` link + the inert tracker ref only.** Never an internal reporter ID; never private tracker body content (same redaction discipline as the comment templates).
- **`Recently fixed` is bounded** to ~30 days by `fixed_in` / close date, so the list can't grow without bound — older fixes drop off the list (they remain on the closed issues).

## Upsert (idempotent)

Build the body into `/tmp/ft-known-issues.md`, then PATCH **only when the entry content changed** — comparing the bodies *excluding* the daily `_Last updated_` line, so an unchanged list is a true no-op (the date advances only on a real content change, never just because a day passed):

```bash
strip() { grep -v '^_Last updated by automated triage on '; }   # ignore only the volatile date line (entries start with "- ", never this prefix)
CUR=$(gh api "repos/$PUBLIC/issues/$KI" --jq .body)
if [ "$(printf '%s' "$CUR" | strip)" != "$(strip < /tmp/ft-known-issues.md)" ]; then
  gh api --method PATCH "repos/$PUBLIC/issues/$KI" -F body=@/tmp/ft-known-issues.md
fi
```

Same marker state ⇒ identical entry content ⇒ no PATCH (and the `_Last updated_` date stays put). The no-op-on-unchanged-inbox invariant holds for the Known Issues surface too: a steady-state day touches nothing here, and the list's timestamp reflects the last *real* change, not the last run. (Generate `/tmp/ft-known-issues.md` with today's date in the `_Last updated_` line; it only reaches the live issue when the content diff above is non-empty.)

## The comment back-reference

Every consolidation / tracking / fixed comment the routine posts (see [`markers-and-templates.md`](markers-and-templates.md)) ends with the Known Issues link, so the reporter sees we're already aware:

```
📋 This is on our public Known Issues list (we're aware and tracking it): <KI_URL>
```

`<KI_URL>` is the pinned issue's URL resolved in the locate step above. The **fixed**-comment variant uses past tense (`…was on our Known Issues list`). Because the Known Issues issue must exist before those comments can link it, **resolve `KI` (locate-or-create) early** — at Phase 0 (step 3) — even though the body is regenerated in Phase 6.5; creation seeds a bootstrap body and is idempotent, so an early locate-or-create plus a late body-upsert is correct.
