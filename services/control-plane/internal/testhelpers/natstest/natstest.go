// Package natstest connects an integration test to the live NATS server and
// decides what the test does when there is none.
//
// Why this exists: natsclient.Connect cannot report an outage. It sets
// RetryOnFailedConnect, so with no server listening it returns a client that is
// still dialing and a nil error. Tests guarded their live-NATS path with
//
//	client, err := natsclient.Connect(url)
//	if err != nil {
//		t.Skipf("NATS unavailable (%v)", err)
//	}
//
// and that skip could no longer fire for an outage. The test went on to wait
// for a message on a connection that never opened, and failed on its own
// timeout about ten seconds later.
//
// This package is a leaf: the standard library, testing and pkg/nats only.
// internal/testhelpers imports the packages whose tests need this, so anything
// imported from internal/ here would make a cycle for one of them.
package natstest

import (
	"os"

	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
)

// TB is the part of testing.TB this package uses. Callers pass their
// *testing.T. The package's own tests pass a recorder, because a real Fatalf
// would fail the test that checks it.
type TB interface {
	Helper()
	Cleanup(func())
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// Connect returns a client that is connected to the NATS server at url, and
// closes it when the test ends. When no server is reachable it calls
// Unavailable, so a caller never holds a client that is still dialing.
//
// The check is one read of the connection state, with no wait. Connect dials
// before it returns: a server that is up is connected by then, and a server
// that is down has already refused the dial.
//
// A test that must check the result of Close closes the client in its own
// cleanup. That cleanup runs first, and the close here then does nothing.
func Connect(t TB, url string) *natsclient.Client {
	t.Helper()
	client, err := natsclient.Connect(url)
	if err != nil {
		// The error can quote the URL, and a NATS URL can carry credentials.
		Unavailable(t, "the NATS URL was refused as configuration")
		return nil
	}
	if !client.IsConnected() {
		// Stop the reconnect loop. It would outlive the test otherwise.
		_ = client.Close()
		Unavailable(t, "no server answered at the NATS URL")
		return nil
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// Unavailable ends a test that needs the live NATS server and cannot reach it.
// It skips the test, except in CI, where it fails it. CI starts a NATS server
// for these tests, so a skip there would report a broken service as a pass.
//
// reason is a fixed phrase. Do not pass a connect error: it can quote the URL.
func Unavailable(t TB, reason string) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf("NATS unavailable in CI (%s); live-NATS tests must run here", reason)
		return
	}
	t.Skipf("NATS unavailable (%s); skipping a live-NATS test", reason)
}
