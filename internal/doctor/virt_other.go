//go:build !linux && !darwin

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
				Remedy: "Boks has no virtual machine backend on this platform yet.\n" +
					"Linux (KVM) is supported; macOS on Apple silicon is next.",
			}
		},
	}
}

func extraChecks() []Check                   { return nil }
func hypervisorLibraryNames() []string       { return nil }
func hypervisorLibrarySearchPaths() []string { return nil }
