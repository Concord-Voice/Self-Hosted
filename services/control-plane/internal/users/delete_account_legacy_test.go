package users

import (
	"context"
	"fmt"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/loginfamily"
	"github.com/google/uuid"
)

// DeleteAccountForTest preserves the data-cleanup suites' pre-step-up setup.
// It is compiled only into the users test binary; production has no proofless
// account-erasure entry point. It runs the production DeleteAccount with only
// the fresh-proof check bypassed, so the cleanup suites exercise the same
// actor check, candidate read and post-commit obligations production does.
func (s *AccountService) DeleteAccountForTest(ctx context.Context, userID string) error {
	creatorID, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("delete account test helper: invalid user id: %w", err)
	}
	return s.DeleteAccount(ctx, AccountErasureRequest{Actor: loginfamilyIdentityForTest(ctx, creatorID), testBypassProof: true})
}

// loginfamilyIdentityForTest returns the context's identity when it is the
// account being erased, and otherwise an identity DeleteAccount refuses.
func loginfamilyIdentityForTest(ctx context.Context, userID uuid.UUID) loginfamily.AccessIdentity {
	identity, ok := loginfamily.AccessIdentityFromContext(ctx)
	if ok && identity.UserID == userID {
		return identity
	}
	return loginfamily.AccessIdentity{UserID: userID}
}
