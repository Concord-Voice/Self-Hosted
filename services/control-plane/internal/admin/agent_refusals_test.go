package admin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// agentReadOnlyRefusals are codes the agent sends only on routes the write
// relay never calls. Each is relayed, if at all, by its own handler.
var agentReadOnlyRefusals = map[string]string{
	"method_not_allowed":      "the console sends each route's fixed method",
	"busy":                    "read routes only; fetch relays any agent 429 as agent_busy",
	"too_many_followers":      "log follow only; fetch relays any agent 429 as agent_busy",
	"status_unavailable":      "GET /v1/status only; fetch relays it as agent_error",
	"health_unavailable":      "GET /v1/health only; fetch relays it as agent_error",
	"logs_unavailable":        "GET /v1/logs only; fetch relays it as agent_error",
	"backups_unavailable":     "GET /v1/backups only; relayed by the server area",
	"authorizer_not_imported": "the login assertion route, relayed by the login handler",
}

// agentEmittedRefusals collects every refusal code literal in the agent's
// socket handlers: refuse(w, status, "code"), bad("code"), refusal{status,
// "code"} and an "error": "code" map entry. setup.go is the first-run wizard,
// which answers a browser, never the control plane.
func agentEmittedRefusals(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "hostagent", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("hostagent sources: %v (%d files)", err, len(files))
	}
	codes := map[string]bool{}
	literal := func(e ast.Expr) {
		if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if code, err := strconv.Unquote(lit.Value); err == nil {
				codes[code] = true
			}
		}
	}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "setup.go" {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if fn, ok := n.Fun.(*ast.Ident); ok && ((fn.Name == "refuse" && len(n.Args) == 3) || (fn.Name == "bad" && len(n.Args) == 1)) {
					literal(n.Args[len(n.Args)-1])
				}
			case *ast.CompositeLit:
				if typ, ok := n.Type.(*ast.Ident); ok && typ.Name == "refusal" && len(n.Elts) == 2 {
					literal(n.Elts[1])
				}
			case *ast.KeyValueExpr:
				if key, ok := n.Key.(*ast.BasicLit); ok && key.Value == `"error"` {
					literal(n.Value)
				}
			}
			return true
		})
	}
	return codes
}

// TestAgentRefusalsMatchTheAgent pins agentRefusals to the codes the agent can
// send: a new agent code must be relayed or classified, and an entry the agent
// no longer sends is stale.
func TestAgentRefusalsMatchTheAgent(t *testing.T) {
	emitted := agentEmittedRefusals(t)
	var missing, stale, both []string
	for code := range emitted {
		_, readOnly := agentReadOnlyRefusals[code]
		switch {
		case agentRefusals[code] && readOnly:
			both = append(both, code)
		case !agentRefusals[code] && !readOnly:
			missing = append(missing, code)
		}
	}
	for code := range agentRefusals {
		if !emitted[code] {
			stale = append(stale, code)
		}
	}
	for code := range agentReadOnlyRefusals {
		if !emitted[code] {
			stale = append(stale, code)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	sort.Strings(both)
	if len(missing) != 0 || len(stale) != 0 || len(both) != 0 {
		t.Fatalf("agent refusals out of step: unrelayed %v, stale %v, classified twice %v", missing, stale, both)
	}
	for _, code := range []string{"update_downgrade_refused", "running_version_unknown", "alerts_not_configured",
		"job_unavailable", "unexpected_argument", "invalid_body", "unknown_action", "writes_disabled"} {
		if !emitted[code] || !agentRefusals[code] {
			t.Errorf("%s: emitted=%t relayed=%t", code, emitted[code], agentRefusals[code])
		}
	}
}
