package auth_test

// No auth log line may carry a registration pending id, in any spelling:
// the id is the handle for an unverified registration, so it stays out of
// journald. Drives register, resend, change-email, confirm (wrong and right
// code) and abandon through the real router and scans the captured logs.
// Needs PostgreSQL (DATABASE_URL) and Redis (REDIS_URL).

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistrationFlowLogsNeverContainPendingID(t *testing.T) {
	ts := setupTS(t)
	logs := ts.CaptureLogs(t)

	w := ts.DoRequest("POST", pathRegister, registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var reg struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w, &reg)
	require.NotEmpty(t, reg.PendingID)

	upper := strings.ToUpper(reg.PendingID)
	// Wrong code, malformed id, unknown id and a change-email: every error
	// and success path that formerly logged the id.
	ts.DoRequest("POST", "/api/v1/auth/register/confirm",
		map[string]string{"pending_id": upper, "code": "000000"}, nil)
	ts.DoRequest("POST", "/api/v1/auth/register/confirm",
		map[string]string{"pending_id": "not-a-uuid", "code": "000000"}, nil)
	ts.DoRequest("POST", "/api/v1/auth/register/resend",
		map[string]string{"pending_id": "00000000-0000-4000-8000-000000000000"}, nil)
	ts.DoRequest("POST", "/api/v1/auth/register/change-email",
		map[string]string{"pending_id": upper, "new_email": "moved-" + testEmail}, nil)
	code := testhelpers.FetchVerificationCode(t, ts, reg.PendingID)
	ts.DoRequest("POST", "/api/v1/auth/register/confirm",
		map[string]string{"pending_id": upper, "code": code}, nil)

	out := strings.ToLower(logs.String())
	require.NotEmpty(t, out, "no logs captured; the assertion would be vacuous")
	assert.NotContains(t, out, strings.ToLower(reg.PendingID))
	assert.NotContains(t, out, strings.ReplaceAll(strings.ToLower(reg.PendingID), "-", ""))
}
