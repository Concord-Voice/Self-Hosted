// Package natstest connects tests to a live NATS server, skipping when none is up.
package natstest

import (
	"testing"

	natsclient "github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/nats"
)

// Connect returns a client connected to url, or skips the test.
//
// natsclient.Connect sets RetryOnFailedConnect, so a server that is down
// returns a reconnecting client and no error. An error check alone never
// skips; the test then waits out its own timeout and fails. Check the
// connection state too.
func Connect(t testing.TB, url string) *natsclient.Client {
	t.Helper()
	c, err := natsclient.Connect(url)
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping live NATS test (runs in CI)", err)
	}
	if !c.IsConnected() {
		_ = c.Close()
		t.Skip("NATS unavailable (not connected); skipping live NATS test (runs in CI)")
	}
	return c
}
