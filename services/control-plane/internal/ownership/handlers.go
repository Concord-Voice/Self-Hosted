// Package ownership provides handlers for server ownership transfer.
package ownership

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/email"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/mfa"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	transferPendingDuration = 24 * time.Hour
	reversalWindowDuration  = 24 * time.Hour

	errMsgInvalidServerID       = "Invalid server ID"
	errMsgServerNotFound        = "Server not found"
	errMsgFailedQueryOwner      = "Failed to query server owner"
	errMsgFailedVerifyOwnership = "Failed to verify ownership"
	errMsgFailedVerifyPassword  = "Failed to verify password"
	errMsgFailedReverseTransfer = "Failed to reverse transfer"

	keyServerID   = "server_id"
	keyUserID     = "user_id"
	keyToUserID   = "to_user_id"
	keyFromUserID = "from_user_id"
	keyExpiresAt  = "expires_at"
	keyServer     = "server"
	keyTransferID = "transfer_id"
	keyMessage    = "message"
)

// Handler handles ownership transfer requests.
type Handler struct {
	db          *sql.DB
	log         *logger.Logger
	hub         *websocket.Hub
	redis       *redis.Client
	cache       *rbac.PermissionCache
	audit       *rbac.AuditWriter
	emailSvc    *email.Service
	mfaVerifier mfa.Verifier
}

// NewHandler creates a new ownership transfer handler.
func NewHandler(
	db *sql.DB,
	log *logger.Logger,
	hub *websocket.Hub,
	redisClient *redis.Client,
	cache *rbac.PermissionCache,
	audit *rbac.AuditWriter,
	emailSvc *email.Service,
	mfaVerifier mfa.Verifier,
) *Handler {
	return &Handler{
		db:          db,
		log:         log,
		hub:         hub,
		redis:       redisClient,
		cache:       cache,
		audit:       audit,
		emailSvc:    emailSvc,
		mfaVerifier: mfaVerifier,
	}
}

// initiateTransferRequest is the request body for InitiateTransfer.
type initiateTransferRequest struct {
	TargetUserID string `json:"target_user_id" binding:"required,uuid"`
	Password     string `json:"password" binding:"required"`
	MFACode      string `json:"mfa_code"`
}

// InitiateTransfer starts an ownership transfer to a target user.
// POST /servers/:id/transfer-ownership
func (h *Handler) InitiateTransfer(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	var req initiateTransferRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	if req.TargetUserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot transfer ownership to yourself"})
		return
	}

	// Verify caller is the server owner
	var ownerID string
	err := h.db.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgServerNotFound})
		return
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryOwner, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyOwnership})
		return
	}
	if ownerID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the server owner can transfer ownership"})
		return
	}

	// Password re-verification
	if err := h.verifyPassword(c, userID, req.Password); err != nil {
		return // verifyPassword already sent error response
	}

	// MFA verification (if enabled)
	if err := h.verifyMFA(c, userID, req.MFACode); err != nil {
		return // verifyMFA already sent error response
	}

	// Verify target is a member of the server
	var targetExists bool
	err = h.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, req.TargetUserID,
	).Scan(&targetExists)
	if err != nil {
		h.log.Error("Failed to check target membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify target user"})
		return
	}
	if !targetExists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Target user is not a member of this server"})
		return
	}

	// Check no pending transfer exists
	var pendingExists bool
	err = h.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM ownership_transfers WHERE server_id = $1 AND status = 'pending')`,
		serverID,
	).Scan(&pendingExists)
	if err != nil {
		h.log.Error("Failed to check pending transfers", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check transfer status"})
		return
	}
	if pendingExists {
		c.JSON(http.StatusConflict, gin.H{"error": "A transfer is already pending for this server"})
		return
	}

	// Generate reversal token
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		h.log.Error("Failed to generate reversal token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to initiate transfer"})
		return
	}
	reversalToken := hex.EncodeToString(tokenBytes)

	// Insert transfer record
	transferID := uuid.New().String()
	now := time.Now()
	expiresAt := now.Add(transferPendingDuration)

	_, err = h.db.ExecContext(ctx, `
		INSERT INTO ownership_transfers (id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at)
		VALUES ($1, $2, $3, $4, 'pending', $5, $6, $7)
	`, transferID, serverID, userID, req.TargetUserID, reversalToken, now, expiresAt)
	if err != nil {
		// Unique index violation means a concurrent request already created a pending transfer
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "A transfer is already pending for this server"})
			return
		}
		h.log.Error("Failed to create transfer record", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to initiate transfer"})
		return
	}

	// Send email notification to current owner (best-effort)
	h.sendTransferEmail(ctx, userID, serverID, req.TargetUserID, reversalToken)

	// Broadcast to server members
	if serverUUID, err := uuid.Parse(serverID); err == nil {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "ownership_transfer_initiated",
			Data: map[string]interface{}{
				keyServerID:   serverID,
				keyFromUserID: userID,
				keyToUserID:   req.TargetUserID,
				keyExpiresAt:  expiresAt.Format(time.RFC3339),
			},
		})
	}

	// Audit log
	if err := h.audit.Log(ctx, serverID, &userID, "ownership_transfer_initiated", keyServer, &serverID, map[string]interface{}{
		keyToUserID:  req.TargetUserID,
		keyExpiresAt: expiresAt.Format(time.RFC3339),
	}); err != nil {
		h.log.Error("Failed to write audit log for ownership_transfer_initiated", "error", err, "server_id", serverID)
	}

	c.JSON(http.StatusCreated, gin.H{
		keyTransferID: transferID,
		keyServerID:   serverID,
		keyFromUserID: userID,
		keyToUserID:   req.TargetUserID,
		"status":      "pending",
		keyExpiresAt:  expiresAt.Format(time.RFC3339),
	})
}

// GetTransferStatus returns the current transfer status for a server.
// GET /servers/:id/transfer-ownership
func (h *Handler) GetTransferStatus(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	// Verify caller is a member
	var isMember bool
	err := h.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID,
	).Scan(&isMember)
	if err != nil {
		h.log.Error("Failed to verify server membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify membership"})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	}

	// Fetch pending transfer
	var transferID, fromUserID, toUserID, status string
	var requestedAt, expiresAt time.Time
	err = h.db.QueryRowContext(ctx, `
		SELECT id, from_user_id, to_user_id, status, requested_at, expires_at
		FROM ownership_transfers
		WHERE server_id = $1 AND status = 'pending'
		ORDER BY requested_at DESC LIMIT 1
	`, serverID).Scan(&transferID, &fromUserID, &toUserID, &status, &requestedAt, &expiresAt)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusOK, gin.H{"transfer": nil})
		return
	}
	if err != nil {
		h.log.Error("Failed to query transfer status", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch transfer status"})
		return
	}

	resp := gin.H{
		keyTransferID:  transferID,
		"status":       status,
		"requested_at": requestedAt.Format(time.RFC3339),
		keyExpiresAt:   expiresAt.Format(time.RFC3339),
	}

	// Only owner and target see the to_user_id
	if userID == fromUserID || userID == toUserID {
		resp[keyFromUserID] = fromUserID
		resp[keyToUserID] = toUserID
	}

	c.JSON(http.StatusOK, gin.H{"transfer": resp})
}

// CancelTransfer cancels a pending ownership transfer.
// DELETE /servers/:id/transfer-ownership
func (h *Handler) CancelTransfer(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	// Verify caller is the server owner
	var ownerID string
	err := h.db.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgServerNotFound})
		return
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryOwner, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyOwnership})
		return
	}
	if ownerID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the server owner can cancel the transfer"})
		return
	}

	// Cancel the pending transfer
	res, err := h.db.ExecContext(ctx, `
		UPDATE ownership_transfers
		SET status = 'cancelled', cancelled_at = NOW()
		WHERE server_id = $1 AND status = 'pending'
	`, serverID)
	if err != nil {
		h.log.Error("Failed to cancel transfer", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to cancel transfer"})
		return
	}

	rows, _ := res.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "No pending transfer to cancel"})
		return
	}

	// Broadcast
	if serverUUID, err := uuid.Parse(serverID); err == nil {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "ownership_transfer_cancelled",
			Data: map[string]interface{}{
				keyServerID: serverID,
			},
		})
	}

	// Audit log
	if err := h.audit.Log(ctx, serverID, &userID, "ownership_transfer_cancelled", keyServer, &serverID, nil); err != nil {
		h.log.Error("Failed to write audit log for ownership_transfer_cancelled", "error", err, "server_id", serverID)
	}

	c.JSON(http.StatusOK, gin.H{keyMessage: "Transfer cancelled"})
}

// ConfirmTransfer completes a pending transfer early (owner confirms).
// POST /servers/:id/transfer-ownership/confirm
func (h *Handler) ConfirmTransfer(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	// Verify caller is the server owner
	var ownerID string
	err := h.db.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgServerNotFound})
		return
	}
	if err != nil {
		h.log.Error(errMsgFailedQueryOwner, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyOwnership})
		return
	}
	if ownerID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the server owner can confirm the transfer"})
		return
	}

	// Fetch pending transfer
	var transferID, toUserID string
	err = h.db.QueryRowContext(ctx, `
		SELECT id, to_user_id
		FROM ownership_transfers
		WHERE server_id = $1 AND status = 'pending'
	`, serverID).Scan(&transferID, &toUserID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "No pending transfer to confirm"})
		return
	}
	if err != nil {
		h.log.Error("Failed to query pending transfer", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to confirm transfer"})
		return
	}

	// Execute the transfer
	if err := h.executeTransfer(ctx, serverID, transferID, userID, toUserID); err != nil {
		switch {
		case errors.Is(err, errTransferAlreadyCompleted):
			c.JSON(http.StatusConflict, gin.H{"error": "Transfer has already been completed or cancelled"})
		case errors.Is(err, errToUserNotMember):
			c.JSON(http.StatusConflict, gin.H{"error": "Target user is no longer a member of this server"})
		case errors.Is(err, errFromUserNotMember):
			c.JSON(http.StatusConflict, gin.H{"error": "Current owner membership record is missing"})
		default:
			h.log.Error("Failed to execute transfer", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to complete transfer"})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{
		keyMessage:     "Ownership transferred",
		"new_owner_id": toUserID,
	})
}

// reverseTransferRequest is the request body for ReverseTransfer.
type reverseTransferRequest struct {
	Password string `json:"password" binding:"required"`
	MFACode  string `json:"mfa_code"`
}

// ReverseTransfer reverses a completed transfer using the email reversal token.
// POST /ownership/reverse/:token
func (h *Handler) ReverseTransfer(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("user_id")
	token := c.Param("token")

	if len(token) != 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid reversal token"})
		return
	}

	// Lookup completed transfer by token
	var transferID, serverID, fromUserID, toUserID string
	var completedAt time.Time
	err := h.db.QueryRowContext(ctx, `
		SELECT id, server_id, from_user_id, to_user_id, completed_at
		FROM ownership_transfers
		WHERE reversal_token = $1 AND status = 'completed'
	`, token).Scan(&transferID, &serverID, &fromUserID, &toUserID, &completedAt)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invalid or expired reversal token"})
		return
	}
	if err != nil {
		h.log.Error("Failed to query reversal token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process reversal"})
		return
	}

	// Verify reversal window (24h from completion)
	if time.Since(completedAt) > reversalWindowDuration {
		c.JSON(http.StatusGone, gin.H{"error": "Reversal window has expired"})
		return
	}

	// Verify caller is the original owner
	if userID != fromUserID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the original owner can reverse the transfer"})
		return
	}

	var req reverseTransferRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Password re-verification
	if err := h.verifyPassword(c, userID, req.Password); err != nil {
		return
	}

	// MFA verification (if enabled)
	if err := h.verifyMFA(c, userID, req.MFACode); err != nil {
		return
	}

	// Execute reversal: swap ownership back
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.log.Error("Failed to begin reversal transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReverseTransfer})
		return
	}
	defer func() { _ = tx.Rollback() }()

	// Guard: verify the server is still owned by the transfer recipient.
	// If another transfer happened after this one, abort to avoid overwriting.
	var currentOwnerID string
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1 FOR UPDATE`, serverID).Scan(&currentOwnerID); err != nil {
		h.log.Error("Failed to query current owner for reversal", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReverseTransfer})
		return
	}
	if currentOwnerID != toUserID {
		c.JSON(http.StatusConflict, gin.H{"error": "Ownership has changed since this transfer — reversal is no longer possible"})
		return
	}

	// Update server owner back to original
	if _, err := tx.ExecContext(ctx, `UPDATE servers SET owner_id = $1 WHERE id = $2`, fromUserID, serverID); err != nil {
		h.log.Error("Failed to update server owner for reversal", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReverseTransfer})
		return
	}

	// Swap legacy roles — verify both users are still members
	resFrom, err := tx.ExecContext(ctx, `UPDATE server_members SET role = 'owner' WHERE server_id = $1 AND user_id = $2`, serverID, fromUserID)
	if err != nil {
		h.log.Error("Failed to update from_user role for reversal", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReverseTransfer})
		return
	}
	if n, _ := resFrom.RowsAffected(); n == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Original owner is no longer a member of this server"})
		return
	}
	resTo, err := tx.ExecContext(ctx, `UPDATE server_members SET role = 'member' WHERE server_id = $1 AND user_id = $2`, serverID, toUserID)
	if err != nil {
		h.log.Error("Failed to update to_user role for reversal", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReverseTransfer})
		return
	}
	if n, _ := resTo.RowsAffected(); n == 0 {
		h.log.Warn("Transfer recipient no longer a member during reversal", "to_user_id", toUserID, "server_id", serverID)
	}

	// Mark transfer as reversed
	if _, err := tx.ExecContext(ctx, `UPDATE ownership_transfers SET status = 'reversed', reversed_at = NOW() WHERE id = $1`, transferID); err != nil {
		h.log.Error("Failed to mark transfer as reversed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReverseTransfer})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit reversal", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReverseTransfer})
		return
	}

	// Invalidate permission cache for both users
	bgCtx := context.Background()
	_ = h.cache.Invalidate(bgCtx, serverID, fromUserID)
	_ = h.cache.Invalidate(bgCtx, serverID, toUserID)

	// Broadcast
	if serverUUID, err := uuid.Parse(serverID); err == nil {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "ownership_transfer_reversed",
			Data: map[string]interface{}{
				keyServerID: serverID,
				"owner_id":  fromUserID,
			},
		})
	}

	// Audit log
	if err := h.audit.Log(ctx, serverID, &userID, "ownership_transfer_reversed", keyServer, &serverID, map[string]interface{}{
		keyTransferID:   transferID,
		"reversed_from": toUserID,
	}); err != nil {
		h.log.Error("Failed to write audit log for ownership_transfer_reversed", "error", err, "server_id", serverID)
	}

	c.JSON(http.StatusOK, gin.H{
		keyMessage: "Ownership transfer reversed",
		"owner_id": fromUserID,
	})
}

// Sentinel errors for executeTransfer so callers can map to appropriate HTTP status codes.
var (
	errTransferAlreadyCompleted = fmt.Errorf("transfer already completed or cancelled")
	errFromUserNotMember        = fmt.Errorf("from_user is no longer a member")
	errToUserNotMember          = fmt.Errorf("to_user is no longer a member")
)

// executeTransfer atomically transfers ownership from one user to another.
// Lock order: ownership_transfers → servers → server_members (matches cleanup job).
func (h *Handler) executeTransfer(ctx context.Context, serverID, transferID, fromUserID, toUserID string) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Step 1: Claim the transfer row FIRST (consistent lock order with cleanup job)
	res, err := tx.ExecContext(ctx, `
		UPDATE ownership_transfers
		SET status = 'completed', completed_at = NOW()
		WHERE id = $1 AND status = 'pending'
	`, transferID)
	if err != nil {
		return fmt.Errorf("mark transfer completed: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return errTransferAlreadyCompleted
	}

	// Step 2: Update server owner
	if _, err := tx.ExecContext(ctx, `UPDATE servers SET owner_id = $1 WHERE id = $2`, toUserID, serverID); err != nil {
		return fmt.Errorf("update server owner: %w", err)
	}

	// Step 3: Swap legacy roles — verify both users are still members
	resFrom, err := tx.ExecContext(ctx, `UPDATE server_members SET role = 'member' WHERE server_id = $1 AND user_id = $2`, serverID, fromUserID)
	if err != nil {
		return fmt.Errorf("update from_user role: %w", err)
	}
	if n, _ := resFrom.RowsAffected(); n == 0 {
		return errFromUserNotMember
	}
	resTo, err := tx.ExecContext(ctx, `UPDATE server_members SET role = 'owner' WHERE server_id = $1 AND user_id = $2`, serverID, toUserID)
	if err != nil {
		return fmt.Errorf("update to_user role: %w", err)
	}
	if n, _ := resTo.RowsAffected(); n == 0 {
		return errToUserNotMember
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Invalidate permission cache for both users
	bgCtx := context.Background()
	_ = h.cache.Invalidate(bgCtx, serverID, fromUserID)
	_ = h.cache.Invalidate(bgCtx, serverID, toUserID)

	// Broadcast
	if serverUUID, err := uuid.Parse(serverID); err == nil {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "ownership_transferred",
			Data: map[string]interface{}{
				keyServerID:    serverID,
				"old_owner_id": fromUserID,
				"new_owner_id": toUserID,
			},
		})
	}

	// Audit log
	if err := h.audit.Log(bgCtx, serverID, nil, "ownership_transferred", keyServer, &serverID, map[string]interface{}{
		keyFromUserID: fromUserID,
		keyToUserID:   toUserID,
	}); err != nil {
		h.log.Error("Failed to write audit log for ownership_transferred", "error", err, "server_id", serverID)
	}

	return nil
}

// verifyPassword checks the user's password against the stored hash.
// On failure, sends an error response to the gin context and returns an error.
func (h *Handler) verifyPassword(c *gin.Context, userID, password string) error {
	var passwordHash string
	err := h.db.QueryRowContext(c.Request.Context(), `SELECT password_hash FROM users WHERE id = $1`, userID).Scan(&passwordHash)
	if err != nil {
		h.log.Error("Failed to fetch password hash", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyPassword})
		return err
	}

	match, err := auth.VerifyPassword(password, passwordHash)
	if err != nil {
		h.log.Error("Failed to verify password", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyPassword})
		return err
	}
	if !match {
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid password"})
		return fmt.Errorf("password mismatch")
	}

	return nil
}

// verifyMFA checks MFA if enabled for the user.
// On failure, sends an error response to the gin context and returns an error.
func (h *Handler) verifyMFA(c *gin.Context, userID, mfaCode string) error {
	ctx := c.Request.Context()
	if !h.mfaVerifier.IsEnabled(ctx, userID) {
		return nil
	}

	if mfaCode == "" {
		methods, _ := h.mfaVerifier.GetEnabledMethods(ctx, userID)
		c.JSON(http.StatusForbidden, gin.H{
			"error":        "MFA verification required",
			"mfa_required": true,
			"methods":      methods,
		})
		return fmt.Errorf("MFA required")
	}

	valid, err := h.mfaVerifier.VerifyCode(ctx, userID, mfaCode)
	if err != nil {
		h.log.Error("Failed to verify MFA code", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify MFA code"})
		return err
	}
	if !valid {
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid MFA code"})
		return fmt.Errorf("invalid MFA code")
	}

	return nil
}

// sendTransferEmail sends an ownership transfer notification to the current owner.
// Failures are logged but do not block the transfer.
func (h *Handler) sendTransferEmail(ctx context.Context, ownerID, serverID, targetUserID, reversalToken string) {
	// Fetch owner's email
	var ownerEmail string
	err := h.db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1`, ownerID).Scan(&ownerEmail)
	if err != nil {
		h.log.Error("Failed to fetch owner email for transfer notification", "error", err)
		return
	}

	// Fetch server name
	var serverName string
	err = h.db.QueryRowContext(ctx, `SELECT name FROM servers WHERE id = $1`, serverID).Scan(&serverName)
	if err != nil {
		h.log.Error("Failed to fetch server name for transfer notification", "error", err)
		return
	}

	// Fetch target username
	var targetUsername string
	err = h.db.QueryRowContext(ctx, `SELECT username FROM users WHERE id = $1`, targetUserID).Scan(&targetUsername)
	if err != nil {
		h.log.Error("Failed to fetch target username for transfer notification", "error", err)
		return
	}

	if err := h.emailSvc.SendOwnershipTransferNotification(ownerEmail, serverName, targetUsername, reversalToken); err != nil {
		h.log.Error("Failed to send ownership transfer email", "error", err, "to", ownerEmail)
	}
}
