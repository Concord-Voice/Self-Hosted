# `.claude/repo-config.json` — per-repo configuration (org schema)

A single, **optional** JSON file at a repo's root that parameterizes the shared
Master skills, hooks, and docs agents. Absent file, or any absent/`null` key,
means the consumer **degrades gracefully** (skips that feature) — never errors.
Read it with `scripts/repo-config.sh` (`rc_get '<jq-filter>' [default]`, `rc_has '<jq-filter>'`).
`owner`/`repo` and `default_branch` are auto-derived from `gh` when not set here.

| Key | Type | Default / when absent | Consumed by |
|---|---|---|---|
| `default_branch` | string | `gh repo view --json defaultBranchRef` → else `main` | release-notes. dev-lifecycle, verify-quality-gate and pr-creation ignore it and use the GitHub API, then `refs/remotes/origin/HEAD`, then `main`, because a PR can edit this key. |
| `docs.spec_dir` / `docs.plan_dir` | string | `docs/superpowers/specs/` / `…/plans/` | dev-lifecycle |
| `docs.counts_script` | string | unset → count-drift probe skipped | dev-lifecycle Phase 4, enhanced-pr-review §2.5.0: repo-relative script that dry-runs (no args) or applies (`--apply`) a metric-count refresh. Read from the default-branch tip (`refs/remotes/origin/HEAD`), never the checkout; it runs only if it is a regular tracked file there (not a symlink) and the branch matches the tip at it and its directory. |
| `docs.counts_script_deps[]` | string[] | `[]` → the script must call no other repo file | dev-lifecycle Phase 4, enhanced-pr-review §2.5.0: repo-relative files or directories the counts script sources or calls. Read from the default-branch tip. Entries must be plain relative paths (one bad entry skips the probe). The probe is skipped with a WARN when the branch differs from the tip under any of them or the directory of a file dep, when one is a symlink, or when untracked files (ignored ones included) sit there. |
| `docs.count_globs[]` | string[] | `[]` → run the probe on every PR when `counts_script` is set | dev-lifecycle Phase 4: paths whose change can make a count stale, as git `:(glob)` pathspecs from the repo root (`*` stays within a directory, `**` crosses them) |
| `project` | object\|null | null → all project-board ops skipped | issue-creation, pr-creation, dev-lifecycle |
| `project.id` | string | — | board mutations (`PVT_…` node id) |
| `project.number` / `project.owner` | int / string | `gh` user when absent | `gh project item-add` |
| `project.fields.status.id` + `.options{}` | string + map | — | move PR/issue Status |
| `project.fields.priority.id` + `.options{}` | string + map | — | set Priority |
| `project.fields.size.id` + `.options{}` | string + map | — | set Size |
| `labels.namespaces` | string[] | `["type","area","domain","release","risk","routine","workflow-validation"]` | issue-creation, pr-creation, labels rule |
| `labels.source` | string | unset → `.claude/rules/labels.md`, validation skipped if that file is absent; set but missing → validation fails closed | pr-creation label validation |
| `board.release_label_prefix` | string | `release:` | phase-status |
| `board.priority_labels` | string[] | `["P1","P2","P3","P4","P5"]` | phase-status |
| `migrations.dir` | string | `migrations/` | migration-reviewer, create-migration, protect-applied-migrations.sh |
| `paths.electron_dir` | string | `.` (repo root) | ipc-channel-audit, pr-review-agent-dispatch |
| `agents.escalation` | string | unset → no escalation agent; an E1-E4 hit is surfaced to the developer | dev-lifecycle, reproduce-then-fix, weekly-deps (`lifecycle-agent-dispatch.md` § Escalation) |
| `agents.design_rows[]` | array of `{agent, match?: [globs], with?: agent}` | `[]` → the shared Phase-2 panel alone | lifecycle-agent-dispatch Phase 2: extra stage-1 design seats, hand-dispatch only (the `design-panel` Workflow does not read it yet). A row fires when a `match` glob is in scope or the `with` agent is dispatched. |
| `agents.producer_rows[]` | array of `{match: [globs], agent}` | `[]` → the shared Phase-3 producer table alone | dev-lifecycle Phase 3 (`lifecycle-agent-dispatch.md` § producer table); checked after shared rows 1–2 (tests, migrations) and before row 3; first match wins; `agent` matches `\A[a-z0-9][a-z0-9-]*\z`; a non-array value or malformed row is skipped and named; repo-row matches are E1–E4 eligible unless the agent is one § Escalation lists as ineligible |
| `red_team.critical_agent` | string | unset → no T3 tier; read from the merge base | enhanced-pr-review (`pr-review-agent-dispatch.md` § risk tiers) |
| `red_team.critical_paths[]` | string[] | `[]` | globs whose change forces T3; ignored unless `critical_agent` is set |
| `red_team.focus_surfaces[]` | string[] | `[]` → red-team uses the Focus Surfaces section written inline in its agent file | red-team |
| `bots` | object[] | `[]` → only generic `Bot: {login}` naming | enhanced-pr-review, dev-lifecycle, weekly-deps |
| `bots[].match` | string | login substring (case-insensitive) for display naming | bot fetch |
| `bots[].name` / `.login` / `.ci_check` / `.marker` | string | display name / bot login / CI check name / PR-body marker | bot fetch, wait-for-bots |
| `bots[].blocking` | bool | `false` → finding is advisory | merge-gate honesty check |
| `bots[].offline` | bool | `false` | wait-for-bots (skip offline bots) |
| `bots[].when` | string | `always` \| `active` \| `shippable`; absent → `always` | wait-for-bots gating |
| `bots[].login` (array form) | string[] | — | a bot that posts under more than one login; matched exactly, never as a substring |
| `bots[].ack_pattern` / `.decline_pattern` / `.head_verdict_pattern` | string (regex) | unset → no special handling | dev-lifecycle Step 7.5.2, enhanced-pr-review: tell a real verdict apart from an acknowledgement, a refusal, or an in-place-edited verdict |
| `review_agents.mandatory` | string[] | unset (floor is `["security-reviewer","code-reviewer","docs-reviewer"]`) | review-pr / enhanced-pr-review. Replaces the always-run floor. Read from the merge base of the head with the **default branch**, never the checkout or a PR-chosen base, so a PR cannot shrink its own floor. Must be a non-empty array of names matching `^[a-z0-9][a-z0-9-]*$`. A malformed value, a non-object `review_agents`, or a config that is not a regular file stops the review. |
| `review_agents.base` | string[] | `[]` | review-pr / enhanced-pr-review. Extra always-run agents, added to the floor; it can never remove one. |
| `review_agents.patterns[]` | `{glob\|label\|content, agent}` | `[]` | specialist dispatch; each entry declares exactly ONE condition — `glob` (changed-file match; no `\|` alternation; `dir/**` any depth, `dir/*` one level), `label` (exact PR label), or `content` (literal substring in the full diff — catches semantic changes made from off-glob files). Repeat the agent for multiple conditions (dispatchers de-dupe by agent, so it runs once). |
| `release_signal` | object\|null | null → Phase 2.6 / release-classification skipped | enhanced-pr-review, dev-lifecycle |
| `release_signal.shippable_globs` | string[] | `[]` | "shippable code changed" gate |
| `release_signal.marker` | string | — | enhanced-pr-review, release-classification |
| `release_signal.version_manifest` | string | unset → version-bump skipped | dev-lifecycle, release-classification |
| `quality_gate.enabled` | bool | `false` → quality-gate steps skipped | dev-lifecycle, verify-quality-gate |
| `quality_gate.coverage_min`/`blockers_max`/`hotspots_max`/`doc_path` | int/int/int/string | — | acceptance criteria, verify-quality-gate |
| `quality_gate.checks[]` | `{name,dir,cmd,fast_cmd,group}` | `[]` → verify-quality-gate reports UNVERIFIED and runs no commands (it never infers a toolchain) | verify-quality-gate |
| `quality_gate.server` | string | `none`; `sonarqube` reads the server verdict via MCP when those tools are available | verify-quality-gate |
| `epic_linkage.enabled` | bool | `false` → parent-epic linkage skipped | dev-lifecycle |
| `epic_linkage.epic_title_pattern` / `.epic_label` | string | `Epic: v{version} release` / `type: epic` | epic linkage |
| `epic_linkage.version_epics` | bool | `true`; `false` → feature epics only, no missing-version-epic flag | phase-status |
| `agents.escalation_surfaces` | string[] | `[]` | reproduce-then-fix E2 only: extra surfaces (for example a concurrency fence) that count as escalation-worthy in its debugging loop; dev-lifecycle's E2 list is fixed in `_shared/lifecycle-agent-dispatch.md` |
| `visual.launch[]` | array of `{match: [globs], dir, cmd, note?}` | `[]` → `/run` or the repo's documented path | dev-lifecycle Phase 4 visual verification: the launcher for each UI surface, picked by the changed files. |
| `feedback_triage.public` / `.tracker` | string (`owner/repo`) | unset → Phase 0 stops | feedback-triage: public intake repo and private tracker |
| `feedback_triage.sentinel_prefix` | string | `feedback-triage` | feedback-triage: HTML-comment marker prefix (`<!-- {prefix}:status v1 -->`) |
| `release.frontend_dir` / `.backend_dir` | string | `frontend` / `backend` | release-checklist |
| `release.tag_prefix` | string | `v` | release-checklist, release-notes |
| `release.workflow` | string | `release.yml` | release-checklist |
| `version_bump.enabled` | bool | `false` → version-bump step skipped | dev-lifecycle |
| `version_bump.source_check` / `.bump_file` | string | — | version-bump handoff |
| `branch_protection.doc_path` / `.code_owner_team` | string | optional prose substitution | enhanced-pr-review, dev-lifecycle |
| `ecosystems` | string[] | `[]` → auto-detect from live PR diffs | weekly-deps |
| `weekly_deps.tiers` | object | `{}` → every bump treated Tier C (manual) | weekly-deps |
| `weekly_deps.security_sensitive_extra` | string[] | `[]` (merged over shared default list) | weekly-deps |
| `weekly_deps.signal3_repo_map` | map | `{}` | weekly-deps community signal |
| `weekly_deps.tier_d_sources` | object[] | `[]` → Tier D yields nothing | weekly-deps |
| `weekly_deps.local_test_cmds` | map(dir→cmd[]) | auto-default from ecosystem | weekly-deps |
| `weekly_deps.ci_workflow` | string | `""` → no required-workflow check; any PR with zero checks is `missing` | weekly-deps Phase 3.1 |
| `weekly_deps.deep_review_skill` | string | `/enhanced-pr-review` (fallback: `/review-pr` manual) | weekly-deps |
| `weekly_deps.cron.{prep,triage,tz_label}` | string | `0 13 * * 3` / `0 15 * * 3` / `ET` | weekly-deps autonomous mode |
| `weekly_deps.digest_issue_title` | string | `Weekly Dependency Triage — Routine Output` | weekly-deps |
| `skip_commit_patterns` | string[] | `[]` → no extra commits skipped | release-notes |
| `ai_policy_doc` | string | unset → glob `docs/policies/ai-*.md`, else omit | release-notes |
| `docs_map.sources[]` | `{glob,docs[]}` | `[]` → docs-reviewer uses heuristics | docs-reviewer, docs-writer, release-notes |
| `docs_map.redundancy[]` | `{fact,source_of_truth,mirrors[]}` | `[]` | docs-reviewer |
| `docs_map.tiers` | `{high[],mid[],low[]}` | heuristic (root README=high, …) | docs-reviewer, docs-writer |
| `docs_map.drift` | object\|null | null → drift-hook scan skipped | docs-reviewer, docs-writer |
| `docs_map.drift.config` | string | required when `drift` is non-null; path to the hook-remediation allowlist JSON (e.g., `".claude/hooks-config.json"`) | docs-reviewer, docs-writer |
| `hooks.protected_patterns` | string[] | `[]` → protect-sensitive blocks nothing extra | protect-sensitive.sh |
| `hooks.test_reminders[]` | `{regex,message,stop}` | `[]` → no domain reminders | test-reminder.sh |
| `hooks.test_paths` | object | defaults below | test-reminder.sh |
| `hooks.test_paths.source_ext` | string[] | `[]` → no coverage nag | test-reminder.sh |
| `hooks.test_paths.skip_ext` | string[] | `["md","json","yaml","yml","css","sql","sh"]` | test-reminder.sh |
| `hooks.test_paths.test_skip_re` | string | `(_test\.go\|\.test\.(ts\|tsx)\|\.spec\.(ts\|tsx))$` | test-reminder.sh |
| `hooks.test_paths.remaps[]` | `{src,dst}` | `[]` → sibling `*.test.ext` | test-reminder.sh |
| `hooks.test_paths.docs_reminder` | string | `""` → no docs-drift nag | test-reminder.sh |

**Protect the guard itself.** `protect-sensitive.sh` only guards what `protected_patterns` lists, so a repo with no config, or a config that leaves out the tooling's own files, lets an agent edit the hooks that are meant to stop it. Every repo should list at least the three tooling patterns in the example: `(^|/)\.claude/(hooks/|settings\.json|repo-config\.json|\.agentic-dev-tooling\.lock$|state/hook-bypass/)`, `(^|/)scripts/repo-config\.sh$` and `(^|/)\.github/workflows/`. Start them with `(^|/)`, not `/`: the hook also tests repo-relative paths such as `.claude/hooks/x.sh`, which carry no leading slash. The `state/hook-bypass/` part matters most: a bypass marker there lets edits through before any pattern is checked, so an agent that can write one can edit every protected file. With it listed, only a person working outside the agent can create a marker. Pair the patterns with `CODEOWNERS` entries for the same paths so a human reviews every change to them.

**Rule:** never put a secret in `repo-config.json` (it is committed). Secrets go through env-var interpolation in `.mcp.json` (see `mcp/README.md`).
