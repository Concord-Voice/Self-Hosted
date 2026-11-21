package natstest_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/natstest"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Port 1 is privileged and never bound by this suite, so the dial is refused
// at once. pkg/nats uses the same address for the same reason.
const noServerURL = "nats://127.0.0.1:1"

// recorder stands in for *testing.T. Skipf and Fatalf return here, where the
// real ones stop the goroutine, so a test can read what the helper decided.
type recorder struct {
	skips    []string
	fatals   []string
	cleanups []func()
}

func (r *recorder) Helper()                  {}
func (r *recorder) Cleanup(f func())         { r.cleanups = append(r.cleanups, f) }
func (r *recorder) Skipf(f string, a ...any) { r.skips = append(r.skips, fmt.Sprintf(f, a...)) }
func (r *recorder) Fatalf(f string, a ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(f, a...))
}

func (r *recorder) runCleanups() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
	r.cleanups = nil
}

// The defect this package exists for. With no server, natsclient.Connect
// returns no error, so a guard on that error never skips. Connect here must
// see the outage, and see it without waiting for a timeout.
func TestConnectSkipsWhenNoServerListens(t *testing.T) {
	t.Setenv("CI", "")

	// The premise: the old guard read this error, and it is nil.
	dialing, err := natsclient.Connect(noServerURL)
	require.NoError(t, err, "an outage is not an error from natsclient.Connect")
	require.False(t, dialing.IsConnected())
	_ = dialing.Close()

	rec := &recorder{}
	start := time.Now()
	client := natstest.Connect(rec, noServerURL)
	elapsed := time.Since(start)

	assert.Nil(t, client, "a caller must not get a client that is still dialing")
	require.Len(t, rec.skips, 1)
	assert.Contains(t, rec.skips[0], "NATS unavailable")
	assert.Empty(t, rec.fatals, "outside CI an outage skips")
	assert.Empty(t, rec.cleanups, "the dialing client is closed at once, not at cleanup")
	assert.Less(t, elapsed, 2*time.Second, "the check must not wait for a timeout")
}

// CI starts a NATS server for these tests. An outage there is a broken
// service, and a skip would report it as a pass.
func TestConnectFailsInCIWhenNoServerListens(t *testing.T) {
	t.Setenv("CI", "true")

	rec := &recorder{}
	client := natstest.Connect(rec, noServerURL)

	assert.Nil(t, client)
	require.Len(t, rec.fatals, 1)
	assert.Contains(t, rec.fatals[0], "NATS unavailable in CI")
	assert.Empty(t, rec.skips, "in CI an outage must not skip")
}

// A URL the client refuses is a configuration fault, which natsclient.Connect
// does report. It takes the same path, and the message never quotes the URL:
// a NATS URL can carry credentials.
func TestConnectTreatsARefusedURLAsUnavailableWithoutQuotingIt(t *testing.T) {
	t.Setenv("CI", "")
	const refused = "nats://[refused-marker"

	_, err := natsclient.Connect(refused)
	require.Error(t, err, "the premise: this URL is refused as configuration")

	rec := &recorder{}
	client := natstest.Connect(rec, refused)

	assert.Nil(t, client)
	require.Len(t, rec.skips, 1)
	assert.NotContains(t, rec.skips[0], "refused-marker")
}

// The other half: a server that is up must not be reported as down. Without
// this, a helper that always skipped would pass every test above.
func TestConnectReturnsAConnectedClientAndClosesItAtCleanup(t *testing.T) {
	t.Setenv("CI", "true")
	url := startProtocolStub(t)

	rec := &recorder{}
	client := natstest.Connect(rec, url)

	assert.Empty(t, rec.skips)
	assert.Empty(t, rec.fatals)
	require.NotNil(t, client)
	assert.True(t, client.IsConnected())
	require.Len(t, rec.cleanups, 1, "the helper owns closing the client")

	rec.runCleanups()
	assert.False(t, client.IsConnected(), "cleanup closes the connection")
}

func TestUnavailableSkipsOutsideCIAndFailsInIt(t *testing.T) {
	t.Run("outside CI", func(t *testing.T) {
		t.Setenv("CI", "")
		rec := &recorder{}
		natstest.Unavailable(rec, "the raw connection failed")
		require.Len(t, rec.skips, 1)
		assert.Contains(t, rec.skips[0], "the raw connection failed")
		assert.Empty(t, rec.fatals)
	})
	t.Run("in CI", func(t *testing.T) {
		t.Setenv("CI", "true")
		rec := &recorder{}
		natstest.Unavailable(rec, "the raw connection failed")
		require.Len(t, rec.fatals, 1)
		assert.Contains(t, rec.fatals[0], "the raw connection failed")
		assert.Empty(t, rec.skips)
	})
}

// A real *testing.T must satisfy the interface, or no caller compiles.
var _ natstest.TB = (*testing.T)(nil)

// startProtocolStub listens on a free local port and answers the two things a
// NATS client needs to reach CONNECTED: the INFO greeting, and PONG for each
// PING. It keeps this package's tests off the shared broker.
func startProtocolStub(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go serveProtocolStub(conn)
		}
	}()
	return "nats://" + listener.Addr().String()
}

func serveProtocolStub(conn net.Conn) {
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
