package database

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventBudgetTransportTLSBlackholeUsesOneInitialDial(t *testing.T) {
	t.Log("B4 database TLS blackhole start")
	peer := newEventBudgetTransportPeer(t, "tls-blackhole")
	client, err := NewEventBudgetRedisClient("rediss://" + peer.listener.Addr().String())
	require.NoError(t, err)
	eventBudgetClientCleanup(t, client)
	require.Zero(t, peer.accepted.Load(), "construction must not dial")

	result := startEventBudgetTransportCommand(t, func(ctx context.Context) error { return client.Ping(ctx).Err() })
	awaitEventBudgetTransportSignal(t, peer.started)
	require.Error(t, awaitEventBudgetTransportResult(t, result))
	// TLS negotiation is inside the default Dialer, unlike Redis HELLO.
	// Its timeout therefore exercises DialerRetries, without replacing Dialer.
	require.EqualValues(t, 1, peer.accepted.Load(), "one failed initial dial, no internal dial retries")
	require.NoError(t, client.Close())
	peer.release()
	assertEventBudgetTransportClosed(t, client, peer, 0)
	t.Log("B4 database TLS blackhole pass")
}

func TestEventBudgetTransportLostEvalReplyDoesNotReplay(t *testing.T) {
	t.Log("B4 database lost EVAL reply start")
	peer := newEventBudgetTransportPeer(t, "drop-eval")
	client, err := NewEventBudgetRedisClient("redis://" + peer.listener.Addr().String())
	require.NoError(t, err)
	eventBudgetClientCleanup(t, client)
	require.NoError(t, client.Ping(t.Context()).Err(), "complete actual RESP3 initialization before the command")
	require.EqualValues(t, 1, peer.accepted.Load())

	// The peer counts the real command then drops its socket. This tests client
	// transport behavior; real Redis debit persistence is covered in websocket.
	require.Error(t, client.Eval(t.Context(), "return 1", nil).Err())
	awaitEventBudgetTransportSignal(t, peer.started)
	require.EqualValues(t, 1, peer.scripts.Load(), "EOF must not replay the application EVAL")
	require.EqualValues(t, 1, peer.accepted.Load())
	require.NoError(t, client.Close())
	peer.release()
	assertEventBudgetTransportClosed(t, client, peer, 1)
	t.Log("B4 database lost EVAL reply pass")
}

func TestEventBudgetTransportCloseInterruptsInFlightEval(t *testing.T) {
	t.Log("B4 database in-flight Close start")
	peer := newEventBudgetTransportPeer(t, "hold-eval")
	client, err := NewEventBudgetRedisClient("redis://" + peer.listener.Addr().String())
	require.NoError(t, err)
	eventBudgetClientCleanup(t, client)
	require.NoError(t, client.Ping(t.Context()).Err())

	result := startEventBudgetTransportCommand(t, func(ctx context.Context) error {
		return client.Eval(ctx, "return 1", nil).Err()
	})
	awaitEventBudgetTransportSignal(t, peer.started)
	// The peer has consumed EVAL and cannot answer until release. Check that
	// Close begins with an outstanding call, rather than after its read timeout.
	select {
	case <-result:
		t.Fatal("EVAL settled before the in-flight Close barrier")
	default:
	}
	require.NoError(t, client.Close())
	require.ErrorIs(t, awaitEventBudgetTransportResult(t, result), net.ErrClosed)
	peer.release()
	assertEventBudgetTransportClosed(t, client, peer, 1)
	t.Log("B4 database in-flight Close pass")
}

func awaitEventBudgetTransportSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("transport fixture barrier was not reached")
	}
}

func startEventBudgetTransportCommand(t *testing.T, command func(context.Context) error) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		result <- command(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("transport fixture command worker did not join")
		}
	})
	return result
}

func awaitEventBudgetTransportResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("transport command did not settle")
		return nil
	}
}

func assertEventBudgetTransportClosed(t *testing.T, client *redis.Client, peer *eventBudgetTransportPeer, scripts int64) {
	t.Helper()
	require.ErrorIs(t, client.Ping(t.Context()).Err(), redis.ErrClosed)
	require.Zero(t, client.PoolStats().TotalConns)
	acceptsAtClose := peer.accepted.Load()
	started := time.Now()
	// Pinned go-redis tryDial sleeps one second between recovery probes and
	// exposes no public worker join. Keep accepting beyond that interval and
	// observe sockets, not process-wide goroutine counts. These cases do not
	// claim to activate its 32-dial-error saturation threshold.
	timer := time.NewTimer(1100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal("post-Close transport observation was interrupted")
	}
	require.GreaterOrEqual(t, time.Since(started), 1100*time.Millisecond)
	require.Equal(t, acceptsAtClose, peer.accepted.Load(), "no delayed socket accepts during the post-Close window")
	require.Equal(t, scripts, peer.scripts.Load(), "no EVAL replay during command failure or teardown")
	require.Equal(t, peer.accepted.Load(), peer.closed.Load(), "accepted fixture sockets must settle after client Close")
	require.Zero(t, client.PoolStats().TotalConns)
	require.NoError(t, peer.Close(), "join every fixture-owned worker")
	t.Log("observed 1.1s after client Close: no later accepts or script commands; fixture workers joined")
}

// eventBudgetTransportPeer is a bounded loopback peer, not a Redis emulator.
// It accepts the production RESP3 setup and PING; EVAL is only counted. All
// fixture deadlines are diagnostic bounds, separate from client policy.
type eventBudgetTransportPeer struct {
	listener net.Listener
	mode     string
	started  chan struct{}
	gate     chan struct{}
	stop     chan struct{}
	accepted atomic.Int64
	closed   atomic.Int64
	scripts  atomic.Int64

	startOnce   sync.Once
	releaseOnce sync.Once
	closeOnce   sync.Once
	acceptDone  chan struct{}
	workers     sync.WaitGroup
	mu          sync.Mutex
	connections map[net.Conn]struct{}
	err         error
}

func newEventBudgetTransportPeer(t *testing.T, mode string) *eventBudgetTransportPeer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	peer := &eventBudgetTransportPeer{
		listener: listener, mode: mode, started: make(chan struct{}), gate: make(chan struct{}),
		stop: make(chan struct{}), acceptDone: make(chan struct{}), connections: make(map[net.Conn]struct{}),
	}
	t.Cleanup(func() { assert.NoError(t, peer.Close()) })
	go peer.accept()
	return peer
}

func (p *eventBudgetTransportPeer) accept() {
	defer close(p.acceptDone)
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				p.recordError("transport fixture accept failed")
			}
			return
		}
		if p.accepted.Add(1) > 32 {
			p.recordError("transport fixture exceeded connection limit")
			p.closeSocket(conn)
			return
		}
		p.mu.Lock()
		p.connections[conn] = struct{}{}
		p.mu.Unlock()
		p.workers.Add(1)
		go p.serve(conn)
	}
}

func (p *eventBudgetTransportPeer) serve(conn net.Conn) {
	defer p.workers.Done()
	defer func() {
		p.closeSocket(conn)
		p.mu.Lock()
		delete(p.connections, conn)
		p.mu.Unlock()
		p.closed.Add(1)
	}()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		p.recordError("transport fixture deadline failed")
		return
	}
	if p.mode == "tls-blackhole" {
		var first [1]byte
		if _, err := io.ReadFull(conn, first[:]); err != nil || first[0] != 0x16 {
			p.recordError("transport fixture expected TLS handshake")
			return
		}
		p.startOnce.Do(func() { close(p.started) })
		p.awaitRelease()
		// Drain only the bounded ClientHello after the client closes. Do not
		// implement TLS, install a custom dialer, or change client timeouts.
		if n, err := io.Copy(io.Discard, io.LimitReader(conn, 32768)); n == 32768 || (err != nil && !eventBudgetTransportDisconnect(err)) {
			p.recordError("transport fixture TLS socket did not settle")
		}
		return
	}
	reader := bufio.NewReaderSize(conn, 128)
	for commands := 0; commands < 32; commands++ {
		args, err := readEventBudgetTransportCommand(reader)
		if err != nil {
			if !eventBudgetTransportDisconnect(err) {
				p.recordError("transport fixture command read failed")
			}
			return
		}
		var reply string
		switch strings.ToLower(args[0]) {
		case "hello":
			if len(args) != 2 || args[1] != "3" {
				p.recordError("transport fixture expected RESP3 initialization")
				return
			}
			reply = "%1\r\n+proto\r\n:3\r\n"
		case "client":
			if len(args) != 4 || !strings.EqualFold(args[1], "setinfo") {
				p.recordError("transport fixture unexpected client command")
				return
			}
			reply = "+OK\r\n"
		case "ping":
			reply = "+PONG\r\n"
		case "eval":
			p.scripts.Add(1)
			p.startOnce.Do(func() { close(p.started) })
			if p.mode == "hold-eval" {
				p.awaitRelease()
				var next [1]byte
				if n, readErr := conn.Read(next[:]); n != 0 || !eventBudgetTransportDisconnect(readErr) {
					p.recordError("transport fixture held socket did not close")
				}
			}
			return
		default:
			p.recordError("transport fixture unexpected command")
			return
		}
		if _, err := io.WriteString(conn, reply); err != nil {
			p.recordError("transport fixture reply failed")
			return
		}
	}
	p.recordError("transport fixture exceeded command limit")
}

func readEventBudgetTransportCommand(reader *bufio.Reader) ([]string, error) {
	count, err := readEventBudgetTransportLength(reader, '*')
	if err != nil {
		return nil, err
	}
	if count < 1 || count > 32 {
		return nil, errors.New("transport fixture invalid array size")
	}
	args := make([]string, 0, count)
	for range count {
		length, err := readEventBudgetTransportLength(reader, '$')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if length > 1024 {
			return nil, errors.New("transport fixture invalid bulk size")
		}
		payload := make([]byte, length+2)
		if _, err := io.ReadFull(reader, payload); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if payload[length] != '\r' || payload[length+1] != '\n' {
			return nil, errors.New("transport fixture invalid bulk terminator")
		}
		args = append(args, string(payload[:length]))
	}
	return args, nil
}

func readEventBudgetTransportLength(reader *bufio.Reader, prefix byte) (int, error) {
	// ReadSlice never accumulates fragments. The peer uses a fixed 128-byte
	// buffer, so an unterminated header fails before growing any allocation.
	header, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return 0, errors.New("transport fixture header exceeds buffer")
	}
	if err != nil {
		if errors.Is(err, io.EOF) && len(header) > 0 {
			return 0, io.ErrUnexpectedEOF
		}
		return 0, err
	}
	if len(header) < 4 || len(header) > 128 || header[0] != prefix || header[len(header)-2] != '\r' {
		return 0, errors.New("transport fixture invalid header")
	}
	digits := header[1 : len(header)-2]
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return 0, errors.New("transport fixture invalid length")
		}
	}
	length, err := strconv.Atoi(string(digits))
	if err != nil {
		return 0, errors.New("transport fixture invalid length")
	}
	return length, nil
}

func eventBudgetTransportDisconnect(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET)
}

func (p *eventBudgetTransportPeer) awaitRelease() {
	select {
	case <-p.gate:
	case <-p.stop:
	case <-time.After(3 * time.Second):
		p.recordError("transport fixture release timed out")
	}
}

func (p *eventBudgetTransportPeer) release() {
	p.releaseOnce.Do(func() { close(p.gate) })
}

func (p *eventBudgetTransportPeer) recordError(message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = errors.Join(p.err, errors.New(message))
}

func (p *eventBudgetTransportPeer) closeSocket(conn net.Conn) {
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		p.recordError("transport fixture socket close failed")
	}
}

func (p *eventBudgetTransportPeer) Close() error {
	p.closeOnce.Do(func() {
		close(p.stop)
		if err := p.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			p.recordError("transport fixture listener close failed")
		}
		// Stop all Add calls before waiting for workers. No library workers
		// are included in this fixture-owned join.
		<-p.acceptDone
		p.mu.Lock()
		connections := make([]net.Conn, 0, len(p.connections))
		for conn := range p.connections {
			connections = append(connections, conn)
		}
		p.mu.Unlock()
		for _, conn := range connections {
			p.closeSocket(conn)
		}
		p.workers.Wait()
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Reject oversized headers without reading the remaining attacker-controlled tail.
func TestEventBudgetTransportRejectsOversizedHeaders(t *testing.T) {
	const maxHeaderRead = 128
	for _, test := range []struct {
		name, wire  string
		prefixBytes int
	}{
		{"late array terminator", "*" + strings.Repeat("x", 64*1024) + "\r\n", 0},
		{"unterminated array header", "*" + strings.Repeat("x", 64*1024), 0},
		{"late bulk-length terminator", "*1\r\n$" + strings.Repeat("x", 64*1024) + "\r\n", 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := bytes.NewReader([]byte(test.wire))
			_, err := readEventBudgetTransportCommand(bufio.NewReaderSize(input, maxHeaderRead))
			if err == nil {
				t.Fatal("malformed oversized RESP header was accepted")
			}
			consumed := len(test.wire) - input.Len()
			if consumed > test.prefixBytes+maxHeaderRead {
				t.Fatalf("parser consumed %d bytes, expected at most prefix %d plus header %d", consumed, test.prefixBytes, maxHeaderRead)
			}
		})
	}
}
