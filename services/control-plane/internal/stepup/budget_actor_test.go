package stepup

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// Public/request context keys cannot manufacture AuthRequired's private
// provenance. These refusal cases deliberately need neither SQL nor Redis.
func TestBudgetActorFromContextRequiresVerifiedProvenance(t *testing.T) {
	identity := loginfamily.AccessIdentity{
		UserID:          uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		SID:             uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		CredentialEpoch: strings.Repeat("a", 32),
		AccessExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second),
	}
	for _, name := range []string{"nil", "empty", "user only", "user and claims", "request identity only", "request identity and raw claims"} {
		t.Run(name, func(t *testing.T) {
			var c *gin.Context
			w := httptest.NewRecorder()
			if name != "nil" {
				c, _ = gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/step-up/password", nil)
			}
			switch name {
			case "user only", "user and claims", "request identity and raw claims":
				c.Set("user_id", identity.UserID.String())
			}
			switch name {
			case "user and claims", "request identity and raw claims":
				c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{
					"iss": middleware.AccessTokenIssuer, "user_id": identity.UserID.String(),
					"sid": identity.SID.String(), "cred_epoch": identity.CredentialEpoch,
					"exp": identity.AccessExpiresAt.Unix(), "email_verified": true,
				})
			}
			if name == "request identity only" || name == "request identity and raw claims" {
				c.Request = c.Request.WithContext(loginfamily.WithAccessIdentity(c.Request.Context(), identity))
			}
			actor, err := BudgetActorFromContext(c)
			require.Equal(t, BudgetActor{}, actor, "refusal must not return partial authority")
			require.NotNil(t, err)
			require.Equal(t, http.StatusInternalServerError, err.Status)
			require.Equal(t, gin.H{"error": ErrMsgVerificationFailed}, err.Body)
			require.False(t, w.Flushed)
			require.Empty(t, w.Body.String(), "constructor must not write the response")
			if c != nil {
				require.False(t, c.IsAborted(), "caller owns response/abort")
				require.False(t, c.Writer.Written())
			}
		})
	}
}

const budgetActorPath = "/api/v1/auth/step-up/password"

type budgetActorHTTPFixture struct {
	db         *sql.DB
	rc         *redis.Client
	identity   loginfamily.AccessIdentity
	familyID   uuid.UUID
	signingKey string
}

// Root/CI must select a disposable isolated database explicitly: SetupTestDB
// migrates and truncates its configured database. Its checked cleanup owns SQL
// teardown and lock release; each fixture owns a private miniredis keyspace.
func newBudgetActorHTTPFixture(t *testing.T, epoch string) budgetActorHTTPFixture {
	t.Helper()
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "select an isolated disposable DATABASE_URL for actor integration")
	db, _ := testdb.SetupTestDB(t)
	rc := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr(), MaxRetries: -1})
	t.Cleanup(func() { require.NoError(t, rc.Close()) })
	identity := loginfamily.AccessIdentity{
		UserID: testdb.CreateUser(t, db), SID: uuid.New(), CredentialEpoch: epoch,
		AccessExpiresAt: time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second),
	}
	if epoch != "" {
		_, err := db.ExecContext(t.Context(), `UPDATE users SET credential_epoch=$2 WHERE id=$1`, identity.UserID, epoch)
		require.NoError(t, err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() {
		if err := tx.Rollback(); !errors.Is(err, sql.ErrTxDone) {
			require.NoError(t, err)
		}
	}()
	barrier, err := loginfamily.AcquireWriterBarrierTx(t.Context(), tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(t.Context(), tx, barrier, []uuid.UUID{identity.UserID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(identity.UserID)
	require.NoError(t, err)
	family, err := loginfamily.CreateFamilyTx(t.Context(), tx, user, epoch)
	require.NoError(t, err)
	require.NoError(t, loginfamily.BindSIDTx(t.Context(), tx, family, identity.SID, identity.AccessExpiresAt))
	familyID := family.Binding().FamilyID
	require.NotEqual(t, uuid.Nil, familyID)
	require.NoError(t, tx.Commit())
	return budgetActorHTTPFixture{db: db, rc: rc, identity: identity, familyID: familyID, signingKey: uuid.NewString()}
}

func (f budgetActorHTTPFixture) claims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": middleware.AccessTokenIssuer, "user_id": f.identity.UserID.String(),
		"sid": f.identity.SID.String(), "cred_epoch": f.identity.CredentialEpoch,
		"email_verified": true, "jti": uuid.NewString(), "exp": f.identity.AccessExpiresAt.Unix(),
	}
}

// This is an adapter integration route, not the production router/password
// verifier. The real AuthRequired is the only provenance producer.
func (f budgetActorHTTPFixture) request(t *testing.T, claims jwt.MapClaims, fence *credepoch.Fence, handlers ...gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(f.signingKey))
	require.NoError(t, err)
	return f.requestBearer(t, "Bearer "+token, fence, handlers...)
}

func (f budgetActorHTTPFixture) requestBearer(t *testing.T, bearer string, fence *credepoch.Fence, handlers ...gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	r := gin.New()
	chain := make([]gin.HandlerFunc, 0, 2+len(handlers))
	chain = append(chain, middleware.AuthRequired(f.signingKey, f.rc, f.db, fence), middleware.RequireVerifiedEmail())
	chain = append(chain, handlers...)
	r.POST(budgetActorPath, chain...)
	req := httptest.NewRequest(http.MethodPost, budgetActorPath, nil)
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func requireBudgetActorSnapshot(t *testing.T, f budgetActorHTTPFixture, actor BudgetActor) {
	t.Helper()
	require.Equal(t, f.identity.UserID, actor.identity.UserID)
	require.Equal(t, f.identity.SID, actor.identity.SID)
	require.Equal(t, f.identity.CredentialEpoch, actor.identity.CredentialEpoch)
	require.True(t, f.identity.AccessExpiresAt.Equal(actor.identity.AccessExpiresAt))
	require.Equal(t, f.familyID, actor.expectedFamilyID)
	require.NotEqual(t, uuid.Nil, actor.expectedFamilyID)
}

func TestBudgetActorFromContextRealAuthRequiredSnapshot(t *testing.T) {
	for _, epoch := range []string{"", strings.Repeat("a", 32)} {
		t.Run("epoch="+epoch, func(t *testing.T) {
			f := newBudgetActorHTTPFixture(t, epoch)
			calls := 0
			w := f.request(t, f.claims(), nil, func(c *gin.Context) {
				calls++
				actor, err := BudgetActorFromContext(c)
				require.Nil(t, err)
				requireBudgetActorSnapshot(t, f, actor)
				require.False(t, c.Writer.Written())
				require.False(t, c.IsAborted())
				// Actor and accessor copies have no mutable carrier capability.
				actor.identity.UserID = uuid.New()
				actor.identity.SID = uuid.New()
				actor.identity.CredentialEpoch = "local-mutated-epoch"
				actor.identity.AccessExpiresAt = time.Time{}
				actor.expectedFamilyID = uuid.New()
				identity, grant, valid := middleware.VerifiedAccessFromContext(c)
				require.True(t, valid)
				identity.SID = uuid.New()
				grant.FamilyID = uuid.New()
				// Canceling the request after admission cannot break a pure copy.
				ctx, cancel := context.WithCancel(c.Request.Context())
				cancel()
				c.Request = c.Request.WithContext(ctx)
				again, err := BudgetActorFromContext(c)
				require.Nil(t, err)
				requireBudgetActorSnapshot(t, f, again)
				c.Status(http.StatusNoContent)
			})
			require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
			require.Equal(t, 1, calls)
			require.Empty(t, w.Body.String())
		})
	}
}

func TestBudgetActorFromContextRejectsPostAdmissionTampering(t *testing.T) {
	f := newBudgetActorHTTPFixture(t, "")
	for _, name := range []string{"untouched", "user context", "claims user", "SID", "epoch", "expiry", "missing claims", "wrong claims type"} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			w := f.request(t, f.claims(), nil, func(c *gin.Context) {
				calls++
				// Prove this is post-admission integrity rejection, not a JWT
				// which was refused before reaching the constructor boundary.
				_, _, admitted := middleware.VerifiedAccessFromContext(c)
				require.True(t, admitted)
				value, exists := c.Get(middleware.JWTClaimsContextKey)
				require.True(t, exists)
				claims, ok := value.(jwt.MapClaims)
				require.True(t, ok)
				switch name {
				case "user context":
					c.Set("user_id", uuid.NewString())
				case "claims user":
					claims["user_id"] = uuid.NewString()
				case "SID":
					claims["sid"] = uuid.NewString()
				case "epoch":
					claims["cred_epoch"] = strings.Repeat("a", 32)
				case "expiry":
					claims["exp"] = f.identity.AccessExpiresAt.Add(time.Second).Unix()
				case "missing claims":
					delete(c.Keys, middleware.JWTClaimsContextKey)
				case "wrong claims type":
					c.Set(middleware.JWTClaimsContextKey, map[string]string{"user_id": f.identity.UserID.String()})
				}
				actor, err := BudgetActorFromContext(c)
				if name == "untouched" {
					require.Nil(t, err)
					requireBudgetActorSnapshot(t, f, actor)
					c.Status(http.StatusNoContent)
					return
				}
				require.Equal(t, BudgetActor{}, actor)
				require.NotNil(t, err)
				require.Equal(t, http.StatusInternalServerError, err.Status)
				require.Equal(t, gin.H{"error": ErrMsgVerificationFailed}, err.Body)
				require.False(t, c.Writer.Written())
				require.False(t, c.IsAborted())
				err.Write(c)
			})
			require.Equal(t, 1, calls)
			if name == "untouched" {
				require.Equal(t, http.StatusNoContent, w.Code)
			} else {
				require.Equal(t, http.StatusInternalServerError, w.Code)
				require.JSONEq(t, `{"error":"Verification failed"}`, w.Body.String())
			}
		})
	}
}

func (f budgetActorHTTPFixture) revoke(t *testing.T) {
	t.Helper()
	tx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() {
		if err := tx.Rollback(); !errors.Is(err, sql.ErrTxDone) {
			require.NoError(t, err)
		}
	}()
	barrier, err := loginfamily.AcquireWriterBarrierTx(t.Context(), tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(t.Context(), tx, barrier, []uuid.UUID{f.identity.UserID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(f.identity.UserID)
	require.NoError(t, err)
	targets, err := loginfamily.ResolveTargetSIDTx(t.Context(), tx, user, f.identity.SID)
	require.NoError(t, err)
	_, err = loginfamily.RevokeFamiliesTx(t.Context(), tx, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeSession, Reason: loginfamily.ReasonExplicitRevoke})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

func TestBudgetActorFromContextPreservesEarlierAuthResponses(t *testing.T) {
	for _, name := range []string{"live control", "missing bearer", "invalid bearer", "expired JWT", "missing SID", "unknown SID", "epoch mismatch", "terminal family", "disabled user", "blocked epoch", "unavailable Redis", "unverified email"} {
		t.Run(name, func(t *testing.T) {
			f := newBudgetActorHTTPFixture(t, "")
			claims := f.claims()
			var fence *credepoch.Fence
			status := http.StatusUnauthorized
			body := `{"error":"Authentication required"}`
			switch name {
			case "live control":
				status = http.StatusNoContent
			case "expired JWT":
				claims["exp"] = time.Now().Add(-time.Minute).Unix()
			case "missing SID":
				delete(claims, "sid")
			case "unknown SID":
				claims["sid"] = uuid.NewString()
			case "epoch mismatch":
				claims["cred_epoch"] = strings.Repeat("a", 32)
			case "terminal family":
				f.revoke(t)
			case "disabled user":
				require.NoError(t, f.rc.Set(t.Context(), middleware.UserDisabledKey(f.identity.UserID.String()), "1", time.Minute).Err())
				status = http.StatusForbidden
				body = `{"error_code":"account_disabled"}`
			case "blocked epoch":
				require.NoError(t, f.rc.Set(t.Context(), credepoch.Key(f.identity.UserID.String()), "blocked:actor-fixture", time.Minute).Err())
				fence = credepoch.New(f.db, f.rc, nil)
			case "unavailable Redis":
				// A separate already-closed client exercises auth's real503
				// without closing a live dependency or changing shared policy.
				closed := redis.NewClient(&redis.Options{Addr: f.rc.Options().Addr, MaxRetries: -1})
				require.NoError(t, closed.Close())
				f.rc = closed
				status = http.StatusServiceUnavailable
				body = `{"error":"Authentication temporarily unavailable"}`
			case "unverified email":
				claims["email_verified"] = false
				status = http.StatusForbidden
				body = `{"error":"Email verification required","code":"EMAIL_NOT_VERIFIED"}`
			}
			calls := 0
			endpoint := func(c *gin.Context) {
				calls++
				actor, err := BudgetActorFromContext(c)
				require.Nil(t, err)
				requireBudgetActorSnapshot(t, f, actor)
				c.Status(http.StatusNoContent)
			}
			var w *httptest.ResponseRecorder
			switch name {
			case "missing bearer":
				w = f.requestBearer(t, "", fence, endpoint)
			case "invalid bearer":
				w = f.requestBearer(t, "Bearer invalid-test-token", fence, endpoint)
			default:
				w = f.request(t, claims, fence, endpoint)
			}
			require.Equal(t, status, w.Code, w.Body.String())
			if name == "live control" {
				require.Equal(t, 1, calls)
			} else {
				require.Zero(t, calls, "middleware refusal must never become an adapter500")
				require.JSONEq(t, body, w.Body.String())
			}
		})
	}
}

func TestBudgetActorFromContextSnapshotDoesNotRecheckAuthority(t *testing.T) {
	f := newBudgetActorHTTPFixture(t, "")
	calls := 0
	endpoint := func(c *gin.Context) {
		calls++
		actor, err := BudgetActorFromContext(c)
		require.Nil(t, err)
		requireBudgetActorSnapshot(t, f, actor)
		// This changes real SQL authority after genuine admission. Construction
		// remains a pure snapshot; a future reservation owns revalidation.
		f.revoke(t)
		again, err := BudgetActorFromContext(c)
		require.Nil(t, err)
		requireBudgetActorSnapshot(t, f, again)
		c.Status(http.StatusNoContent)
	}
	w := f.request(t, f.claims(), nil, endpoint)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	require.Equal(t, 1, calls)
	// A newly admitted request still sees the real terminal family.
	w = f.request(t, f.claims(), nil, endpoint)
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.Equal(t, 1, calls)
	require.JSONEq(t, `{"error":"Authentication required"}`, w.Body.String())
}
