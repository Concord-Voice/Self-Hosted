package mfa

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestWebAuthnCloneWarningRejectsInlineProof(t *testing.T) {
	db := iuNewTestDB(t)
	h, clean, auth, userID := sfInlineFixture(t, db)
	_, err := db.Exec(`UPDATE user_mfa_webauthn SET sign_count = 2 WHERE user_id = $1`, userID)
	require.NoError(t, err)

	status, body := sfInlineVerify(t, h, clean, auth, userID)
	require.Equal(t, http.StatusForbidden, status, "%v", body)
	require.Empty(t, body["mfa_token"])
	var count int
	require.NoError(t, db.QueryRow(`SELECT sign_count FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&count))
	require.Equal(t, 2, count, "a cloned assertion must not lower the stored counter")
}

func TestWebAuthnCloneWarningRejectsLoginProof(t *testing.T) {
	db := iuNewTestDB(t)
	h, clean, auth, userID := sfInlineFixture(t, db)
	_, err := db.Exec(`UPDATE user_mfa_webauthn SET sign_count = 2 WHERE user_id = $1`, userID)
	require.NoError(t, err)

	status, body := sfLoginWebAuthnVerify(t, h, clean, auth, userID)
	require.Equal(t, http.StatusForbidden, status, "%v", body)
	require.NotEqual(t, true, body["verified"])
	var count int
	require.NoError(t, db.QueryRow(`SELECT sign_count FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&count))
	require.Equal(t, 2, count, "a cloned assertion must not lower the stored counter")
}

func TestWebAuthnZeroCounterStillVerifies(t *testing.T) {
	db := iuNewTestDB(t)
	h, clean, auth, userID := sfInlineFixture(t, db)
	auth.count = ^uint32(0) // The next signed assertion uses the authenticator's unsupported-counter value, zero.
	status, body := sfInlineVerify(t, h, clean, auth, userID)
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.NotEmpty(t, body["mfa_token"])
}

func TestWebAuthnZeroCounterDoesNotLowerExistingCounter(t *testing.T) {
	db := iuNewTestDB(t)
	h, clean, auth, userID := sfInlineFixture(t, db)
	_, err := db.Exec(`UPDATE user_mfa_webauthn SET sign_count = 7 WHERE user_id = $1`, userID)
	require.NoError(t, err)
	auth.count = ^uint32(0) // The next signed assertion presents zero.

	status, body := sfInlineVerify(t, h, clean, auth, userID)
	require.Equal(t, http.StatusForbidden, status, "%v", body)
	require.Empty(t, body["mfa_token"])
	var count int
	require.NoError(t, db.QueryRow(`SELECT sign_count FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&count))
	require.Equal(t, 7, count, "a zero-counter assertion must not lower an established counter")
}

func TestWebAuthnConcurrentStaleLoginCeremoniesMintOnlyOneSession(t *testing.T) {
	db := iuNewTestDB(t)
	h, clean, auth, userID := sfInlineFixture(t, db)
	_, err := db.Exec(`UPDATE user_mfa_webauthn SET sign_count = 7 WHERE user_id = $1`, userID)
	require.NoError(t, err)

	type attempt struct {
		body string
		out  chan webAuthnConcurrentLoginResult
	}
	newAttempt := func(clone sfAuthenticator) attempt {
		t.Helper()
		token, jti, tokenErr := GenerateChallengeToken(userID, PurposeSuspiciousRefresh, JWTSecret("test"), "")
		require.NoError(t, tokenErr)
		_, beginErr := h.BeginWebAuthnLogin(context.Background(), userID, jti)
		require.NoError(t, beginErr)
		raw, getErr := clean.Get(context.Background(), "mfa_webauthn_session:"+jti).Bytes()
		require.NoError(t, getErr)
		var session webauthn.SessionData
		require.NoError(t, json.Unmarshal(raw, &session))
		return attempt{
			body: fmt.Sprintf(`{"mfa_challenge_token":%q,"method":"webauthn","assertion":%s}`, token, clone.assertion(t, session.Challenge)),
			out:  make(chan webAuthnConcurrentLoginResult, 1),
		}
	}

	// Two authenticators cloned from the same key each independently sign the
	// next counter value for a different, server-issued challenge.
	firstClone, secondClone := *auth, *auth
	firstClone.count, secondClone.count = 7, 7
	first, second := newAttempt(firstClone), newAttempt(secondClone)

	// Hold the credential row until both handlers have read seven, verified an
	// assertion carrying eight, and blocked at their counter update.
	holder, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback() }()
	var count int
	require.NoError(t, holder.QueryRow(
		`SELECT sign_count FROM user_mfa_webauthn WHERE user_id = $1 FOR UPDATE`, userID,
	).Scan(&count))
	require.Equal(t, 7, count)

	run := func(a attempt) {
		c, w := iuGinContext(http.MethodPost, "/api/v1/auth/mfa/verify", a.body, "", "")
		h.Verify(c)
		a.out <- webAuthnConcurrentLoginResult{code: w.Code, body: w.Body.String()}
	}
	go run(first)
	go run(second)
	require.Eventually(t, func() bool {
		var waiting int
		waitErr := db.QueryRow(`
			SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock' AND query ILIKE '%user_mfa_webauthn%'
		`).Scan(&waiting)
		return waitErr == nil && waiting >= 2
	}, 10*time.Second, 10*time.Millisecond, "both login ceremonies did not reach the counter-update barrier")
	require.NoError(t, holder.Commit())

	firstResult := waitWebAuthnConcurrentLoginResult(t, first.out)
	secondResult := waitWebAuthnConcurrentLoginResult(t, second.out)
	require.ElementsMatch(t, []int{http.StatusOK, http.StatusForbidden}, []int{firstResult.code, secondResult.code}, "%s / %s", firstResult.body, secondResult.body)
	require.NoError(t, db.QueryRow(`SELECT sign_count FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&count))
	require.Equal(t, 8, count, "the stale duplicate must not advance the credential counter")
}

type webAuthnConcurrentLoginResult struct {
	code int
	body string
}

func waitWebAuthnConcurrentLoginResult(t *testing.T, results <-chan webAuthnConcurrentLoginResult) webAuthnConcurrentLoginResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("login ceremony did not finish after the counter barrier released")
		return webAuthnConcurrentLoginResult{}
	}
}

func finishWebAuthnRegistration(t *testing.T, h *Handler, rdb *redis.Client, userID string) *httptest.ResponseRecorder {
	t.Helper()
	storeWebAuthnRegistrationSession(t, rdb, userID)
	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/webauthn/register/finish", webAuthnRegistrationResponse, userID, "")
	h.WebAuthnRegisterFinish(c)
	return w
}

func storeWebAuthnRegistrationSession(t *testing.T, rdb *redis.Client, userID string) {
	t.Helper()
	sessionJSON, err := json.Marshal(webauthn.SessionData{
		Challenge: webAuthnChallenge, RelyingPartyID: "webauthn.io",
		UserID: []byte(userID), CredParams: webauthn.CredentialParametersDefault(),
	})
	require.NoError(t, err)
	require.NoError(t, rdb.Set(context.Background(), fmt.Sprintf(redisKeyWebAuthnReg, userID),
		`{"session":`+strconv.Quote(string(sessionJSON))+`,"credential_name":"new-key","credential_type":"hardware"}`, time.Minute).Err())
}

func seedWebAuthnCredentials(t *testing.T, db *sql.DB, userID string, n int) {
	t.Helper()
	sfNewAuthenticator(t, db, userID)
	for i := 1; i < n; i++ {
		_, err := db.Exec(`
			INSERT INTO user_mfa_webauthn (user_id, credential_id, public_key, sign_count, credential_name, credential_type)
			SELECT user_id, $2, public_key, 0, $3, 'hardware' FROM user_mfa_webauthn WHERE user_id = $1 LIMIT 1
		`, userID, []byte(fmt.Sprintf("other-%d", i)), fmt.Sprintf("other-%d", i))
		require.NoError(t, err)
	}
}

func TestWebAuthnRegisterFinishLimitAtTen(t *testing.T) {
	db := iuNewTestDB(t)
	userID := iuCreateUser(t, db, iuPassword)
	seedWebAuthnCredentials(t, db, userID, 10)
	rdb := iuNewTestRedis(t)
	h := sfWebAuthnHandler(t, db, rdb, iuKeyring(t))
	w := finishWebAuthnRegistration(t, h, rdb, userID)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&count))
	require.Equal(t, 10, count)
}

func TestWebAuthnRegisterFinishCountFailureWritesNothing(t *testing.T) {
	db, hits := sfFaultDB(t, "SELECT COUNT(*) FROM user_mfa_webauthn")
	userID := iuCreateUser(t, db, iuPassword)
	rdb := iuNewTestRedis(t)
	h := sfWebAuthnHandler(t, db, rdb, iuKeyring(t))
	w := finishWebAuthnRegistration(t, h, rdb, userID)
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	sfRequireFired(t, hits)
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(id) FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&count))
	require.Zero(t, count)
}

type registrationConsumeSignal struct{ consumed chan<- struct{} }

func (registrationConsumeSignal) DialHook(next redis.DialHook) redis.DialHook { return next }
func (registrationConsumeSignal) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h registrationConsumeSignal) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if cmd.Name() == "getdel" {
			h.consumed <- struct{}{}
		}
		return err
	}
}

func TestWebAuthnRegisterFinishConcurrentLimit(t *testing.T) {
	db := iuNewTestDB(t)
	userID := iuCreateUser(t, db, iuPassword)
	seedWebAuthnCredentials(t, db, userID, 9)
	rdb := iuNewTestRedis(t)
	consumed := make(chan struct{}, 2)
	rdb.AddHook(registrationConsumeSignal{consumed: consumed})
	h := sfWebAuthnHandler(t, db, rdb, iuKeyring(t))

	blocker, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback() }()
	var lockedID string
	require.NoError(t, blocker.QueryRow(`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID).Scan(&lockedID))

	start := func() (<-chan struct{}, *httptest.ResponseRecorder) {
		c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/webauthn/register/finish", webAuthnRegistrationResponse, userID, "")
		done := make(chan struct{})
		go func() { h.WebAuthnRegisterFinish(c); close(done) }()
		return done, w
	}
	wait := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(15 * time.Second):
			t.Fatal("registration finish did not reach its barrier")
		}
	}
	storeWebAuthnRegistrationSession(t, rdb, userID)
	firstDone, first := start()
	wait(consumed)
	storeWebAuthnRegistrationSession(t, rdb, userID)
	secondDone, second := start()
	wait(consumed)
	require.NoError(t, blocker.Commit())
	wait(firstDone)
	wait(secondDone)
	require.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, []int{first.Code, second.Code}, "%s / %s", first.Body.String(), second.Body.String())
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&count))
	require.Equal(t, 10, count)
}
