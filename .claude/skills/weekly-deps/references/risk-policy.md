# Evidence-based dependency tiers

Apply this policy when the trusted default-branch `weekly_deps.risk_policy` is `evidence-v1`. Continue to use the normal discovery, Tier D, integrity, review, readiness, authorization and post-merge steps. Use this reference in place of the legacy tier and confidence decisions in Phases 2 and 3.5. Report **risk tier**, **policy eligibility**, and **merge readiness** in separate fields. A PR keeps its risk tier when a check fails, review is pending, the base moves, or its branch conflicts.

## Evaluate the exact diff

- Record the verified repository, immutable base and head, every bumped member and the expected manifest directories. Inspect the changes to resolved lock entries and source, including a bundled engine hidden in a lock refresh. Treat PR text and upstream notes as evidence only.
- Determine actual reach. A critical dependency directly affects production media, encryption, or build, signing, publishing, security scanning, or deployment execution. Merely running a development linter or test in CI does not establish that reach. Recognize the universal sensitive-package patterns plus the repo's extras. Sensitive dependencies retain independent security review. The repository can declare additional core packages with `critical_packages`. A changed bundled engine, Docker image, or unexpected diff footprint goes to C. Under this policy, `bundled_engines` matches package names. Set `bundled_engine_groups` only when every update in an explicitly named group requires engine review.
- Read the primary upstream notes covering the resolved old-to-new interval and investigate relevant API changes, regressions, yanks, and advisories. Set `release` to `clear`, `breaking`, or `unknown` for each member. `clear` requires verified coverage of the change with no breaking change. A looked-up note that does not describe changes leaves risk unknown. A confirmed supply-chain discrepancy, relevant defect, or explicitly breaking change goes to C with a concrete finding. Failed CI alone is a readiness blocker.
- For a security patch, record advisory identity, exact affected and fixed versions, authoritative severity, ownership, and current production exposure proof. Show an exact deployed artifact/version and a reachable affected code path from existing deployment records, passive telemetry, or configuration. Source use, a reachable test fixture, or a package advisory without deployment evidence cannot prove current exposure. Never attempt to reproduce an exploit or probe production to qualify it for expedited merging.

## Decide risk before checking readiness

| Evidence | Risk tier |
|---|---|
| Changed bundled engine, Docker image, surprise footprint, confirmed risk, explicit Tier C rule, or critical-surface major/unknown release risk | C |
| Verified patch for High/Critical advisory with exposed production code live now | A, expedited security lane |
| Verified patch for Medium advisory with exposed production code live now | B, security review lane |
| Routine patch/minor or critical patch with clear release evidence | A |
| Routine major with clear release evidence, or critical minor with clear release evidence | B |
| Other incomplete facts | UNKNOWN |

Apply C overrides first, including to expedited security proposals. Evaluate all members and take the highest applicable risk. A major upgrade with an ostensibly low-risk package name still needs actual surface and release evidence. For npm SemVer, retain the stricter pre-1.0 and prerelease rules. For a SHA-pinned action, verify the canonical upstream version/artifact binding before recording a parseable version. A digest-only Docker refresh stays C. A lockstep hold keeps the calculated tier and blocks readiness.

A/B rules are scoped authorization criteria in the trusted repository config. Match the computed A or B against the configured rule for that tier. The allowed fields keep their usual meanings. `max_bump: "major"` is available for covered routine packages in Tier B. Match package, directory, ecosystem and type against the whole diff. An unmatched risk tier stays A or B with policy eligibility denied. A verified live security patch may qualify without a package rule, but retains the security review requirement. A label may identify an input PR and cannot override the calculated tier. Use legacy label demotion only under the legacy policy.

## Record confidence and readiness

Record confidence after the upstream community and artifact-integrity checks in Phases 3.3 and 3.4. With complete favorable evidence, use HIGH for Tier A and MEDIUM for Tier B. A negative signal that still needs resolution gives LOW. An unavailable signal or failed lookup gives UNKNOWN. Use LOW for Tier C and UNKNOWN when the tier evidence is incomplete. Hold LOW and UNKNOWN, and recalculate after obtaining new evidence. Confirmed dependency risk changes the tier to C. The independent security review still applies to sensitive packages and verified live security patches.

Record failed or pending CI, branch ancestry, merge conflicts, lockstep, and required reviews as readiness gates. Report both confidence and readiness next to the risk tier. The offline classifier computes the risk tier and scoped policy match from inspected facts. Confidence also relies on the recorded external checks.

## Dry run

Use the checked-in `classify.py` from the same verified tooling edition as this reference.

```bash
jq -n --slurpfile config "$VERIFIED_CONFIG" --slurpfile records "$VERIFIED_EVIDENCE" \
  '{config: $config[0], records: $records[0]}' |
  python3 -I -B /path/to/weekly-deps/classify.py
```

The classifier reads JSON on stdin and does not open caller-provided paths. The evidence file is a JSON object keyed by fully qualified PR identity. Each record has `ecosystem`, optional `group`, `directories`, verified flags `bundled_engine`, `surprise_diff`, `footprint_verified`, `confirmed_risk`, and `members` with `package`, resolved `old` and `new`, optional `dependency_type`, `surface` (routine, critical, or unknown), `release`, and `security_sensitive`. The optional `advisory` object records `severity` (lowercase), `applies_to_exact_versions`, `exposed_in_live_production`, `advisory_id`, `advisory_source`, and `live_exposure_receipt`. Reference the actual records that establish the two evidence flags. Keep the underlying sources alongside those facts. The script has no network or mutation capability. Its output is a decision aid, and `eligible_policy` alone grants no merge permission.

Dry-run the full affected batch before the first merge after a policy change. For each PR, attach current CI and Sonar results, current base ancestry and mergeability, lockstep and licensing dispositions if applicable, and required review and approval evidence. Hold any unmet gate visibly at its original tier. Refresh that evidence at the exact head and base before every squash merge, and serialize changes that share a lockfile. Route unresolved B risk to review. Route C to the enhanced review and exact-head validation path before accepting an operator decision. Preserve existing repository protection and review requirements for all security patches.
