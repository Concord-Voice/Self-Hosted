package api_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWebSocketJWTAttestationWiring(t *testing.T) {
	data, err := os.ReadFile("router.go") //nolint:gosec // fixed source file in test
	require.NoError(t, err)
	source := string(data)
	require.Contains(t, source, "wsHandler.SetAttestation(cfg.RequireClientAttestation, log)")
	require.Contains(t, source, "authRequired.Use(middleware.RequireSessionHeaderMatchesToken())")
	require.Less(t,
		strings.Index(source, "authRequired.Use(middleware.RequireSessionHeaderMatchesToken())"),
		strings.Index(source, "authRequired.Use(middleware.RequireAttestation(cfg.RequireClientAttestation"),
	)
	require.Contains(t, source, `v1.GET("/ws", wsHandler.HandleWebSocket)`)
	require.NotContains(t, source, `authRequired.GET("/ws"`)
	require.True(t, strings.Index(source, "websocket.NewHandler(") < strings.Index(source, `v1.GET("/ws", wsHandler.HandleWebSocket)`))
}
