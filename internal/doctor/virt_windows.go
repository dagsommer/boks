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
// The obstacle is no longer a missing device. This project maintains a WHP backend for libkrun
// in packaging/libkrun-windows/, and as of 2026-08-13 every libkrun crate compiles for Windows
// and krun.dll links on a Windows CI runner with virtio-fs, -blk, -console, -balloon, -rng and
// virtio-net. What has not happened is a boot: no sandbox has ever started on Windows, that
// backend has never executed an instruction, and no Ethernet frame has crossed that device.
//
// So this check must say something narrower than it used to, and harder to get right: the
// pieces exist and have never been run together. Telling a user to enable a Windows feature
// would be wrong, because nothing on this machine can consume it yet.
//
// This check deliberately probes for nothing — not the Hypervisor Platform feature, not
// containerd, not a shim. Probing would imply that installing the missing pieces leads
// somewhere, and today it does not: there is no Boks Windows backend wired up to enable.
//
// One thing here HAS been executed on Windows, on 2026-08-13: WHvGetCapability with
// WHvCapabilityCodeHypervisorPresent returns TRUE from an ordinary unelevated process, so the
// hypervisor API itself needs no elevation. Everything else below is read from source.
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
					"the reference product ships on it. A WHP backend for libkrun is being built in\n" +
					"this project: every crate now compiles for Windows and krun.dll links in CI,\n" +
					"virtio-net included. No sandbox has ever booted on Windows, so none of that is\n" +
					"yet a working VM, and enabling Hyper-V or the Hypervisor Platform will not make\n" +
					"one — nothing here is wired up to use it.\n" +
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
