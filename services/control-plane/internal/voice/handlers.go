// Package voice provides handlers for voice channel state management.
// It exposes REST endpoints for voice join authorization and participant listing,
// and processes NATS events from the media plane to keep the DB and WS hub in sync.
package voice

import (
	"database/sql"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	natsclient "github.com/markdrogersjr/Concord/services/control-plane/pkg/nats"
)

// Handler handles voice-related requests.
type Handler struct {
	db       *sql.DB
	log      *logger.Logger
	hub      *websocket.Hub
	cfg      *config.Config
	resolver *rbac.Resolver
	nats     *natsclient.Client
}

// NewHandler creates a new voice handler.
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub, cfg *config.Config, resolver *rbac.Resolver, nats *natsclient.Client) *Handler {
	return &Handler{
		db:       db,
		log:      log,
		hub:      hub,
		cfg:      cfg,
		resolver: resolver,
		nats:     nats,
	}
}

// Participant represents a user currently in a voice channel.
type Participant struct {
	UserID          string `json:"user_id"`
	Username        string `json:"username"`
	DisplayName     string `json:"display_name,omitempty"`
	AvatarURL       string `json:"avatar_url,omitempty"`
	IsMuted         bool   `json:"is_muted"`
	IsDeafened      bool   `json:"is_deafened"`
	IsVideoOn       bool   `json:"is_video_on"`
	IsScreenSharing bool   `json:"is_screen_sharing"`
	JoinedAt        string `json:"joined_at"`
	ServerMuted     bool   `json:"server_muted"`
	ServerDeafened  bool   `json:"server_deafened"`
}

// GetParticipants returns all users currently in a voice channel.
// GET /channels/:id/voice/participants
func (h *Handler) GetParticipants(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	// Verify the user has access to this channel's server
	var serverID string
	err := h.db.QueryRow(`
		SELECT c.server_id FROM channels c
		INNER JOIN server_members sm ON sm.server_id = c.server_id AND sm.user_id = $2
		WHERE c.id = $1
	`, channelID, userID).Scan(&serverID)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return
	} else if err != nil {
		h.log.Error("Failed to check channel access", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchParticipants})
		return
	}

	// Fetch voice participants with user details and server enforcement flags
	rows, err := h.db.Query(`
		SELECT vp.user_id, u.username, COALESCE(u.display_name, ''), COALESCE(u.avatar_url, ''),
		       vp.is_muted, vp.is_deafened, vp.is_video_on, vp.is_screen_sharing, vp.joined_at,
		       sm.server_muted, sm.server_deafened
		FROM voice_participants vp
		INNER JOIN users u ON u.id = vp.user_id
		INNER JOIN channels c ON c.id = vp.channel_id
		INNER JOIN server_members sm ON sm.server_id = c.server_id AND sm.user_id = vp.user_id
		WHERE vp.channel_id = $1
		ORDER BY vp.joined_at ASC
	`, channelID)
	if err != nil {
		h.log.Error("Failed to query voice participants", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchParticipants})
		return
	}
	defer func() { _ = rows.Close() }()

	participants := []Participant{}
	for rows.Next() {
		var p Participant
		if err := rows.Scan(
			&p.UserID, &p.Username, &p.DisplayName, &p.AvatarURL,
			&p.IsMuted, &p.IsDeafened, &p.IsVideoOn, &p.IsScreenSharing, &p.JoinedAt,
			&p.ServerMuted, &p.ServerDeafened,
		); err != nil {
			h.log.Error("Failed to scan voice participant", "error", err)
			continue
		}
		participants = append(participants, p)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating voice participants", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchParticipants})
		return
	}

	c.JSON(http.StatusOK, gin.H{"participants": participants})
}

// AuthorizeJoin checks that a user can join a voice channel and returns
// media plane connection details.
// POST /channels/:id/voice/join
func (h *Handler) AuthorizeJoin(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	// Fetch channel details + verify membership in one query
	var channelName, channelType, serverID string
	var isEncrypted bool
	var audioQualityTier *string
	err := h.db.QueryRow(`
		SELECT c.id, c.name, c.type, c.server_id, c.is_encrypted, c.audio_quality_tier
		FROM channels c
		INNER JOIN server_members sm ON sm.server_id = c.server_id AND sm.user_id = $2
		WHERE c.id = $1
	`, channelID, userID).Scan(&channelID, &channelName, &channelType, &serverID, &isEncrypted, &audioQualityTier)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": "Channel not found or access denied"})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch channel for voice join", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return
	}

	if channelType != "voice" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Not a voice channel"})
		return
	}

	// Check PermJoinVoice permission (with channelID for SBAC overrides)
	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermJoinVoice)
	if permErr != nil {
		h.log.Error("Failed to check voice permissions", "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// Get effective permissions for this channel (media plane uses this to enforce
	// PermSpeak, PermScreenShare, PermMuteMembers, PermDeafenMembers, PermMoveMembers)
	effectivePerms, permResolveErr := h.resolver.GetEffectivePermissions(c.Request.Context(), serverID, userID, channelID)
	if permResolveErr != nil {
		h.log.Error("Failed to resolve effective voice permissions", "error", permResolveErr, "user_id", userID, "channel_id", channelID, "server_id", serverID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return
	}

	// Query server-enforced mute/deafen flags for this member
	var serverMuted, serverDeafened bool
	if err := h.db.QueryRow(`SELECT server_muted, server_deafened FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, userID).Scan(&serverMuted, &serverDeafened); err != nil {
		h.log.Error("Failed to query server enforcement flags", "error", err, "user_id", userID, "server_id", serverID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return
	}

	h.log.Info("Voice join authorized", "user_id", userID, "channel_id", channelID, "server_id", serverID)

	c.JSON(http.StatusOK, gin.H{
		"allowed":          true,
		"media_server_url": h.cfg.MediaPlaneURL,
		"ice_servers":      h.cfg.ICEServers(userID),
		"permissions":      strconv.FormatInt(int64(effectivePerms), 10),
		"server_muted":     serverMuted,
		"server_deafened":  serverDeafened,
		"channel": gin.H{
			"id":                 channelID,
			"name":               channelName,
			"server_id":          serverID,
			"is_encrypted":       isEncrypted,
			"audio_quality_tier": audioQualityTier,
		},
	})
}

// AuthorizeVoiceAction checks whether a user has permission to perform a voice
// moderation action (mute, deafen, move) on a target user. The media plane
// should call this endpoint before executing moderation commands.
// POST /channels/:id/voice/authorize-action
func (h *Handler) AuthorizeVoiceAction(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	var req struct {
		Action       string `json:"action" binding:"required,oneof=mute deafen move"`
		TargetUserID string `json:"target_user_id" binding:"required,uuid"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Get server ID from channel
	var serverID string
	err := h.db.QueryRow(`
		SELECT c.server_id FROM channels c
		INNER JOIN server_members sm ON sm.server_id = c.server_id AND sm.user_id = $2
		WHERE c.id = $1
	`, channelID, userID).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return
	}
	if err != nil {
		h.log.Error("Failed to check channel access", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthAction})
		return
	}

	// Map action to required permission
	var perm rbac.Permission
	switch req.Action {
	case "mute":
		perm = rbac.PermMuteMembers
	case "deafen":
		perm = rbac.PermDeafenMembers
	case "move":
		perm = rbac.PermMoveMembers
	}

	// Check permission
	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, perm)
	if permErr != nil {
		h.log.Error("Failed to check voice moderation permission", "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthAction})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// Verify target user is a member of this server
	var targetIsMember bool
	if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, req.TargetUserID).Scan(&targetIsMember); err != nil {
		h.log.Error("Failed to check target membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthAction})
		return
	}
	if !targetIsMember {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgTargetNotMember})
		return
	}

	// Hierarchy check: cannot moderate members with equal or higher role
	if hierErr := h.resolver.CheckHierarchy(c.Request.Context(), serverID, userID, req.TargetUserID); hierErr != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgHierarchyViolation})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"allowed":    true,
		"action":     req.Action,
		"target_id":  req.TargetUserID,
		"channel_id": channelID,
	})
}

// --- Server-enforced voice moderation ---

const (
	errMsgInvalidChannelID   = "Invalid channel ID"
	errMsgInvalidServerID    = "Invalid server ID"
	errMsgInvalidUserID      = "Invalid user ID"
	errMsgNotMember          = "Not a member of this server"
	errMsgFetchParticipants  = "Failed to fetch participants"
	errMsgFailedAuthorize    = "Failed to authorize"
	errMsgFailedAuthAction   = "Failed to authorize action"
	errMsgInsufficientPerms  = "Insufficient permissions"
	errMsgFailedCheckPerms   = "Failed to check permissions"
	errMsgFailedCheckMember  = "Failed to check membership"
	errMsgTargetNotMember    = "Target user is not a member of this server"
	errMsgHierarchyViolation = "Cannot moderate a member with equal or higher role position"
	errMsgTargetNotInVoice   = "Target user is not in a voice channel"
	errMsgCannotTargetSelf   = "Cannot target yourself"
)

// voiceModContext holds the validated state from authorizeVoiceMod.
type voiceModContext struct {
	serverID string
	targetID string
}

// authorizeVoiceMod validates params, checks membership, permission, and optionally hierarchy.
// For user-level actions (requireHierarchy=false), it also enforces the self-target guard.
// Returns nil and sends the HTTP error response if any check fails.
func (h *Handler) authorizeVoiceMod(c *gin.Context, perm rbac.Permission, requireHierarchy bool) *voiceModContext {
	actorID := c.GetString("user_id")
	serverID := c.Param("id")
	targetID := c.Param("userId")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return nil
	}
	if _, err := uuid.Parse(targetID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidUserID})
		return nil
	}

	// Always verify actor is a member of the server
	if !h.checkMembership(c, serverID, actorID) {
		return nil
	}

	if !requireHierarchy {
		// User-level: self-target guard
		if actorID == targetID {
			c.JSON(http.StatusBadRequest, gin.H{"error": errMsgCannotTargetSelf})
			return nil
		}
	}

	// Check permission
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, actorID, "", perm)
	if err != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", err, "permission", perm)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPerms})
		return nil
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return nil
	}

	if requireHierarchy && h.resolver.CheckHierarchy(c.Request.Context(), serverID, actorID, targetID) != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgHierarchyViolation})
		return nil
	}

	return &voiceModContext{serverID: serverID, targetID: targetID}
}

// checkMembership verifies that userID is a member of serverID.
// Returns false and sends the HTTP error response if the check fails.
func (h *Handler) checkMembership(c *gin.Context, serverID, userID string) bool {
	var exists bool
	if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID).Scan(&exists); err != nil {
		h.log.Error(errMsgFailedCheckMember, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckMember})
		return false
	}
	if !exists {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return false
	}
	return true
}

// findVoiceChannel queries the voice channel a target is connected to in a server.
// Returns the channel ID (empty if not in voice) and any real DB error.
func (h *Handler) findVoiceChannel(serverID, targetID string) (string, error) {
	var channelID string
	err := h.db.QueryRow(`
		SELECT vp.channel_id FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		WHERE c.server_id = $1 AND vp.user_id = $2
	`, serverID, targetID).Scan(&channelID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return channelID, nil
}

// publishEnforcement publishes a NATS enforcement message if the target is in voice.
func (h *Handler) publishEnforcement(subject, channelID, targetID, action string) {
	if channelID == "" || h.nats == nil {
		return
	}
	if pubErr := h.nats.Publish(subject, map[string]interface{}{
		"channelId": channelID, "userId": targetID, "action": action,
	}); pubErr != nil {
		h.log.Error("Failed to publish NATS enforcement", "error", pubErr, "subject", subject, "action", action)
	}
}

// broadcastVoiceStateUpdate sends a voice_state_update WS event to all server members.
func (h *Handler) broadcastVoiceStateUpdate(serverID, targetID, channelID, action string) {
	serverUUID, _ := uuid.Parse(serverID)
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "voice_state_update",
		Data: map[string]interface{}{
			"action":     action,
			"user_id":    targetID,
			"server_id":  serverID,
			"channel_id": channelID,
		},
	})
}

// enforcementParams groups the per-action strings for applyServerEnforcement.
type enforcementParams struct {
	query       string // SQL UPDATE statement
	natsSubject string // NATS subject to publish on
	natsAction  string // action field in NATS payload
	wsAction    string // action field in WS broadcast
	successMsg  string // HTTP 200 message
	failMsg     string // HTTP 500 / log message
}

// applyServerEnforcement executes the SQL update, finds voice channel, publishes NATS, broadcasts WS.
func (h *Handler) applyServerEnforcement(c *gin.Context, ctx *voiceModContext, p enforcementParams) {
	result, err := h.db.Exec(p.query, ctx.serverID, ctx.targetID)
	if err != nil {
		h.log.Error(p.failMsg, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": p.failMsg})
		return
	}
	rowsAffected, raErr := result.RowsAffected()
	if raErr != nil {
		h.log.Error("Failed to check rows affected", "error", raErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": p.failMsg})
		return
	}
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgTargetNotMember})
		return
	}
	channelID, findErr := h.findVoiceChannel(ctx.serverID, ctx.targetID)
	if findErr != nil {
		h.log.Error("Failed to find voice channel for enforcement", "error", findErr, "server_id", ctx.serverID, "target_id", ctx.targetID)
	}
	h.publishEnforcement(p.natsSubject, channelID, ctx.targetID, p.natsAction)
	h.broadcastVoiceStateUpdate(ctx.serverID, ctx.targetID, channelID, p.wsAction)
	c.JSON(http.StatusOK, gin.H{"message": p.successMsg})
}

// userLevelAction sends a real-time user-level enforcement command via NATS.
func (h *Handler) userLevelAction(c *gin.Context, perm rbac.Permission, natsSubject, natsAction, successMsg, failMsg string) {
	ctx := h.authorizeVoiceMod(c, perm, false)
	if ctx == nil {
		return
	}

	channelID, findErr := h.findVoiceChannel(ctx.serverID, ctx.targetID)
	if findErr != nil {
		h.log.Error("Failed to find voice channel", "error", findErr, "server_id", ctx.serverID, "target_id", ctx.targetID)
	}
	if channelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgTargetNotInVoice})
		return
	}

	if h.nats != nil {
		if pubErr := h.nats.Publish(natsSubject, map[string]interface{}{
			"channelId": channelID, "userId": ctx.targetID, "action": natsAction,
		}); pubErr != nil {
			h.log.Error(failMsg, "error", pubErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": failMsg})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": successMsg})
}

// ServerMute applies a persistent server-level mute to a member.
// POST /servers/:id/voice/:userId/mute
func (h *Handler) ServerMute(c *gin.Context) {
	ctx := h.authorizeVoiceMod(c, rbac.PermMuteMembers, true)
	if ctx == nil {
		return
	}
	h.applyServerEnforcement(c, ctx, enforcementParams{
		query:       `UPDATE server_members SET server_muted = true WHERE server_id = $1 AND user_id = $2`,
		natsSubject: "voice.enforce.mute", natsAction: "mute", wsAction: "server_muted",
		successMsg: "Member server-muted", failMsg: "Failed to mute member",
	})
}

// ServerUnmute removes a persistent server-level mute from a member.
// DELETE /servers/:id/voice/:userId/mute
func (h *Handler) ServerUnmute(c *gin.Context) {
	ctx := h.authorizeVoiceMod(c, rbac.PermMuteMembers, true)
	if ctx == nil {
		return
	}

	// Check if target is server_deafened — cannot unmute without undeafening first
	var serverDeafened bool
	if err := h.db.QueryRow(`SELECT server_deafened FROM server_members WHERE server_id = $1 AND user_id = $2`,
		ctx.serverID, ctx.targetID).Scan(&serverDeafened); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgTargetNotMember})
			return
		}
		h.log.Error("Failed to check deafen state", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to unmute member"})
		return
	}
	if serverDeafened {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot unmute a server-deafened member; undeafen first"})
		return
	}

	// Remove server mute
	if _, err := h.db.Exec(`UPDATE server_members SET server_muted = false WHERE server_id = $1 AND user_id = $2`,
		ctx.serverID, ctx.targetID); err != nil {
		h.log.Error("Failed to server-unmute member", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to unmute member"})
		return
	}

	channelID, findErr := h.findVoiceChannel(ctx.serverID, ctx.targetID)
	if findErr != nil {
		h.log.Error("Failed to find voice channel for unmute enforcement", "error", findErr, "server_id", ctx.serverID, "target_id", ctx.targetID)
	}
	h.publishEnforcement("voice.enforce.mute", channelID, ctx.targetID, "unmute")
	h.broadcastVoiceStateUpdate(ctx.serverID, ctx.targetID, channelID, "server_unmuted")

	c.JSON(http.StatusOK, gin.H{"message": "Member server-unmuted"})
}

// ServerDeafen applies a persistent server-level deafen (implies mute) to a member.
// POST /servers/:id/voice/:userId/deafen
func (h *Handler) ServerDeafen(c *gin.Context) {
	ctx := h.authorizeVoiceMod(c, rbac.PermDeafenMembers, true)
	if ctx == nil {
		return
	}
	h.applyServerEnforcement(c, ctx, enforcementParams{
		query:       `UPDATE server_members SET server_muted = true, server_deafened = true WHERE server_id = $1 AND user_id = $2`,
		natsSubject: "voice.enforce.deafen", natsAction: "deafen", wsAction: "server_deafened",
		successMsg: "Member server-deafened", failMsg: "Failed to deafen member",
	})
}

// ServerUndeafen removes a persistent server-level deafen (and mute) from a member.
// DELETE /servers/:id/voice/:userId/deafen
func (h *Handler) ServerUndeafen(c *gin.Context) {
	ctx := h.authorizeVoiceMod(c, rbac.PermDeafenMembers, true)
	if ctx == nil {
		return
	}
	h.applyServerEnforcement(c, ctx, enforcementParams{
		query:       `UPDATE server_members SET server_deafened = false, server_muted = false WHERE server_id = $1 AND user_id = $2`,
		natsSubject: "voice.enforce.deafen", natsAction: "undeafen", wsAction: "server_undeafened",
		successMsg: "Member server-undeafened", failMsg: "Failed to undeafen member",
	})
}

// UserMute sends a real-time user-level mute command to the media plane.
// No persistent DB state — requires the target to be in voice.
// POST /servers/:id/voice/:userId/user-mute
func (h *Handler) UserMute(c *gin.Context) {
	h.userLevelAction(c, rbac.PermMuteMembers, "voice.user_mute", "mute", "User mute command sent", "Failed to mute user")
}

// UserDeafen sends a real-time user-level deafen command to the media plane.
// No persistent DB state — requires the target to be in voice.
// POST /servers/:id/voice/:userId/user-deafen
func (h *Handler) UserDeafen(c *gin.Context) {
	h.userLevelAction(c, rbac.PermDeafenMembers, "voice.user_deafen", "deafen", "User deafen command sent", "Failed to deafen user")
}
