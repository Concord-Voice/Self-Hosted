package sentry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	concordsentry "github.com/markdrogersjr/Concord/services/control-plane/internal/sentry"
)

const (
	testOrgSlug   = "org-1"
	testProjSlug  = "proj-1"
	testAuthToken = "token-xyz" // pragma: allowlist secret
	testClientID  = "abc123"    // pragma: allowlist secret
)

func TestAdminClient_DeleteEventsForUser_HappyPath(t *testing.T) {
	var received *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	client := concordsentry.NewAdminClient(srv.URL, testOrgSlug, testProjSlug, testAuthToken)
	err := client.DeleteEventsForUser(context.Background(), "abc123def456") // pragma: allowlist secret
	require.NoError(t, err)

	assert.Equal(t, "DELETE", received.Method)
	assert.Contains(t, received.URL.Path, "/organizations/"+testOrgSlug+"/issues/")
	assert.Contains(t, received.URL.RawQuery, "query=user.id%3Aabc123def456")
	assert.Contains(t, received.URL.RawQuery, "project="+testProjSlug)
	assert.Equal(t, "Bearer "+testAuthToken, received.Header.Get("Authorization"))
}

func TestAdminClient_DeleteEventsForUser_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	client := concordsentry.NewAdminClient(srv.URL, testOrgSlug, testProjSlug, testAuthToken)
	err := client.DeleteEventsForUser(context.Background(), testClientID)
	require.Error(t, err)
}

// Covers the http.NewRequestWithContext error branch by passing a baseURL
// that cannot be parsed as a valid URL.
func TestAdminClient_DeleteEventsForUser_BuildRequestError(t *testing.T) {
	client := concordsentry.NewAdminClient("://not a url", testOrgSlug, testProjSlug, testAuthToken)
	err := client.DeleteEventsForUser(context.Background(), testClientID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "build request")
}

// Covers the io.Copy(io.Discard, resp.Body) body-drain branch by returning a
// 500 with a non-empty body that the client must drain before surfacing the
// error.
func TestAdminClient_DeleteEventsForUser_DrainsBodyOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"internal error from sentry"}`))
	}))
	defer srv.Close()

	client := concordsentry.NewAdminClient(srv.URL, testOrgSlug, testProjSlug, testAuthToken)
	err := client.DeleteEventsForUser(context.Background(), testClientID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}
