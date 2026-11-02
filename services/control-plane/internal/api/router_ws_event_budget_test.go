package api

import (
	"context"
	"io"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Process owns this engine; Hub and router borrow it, including error paths.
type routerBudgetBorrowedAdmitter struct{ admits, closes atomic.Int32 }

func (a *routerBudgetBorrowedAdmitter) Admit(context.Context, uuid.UUID, websocket.EventPolicy, time.Time) websocket.AdmissionDecision {
	a.admits.Add(1)
	return websocket.AdmissionDecision{Action: websocket.AdmissionAllow, Source: websocket.AdmissionSourceRedis}
}
func (a *routerBudgetBorrowedAdmitter) Close() error { a.closes.Add(1); return nil }

func TestRouterBudgetHubUsesValidatingConstructorAndBorrowsEngine(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, scale := range []int{25, 100, 400} {
			policy := config.WSEventBudgetConfig{Enabled: enabled, ScalePercent: scale}
			engine := &routerBudgetBorrowedAdmitter{}
			events := &eventRecorder{}
			hub, err := newHubWithSecurityEvents(nil, nil, nil, events, policy, engine)
			require.NoError(t, err)
			require.NotNil(t, hub)
			// Both DTO fields and the exact borrowed engine reach the Hub.
			actual := reflect.ValueOf(hub).Elem().FieldByName("eventBudgetConfig")
			require.True(t, actual.IsValid())
			require.Equal(t, enabled, actual.FieldByName("Enabled").Bool())
			require.Equal(t, int64(scale), actual.FieldByName("ScalePercent").Int())
			require.Equal(t, reflect.ValueOf(engine).Pointer(), nestedEmitterPointer(t, hub, "eventAdmitter"))
			require.Equal(t, reflect.ValueOf(events).Pointer(), nestedEmitterPointer(t, hub, "securityEvents"))
			policy.Enabled = !enabled
			policy.ScalePercent = 0
			require.Equal(t, enabled, actual.FieldByName("Enabled").Bool(), "startup policy is copied")
			go hub.Run()
			hub.Shutdown()
			hub.Shutdown()
			require.Zero(t, engine.admits.Load(), "assembly must not make an admission decision")
			require.Zero(t, engine.closes.Load(), "Hub never acquires ownership of borrowed Close")
		}
	}
}

func TestRouterBudgetHubRejectsMissingEngineAndInvalidPolicy(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, scale := range []int{0, 24, 401} {
			engine := &routerBudgetBorrowedAdmitter{}
			hub, err := newHubWithSecurityEvents(nil, nil, nil, nil, config.WSEventBudgetConfig{Enabled: enabled, ScalePercent: scale}, engine)
			require.ErrorContains(t, err, "scale")
			require.Nil(t, hub)
			require.Zero(t, engine.admits.Load())
			require.Zero(t, engine.closes.Load())
		}
	}
	hub, err := newHubWithSecurityEvents(nil, nil, nil, nil, config.WSEventBudgetConfig{Enabled: true, ScalePercent: 100}, nil)
	require.ErrorContains(t, err, "admitter")
	require.Nil(t, hub)
	// Disabled construction is a live control, never an implicit scale fix.
	hub, err = newHubWithSecurityEvents(nil, nil, nil, nil, config.WSEventBudgetConfig{Enabled: false, ScalePercent: 100}, nil)
	require.NoError(t, err)
	require.NotNil(t, hub)
	go hub.Run()
	hub.Shutdown()
}

func TestRouterBudgetAdmissionErrorsPrecedePresenceInitialization(t *testing.T) {
	for _, test := range []struct {
		name   string
		policy config.WSEventBudgetConfig
		engine *routerBudgetBorrowedAdmitter
		want   string
	}{
		{"enabled requires engine", config.WSEventBudgetConfig{Enabled: true, ScalePercent: 100}, nil, "admitter"},
		{"enabled invalid scale", config.WSEventBudgetConfig{Enabled: true, ScalePercent: 0}, &routerBudgetBorrowedAdmitter{}, "scale"},
		{"disabled invalid scale", config.WSEventBudgetConfig{Enabled: false, ScalePercent: 0}, &routerBudgetBorrowedAdmitter{}, "scale"},
		{"disabled later error control", config.WSEventBudgetConfig{Enabled: false, ScalePercent: 100}, nil, "presence history service unavailable"},
		{"enabled borrowed engine later error", config.WSEventBudgetConfig{Enabled: true, ScalePercent: 100}, &routerBudgetBorrowedAdmitter{}, "presence history service unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{Environment: "test", WSEventBudget: test.policy}
			deps := RouterDependencies{}
			// Avoid assigning a typed-nil pointer to the interface: the missing
			// engine case must actually cross the production nil boundary.
			if test.engine != nil {
				deps.WSEventAdmitter = test.engine
			}
			router, hub, nc, metrics, voice, recheck, closePresence, rail, reconciler, complete, err := NewRouter(
				t.Context(), nil, nil, cfg, nil, logger.NewWithWriter(io.Discard), deps,
			)
			require.ErrorContains(t, err, test.want)
			require.Nil(t, router)
			require.Nil(t, hub)
			require.Nil(t, nc)
			require.Nil(t, metrics)
			require.Nil(t, voice)
			require.Nil(t, recheck)
			require.Nil(t, closePresence)
			require.Nil(t, rail)
			require.Nil(t, reconciler)
			require.Nil(t, complete)
			if test.engine != nil {
				require.Zero(t, test.engine.admits.Load())
				require.Zero(t, test.engine.closes.Load(), "router error does not transfer engine ownership")
			}
		})
	}
}
