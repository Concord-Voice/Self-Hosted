package sentry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// AdminClient wraps the Sentry Events-Delete REST API with zero-persistence
// discipline: it never logs the clientId value passed to DeleteEventsForUser.
type AdminClient struct {
	baseURL  string
	orgSlug  string
	projSlug string
	token    string
	http     *http.Client
}

// NewAdminClient constructs an AdminClient pointed at the given Sentry API
// base URL (e.g. "https://sentry.io").
func NewAdminClient(baseURL, orgSlug, projSlug, token string) *AdminClient {
	return &AdminClient{
		baseURL:  baseURL,
		orgSlug:  orgSlug,
		projSlug: projSlug,
		token:    token,
		http:     &http.Client{Timeout: 10 * time.Second},
	}
}

// DeleteEventsForUser instructs Sentry to delete all issues tagged with the
// given user ID. Per Sentry's API docs, there is no direct bulk event-delete
// endpoint filterable by query; the closest supported call is the org-scoped
// issues-bulk-delete endpoint
//
//	DELETE /api/0/organizations/{org}/issues/?query=user.id:{clientID}&project={projSlug}
//
// which matches issues and removes them along with their events. Sentry's
// search syntax supports `user.id:X` as a query filter. The `project` query
// parameter scopes the deletion to this project; without it, the call would
// target every project in the org visible to the auth token.
//
// The clientId MUST NOT be logged by callers; this function does not log it
// either.
func (c *AdminClient) DeleteEventsForUser(ctx context.Context, clientID string) error {
	q := url.Values{}
	q.Set("query", "user.id:"+clientID)
	q.Set("project", c.projSlug)
	target := fmt.Sprintf("%s/api/0/organizations/%s/issues/?%s",
		c.baseURL, c.orgSlug, q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return sanitizeTransportError(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req) //nolint:gosec // URL built from internal config (SENTRY_API_BASE + slugs), not user input
	if err != nil {
		return sanitizeTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("sentry delete returned status %d", resp.StatusCode)
	}
	return nil
}

// sanitizeTransportError strips the URL from a *url.Error before wrapping so
// the clientId embedded in the Sentry bulk-delete query string ("user.id:<id>")
// never reaches a log via the error chain. Per [internal]rules/observability.md,
// the clientId is NEVER logged — and net/http's *url.Error.Error() includes
// the request URL verbatim. We preserve the Op and underlying Err so operators
// can still diagnose the class of failure (DNS, timeout, TLS, etc.) without
// exposing the PII-bearing query string.
func sanitizeTransportError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("delete request (%s): %w", urlErr.Op, urlErr.Err)
	}
	return fmt.Errorf("delete request: %w", err)
}
