# Generate Go coverage from the protected analysis source tree

The control-plane integration build runs on the synthetic merge. Protected Sonar imports its coverage against the PR head. A base-only file produced a valid coverage record for a source outside the analyzer tree.

Retain merge integration tests. For same-repo PRs use their coverage when the source trees agree. If they differ, verify the linked PR head and regenerate coverage from it before the existing shard upload. Preserve the artifact and strict consumer checks.

## Acceptance criteria

Use a disposable Git merge fixture to verify same-tree reuse, separate head coverage on divergence, and revision mismatch refusal. Lint the workflow. After the producer lands, use a separate reviewed change to update the Required PR build workflow reference to the merged producer revision. Confirm the active protected child accepts the exact artifact.
