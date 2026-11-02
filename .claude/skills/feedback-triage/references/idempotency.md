# Idempotency & crash-safety

The routine runs every 24h. A correct run with no new feedback makes **zero** mutations. Every action is guarded so it fires only on a genuine state transition, and the guard is re-derivable from durable GitHub state even if a prior run crashed after acting but before writing its marker.

## Two sources of truth, unioned

For each issue, derive "what has already been done" from **both**:

1. **The managed marker** (fast path — `references/markers-and-templates.md`).
2. **Durable GitHub state** (authoritative fallback — survives a lost marker write):
   - Issue open/closed + close reason.
   - Presence of a routine **sentinel comment** (consolidation / fixed / declined copy).
   - Labels (`duplicate`, `wontfix`, …).
   - For tracker linkage: a tracker issue whose body contains the public issue URL.

When the two disagree, **durable state wins** and the marker is rewritten to match. This closes the "crashed mid-run" window: e.g. a dupe that is already `closed` + has the consolidation sentinel is treated as done even if the survivor's marker never got the `consolidated` entry — and the next run repairs the marker without re-closing anything.

## Per-action guards

| Action | Fire only if … | Re-derivation fallback |
|---|---|---|
| Count a member into a survivor | member # not already in survivor's `consolidated` list | member issue is open & lacks the consolidation sentinel |
| Post consolidation comment + close dupe | dupe is open AND has no consolidation sentinel comment | — |
| Seed 👍 on survivor | survivor has no 👍 from the running account | `gh api reactions` is per-account idempotent — re-adding is a safe 200, but check first to avoid log noise |
| Create tracker tracking issue | marker `alpha` empty AND the tracker search **with literal-URL confirmation** (SKILL.md Phase 4) finds no back-ref | the confirmed search **is** the fallback — never create if a back-ref exists |
| Post/refresh tracking note | derived `alpha`+`release` differ from marker's | — |
| Post fix-shipped + close public | marker `state` ≠ `fixed` AND the tracker issue is closed-completed | public issue still open & lacks the fixed sentinel |
| Post in-progress note | marker `state` ≠ `in-progress` AND the tracker issue is in-progress | — |
| Create + pin Known Issues issue | no open issue carries the `known-issues v1` sentinel | locate-by-sentinel is the guard — never create a second; if 3 pins are taken and none is ours, digest it |
| Regenerate Known Issues body | freshly-built body differs from the live pinned-issue body | the live body **is** the comparison source — identical ⇒ no PATCH (no-op run touches nothing here either) |

## Reverse-sync state machine (Phase 6)

Read the tracker issue's state, map to a target public state, act only on the edge:

```
                 ┌─────────────┐
 untriaged ──►   │  tracked    │  (tracker issue exists, open)
                 └─────┬───────┘
      tracker in-progress │           tracker closed-completed
                 ┌────────▼────────┐  ┌───────────────────────┐
                 │  in-progress    │─►│  fixed  (close public) │
                 └─────────────────┘  └───────────────────────┘
      tracker closed not-planned │
                 ┌───────────────▼─────┐
                 │ declined (close public)
                 └─────────────────────┘
```

- Transitions are **monotonic toward closure**; never reopen a public issue the routine closed (a human can). If a closed-fixed public issue's tracker twin reopens, surface it in the digest for human attention rather than auto-reopening.
- "Tracker closed-completed" = closed with state_reason `completed` AND (a merged fix PR referencing it OR a shipped release tag/build containing the fix — NOT merely the issue's deadline milestone, which under the corrected model is a complete-by version, not proof the fix shipped). A bare manual close without those signals is treated as `in-progress`/ambiguous and digested, not auto-propagated to a public close.

## Ordering within a run (crash leaves a safe partial state)

1. **Survivor first, dupes second** in consolidation — a crash after counting but before closing dupes just means dupes get closed next run (survivor already correct, guard prevents double-count).
2. **Tracker create → marker write → public status note** — if it crashes after create but before the marker write, the Phase 4 tracker-search fallback finds the issue next run and resumes at the marker write (no duplicate tracker issue).
3. **Public close is the LAST action** for any issue in a given phase — never close before the explanatory comment and marker update are posted, so a closed issue always carries its reason.

## What "no-op run" looks like

A healthy steady-state daily run on an unchanged inbox: catalogs everything, finds every marker current, derives no transitions, posts nothing, closes nothing, and writes a digest reading `0 consolidations, 0 created, 0 closed — inbox steady`. If a run ever mutates on an unchanged inbox, that's a guard bug — treat it as a regression.
