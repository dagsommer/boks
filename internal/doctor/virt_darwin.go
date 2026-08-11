//go:build darwin

package doctor

import (
	"context"
	"os"
	"runtime"
)

// virtualizationCheck reports whether the machine can host a libkrun VM.
//
// macOS exposes virtualisation through Hypervisor.framework, which requires Apple silicon
// for the configuration libkrun uses. There is no user-space probe equivalent to opening
// /dev/kvm, so this reports what can be determined without attempting a boot.
func virtualizationCheck() Check {
	return Check{
		Name: "virtualization",
		Run: func(ctx context.Context, env Env) Result {
			if runtime.GOARCH != "arm64" {
				return Result{
					Status: StatusFail,
					Detail: "no supported hypervisor on " + runtime.GOARCH,
					Remedy: "Boks on macOS requires Apple silicon and Hypervisor.framework.",
				}
			}
			return Result{
				Status: StatusWarn,
				Detail: "Hypervisor.framework assumed available",
				Remedy: "Boks cannot probe Hypervisor.framework without booting a VM.\n" +
					"This check reports architecture support only; it has not been\n" +
					"confirmed that a VM will start.",
			}
		},
	}
}

func hypervisorLibraryNames() []string {
	return []string{"libkrun.dylib", "libkrun.1.dylib"}
}

func hypervisorLibrarySearchPaths() []string {
	paths := []string{
		"/opt/homebrew/lib", // Apple silicon Homebrew prefix
		"/usr/local/lib",
	}
	if extra := os.Getenv("DYLD_LIBRARY_PATH"); extra != "" {
		paths = append(paths, splitList(extra)...)
	}
	return paths
}
