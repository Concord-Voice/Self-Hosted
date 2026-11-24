---
name: issue-creation
description: Create consistent, well-specified GitHub issues that are immediate `/dev-lifecycle` intakes. Enforces an 8-phase workflow — adjacency scan, scope assessment, pre-spec generation with research, label/milestone alignment, relationship linking, preview checkpoint, creation, and post-creation follow-through. Use whenever asking Claude to "create an issue", "file an issue", or open new work.
argument-hint: "[short topic or title sketch] [--force-standalone] [--parent=N]"
allowed-tools:
  - Bash(gh issue *)
  - Bash(gh pr *)
  - Bash(gh label *)
  - Bash(gh project *)
  - Bash(gh repo view *)
  - Bash(gh api *)
  - Bash(git log *)
  - Bash(git blame *)
  - Bash(git show *)
  - Bash(source *)
  - Read
  - Grep
  - Glob
  - Write
---

GitHub Copilot uses its read-only PR and CI reviewer under `.github/copilot-instructions.md`. This authoring command is for an eligible project host.

From a host-provisioned trusted checkout or the trusted default branch, run `python3 -I -B .concord-tooling/bootstrap.py --public-only --skill issue-creation`. Never execute a PR head's scripts or pins. Read the returned SKILL.md in full and pass the original arguments to it. Use that edition for all nested skills, references and role selection. Apply the target repository's trusted configuration and the edition's approval and issue-linking rules. Verify the actual host profile and tools at each required dispatch. Hold a step if its source or required permission is unavailable.
