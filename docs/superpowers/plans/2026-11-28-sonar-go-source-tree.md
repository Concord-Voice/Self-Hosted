# Generate Go coverage from the protected analysis source tree implementation plan

1. Confirm the affected source contract on the current default branch.
2. Retain merge integration tests. For same-repo PRs use their coverage when the source trees agree. If they differ, verify the linked PR head and regenerate coverage from it before the existing shard upload. Preserve the artifact and strict consumer checks.
3. Use a disposable Git merge fixture to verify same-tree reuse, separate head coverage on divergence, and revision mismatch refusal. Lint the workflow. After the producer lands, use a separate reviewed change to update the Required PR build workflow reference to the merged producer revision. Confirm the active protected child accepts the exact artifact.
4. Run the applicable independent reviews and required CI. Request merge approval for the reviewed head.
