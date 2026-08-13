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
func extraChecks() []Check { return nil }

// hypervisorLibraryNames is krun.dll, and used to be nothing at all.
//
// Returning nil made the shared check print "hypervisor library  skip  not applicable on this
// platform" — which was defensible while the VMM question was open, and is not now. The shim
// that runs on Windows is upstream nerdbox's, and on Windows it loads its VMM from a single
// filename: `krun.dll` (internal/vm/libkrun/instance.go). "Not applicable" was reported on a
// Windows 11 machine on 2026-08-13 with a krun.dll sitting in the directory being searched.
func hypervisorLibraryNames() []string { return []string{"krun.dll"} }

// hypervisorLibrarySearchPaths is the shim's own scan, not a Windows-flavoured guess at one.
//
// There is no /usr/lib equivalent to enumerate here and no loader configuration to fall back
// on: the shim builds a path out of PATH and LIBKRUN_PATH and hands the result to
// syscall.LoadLibrary, so those two variables are the whole search. nerdboxSearchPaths is
// that list, shared with the guest-image check because the shim resolves both the same way.
func hypervisorLibrarySearchPaths() []string { return nerdboxSearchPaths() }

// hypervisorLibraryHint says what a miss means here, which is more than it means on Unix: the
// shim never consults the loader's search path on Windows, so a krun.dll this check cannot
// find is one the shim cannot find either — subject to containerd's PATH not being ours.
//
// Where to get one takes care to state, because the true answer changed recently and is
// easy to overclaim in either direction. packaging/libkrun-windows/ now carries a patch
// series that builds a real krun.dll: CI links it on a windows-latest runner with virtio-net,
// and all nineteen symbols nerdbox resolves at load are exported. But no VM has ever booted
// on Windows, and a boot attempt on real hardware got as far as kernel loading and stopped
// there. So the remedy says a DLL can be built and declines to say it will start a sandbox.
// Anything exporting libkrun's C ABI under this name loads.
func hypervisorLibraryHint() string {
	return "Searched PATH and LIBKRUN_PATH, which is the shim's whole search on Windows:\n" +
		"it loads the resolved path directly and never consults the loader's search path.\n" +
		"Note that containerd's PATH is the daemon's, not your shell's.\n" +
		"There is no krun.dll to install from upstream libkrun: its Windows port is targeted\n" +
		"at libkrun 2.0. This repository builds one — apply packaging/libkrun-windows/patches/\n" +
		"to the pinned revision in that directory and build with --features blk,net.\n" +
		"Having one is not enough to start a sandbox: no VM has ever booted on Windows, and\n" +
		"the furthest a real attempt reached was loading the guest kernel. See the platform\n" +
		"check."
}
