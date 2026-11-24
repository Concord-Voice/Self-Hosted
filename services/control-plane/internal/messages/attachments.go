package messages

import (
	"database/sql"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/lib/pq"
)

// loadAttachmentsForMessages batch-loads attachment summaries for a set of messages.
// Returns a map from message ID to its attachment summaries. Avoids N+1 queries.
func loadAttachmentsForMessages(db *sql.DB, messageIDs []string) (map[string][]models.AttachmentSummary, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}

	rows, err := db.Query(`
		SELECT ma.message_id, mf.id, mf.file_type, mf.mime_type, mf.file_size,
		       mf.uploader_id, mf.key_version, mf.channel_id, mf.conversation_id, mf.e2ee_format
		FROM message_attachments ma
		INNER JOIN media_files mf ON ma.file_id = mf.id
		WHERE ma.message_id = ANY($1::uuid[])
		  AND mf.deleted_at IS NULL
		ORDER BY ma.message_id, ma.position
	`, pq.Array(messageIDs))
	if err != nil {
		return nil, err
	}
	return ScanAttachmentSummaries(rows)
}

// ScanAttachmentSummaries consumes rows of
// (message_id, file ID/type/MIME/size, candidate uploader/key/scope, e2ee_format)
// and groups the
// summaries by message ID, closing rows before returning. Shared by the
// channel-message loader above and the DM loader in internal/dm, whose
// queries differ only by attachment table (message_attachments vs
// dm_message_attachments) — the SQL stays a per-package literal so no
// query text is ever built dynamically.
func ScanAttachmentSummaries(rows *sql.Rows) (map[string][]models.AttachmentSummary, error) {
	defer func() { _ = rows.Close() }()

	result := make(map[string][]models.AttachmentSummary)
	for rows.Next() {
		var msgID string
		var a models.AttachmentSummary
		if err := rows.Scan(
			&msgID, &a.ID, &a.FileType, &a.MimeType, &a.FileSize,
			&a.UploaderID, &a.KeyVersion, &a.ChannelID, &a.ConversationID, &a.E2EEFormat,
		); err != nil {
			return nil, err
		}
		result[msgID] = append(result[msgID], a)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return result, nil
}
