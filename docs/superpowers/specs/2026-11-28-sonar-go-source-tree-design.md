# Match protected Go coverage to the source it describes

The integration build tests the synthetic merge, while protected Sonar scans
the PR head. New files on the base branch can appear in Go coverage even when
they are absent from the PR head. Running the tests again on the head in the
same job would reuse a database template migrated from the merge checkout.

Keep the existing build and fixtures. Tie the v2 coverage provenance to the
verified caller event and exact signed merge, then compare each covered path
with the protected PR head. Keep identical regular files. Omit only a net
base-side difference established by Git. Reject unknown files and differing
PR-side changes. Evidence without a verified event SHA keeps the existing
PR-tree importer and cannot omit coverage entries.

## Acceptance criteria

Use local Git fixtures for identical coverage, base-only additions, same-path
base edits, PR-side differences, unknown paths, and stale revision evidence.
Test the verified event claim. The existing protected workflow loads the
importer from main. Use a fresh protected run after merge to verify the live
claim and a base-only mismatch.
