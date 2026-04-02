// Package media provides HTTP handlers for media file upload, download, and proxy access.
// It implements a two-tier access model:
//
//   - Tier 1 (Authenticated): Profile images, server icons/banners, emojis, sounds.
//     Server-readable, processed on upload (resize, re-encode). Served via proxy
//     endpoints that validate JWT auth and context membership.
//
//   - Tier 2 (E2EE): Chat attachments in encrypted channels/DMs. Client-side encrypted
//     before upload and stored as opaque blobs; downloads are served via an authenticated
//     proxy endpoint that enforces membership checks. Server never sees plaintext.
package media

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	errMsgAccessDenied       = "Access denied"
	errMsgFailedVerifyAccess = "Failed to verify access"
	errMsgFailedVerifyPerms  = "Failed to verify permissions"
	purposeDMIcon            = "dm-icon"
)

// ObjectStore defines the storage operations required by the media handler.
// This interface is satisfied by *storage.Client and can be mocked for testing.
type ObjectStore interface {
	PutObject(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error
	GetObject(ctx context.Context, key string) (io.ReadCloser, string, error)
	PresignedGetURL(ctx context.Context, key string, expires time.Duration) (string, error)
	DeleteObject(ctx context.Context, key string) error
}

// Tier 1 profile image dimension limits (output size after processing).
// Exported for use by the migrate-media CLI tool.
const (
	AvatarMaxDim = 512  // Avatars resize to fit 512x512
	BannerMaxW   = 1500 // Banners resize to fit 1500x500
	BannerMaxH   = 500
	IconMaxDim   = 512 // Server icons resize to fit 512x512
)

// Tier 1 profile image raw upload limits (before processing).
// These are purpose-specific, not type-specific — kept small because
// the server resizes everything down anyway.
const (
	avatarMaxUpload = 5 * 1024 * 1024  // 5 MB
	bannerMaxUpload = 10 * 1024 * 1024 // 10 MB
	iconMaxUpload   = 5 * 1024 * 1024  // 5 MB
)

// Allowed MIME types for Tier 1 image uploads
var allowedImageTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// FileType classifies uploaded files for client-side rendering hints.
type FileType string

// File type constants for media classification.
const (
	FileTypePhoto    FileType = "photo"
	FileTypeAnimated FileType = "animated"
	FileTypeVideo    FileType = "video"
	FileTypeAudio    FileType = "audio"
	FileTypeFile     FileType = "file"
)

// MediaTier distinguishes access control models.
const (
	MediaTierAuthenticated = 1 // Tier 1: auth-gated, server-readable
	MediaTierE2EE          = 2 // Tier 2: E2EE, control-plane proxied access
)

// Handler provides HTTP handlers for media operations.
type Handler struct {
	db       *sql.DB
	store    ObjectStore
	log      *logger.Logger
	cfg      *config.Config
	resolver *rbac.Resolver
}

// NewHandler creates a new media handler.
func NewHandler(db *sql.DB, store ObjectStore, log *logger.Logger, cfg *config.Config, resolver *rbac.Resolver) *Handler {
	return &Handler{
		db:       db,
		store:    store,
		log:      log,
		cfg:      cfg,
		resolver: resolver,
	}
}

// UploadAvatar handles avatar image uploads.
// POST /api/v1/media/upload/avatar
// Accepts multipart/form-data with a "file" field.
// Processes the image (resize to 512x512, re-encode) and stores in object storage.
// Returns the storage key for use in profile updates.
func (h *Handler) UploadAvatar(c *gin.Context) {
	userID := c.GetString("user_id")
	h.handleTier1Upload(c, userID, "avatar", avatarMaxUpload, AvatarMaxDim, AvatarMaxDim)
}

// UploadBanner handles banner/header image uploads.
// POST /api/v1/media/upload/banner
func (h *Handler) UploadBanner(c *gin.Context) {
	userID := c.GetString("user_id")
	h.handleTier1Upload(c, userID, "banner", bannerMaxUpload, BannerMaxW, BannerMaxH)
}

// UploadServerIcon handles server icon uploads.
// POST /api/v1/media/upload/server-icon
func (h *Handler) UploadServerIcon(c *gin.Context) {
	userID := c.GetString("user_id")
	h.handleTier1Upload(c, userID, "server-icon", iconMaxUpload, IconMaxDim, IconMaxDim)
}

// UploadServerBanner handles server banner uploads.
// POST /api/v1/media/upload/server-banner
func (h *Handler) UploadServerBanner(c *gin.Context) {
	userID := c.GetString("user_id")
	h.handleTier1Upload(c, userID, "server-banner", bannerMaxUpload, BannerMaxW, BannerMaxH)
}

// UploadDMIcon handles group DM icon uploads.
// POST /api/v1/media/upload/dm-icon
func (h *Handler) UploadDMIcon(c *gin.Context) {
	userID := c.GetString("user_id")
	h.handleTier1Upload(c, userID, purposeDMIcon, iconMaxUpload, IconMaxDim, IconMaxDim)
}

// UploadAttachment handles E2EE file uploads for chat attachments (Tier 2).
// POST /api/v1/media/upload/attachment
// The file body is pre-encrypted ciphertext — the server stores it as-is.
func (h *Handler) UploadAttachment(c *gin.Context) {
	userID := c.GetString("user_id")

	// Cap request body size before multipart parsing to prevent memory/disk DoS
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.cfg.UploadMaxSize+4096) // +4KB for multipart headers

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		if strings.Contains(err.Error(), "http: request body too large") {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error":    fmt.Sprintf("File exceeds maximum upload size of %d bytes", h.cfg.UploadMaxSize),
				"max_size": h.cfg.UploadMaxSize,
			})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing file in request"})
		return
	}
	defer func() { _ = file.Close() }()

	// Enforce global upload limit (belt-and-suspenders with MaxBytesReader)
	if header.Size > h.cfg.UploadMaxSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error":    fmt.Sprintf("File exceeds maximum upload size of %d bytes", h.cfg.UploadMaxSize),
			"max_size": h.cfg.UploadMaxSize,
		})
		return
	}

	// Client declares the file type for rendering hints (server can't verify — it's ciphertext)
	fileType := FileType(c.PostForm("file_type"))
	if !isValidFileType(fileType) {
		fileType = FileTypeFile
	}

	// Client provides the MIME type (original, pre-encryption) for rendering hints
	mimeType := c.PostForm("mime_type")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	// Key version (CSK epoch) for forward secrecy — required for E2EE attachments
	keyVersionStr := c.PostForm("key_version")
	keyVersion := 1
	if keyVersionStr != "" {
		if v, err := fmt.Sscanf(keyVersionStr, "%d", &keyVersion); err != nil || v != 1 || keyVersion < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key_version must be a positive integer"})
			return
		}
	}

	// Context (channel_id XOR conversation_id) for access control
	channelID := c.PostForm("channel_id")
	conversationID := c.PostForm("conversation_id")

	if channelID == "" && conversationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Either channel_id or conversation_id is required for attachments"})
		return
	}
	if channelID != "" && conversationID != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Exactly one of channel_id or conversation_id must be provided"})
		return
	}

	// Validate UUID format and verify membership before storing
	if channelID != "" {
		if _, err := uuid.Parse(channelID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel_id"})
			return
		}
		if !h.userHasChannelAccess(c, userID, channelID) {
			return
		}
		if !h.checkAttachPermission(c, userID, channelID) {
			return
		}
	}
	if conversationID != "" {
		if _, err := uuid.Parse(conversationID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation_id"})
			return
		}
		if !h.userHasDMAccess(c, userID, conversationID) {
			return
		}
	}

	fileID := uuid.New().String()
	storageKey := fmt.Sprintf("attachments/%s", fileID)

	// Store the encrypted blob as-is
	if err := h.store.PutObject(c.Request.Context(), storageKey, file, header.Size, "application/octet-stream"); err != nil {
		h.log.Error("Failed to store attachment", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store file"})
		return
	}

	// Record metadata in database
	var chID, convID interface{}
	if channelID != "" {
		chID = channelID
	}
	if conversationID != "" {
		convID = conversationID
	}

	insertQuery := `
		INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key,
		                         is_encrypted, key_version, channel_id, conversation_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, TRUE, $8, $9, $10, NOW())
	`
	_, err = h.db.Exec(insertQuery, fileID, userID, string(fileType), MediaTierE2EE,
		mimeType, header.Size, storageKey, keyVersion, chID, convID)
	if err != nil {
		// Clean up the stored object on DB failure
		_ = h.store.DeleteObject(c.Request.Context(), storageKey)
		h.log.Error("Failed to record attachment metadata", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record file metadata"})
		return
	}

	h.log.Info("Attachment uploaded", "file_id", fileID, "user_id", userID, "size", header.Size, "type", fileType)

	c.JSON(http.StatusCreated, gin.H{
		"file_id":     fileID,
		"storage_key": storageKey,
		"file_type":   fileType,
		"file_size":   header.Size,
	})
}

// DownloadAttachment proxies an E2EE attachment download through the control plane.
// GET /api/v1/media/attachments/:file_id
// Validates that the requesting user has access to the channel/conversation,
// then streams the encrypted blob from MinIO to the client.
//
// This is a proxy (not a presigned URL redirect) because MinIO is only reachable
// within the Docker network — clients cannot reach minio:9000 directly.
func (h *Handler) DownloadAttachment(c *gin.Context) {
	userID := c.GetString("user_id")
	fileID := c.Param("file_id")

	if _, err := uuid.Parse(fileID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid file ID"})
		return
	}

	// Fetch file metadata and verify access
	var storageKey, mimeType string
	var fileSize int64
	var channelID, conversationID *string

	query := `SELECT storage_key, mime_type, file_size, channel_id, conversation_id FROM media_files
	          WHERE id = $1 AND deleted_at IS NULL AND media_tier = 2 AND is_encrypted = TRUE`
	err := h.db.QueryRow(query, fileID).Scan(&storageKey, &mimeType, &fileSize, &channelID, &conversationID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch file metadata", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch file"})
		return
	}

	// Verify membership in the associated channel or conversation
	if channelID != nil {
		if !h.userHasChannelAccess(c, userID, *channelID) {
			return // response already sent
		}
	} else if conversationID != nil {
		if !h.userHasDMAccess(c, userID, *conversationID) {
			return
		}
	} else {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgAccessDenied})
		return
	}

	// Stream the encrypted blob from MinIO to the client
	obj, contentType, err := h.store.GetObject(c.Request.Context(), storageKey)
	if err != nil {
		if strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), "not found") {
			c.JSON(http.StatusNotFound, gin.H{"error": "File not found in storage"})
			return
		}
		h.log.Error("Failed to fetch attachment from storage", "error", err, "file_id", fileID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to download file"})
		return
	}
	defer func() { _ = obj.Close() }()

	// E2EE attachments are opaque ciphertext — use stored MIME type hint for the
	// client but serve as octet-stream since the content is encrypted
	_ = contentType // from storage (application/octet-stream)
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Length", fmt.Sprintf("%d", fileSize))
	c.Header("X-File-Mime-Type", mimeType) // original MIME type hint for client-side decryption
	c.Header("Cache-Control", "private, no-store")
	c.Status(http.StatusOK)

	if _, err := io.Copy(c.Writer, obj); err != nil {
		h.log.Warn("Failed to stream attachment to client", "error", err, "file_id", fileID)
	}
}

// ProxyAvatar serves a user's avatar image through the control plane.
// GET /api/v1/media/avatars/:user_id
// Requires valid JWT. Sets cache headers for browser caching.
func (h *Handler) ProxyAvatar(c *gin.Context) {
	targetUserID := c.Param("user_id")
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	h.proxyTier1Media(c, fmt.Sprintf("avatars/%s", targetUserID))
}

// ProxyBanner serves a user's banner/header image through the control plane.
// GET /api/v1/media/banners/:user_id
func (h *Handler) ProxyBanner(c *gin.Context) {
	targetUserID := c.Param("user_id")
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	h.proxyTier1Media(c, fmt.Sprintf("banners/%s", targetUserID))
}

// ProxyServerIcon serves a server's icon through the control plane.
// GET /api/v1/media/server-icons/:server_id
// Requires JWT + server membership.
func (h *Handler) ProxyServerIcon(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("server_id")
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}

	if !h.userIsServerMember(c, userID, serverID) {
		return
	}

	h.proxyTier1Media(c, fmt.Sprintf("server-icons/%s", serverID))
}

// ProxyServerBanner serves a server's banner through the control plane.
// GET /api/v1/media/server-banners/:server_id
func (h *Handler) ProxyServerBanner(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("server_id")
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}

	if !h.userIsServerMember(c, userID, serverID) {
		return
	}

	h.proxyTier1Media(c, fmt.Sprintf("server-banners/%s", serverID))
}

// ProxyDMIcon serves a group DM's icon through the control plane.
// GET /api/v1/media/dm-icons/:conversationId
// Requires JWT + DM membership.
func (h *Handler) ProxyDMIcon(c *gin.Context) {
	userID := c.GetString("user_id")
	conversationID := c.Param("conversationId")
	if _, err := uuid.Parse(conversationID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation ID"})
		return
	}

	if !h.userHasDMAccess(c, userID, conversationID) {
		return
	}

	h.proxyTier1Media(c, fmt.Sprintf("dm-icons/%s", conversationID))
}

// DeleteMedia soft-deletes a media file and removes it from object storage.
// DELETE /api/v1/media/:file_id
// Only the uploader can delete their own Tier 2 (attachment) files.
// Tier 1 assets (avatars, banners, server icons) are managed via profile/server
// update endpoints and cannot be deleted directly.
func (h *Handler) DeleteMedia(c *gin.Context) {
	userID := c.GetString("user_id")
	fileID := c.Param("file_id")

	if _, err := uuid.Parse(fileID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid file ID"})
		return
	}

	// Verify ownership, tier 2 only, and get storage key
	var storageKey string
	query := `SELECT storage_key FROM media_files WHERE id = $1 AND uploader_id = $2 AND media_tier = 2 AND deleted_at IS NULL`
	err := h.db.QueryRow(query, fileID, userID).Scan(&storageKey)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch file for deletion", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete file"})
		return
	}

	// Soft-delete in DB
	_, err = h.db.Exec(`UPDATE media_files SET deleted_at = NOW() WHERE id = $1`, fileID)
	if err != nil {
		h.log.Error("Failed to soft-delete file", "error", err, "file_id", fileID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete file"})
		return
	}

	// Remove from object storage
	if err := h.store.DeleteObject(c.Request.Context(), storageKey); err != nil {
		h.log.Warn("Failed to delete object from storage (orphaned)", "error", err, "key", storageKey)
	}

	h.log.Info("Media file deleted", "file_id", fileID, "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// --- Internal helpers ---

// handleTier1Upload processes a Tier 1 (authenticated) image upload.
// It validates the file, processes (resize + re-encode), stores in MinIO,
// and records metadata in the database.
func (h *Handler) handleTier1Upload(c *gin.Context, userID, purpose string, maxSize int64, maxW, maxH int) {
	// Cap request body size before multipart parsing to prevent memory/disk DoS
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxSize+4096) // +4KB for multipart headers

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		if strings.Contains(err.Error(), "http: request body too large") {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error":    fmt.Sprintf("File exceeds maximum size of %d bytes", maxSize),
				"max_size": maxSize,
			})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing file in request"})
		return
	}
	defer func() { _ = file.Close() }()

	if header.Size > maxSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error":    fmt.Sprintf("File exceeds maximum size of %d bytes", maxSize),
			"max_size": maxSize,
		})
		return
	}

	// For server uploads, validate server_id and check manage_server permission BEFORE processing
	var serverID string
	if purpose == "server-icon" || purpose == "server-banner" {
		serverID = c.PostForm("server_id")
		if serverID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "server_id is required"})
			return
		}
		if _, err := uuid.Parse(serverID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server_id"})
			return
		}
		if !h.userCanManageServer(c, userID, serverID) {
			return // 403 already sent
		}
	}

	// For DM icon uploads, validate conversation_id and check admin role BEFORE processing
	var conversationID string
	if purpose == purposeDMIcon {
		conversationID = c.PostForm("conversation_id")
		if conversationID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "conversation_id is required"})
			return
		}
		if _, err := uuid.Parse(conversationID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation_id"})
			return
		}
		if !h.userIsDMAdmin(c, userID, conversationID) {
			return // 403 already sent
		}
	}

	// Validate MIME type
	contentType := header.Header.Get("Content-Type")
	if contentType == "" || !allowedImageTypes[contentType] {
		// Sniff from content if header is missing/wrong
		buf := make([]byte, 512)
		n, readErr := file.Read(buf)
		if readErr != nil && readErr != io.EOF {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read file"})
			return
		}
		if n == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Empty file"})
			return
		}
		contentType = http.DetectContentType(buf[:n])
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process file"})
			return
		}
	}

	if !allowedImageTypes[contentType] {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":         "Invalid image type. Allowed: JPEG, PNG, GIF, WebP",
			"allowed_types": []string{"image/jpeg", "image/png", "image/gif", "image/webp"},
		})
		return
	}

	// Process (resize + re-encode)
	// Use PNG output for avatars/icons (may have transparency), JPEG for banners
	var processed *ProcessedImage
	if purpose == "banner" || purpose == "server-banner" {
		processed, err = ProcessImage(file, maxW, maxH)
	} else {
		processed, err = ProcessImagePNG(file, maxW, maxH)
	}
	if err != nil {
		h.log.Error("Failed to process image", "error", err, "user_id", userID, "purpose", purpose)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to process image. Ensure the file is a valid image."})
		return
	}

	// Determine storage key based on purpose
	var storageKey string
	switch purpose {
	case "avatar":
		storageKey = fmt.Sprintf("avatars/%s", userID)
	case "banner":
		storageKey = fmt.Sprintf("banners/%s", userID)
	case "server-icon":
		storageKey = fmt.Sprintf("server-icons/%s", serverID)
	case "server-banner":
		storageKey = fmt.Sprintf("server-banners/%s", serverID)
	case purposeDMIcon:
		storageKey = fmt.Sprintf("dm-icons/%s", conversationID)
	}

	// Upload to object storage
	reader := bytes.NewReader(processed.Data)
	if err := h.store.PutObject(c.Request.Context(), storageKey, reader, int64(len(processed.Data)), processed.ContentType); err != nil {
		h.log.Error("Failed to store processed image", "error", err, "user_id", userID, "purpose", purpose)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store image"})
		return
	}

	// Record in media_files table
	fileID := uuid.New().String()
	fileType := FileTypePhoto

	insertQuery := `
		INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (storage_key) WHERE deleted_at IS NULL
		DO UPDATE SET uploader_id = EXCLUDED.uploader_id, file_size = EXCLUDED.file_size, mime_type = EXCLUDED.mime_type, updated_at = NOW()
		RETURNING id
	`
	err = h.db.QueryRow(insertQuery, fileID, userID, string(fileType), MediaTierAuthenticated,
		processed.ContentType, len(processed.Data), storageKey).Scan(&fileID)
	if err != nil {
		h.log.Error("Failed to record media metadata", "error", err, "user_id", userID)
		// Clean up the just-uploaded object to avoid orphans in storage
		if delErr := h.store.DeleteObject(c.Request.Context(), storageKey); delErr != nil {
			h.log.Error("Failed to delete orphaned media object", "error", delErr, "storage_key", storageKey)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record media metadata"})
		return
	}

	// For DM icon uploads, update the conversation's icon_url
	if purpose == purposeDMIcon {
		proxyURL := fmt.Sprintf("/api/v1/media/dm-icons/%s", conversationID)
		if _, dbErr := h.db.Exec(
			`UPDATE dm_conversations SET icon_url = $1, updated_at = NOW() WHERE id = $2`,
			proxyURL, conversationID,
		); dbErr != nil {
			h.log.Error("Failed to update DM icon URL", "error", dbErr, "conversation_id", conversationID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update group icon"})
			return
		}
	}

	h.log.Info("Tier 1 image uploaded", "purpose", purpose, "user_id", userID,
		"size_original", header.Size, "size_processed", len(processed.Data),
		"dimensions", fmt.Sprintf("%dx%d", processed.Width, processed.Height))

	c.JSON(http.StatusCreated, gin.H{
		"file_id":     fileID,
		"storage_key": storageKey,
		"url":         fmt.Sprintf("/api/v1/media/%s", storageKey),
		"file_size":   len(processed.Data),
		"width":       processed.Width,
		"height":      processed.Height,
	})
}

// proxyTier1Media fetches an object from MinIO and streams it to the client
// with appropriate cache headers. Used for avatars, banners, server icons.
func (h *Handler) proxyTier1Media(c *gin.Context, key string) {
	obj, contentType, err := h.store.GetObject(c.Request.Context(), key)
	if err != nil {
		if strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), "not found") {
			c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
			return
		}
		h.log.Error("Failed to fetch media from storage", "error", err, "key", key)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	defer func() { _ = obj.Close() }()

	// Cache for 1 hour, allow revalidation
	c.Header("Cache-Control", "private, max-age=3600, must-revalidate")
	c.Header("Content-Type", contentType)
	c.Status(http.StatusOK)

	if _, err := io.Copy(c.Writer, obj); err != nil {
		h.log.Warn("Failed to stream media to client", "error", err, "key", key)
	}
}

// userHasChannelAccess checks if a user is a member of the server that owns a channel.
func (h *Handler) userHasChannelAccess(c *gin.Context, userID, channelID string) bool {
	query := `
		SELECT EXISTS(
			SELECT 1 FROM channels ch
			JOIN server_members sm ON sm.server_id = ch.server_id
			WHERE ch.id = $1 AND sm.user_id = $2
		)
	`
	var hasAccess bool
	if err := h.db.QueryRow(query, channelID, userID).Scan(&hasAccess); err != nil {
		h.log.Error("Failed to check channel access", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyAccess})
		return false
	}
	if !hasAccess {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgAccessDenied})
		return false
	}
	return true
}

// userHasDMAccess checks if a user is a participant in a DM conversation.
func (h *Handler) userHasDMAccess(c *gin.Context, userID, conversationID string) bool {
	query := `SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`
	var hasAccess bool
	if err := h.db.QueryRow(query, conversationID, userID).Scan(&hasAccess); err != nil {
		h.log.Error("Failed to check DM access", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyAccess})
		return false
	}
	if !hasAccess {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgAccessDenied})
		return false
	}
	return true
}

// userIsDMAdmin checks if a user is an admin participant in a group DM conversation.
func (h *Handler) userIsDMAdmin(c *gin.Context, userID, conversationID string) bool {
	var role string
	err := h.db.QueryRow(
		`SELECT dp.role FROM dm_participants dp
		 JOIN dm_conversations dc ON dc.id = dp.conversation_id
		 WHERE dp.conversation_id = $1 AND dp.user_id = $2 AND dc.is_group = TRUE`,
		conversationID, userID,
	).Scan(&role)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusForbidden, gin.H{"error": "Not a group DM participant"})
		} else {
			h.log.Error("Failed to check DM admin", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyAccess})
		}
		return false
	}
	if role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only group admins can upload icons"})
		return false
	}
	return true
}

// userIsServerMember checks if a user is a member of a server.
func (h *Handler) userIsServerMember(c *gin.Context, userID, serverID string) bool {
	query := `SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`
	var isMember bool
	if err := h.db.QueryRow(query, serverID, userID).Scan(&isMember); err != nil {
		h.log.Error("Failed to check server membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyAccess})
		return false
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return false
	}
	return true
}

// checkAttachPermission checks if a user has the ATTACH_FILES RBAC permission for a channel.
func (h *Handler) checkAttachPermission(c *gin.Context, userID, channelID string) bool {
	// Look up server_id for this channel
	var serverID string
	err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT server_id FROM channels WHERE id = $1`, channelID,
	).Scan(&serverID)
	if err != nil {
		h.log.Error("Failed to look up channel server", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyPerms})
		return false
	}
	if h.resolver == nil {
		// Fallback to membership-only if no RBAC resolver configured (e.g. tests)
		return h.userIsServerMember(c, userID, serverID)
	}
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermAttachFiles)
	if err != nil {
		h.log.Error("Failed to check attach permission", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyPerms})
		return false
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": "You don't have permission to attach files in this channel"})
		return false
	}
	return true
}

// userCanManageServer checks if a user has the manage_server RBAC permission.
// Used for server icon/banner uploads to match the same gate as UpdateServer.
func (h *Handler) userCanManageServer(c *gin.Context, userID, serverID string) bool {
	if h.resolver == nil {
		// Fallback to membership-only if no RBAC resolver configured (e.g. tests)
		return h.userIsServerMember(c, userID, serverID)
	}
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermManageServer)
	if err != nil {
		h.log.Error("Failed to check server permission", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyPerms})
		return false
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": "Insufficient permissions"})
		return false
	}
	return true
}

func isValidFileType(ft FileType) bool {
	switch ft {
	case FileTypePhoto, FileTypeAnimated, FileTypeVideo, FileTypeAudio, FileTypeFile:
		return true
	}
	return false
}
