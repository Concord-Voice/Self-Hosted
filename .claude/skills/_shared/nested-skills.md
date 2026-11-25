# Running a named skill

Resolve the requested name in the active client's skill catalog for this project.
Use its reported source and namespace. If the project selects an owner-specific
version, verify that source before running it. Preserve model-invocation
restrictions, required approvals and the current mode.

When the client exposes a skill invocation tool, invoke the resolved name with
the requested arguments. On clients that activate skills by reading their
instructions, load the resolved `SKILL.md`, bind the arguments and target
repository, follow its steps and open its relative references from the skill
directory. Resume the caller when the nested skill returns its result.

If the skill is missing, collides with another source, or requires a capability
this session cannot use, report the affected phase, skill, and missing capability.
Keep a mandatory phase pending until it can actually run. A catalog entry or
a file read alone does not complete the nested workflow.
