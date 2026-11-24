package admin

import (
	"errors"
	"mime"
	"net/http"
	"regexp"
	"strconv"

	"github.com/gin-gonic/gin"
)

// maxNoticeAckBody bounds the acknowledgement body, {"cursor":"<digits>"}.
const maxNoticeAckBody = 1 << 10

// noticeCursorPattern is the cursor the sign-in response carries: a positive
// decimal audit row id.
var noticeCursorPattern = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

type noticeAckRequest struct {
	Cursor string `json:"cursor"`
}

// AcknowledgeSignInNotice records that the portal displayed the sign-in
// notice of this session, so the next sign-in counts only the admins added
// after it. POST {"cursor": "<notice_cursor from the sign-in response>"};
// 204 on success.
//
// It is a gated route (AdminAuthRequired), so it needs the SameSite=Strict
// session cookie. It also takes only a JSON body, which a cross-site form
// cannot send without a CORS preflight, and refuses a request the browser
// marks as cross-site. The cursor is checked against the one this session's
// sign-in showed (AuditLog.AcknowledgeSignInNotice), so a client cannot skip
// creations by acknowledging a higher one.
func (h *Handler) AcknowledgeSignInNotice(c *gin.Context) {
	ctx := c.Request.Context()
	adminID := c.GetString(adminIDContextKey)
	sid, err := c.Cookie(adminCookieName)
	if err != nil || sid == "" || adminID == "" {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	if !sameOriginJSON(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}
	var req noticeAckRequest
	if decodeStrict(c, &req, maxNoticeAckBody) != nil || !noticeCursorPattern.MatchString(req.Cursor) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_cursor"})
		return
	}
	cursor, err := strconv.ParseInt(req.Cursor, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_cursor"})
		return
	}
	switch err := h.audit.AcknowledgeSignInNotice(ctx, adminID, sid, cursor); {
	case err == nil:
		c.Status(http.StatusNoContent)
	case errors.Is(err, ErrNoticeCursorInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_cursor"})
	case errors.Is(err, ErrNoticeCursorStale):
		c.JSON(http.StatusConflict, gin.H{"error": "stale_cursor"})
	case errors.Is(err, errAuditWrite):
		h.log.Error("admin sign-in notice acknowledgement not recorded", "error", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "audit_unavailable"})
	default:
		h.log.Error("admin sign-in notice acknowledgement failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
	}
}

// sameOriginJSON reports whether r carries a JSON body and is not marked by
// the browser as cross-site (Sec-Fetch-Site, when present, is same-origin).
func sameOriginJSON(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != contentTypeJSON {
		return false
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin"
}
