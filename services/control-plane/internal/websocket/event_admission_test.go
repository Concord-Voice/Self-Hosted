package websocket

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These expectations come from the frozen policy contract, independently of
// registry storage. They describe policy groundwork, not runtime enforcement.
func inboundPolicyExpectations() map[string]EventPolicy {
	return map[string]EventPolicy{
		"message":                {EventClassContent, BudgetScopeExistingContent, false},
		"dm_message":             {EventClassContent, BudgetScopeExistingContent, false},
		"typing":                 {EventClassTyping, BudgetScopeAccount, false},
		"dm_typing":              {EventClassTyping, BudgetScopeAccount, false},
		"subscribe":              {EventClassSubscription, BudgetScopeAccount, true},
		"unsubscribe":            {EventClassSubscription, BudgetScopeAccount, true},
		"subscribe_server":       {EventClassSubscription, BudgetScopeAccount, true},
		"unsubscribe_server":     {EventClassSubscription, BudgetScopeAccount, true},
		"subscribe_dm":           {EventClassSubscription, BudgetScopeAccount, true},
		"unsubscribe_dm":         {EventClassSubscription, BudgetScopeAccount, true},
		"heartbeat":              {EventClassPresence, BudgetScopeLocalMaintenance, false},
		"set_status":             {EventClassPresence, BudgetScopeAccount, false},
		"profile_update":         {EventClassProfile, BudgetScopeAccount, false},
		"server_update":          {EventClassServer, BudgetScopeAccount, false},
		"connection_ready_probe": {EventClassReady, BudgetScopeAccount, true},
		"auth_renew":             {EventClassRenewal, BudgetScopeAccount, false},
	}
}

// Class labels are closed strings: unsupported numeric classes must not create
// new observability dimensions.
func TestEventClassStringUsesClosedNames(t *testing.T) {
	tests := []struct {
		class EventClass
		want  string
	}{
		{EventClassContent, "content"},
		{EventClassTyping, "typing"},
		{EventClassSubscription, "subscription"},
		{EventClassPresence, "presence"},
		{EventClassProfile, "profile"},
		{EventClassServer, "server"},
		{EventClassReady, "ready"},
		{EventClassRenewal, "renewal"},
		{0, "unknown"},
		{255, "unknown"},
	}
	for _, test := range tests {
		t.Run(strconv.Itoa(int(test.class)), func(t *testing.T) {
			assert.Equal(t, test.want, test.class.String())
		})
	}
}

func TestClassifyInboundEventPolicy(t *testing.T) {
	for name, want := range inboundPolicyExpectations() {
		t.Run(name, func(t *testing.T) {
			got, ok := classifyInboundEvent(name)
			require.True(t, ok)
			assert.Equal(t, want, got)
			assert.NotZero(t, got.Class, "zero class must remain invalid")
			assert.NotZero(t, got.Scope, "zero scope must remain invalid")
		})
	}
}

func TestClassifyInboundEventRejectsUnknown(t *testing.T) {
	for _, name := range []string{"", "unknown", "MESSAGE", " message", "message ", "auth_renew_result", "connection_ready", "heartbeat_ack", "ping", "pong"} {
		t.Run(strconv.Quote(name), func(t *testing.T) {
			got, ok := classifyInboundEvent(name)
			assert.False(t, ok)
			assert.Equal(t, EventPolicy{}, got, "unknown input must not acquire a class")
		})
	}
}

func TestClassifyInboundEventReturnsIndependentValues(t *testing.T) {
	for name, want := range inboundPolicyExpectations() {
		t.Run(name, func(t *testing.T) {
			got, ok := classifyInboundEvent(name)
			require.True(t, ok)
			got.Class = 0
			got.Scope = 0
			got.MayHold = !want.MayHold
			assert.NotEqual(t, want, got)
			for otherName, otherWant := range inboundPolicyExpectations() {
				again, found := classifyInboundEvent(otherName)
				require.True(t, found)
				assert.Equal(t, otherWant, again, "mutating %s must not alter %s", name, otherName)
			}
		})
	}
}

func TestValidateEventBudgetConfigScale(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			for _, scale := range []int{25, 33, 100, 400} {
				assert.NoError(t, ValidateEventBudgetConfig(EventBudgetConfig{Enabled: enabled, ScalePercent: scale}), "scale %d", scale)
			}
			for _, scale := range []int{-1, 0, 24, 401, 1000} {
				assert.Error(t, ValidateEventBudgetConfig(EventBudgetConfig{Enabled: enabled, ScalePercent: scale}), "scale %d", scale)
			}
		})
	}
	assert.Error(t, ValidateEventBudgetConfig(EventBudgetConfig{}), "zero config is invalid, not implicit disable")
}

func TestAccountEventBudgetFrozenVectors(t *testing.T) {
	tests := []struct {
		name      string
		class     EventClass
		intervals [3]int64
		bursts    [3]int
	}{
		{"subscription", EventClassSubscription, [3]int64{50000, 12500, 3125}, [3]int{40, 160, 640}},
		{"typing", EventClassTyping, [3]int64{400000, 100000, 25000}, [3]int{10, 40, 160}},
		{"profile", EventClassProfile, [3]int64{20000000, 5000000, 1250000}, [3]int{2, 8, 32}},
		{"server", EventClassServer, [3]int64{8000000, 2000000, 500000}, [3]int{4, 16, 64}},
		{"presence", EventClassPresence, [3]int64{2000000, 500000, 125000}, [3]int{4, 16, 64}},
		{"ready", EventClassReady, [3]int64{4000000, 1000000, 250000}, [3]int{4, 16, 64}},
		{"renewal", EventClassRenewal, [3]int64{8000000, 2000000, 500000}, [3]int{4, 16, 64}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for i, scale := range []int{25, 100, 400} {
				for _, enabled := range []bool{true, false} {
					t.Run(strconv.Itoa(scale)+"/enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
						got, err := accountEventBudget(EventBudgetConfig{Enabled: enabled, ScalePercent: scale}, test.class)
						require.NoError(t, err)
						want := eventBudgetParameters{Interval: time.Duration(test.intervals[i]) * time.Microsecond, Burst: test.bursts[i]}
						assert.Equal(t, want, got, "disable bypass belongs to runtime; pure parameter derivation stays valid")
						got.Interval = 0
						got.Burst = 0
						again, err := accountEventBudget(EventBudgetConfig{Enabled: enabled, ScalePercent: scale}, test.class)
						require.NoError(t, err)
						assert.Equal(t, want, again, "returned values must not mutate stored parameters")
					})
				}
			}
		})
	}
}

func TestAccountEventBudgetRoundsTowardLessAllowance(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		got, err := accountEventBudget(EventBudgetConfig{Enabled: enabled, ScalePercent: 33}, EventClassSubscription)
		require.NoError(t, err)
		assert.Equal(t, eventBudgetParameters{Interval: 37879 * time.Microsecond, Burst: 52}, got)
	}
}

func TestAccountEventBudgetRejectsInvalidConfigEvenWhenDisabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, scale := range []int{-1, 0, 24, 401, 1000} {
			t.Run(strconv.Itoa(scale)+"/enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
				for _, class := range []EventClass{EventClassSubscription, EventClassTyping, EventClassProfile, EventClassServer, EventClassPresence, EventClassReady, EventClassRenewal} {
					_, err := accountEventBudget(EventBudgetConfig{Enabled: enabled, ScalePercent: scale}, class)
					assert.Error(t, err, "invalid config cannot become an allow through disabled=%v, class=%d", !enabled, class)
				}
			})
		}
	}
}

func TestAccountEventBudgetRejectsContentAndUnknownClasses(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, class := range []EventClass{0, EventClassContent, 255} {
			_, err := accountEventBudget(EventBudgetConfig{Enabled: enabled, ScalePercent: 100}, class)
			assert.Error(t, err, "class %d cannot receive new account parameters, enabled=%v", class, enabled)
		}
	}
}

// Census the actual dispatch owners, not a registry-generated list. A new hub
// route or intercepted type must fail until it receives an explicit policy.
// Paths are relative to the package so this also works with go test -trimpath.
func TestInboundEventPolicyCoversActualDispatchRoutes(t *testing.T) {
	hub := parseEventAdmissionSource(t, "hub.go")
	handler := eventAdmissionMethod(t, hub, "Hub", "handleIncoming")
	routes := map[string]bool{}
	switches := 0
	ast.Inspect(handler.Body, func(node ast.Node) bool {
		sw, ok := node.(*ast.SwitchStmt)
		if !ok || !eventAdmissionIsMessageType(sw.Tag) {
			return true
		}
		switches++
		for _, stmt := range sw.Body.List {
			clause, ok := stmt.(*ast.CaseClause)
			require.True(t, ok)
			for _, expr := range clause.List {
				literal, ok := expr.(*ast.BasicLit)
				require.True(t, ok, "hub dispatch names must be independently resolvable string literals")
				require.Equal(t, token.STRING, literal.Kind)
				name, err := strconv.Unquote(literal.Value)
				require.NoError(t, err)
				require.False(t, routes[name], "duplicate hub dispatch route %s", name)
				routes[name] = true
			}
		}
		return false
	})
	require.Equal(t, 1, switches, "must find the actual msg.Type dispatch switch")

	renewal := parseEventAdmissionSource(t, "token_renewal.go")
	constants := map[string]string{}
	for _, decl := range renewal.Decls {
		group, ok := decl.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, spec := range group.Specs {
			value, ok := spec.(*ast.ValueSpec)
			require.True(t, ok)
			for i, name := range value.Names {
				if i >= len(value.Values) {
					continue
				}
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				text, err := strconv.Unquote(literal.Value)
				require.NoError(t, err)
				constants[name.Name] = text
			}
		}
	}
	// Interception moved into enabled/disabled helpers. Follow the actual
	// Client call graph rooted at dispatchFrame: an unreachable helper must
	// never satisfy the census, and a new reachable helper must be inspected.
	interceptions := map[string]int{}
	interceptedRoutes := map[string]bool{}
	for _, dispatch := range eventAdmissionReachableClientMethods(t, "dispatchFrame") {
		ast.Inspect(dispatch.Body, func(node ast.Node) bool {
			branch, ok := node.(*ast.IfStmt)
			if !ok {
				return true
			}
			condition, ok := branch.Cond.(*ast.BinaryExpr)
			if !ok || condition.Op != token.EQL {
				return true
			}
			var nameExpr ast.Expr
			if eventAdmissionIsMessageType(condition.X) {
				nameExpr = condition.Y
			}
			if eventAdmissionIsMessageType(condition.Y) {
				nameExpr = condition.X
			}
			if nameExpr == nil {
				return true
			}
			interceptions[dispatch.Name.Name]++
			var name string
			switch expr := nameExpr.(type) {
			case *ast.Ident:
				var found bool
				name, found = constants[expr.Name]
				require.True(t, found, "intercepted type constant must resolve from token_renewal.go")
			case *ast.BasicLit:
				require.Equal(t, token.STRING, expr.Kind)
				var err error
				name, err = strconv.Unquote(expr.Value)
				require.NoError(t, err)
			default:
				t.Fatalf("unrecognized intercepted event expression %T", nameExpr)
			}
			require.False(t, routes[name] && !interceptedRoutes[name], "intercepted route should not also reach the hub: %s", name)
			interceptedRoutes[name] = true
			routes[name] = true
			return true
		})
	}
	require.Equal(t, map[string]int{"dispatchLegacyEvent": 1, "dispatchBudgetedEvent": 1}, interceptions,
		"both reachable dispatch modes must intercept renewal exactly once")
	expectedNames := map[string]bool{}
	for name, want := range inboundPolicyExpectations() {
		expectedNames[name] = true
		got, ok := classifyInboundEvent(name)
		assert.True(t, ok, "actual route %s needs registry policy", name)
		assert.Equal(t, want, got, "route %s", name)
	}
	assert.Equal(t, expectedNames, routes, "actual dispatch route union must match the independently specified policy table")
}

func eventAdmissionReachableClientMethods(t *testing.T, root string) []*ast.FuncDecl {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)
	methods := map[string]*ast.FuncDecl{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file := parseEventAdmissionSource(t, path)
		for _, decl := range file.Decls {
			method, ok := decl.(*ast.FuncDecl)
			if !ok || method.Recv == nil || len(method.Recv.List) != 1 {
				continue
			}
			pointer, ok := method.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			receiver, ok := pointer.X.(*ast.Ident)
			if ok && receiver.Name == "Client" {
				require.NotNil(t, method.Body)
				methods[method.Name.Name] = method
			}
		}
	}
	require.Contains(t, methods, root, "actual Client dispatch root must exist")
	pending := []string{root}
	seen := map[string]bool{}
	reachable := make([]*ast.FuncDecl, 0, len(methods))
	for len(pending) != 0 {
		name := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[name] {
			continue
		}
		seen[name] = true
		method := methods[name]
		reachable = append(reachable, method)
		require.Len(t, method.Recv.List[0].Names, 1)
		receiver := method.Recv.List[0].Names[0].Name
		ast.Inspect(method.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			owner, ok := selector.X.(*ast.Ident)
			if ok && owner.Name == receiver && methods[selector.Sel.Name] != nil {
				pending = append(pending, selector.Sel.Name)
			}
			return true
		})
	}
	return reachable
}

func parseEventAdmissionSource(t *testing.T, path string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	require.NoError(t, err)
	return file
}

func eventAdmissionMethod(t *testing.T, file *ast.File, receiver, name string) *ast.FuncDecl {
	t.Helper()
	for _, decl := range file.Decls {
		method, ok := decl.(*ast.FuncDecl)
		if !ok || method.Name.Name != name || method.Recv == nil || len(method.Recv.List) != 1 {
			continue
		}
		pointer, ok := method.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		ident, ok := pointer.X.(*ast.Ident)
		if ok && ident.Name == receiver {
			require.NotNil(t, method.Body)
			return method
		}
	}
	t.Fatalf("missing %s.%s dispatch owner", receiver, name)
	return nil
}

func eventAdmissionIsMessageType(expr ast.Expr) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Type" {
		return false
	}
	ident, ok := selector.X.(*ast.Ident)
	return ok && ident.Name == "msg"
}
