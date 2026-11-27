package natstest

import (
	"os"
	"testing"
)

// runSkips reports whether Connect skipped the subtest instead of returning.
func runSkips(t *testing.T, url string) bool {
	t.Helper()
	var sub *testing.T
	t.Run("connect", func(st *testing.T) {
		sub = st
		_ = Connect(st, url)
	})
	return sub.Skipped()
}

// Port 1 refuses: the retrying client comes back not connected, with no error.
func TestConnectSkipsWhenServerIsDown(t *testing.T) {
	if !runSkips(t, "nats://127.0.0.1:1") {
		t.Fatal("Connect returned for an unreachable server instead of skipping")
	}
}

func TestConnectSkipsOnConnectError(t *testing.T) {
	if !runSkips(t, "nats://[::1") {
		t.Fatal("Connect returned for an unparseable URL instead of skipping")
	}
}

func TestConnectReturnsConnectedClient(t *testing.T) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		url = "nats://localhost:4222"
	}
	c := Connect(t, url)
	defer func() { _ = c.Close() }()
	if !c.IsConnected() {
		t.Fatal("Connect returned a client that is not connected")
	}
}
