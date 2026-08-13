//go:build !windows

package network

// vmmSupported reports whether anything on this platform can put a guest's frames on the
// link this package binds. It is the only platform gate left in the package: the stack, the
// switch, the framing and the socket are the same code everywhere since the link became a
// stream. See vmm_windows.go for what is missing there, and why it is a VMM rather than a
// socket type.
func vmmSupported() error { return nil }
