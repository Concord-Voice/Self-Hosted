package mfa

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/require"
)

// changingTOTPStore runs `change` just before the step update, simulating a
// re-enrollment or disable that lands after the code was matched.
type changingTOTPStore struct {
	*sql.DB
	change string
	args   []interface{}
	done   bool
}

func (s *changingTOTPStore) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if !s.done && s.change != "" && strings.Contains(query, "SET last_used_step = $2") {
		s.done = true
		if _, err := s.DB.ExecContext(ctx, s.change, s.args...); err != nil {
			return nil, fmt.Errorf("simulate TOTP change: %w", err)
		}
	}
	return s.DB.ExecContext(ctx, query, args...)
}

func TestAcceptTOTPStepIsFencedOnTheMatchedSecret(t *testing.T) {
	keyring, err := ParseKeyring(strings.Repeat("00", 32), 1, "")
	require.NoError(t, err)
	_, newNonce, _, err := keyring.Seal([]byte("re-enrolled-seed"))
	require.NoError(t, err)

	for _, tc := range []struct {
		name, change string
		args         func(userID string) []interface{}
		want         bool
	}{
		{name: "no change accepts", want: true},
		{name: "re-enrollment refuses", change: `UPDATE user_mfa_totp SET totp_secret_nonce = $1 WHERE user_id = $2`,
			args: func(u string) []interface{} { return []interface{}{newNonce, u} }},
		{name: "disable refuses", change: `UPDATE user_mfa_totp SET enabled = FALSE WHERE user_id = $1`,
			args: func(u string) []interface{} { return []interface{}{u} }},
		{name: "unconfirm refuses", change: `UPDATE user_mfa_totp SET confirmed = FALSE WHERE user_id = $1`,
			args: func(u string) []interface{} { return []interface{}{u} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := dbtest.SetupTestDB(t)
			t.Cleanup(cleanup)
			userID := dbtest.CreateUser(t, db).String()
			secretEnc, secretNonce, keyVersion, err := keyring.Seal([]byte("old-seed"))
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
				VALUES ($1, $2, $3, $4, TRUE, TRUE)`, userID, secretEnc, secretNonce, keyVersion)
			require.NoError(t, err)

			store := &changingTOTPStore{DB: db, change: tc.change}
			if tc.args != nil {
				store.args = tc.args(userID)
			}
			ok, method, err := acceptTOTPStep(context.Background(), store, userID, 42, secretNonce)

			require.NoError(t, err)
			require.Equal(t, tc.want, ok)
			var step sql.NullInt64
			require.NoError(t, db.QueryRow(`SELECT last_used_step FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(&step))
			if tc.want {
				require.Equal(t, "totp", method)
				require.Equal(t, int64(42), step.Int64)
			} else {
				require.Empty(t, method)
				require.False(t, step.Valid, "a refused code must not spend the step")
				require.True(t, store.done, "the simulated change must have run")
			}
		})
	}
}
