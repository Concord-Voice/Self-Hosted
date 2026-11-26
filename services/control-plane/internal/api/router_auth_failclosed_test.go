package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAuthRoutesUseFailClosedIPLimiter pins the Security #163 wiring: every
// per-IP limiter inside the public auth route group must be the fail-closed
// constructor, so a Redis outage rejects with the fixed 503 instead of
// admitting an uncounted attempt. The check is syntactic on purpose: the rate
// limiter itself no longer infers fail-closed from the request path.
func TestAuthRoutesUseFailClosedIPLimiter(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "router.go", nil, 0)
	require.NoError(t, err)

	var group *ast.BlockStmt
	ast.Inspect(file, func(n ast.Node) bool {
		block, ok := n.(*ast.BlockStmt)
		if !ok || group != nil {
			return group == nil
		}
		for i, stmt := range block.List {
			if !assignsAuthRoutes(stmt) {
				continue
			}
			for _, next := range block.List[i+1:] {
				if inner, ok := next.(*ast.BlockStmt); ok {
					group = inner
					break
				}
			}
		}
		return group == nil
	})
	require.NotNil(t, group, "authRoutes group block not found in router.go")

	var checked int
	ast.Inspect(group, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !strings.HasPrefix(sel.Sel.Name, "RateLimitByIP") {
			return true
		}
		checked++
		require.Contains(t, sel.Sel.Name, "FailClosed",
			"router.go:%d: auth route uses availability-first limiter %s", fset.Position(call.Pos()).Line, sel.Sel.Name)
		return true
	})
	require.Positive(t, checked, "expected per-IP limiters inside the auth group")
}

func assignsAuthRoutes(stmt ast.Stmt) bool {
	assign, ok := stmt.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 {
		return false
	}
	ident, ok := assign.Lhs[0].(*ast.Ident)
	return ok && ident.Name == "authRoutes"
}
