package auth_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

func registerRaw(t *testing.T, body []byte, ts *testhelpers.TestServer) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, pathRegister, bytes.NewReader(body))
	req.Header.Set(headerContentType, contentTypeJSON)
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)
	return w
}

func TestRegisterBodyBoundAndOneJSONValue(t *testing.T) {
	ts := setupTS(t)
	validBody, err := json.Marshal(registerPayload())
	require.NoError(t, err)
	payload := registerPayload()
	payload["username"] = "x" // ordinary validation fails after the bounded read
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	require.Less(t, len(body), 64*1024)

	for _, tc := range []struct {
		name string
		body []byte
		want int
	}{
		{"exact limit", append(bytes.Clone(body), bytes.Repeat([]byte(" "), 64*1024-len(body))...), http.StatusBadRequest},
		{"over limit", append(bytes.Clone(body), bytes.Repeat([]byte(" "), 64*1024+1-len(body))...), http.StatusRequestEntityTooLarge},
		{"trailing value", append(bytes.Clone(validBody), []byte(` {}`)...), http.StatusBadRequest},
		{"malformed", []byte(`{"email":`), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := registerRaw(t, tc.body, ts)
			require.Equal(t, tc.want, w.Code, w.Body.String())
		})
	}
	var pending int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM pending_registrations WHERE email = $1`, testEmail).Scan(&pending))
	require.Zero(t, pending)
}

func TestRegisterRejectsOversizedDecodedKeyFields(t *testing.T) {
	ts := setupTS(t)
	for _, tc := range []struct {
		name, field string
		bytes       int
	}{
		{"public key", "public_key", 1025},
		{"wrapped key", "wrapped_private_key", 32*1024 + 1},
		{"salt", "key_derivation_salt", 65},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := registerPayload()
			payload["email"] = strings.ReplaceAll(tc.name, " ", "") + "@bounds.concord.test"
			payload[tc.field] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, tc.bytes))
			w := ts.DoRequest(http.MethodPost, pathRegister, payload, nil)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			var pending int
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM pending_registrations WHERE email = $1`, payload["email"]).Scan(&pending))
			require.Zero(t, pending)
		})
	}
}

func TestRegisterAcceptsRepresentativeDesktopKeySizes(t *testing.T) {
	ts := setupTS(t)
	payload := registerPayload()
	payload["email"] = "desktopkey@bounds.concord.test"
	payload["username"] = "desktopkeybounds"
	payload["wrapped_private_key"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 3500))
	payload["key_derivation_salt"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x24}, 16))
	w := ts.DoRequest(http.MethodPost, pathRegister, payload, nil)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

func TestRegisterConfirmedUserConflictLeavesNoPending(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "boundexisting")
	payload := registerPayload()
	payload["email"] = user.Email
	payload["username"] = "boundnew"
	w := ts.DoRequest(http.MethodPost, pathRegister, payload, nil)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var pending int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM pending_registrations WHERE LOWER(email) = LOWER($1)`, user.Email).Scan(&pending))
	require.Zero(t, pending)
}
