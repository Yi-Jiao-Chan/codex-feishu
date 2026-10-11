//go:build !linux

package control

import "net"

// Do not silently fall back to filesystem-only authentication.
func sameUser(*net.UnixConn) error { return ErrUnsafe }
