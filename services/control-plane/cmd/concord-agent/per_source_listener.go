package main

import (
	"net"
	"sync"
)

// limitPerSource bounds connections held by one canonical TCP source. A
// connection with no TCP source is refused before TLS can admit it.
func limitPerSource(inner net.Listener, maximum int) net.Listener {
	return &perSourceListener{Listener: inner, max: maximum, active: make(map[string]int)}
}

type perSourceListener struct {
	net.Listener
	max int

	mu     sync.Mutex
	active map[string]int
}

func (l *perSourceListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		source, ok := canonicalTCPSource(conn.RemoteAddr())
		if !ok || !l.reserve(source) {
			_ = conn.Close()
			continue
		}
		return &perSourceConn{Conn: conn, release: func() { l.release(source) }}, nil
	}
}

func canonicalTCPSource(addr net.Addr) (string, bool) {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || tcp.IP == nil {
		return "", false
	}
	if ipv4 := tcp.IP.To4(); ipv4 != nil {
		return ipv4.String(), true
	}
	return tcp.IP.String(), true
}

func (l *perSourceListener) reserve(source string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[source] >= l.max {
		return false
	}
	l.active[source]++
	return true
}

func (l *perSourceListener) release(source string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[source] == 1 {
		delete(l.active, source)
		return
	}
	l.active[source]--
}

type perSourceConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *perSourceConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
