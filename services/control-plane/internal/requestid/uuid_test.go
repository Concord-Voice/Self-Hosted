package requestid

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const canonicalUUID = "c0ffee00-abcd-4ef0-8abc-0123456789ab"

func TestCanonical_AcceptsUUIDSpellings(t *testing.T) {
	cases := map[string]string{
		"canonical": canonicalUUID,
		"uppercase": strings.ToUpper(canonicalUUID),
		"braces":    "{" + canonicalUUID + "}",
		"URN":       "urn:uuid:" + canonicalUUID,
		"raw hex":   "c0ffee00abcd4ef08abc0123456789ab",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Canonical(raw)
			require.NoError(t, err)
			assert.Equal(t, canonicalUUID, got)
		})
	}
}

func TestCanonical_RejectsMalformedUUID(t *testing.T) {
	got, err := Canonical("not-a-uuid")
	require.Error(t, err)
	assert.Empty(t, got)
}

func TestParam_MalformedUUIDDoesNotMutateResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}

	got, err := Param(context, "id")
	require.Error(t, err)
	assert.Empty(t, got)
	assert.Equal(t, http.StatusOK, recorder.Code, "Param leaves status selection to the handler")
	assert.Empty(t, recorder.Body.String(), "Param must not write an error body")
	assert.False(t, context.IsAborted(), "Param must not abort the handler chain")
}
