package websocket

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
)

const (
	budgetProxyConnections = 32
	budgetProxyIOTimeout   = 5 * time.Second
	budgetProxyFrameBytes  = 1024 * 1024
	budgetProxyFrameDepth  = 16
	budgetProxyCollection  = 1024
	budgetProxyFrameNodes  = 4096
)

type budgetProxyFault struct {
	hold     bool
	consumed chan struct{}
	release  chan struct{}
	once     sync.Once
}

// budgetTransportProxy forwards actual RESP bytes. Only a complete successful
// script response can consume an armed fault; NOSCRIPT and setup pass through.
type budgetTransportProxy struct {
	url         string
	upstream    string
	listener    net.Listener
	ctx         context.Context
	cancel      context.CancelFunc
	acceptDone  chan struct{}
	workers     sync.WaitGroup
	slots       chan struct{}
	scriptCalls atomic.Int64

	mu        sync.Mutex
	stopped   bool
	pairs     map[*budgetProxyPair]struct{}
	armed     *budgetProxyFault
	held      *budgetProxyFault
	err       error
	closeOnce sync.Once
	closeErr  error
}

type budgetProxyPair struct {
	mu     sync.Mutex
	down   net.Conn
	up     net.Conn
	closed bool
}

func newBudgetTransportProxy(t *testing.T, upstreamURL string) *budgetTransportProxy {
	t.Helper()
	if upstreamURL != redistest.URL(t) {
		t.Fatal("budget transport fixture requires the allocated isolated Redis URL")
	}
	parsed, err := url.Parse(upstreamURL)
	if err != nil || parsed.Scheme != "redis" {
		t.Fatal("budget transport fixture requires a plaintext Redis URL")
	}
	options := redistest.Options(t)
	if options.Network != "tcp" || options.TLSConfig != nil {
		t.Fatal("budget transport fixture Redis endpoint invalid")
	}
	ctx, cancel := context.WithCancel(t.Context())
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatal("budget transport fixture listener unavailable")
	}
	// Preserve the allocated DB, credentials, protocol and every URL option.
	// TLS is explicitly unsupported here, never silently downgraded.
	parsed.Host = listener.Addr().String()
	p := &budgetTransportProxy{
		url: parsed.String(), upstream: options.Addr, listener: listener,
		ctx: ctx, cancel: cancel, acceptDone: make(chan struct{}),
		slots: make(chan struct{}, budgetProxyConnections),
		pairs: make(map[*budgetProxyPair]struct{}),
	}
	go p.accept()
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err) // Only fixed fixture errors are retained.
		}
	})
	return p
}

func (p *budgetTransportProxy) URL() string { return p.url }

func (p *budgetTransportProxy) ScriptCalls() int { return int(p.scriptCalls.Load()) }

func (p *budgetTransportProxy) ArmDrop() <-chan struct{} { return p.arm(false) }

func (p *budgetTransportProxy) ArmHold() <-chan struct{} { return p.arm(true) }

func (p *budgetTransportProxy) arm(hold bool) <-chan struct{} {
	fault := &budgetProxyFault{hold: hold, consumed: make(chan struct{}), release: make(chan struct{})}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || p.armed != nil || p.held != nil {
		p.recordLocked("budget transport fixture cannot overlap faults")
		close(fault.consumed)
		return fault.consumed
	}
	p.armed = fault
	return fault.consumed
}

func (p *budgetTransportProxy) Release() {
	p.mu.Lock()
	fault := p.held
	p.mu.Unlock()
	if fault != nil {
		fault.once.Do(func() { close(fault.release) })
	}
}

func (p *budgetTransportProxy) takeFault() *budgetProxyFault {
	p.mu.Lock()
	defer p.mu.Unlock()
	fault := p.armed
	if fault != nil {
		p.armed = nil
		if fault.hold {
			p.held = fault
		}
		close(fault.consumed)
	}
	return fault
}

func (p *budgetTransportProxy) recordLocked(message string) {
	if p.err == nil {
		p.err = errors.New(message)
	}
}

func (p *budgetTransportProxy) record(message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recordLocked(message)
}

func (p *budgetTransportProxy) accept() {
	defer close(p.acceptDone)
	for {
		down, err := p.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) && p.ctx.Err() == nil {
				p.record("budget transport fixture accept failed")
			}
			return
		}
		p.mu.Lock()
		if p.stopped {
			p.mu.Unlock()
			p.closeConn(down)
			return
		}
		select {
		case p.slots <- struct{}{}:
		default:
			p.recordLocked("budget transport fixture connection bound exceeded")
			p.mu.Unlock()
			p.closeConn(down)
			continue
		}
		pair := &budgetProxyPair{down: down}
		p.pairs[pair] = struct{}{}
		p.workers.Add(1)
		p.mu.Unlock()
		go p.serve(pair)
	}
}

func (p *budgetTransportProxy) serve(pair *budgetProxyPair) {
	defer p.workers.Done()
	defer func() {
		p.closePair(pair)
		p.mu.Lock()
		delete(p.pairs, pair)
		p.mu.Unlock()
		<-p.slots
	}()
	up, err := (&net.Dialer{Timeout: budgetProxyIOTimeout}).DialContext(p.ctx, "tcp", p.upstream)
	if err != nil {
		if p.ctx.Err() == nil {
			p.record("budget transport fixture upstream dial failed")
		}
		return
	}
	pair.mu.Lock()
	if pair.closed {
		pair.mu.Unlock()
		p.closeConn(up)
		return
	}
	pair.up = up
	pair.mu.Unlock()
	downReader := bufio.NewReader(pair.down)
	upReader := bufio.NewReader(up)
	for {
		if err := pair.down.SetReadDeadline(time.Now().Add(budgetProxyIOTimeout)); err != nil {
			p.recordIO(err, "budget transport fixture request deadline failed")
			return
		}
		request, wire, err := budgetProxyReadFrame(downReader)
		if err != nil {
			if !budgetProxyDisconnected(err) {
				p.recordIO(err, "budget transport fixture request framing failed")
			}
			return
		}
		if request.kind != '*' || len(request.items) == 0 || request.items[0].kind != '$' {
			p.record("budget transport fixture request must be a command array")
			return
		}
		command := request.items[0].text
		script := strings.EqualFold(command, "EVALSHA") || strings.EqualFold(command, "EVAL")
		if script {
			p.scriptCalls.Add(1)
		}
		if err := budgetProxyWrite(up, wire); err != nil {
			p.recordIO(err, "budget transport fixture upstream write failed")
			return
		}
		reply, response, err := p.readReply(pair.down, up, upReader)
		if err != nil {
			p.recordIO(err, "budget transport fixture reply framing failed")
			return
		}
		if script && reply.kind != '-' && reply.kind != '!' {
			if fault := p.takeFault(); fault != nil {
				if !fault.hold || !p.waitHeld(fault) {
					return // Deferred socket closure drops the entire consumed reply.
				}
				// The assertion may cancel/close its client while the reply is held.
				// An undeliverable released reply is an expected injected fault.
				if err := budgetProxyWrite(pair.down, response); err != nil {
					return
				}
				continue
			}
		}
		if err := budgetProxyWrite(pair.down, response); err != nil {
			p.recordIO(err, "budget transport fixture downstream write failed")
			return
		}
	}
}

func (p *budgetTransportProxy) readReply(down, up net.Conn, reader *bufio.Reader) (budgetProxyFrame, []byte, error) {
	if err := up.SetReadDeadline(time.Now().Add(budgetProxyIOTimeout)); err != nil {
		return budgetProxyFrame{}, nil, err
	}
	var response []byte
	for frames := 0; frames < budgetProxyCollection; frames++ {
		frame, wire, err := budgetProxyReadFrame(reader)
		if err != nil {
			return budgetProxyFrame{}, nil, err
		}
		if frame.kind == '>' {
			// Pushes are independent of the command response. Maintenance is
			// disabled by the real constructor, but RESP3 framing stays intact.
			if err := budgetProxyWrite(down, wire); err != nil {
				return budgetProxyFrame{}, nil, err
			}
			continue
		}
		if len(wire) > budgetProxyFrameBytes-len(response) {
			return budgetProxyFrame{}, nil, errors.New("budget transport fixture reply exceeds bound")
		}
		response = append(response, wire...)
		if frame.kind != '|' {
			return frame, response, nil
		}
	}
	return budgetProxyFrame{}, nil, errors.New("budget transport fixture reply frame count exceeds bound")
}

func (p *budgetTransportProxy) waitHeld(fault *budgetProxyFault) bool {
	defer func() {
		p.mu.Lock()
		if p.held == fault {
			p.held = nil
		}
		p.mu.Unlock()
	}()
	timer := time.NewTimer(budgetProxyIOTimeout)
	defer timer.Stop()
	select {
	case <-fault.release:
		return true
	case <-p.ctx.Done():
		return false
	case <-timer.C:
		p.record("budget transport fixture held reply was not released")
		return false
	}
}

func budgetProxyWrite(conn net.Conn, wire []byte) error {
	if err := conn.SetWriteDeadline(time.Now().Add(budgetProxyIOTimeout)); err != nil {
		return err
	}
	_, err := io.Copy(conn, bytes.NewReader(wire))
	return err
}

func budgetProxyDisconnected(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

func (p *budgetTransportProxy) recordIO(err error, message string) {
	// Only our teardown makes a closed local socket expected here. Upstream
	// EOF/reset and parser failures must surface through checked fixture Close.
	// Client disconnects while reading a request and released fault replies are
	// handled explicitly at those two boundaries instead.
	if p.ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
		p.record(message)
	}
}

func (p *budgetTransportProxy) closeConn(conn net.Conn) {
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		p.record("budget transport fixture connection cleanup failed")
	}
}

func (p *budgetTransportProxy) closePair(pair *budgetProxyPair) {
	pair.mu.Lock()
	if pair.closed {
		pair.mu.Unlock()
		return
	}
	pair.closed = true
	up := pair.up
	pair.mu.Unlock()
	p.closeConn(pair.down)
	if up != nil {
		p.closeConn(up)
	}
}

func (p *budgetTransportProxy) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.stopped = true
		pairs := make([]*budgetProxyPair, 0, len(p.pairs))
		for pair := range p.pairs {
			pairs = append(pairs, pair)
		}
		p.mu.Unlock()
		p.cancel()
		if err := p.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			p.record("budget transport fixture listener cleanup failed")
		}
		for _, pair := range pairs {
			p.closePair(pair)
		}
		<-p.acceptDone
		p.workers.Wait()
		p.mu.Lock()
		p.closeErr = p.err
		p.mu.Unlock()
	})
	return p.closeErr
}

type budgetProxyFrame struct {
	kind  byte
	text  string
	items []budgetProxyFrame
}

type budgetProxyParser struct {
	reader *bufio.Reader
	wire   bytes.Buffer
	nodes  int
}

func budgetProxyReadFrame(reader *bufio.Reader) (budgetProxyFrame, []byte, error) {
	parser := budgetProxyParser{reader: reader}
	frame, err := parser.read(0)
	return frame, parser.wire.Bytes(), err
}

func (p *budgetProxyParser) append(data []byte) error {
	if len(data) > budgetProxyFrameBytes-p.wire.Len() {
		return errors.New("budget transport fixture RESP size exceeds bound")
	}
	// bytes.Buffer.Write cannot fail; its error is nevertheless checked.
	if _, err := p.wire.Write(data); err != nil {
		return errors.New("budget transport fixture RESP buffering failed")
	}
	return nil
}

func (p *budgetProxyParser) read(depth int) (budgetProxyFrame, error) {
	p.nodes++
	if depth > budgetProxyFrameDepth || p.nodes > budgetProxyFrameNodes {
		return budgetProxyFrame{}, errors.New("budget transport fixture RESP nesting exceeds bound")
	}
	// ReadSlice is bounded by the reader's fixed buffer; header lines cannot
	// grow without limit. Bulk data is read separately after checking length.
	line, err := p.reader.ReadSlice('\n')
	if err != nil {
		return budgetProxyFrame{}, err
	}
	if len(line) < 3 || line[len(line)-2] != '\r' {
		return budgetProxyFrame{}, errors.New("budget transport fixture RESP header invalid")
	}
	if err := p.append(line); err != nil {
		return budgetProxyFrame{}, err
	}
	frame := budgetProxyFrame{kind: line[0], text: string(line[1 : len(line)-2])}
	switch frame.kind {
	case '+', '-', ':', ',', '(', '#', '_':
		return frame, nil
	case '$', '!', '=':
		return p.bulk(frame)
	case '*', '%', '~', '>', '|':
		return p.collection(frame, depth)
	default:
		return budgetProxyFrame{}, errors.New("budget transport fixture RESP type unsupported")
	}
}

func (p *budgetProxyParser) bulk(frame budgetProxyFrame) (budgetProxyFrame, error) {
	length, err := strconv.Atoi(frame.text)
	if err != nil || length < -1 || (length == -1 && frame.kind != '$') {
		return budgetProxyFrame{}, errors.New("budget transport fixture RESP bulk length invalid")
	}
	if length == -1 {
		return frame, nil
	}
	if length > budgetProxyFrameBytes-p.wire.Len()-2 {
		return budgetProxyFrame{}, errors.New("budget transport fixture RESP bulk exceeds bound")
	}
	data := make([]byte, length+2)
	if _, err := io.ReadFull(p.reader, data); err != nil {
		return budgetProxyFrame{}, err
	}
	if data[length] != '\r' || data[length+1] != '\n' {
		return budgetProxyFrame{}, errors.New("budget transport fixture RESP bulk terminator invalid")
	}
	if err := p.append(data); err != nil {
		return budgetProxyFrame{}, err
	}
	frame.text = string(data[:length])
	return frame, nil
}

func (p *budgetProxyParser) collection(frame budgetProxyFrame, depth int) (budgetProxyFrame, error) {
	length, err := strconv.Atoi(frame.text)
	if err != nil || length < -1 || length > budgetProxyCollection || (length == -1 && frame.kind != '*') {
		return budgetProxyFrame{}, errors.New("budget transport fixture RESP collection length invalid")
	}
	if length == -1 {
		return frame, nil
	}
	if frame.kind == '%' || frame.kind == '|' {
		length *= 2
	}
	frame.items = make([]budgetProxyFrame, length)
	for index := range frame.items {
		item, err := p.read(depth + 1)
		if err != nil {
			return budgetProxyFrame{}, err
		}
		frame.items[index] = item
	}
	return frame, nil
}
