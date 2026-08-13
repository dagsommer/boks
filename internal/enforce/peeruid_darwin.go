package enforce

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID reports the uid on the other end of a UNIX socket, via LOCAL_PEERCRED.
//
// Darwin's answer comes as an xucred rather than a ucred, and it carries no pid, which is why
// this is a separate file rather than a shared one with a different constant. See
// peeruid_linux.go for why the check is here at all: the directory's 0700 is the control, and
// this makes a mistake in it visible.
func peerUID(conn net.Conn) (int, bool) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, false
	}
	var creds *unix.Xucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		creds, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil || credErr != nil || creds == nil {
		return 0, false
	}
	return int(creds.Uid), true
}
