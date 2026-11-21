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
		SELECT ma.message_id, mf.id, mf.file_type, mf.mime_type, mf.file_size
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

// LoadDMAttachmentsForMessages batch-loads attachment summaries for a set of DM
// messages. Returns a map from message ID to its attachment summaries. Avoids
// N+1 queries. It is the one DM attachment query: the DM message list in
// internal/dm and the DM pin list in this package both read through it.
func LoadDMAttachmentsForMessages(db *sql.DB, messageIDs []string) (map[string][]models.AttachmentSummary, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}

	rows, err := db.Query(`
		SELECT ma.message_id, mf.id, mf.file_type, mf.mime_type, mf.file_size
		FROM dm_message_attachments ma
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
// (message_id, file id, file_type, mime_type, file_size) and groups the
// summaries by message ID, closing rows before returning. Shared by the
// channel-message and DM loaders above, whose queries differ only by
// attachment table (message_attachments vs dm_message_attachments) — each
// keeps its SQL as a literal so no query text is ever built dynamically.
func ScanAttachmentSummaries(rows *sql.Rows) (map[string][]models.AttachmentSummary, error) {
	defer func() { _ = rows.Close() }()

	result := make(map[string][]models.AttachmentSummary)
	for rows.Next() {
		var msgID string
		var a models.AttachmentSummary
		if err := rows.Scan(&msgID, &a.ID, &a.FileType, &a.MimeType, &a.FileSize); err != nil {
			return nil, err
		}
		result[msgID] = append(result[msgID], a)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return result, nil
}
