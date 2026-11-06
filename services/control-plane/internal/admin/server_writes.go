package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/redis/go-redis/v9"
)

// Server write actions (#188): the console asks the host agent to run one
// fixed concord-selfhost action and gets a job id back to poll. Actions that
// take the server down or replace its data need a fresh WebAuthn assertion
// bound to that action; every attempt writes one server_write audit row.

const (
	serverStepUpPrefix = "admin_server_stepup:"
	// serverStepUpTTL is short: the operator taps the key right after asking.
	serverStepUpTTL = 2 * time.Minute
	// maxServerActionBody leaves room for a WebAuthn assertion.
	maxServerActionBody = 64 << 10
	maxAgentJob         = 128 << 10
)

// serverActions maps each write action to whether it needs a fresh WebAuthn
// assertion. Start and backup neither stop the server nor change its data.
var serverActions = map[string]bool{
	"start": false, "backup": false,
	"stop": true, "restart": true, "update": true, "restore": true,
}

// These mirror the agent's own checks; the agent re-checks them.
var (
	serverUpdateTag     = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)
	serverBackupArchive = regexp.MustCompile(`^concord-backup-[0-9]{8}T[0-9]{6}Z\.tar\.gz$`)
	serverJobID         = regexp.MustCompile(`^[0-9a-f]{32}$`)
	serverStepUpHandle  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// agentRefusals are the agent's own error codes, passed through unchanged.
var agentRefusals = map[string]bool{
	"invalid_tag": true, "invalid_archive": true, "unexpected_argument": true,
	"invalid_body": true, "untrusted_backups": true, "unknown_archive": true,
	"security_log_boundary_missing": true, "job_unavailable": true,
	"write_in_progress": true, "unknown_action": true, "unknown_job": true,
}

type serverStepUp struct {
	AdminID string               `json:"admin_id"`
	Action  string               `json:"action"`
	Session webauthn.SessionData `json:"session"`
}

type serverActionRequest struct {
	Tag     string `json:"tag,omitempty"`
	Archive string `json:"archive,omitempty"`
	StepUp  *struct {
		Handle    string          `json:"handle"`
		Assertion json.RawMessage `json:"assertion"`
	} `json:"stepup,omitempty"`
}

// ServerStepUpBegin starts a WebAuthn assertion for one step-up action and
// returns its handle. POST {"action": "..."}.
func (h *Handler) ServerStepUpBegin(c *gin.Context) {
	var req struct {
		Action string `json:"action"`
	}
	if decodeStrict(c, &req, 1<<10) != nil || !serverActions[req.Action] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	adminID := c.GetString(adminIDContextKey)
	user, err := h.webAuthnUserForAdmin(c.Request.Context(), adminID)
	if err != nil {
		h.log.Error("admin step-up user build failed", "error", err)
		c.JSON(http.StatusForbidden, gin.H{"error": "stepup_unavailable"})
		return
	}
	assertion, session, err := h.webauthn.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		h.log.Error("admin step-up begin failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error"})
		return
	}
	handle, err := h.storeServerStepUp(c.Request.Context(), serverStepUp{AdminID: adminID, Action: req.Action, Session: *session})
	if err != nil {
		h.log.Error("admin step-up store failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error"})
		return
	}
	c.JSON(http.StatusOK, passwordResponse{Handle: handle, PublicKey: assertion})
}

// ServerAction asks the agent to start one write action and returns its job.
func (h *Handler) ServerAction(c *gin.Context) {
	action := c.Param("action")
	needsStepUp, known := serverActions[action]
	if !known {
		h.server.recordWrite(c, map[string]any{"action": "unknown"}, AuditDenied, "unknown_action")
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown_action"})
		return
	}
	detail := map[string]any{"action": action}
	deny := func(code int, reason string) {
		h.server.recordWrite(c, detail, AuditDenied, reason)
		c.JSON(code, gin.H{"error": reason})
	}
	var req serverActionRequest
	if decodeStrict(c, &req, maxServerActionBody) != nil {
		deny(http.StatusBadRequest, "invalid_request")
		return
	}
	switch {
	case action == "update" && !serverUpdateTag.MatchString(req.Tag),
		action == "restore" && !serverBackupArchive.MatchString(req.Archive),
		action != "update" && req.Tag != "",
		action != "restore" && req.Archive != "":
		deny(http.StatusBadRequest, "invalid_request")
		return
	}
	if req.Tag != "" {
		detail["tag"] = req.Tag
	}
	if req.Archive != "" {
		detail["archive"] = req.Archive
	}
	if needsStepUp {
		if req.StepUp == nil {
			deny(http.StatusForbidden, "stepup_required")
			return
		}
		if !h.verifyServerStepUp(c.Request.Context(), c.GetString(adminIDContextKey), action, req.StepUp.Handle, req.StepUp.Assertion) {
			deny(http.StatusForbidden, "stepup_failed")
			return
		}
		detail["stepup"] = true
	}
	// Send only the arguments the action takes; the agent refuses the rest.
	args := map[string]string{}
	if req.Tag != "" {
		args["tag"] = req.Tag
	}
	if req.Archive != "" {
		args["archive"] = req.Archive
	}
	payload, err := json.Marshal(args)
	if err != nil {
		deny(http.StatusInternalServerError, "internal_error")
		return
	}
	h.server.startJob(c, action, detail, payload)
}

// ServerJobs returns the agent's recent jobs.
func (s *ServerArea) ServerJobs(c *gin.Context) { s.proxyJSON(c, "jobs", "/v1/jobs") }

// ServerJob returns one job by id.
func (s *ServerArea) ServerJob(c *gin.Context) {
	id := c.Param("id")
	if !serverJobID.MatchString(id) {
		s.record(c, "job", "", AuditDenied, "invalid_request")
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	s.proxyJob(c, id)
}

func (s *ServerArea) proxyJob(c *gin.Context, id string) {
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, "http://agent/v1/jobs/"+id, nil)
	if err != nil {
		s.fail(c, "job", "", http.StatusInternalServerError, "internal_error")
		return
	}
	resp, err := s.agent.Do(req)
	if err != nil {
		s.fail(c, "job", "", http.StatusServiceUnavailable, "agent_unavailable")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAgentJob+1))
	if err != nil || len(body) > maxAgentJob || !json.Valid(body) {
		s.fail(c, "job", "", http.StatusBadGateway, "agent_error")
		return
	}
	if resp.StatusCode == http.StatusNotFound {
		s.fail(c, "job", "", http.StatusNotFound, "unknown_job")
		return
	}
	if resp.StatusCode != http.StatusOK {
		s.fail(c, "job", "", http.StatusBadGateway, "agent_error")
		return
	}
	s.record(c, "job", "", AuditSuccess, "")
	c.Data(http.StatusOK, "application/json", body)
}

// startJob posts the action to the agent and relays the job or its refusal.
func (s *ServerArea) startJob(c *gin.Context, action string, detail map[string]any, payload []byte) {
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "http://agent/v1/actions/"+action, bytes.NewReader(payload))
	if err != nil {
		s.writeFail(c, detail, http.StatusInternalServerError, "internal_error")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.agent.Do(req)
	if err != nil {
		s.writeFail(c, detail, http.StatusServiceUnavailable, "agent_unavailable")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAgentJob+1))
	if err != nil || len(body) > maxAgentJob {
		s.writeFail(c, detail, http.StatusBadGateway, "agent_error")
		return
	}
	if resp.StatusCode != http.StatusAccepted {
		var refusal struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &refusal) == nil && agentRefusals[refusal.Error] && resp.StatusCode >= 400 && resp.StatusCode < 600 {
			s.writeFail(c, detail, resp.StatusCode, refusal.Error)
			return
		}
		s.writeFail(c, detail, http.StatusBadGateway, "agent_error")
		return
	}
	var job struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &job) != nil || !serverJobID.MatchString(job.ID) {
		// The agent may have started it; say so in the audit row.
		s.writeFail(c, detail, http.StatusBadGateway, "agent_error")
		return
	}
	detail["job_id"] = job.ID
	s.recordWrite(c, detail, AuditSuccess, "")
	c.Data(http.StatusAccepted, "application/json", body)
}

func (s *ServerArea) writeFail(c *gin.Context, detail map[string]any, code int, reason string) {
	s.recordWrite(c, detail, AuditFailure, reason)
	c.JSON(code, gin.H{"error": reason})
}

// verifyServerStepUp redeems a step-up handle once. It must belong to this
// admin and this action, and the assertion must verify with UV.
func (h *Handler) verifyServerStepUp(ctx context.Context, adminID, action, handle string, assertion json.RawMessage) bool {
	if adminID == "" || !serverStepUpHandle.MatchString(handle) {
		return false
	}
	raw, err := h.redis.GetDel(ctx, serverStepUpPrefix+handle).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			h.log.Warn("admin step-up load failed", "error", err)
		}
		return false
	}
	var ch serverStepUp
	if json.Unmarshal([]byte(raw), &ch) != nil || ch.AdminID != adminID || ch.Action != action {
		return false
	}
	user, err := h.webAuthnUserForAdmin(ctx, adminID)
	if err != nil {
		return false
	}
	cred, err := h.webauthn.FinishLoginWithBytes(user, ch.Session, assertion)
	if err != nil {
		h.log.Warn("admin step-up assertion rejected", "error", err)
		return false
	}
	if err := h.repo.UpdateCredentialSignCount(ctx, cred.ID, int64(cred.Authenticator.SignCount)); err != nil {
		h.log.Warn("admin sign-count persist failed", "error", err)
	}
	return true
}

func (h *Handler) storeServerStepUp(ctx context.Context, ch serverStepUp) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate step-up handle: %w", err)
	}
	handle := hex.EncodeToString(raw)
	payload, err := json.Marshal(ch)
	if err != nil {
		return "", fmt.Errorf("marshal step-up: %w", err)
	}
	if err := h.redis.Set(ctx, serverStepUpPrefix+handle, payload, serverStepUpTTL).Err(); err != nil {
		return "", fmt.Errorf("store step-up: %w", err)
	}
	return handle, nil
}

// decodeStrict reads exactly one JSON object with no unknown fields. An empty
// body decodes as {}.
func decodeStrict(c *gin.Context, dst any, limit int64) error {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return errors.New("body too large")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}
