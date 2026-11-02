# `.claude/repo-config.json` — per-repo configuration (org schema)

A single, **optional** JSON file at a repo's root that parameterizes the shared
Master skills, hooks, and docs agents. Absent file, or any absent/`null` key,
means the consumer **degrades gracefully** (skips that feature) — never errors.
Read it with `scripts/repo-config.sh` (`rc_get '<jq-filter>' [default]`, `rc_has '<jq-filter>'`).
`owner`/`repo` and `default_branch` are auto-derived from `gh` when not set here.

| Key | Type | Default / when absent | Consumed by |
|---|---|---|---|
| `default_branch` | string | `gh repo view --json defaultBranchRef` → else `main` | dev-lifecycle, release-notes |
| `docs.spec_dir` / `docs.plan_dir` | string | `docs/superpowers/specs/` / `…/plans/` | dev-lifecycle |
| `project` | object\|null | null → all project-board ops skipped | issue-creation, pr-creation, dev-lifecycle |
| `project.id` | string | — | board mutations (`PVT_…` node id) |
| `project.number` / `project.owner` | int / string | `gh` user when absent | `gh project item-add` |
| `project.fields.status.id` + `.options{}` | string + map | — | move PR/issue Status |
| `project.fields.priority.id` + `.options{}` | string + map | — | set Priority |
| `project.fields.size.id` + `.options{}` | string + map | — | set Size |
| `labels.namespaces` | string[] | `["type","area","domain","release","risk","routine","workflow-validation"]` | issue-creation, pr-creation, labels rule |
| `labels.source` | string | `.claude/rules/labels.md` (skip validation if absent) | pr-creation label validation |
| `board.release_label_prefix` | string | `release:` | phase-status |
| `board.priority_labels` | string[] | `["P1","P2","P3","P4","P5"]` | phase-status |
| `migrations.dir` | string | `migrations/` | migration-reviewer, create-migration, protect-applied-migrations.sh |
| `paths.electron_dir` | string | `.` (repo root) | ipc-channel-audit, pr-review-agent-dispatch |
| `red_team.focus_surfaces[]` | string[] | `[]` → red-team uses the Focus Surfaces section written inline in its agent file | red-team |
| `bots` | object[] | `[]` → only generic `Bot: {login}` naming | enhanced-pr-review, dev-lifecycle, weekly-deps |
| `bots[].match` | string | login substring (case-insensitive) for display naming | bot fetch |
| `bots[].name` / `.login` / `.ci_check` / `.marker` | string | display name / bot login / CI check name / PR-body marker | bot fetch, wait-for-bots |
| `bots[].blocking` | bool | `false` → finding is advisory | merge-gate honesty check |
| `bots[].offline` | bool | `false` | wait-for-bots (skip offline bots) |
| `bots[].when` | string | `always` \| `active` \| `shippable`; absent → `always` | wait-for-bots gating |
| `review_agents.base` | string[] | `["security-reviewer","code-reviewer","docs-reviewer"]` | review-pr / enhanced-pr-review |
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
| `version_bump.enabled` | bool | `false` → version-bump step skipped | dev-lifecycle |
| `version_bump.source_check` / `.bump_file` | string | — | version-bump handoff |
| `branch_protection.doc_path` / `.code_owner_team` | string | optional prose substitution | enhanced-pr-review, dev-lifecycle |
| `ecosystems` | string[] | `[]` → auto-detect from live PR diffs | weekly-deps |
| `weekly_deps.tiers` | object | `{}` → every bump treated Tier C (manual) | weekly-deps |
| `weekly_deps.security_sensitive_extra` | string[] | `[]` (merged over shared default list) | weekly-deps |
| `weekly_deps.signal3_repo_map` | map | `{}` | weekly-deps community signal |
| `weekly_deps.tier_d_sources` | object[] | `[]` → Tier D yields nothing | weekly-deps |
| `weekly_deps.local_test_cmds` | map(dir→cmd[]) | auto-default from ecosystem | weekly-deps |
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

**Rule:** never put a secret in `repo-config.json` (it is committed). Secrets go through env-var interpolation in `.mcp.json` (see `mcp/README.md`).
