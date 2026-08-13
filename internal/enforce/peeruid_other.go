//go:build !linux && !darwin

package enforce

import "net"

// peerUID has no portable answer outside Linux and Darwin, and says so rather than guessing.
//
// The caller treats "unknown" as "do not check", which is correct: the control of record is
// the 0700 directory the socket sits in, and the credential check is a second opinion that
// some platforms can offer and others cannot. Windows reaches this file, and Boks has no
// sandbox network there at all — see internal/network/gateway_windows.go — so no control
// socket is ever bound on it.
func peerUID(net.Conn) (int, bool) { return 0, false }
