package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
)

// Server write actions (#188): the console asks the host agent to run one
// fixed concord-selfhost action and gets a job id back to poll. The root agent
// owns the fresh WebAuthn ceremony and binds its one-use proof to every action.

const (
	// maxServerActionBody leaves room for a WebAuthn assertion.
	maxServerActionBody = 64 << 10
	maxAgentJob         = 128 << 10
	contentTypeJSON     = "application/json"
)

// serverActions is a closed mirror of the agent's write-action allowlist.
var serverActions = map[string]struct{}{
	"start": {}, "backup": {}, "stop": {}, "restart": {}, "update": {}, "restore": {},
}

// These mirror the agent's own checks; the agent re-checks them.
var (
	serverUpdateTag     = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)
	serverBackupArchive = regexp.MustCompile(`^concord-backup-[0-9]{8}T[0-9]{6}Z\.tar\.gz$`)
	serverJobID         = regexp.MustCompile(`^[0-9a-f]{32}$`)
	serverStepUpHandle  = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

// agentRefusals are the agent's own error codes, passed through unchanged.
var agentRefusals = map[string]bool{
	"invalid_tag": true, "invalid_archive": true, "unexpected_argument": true,
	"invalid_body": true, "untrusted_backups": true, "unknown_archive": true,
	"security_log_boundary_missing": true, "job_unavailable": true,
	"write_in_progress": true, "unknown_action": true, "unknown_job": true,
	"appliance_managed": true, "backup_rate_limited": true,
}

type serverActionRequest struct {
	Tag     string `json:"tag,omitempty"`
	Archive string `json:"archive,omitempty"`
	StepUp  *struct {
		Handle    string          `json:"handle"`
		Assertion json.RawMessage `json:"assertion"`
	} `json:"stepup,omitempty"`
}

type serverStepUpRequest struct {
	Action  string `json:"action"`
	Tag     string `json:"tag,omitempty"`
	Archive string `json:"archive,omitempty"`
}

// ServerStepUpBegin asks the root agent for an action-bound WebAuthn ceremony.
// POST {"action":"...","tag":"...","archive":"..."}; the agent stores
// the validated arguments and only accepts the returned proof for that action.
func (h *Handler) ServerStepUpBegin(c *gin.Context) {
	var req serverStepUpRequest
	detail := map[string]any{"phase": "stepup_begin"}
	fail := func(code int, result, reason string) {
		h.server.recordWrite(c, detail, result, reason)
		c.JSON(code, gin.H{"error": reason})
	}
	if decodeStrict(c, &req, 1<<10) != nil {
		detail["action"] = "unknown"
		fail(http.StatusBadRequest, AuditDenied, "invalid_request")
		return
	}
	if _, known := serverActions[req.Action]; !known {
		detail["action"] = "unknown"
		fail(http.StatusBadRequest, AuditDenied, "invalid_request")
		return
	}
	detail["action"] = req.Action
	args, ok := serverActionArgs(req.Action, serverActionRequest{Tag: req.Tag, Archive: req.Archive})
	if !ok {
		fail(http.StatusBadRequest, AuditDenied, "invalid_request")
		return
	}
	for k, v := range args {
		detail[k] = v
	}
	payload, err := json.Marshal(struct {
		AdminID   string            `json:"admin_id"`
		Action    string            `json:"action"`
		Arguments map[string]string `json:"arguments"`
	}{c.GetString(adminIDContextKey), req.Action, args})
	if err != nil {
		fail(http.StatusInternalServerError, AuditFailure, "internal_error")
		return
	}
	agentReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "http://agent/v1/authorizations", bytes.NewReader(payload))
	if err != nil {
		fail(http.StatusInternalServerError, AuditFailure, "internal_error")
		return
	}
	agentReq.Header.Set("Content-Type", contentTypeJSON)
	resp, err := h.server.agent.Do(agentReq)
	if err != nil {
		fail(http.StatusServiceUnavailable, AuditFailure, "stepup_unavailable")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAgentJob+1))
	if err != nil || len(body) > maxAgentJob {
		fail(http.StatusBadGateway, AuditFailure, "agent_error")
		return
	}
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusForbidden:
			fail(http.StatusForbidden, AuditDenied, "stepup_failed")
		case http.StatusServiceUnavailable:
			fail(http.StatusServiceUnavailable, AuditFailure, "stepup_unavailable")
		case http.StatusTooManyRequests:
			fail(http.StatusTooManyRequests, AuditFailure, "stepup_unavailable")
		default:
			fail(http.StatusBadGateway, AuditFailure, "agent_error")
		}
		return
	}
	var authorization struct {
		Handle  string          `json:"handle"`
		Options json.RawMessage `json:"options"`
	}
	if json.Unmarshal(body, &authorization) != nil || !serverStepUpHandle.MatchString(authorization.Handle) || !json.Valid(authorization.Options) {
		fail(http.StatusBadGateway, AuditFailure, "agent_error")
		return
	}
	h.server.recordWrite(c, detail, AuditSuccess, "")
	c.JSON(http.StatusOK, gin.H{"handle": authorization.Handle, "publicKey": authorization.Options})
}

// ServerAction asks the agent to start one write action and returns its job.
func (h *Handler) ServerAction(c *gin.Context) {
	action := c.Param("action")
	_, known := serverActions[action]
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
	if _, ok := serverActionArgs(action, req); !ok {
		deny(http.StatusBadRequest, "invalid_request")
		return
	}
	if req.StepUp == nil || !serverStepUpHandle.MatchString(req.StepUp.Handle) || len(req.StepUp.Assertion) == 0 {
		deny(http.StatusForbidden, "stepup_required")
		return
	}
	detail["stepup"] = true
	payload, err := json.Marshal(req.StepUp)
	if err != nil {
		deny(http.StatusInternalServerError, "internal_error")
		return
	}
	h.server.startJob(c, action, detail, payload)
}

// serverActionArgs checks the tag and archive and returns only the arguments
// the action takes; the agent refuses the rest.
func serverActionArgs(action string, req serverActionRequest) (map[string]string, bool) {
	args := map[string]string{}
	switch {
	case action == "update" && !serverUpdateTag.MatchString(req.Tag),
		action == "restore" && !serverBackupArchive.MatchString(req.Archive),
		action != "update" && req.Tag != "",
		action != "restore" && req.Archive != "":
		return nil, false
	case action == "update":
		args["tag"] = req.Tag
	case action == "restore":
		args["archive"] = req.Archive
	}
	return args, true
}

// ServerJobs returns the agent's recent jobs.
func (s *ServerArea) ServerJobs(c *gin.Context) { s.proxyJSON(c, "jobs", "/v1/jobs") }

// ServerBackups lists the archives restore accepts.
func (s *ServerArea) ServerBackups(c *gin.Context) { s.proxyJSON(c, "backups", "/v1/backups") }

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
	if resp.StatusCode == http.StatusConflict {
		s.agentConflict(c, "job", "", bytes.NewReader(body))
		return
	}
	if resp.StatusCode != http.StatusOK {
		s.fail(c, "job", "", http.StatusBadGateway, "agent_error")
		return
	}
	s.record(c, "job", "", AuditSuccess, "")
	c.Data(http.StatusOK, contentTypeJSON, body)
}

// startJob records the request, posts the action to the agent and relays the
// job or its refusal. The request row is written first and the agent is not
// called if that write fails, so no write runs unaudited. The agent call does
// not follow the client: once sent, the job may run, and the outcome row must
// say what happened rather than that the client left.
func (s *ServerArea) startJob(c *gin.Context, action string, detail map[string]any, payload []byte) {
	if err := s.writeAudit(c, EventServerWrite, requestedRow(detail), AuditSuccess, ""); err != nil {
		s.writeFail(c, detail, http.StatusServiceUnavailable, "audit_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), agentTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/v1/actions/"+action, bytes.NewReader(payload))
	if err != nil {
		s.writeFail(c, detail, http.StatusInternalServerError, "internal_error")
		return
	}
	req.Header.Set("Content-Type", contentTypeJSON)
	resp, err := s.agent.Do(req)
	if err != nil {
		// The request may have reached the agent before the transport failed.
		detail["may_have_started"] = true
		s.writeFail(c, detail, http.StatusServiceUnavailable, "agent_unavailable")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAgentJob+1))
	if err != nil || len(body) > maxAgentJob {
		detail["may_have_started"] = resp.StatusCode == http.StatusAccepted
		s.writeFail(c, detail, http.StatusBadGateway, "agent_error")
		return
	}
	if resp.StatusCode != http.StatusAccepted {
		var refusal struct {
			Error     string `json:"error"`
			Appliance string `json:"appliance"`
		}
		if json.Unmarshal(body, &refusal) == nil && agentRefusals[refusal.Error] && resp.StatusCode >= 400 && resp.StatusCode < 600 {
			if refusal.Error == "appliance_managed" && applianceNames[refusal.Appliance] {
				// The panel needs the name to point at the appliance's own UI.
				s.recordWrite(c, detail, AuditFailure, refusal.Error)
				c.JSON(resp.StatusCode, gin.H{"error": refusal.Error, "appliance": refusal.Appliance})
				return
			}
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
		// The agent accepted the action, so the job may be running.
		detail["may_have_started"] = true
		s.writeFail(c, detail, http.StatusBadGateway, "agent_error")
		return
	}
	detail["job_id"] = job.ID
	s.recordWrite(c, detail, AuditSuccess, "")
	c.Data(http.StatusAccepted, contentTypeJSON, body)
}

func (s *ServerArea) writeFail(c *gin.Context, detail map[string]any, code int, reason string) {
	s.recordWrite(c, detail, AuditFailure, reason)
	c.JSON(code, gin.H{"error": reason})
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

// requestedRow is the detail of the row written before the agent call.
func requestedRow(detail map[string]any) map[string]any {
	row := maps.Clone(detail)
	row["phase"] = "requested"
	return row
}
