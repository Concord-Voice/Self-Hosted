# Return the existing stale erasure confirmation response implementation plan

1. Confirm the affected source contract on the current default branch.
2. Use the existing preparation error mapper inside the verified-account guard. Keep the guard and fresh proof requirements in place. Record the user-facing behavior in the prerelease changelog.
3. Verify that the response is 409 email_confirmation_stale and the erasure action was never called. Review the guard defensively before publication.
4. Run the applicable independent reviews and required CI. Request merge approval for the reviewed head.
