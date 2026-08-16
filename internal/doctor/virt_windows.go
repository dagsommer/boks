//go:build windows

package doctor

import (
	"context"
)

// Windows hosts exactly the kind of sandbox Boks builds, and as of 2026-08-14 it hosts Boks'
// own. This check has now been wrong in both directions, and the second way is worth naming
// because it is the harder one to notice.
//
// It began as "no VM backend for windows", which implied a platform gap that does not exist:
// the reference product, Docker Sandboxes, runs Linux microVMs on Windows through the
// **Windows Hypervisor Platform** — a user-mode hypervisor API, with a VMM that emulates
// virtio-net in user space and terminates the guest's traffic in a userspace network stack.
// That is Boks' architecture, running on Windows, in a shipping product.
//
// It then became a `fail` that said "That was ctr, not Boks … no Ethernet frame has yet
// crossed one on Windows, so it declines to start a sandbox whose policy it could not
// enforce", and pointed the user at WSL2. Every clause of that is now false:
//
//   - **Boks runs the sandbox itself.** `boks run --net nat shell <workspace> -- uname -a` on
//     Windows 11 hardware, exit 0 in 12.2 s, guest kernel 6.12.44 (2026-08-14).
//   - **A frame has crossed.** The guest attached to Boks' own link socket, and the policy
//     engine judged real traffic across it: an allowed destination completed a TCP connection
//     to a resolved GitHub address, a denied one was refused at CONNECT. On 2026-08-15 the
//     same probe returned HTTP 200 from github.com through Boks' gvisor stack while the denied
//     host still failed at `CONNECT tunnel failed, response 403`.
//   - **Nothing declines.** internal/network/vmm_windows.go held that refusal and no longer
//     does; network.Unexercised() returns nil there.
//   - **WSL2 is no longer "in the meantime".** It remains a supported route and is where the
//     Linux verification was done, but it is an alternative rather than the only option.
//
// A `fail` here is not a stale comment: README.md and docs/get-started.md both tell the reader
// "Nothing should be `fail`", so this line was instructing users of a working machine to stop.
//
// # What this check can and cannot say
//
// It probes nothing, and that is unchanged. WHvGetCapability with
// WHvCapabilityCodeHypervisorPresent returns TRUE from an ordinary unelevated process
// (measured 2026-08-13), which is the one thing that could be probed cheaply — but a TRUE
// there says the API is present, not that a partition can be created, and the pieces that
// actually decide whether a sandbox boots are checked by name elsewhere: `hypervisor library`
// stats for krun.dll on the PATH containerd is started with, `vm runtime` for the shim,
// `guest image` for the kernel and rootfs, and `runtime skew` for containerd's version. So
// this reports what is architecturally true and defers the verdict to those.
//
// It is a warning rather than an `ok` for the same reason macOS is: nothing here boots a VM,
// so "the hypervisor works" is not something this command has established on this host.
func virtualizationCheck() Check {
	return Check{
		Name: "virtualization",
		Run: func(ctx context.Context, env Env) Result {
			return Result{
				Status: StatusWarn,
				Detail: "Windows Hypervisor Platform assumed available",
				Remedy: "Boks cannot probe the Windows Hypervisor Platform without booting a VM, so\n" +
					"this line reports platform support only. A sandbox has been run here — on\n" +
					"2026-08-14 and 2026-08-15, from an unelevated shell, with network policy\n" +
					"enforced (docs/verification.md) — but not on this machine by this command.\n" +
					"\n" +
					"If a sandbox fails to start, the pieces that decide it are checked by name on\n" +
					"the lines above: hypervisor library (krun.dll), vm runtime (the shim), guest\n" +
					"image (kernel and rootfs) and runtime skew (containerd's version). If the\n" +
					"Hypervisor Platform itself is off, enable the 'Windows Hypervisor Platform'\n" +
					"optional feature and reboot.",
			}
		},
	}
}

// extraChecks adds no Windows-only requirements.
//
// Not because there is nothing Windows-specific — there is a great deal, in
// packaging/libkrun-windows and packaging/containerd-windows — but because none of it is a
// separate *prerequisite* a user installs. The Windows download is one zip containing every
// piece, and each of those pieces is already covered by a shared check that resolves it
// through the PATH `boks daemon` starts containerd with (internal/doctor/paths.go). A
// Windows-only checklist on top of that would ask the same questions twice.
//
// The hypervisor library check used to report "not applicable" here and no longer does; see
// the Windows arm of hypervisorLibraryResult in checks.go for why that skip was wrong.
func extraChecks() []Check { return nil }
