//go:build !linux && !darwin && !windows

package doctor

import (
	"context"
	"runtime"
)

// virtualizationCheck on unsupported platforms reports why Boks cannot run rather than
// pretending the requirement does not exist.
func virtualizationCheck() Check {
	return Check{
		Name: "virtualization",
		Run: func(ctx context.Context, env Env) Result {
			return Result{
				Status: StatusFail,
				Detail: "no VM backend for " + runtime.GOOS,
				Remedy: unsupportedRemedy(),
			}
		},
	}
}

func extraChecks() []Check                   { return nil }
func hypervisorLibraryNames() []string       { return nil }
func hypervisorLibrarySearchPaths() []string { return nil }

// hypervisorLibraryHint is never shown here: with no library names, the check skips before it
// can miss. It exists because the shared check requires every platform to provide one.
func hypervisorLibraryHint() string { return "" }

// unsupportedRemedy is for platforms with no VM backend at all.
//
// Windows is not one of them and is not handled here — see virt_windows.go, whose build tag
// excludes this file. That separation exists because the Windows answer is specific, and the
// generic one this function used to give was wrong in a way worth not repeating: it said the
// nerdbox shim does not build for Windows. It does. `GOOS=windows go build` produces the shim
// binary from upstream today, and a shipping product runs exactly that binary.
func unsupportedRemedy() string {
	return "Boks has no virtual machine backend on this platform.\n" +
		"macOS on Apple silicon is the verified platform; Linux with KVM is supported\n" +
		"but has not been exercised end to end."
}
