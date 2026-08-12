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
				Remedy: windowsRemedy(),
			}
		},
	}
}

func extraChecks() []Check                   { return nil }
func hypervisorLibraryNames() []string       { return nil }
func hypervisorLibrarySearchPaths() []string { return nil }

// windowsRemedy explains why there is no backend here, and what to do instead.
//
// Windows is not one missing piece but two, both upstream: libkrun is a KVM and
// Hypervisor.framework VMM with no Windows port, and the nerdbox shim does not build for
// Windows either. Neither is something Boks can supply — so rather than promise native
// support, this points at the arrangement that can work today.
//
// WSL2 is that arrangement. With nested virtualisation enabled it exposes /dev/kvm to the
// Linux side, which is the same backend Boks already uses. It is Linux-on-Windows rather than
// native Windows, and it is untested by this project, which the text says.
func windowsRemedy() string {
	if runtime.GOOS != "windows" {
		return "Boks has no virtual machine backend on this platform.\n" +
			"macOS on Apple silicon is the verified platform; Linux with KVM is supported\n" +
			"but has not been exercised end to end."
	}
	return "Boks has no native Windows backend, and cannot gain one on its own: the VMM it\n" +
		"uses (libkrun) targets KVM and Hypervisor.framework, and the container runtime\n" +
		"shim does not build for Windows. Both are upstream projects.\n" +
		"\n" +
		"What can work today is WSL2 with nested virtualisation, where Boks runs as the\n" +
		"Linux program it is:\n" +
		"  1. add 'nestedVirtualization=true' under [wsl2] in %UserProfile%\\.wslconfig\n" +
		"  2. 'wsl --shutdown', then reopen the distribution\n" +
		"  3. check 'ls /dev/kvm' inside WSL, and run 'boks doctor' there\n" +
		"Nobody has verified this arrangement for Boks, so treat it as a lead, not a\n" +
		"supported configuration."
}
