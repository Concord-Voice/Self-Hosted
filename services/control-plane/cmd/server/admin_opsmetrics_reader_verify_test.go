package main

import (
	"context"
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

// The reader role is a PostgreSQL cluster resource that the live colour is
// using while a verify-mode colour boots beside it. Opening the reader rotates
// the role password, and disabling it (at boot, when the features are off, or
// on close) sets NOLOGIN and terminates every reader session, so any of them
// breaks the live colour's reader.

func refuseReaderOpen(t *testing.T) openAdminOpsMetricsReaderFunc {
	return func(context.Context, *sql.DB, string) (opsmetrics.Reader, func(context.Context) error, error) {
		t.Fatal("verify mode must not open the reader: that rotates the shared role's password")
		return nil, nil, nil
	}
}

func refuseReaderDisable(t *testing.T) ensureAdminOpsMetricsReaderDisabledFunc {
	return func(context.Context, *sql.DB) error {
		t.Fatal("verify mode must not disable the reader login: that terminates the live colour's reader sessions")
		return nil
	}
}

func TestAdminOpsMetricsReaderVerifyModeWritesNoReaderRole(t *testing.T) {
	t.Run("boot reconciliation", func(t *testing.T) {
		// Mutation: run the boot reconciliation in verify mode.
		require.NoError(t, reconcileAdminOpsMetricsReaderAtBoot(context.Background(), true, nil, refuseReaderDisable(t)))
	})

	for _, tc := range []struct {
		name         string
		adminEnabled bool
		opsEnabled   bool
	}{
		{name: "both enabled", adminEnabled: true, opsEnabled: true},
		{name: "admin disabled", adminEnabled: false, opsEnabled: true},
		{name: "metrics disabled", adminEnabled: true, opsEnabled: false},
		{name: "both disabled", adminEnabled: false, opsEnabled: false},
	} {
		t.Run("configure and stop, "+tc.name, func(t *testing.T) {
			cfg := &config.Config{
				AdminConsoleEnabled: tc.adminEnabled,
				OpsMetrics:          config.OpsMetricsConfig{Enabled: tc.opsEnabled},
			}
			runtime, err := configureAdminOpsMetricsReader(
				context.Background(), cfg, nil, true, refuseReaderOpen(t), refuseReaderDisable(t))
			// Mutation: open the reader (both enabled) or disable its login
			// (otherwise) in verify mode.
			require.NoError(t, err)
			require.Nil(t, runtime.Reader(), "verify mode runs with the reader stopped")
			// Mutation: hand the verify-mode runtime a close function, which
			// disables the login when the green colour is stopped.
			require.Nil(t, runtime.closeFn)
			require.NoError(t, runtime.Stop(context.Background()))
		})
	}

	t.Run("incomplete configuration still fails closed", func(t *testing.T) {
		runtime, err := configureAdminOpsMetricsReader(context.Background(), nil, nil, true, refuseReaderOpen(t), refuseReaderDisable(t))
		require.Nil(t, runtime)
		require.EqualError(t, err, "admin operations metrics reader configuration is incomplete")
	})
}

// Positive control: outside verify mode the boot reconciliation still revokes a
// credential left by an unclean prior exit and propagates its failure.
func TestReconcileAdminOpsMetricsReaderAtBootDisablesOutsideVerifyMode(t *testing.T) {
	calls := 0
	wantErr := errors.New("cannot revoke login")
	err := reconcileAdminOpsMetricsReaderAtBoot(context.Background(), false, nil, func(context.Context, *sql.DB) error {
		calls++
		return wantErr
	})
	// Mutation: skip the reconciliation in apply mode, or swallow its error.
	require.Equal(t, 1, calls)
	require.ErrorIs(t, err, wantErr)

	require.EqualError(t, reconcileAdminOpsMetricsReaderAtBoot(context.Background(), false, nil, nil),
		"admin operations metrics reader reconciliation is incomplete")
}

// TestRunControlPlaneGatesReaderRoleWritesOnVerifyMode pins the caller side of
// the gate above: the helpers skip the reader-role writes only when
// runControlPlane hands them the parsed mode, and the deferred reader is bound
// only outside verify mode (binding the stopped runtime's nil reader is an
// error that would refuse startup). runControlPlane needs a live database and
// Redis, so the wiring is read from its syntax tree.
func TestRunControlPlaneGatesReaderRoleWritesOnVerifyMode(t *testing.T) {
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, "main.go", nil, 0)
	require.NoError(t, err)
	var body *ast.BlockStmt
	for _, declaration := range parsed.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "runControlPlane" {
			body = function.Body
		}
	}
	require.NotNil(t, body, "runControlPlane is missing")

	gatedCalls := map[string]int{}
	binds, gatedBinds := 0, 0
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			name := activityHistoryCallName(n.Fun)
			switch name {
			case "EnsureOpsMetricsReaderLoginDisabled", "DisableOpsMetricsReaderLogin", "OpenOpsMetricsReaderConnection":
				// Mutation: call a reader-role function directly, bypassing the gate.
				t.Errorf("runControlPlane calls %s directly at %s", name, files.Position(n.Pos()))
			case "reconcileAdminOpsMetricsReaderAtBoot", "configureAdminOpsMetricsReader":
				// Mutation: pass a literal false instead of the parsed mode.
				if !argsMentionIdent(n.Args, "migrationsVerify") {
					t.Errorf("%s at %s does not receive migrationsVerify", name, files.Position(n.Pos()))
				}
				gatedCalls[name]++
			}
			if isDeferredReaderBind(n) {
				binds++
			}
		case *ast.IfStmt:
			if mentionsIdent(n.Cond, "migrationsVerify") {
				ast.Inspect(n.Body, func(inner ast.Node) bool {
					if call, ok := inner.(*ast.CallExpr); ok && isDeferredReaderBind(call) {
						gatedBinds++
					}
					return true
				})
			}
		}
		return true
	})
	require.Equal(t, map[string]int{"reconcileAdminOpsMetricsReaderAtBoot": 1, "configureAdminOpsMetricsReader": 1}, gatedCalls)
	// Mutation: bind the deferred reader in verify mode.
	require.Equal(t, 1, binds, "runControlPlane binds the deferred reader once")
	require.Equal(t, 1, gatedBinds, "the deferred reader bind must be gated on migrationsVerify")
}

func isDeferredReaderBind(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Bind" {
		return false
	}
	receiver, ok := selector.X.(*ast.Ident)
	return ok && receiver.Name == "deferredAdminMetricsReader"
}

func argsMentionIdent(args []ast.Expr, name string) bool {
	for _, arg := range args {
		if mentionsIdent(arg, name) {
			return true
		}
	}
	return false
}

func mentionsIdent(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(inner ast.Node) bool {
		if ident, ok := inner.(*ast.Ident); ok && ident.Name == name {
			found = true
		}
		return !found
	})
	return found
}
