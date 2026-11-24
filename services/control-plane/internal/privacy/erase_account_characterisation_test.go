package privacy_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/privacy"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const deliveryFailedBody = `{"error":"Your account was deleted. Updating everyone who could see you is taking longer than usual."}`

// TestEraseAccount_DeleteErrorMapping pins every DeleteAccount error arm's
// status AND body, including the precedence between sentinels when one error
// wraps several. Each case runs with and without a logger so the nil-logger
// guards are exercised on every logging arm.
func TestEraseAccount_DeleteErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
	}{
		{"not found", users.ErrUserNotFound, http.StatusNotFound, `{"error":"account not found"}`},
		{"epoch mismatch", fmt.Errorf("wrap: %w", credepoch.ErrEpochMismatch), http.StatusUnauthorized, `{"error":"authentication required"}`},
		{"candidate drift", users.ErrErasureCandidateSetDrifted, http.StatusConflict, `{"error":"account state changed; retry the deletion"}`},
		{"login family unavailable", fmt.Errorf("wrap: %w", loginfamily.ErrUnavailable), http.StatusServiceUnavailable, `{"error":"Authentication temporarily unavailable"}`},
		{"unknown", errors.New("db down"), http.StatusInternalServerError, `{"error":"account deletion failed"}`},
		{"post-commit delivery", presencecapture.ErrPostCommitDelivery, http.StatusServiceUnavailable, deliveryFailedBody},
		// Precedence: the first matching arm in source order wins.
		{"not found beats epoch", errors.Join(credepoch.ErrEpochMismatch, users.ErrUserNotFound), http.StatusNotFound, `{"error":"account not found"}`},
		{"not found beats delivery", errors.Join(presencecapture.ErrPostCommitDelivery, users.ErrUserNotFound), http.StatusNotFound, `{"error":"account not found"}`},
		{"epoch beats drift", errors.Join(users.ErrErasureCandidateSetDrifted, credepoch.ErrEpochMismatch), http.StatusUnauthorized, `{"error":"authentication required"}`},
		{"drift beats unavailable", errors.Join(loginfamily.ErrUnavailable, users.ErrErasureCandidateSetDrifted), http.StatusConflict, `{"error":"account state changed; retry the deletion"}`},
		{"unavailable beats delivery", errors.Join(presencecapture.ErrPostCommitDelivery, loginfamily.ErrUnavailable), http.StatusServiceUnavailable, `{"error":"Authentication temporarily unavailable"}`},
	}
	for _, tc := range cases {
		for _, withLog := range []bool{true, false} {
			name := tc.name + "/nil-logger"
			var log *logger.Logger
			if withLog {
				name = tc.name + "/logger"
				log = logger.New("test")
			}
			t.Run(name, func(t *testing.T) {
				stub := &stubAccountDeleter{err: tc.err}
				h := privacy.NewHandler(stub, nil, log)
				c, w := newTestContext(t, `{}`, "char-user")

				h.EraseAccount(c)

				require.True(t, stub.called)
				assert.Equal(t, "char-user", stub.calledWithUser)
				assert.Equal(t, tc.wantStatus, c.Writer.Status())
				assert.JSONEq(t, tc.wantBody, w.Body.String())
			})
		}
	}
}

func TestEraseAccount_PreDeleteRefusalsBodies(t *testing.T) {
	t.Run("missing user id", func(t *testing.T) {
		stub := &stubAccountDeleter{}
		h := privacy.NewHandler(stub, nil, nil)
		c, w := newTestContext(t, `{}`, "")
		h.EraseAccount(c)
		assert.Equal(t, http.StatusUnauthorized, c.Writer.Status())
		assert.JSONEq(t, `{"error":"authentication required"}`, w.Body.String())
		assert.False(t, stub.called)
	})
	t.Run("invalid body", func(t *testing.T) {
		stub := &stubAccountDeleter{}
		h := privacy.NewHandler(stub, nil, nil)
		c, w := newTestContext(t, `{bad`, "char-user")
		h.EraseAccount(c)
		assert.Equal(t, http.StatusBadRequest, c.Writer.Status())
		assert.JSONEq(t, `{"error":"invalid body"}`, w.Body.String())
		assert.False(t, stub.called)
	})
	t.Run("success has empty body", func(t *testing.T) {
		stub := &stubAccountDeleter{}
		h := privacy.NewHandler(stub, nil, nil)
		c, w := newTestContext(t, `{}`, "char-user")
		h.EraseAccount(c)
		assert.Equal(t, http.StatusNoContent, c.Writer.Status())
		assert.Empty(t, w.Body.String())
	})
}

// A revocation failure wins over a durable delivery failure: the handler must
// answer 500 (revocation) rather than the 503 delivery message. Runs with and
// without a logger.
func TestEraseAccount_RevocationFailureAfterDeliveryFailure(t *testing.T) {
	for _, withLog := range []bool{true, false} {
		t.Run(fmt.Sprintf("logger=%v", withLog), func(t *testing.T) {
			rdb := redistest.Client(t)
			var log *logger.Logger
			if withLog {
				log = logger.New("test")
			}
			stub := &stubAccountDeleter{err: presencecapture.ErrPostCommitDelivery}
			h := privacy.NewHandler(stub, rdb, log)
			c, w := newTestContext(t, `{}`, "char-user")
			c.Set(middleware.JWTClaimsContextKey, "not-jwt-map-claims")

			h.EraseAccount(c)

			assert.Equal(t, http.StatusInternalServerError, c.Writer.Status())
			assert.JSONEq(t, `{"error":"access token revocation failed"}`, w.Body.String())
		})
	}
}
