//go:build !linux && !darwin

package cli

// Boks does not boot sandboxes on these platforms yet, so there is nothing to size. The
// caller falls back to a fixed default; an explicit -memory always wins over either.
func hostMemoryMiB() (int, bool) { return 0, false }
