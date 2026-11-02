package managedredis

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// EventBudgetTestRuntime creates only a synthetic test fixture. Keeping this
// bridge in _test.go gives external constructor tests access without expanding
// the production Runtime interface or exposing credential-valued internals.
func EventBudgetTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	scope := credentialTestScope()
	now := credentialTestNow
	reader := &mutableCredentialReader{name: scope.Name}
	reader.set(credentialTestRaw(t, scope, 12, "cv_cp_12", "synthetic-password", "rotation-12", now.Add(-time.Minute), now.Add(time.Hour)), scope.Name, nil)
	runtime, err := NewRuntime(reader, scope, "control-plane", func() time.Time { return now })
	require.NoError(t, err)
	require.NoError(t, runtime.Refresh(context.Background()))
	return runtime
}

// EventBudgetTestListenerCount exposes a locked aggregate only in test builds.
func EventBudgetTestListenerCount(runtime *Runtime) int {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return len(runtime.listeners)
}

// EventBudgetTestControlPlaneACLRule exposes the checked renderer only to tests.
func EventBudgetTestControlPlaneACLRule() string {
	return serviceACLRule("control-plane")
}
