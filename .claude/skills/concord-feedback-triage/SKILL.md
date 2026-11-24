---
name: concord-feedback-triage
description: Run Concord feedback triage under its pinned owner policy.
---

Run `python3 .concord-tooling/bootstrap.py --skill feedback-triage` from a trusted default-branch or host-provisioned tooling checkout. Never execute a PR head's scripts or pins. Read the returned SKILL.md in full and follow it with its own adjacent references, dispatch requirements and approval gates. If resolution fails, report that capability as unavailable and stop this workflow.
