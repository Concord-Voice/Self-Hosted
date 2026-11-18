package channels

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/requestid"
)

// The pending-keys reader is paged (VULN-080, VULN-207). One keyset cursor
// runs over both sources in (created_at, source, id) order. Visibility is
// applied to every examined row before it counts toward the page, so rows the
// caller cannot act on never fill a page, and one request examines at most
// pendingKeysScanCap rows. When the cap ends a request with the page not full,
// next_cursor points past the last examined row, so later visible rows are
// still reached. That row can be one the caller cannot see, so the cursor is
// sealed (pendingCursorCodec) and tells the client nothing about it.
const (
	pendingKeysDefaultLimit = 100
	pendingKeysMaxLimit     = 200
	pendingKeysScanCap      = 1000
	pendingKeysBatchSize    = 200
	// A sealed cursor is at most 134 characters: a microsecond created_at,
	// the source and the id, plus a 12-byte nonce and a 16-byte tag.
	pendingCursorMaxLen = 160
	pendingCursorKeyLen = 32 // AES-256

	pendingSourceChannel = "channel"
	pendingSourceDM      = "dm"

	errMsgInvalidPendingKeysPage = "Invalid limit or cursor"
)

var (
	errInvalidPendingCursor     = errors.New("invalid pending key request cursor")
	errInvalidPendingKeysLimit  = errors.New("invalid pending key request limit")
	errPendingCursorUnavailable = errors.New("pending key request cursor key unavailable")
)

// pendingCursor is the position of one examined pending request.
type pendingCursor struct {
	CreatedAt time.Time
	Source    string
	ID        string
}

// pendingCursorCodec seals cursors with AES-256-GCM under a key drawn when the
// handler is built, so a client can neither read a cursor nor make one, and a
// cursor that does not open is rejected. The control plane runs one replica,
// so a restart only invalidates the cursors in flight: the next request with
// one gets 400 and the client starts again from the first page.
type pendingCursorCodec struct {
	aead cipher.AEAD // nil when no key could be made: every cursor then fails
}

func newPendingCursorCodec() (pendingCursorCodec, error) {
	key := make([]byte, pendingCursorKeyLen)
	if _, err := rand.Read(key); err != nil {
		return pendingCursorCodec{}, fmt.Errorf("generate pending cursor key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return pendingCursorCodec{}, fmt.Errorf("create pending cursor cipher: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return pendingCursorCodec{}, fmt.Errorf("create pending cursor AEAD: %w", err)
	}
	return pendingCursorCodec{aead: aead}, nil
}

func (k pendingCursorCodec) seal(c pendingCursor) (string, error) {
	return k.sealText(c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.Source + "|" + c.ID)
}

func (k pendingCursorCodec) sealText(raw string) (string, error) {
	if k.aead == nil {
		return "", errPendingCursorUnavailable
	}
	return base64.RawURLEncoding.EncodeToString(k.aead.Seal(nil, nil, []byte(raw), nil)), nil //nolint:gosec // G407: NewGCMWithRandomNonce draws the nonce itself and requires a nil nonce argument
}

func (k pendingCursorCodec) open(s string) (pendingCursor, error) {
	if k.aead == nil || s == "" || len(s) > pendingCursorMaxLen {
		return pendingCursor{}, errInvalidPendingCursor
	}
	sealed, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return pendingCursor{}, errInvalidPendingCursor
	}
	raw, err := k.aead.Open(nil, nil, sealed, nil)
	if err != nil {
		return pendingCursor{}, errInvalidPendingCursor
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		return pendingCursor{}, errInvalidPendingCursor
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return pendingCursor{}, errInvalidPendingCursor
	}
	if parts[1] != pendingSourceChannel && parts[1] != pendingSourceDM {
		return pendingCursor{}, errInvalidPendingCursor
	}
	id, err := requestid.Canonical(parts[2])
	if err != nil {
		return pendingCursor{}, errInvalidPendingCursor
	}
	return pendingCursor{CreatedAt: createdAt, Source: parts[1], ID: id}, nil
}

// parsePendingKeysPage reads ?limit= and ?cursor=. An absent limit is the
// default; a present limit must be an integer in [1, max]. A present cursor
// must open. Each parameter may appear at most once.
func parsePendingKeysPage(c *gin.Context, cursors pendingCursorCodec) (limit int, after *pendingCursor, err error) {
	limit = pendingKeysDefaultLimit
	query := c.Request.URL.Query()
	if values, ok := query["limit"]; ok {
		if len(values) != 1 {
			return 0, nil, errInvalidPendingKeysLimit
		}
		n, convErr := strconv.Atoi(values[0])
		if convErr != nil || n < 1 || n > pendingKeysMaxLimit {
			return 0, nil, errInvalidPendingKeysLimit
		}
		limit = n
	}
	if values, ok := query["cursor"]; ok {
		if len(values) != 1 {
			return 0, nil, errInvalidPendingCursor
		}
		cursor, decErr := cursors.open(values[0])
		if decErr != nil {
			return 0, nil, decErr
		}
		after = &cursor
	}
	return limit, after, nil
}

// pendingCandidatesQuery returns the next candidates after an optional
// cursor ($2..$4, all NULL for the first page), in reader order. The channel
// arm returns a request only to a holder of the channel's active epoch: an
// active initial-distribution marker pins its successor epoch and stays
// creator-only; otherwise the active epoch is the newer of the surviving
// wrapped-key rows and the revocation ledger. The DM arm returns another
// participant's request only to a holder of the conversation's current epoch,
// because a fulfiller wraps the key it holds and the server stamps the row at
// the current epoch.
const pendingCandidatesQuery = `
	SELECT u.source, u.id, u.context_id, u.user_id, u.key_version, u.created_at
	FROM (
		SELECT DISTINCT 'channel'::text AS source, pkr.id, pkr.channel_id AS context_id, pkr.user_id,
			COALESCE(cid.key_version, active_epoch.key_version) AS key_version, pkr.created_at
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
		UNION ALL
		SELECT 'dm'::text, dpkr.id, dpkr.conversation_id, dpkr.user_id, 0, dpkr.created_at
		FROM dm_pending_key_requests dpkr
		INNER JOIN dm_channel_keys dck ON dpkr.conversation_id = dck.conversation_id AND dck.user_id = $1
		WHERE dpkr.user_id != $1
		  AND dck.key_version = (
			SELECT MAX(key_version) FROM dm_channel_keys WHERE conversation_id = dpkr.conversation_id
		  )
	) u
	WHERE $2::timestamptz IS NULL
	   OR (u.created_at, u.source, u.id) > ($2::timestamptz, $3::text, $4::uuid)
	ORDER BY u.created_at, u.source, u.id
	LIMIT $5
`

type pendingCandidate struct {
	req    pendingKeyRequest
	source string
	pos    pendingCursor
}

// readPendingCandidates reads up to n candidates after the cursor. The rows are
// drained and closed before any visibility check runs, so this read's pooled
// connection is not held across those checks.
func (h *Handler) readPendingCandidates(ctx context.Context, callerID string, after *pendingCursor, n int) ([]pendingCandidate, error) {
	var afterTime, afterSource, afterID interface{}
	if after != nil {
		afterTime, afterSource, afterID = after.CreatedAt, after.Source, after.ID
	}
	rows, err := h.db.QueryContext(ctx, pendingCandidatesQuery, callerID, afterTime, afterSource, afterID, n)
	if err != nil {
		return nil, fmt.Errorf("query pending key requests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	candidates := make([]pendingCandidate, 0, n)
	for rows.Next() {
		var cand pendingCandidate
		var createdAt time.Time
		if err := rows.Scan(&cand.source, &cand.req.ID, &cand.req.ChannelID, &cand.req.UserID, &cand.req.KeyVersion, &createdAt); err != nil {
			return nil, fmt.Errorf("scan pending key request: %w", err)
		}
		// The format database/sql used when this column was scanned into a
		// string, so the row JSON is unchanged.
		cand.req.CreatedAt = createdAt.Format(time.RFC3339Nano)
		cand.pos = pendingCursor{CreatedAt: createdAt, Source: cand.source, ID: cand.req.ID}
		candidates = append(candidates, cand)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending key requests: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close pending key request rows: %w", err)
	}
	return candidates, nil
}

// pendingKeyRequestsPage returns up to limit visible pending requests after
// the cursor, examining at most pendingKeysScanCap candidates. next is the
// last examined candidate when the page is full or the scan cap ended it, and
// nil only when the candidates ran out first.
// Visibility does not depend on the cursor, so a forged or foreign cursor can
// only skip rows, never reveal one.
func (h *Handler) pendingKeyRequestsPage(ctx context.Context, callerID string, after *pendingCursor, limit int) ([]pendingKeyRequest, *pendingCursor, error) {
	vis := newPendingVisibility(h, callerID)
	page := make([]pendingKeyRequest, 0, limit)
	cursor := after
	for examined := 0; examined < pendingKeysScanCap; {
		n := min(pendingKeysBatchSize, pendingKeysScanCap-examined)
		batch, err := h.readPendingCandidates(ctx, callerID, cursor, n)
		if err != nil {
			return nil, nil, err
		}
		vis.resolveChannelBatch(ctx, batch)
		exhausted := len(batch) < n
		for i := range batch {
			examined++
			cursor = &batch[i].pos
			visible, err := vis.visible(ctx, batch[i])
			if err != nil {
				return nil, nil, err
			}
			if !visible {
				continue
			}
			page = append(page, batch[i].req)
			// A full page always carries a cursor. Whether more candidates
			// follow is not looked at: they may all be hidden from the caller.
			if len(page) == limit {
				return page, cursor, nil
			}
		}
		if exhausted {
			return page, nil, nil
		}
	}
	return page, cursor, nil
}

// pendingVisibility applies the visibility rule of the unpaged reader it
// replaced (kept as the test oracle legacyPendingKeyRequests). A channel request is visible
// when both the caller and the requester can VIEW the channel (CV-CAN-005); a
// DM request is visible when the conversation is still available to the
// caller. Results are memoized for the request.
type pendingVisibility struct {
	h          *Handler
	callerID   string
	callerView map[string]bool // channel -> caller can view
	requester  map[string]bool // channel|requester -> requester can view
	dmOpen     map[string]bool // conversation -> available to the caller
}

func newPendingVisibility(h *Handler, callerID string) *pendingVisibility {
	return &pendingVisibility{
		h:          h,
		callerID:   callerID,
		callerView: map[string]bool{},
		requester:  map[string]bool{},
		dmOpen:     map[string]bool{},
	}
}

func requesterViewKey(channelID, userID string) string { return channelID + "|" + userID }

// resolveChannelBatch resolves the batch's channel visibility in bulk: the
// caller once per new channel, then each requester once per channel the
// caller can view, both with one fresh resolve per distinct server.
func (v *pendingVisibility) resolveChannelBatch(ctx context.Context, batch []pendingCandidate) {
	v.resolveCallerViews(ctx, batch)
	v.resolveRequesterViews(ctx, batch)
}

// resolveCallerViews memoizes the caller's view of each channel new to the batch.
func (v *pendingVisibility) resolveCallerViews(ctx context.Context, batch []pendingCandidate) {
	var newChannels []string
	seen := map[string]bool{}
	for _, cand := range batch {
		ch := cand.req.ChannelID
		if cand.source != pendingSourceChannel || seen[ch] {
			continue
		}
		seen[ch] = true
		if _, known := v.callerView[ch]; !known {
			newChannels = append(newChannels, ch)
		}
	}
	for ch, canView := range v.h.pendingChannelViews(ctx, v.callerID, newChannels) {
		v.callerView[ch] = canView
	}
}

// resolveRequesterViews memoizes each requester's view of the channels the
// caller can view.
func (v *pendingVisibility) resolveRequesterViews(ctx context.Context, batch []pendingCandidate) {
	byRequester := map[string][]string{}
	for _, cand := range batch {
		ch, user := cand.req.ChannelID, cand.req.UserID
		if cand.source != pendingSourceChannel || !v.callerView[ch] {
			continue
		}
		if _, known := v.requester[requesterViewKey(ch, user)]; !known {
			byRequester[user] = append(byRequester[user], ch)
		}
	}
	for user, chans := range byRequester {
		for ch, canView := range v.h.pendingChannelViews(ctx, user, chans) {
			v.requester[requesterViewKey(ch, user)] = canView
		}
	}
}

// visible reports a candidate's visibility. Channel results come from
// resolveChannelBatch; a channel missing from the memo is hidden. A DM
// availability error fails the request rather than return a truncated queue.
func (v *pendingVisibility) visible(ctx context.Context, cand pendingCandidate) (bool, error) {
	if cand.source == pendingSourceChannel {
		return v.callerView[cand.req.ChannelID] && v.requester[requesterViewKey(cand.req.ChannelID, cand.req.UserID)], nil
	}
	conv := cand.req.ChannelID
	if open, known := v.dmOpen[conv]; known {
		return open, nil
	}
	open, err := v.h.dmPendingConversationAvailable(ctx, conv, v.callerID)
	if err != nil {
		return false, fmt.Errorf("check pending DM conversation availability: %w", err)
	}
	v.dmOpen[conv] = open
	return open, nil
}

// pendingChannelViews reports channelKeyAccess's canView for userID on each
// channel, in bulk: one membership read for every channel, then one fresh
// resolve per distinct server. Every channel is in the result. An error hides
// every channel it covers (fail closed), as channelKeyAccess's callers here
// always did, so a persistent failure cannot re-expose a hidden queue.
func (h *Handler) pendingChannelViews(ctx context.Context, userID string, channelIDs []string) map[string]bool {
	views := make(map[string]bool, len(channelIDs))
	for _, ch := range channelIDs {
		views[ch] = false
	}
	if len(channelIDs) == 0 {
		return views
	}
	user, err := requestid.Canonical(userID)
	if err != nil {
		return views
	}
	targets, err := h.readPendingChannelTargets(ctx, user, channelIDs)
	if err != nil {
		h.log.Warn("Failed to read pending key request channels", "error", err)
		return views
	}
	for serverID, serverTargets := range targets {
		if h.resolver == nil {
			for _, target := range serverTargets {
				views[target.channelID] = true
			}
			continue
		}
		ids := make([]string, 0, len(serverTargets))
		for _, target := range serverTargets {
			ids = append(ids, target.channelID)
		}
		perms, permErr := h.resolver.ResolveEffectivePermissionsForChannelsFresh(ctx, serverID, user, ids)
		if permErr != nil {
			h.log.Warn("Failed to resolve pending key request channel permissions", "error", permErr)
			continue
		}
		for _, target := range serverTargets {
			views[target.channelID] = perms[target.channelID].Has(target.viewPerm)
		}
	}
	return views
}

// readPendingChannelTargets is readChannelKeyTarget for many channels: each
// channel the user is a server member for, grouped by server, with the view
// permission its type requires. A channel that is missing or whose server the
// user is not a member of is absent.
func (h *Handler) readPendingChannelTargets(ctx context.Context, userID string, channelIDs []string) (map[string][]channelKeyTarget, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT c.id, c.server_id, c.type FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id AND sm.user_id = $2
		WHERE c.id = ANY($1::uuid[])
	`, pq.Array(channelIDs), userID)
	if err != nil {
		return nil, fmt.Errorf("query pending key request channels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	targets := map[string][]channelKeyTarget{}
	for rows.Next() {
		target := channelKeyTarget{userID: userID}
		var channelType string
		if err := rows.Scan(&target.channelID, &target.serverID, &channelType); err != nil {
			return nil, fmt.Errorf("scan pending key request channel: %w", err)
		}
		target.viewPerm = viewPermForType(channelType)
		targets[target.serverID] = append(targets[target.serverID], target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending key request channels: %w", err)
	}
	return targets, nil
}
