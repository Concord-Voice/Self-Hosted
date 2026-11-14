package admin

import (
	"context"
	"database/sql"
	"io"
	"net/url"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

// export_test.go exposes a few unexported helpers to the external `admin_test`
// package so behavioral tests can exercise them without widening the public API
// surface. This is the idiomatic Go "export for test" bridge; it compiles only
// under `go test` (the _test.go suffix), so the production API stays minimal.

// CheckAAGUIDForTest is a test-only wrapper around the unexported checkAAGUID,
// the AAGUID allow-list gate enforced at admin WebAuthn enrollment (#1688 §8).
func CheckAAGUIDForTest(aaguid []byte, allowed []string) error {
	return checkAAGUID(aaguid, allowed)
}

// WindowForLevelForTest exposes the lock-escalation schedule (#221).
func WindowForLevelForTest(level int64) time.Duration { return windowForLevel(level) }

// EnrollBaseURLForTest exposes the unexported enrollBaseURL helper to the
// external admin_test package so table tests can exercise both branches
// (configured origin vs. localhost fallback) without changing the production API.
func EnrollBaseURLForTest(cfg *config.Config) string {
	return enrollBaseURL(cfg)
}

// RunAdminCtlForTest drives the unexported runAdminCtl with fully injected deps
// (isolated DB + Redis + in-memory stdin/stdout), so adminctl_test.go can
// exercise the bootstrap / reset-enrollment verbs without a real TTY or the
// production config.Load + db/redis-open path. Returns the verb's exit code.
func RunAdminCtlForTest(
	ctx context.Context,
	db *sql.DB,
	rdb *redis.Client,
	stdin io.Reader,
	stdout io.Writer,
	enrollBaseURL string,
	args []string,
) int {
	u, _ := url.Parse(enrollBaseURL)
	deps := adminCtlDeps{
		repo:          NewAdminRepo(db),
		audit:         NewAuditLog(db),
		enroll:        NewEnrollmentStore(rdb),
		lockout:       NewLockout(rdb, nil),
		enrollBaseURL: enrollBaseURL,
		rpID:          u.Hostname(),
		rpOrigins:     []string{enrollBaseURL},
		stdin:         stdin,
		stdout:        stdout,
		stderr:        io.Discard,
	}
	return runAdminCtl(ctx, deps, args)
}

// RunAdminCtlWithWriterForTest exercises the production entry ordering with a
// controllable security-event writer opener.
func RunAdminCtlWithWriterForTest(ctx context.Context, db *sql.DB, rdb *redis.Client, stdin io.Reader, stdout io.Writer, baseURL string, args []string, open func() (securityevent.Emitter, io.Closer, error)) int {
	u, _ := url.Parse(baseURL)
	deps := adminCtlDeps{
		repo:          NewAdminRepo(db),
		audit:         NewAuditLog(db),
		enroll:        NewEnrollmentStore(rdb),
		lockout:       NewLockout(rdb, nil),
		enrollBaseURL: baseURL,
		rpID:          u.Hostname(),
		rpOrigins:     []string{baseURL},
		stdin:         stdin,
		stdout:        stdout,
		stderr:        io.Discard,
	}
	return runAdminCtlWithSecurityEvents(ctx, deps, args, open)
}

// UseServerAgentForTest points the handler's Server area at a test socket.
func UseServerAgentForTest(h *Handler, sock string) {
	h.server.agent = unixClient(sock)
}
