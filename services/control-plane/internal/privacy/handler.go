// Package privacy provides a GDPR Article 17 account-erasure handler.
package privacy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Handler exposes the privacy erasure endpoints.
type Handler struct {
	account            users.AccountDeleter
	redis              *redis.Client
	log                *logger.Logger
	erasureEmailSender ErasureEmailSender
}

// ErasureEmailSender sends the one-use account-erasure confirmation link.
// Implementations must never log the URL or its embedded secret.
type ErasureEmailSender interface {
	SendAccountErasureConfirmation(to, confirmationURL string) error
}

// NewHandler builds a Handler wired to the given account-deletion service.
// Panics if account is nil — every method dereferences it unconditionally;
// a nil value would surface as an opaque nil-pointer panic on the first
// request. Panicking at construction makes mis-wiring obvious at startup.
//
// A nil redis client IS allowed in direct unit tests that do not exercise
// current-token revocation; production callers must always pass a non-nil
// client. A nil logger IS allowed in tests that do not exercise the failure
// path; production callers must always pass a non-nil logger. The log field is
// only dereferenced behind a nil-check on the error path.
func NewHandler(account users.AccountDeleter, redisClient *redis.Client, log *logger.Logger) *Handler {
	if account == nil {
		panic("privacy.NewHandler: account AccountDeleter must not be nil")
	}
	return &Handler{account: account, redis: redisClient, log: log}
}

// SetErasureEmailSender wires delivery for unverified-email erasure intents.
func (h *Handler) SetErasureEmailSender(sender ErasureEmailSender) { h.erasureEmailSender = sender }

const maxEraseAccountRequestBytes int64 = 8192

type eraseAccountRequest struct {
	Password                string   `json:"password"` //nolint:gosec // request field, not a stored secret
	MFACode                 string   `json:"mfa_code"`
	ConfirmedOwnedServerIDs []string `json:"confirmed_owned_server_ids"`
	// ClientID remains accepted only for the compatibility window. It has no
	// authority and is deliberately not copied into AccountErasureRequest.
	ClientID          string                         `json:"clientId"`
	EmailConfirmation *eraseEmailConfirmationRequest `json:"email_confirmation"`
}

type eraseEmailConfirmationRequest struct {
	RequestID string `json:"request_id"`
	Token     string `json:"token"`
}

type parsedEraseAccountRequest struct {
	Password                string
	MFACode                 string
	ConfirmedOwnedServerIDs []uuid.UUID
	EmailConfirmation       *eraseEmailConfirmationRequest
}

func bindEraseAccountRequest(c *gin.Context) (parsedEraseAccountRequest, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxEraseAccountRequestBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var body eraseAccountRequest
	if err := decoder.Decode(&body); err != nil {
		if !errors.Is(err, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		}
		if errors.Is(err, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		}
		return parsedEraseAccountRequest{}, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return parsedEraseAccountRequest{}, false
	}
	if body.ConfirmedOwnedServerIDs == nil {
		// A bearer-only request must reach the proof gate and receive the
		// indistinguishable step_up_required refusal. Once any proof material is
		// supplied, the acknowledgement array is independently mandatory.
		if body.Password != "" || body.MFACode != "" || body.EmailConfirmation != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
			return parsedEraseAccountRequest{}, false
		}
		return parsedEraseAccountRequest{}, true
	}
	confirmed := make([]uuid.UUID, len(body.ConfirmedOwnedServerIDs))
	var previous string
	for i, value := range body.ConfirmedOwnedServerIDs {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value || (i > 0 && value <= previous) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
			return parsedEraseAccountRequest{}, false
		}
		confirmed[i] = id
		previous = value
	}
	return parsedEraseAccountRequest{
		Password:                body.Password,
		MFACode:                 body.MFACode,
		ConfirmedOwnedServerIDs: confirmed,
		EmailConfirmation:       body.EmailConfirmation,
	}, true
}

// EraseAccount handles POST /api/v1/privacy/erase-account.
// Performs the atomic DB erasure via the AccountDeleter, which cascades
// through every user_id-FK table configured with ON DELETE CASCADE and
// inserts a privacy-safe audit row.
func (h *Handler) EraseAccount(c *gin.Context) {
	userID := c.GetString("user_id")
	// Defense-in-depth: auth middleware is expected to have populated user_id
	// before this handler runs. If a future routing change exposed this
	// endpoint behind optional auth, DeleteAccount would attempt a
	// DELETE WHERE id = '' and the audit row would be written without a
	// meaningful subject. Fail closed instead.
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	request, ok := bindEraseAccountRequest(c)
	if !ok {
		return
	}
	identity, ok := loginfamily.AccessIdentityFromContext(c.Request.Context())
	if !ok || identity.UserID.String() != userID {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	attempt, stepErr := h.account.PrepareAccountErasure(c.Request.Context(), userID, request.Password, request.MFACode)
	if stepErr != nil {
		stepErr.Write(c)
		return
	}
	erasure := users.AccountErasureRequest{
		Actor:                   identity,
		Password:                request.Password,
		MFACode:                 request.MFACode,
		PasswordAttempt:         attempt,
		ConfirmedOwnedServerIDs: request.ConfirmedOwnedServerIDs,
	}
	verifiedEmail, err := h.account.EmailVerified(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "email confirmation status unavailable"})
		return
	}
	if !verifiedEmail {
		if request.EmailConfirmation == nil {
			h.issueEmailErasureConfirmation(c, erasure)
			return
		}
		claim, ok := h.claimEmailErasureConfirmation(c, erasure, request.EmailConfirmation)
		if !ok {
			return
		}
		defer h.settleEmailErasureConfirmation(claim, &err)
	} else if request.EmailConfirmation != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	// deliveryFailed marks a DURABLE erasure whose presence delivery did not
	// settle. The account is gone; only the fan-out failed.
	var deliveryFailed bool
	if err = h.account.DeleteAccount(c.Request.Context(), erasure); err != nil {
		var verificationErr *stepup.Error
		if errors.As(err, &verificationErr) {
			verificationErr.Write(c)
			return
		}
		var ownedSetErr *users.ErrErasureOwnedServerSetDrifted
		if errors.As(err, &ownedSetErr) {
			c.JSON(http.StatusConflict, gin.H{
				"error_code":                "owned_server_confirmation_stale",
				"required_owned_server_ids": ownedSetErr.RequiredOwnedServerIDs,
			})
			return
		}
		if errors.Is(err, users.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "account not found"})
			return
		}
		if errors.Is(err, credepoch.ErrEpochMismatch) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		if errors.Is(err, users.ErrErasureCandidateSetDrifted) {
			c.JSON(http.StatusConflict, gin.H{
				"error": "account state changed; retry the deletion",
			})
			return
		}
		if errors.Is(err, loginfamily.ErrUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Authentication temporarily unavailable"})
			return
		}
		if !errors.Is(err, presencecapture.ErrPostCommitDelivery) {
			if h.log != nil {
				h.log.Error("erase-account: account deletion failed",
					"user_id", userID,
					"error", err,
				)
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "account deletion failed"})
			return
		}
		deliveryFailed = true
		if h.log != nil {
			h.log.Error("erase-account: presence delivery did not settle",
				"user_id", userID,
				"failure_class", "delivery",
				"error", err,
			)
		}
	}

	if err := h.revokeAccessTokens(c, userID); err != nil {
		if h.log != nil {
			h.log.Error("erase-account: access token revocation failed",
				"user_id", userID,
				"error", err,
			)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "access token revocation failed"})
		return
	}

	// Erased and de-authorized; only the presence fan-out is unsettled. Report
	// that rather than a 204 the caller would read as fully settled.
	if deliveryFailed {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "Your account was deleted. Updating everyone who could see you is taking longer than usual.",
		})
		return
	}

	c.Status(http.StatusNoContent)
}

func (h *Handler) issueEmailErasureConfirmation(c *gin.Context, request users.AccountErasureRequest) {
	prepared, err := h.account.PrepareEmailErasureConfirmation(c.Request.Context(), request)
	if h.writeErasurePreparationError(c, err) {
		return
	}
	intent, err := NewEmailConfirmationStore(h.redis).Issue(c.Request.Context(), EmailConfirmationBinding{
		Recipient: prepared.Recipient, UserID: request.Actor.UserID.String(), FamilyID: prepared.FamilyID,
		CredentialEpoch: prepared.CredentialEpoch, OwnedServerIDs: prepared.OwnedServerIDs,
	})
	if err != nil {
		h.writeEmailConfirmationError(c, err)
		return
	}
	if h.erasureEmailSender == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "email confirmation unavailable"})
		return
	}
	confirmationURL := "concord://account-erasure/confirm/" + intent.RequestID + "/" + intent.Token
	if err := h.erasureEmailSender.SendAccountErasureConfirmation(prepared.Recipient, confirmationURL); err != nil {
		if h.log != nil {
			h.log.Error("erase-account: email confirmation delivery failed", "error", err)
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "email confirmation unavailable"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"status": "email_confirmation_required", "request_id": intent.RequestID,
		"expires_in": int(intent.ExpiresIn.Seconds()),
	})
}

func (h *Handler) claimEmailErasureConfirmation(
	c *gin.Context, request users.AccountErasureRequest, confirmation *eraseEmailConfirmationRequest,
) (*EmailConfirmationClaim, bool) {
	if confirmation.RequestID == "" || confirmation.Token == "" {
		c.JSON(http.StatusConflict, gin.H{"error_code": "email_confirmation_stale"})
		return nil, false
	}
	prepared, err := h.account.EmailErasureConfirmationBinding(c.Request.Context(), request)
	if h.writeErasurePreparationError(c, err) {
		return nil, false
	}
	claim, err := NewEmailConfirmationStore(h.redis).Claim(c.Request.Context(), confirmation.RequestID, confirmation.Token,
		EmailConfirmationBinding{Recipient: prepared.Recipient, UserID: request.Actor.UserID.String(), FamilyID: prepared.FamilyID,
			CredentialEpoch: prepared.CredentialEpoch, OwnedServerIDs: prepared.OwnedServerIDs})
	if err != nil {
		h.writeEmailConfirmationError(c, err)
		return nil, false
	}
	return claim, true
}

func (h *Handler) settleEmailErasureConfirmation(claim *EmailConfirmationClaim, operationErr *error) {
	if claim == nil {
		return
	}
	if *operationErr == nil || errors.Is(*operationErr, presencecapture.ErrPostCommitDelivery) {
		if err := claim.Commit(context.Background()); err != nil && h.log != nil {
			h.log.Error("erase-account: email confirmation commit failed", "error", err)
		}
		return
	}
	if errors.Is(*operationErr, presencecapture.ErrCommitUnresolved) {
		return
	}
	if err := claim.Release(context.Background()); err != nil && h.log != nil {
		h.log.Error("erase-account: email confirmation release failed", "error", err)
	}
}

func (h *Handler) writeErasurePreparationError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	var verificationErr *stepup.Error
	if errors.As(err, &verificationErr) {
		verificationErr.Write(c)
		return true
	}
	var ownedSetErr *users.ErrErasureOwnedServerSetDrifted
	if errors.As(err, &ownedSetErr) {
		c.JSON(http.StatusConflict, gin.H{"error_code": "owned_server_confirmation_stale", "required_owned_server_ids": ownedSetErr.RequiredOwnedServerIDs})
		return true
	}
	if errors.Is(err, users.ErrErasureEmailAlreadyVerified) {
		c.JSON(http.StatusConflict, gin.H{"error_code": "email_confirmation_stale"})
		return true
	}
	if errors.Is(err, credepoch.ErrEpochMismatch) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return true
	}
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "email confirmation unavailable"})
	return true
}

func (h *Handler) writeEmailConfirmationError(c *gin.Context, err error) {
	if errors.Is(err, ErrEmailConfirmationStale) || errors.Is(err, ErrEmailConfirmationInvalid) {
		c.JSON(http.StatusConflict, gin.H{"error_code": "email_confirmation_stale"})
		return
	}
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "email confirmation unavailable"})
}

func (h *Handler) revokeAccessTokens(c *gin.Context, userID string) error {
	if h.redis == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	if err := h.redis.Set(ctx, middleware.UserDisabledKey(userID), "1", auth.AccessTokenTTL).Err(); err != nil {
		return fmt.Errorf("denylist erased user: %w", err)
	}

	rawClaims, ok := c.Get(middleware.JWTClaimsContextKey)
	if !ok {
		return nil
	}
	claims, ok := rawClaims.(jwt.MapClaims)
	if !ok {
		return errors.New("jwt claims have unexpected type")
	}

	jti, ok := claims["jti"].(string)
	if !ok || jti == "" {
		return errors.New("jwt claims missing jti")
	}
	expiresAt, err := claims.GetExpirationTime()
	if err != nil {
		return fmt.Errorf("read jwt expiration: %w", err)
	}
	if expiresAt == nil {
		return errors.New("jwt claims missing expiration")
	}

	if err := middleware.BlacklistToken(ctx, h.redis, jti, time.Until(expiresAt.Time)); err != nil {
		return fmt.Errorf("blacklist current access token: %w", err)
	}
	return nil
}
