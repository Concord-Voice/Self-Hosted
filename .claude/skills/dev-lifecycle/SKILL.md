---
name: dev-lifecycle
description: Orchestrate the full development lifecycle from issue understanding through merge-ready. Semi-automated with checkpoints at visibility boundaries. Re-entrant — detects current phase from repo/PR state. Supports parent issues with child tracking.
argument-hint: "[issue-number|ad-hoc-slug] [--set N,N,N | --epic N] [--approach=inline|subagent|dynamic] [--mode=in-the-loop|on-the-loop|automated]"
allowed-tools:
  - Bash(gh issue *)
  - Bash(gh pr *)
  - Bash(gh api *)
  - Bash(gh run *)
  - Bash(git branch *)
  - Bash(git log *)
  - Bash(git status *)
  - Bash(date *)
  - Bash(sleep *)
  - Bash(printf *)
  - Bash(tr *)
  - Bash(jq *)
  - Bash(sort *)
  - Bash(grep *)
  - Bash(mkdir *)
  - Bash(cut *)
  - Bash(tail *)
  - Bash(head *)
  - Bash(wc *)
  - Bash(sed *)
  - Bash(awk *)
  - Bash(cat *)
  - Bash(ls *)
  - Bash(rm -f *)
  - Read
  - Grep
  - Glob
---

GitHub Copilot uses its read-only PR and CI reviewer under `.github/copilot-instructions.md`. This authoring command is for an eligible project host.

From a host-provisioned trusted checkout or the trusted default branch, run `python3 -I -B .concord-tooling/bootstrap.py --public-only --skill dev-lifecycle`. Never execute a PR head's scripts or pins. Read the returned SKILL.md in full and pass the original arguments to it. Use that edition for all nested skills, references and role selection. Apply the target repository's trusted configuration and the edition's approval and issue-linking rules. Verify the actual host profile and tools at each required dispatch. Hold a step if its source or required permission is unavailable.
