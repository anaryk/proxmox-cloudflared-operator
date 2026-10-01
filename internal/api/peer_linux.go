//go:build linux

package api

import (
	"context"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerChecks is true where the kernel tells who is on the other end of a unix
// socket.
const peerChecks = true

// connContext puts the uid of the peer into the context of a connection. When
// the credentials cannot be read, nothing is put there, and the peer check
// refuses the connection's requests.
func connContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	uid, err := readPeerUID(uc)
	if err != nil {
		return ctx
	}
	return withPeerUID(ctx, uid)
}

func readPeerUID(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if sockErr != nil {
		return 0, fmt.Errorf("reading the peer credentials: %w", sockErr)
	}
	return cred.Uid, nil
}
