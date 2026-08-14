//go:build !linux && !darwin

package enforce

import "net"

// peerUID has no portable answer outside Linux and Darwin, and says so rather than guessing.
//
// The caller treats "unknown" as "do not check", which is correct where the control of record
// — the 0700 directory the socket sits in — is enforced by the kernel: the credential check is
// a second opinion that some platforms can offer and others cannot. That is the situation on
// the BSDs and on Solaris, which reach this file and keep the mode.
//
// **Windows is not in that situation, and no longer reaches this decision at all.** It has
// neither half: the permission bits are ignored there, and AF_UNIX carries no peer credentials
// for this function to read even in principle. "Unknown means do not check" would therefore
// mean "no check at all", which is why the control socket is not bound on Windows in the first
// place — see control_windows.go. This file is still compiled for it, because the build
// constraint says so and because peerUID is referenced from platform-neutral code, but on
// Windows nothing ever calls it.
func peerUID(net.Conn) (int, bool) { return 0, false }
