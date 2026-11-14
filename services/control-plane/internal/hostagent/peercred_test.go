package hostagent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPeerGateDefaultsToDenyAndRequiresExactUID(t *testing.T) {
	server := NewServer(&fakeRunner{out: statusJSON})
	request := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code)

	request = request.WithContext(withPeer(request.Context(), Peer{UID: 10007}))
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code)

	request = request.WithContext(withPeer(request.Context(), Peer{UID: 10006}))
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.Error(t, server.SetExpectedPeerUID(0))
	require.Error(t, server.SetExpectedPeerUID(-1))
	require.NoError(t, server.SetExpectedPeerUID(54321))
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code)
}

func TestPeerConnContextPropagatesOnlyReadSuccess(t *testing.T) {
	client, serverConn := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = serverConn.Close() }()
	ctx := peerConnContextWith(func(net.Conn) (Peer, error) { return Peer{PID: 7, UID: 10006, GID: 9}, nil })(context.Background(), serverConn)
	peer, ok := peerFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, Peer{PID: 7, UID: 10006, GID: 9}, peer)
	ctx = peerConnContextWith(func(net.Conn) (Peer, error) { return Peer{}, errors.New("unavailable") })(context.Background(), serverConn)
	_, ok = peerFromContext(ctx)
	require.False(t, ok)
}
