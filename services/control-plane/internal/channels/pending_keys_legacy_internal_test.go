package channels

import (
	"context"
	"fmt"
)

// legacyPendingKeyRequests is the pending-keys reader as it was before paging
// (VULN-080, VULN-207): one unbounded channel query filtered row by row through
// channelKeyAccess, then one unbounded DM query filtered row by row through
// dmPendingConversationAvailable. It is kept only as the oracle for
// TestPendingKeys_VisibilityMatchesLegacy, which shows that the paged reader
// returns the same rows.
func (h *Handler) legacyPendingKeyRequests(ctx context.Context, callerID string) ([]pendingKeyRequest, error) {
	channelRows, err := h.db.QueryContext(ctx, `
		SELECT DISTINCT pkr.id, pkr.channel_id, pkr.user_id, COALESCE(cid.key_version, active_epoch.key_version), pkr.created_at
		FROM pending_key_requests pkr
		LEFT JOIN channel_initial_key_distributions cid ON cid.channel_id = pkr.channel_id
		CROSS JOIN LATERAL (
			SELECT GREATEST(
				COALESCE(MAX(key_version), 1),
				COALESCE((SELECT MAX(successor_epoch) FROM key_revocations WHERE channel_id = pkr.channel_id), 1)
			) AS key_version
			FROM channel_keys
			WHERE channel_id = pkr.channel_id
		) active_epoch
		INNER JOIN channel_keys ck ON pkr.channel_id = ck.channel_id
			AND ck.user_id = $1
			AND (
				(cid.key_version IS NULL AND ck.key_version = active_epoch.key_version)
				OR (cid.creator_id = $1 AND ck.key_version = cid.key_version)
			)
		ORDER BY pkr.created_at ASC
	`, callerID)
	if err != nil {
		return nil, fmt.Errorf("legacy channel query: %w", err)
	}
	defer func() { _ = channelRows.Close() }()
	candidates := []pendingKeyRequest{}
	for channelRows.Next() {
		var req pendingKeyRequest
		if err := channelRows.Scan(&req.ID, &req.ChannelID, &req.UserID, &req.KeyVersion, &req.CreatedAt); err != nil {
			return nil, fmt.Errorf("legacy channel scan: %w", err)
		}
		candidates = append(candidates, req)
	}
	if err := channelRows.Err(); err != nil {
		return nil, fmt.Errorf("legacy channel iterate: %w", err)
	}

	requests := []pendingKeyRequest{}
	callerCanView := map[string]bool{}
	for _, req := range candidates {
		canView, known := callerCanView[req.ChannelID]
		if !known {
			_, cv, vErr := h.channelKeyAccess(ctx, req.ChannelID, callerID)
			canView = vErr == nil && cv
			callerCanView[req.ChannelID] = canView
		}
		if !canView {
			continue
		}
		if _, cv, vErr := h.channelKeyAccess(ctx, req.ChannelID, req.UserID); vErr != nil || !cv {
			continue
		}
		requests = append(requests, req)
	}

	dmRows, err := h.db.QueryContext(ctx, `
		SELECT dpkr.id, dpkr.conversation_id, dpkr.user_id, dpkr.created_at
		FROM dm_pending_key_requests dpkr
		INNER JOIN dm_channel_keys dck ON dpkr.conversation_id = dck.conversation_id AND dck.user_id = $1
		WHERE dpkr.user_id != $1
		  AND dck.key_version = (
			SELECT MAX(key_version) FROM dm_channel_keys WHERE conversation_id = dpkr.conversation_id
		  )
		ORDER BY dpkr.created_at ASC
	`, callerID)
	if err != nil {
		return nil, fmt.Errorf("legacy dm query: %w", err)
	}
	defer func() { _ = dmRows.Close() }()
	dmCandidates := []pendingKeyRequest{}
	for dmRows.Next() {
		var req pendingKeyRequest
		if err := dmRows.Scan(&req.ID, &req.ChannelID, &req.UserID, &req.CreatedAt); err != nil {
			return nil, fmt.Errorf("legacy dm scan: %w", err)
		}
		dmCandidates = append(dmCandidates, req)
	}
	if err := dmRows.Err(); err != nil {
		return nil, fmt.Errorf("legacy dm iterate: %w", err)
	}
	for _, req := range dmCandidates {
		available, guardErr := h.dmPendingConversationAvailable(ctx, req.ChannelID, callerID)
		if guardErr != nil {
			return nil, fmt.Errorf("legacy dm availability: %w", guardErr)
		}
		if available {
			requests = append(requests, req)
		}
	}
	return requests, nil
}
