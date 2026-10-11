//go:build linux

package control

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func sameUser(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return ErrUnsafe
	}
	matched := false
	if err := raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		matched = e == nil && int(cred.Uid) == os.Geteuid()
	}); err != nil || !matched {
		return ErrUnsafe
	}
	return nil
}
