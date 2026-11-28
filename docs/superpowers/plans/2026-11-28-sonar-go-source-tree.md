# Match protected Go coverage to the source it describes

1. Keep the synthetic-merge coverage producer and its fixtures.
2. Verify the signed merge and its ordered parents for v2 Go reports. A
   present verified caller event SHA must name that merge.
3. Keep coverage for regular Go files with identical source bytes. The
   executable bit alone does not change those bytes. Omit a differing record
   only when the verified event names the merge and exact Git entries prove
   the change came from the base side. Reject unknown files or other mismatches.
4. Run the focused Python and Go tests, independent review, and required CI.
   Seek merge approval for the reviewed head.
5. After approval and merge, verify a fresh protected Sonar run with actual
   base-side drift.
