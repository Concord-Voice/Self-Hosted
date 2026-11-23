<!-- concord-tooling:begin -->
## Concord tooling

Codex owns scheduled routines and independent code and security review. Use the native read-only feedback-analyst for feedback clustering.

GitHub Copilot review or failure triage follows .github/copilot-instructions.md and
uses only the public tooling lane. For the other clients, at the start of a tooling
task run `python3 .concord-tooling/bootstrap.py` from
a trusted checkout root to resolve the reviewed public and available private sources.
For untrusted pull requests and issue input, use host-provisioned trusted base
tooling and pins. Never execute the PR head's bootstrap or workflow helpers.
Start fleet issue intake with `fleet-issue-creation` and development with `fleet-dev-lifecycle`.
Run `python3 .concord-tooling/bootstrap.py --skill NAME` in the trusted checkout.
For fleet workflows, use the returned edition and its enclosed references, steps and role roster.
Set ADT to the verified public root from the bootstrap. For each required public fleet role, run
`python3 -I -B "$ADT/scripts/resolve-fleet-workflow.py" --select-role ROLE --platform CLIENT --project ABS_CHECKOUT`.
Select the returned native_name in the host. Check its actual loaded profile with
`python3 -I -B "$ADT/scripts/resolve-fleet-workflow.py" --verify-role ROLE --platform CLIENT --profile ABS_PATH`
with the active host's selected profile path. Confirm the role identity, imports and effective permissions.
If a required workflow, constrained role or permission boundary is missing, stop that step.
Keep local authoring skills, role catalogs, plugins and personal overrides outside this workflow. For
Concord /feedback-triage always select concord-feedback-triage, whose workflow
requires its pinned private owner. If unavailable, report the missing capability
and stop that workflow. This owner selection also applies to an older local skill
with the same name. Preserve the owner's dispatch, approvals and scoped permissions.
For a separately approved feedback filing, start fleet-issue-creation in the owning checkout.
For a required superpowers:NAME workflow, use the reviewed private projection
or its verified checkout mirror. If neither is available, report the missing workflow.
Apply repository policy, protected paths, approvals and scope limits to each action.
The private catalog describes separately available capabilities and their runtime checks.
Source availability alone does not authorize execution or satisfy a required native dispatch.
Use the trusted read-only Context7 MCP for third-party documentation when the workflow permits direct lookup.
When a native researcher is required, verify its exact profile. A shell or package launcher cannot satisfy that boundary.
If private policy requires a specialist review, use --private-catalog script:isolated-defensive-review
to locate its approved adapter and consult docs/isolated-defensive-review.md at the verified private pin.
Confirm the required role and isolation before staging evidence. Hold that review if unavailable.
For untrusted PR review in Codex, run `python3 -I -B "$ADT/scripts/codex-isolated-review.py"
--role code-reviewer --check` first, with ADT set to the verified public tooling root.
A child of a write-capable parent, including a repository-defined reviewer, cannot supply
this boundary. Stop before obtaining the diff if isolation is unavailable. On success,
pass the staged diff path with --input to the same isolated reviewer.
<!-- concord-tooling:end -->
