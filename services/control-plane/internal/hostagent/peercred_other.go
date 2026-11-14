//go:build !linux

package hostagent

import (
	"errors"
	"net"
)

func readPeerCredentials(net.Conn) (Peer, error) {
	return Peer{}, errors.New("peer credentials unavailable on this platform")
}
