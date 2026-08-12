//go:build windows

package doctor

import (
	"context"
)

// Windows has a hypervisor and a VM-backed container runtime, and Boks still cannot use it.
// This check exists to say which of those two facts is the problem, because the previous
// message ("no VM backend for windows") named the wrong one and implied that a runtime
// gaining Windows support would be enough.
//
// It would not. The spike behind docs/windows.md read hcsshim's and containerd's own source
// and found the VM half of the problem already solved: containerd v2.2.6 ships a
// `windows-lcow` snapshotter, `io.containerd.runhcs.v1` is its default Windows runtime
// handler, and hcsshim boots a Linux utility VM that mounts an arbitrary OCI image and takes
// bind mounts at free-form guest paths. What is missing is the *network*, and it is missing
// in a way no installation step fixes: HCS attaches a NIC to a utility VM as an HNS endpoint
// (`NetworkAdapter{EndpointId, MacAddress, IovSettings}` — there is no socket-backed
// variant), so a host userspace process cannot terminate the guest's Ethernet frames. Boks'
// entire enforcement model is a host-side netstack on the far end of the guest's NIC. Without
// that, `boks run` on Windows would be a sandbox whose network policy is decorative.
//
// Nothing here has been executed on Windows. No machine on this project has Hyper-V; the
// findings are read from source, and the remedy below is a pointer to the analysis rather
// than a set of steps that would make a sandbox start.
func virtualizationCheck() Check {
	return Check{
		Name: "virtualization",
		Run: func(ctx context.Context, env Env) Result {
			return Result{
				Status: StatusFail,
				Detail: "no supported VM backend on Windows",
				Remedy: "Boks does not run sandboxes on Windows, and the blocker is not the hypervisor.\n" +
					"Hyper-V can host a Linux utility VM through containerd and the runhcs shim, and\n" +
					"that part of Boks' design ports. The part that does not is enforcement: the Host\n" +
					"Compute Service only attaches a VM NIC as an HNS endpoint, so no host process can\n" +
					"terminate and judge the guest's traffic the way Boks does on Linux and macOS.\n" +
					"A sandbox would boot with a network Boks could not police.\n" +
					"See docs/windows.md for the evidence and what would have to change upstream.",
			}
		},
	}
}

// extraChecks adds no Windows-only requirements. Probing for Hyper-V, the runhcs shim or the
// LCOW boot files would imply that installing them leads somewhere; it does not, so this
// deliberately stays empty rather than offering a checklist that cannot be completed.
func extraChecks() []Check { return nil }

// hypervisorLibraryNames is empty because the Windows path would link against no libkrun —
// the VMM is the platform's own. The check reports "not applicable" rather than a false miss.
func hypervisorLibraryNames() []string { return nil }

func hypervisorLibrarySearchPaths() []string { return nil }
