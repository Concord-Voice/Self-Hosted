package users_test

// A busy backup-code check on account erasure judged nothing: both the
// email-confirmation prepare and the erasure itself give the shared MFA charge
// back after the transaction ends, and the account stays.

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/users"
)

func TestErasure_BusyBackupCheckRefundsTheCharge(t *testing.T) {
	for _, route := range []struct {
		name       string
		unverified bool
		call       func(context.Context, *users.AccountService, users.AccountErasureRequest) error
	}{
		{name: "delete account", call: func(ctx context.Context, s *users.AccountService, r users.AccountErasureRequest) error {
			return s.DeleteAccount(ctx, r)
		}},
		{name: "prepare email confirmation", unverified: true, call: func(ctx context.Context, s *users.AccountService, r users.AccountErasureRequest) error {
			_, err := s.PrepareEmailErasureConfirmation(ctx, r)
			return err
		}},
	} {
		t.Run(route.name, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			var user testhelpers.TestUser
			if route.unverified {
				user = ts.CreateTestUserUnverified(t, "erase-busy-prepare")
			} else {
				user = ts.CreateTestUser(t, "erase-busy-delete")
			}
			kpEnrollBackupCodeTOTP(t, ts.DB, user.ID)
			service := erasureService(ts)
			service.SetMFAVerifier(krMFAHandler(t, ts.DB, ts.Redis))
			shared := stepup.DangerousActionBudget(ts.Redis).Key(user.ID)

			release := holdBackupHashSlots(t)
			// One more than the budget: without the refund the last charge in
			// erasureRequest is refused with 429.
			for range stepup.BudgetLimit + 1 {
				ctx, request := erasureRequest(t, ts, service, user, user.Password, busyBackupCode)
				stepErr := requireStepUpStatus(t, route.call(ctx, service, request), http.StatusServiceUnavailable)
				require.True(t, stepup.IsBusy(stepErr), "%v", stepErr.Body)
				require.Equal(t, 0, redisCount(t, ts.Redis, shared), "a busy check must not cost a shared attempt")
				requireUserExists(t, ts, user.ID, true)
			}
			release()

			ctx, request := erasureRequest(t, ts, service, user, user.Password, "00000000")
			_ = requireStepUpStatus(t, route.call(ctx, service, request), http.StatusForbidden)
			require.Equal(t, 1, redisCount(t, ts.Redis, shared), "a wrong code costs exactly one shared attempt")
			requireUserExists(t, ts, user.ID, true)
		})
	}
}
