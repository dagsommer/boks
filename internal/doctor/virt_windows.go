//go:build windows

package doctor

import (
	"context"
)

// Windows can host exactly the kind of sandbox Boks builds, and as of 2026-08-14 it does host
// one — just not for Boks. This check exists to say which of those two facts is the problem,
// because the message it replaced ("no VM backend for windows") named neither.
//
// The spike behind docs/windows.md found that the reference product, Docker Sandboxes, runs
// Linux microVMs on Windows through the **Windows Hypervisor Platform**: a user-mode
// hypervisor API, with a VMM that emulates virtio-net in user space and terminates the guest's
// traffic in a userspace network stack. That is Boks' architecture, running on Windows, in a
// shipping product. So the platform is not the obstacle and this check must not imply it is.
//
// Nor is the VMM, any more. This project maintains a WHP backend for libkrun in
// packaging/libkrun-windows/ and a shim patch series in packaging/nerdbox-windows/, and on
// 2026-08-14 `ctr tasks start` ran a Linux container end to end on real Windows 11 hardware —
// containerd, the nerdbox shim over ttrpc, krun.dll, WHP — in about 2.2 s from cold. The guest
// reported its own 6.12.44 kernel and an advancing clock. See docs/verification.md.
//
// What is missing is Boks' own boundary. Boks judges every flow at the guest's virtio-net
// device, that device has no Windows backend upstream, and no Ethernet frame has crossed the
// one in this repository's patch series. internal/network/vmm_windows.go refuses to start
// sandbox networking on Windows for exactly that reason, so `boks run` stops before it starts
// a VM. This check has to fail for the same reason, and name it.
//
// It deliberately probes for nothing — not the Hypervisor Platform feature, not containerd,
// not a shim. Not because a checklist would lead nowhere: it demonstrably leads to a running
// container, and docs/windows-e2e.md is that checklist. Because it does not lead to a *Boks
// sandbox*, and a passing prerequisite list in `boks doctor` would say that it did.
//
// Two things here have been executed on Windows. WHvGetCapability with
// WHvCapabilityCodeHypervisorPresent returns TRUE from an ordinary unelevated process, so the
// hypervisor API itself needs no elevation (2026-08-13); and running a container through
// containerd does require elevation or Developer Mode, for a symlink containerd creates in the
// task bundle (2026-08-14). Everything else below is read from source.
func virtualizationCheck() Check {
	return Check{
		Name: "virtualization",
		Run: func(ctx context.Context, env Env) Result {
			return Result{
				Status: StatusFail,
				Detail: "Boks does not start sandboxes on Windows",
				Remedy: "Boks does not run sandboxes on Windows yet, and neither the platform nor the\n" +
					"hypervisor is the reason. Windows exposes a user-mode hypervisor API — the\n" +
					"Windows Hypervisor Platform — which supports exactly the microVM-plus-userspace-\n" +
					"netstack design Boks uses, and on 2026-08-14 a Linux container ran in a microVM\n" +
					"on it through containerd, the nerdbox shim and this project's krun.dll.\n" +
					"\n" +
					"That was ctr, not Boks. Boks enforces network policy at the guest's virtio-net\n" +
					"device; no Ethernet frame has yet crossed one on Windows, so it declines to\n" +
					"start a sandbox whose policy it could not enforce. Enabling Hyper-V or the\n" +
					"Hypervisor Platform will not change that.\n" +
					"\n" +
					"To use Boks on this machine today, run it inside WSL2 with nested\n" +
					"virtualisation, where it is an ordinary Linux program — a route this project\n" +
					"has designed for and not itself run. See docs/windows.md.",
			}
		},
	}
}

// extraChecks adds no Windows-only requirements, deliberately — see the comment above on why
// this does not offer a prerequisite checklist.
//
// The hypervisor library check reports "not applicable" here for the same reason. krun.dll is
// real and this project builds it, but nothing Boks runs on Windows would load it: the
// platform check fails first, and a verdict on a file no Boks code path opens would be noise
// on top of it. When `boks run` reaches a VM on Windows, that skip is the first thing to
// revisit — the file it would report on is the one docs/windows-e2e.md spends a section
// getting onto containerd's PATH.
func extraChecks() []Check { return nil }
