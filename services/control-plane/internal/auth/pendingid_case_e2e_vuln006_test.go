package auth_test

// End-to-end regression: change-email with an upper-case pending id must
// invalidate the code mailed to the previous address.
// Needs PostgreSQL (DATABASE_URL) and Redis (REDIS_URL).

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpperCasePendingIDChangeEmailDoesNotLeaveOldCodeValid(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var reg struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w, &reg)
	oldCode := testhelpers.FetchVerificationCode(t, ts, reg.PendingID)

	victim := "victim-vuln006@example.com"
	w2 := ts.DoRequest("POST", "/api/v1/auth/register/change-email",
		map[string]string{"pending_id": strings.ToUpper(reg.PendingID), "new_email": victim}, nil)
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())

	w3 := ts.DoRequest("POST", "/api/v1/auth/register/confirm",
		map[string]string{"pending_id": reg.PendingID, "code": oldCode}, nil)
	assert.NotEqual(t, http.StatusOK, w3.Code, "old code accepted after change-email: %s", w3.Body.String())

	var users int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE email = $1`, victim).Scan(&users))
	assert.Zero(t, users)
}
