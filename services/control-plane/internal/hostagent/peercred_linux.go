//go:build linux

package hostagent

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

func readPeerCredentials(conn net.Conn) (Peer, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return Peer{}, errors.New("peer credentials require Unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return Peer{}, err
	}
	var peer Peer
	var readErr error
	if err := raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			readErr = err
			return
		}
		peer = Peer{PID: int(cred.Pid), UID: int(cred.Uid), GID: int(cred.Gid)}
	}); err != nil {
		return Peer{}, err
	}
	return peer, readErr
}
