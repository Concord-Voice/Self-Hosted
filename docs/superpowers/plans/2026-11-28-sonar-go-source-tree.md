# Match protected Go coverage to the source it describes

1. Keep the synthetic-merge coverage producer and its fixtures.
2. Bind importer projection to the verified caller event SHA, signed merge,
   and ordered parents.
3. Keep only records for identical regular Git entries in the analysis tree.
   Omit records only for proven base-side drift. Fail closed on unknown or
   PR-side mismatches.
4. Run the focused Python and Go tests, independent review, and required CI.
   Seek merge approval for the reviewed head.
5. After approval and merge, verify a fresh protected Sonar run with actual
   base-side drift.
