package users

import (
	"context"
	"errors"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/google/uuid"
)

// DeleteAccountForTest preserves the data-cleanup suites' pre-step-up setup.
// It is compiled only into the users test binary; production has no proofless
// account-erasure entry point. Fresh-proof coverage exercises DeleteAccount.
func (s *AccountService) DeleteAccountForTest(ctx context.Context, userID string) error {
	creatorID, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("delete account test helper: invalid user id: %w", err)
	}
	candidates, overflow, err := readErasurePrivateCallCandidates(ctx, s.db, creatorID)
	if err != nil {
		return fmt.Errorf("delete account test helper: read private-call candidates: %w", err)
	}
	if overflow {
		return fmt.Errorf("private-call candidate fan-out exceeds %d subjects", maxErasurePrivateCallSubjects)
	}
	if len(candidates) > 0 && (s.activityCleanup == nil || s.activePlans == nil) {
		return errors.New("delete account test helper: private-call erasure wiring unavailable")
	}
	survivorSubjects := append([]uuid.UUID(nil), candidates...)
	request := AccountErasureRequest{Actor: loginfamilyIdentityForTest(ctx, creatorID), testBypassProof: true}
	committed, operationErr := s.deleteAccountWithActivityCleanup(ctx, request, creatorID, candidates, &survivorSubjects)
	if committed != nil {
		s.closeErasedFamilies(ctx, committed.familyOwners)
		s.runPostCommitObligations(ctx, userID, committed.channelIDs,
			committed.serverIDs, committed.drained, committed.stranded, committed.commitUnresolved)
		if s.seatsErased != nil && len(committed.seats) > 0 && !committed.commitUnresolved {
			s.seatsErased(ctx, committed.seats)
		}
	}
	return operationErr
}

func loginfamilyIdentityForTest(ctx context.Context, userID uuid.UUID) loginfamily.AccessIdentity {
	identity, ok := loginfamily.AccessIdentityFromContext(ctx)
	if ok && identity.UserID == userID {
		return identity
	}
	return loginfamily.AccessIdentity{UserID: userID}
}
