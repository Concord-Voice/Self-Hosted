package admin

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// hostAgentSocket is where docker-compose.host-agent.yml mounts the Concord
// Host Agent's socket (#186). Without the overlay it does not exist and every
// Server-area call answers 503.
const hostAgentSocket = "/run/concord-agent/agent.sock"

const (
	maxAgentJSON = 1 << 20
	// 1000 capped lines plus the agent's "output incomplete" marker line.
	maxAgentLogs      = 1000*4097 + 64
	defaultLogLines   = 200
	maxLogLines       = 1000
	auditWriteTimeout = 5 * time.Second
	// agentTimeout sits above the agent's 60-second health budget.
	agentTimeout = 70 * time.Second
	// writeGrace is the time left to send an answer once the agent replies.
	writeGrace = 5 * time.Second
)

// serverLogServices mirrors the agent's own allowlist; the agent re-checks it.
var serverLogServices = map[string]bool{
	"control-plane": true, "media-plane": true, "postgres": true, "redis": true,
	"nats": true, "minio": true, "coturn": true,
}

type auditWriter interface {
	Write(ctx context.Context, ev AuditEvent) error
}

// ServerArea serves the Server area of the Admin Console by asking the host
// agent. Every call, allowed or not, writes one audit row.
type ServerArea struct {
	agent *http.Client
	audit auditWriter
	log   *logger.Logger
}

// NewServerArea returns a ServerArea that talks to the agent on its socket.
func NewServerArea(audit auditWriter, log *logger.Logger) *ServerArea {
	return &ServerArea{agent: unixClient(hostAgentSocket), audit: audit, log: log}
}

func unixClient(sock string) *http.Client {
	return &http.Client{
		Timeout: agentTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
}

// Status returns the agent's `status --json` object.
func (s *ServerArea) Status(c *gin.Context) { s.proxyJSON(c, "status", "/v1/status") }

// Health returns the agent's `health --json` object.
func (s *ServerArea) Health(c *gin.Context) { s.proxyJSON(c, "health", "/v1/health") }

// Logs returns the last lines of one service's logs as plain text.
func (s *ServerArea) Logs(c *gin.Context) {
	service := c.Query("service")
	lines := defaultLogLines
	valid := serverLogServices[service]
	if v := c.Query("lines"); valid && v != "" {
		n, err := strconv.Atoi(v)
		valid = err == nil && n >= 1 && n <= maxLogLines
		lines = n
	}
	if !valid {
		s.record(c, "logs", "", AuditDenied, "invalid_request")
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	q := url.Values{"service": {service}, "lines": {strconv.Itoa(lines)}}
	body, ok := s.fetch(c, "logs", service, "/v1/logs?"+q.Encode(), maxAgentLogs)
	if !ok {
		return
	}
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, "text/plain; charset=utf-8", body)
}

func (s *ServerArea) proxyJSON(c *gin.Context, action, path string) {
	body, ok := s.fetch(c, action, "", path, maxAgentJSON)
	if !ok {
		return
	}
	c.Data(http.StatusOK, contentTypeJSON, body)
}

// fetch asks the agent and writes the error response itself when it fails.
func (s *ServerArea) fetch(c *gin.Context, action, service, path string, limit int64) ([]byte, bool) {
	// The control plane's server-wide WriteTimeout (15 s) is shorter than a
	// health run, so extend it for this response or the answer is dropped.
	_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(agentTimeout + writeGrace))
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, "http://agent"+path, nil)
	if err != nil {
		s.fail(c, action, service, http.StatusInternalServerError, "internal_error")
		return nil, false
	}
	resp, err := s.agent.Do(req)
	if err != nil {
		s.fail(c, action, service, http.StatusServiceUnavailable, "agent_unavailable")
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		s.fail(c, action, service, http.StatusTooManyRequests, "agent_busy")
		return nil, false
	case resp.StatusCode != http.StatusOK:
		s.fail(c, action, service, http.StatusBadGateway, "agent_error")
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(body)) > limit || (limit == maxAgentJSON && !json.Valid(body)) {
		s.fail(c, action, service, http.StatusBadGateway, "agent_error")
		return nil, false
	}
	s.record(c, action, service, AuditSuccess, "")
	return body, true
}

func (s *ServerArea) fail(c *gin.Context, action, service string, code int, reason string) {
	s.record(c, action, service, AuditFailure, reason)
	c.JSON(code, gin.H{"error": reason})
}

func (s *ServerArea) record(c *gin.Context, action, service, result, reason string) {
	detail := map[string]any{"action": action}
	if service != "" {
		detail["service"] = service
	}
	s.writeAudit(c, EventServerRead, detail, result, reason)
}

// recordWrite writes the server_write row for one write attempt.
func (s *ServerArea) recordWrite(c *gin.Context, detail map[string]any, result, reason string) {
	s.writeAudit(c, EventServerWrite, detail, result, reason)
}

func (s *ServerArea) writeAudit(c *gin.Context, event string, detail map[string]any, result, reason string) {
	if reason != "" {
		detail["reason"] = reason
	}
	ev := AuditEvent{EventType: event, Result: result, Detail: detail}
	if id := c.GetString(adminIDContextKey); id != "" {
		ev.AdminID = &id
	}
	// A client that leaves mid-call must not cancel the row: the agent has
	// already run the command.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), auditWriteTimeout)
	defer cancel()
	if err := s.audit.Write(ctx, ev); err != nil {
		s.log.Warn("admin audit write failed", "error", err)
	}
}
