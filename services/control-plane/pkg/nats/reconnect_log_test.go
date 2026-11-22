package nats

import (
	"bufio"
	"bytes"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// CWE-532. The reconnect handler names the server it reached. nc.ConnectedUrl()
// returns the URL as configured, password included, so every reconnect wrote
// the NATS password into the control plane's log. The line now carries the
// host and port, which is all it is for.
//
// Falsified by logging nc.ConnectedUrl() again: the password assertion fails.
// No assertion quotes the log, so a failure never prints the password.
func TestReconnectLogLineNamesTheHostWithoutThePassword(t *testing.T) {
	const fakePassword = "fake-reconnect-password-not-a-credential" // #nosec G101 -- synthetic marker, not a credential // pragma: allowlist secret
	logs := &lockedLogBuffer{}
	previous := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	server := startReconnectStub(t)
	client, err := Connect("nats://concord:" + fakePassword + "@" + server.addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.True(t, client.IsConnected(), "the stub answers the handshake, so Connect returns connected")

	// Drop the first connection from the server side. The client reconnects
	// to the same listener after ReconnectWait.
	select {
	case first := <-server.accepted:
		require.NoError(t, first.Close())
	case <-time.After(10 * time.Second):
		t.Fatal("the stub never accepted the client's connection")
	}

	require.Eventually(t, func() bool { return reconnectLogLine(logs.String()) != "" },
		20*time.Second, 20*time.Millisecond, "the client never logged a reconnect")
	require.True(t, strings.Contains(reconnectLogLine(logs.String()), server.addr),
		"the reconnect line must still name the server it reached")
	require.False(t, strings.Contains(logs.String(), fakePassword),
		"a log line carries the NATS URL password")
}

// The line is built from the host alone. Masking the password is not enough:
// a NATS URL can carry a token in the user position (nats://token@host), and
// url.Redacted, which nats.go's ConnectedUrlRedacted uses, leaves the user in.
func TestConnectedServerKeepsOnlyTheHost(t *testing.T) {
	const fakeSecret = "fake-nats-secret-not-a-credential" // #nosec G101 -- synthetic marker, not a credential // pragma: allowlist secret
	for name, rawURL := range map[string]string{
		"two-part userinfo": "nats://concord:" + fakeSecret + "@nats.internal:4222",
		"one-part userinfo": "nats://" + fakeSecret + "@nats.internal:4222",
		"no userinfo":       "nats://nats.internal:4222",
		"tls scheme":        "tls://concord:" + fakeSecret + "@nats.internal:4222",
	} {
		t.Run(name, func(t *testing.T) {
			got := connectedServer(rawURL)
			require.Equal(t, "nats.internal:4222", got)
		})
	}
	t.Run("a URL that does not parse names no part of itself", func(t *testing.T) {
		got := connectedServer("nats://" + fakeSecret + "@[broken")
		require.False(t, strings.Contains(got, fakeSecret), "the fallback carries the secret")
		require.NotEmpty(t, got)
	})
	t.Run("an empty URL", func(t *testing.T) {
		require.NotEmpty(t, connectedServer(""))
	})
}

// reconnectLogLine returns the first reconnect line in the captured log, or "".
func reconnectLogLine(captured string) string {
	for _, line := range strings.Split(captured, "\n") {
		if strings.Contains(line, "NATS reconnected to ") {
			return line
		}
	}
	return ""
}

// lockedLogBuffer collects the standard logger, which the NATS callback
// goroutine writes while the test reads.
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type reconnectStub struct {
	addr     string
	accepted chan net.Conn
}

// startReconnectStub listens on a free local port and answers what a NATS
// client needs to reach CONNECTED: the INFO greeting, and PONG for each PING.
// Each accepted connection is also handed to the test, which drops it to force
// a reconnect. It keeps this test off the shared broker.
func startReconnectStub(t *testing.T) reconnectStub {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	stub := reconnectStub{addr: listener.Addr().String(), accepted: make(chan net.Conn, 16)}
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			select {
			case stub.accepted <- conn:
			default:
			}
			go serveReconnectStub(conn)
		}
	}()
	return stub
}

func serveReconnectStub(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	const info = `INFO {"server_id":"stub","version":"2.10.0","proto":1,"max_payload":1048576}` + "\r\n"
	if _, err := conn.Write([]byte(info)); err != nil {
		return
	}
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		if strings.HasPrefix(line, "PING") {
			if _, err := conn.Write([]byte("PONG\r\n")); err != nil {
				return
			}
		}
	}
}
