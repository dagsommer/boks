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
// The obstacle is no longer a missing device, and as of 2026-08-13 it is no longer a question
// of whether the hypervisor backend runs. This project maintains a WHP backend for libkrun in
// packaging/libkrun-windows/; every crate compiles, krun.dll links on a Windows CI runner, and
// a Linux 6.12.44 guest booted through it on real hardware — device init, virtio-blk against
// the EROFS rootfs, VFS mount, execve of userspace.
//
// What that is not is a sandbox. Boks reaches libkrun through containerd and the nerdbox shim,
// and neither has been exercised on Windows; the boot above was a direct C probe against the
// DLL. The guest clock also does not advance yet. So this check still fails, and the reason it
// gives has to be the real one rather than the old one.
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
					"this project, and on 2026-08-13 a Linux guest booted through it on real Windows\n" +
					"hardware, mounted its root filesystem and reached userspace. That was a direct\n" +
					"probe against krun.dll: Boks drives libkrun through containerd and the nerdbox\n" +
					"shim, and neither has been exercised on Windows, so there is still no sandbox\n" +
					"here to start. Enabling Hyper-V or the Hypervisor Platform will not change that.\n" +
					"\n" +
					"To use Boks on this machine today, run it inside WSL2 with nested\n" +
					"virtualisation, where it is an ordinary Linux program. See docs/windows.md.",
			}
		},
	}
}

// extraChecks adds no Windows-only requirements, deliberately — see the comment above on why
// this does not offer a prerequisite checklist.
//
// The hypervisor library check reports "not applicable" here for the same reason: upstream's
// Windows shim would load krun.dll, but Boks has no Windows backend to load it for, so a
// verdict on the file would be noise on top of the failure above.
func extraChecks() []Check { return nil }
