package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

// adminIDContextKey is the gin.Context key under which AdminAuthRequired stores
// the resolved admin id for downstream handlers. Handlers read it via
// c.GetString(adminIDContextKey) — it is the ADMIN identity, never a user_id.
const adminIDContextKey = "admin_id"

// adminSessionContextKey is the gin.Context key under which AdminAuthRequired
// stores adminSessionBinding(sid) for the session that passed. It names the
// session without carrying the session id, so a handler can bind state to one
// session (the create_admin step-up does) and never hold the cookie value.
const adminSessionContextKey = "admin_session_binding"

// adminSessionBinding is a domain-separated SHA-256 of a session id. The id is
// 256 random bits, so the digest cannot be reversed or guessed, and the
// domain keeps it distinct from the sign-in notice's session digest.
func adminSessionBinding(sid string) string {
	digest := sha256.Sum256([]byte("admin_session_binding\x00v1\x00" + sid))
	return hex.EncodeToString(digest[:])
}

// SessionGenerationResolver resolves the generation that is authorized for an active admin.
type SessionGenerationResolver interface {
	ActiveSessionGeneration(ctx context.Context, adminID string) (int64, error)
}

// AdminAuthRequired gates admin-console routes on a valid opaque admin session.
// It reads ONLY the `__Host-cv_admin_sid` cookie and resolves it through the
// SessionStore — it NEVER consults the user-facing Authorization header / JWT,
// keeping the admin identity fully isolated from end-user accounts (#1688 §6).
//
// On a valid session it sets c.Set("admin_id", <adminID>) and the session's
// binding (adminSessionContextKey), then calls c.Next().
// On any failure (no cookie, unknown/expired/malformed sid) it aborts with 401
// and never reaches the wrapped handler — the deny-by-default posture verified by
// the reflective routes_test.
//
//nolint:revive // Admin* prefix is the #1688 cross-task naming contract (see types.go header).
func AdminAuthRequired(sessions *SessionStore, authority SessionGenerationResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		sid, err := c.Cookie(adminCookieName)
		if err != nil || sid == "" {
			// No admin session cookie present.
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}

		sess, err := sessions.Get(c.Request.Context(), sid)
		if err != nil {
			// Unknown, expired (idle or absolute), or malformed session id.
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		generation, err := authority.ActiveSessionGeneration(c.Request.Context(), sess.AdminID)
		if err != nil && !errors.Is(err, ErrAdminNotFound) {
			// A database fault still fails closed, but it must not look like an
			// ordinary sign-out.
			slog.Error("admin session authority check failed", "error", err)
		}
		if err != nil || generation != sess.SessionGeneration {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}

		c.Set(adminIDContextKey, sess.AdminID)
		c.Set(adminSessionContextKey, adminSessionBinding(sid))
		c.Next()
	}
}
