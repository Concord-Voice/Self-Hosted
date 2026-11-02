---
description: Inventory of plugin-cache customizations and the re-apply procedure after plugin upgrades
paths:
  - ".claude/**"
  - "scripts/**"
---

# Plugin-Cache Customization

Some workflows depend on extensions to upstream plugin skills that live in `~/.claude/plugins/cache/claude-plugins-official/`. The plugin system overwrites the cache on upgrade — **modifications do not survive a plugin upgrade.**

This file is the canonical inventory of those modifications. Add an entry whenever you patch a plugin-cache file. The smoke tests listed below alert when modifications are missing (e.g., after a plugin upgrade).

## Active customizations

Each entry records: target cache path + version · Modification · Purpose · Issue/Spec · Smoke test · manual-vs-auto note.

This file ships with **no concrete entries** — it is the template. Add one per plugin-cache modification your repo makes.

> **Scope rule.** A customization belongs in the repo whose specifics it encodes, not in a shared copy of this file. If the patch only makes sense because it knows a particular repo's hook patterns, protected paths, or scripts, it stays with that repo — a repo that does not wire those hooks would be re-applying a modification it cannot use. Only genuinely repo-independent patches belong in a copy that several repos share.

## Re-applying after a plugin upgrade

1. Run any smoke tests listed in the Active customizations entries for your repo — if a test fails/WARNs and the relevant plugin IS installed, the corresponding modification was wiped.
2. Re-apply by reading this file's "Active customizations" entries and re-executing the documented Edit steps for each affected file.
3. Update the version number in this file's headings if the plugin major/minor changed (note: some plugins use an `unknown/` directory rather than a version directory — track that separately if it changes).
4. Re-run the smoke tests to confirm.

## When to upstream

If a customization stabilizes and we determine it's broadly useful (not just repo-specific), file an issue/PR upstream in the superpowers plugin repository. Customizations coupled to repo-specific scripts (hooks, patterns) stay local — but a generic interface extracted from the pattern could be upstreamed. Hooks coupled to repo-specific scripts stay in the local overlay; only the general principle travels.

## Threat model

Plugin-cache modifications are trust-equal to any other file in `.claude/` — they're owned by the developer and reviewed in the same way. The smoke test catches accidental wipes (plugin upgrade); it does NOT catch malicious tampering (a compromised plugin upgrade could re-introduce the prompt section but with malicious content). For higher assurance, file a SHA hash of the customized section in this document and verify post-upgrade — but that's beyond current scope.
