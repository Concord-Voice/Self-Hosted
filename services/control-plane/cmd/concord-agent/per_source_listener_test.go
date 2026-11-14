package main

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/hostagent"
)

type queuedListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newQueuedListener() *queuedListener {
	return &queuedListener{conns: make(chan net.Conn, 16), closed: make(chan struct{})}
}

func (l *queuedListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *queuedListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *queuedListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4zero} }

type sourceConn struct {
	net.Conn
	remote net.Addr
}

func (c sourceConn) RemoteAddr() net.Addr { return c.remote }

func queueTCPConn(t *testing.T, l *queuedListener, ip net.IP) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	select {
	case l.conns <- sourceConn{Conn: server, remote: &net.TCPAddr{IP: ip}}:
	case <-time.After(time.Second):
		_ = client.Close()
		_ = server.Close()
		t.Fatal("listener did not accept queued connection")
	}
	return client
}

func TestLimitPerSourceRejectsFifthAndKeepsOtherSourcesAvailable(t *testing.T) {
	raw := newQueuedListener()
	limited := limitPerSource(raw, 4)
	t.Cleanup(func() { _ = limited.Close() })

	held := make([]net.Conn, 0, 4)
	for range 4 {
		client := queueTCPConn(t, raw, net.ParseIP("192.0.2.1"))
		conn, err := limited.Accept()
		if err != nil {
			_ = client.Close()
			t.Fatal(err)
		}
		held = append(held, conn)
		t.Cleanup(func() { _ = client.Close() })
	}
	for _, conn := range held {
		conn := conn
		t.Cleanup(func() { _ = conn.Close() })
	}

	fifth := queueTCPConn(t, raw, net.ParseIP("::ffff:192.0.2.1"))
	t.Cleanup(func() { _ = fifth.Close() })
	bClient := queueTCPConn(t, raw, net.ParseIP("192.0.2.2"))
	t.Cleanup(func() { _ = bClient.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := limited.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	select {
	case conn := <-accepted:
		t.Cleanup(func() { _ = conn.Close() })
		if got, ok := canonicalTCPSource(conn.RemoteAddr()); !ok || got != "192.0.2.2" {
			t.Fatalf("accepted source %q, ok=%v; want 192.0.2.2", got, ok)
		}
	case <-time.After(time.Second):
		t.Fatal("second source was blocked by first source's limit")
	}

	if _, err := fifth.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
		t.Fatalf("fifth connection was not promptly closed: %v", err)
	}

	if err := held[0].Close(); err != nil {
		t.Fatal(err)
	}
	if err := held[0].Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	client := queueTCPConn(t, raw, net.ParseIP("192.0.2.1"))
	t.Cleanup(func() { _ = client.Close() })
	conn, err := limited.Accept()
	if err != nil {
		t.Fatalf("slot was not released after close: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
}

func TestLimitPerSourceRefusesNonTCPConnections(t *testing.T) {
	raw := newQueuedListener()
	limited := limitPerSource(raw, 1)
	t.Cleanup(func() { _ = limited.Close() })

	nonTCPClient, nonTCPServer := net.Pipe()
	select {
	case raw.conns <- nonTCPServer:
	case <-time.After(time.Second):
		t.Fatal("listener did not accept non-TCP test connection")
	}
	t.Cleanup(func() { _ = nonTCPClient.Close() })

	client := queueTCPConn(t, raw, net.ParseIP("192.0.2.2"))
	t.Cleanup(func() { _ = client.Close() })
	conn, err := limited.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if got, ok := canonicalTCPSource(conn.RemoteAddr()); !ok || got != "192.0.2.2" {
		t.Fatalf("accepted source %q, ok=%v; want 192.0.2.2", got, ok)
	}
	if _, err := nonTCPClient.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
		t.Fatalf("non-TCP connection was not closed: %v", err)
	}
}

func TestLimitPerSourceReleasesAfterTLSHandshakeFailure(t *testing.T) {
	cfg, _, err := hostagent.SelfSignedTLS(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	limited := limitPerSource(raw, 1)
	server := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(tls.NewListener(limited, cfg)) }()

	plain, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	listener := limited.(*perSourceListener)
	if !waitForSourceSlots(listener, "127.0.0.1", 1, time.Second) {
		t.Fatal("TLS handshake did not reserve source slot")
	}
	if _, err := plain.Write([]byte("not tls")); err != nil {
		t.Fatal(err)
	}
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	if !waitForSourceSlots(listener, "127.0.0.1", 0, time.Second) {
		t.Fatal("TLS handshake failure held source slot")
	}

	conn, err := tls.Dial("tcp", raw.Addr().String(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec // self-signed test listener
	if err != nil {
		t.Fatalf("source slot was not released for TLS: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}

func waitForSourceSlots(listener *perSourceListener, source string, want int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		listener.mu.Lock()
		active := listener.active[source]
		listener.mu.Unlock()
		if active == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
