package mfa

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type recordingCompletionKind struct {
	loginCalls int
	linkCalls  int
	userID     string
	jti        string
	epoch      string
	method     string
}

func (r *recordingCompletionKind) CompleteLogin(c *gin.Context, _ string, _ bool, _ string, _ securityevent.AuthMethod) bool {
	r.loginCalls++
	c.Status(http.StatusOK)
	return true
}

func (r *recordingCompletionKind) CompleteSSOLink(c *gin.Context, userID, jti, epoch, method string) bool {
	r.linkCalls++
	r.userID, r.jti, r.epoch, r.method = userID, jti, epoch, method
	c.Status(http.StatusOK)
	return true
}

func TestCompletionKindDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	for _, test := range []struct {
		name       string
		kind       CompletionKind
		wantLogin  int
		wantLink   int
		wantStatus int
	}{
		{"legacy", "", 1, 0, http.StatusOK},
		{"login", CompletionKindLogin, 1, 0, http.StatusOK},
		{"sso link", CompletionKindSSOLink, 0, 1, http.StatusOK},
		{"unknown", "unsafe", 0, 0, http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := NewHandler(nil, client, logger.New("test"), nil, "test", nil, "test")
			recording := &recordingCompletionKind{}
			h.SetLoginCompleter(recording)
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", nil)
			claims := &ChallengeClaims{UserID: "user-1", Purpose: PurposeLogin, CompletionKind: test.kind, CredentialEpoch: "epoch-1", RegisteredClaims: jwt.RegisteredClaims{ID: "jti-1"}}

			got := h.completeVerifyPurpose(context.Background(), c, claims, PurposeLogin, true, "totp")
			require.Equal(t, test.wantStatus == http.StatusOK, got)
			require.Equal(t, test.wantStatus, response.Code)
			require.Equal(t, test.wantLogin, recording.loginCalls)
			require.Equal(t, test.wantLink, recording.linkCalls)
			if test.wantLink > 0 {
				require.Equal(t, "user-1", recording.userID)
				require.Equal(t, "jti-1", recording.jti)
				require.Equal(t, "epoch-1", recording.epoch)
				require.Equal(t, "totp", recording.method)
			}
		})
	}
}

func TestCompletionKindUnknownRejectedBeforeFactor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	claims := ChallengeClaims{UserID: "user-1", Purpose: PurposeLogin, CompletionKind: "unsafe"}
	token, _, err := generateChallengeTokenWithTTL(claims, JWTSecret(testJWTSecret), challengeTTL)
	require.NoError(t, err)
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	h := NewHandler(nil, client, logger.New("test"), nil, testJWTSecret, nil, "test")
	recording := &recordingCompletionKind{}
	h.SetLoginCompleter(recording)
	body, err := json.Marshal(verifyRequest{ChallengeToken: token, Method: "totp", Code: "123456"})
	require.NoError(t, err)
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Verify(c)

	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Zero(t, recording.loginCalls)
	require.Zero(t, recording.linkCalls)
	require.Zero(t, len(mini.Keys()), "invalid completion kind must not spend a factor or challenge")
}

func TestSSOLinkDispatchRejectsReplayedChallengeBeforeFactor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	claims := ChallengeClaims{UserID: "user-1", Purpose: PurposeLogin, CompletionKind: CompletionKindSSOLink}
	token, jti, err := generateChallengeTokenWithTTL(claims, JWTSecret(testJWTSecret), challengeTTL)
	require.NoError(t, err)
	mini := miniredis.RunT(t)
	require.NoError(t, mini.Set(fmt.Sprintf(redisKeyMFAChallengeUsed, jti), "1"))
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	h := NewHandler(nil, client, logger.New("test"), nil, testJWTSecret, nil, "test")
	recording := &recordingCompletionKind{}
	h.SetLoginCompleter(recording)
	body, err := json.Marshal(verifyRequest{ChallengeToken: token, Method: "totp", Code: "123456"})
	require.NoError(t, err)
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Verify(c)

	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Zero(t, recording.loginCalls)
	require.Zero(t, recording.linkCalls)
}

func TestSSOLinkDispatchRequiresFactorAndOneUseClaim(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := newVerifyFixture(t)
	recording := &recordingCompletionKind{}
	fixture.h.SetLoginCompleter(recording)
	claims := ChallengeClaims{UserID: enrollUser, Purpose: PurposeLogin, CompletionKind: CompletionKindSSOLink, CredentialEpoch: "epoch-1", PrimaryAuthMethod: securityevent.AuthPassword}
	token, jti, err := generateChallengeTokenWithTTL(claims, JWTSecret(fixture.h.jwtSecret), challengeTTL)
	require.NoError(t, err)
	fixture.token = token

	wrong := fixture.verify(fixture.wrong)
	require.Equal(t, http.StatusForbidden, wrong.Code)
	require.Zero(t, recording.loginCalls)
	require.Zero(t, recording.linkCalls)
	require.False(t, fixture.exists(t, fmt.Sprintf(redisKeyMFAChallengeUsed, jti)))

	correct := fixture.verify(fixture.right)
	require.Equal(t, http.StatusOK, correct.Code)
	require.Zero(t, recording.loginCalls)
	require.Equal(t, 1, recording.linkCalls)
	require.Equal(t, jti, recording.jti)
	require.Equal(t, "totp", recording.method)
	require.True(t, fixture.exists(t, fmt.Sprintf(redisKeyMFAChallengeUsed, jti)))

	replay := fixture.verify(fixture.right)
	require.Equal(t, http.StatusUnauthorized, replay.Code)
	require.Zero(t, recording.loginCalls)
	require.Equal(t, 1, recording.linkCalls)
}

func TestSSOLinkDispatchCarriesVerifiedEmailMethod(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	require.NoError(t, mini.Set("mfa_email_login:jti-email", "123456"))
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	h := NewHandler(nil, client, logger.New("test"), nil, testJWTSecret, nil, "test")
	recording := &recordingCompletionKind{}
	h.SetLoginCompleter(recording)
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", nil)
	claims := &ChallengeClaims{UserID: "user-1", Purpose: PurposeLogin, CompletionKind: CompletionKindSSOLink, RegisteredClaims: jwt.RegisteredClaims{ID: "jti-email"}}

	verified, method, responded := h.verifyByMethod(c.Request.Context(), c, verifyRequest{Method: "email", Code: "123456"}, claims, false)
	require.True(t, verified)
	require.False(t, responded)
	require.Equal(t, "email", method)
	require.True(t, h.completeVerifyPurpose(c.Request.Context(), c, claims, PurposeLogin, true, method))
	require.Equal(t, 1, recording.linkCalls)
	require.Equal(t, "email", recording.method)
	require.Zero(t, recording.loginCalls)
}

func TestCompletionKindSurvivesSignedChallenge(t *testing.T) {
	for _, kind := range []CompletionKind{"", CompletionKindLogin, CompletionKindSSOLink} {
		claims := ChallengeClaims{UserID: "user-1", Purpose: PurposeLogin, CompletionKind: kind}
		token, _, err := generateChallengeTokenWithTTL(claims, JWTSecret(testJWTSecret), challengeTTL)
		require.NoError(t, err)
		got, err := ValidateChallengeToken(token, testJWTSecret, PurposeLogin)
		require.NoError(t, err)
		require.Equal(t, kind, got.CompletionKind)
	}
}

func TestCompletionKindRejectedOutsideLogin(t *testing.T) {
	claims := ChallengeClaims{UserID: "user-1", Purpose: PurposeMFAUpgrade, CompletionKind: CompletionKindSSOLink, RefreshSessionID: "session-1"}
	token, _, err := generateChallengeTokenWithTTL(claims, JWTSecret(testJWTSecret), challengeTTL)
	require.NoError(t, err)
	_, err = ValidateChallengeToken(token, testJWTSecret, PurposeMFAUpgrade)
	require.Error(t, err)
}
