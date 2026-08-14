//go:build !windows

package network

// Unexercised reports whether anything on this platform has been observed putting a guest's
// frames on the link this package binds.
//
// It is nil here: a real guest has been watched across this link on macOS, over the stream
// transport, with the policy engine refusing denied destinations from the SYN
// (docs/verification.md, 2026-08-13). It is the last platform question this package asks, and
// it is a question rather than a gate — nothing refuses to start on the answer. See
// vmm_windows.go for the platform where the answer is not nil, and for what a caller is
// expected to do with it.
func Unexercised() error { return nil }
