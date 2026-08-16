//go:build !windows

package network

// Unexercised reports whether anything on this platform has been observed putting a guest's
// frames on the link this package binds.
//
// It is nil here: a real guest has been watched across this link on macOS, over the stream
// transport, with the policy engine refusing denied destinations from the SYN
// (docs/verification.md, 2026-08-13). It is the last platform question this package asks, and
// it is a question rather than a gate — nothing refuses to start on the answer.
//
// It is nil in vmm_windows.go too, since 2026-08-15, so no platform reports an unexercised
// link today. That file records what the answer was, what retired it, and why the question is
// still asked rather than deleted.
func Unexercised() error { return nil }
