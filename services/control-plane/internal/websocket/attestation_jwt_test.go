package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

func TestJWTWebSocketAttestationOnlyOnFallback(t *testing.T) {
	db := setupHubTestDB(t)
	rdb := setupHubTestRedis(t)
	userID := dbtest.CreateUser(t, db)
	access := admissionCanonicalAccess(t, db, userID)
	h := NewHandler(nil, db, rdb, testJWTSecret, nil, nil, nil, true, logger.NewWithWriter(io.Discard))

	request := func(path string, headers http.Header) (*gin.Context, *httptest.ResponseRecorder) {
		t.Helper()
		out := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(out)
		c.Request = httptest.NewRequest(http.MethodGet, path, nil)
		c.Request.Header = headers
		return c, out
	}
	jwtPath := wsTokenPath + access.token
	missing, missingOut := request(jwtPath, http.Header{"X-Session-Id": []string{access.sid.String()}})
	h.HandleWebSocket(missing)
	require.Equal(t, http.StatusForbidden, missingOut.Code)
	require.Contains(t, missingOut.Body.String(), string(attestation.ErrMissing))

	record := attestation.TokenRecord{Token: "proof", Version: "desktop-v1", UserID: attestation.HashUserID(userID.String()), IssuedAt: time.Now()}
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	require.NoError(t, rdb.Set(context.Background(), "attestation:"+access.sid.String()+":web", raw, time.Minute).Err())
	bad, badOut := request(jwtPath, http.Header{
		"X-Attestation-Token": []string{"wrong"},
		"X-Session-Id":        []string{access.sid.String()},
	})
	h.HandleWebSocket(bad)
	require.Equal(t, http.StatusForbidden, badOut.Code)
	require.Contains(t, badOut.Body.String(), string(attestation.ErrInvalid))

	good, goodOut := request(jwtPath, http.Header{
		"X-Attestation-Token": []string{"proof"},
		"X-Session-Id":        []string{access.sid.String()},
	})
	gotUser, gotSID, err := h.authenticateWebSocket(good)
	require.NoError(t, err)
	require.Equal(t, userID, gotUser)
	require.Equal(t, access.sid.String(), gotSID)
	require.False(t, goodOut.Code == http.StatusForbidden)
	require.Equal(t, "desktop-v1", good.GetString("attestation_version"))

	// The ticket was minted from this JWT after attestation. Its 30-second
	// redemption rail must not perform a second attestation lookup.
	ticket := admissionMintTicket(t, db, rdb, access)
	rdb.AddHook(attestationReadFailureHook{})
	ticketCtx, ticketOut := request(wsTicketPath+ticket, nil)
	gotUser, gotSID, err = h.authenticateWebSocket(ticketCtx)
	require.NoError(t, err)
	require.Equal(t, userID, gotUser)
	require.Equal(t, access.sid.String(), gotSID)
	require.False(t, ticketOut.Code == http.StatusForbidden)
}

func TestJWTWebSocketAttestationRejectsDifferentSessionOfSameUser(t *testing.T) {
	db := setupHubTestDB(t)
	rdb := setupHubTestRedis(t)
	userID := dbtest.CreateUser(t, db)
	access := admissionCanonicalAccess(t, db, userID)
	other := admissionCanonicalAccess(t, db, userID)
	record := attestation.TokenRecord{Token: "proof", Version: "desktop-v1", UserID: attestation.HashUserID(userID.String()), IssuedAt: time.Now()}
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	require.NoError(t, rdb.Set(context.Background(), "attestation:"+other.sid.String()+":web", raw, time.Minute).Err())
	h := NewHandler(nil, db, rdb, testJWTSecret, nil, nil, nil, true, logger.NewWithWriter(io.Discard))
	out := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(out)
	c.Request = httptest.NewRequest(http.MethodGet, wsTokenPath+access.token, nil)
	c.Request.Header.Set("X-Session-ID", other.sid.String())
	c.Request.Header.Set("X-Attestation-Token", "proof")
	h.HandleWebSocket(c)
	require.Equal(t, http.StatusForbidden, out.Code)
	require.Contains(t, out.Body.String(), "X-Session-ID does not match the access token")
}

func TestWSTicketRejectsAttestationFromDifferentSession(t *testing.T) {
	db := setupHubTestDB(t)
	rdb := setupHubTestRedis(t)
	userID := dbtest.CreateUser(t, db)
	access := admissionCanonicalAccess(t, db, userID)
	other := admissionCanonicalAccess(t, db, userID)
	record := attestation.TokenRecord{Token: "proof", Version: "desktop-v1", UserID: attestation.HashUserID(userID.String()), IssuedAt: time.Now()}
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	require.NoError(t, rdb.Set(context.Background(), "attestation:"+other.sid.String()+":web", raw, time.Minute).Err())
	router := gin.New()
	router.POST("/ticket", middleware.AuthRequired(testJWTSecret, rdb, db, nil),
		middleware.RequireSessionHeaderMatchesToken(),
		middleware.RequireAttestation(true, rdb, logger.NewWithWriter(io.Discard)),
		auth.NewWSTicketHandler(rdb, testJWTSecret).IssueTicket)
	request := httptest.NewRequest(http.MethodPost, "/ticket", nil)
	request.Header.Set("Authorization", "Bearer "+access.token)
	request.Header.Set("X-Session-ID", other.sid.String())
	request.Header.Set("X-Attestation-Token", "proof")
	out := httptest.NewRecorder()
	router.ServeHTTP(out, request)
	require.Equal(t, http.StatusForbidden, out.Code)
	keys, err := rdb.Keys(context.Background(), wsTicketKeyPfx+"*").Result()
	require.NoError(t, err)
	require.Empty(t, keys)
}

func TestJWTWebSocketAttestationRedisFailureIs503(t *testing.T) {
	db := setupHubTestDB(t)
	rdb := setupHubTestRedis(t)
	userID := dbtest.CreateUser(t, db)
	access := admissionCanonicalAccess(t, db, userID)
	rdb.AddHook(attestationReadFailureHook{})
	h := NewHandler(nil, db, rdb, testJWTSecret, nil, nil, nil, true, logger.NewWithWriter(io.Discard))
	out := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(out)
	c.Request = httptest.NewRequest(http.MethodGet, wsTokenPath+access.token, nil)
	c.Request.Header.Set("X-Session-ID", access.sid.String())
	c.Request.Header.Set("X-Attestation-Token", "proof")
	h.HandleWebSocket(c)
	require.Equal(t, http.StatusServiceUnavailable, out.Code)
	require.Contains(t, out.Body.String(), "attestation registry temporarily unavailable")
}

func TestJWTWebSocketAttestationValidProofUpgrades(t *testing.T) {
	db := setupHubTestDB(t)
	rdb := setupHubTestRedis(t)
	hub := NewHub(db, rdb)
	go hub.Run()
	userID := dbtest.CreateUser(t, db)
	access := admissionCanonicalAccess(t, db, userID)
	record := attestation.TokenRecord{Token: "proof", Version: "desktop-v1", UserID: attestation.HashUserID(userID.String()), IssuedAt: time.Now()}
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	require.NoError(t, rdb.Set(context.Background(), "attestation:"+access.sid.String()+":web", raw, time.Minute).Err())
	h := NewHandler(hub, db, rdb, testJWTSecret, []string{"*"}, nil, nil, true, logger.NewWithWriter(io.Discard))
	server := setupWSTestServer(t, h, hub)
	headers := http.Header{}
	headers.Set("X-Attestation-Token", "proof")
	headers.Set("X-Session-ID", access.sid.String())
	conn, response, err := gorillaWS.DefaultDialer.Dial("ws"+server.URL[4:]+wsTokenPath+access.token, headers)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
	require.Eventually(t, func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		for clientID := range hub.userClients[userID] {
			if client := hub.clients[clientID]; client != nil && client.SessionID == access.sid.String() {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, conn.Close())
}

type attestationReadFailureHook struct{}

func (attestationReadFailureHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (attestationReadFailureHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" && strings.HasPrefix(cmd.String(), "get attestation:") {
			err := errors.New("forced attestation store fault")
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}
func (attestationReadFailureHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
