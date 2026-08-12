//go:build windows

package doctor

import (
	"context"
)

// Windows can host exactly the kind of sandbox Boks builds. Boks just cannot build one there
// yet, and this check exists to say which of those two facts is the problem — because the
// message it replaced ("no VM backend for windows") named neither.
//
// The spike behind docs/windows.md found that the reference product, Docker Sandboxes, runs
// Linux microVMs on Windows through the **Windows Hypervisor Platform**: a user-mode
// hypervisor API, with a VMM that emulates virtio-net in user space and terminates the guest's
// traffic in a userspace network stack. That is Boks' architecture, running on Windows, in a
// shipping product. So the platform is not the obstacle and this check must not imply it is.
//
// The obstacle is one device. libkrun has carried a WHP backend since mid-2026 — vCPU loop,
// instruction emulator, virtio-fs, -blk, -console, -balloon and -rng — targeting libkrun 2.0,
// and upstream nerdbox already builds a Windows shim that loads it as krun.dll. The single
// device not ported is **virtio-net**, which is the one Boks' enforcement depends on.
//
// This check deliberately probes for nothing — not the Hypervisor Platform feature, not
// containerd, not a shim. Probing would imply that installing the missing pieces leads
// somewhere, and today it does not: there is no Boks Windows backend to enable.
//
// Nothing here has been executed on Windows. No machine on this project has it; the findings
// are read from source, from Microsoft's documentation and from Docker's shipped binaries.
func virtualizationCheck() Check {
	return Check{
		Name: "virtualization",
		Run: func(ctx context.Context, env Env) Result {
			return Result{
				Status: StatusFail,
				Detail: "Boks has no Windows VM backend",
				Remedy: "Boks does not run sandboxes on Windows yet, and the platform is not the reason.\n" +
					"Windows exposes a user-mode hypervisor API — the Windows Hypervisor Platform —\n" +
					"which supports exactly the microVM-plus-userspace-netstack design Boks uses, and\n" +
					"the reference product ships on it. libkrun's WHP backend is in progress upstream\n" +
					"for libkrun 2.0, and virtio-net is the one device still unported — which is the\n" +
					"one Boks needs. Enabling Hyper-V or the Hypervisor Platform will not help until\n" +
					"it lands.\n" +
					"\n" +
					"To use Boks on this machine today, run it inside WSL2 with nested\n" +
					"virtualisation, where it is an ordinary Linux program. See docs/windows.md.",
			}
		},
	}
}

// extraChecks adds no Windows-only requirements, deliberately — see the comment above on why
// this does not offer a prerequisite checklist.
func extraChecks() []Check { return nil }

// hypervisorLibraryNames is empty because a Windows port would not link libkrun at all; the
// VMM question is open. The shared check reports "not applicable" rather than a false miss.
func hypervisorLibraryNames() []string { return nil }

func hypervisorLibrarySearchPaths() []string { return nil }
