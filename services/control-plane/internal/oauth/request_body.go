package oauth

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// maxSSORequestBytes caps the entire JSON request, including fields ignored by
// the binder and bytes after the first JSON value. It leaves room for the
// permitted 64 KiB apple_user_data field plus id_token and JSON overhead.
// Registration's wrapped key is bounded separately before it is decoded.
const maxSSORequestBytes = 128 * 1024

var errSSORequestTooLarge = errors.New("sso request body too large")

func bindSSORequest(c *gin.Context, req any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxSSORequestBytes)
	// ShouldBindBodyWithJSON reads the whole body before decoding. ShouldBindJSON
	// can stop after the first value and leave an oversized suffix unread.
	if err := c.ShouldBindBodyWithJSON(req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errSSORequestTooLarge
		}
		return err
	}
	body, ok := c.Get(gin.BodyBytesKey)
	raw, isBytes := body.([]byte)
	if !ok || !isBytes || !json.Valid(raw) {
		return errors.New("invalid SSO JSON")
	}
	return nil
}
