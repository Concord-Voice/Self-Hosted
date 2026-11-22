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

// fakeSecret stands in for a NATS password or token. No assertion quotes the
// captured log, so a failure never prints it.
const fakeSecret = "fake-nats-secret-not-a-credential" // #nosec G101 -- synthetic marker, not a credential // pragma: allowlist secret

const (
	reconnectedLine  = "NATS reconnected to "
	disconnectedLine = "NATS disconnected: "
)

// CWE-532. The reconnect handler names the server it reached. nc.ConnectedUrl()
// returns the URL as configured, so every reconnect wrote the NATS password, or
// a token in the user position, into the log. The line now carries the host and
// port, which is all it is for.
//
// Both forms run through the handler, not only through connectedServer. A
// handler that logged nats.go's ConnectedUrlRedacted would pass the first case
// and still print the token: url.Redacted masks the password and keeps the user.
func TestReconnectLogLineNamesTheHostWithoutTheCredential(t *testing.T) {
	for name, userinfo := range map[string]string{
		"two-part userinfo": "concord:" + fakeSecret,
		"one-part userinfo": fakeSecret,
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureStandardLog(t)
			server := startReconnectStub(t)
			client, err := Connect("nats://" + userinfo + "@" + server.addr)
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })
			require.True(t, client.IsConnected(), "the stub answers the handshake, so Connect returns connected")

			// Drop the first connection from the server side. The client
			// reconnects to the same listener after ReconnectWait.
			require.NoError(t, server.nextConn(t).Close())

			require.Eventually(t, func() bool { return logLineWith(logs.String(), reconnectedLine) != "" },
				20*time.Second, 20*time.Millisecond, "the client never logged a reconnect")
			require.True(t, strings.Contains(logLineWith(logs.String(), reconnectedLine), server.addr),
				"the reconnect line must still name the server it reached")
			require.False(t, strings.Contains(logs.String(), fakeSecret),
				"a log line carries the credential from the NATS URL")
		})
	}
}

// CWE-117. nats.go reports a malformed protocol line as
// "nats: Parse Error [state]: '<the rest of what it read>'", built from the raw
// bytes. The disconnect handler printed that error as it came, so a server, or
// anyone on an unauthenticated path to the client, could end the line and start
// one of its own. The handler now quotes the text, as the async error line does.
func TestDisconnectLogLineQuotesWhatTheServerSent(t *testing.T) {
	logs := captureStandardLog(t)
	server := startReconnectStub(t)
	client, err := Connect("nats://" + server.addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.True(t, client.IsConnected())

	const marker = "forged-by-the-server"
	const forged = "2026/11/21 09:00:00 " + marker + " auth: admin login succeeded"
	_, err = server.nextConn(t).Write([]byte("Q\n" + forged + "\r\n"))
	require.NoError(t, err)

	require.Eventually(t, func() bool { return logLineWith(logs.String(), disconnectedLine) != "" },
		20*time.Second, 20*time.Millisecond, "the client never logged a disconnect")
	for _, line := range strings.Split(logs.String(), "\n") {
		require.False(t, strings.HasPrefix(line, forged), "text the server chose begins a log line of its own")
	}
	// The premise. If the server's text never reached the handler, the check
	// above would pass for any handler.
	require.True(t, strings.Contains(logLineWith(logs.String(), disconnectedLine), marker),
		"the disconnect line does not carry the text the server sent")
}

func TestConnectedServerKeepsOnlyTheHost(t *testing.T) {
	for name, tc := range map[string]struct{ rawURL, want string }{
		"two-part userinfo":        {"nats://concord:" + fakeSecret + "@nats.internal:4222", "nats.internal:4222"},
		"one-part userinfo":        {"nats://" + fakeSecret + "@nats.internal:4222", "nats.internal:4222"},
		"no userinfo":              {"nats://nats.internal:4222", "nats.internal:4222"},
		"tls scheme":               {"tls://concord:" + fakeSecret + "@nats.internal:4222", "nats.internal:4222"},
		"websocket with a path":    {"wss://concord:" + fakeSecret + "@nats.internal:443/bus?k=" + fakeSecret, "nats.internal:443"},
		"percent-encoded userinfo": {"nats://concord:p%40" + fakeSecret + "@nats.internal:4222", "nats.internal:4222"},
		"IPv6 literal":             {"nats://concord:" + fakeSecret + "@[::1]:4222", "[::1]:4222"},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, connectedServer(tc.rawURL))
		})
	}

	// A server chooses the host of every address it advertises to the client.
	// An IPv6 zone may carry percent-encoded spaces, and url.URL.Host is the
	// decoded form. Printed as decoded, the zone's text reads as fields of the
	// log line (CWE-117).
	t.Run("a zone the server chose stays encoded", func(t *testing.T) {
		const advertised = "[::ffff:127.0.0.1%25%20forged%20level=error]:4222"
		got := connectedServer("nats://" + advertised)
		require.Equal(t, advertised, got)
		require.False(t, strings.ContainsAny(got, " \t\r\n"), "the rendered host carries raw whitespace")
	})
	t.Run("a URL that does not parse names no part of itself", func(t *testing.T) {
		got := connectedServer("nats://" + fakeSecret + "@[broken")
		require.False(t, strings.Contains(got, fakeSecret), "the fallback carries the secret")
		require.NotEmpty(t, got)
	})
	t.Run("an empty URL", func(t *testing.T) {
		require.NotEmpty(t, connectedServer(""))
	})
}

// captureStandardLog points the standard logger at a buffer for one test.
func captureStandardLog(t *testing.T) *lockedLogBuffer {
	t.Helper()
	logs := &lockedLogBuffer{}
	previous := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	return logs
}

// logLineWith returns the first captured line that contains want, or "".
func logLineWith(captured, want string) string {
	for _, line := range strings.Split(captured, "\n") {
		if strings.Contains(line, want) {
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

// nextConn returns the next connection the stub accepted.
func (s reconnectStub) nextConn(t *testing.T) net.Conn {
	t.Helper()
	select {
	case conn := <-s.accepted:
		return conn
	case <-time.After(10 * time.Second):
		t.Fatal("the stub never accepted the client's connection")
		return nil
	}
}

// startReconnectStub listens on a free local port and answers what a NATS
// client needs to reach CONNECTED: the INFO greeting, and PONG for each PING.
// Each accepted connection is also handed to the test, which drops it or writes
// to it. It keeps these tests off the shared broker.
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
