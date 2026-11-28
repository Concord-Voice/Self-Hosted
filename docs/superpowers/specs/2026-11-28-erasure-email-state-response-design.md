# Return the existing stale erasure confirmation response

If email becomes verified before a pending erasure confirmation is submitted, the server rejects it with a generic 400. Desktop can clear that confirmation when it recognizes the existing stale response.

Use the existing preparation error mapper inside the verified-account guard. Keep the guard and fresh proof requirements in place. Record the user-facing behavior in the prerelease changelog.

## Acceptance criteria

Verify that the response is 409 email_confirmation_stale and the erasure action was never called. Review the guard defensively before publication.
