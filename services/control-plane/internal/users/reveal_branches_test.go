package users_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// hashLeakingVerifier fails the way a corrupt stored hash does: the error
// text carries hash material.
type hashLeakingVerifier struct{}

func (hashLeakingVerifier) VerifyPassword(context.Context, string, string) (string, error) {
	return "", errors.New("decode $argon2id$v=19$m=65536,t=3,p=4$SECRETHASH")
}

func revealDirect(h *users.Handler, userID string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", userID)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/users/me/keys/reveal",
		strings.NewReader(`{"current_password":"whatever"}`)) // pragma: allowlist secret
	c.Request.Header.Set("Content-Type", "application/json")
	h.RevealMyKeys(c)
	return w
}

func TestRevealMyKeys_DBErrorIs500(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	cleanup() // a closed pool fails the password-state read
	h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil, nil, nil)
	h.SetPasswordVerifier(hashLeakingVerifier{})

	assert.Equal(t, http.StatusInternalServerError, revealDirect(h, uuid.NewString()).Code)
}

func TestRevealMyKeys_UnknownUserIs404(t *testing.T) {
	ts := setupTS(t)
	h := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, nil, nil, nil, nil)
	h.SetPasswordVerifier(hashLeakingVerifier{})

	assert.Equal(t, http.StatusNotFound, revealDirect(h, uuid.NewString()).Code)
}

// An unwired verifier refuses rather than falling back to an unlocked check.
func TestRevealMyKeys_UnwiredVerifierIs500(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "keysrevnil")
	h := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, nil, nil, nil, nil)

	w := revealDirect(h, user.ID)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NotContains(t, w.Body.String(), "wrapped_private_key")
}

// A verifier fault is a 500, and the error (which can embed the hash) is
// never logged.
func TestRevealMyKeys_VerifierFaultIsNotLogged(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "keysrevfault")
	var logs bytes.Buffer
	h := users.NewHandler(ts.DB, logger.NewWithWriter(&logs), nil, nil, nil, nil, nil)
	h.SetPasswordVerifier(hashLeakingVerifier{})

	w := revealDirect(h, user.ID)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, logs.String(), "key_reveal_password_verify_failed")
	assert.NotContains(t, logs.String(), "SECRETHASH")
	assert.NotContains(t, w.Body.String(), "SECRETHASH")
}

// A corrupt stored hash fails the password change with 500, and the decode
// error (which carries hash material) never reaches the log.
func TestChangePassword_CorruptHashIsNotLogged(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "chpwcorrupt")
	_, err := ts.DB.Exec(`UPDATE users SET password_hash = '$argon2id$v=19$m=65536,t=3,p=4$SECRETHASH' WHERE id = $1`, user.ID) // pragma: allowlist secret
	require.NoError(t, err)
	logs := ts.CaptureLogs(t)

	_, newWrapped, newSalt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", urlUsersMePassword, map[string]interface{}{
		keyCurrentPassword:   user.Password,
		keyNewPassword:       "NewStrongPassword456!", // pragma: allowlist secret
		keyWrappedPrivateKey: newWrapped,
		keyKeyDerivationSalt: newSalt,
	}, testhelpers.AuthHeaders(user.AccessToken))

	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	assert.Contains(t, logs.String(), "password_verify_failed")
	assert.NotContains(t, logs.String(), "SECRETHASH")
}
