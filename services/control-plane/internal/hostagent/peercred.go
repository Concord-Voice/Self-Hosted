package hostagent

import (
	"context"
	"net"
)

// Peer is the kernel-reported identity of a Unix-socket client.
type Peer struct {
	PID int
	UID int
	GID int
}

type peerContextKey struct{}

func withPeer(ctx context.Context, peer Peer) context.Context {
	return context.WithValue(ctx, peerContextKey{}, peer)
}

func peerFromContext(ctx context.Context) (Peer, bool) {
	peer, ok := ctx.Value(peerContextKey{}).(Peer)
	return peer, ok
}

// PeerConnContext captures SO_PEERCRED once per accepted Unix connection.
// An unavailable credential leaves the request without a peer and is denied.
func PeerConnContext(ctx context.Context, conn net.Conn) context.Context {
	return peerConnContextWith(readPeerCredentials)(ctx, conn)
}

func peerConnContextWith(reader func(net.Conn) (Peer, error)) func(context.Context, net.Conn) context.Context {
	return func(ctx context.Context, conn net.Conn) context.Context {
		peer, err := reader(conn)
		if err != nil {
			return ctx
		}
		return withPeer(ctx, peer)
	}
}
