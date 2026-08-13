package enforce

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID reports the uid on the other end of a UNIX socket, via SO_PEERCRED.
//
// The kernel fills this in at connect time from the connecting process's real credentials, so
// it cannot be claimed by the peer. It is a second check rather than the first: the socket's
// directory is 0700, which is what keeps other users out, and this is what makes a mistake in
// those permissions visible instead of silent.
func peerUID(conn net.Conn) (int, bool) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, false
	}
	var creds *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		creds, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credErr != nil || creds == nil {
		return 0, false
	}
	return int(creds.Uid), true
}
