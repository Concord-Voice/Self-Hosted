package mfa

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// VULN-028: the two MFA method-list handlers take a small closed set. An
// overlong or unknown list is refused before step-up, Redis or any per-entry
// loop; a short list with repeats is reduced to its first-seen unique values.

func repeatedMethods(method string, n int) string {
	quoted := make([]string, n)
	for i := range quoted {
		quoted[i] = fmt.Sprintf("%q", method)
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

func TestNormalizeMethodSet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     []string
		allowed []string
		maxRaw  int
		want    []string
		ok      bool
	}{
		{name: "nil clears", raw: nil, allowed: recoveryOnlyMethodSet, maxRaw: maxRecoveryOnlyMethods, want: []string{}, ok: true},
		{name: "first-seen order", raw: []string{"sms", "totp", "sms"}, allowed: recoveryOnlyMethodSet, maxRaw: maxRecoveryOnlyMethods, want: []string{"sms", "totp"}, ok: true},
		{name: "all four", raw: []string{"totp", "webauthn", "email", "sms"}, allowed: recoveryOnlyMethodSet, maxRaw: maxRecoveryOnlyMethods, want: []string{"totp", "webauthn", "email", "sms"}, ok: true},
		{name: "five raw entries", raw: []string{"totp", "totp", "totp", "totp", "totp"}, allowed: recoveryOnlyMethodSet, maxRaw: maxRecoveryOnlyMethods},
		{name: "unknown", raw: []string{"totp", "carrier_pigeon"}, allowed: recoveryOnlyMethodSet, maxRaw: maxRecoveryOnlyMethods},
		{name: "case is exact", raw: []string{"EMAIL"}, allowed: emailSmsSetupMethodSet, maxRaw: maxEmailSmsSetupMethods},
		{name: "setup duplicate", raw: []string{"email", "email"}, allowed: emailSmsSetupMethodSet, maxRaw: maxEmailSmsSetupMethods, want: []string{"email"}, ok: true},
		{name: "setup three raw entries", raw: []string{"email", "sms", "email"}, allowed: emailSmsSetupMethodSet, maxRaw: maxEmailSmsSetupMethods},
		{name: "setup refuses totp", raw: []string{"totp"}, allowed: emailSmsSetupMethodSet, maxRaw: maxEmailSmsSetupMethods},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeMethodSet(tc.raw, tc.allowed, tc.maxRaw)
			require.Equal(t, tc.ok, ok)
			if tc.ok {
				require.Equal(t, tc.want, got)
			} else {
				require.Nil(t, got)
			}
		})
	}
}

func TestFilterValidRecoveryOnlyStoresEachMethodOnce(t *testing.T) {
	enabled := map[string]bool{"totp": true, "email": true}
	require.Equal(t, []string{"email", "totp"}, filterValidRecoveryOnly([]string{"email", "totp", "email", "totp"}, enabled))
}

func TestSetRecoveryOnlyRefusesAnUnboundedListBeforeStepUp(t *testing.T) {
	for name, methods := range map[string]string{
		"1000 duplicates": repeatedMethods("email", 1000),
		"five entries":    `["totp","webauthn","email","sms","sms"]`,
		"unknown method":  `["carrier_pigeon"]`,
	} {
		t.Run(name, func(t *testing.T) {
			// No scripted answers: any step-up or database read would fail
			// and change the status, so a 400 proves the refusal came first.
			h, redisClient := scriptedHandler(t, &mfaEventDB{passwordHash: correctPasswordHash(t)}, "test")
			c, response := enrollRequest("/api/v1/auth/mfa/recovery-only",
				`{"methods":`+methods+`,"password":"correct"}`, "session-a")

			h.SetRecoveryOnly(c)

			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			keys, err := redisClient.Keys(context.Background(), "*").Result()
			require.NoError(t, err)
			require.Empty(t, keys, "the refusal must precede every Redis write, step-up budget included")
		})
	}
}

func TestSetRecoveryOnlyStoresARepeatedMethodOnce(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{passwordHash: correctPasswordHash(t)}, "test",
		scriptedAnswer{match: "SELECT mfa_enabled FROM users", values: []driver.Value{false}},
		scriptedAnswer{match: "SELECT mfa_methods FROM users", values: []driver.Value{[]byte("{totp,email}")}},
		scriptedAnswer{match: "RETURNING recovery_hardened", values: []driver.Value{true}},
	)
	c, response := enrollRequest("/api/v1/auth/mfa/recovery-only", `{"methods":["email","email"],"password":"correct"}`, "session-a")

	h.SetRecoveryOnly(c)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var body struct {
		RecoveryOnlyMethods []string `json:"recovery_only_methods"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, []string{"email"}, body.RecoveryOnlyMethods)
}

func TestEmailSmsSetupRefusesAnUnboundedListBeforeStepUp(t *testing.T) {
	for name, methods := range map[string]string{
		"1000 duplicates": repeatedMethods("sms", 1000),
		"three entries":   `["email","sms","sms"]`,
		"empty":           `[]`,
		"unknown method":  `["sms","carrier_pigeon"]`,
	} {
		t.Run(name, func(t *testing.T) {
			h, redisClient := scriptedHandler(t, &mfaEventDB{passwordHash: correctPasswordHash(t)}, "test")
			c, response := enrollRequest("/api/v1/auth/mfa/email-sms/setup",
				`{"methods":`+methods+`,"password":"correct"}`, "session-a")

			h.EmailSmsSetup(c)

			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), "methods must be 'email' and/or 'sms'")
			keys, err := redisClient.Keys(context.Background(), "*").Result()
			require.NoError(t, err)
			require.Empty(t, keys, "the refusal must precede every Redis write and code generation")
		})
	}
}

// The availability check still runs on the normalized set, outside a
// development or test environment.
func TestEmailSmsSetupRefusesSMSAfterNormalizing(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{passwordHash: correctPasswordHash(t)}, "production")
	c, response := enrollRequest("/api/v1/auth/mfa/email-sms/setup", `{"methods":["sms","sms"],"password":"correct"}`, "session-a")

	h.EmailSmsSetup(c)

	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	keys, err := redisClient.Keys(context.Background(), "*").Result()
	require.NoError(t, err)
	require.Empty(t, keys)
}

func TestGenerateAndStoreEmailSmsCodesIsBounded(t *testing.T) {
	h, redisClient := scriptedHandler(t, &mfaEventDB{}, "test")
	ctx := context.Background()

	for name, methods := range map[string][]string{
		"1000 duplicates": strings.Split(strings.Repeat("email,", 999)+"email", ","),
		"unknown":         {"push"},
		"empty":           {},
	} {
		t.Run(name, func(t *testing.T) {
			codes, err := h.generateAndStoreEmailSmsCodes(ctx, enrollUser, methods)
			require.Error(t, err)
			require.Nil(t, codes)
			keys, keysErr := redisClient.Keys(ctx, "*").Result()
			require.NoError(t, keysErr)
			require.Empty(t, keys, "no code may be generated or stored for a refused list")
		})
	}

	codes, err := h.generateAndStoreEmailSmsCodes(ctx, enrollUser, []string{"sms", "sms"})
	require.NoError(t, err)
	require.Len(t, codes, 1)
	keys, err := redisClient.Keys(ctx, "*").Result()
	require.NoError(t, err)
	require.Equal(t, []string{fmt.Sprintf(redisKeyEmailSmsSetup, enrollUser, "sms")}, keys)
}
