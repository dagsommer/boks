//go:build !windows

package enforce

// controlSocketSecurable reports whether the supervisor's control socket can be given the
// protections control.go claims for it.
//
// Everywhere but Windows it can, and both halves are real: the socket sits in a directory the
// kernel enforces as 0700, and a connection's peer credentials are checked against this
// process's own uid wherever the platform can report them (peerUID; Linux and Darwin can, and
// the BSDs that reach peeruid_other.go still have the mode). Nothing changes here, and a test
// pins that this returns nil so that a future platform split cannot quietly stop binding the
// socket on the platforms where `boks ports` works.
func controlSocketSecurable() error { return nil }
