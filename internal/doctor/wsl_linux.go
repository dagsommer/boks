//go:build linux

package doctor

import (
	"os"
	"regexp"
	"strings"
)

// WSL2 is Boks' only usable answer for Windows today (see docs/windows.md), and the way it
// fails is different enough from bare-metal Linux that a generic "enable nested virtualisation"
// message sends people to the wrong place. This file supplies the detection and the three
// distinct diagnoses.
//
// **None of this has been executed on WSL.** No machine on this project runs Windows. The
// signals below are taken from WSL's own source and issue tracker; the shape of the logic is
// testable here, the values it reads are not.

// wslMarker is the file WSL's init creates in every distribution on every boot.
//
// It is the detection signal rather than the more familiar /proc/sys/kernel/osrelease string
// match, because osrelease carries CONFIG_LOCALVERSION and a user who builds a custom WSL
// kernel can lose the "microsoft-standard-WSL2" suffix entirely — and a custom kernel is
// exactly the configuration most likely to be missing KVM, i.e. the case this check exists to
// explain. Three other candidates were rejected: $WSL_DISTRO_NAME is environment-only and
// absent under systemd units and cron, /run/WSL disappears when interop is disabled, and
// /mnt/wsl moves when automount.root is set.
const wslMarker = "/bin/wslinfo"

// inWSL reports whether Boks is running inside a WSL distribution.
//
// The osrelease fallback covers builds predating wslinfo. A false positive here only changes
// the wording of a failure message, never a decision, so the loose fallback is safe.
func inWSL() bool {
	if _, err := os.Stat(wslMarker); err == nil {
		return true
	}
	release, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(release))
	return strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl")
}

// virtualizationExposed reports whether the vCPU carries a hardware virtualisation flag.
//
// This is the discriminator that makes the three WSL diagnoses distinguishable. The Windows
// setting that controls nested virtualisation is literally
// ComputeTopology.Processor.ExposeVirtualizationExtensions, so when it is off the guest CPU has
// no vmx/svm flag at all — whereas a merely unloaded kvm module leaves the flag present. The
// Windows-side error text ("Nested virtualization is not supported on this machine.") is
// written to wsl.exe's stderr on the *Windows* side and can never be seen from in here, so
// reading the flag is the only signal available.
func virtualizationExposed() bool {
	info, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		// Unreadable cpuinfo must not turn into a confident claim either way; the
		// caller treats false as "cannot confirm" and says so.
		return false
	}
	return cpuFlagRE.MatchString(string(info))
}

// cpuFlagRE matches a flags line exposing Intel VT-x (vmx) or AMD-V (svm). Word boundaries
// matter: "svm" appears inside other flag names.
var cpuFlagRE = regexp.MustCompile(`(?m)^flags\s*:.*\b(vmx|svm)\b`)

// wslKVMMissingRemedy explains an absent /dev/kvm inside WSL.
//
// Two very different causes produce it and the remedies do not overlap, which is why this
// branches rather than listing both.
func wslKVMMissingRemedy() string {
	const common = "\n" +
		"Boks needs KVM inside the distribution. WSL is the only route to Boks on Windows\n" +
		"today; see docs/windows.md."

	if !virtualizationExposed() {
		// No vmx/svm: the hypervisor is not exposing virtualisation extensions to
		// this VM at all, so nothing inside the distribution can fix it.
		return "This is WSL, and the vCPU exposes no virtualisation extensions (no vmx/svm in\n" +
			"/proc/cpuinfo), so nested virtualisation is off at the Windows level. Nothing\n" +
			"installed inside the distribution can change that.\n" +
			"\n" +
			"Note that nested virtualisation is already ON by default on Windows 11 x64, so\n" +
			"adding it to .wslconfig is usually NOT the fix. The likely causes are:\n" +
			"  - Windows 10, or an ARM64 device: not supported.\n" +
			"  - A CPU predating Intel Haswell or AMD Zen.\n" +
			"  - safeMode=true in .wslconfig.\n" +
			"  - An enterprise policy (AllowNestedVirtualization) disabling it.\n" +
			"\n" +
			"If it really is disabled, set this in %UserProfile%\\.wslconfig on the Windows\n" +
			"side — the section and key are case-sensitive, and there is no equivalent in\n" +
			"/etc/wsl.conf:\n" +
			"\n" +
			"    [wsl2]\n" +
			"    nestedVirtualization=true\n" +
			"\n" +
			"then run 'wsl --shutdown' in Windows and reopen the distribution. Beware that a\n" +
			"malformed .wslconfig is ignored silently: WSL starts normally and the setting\n" +
			"simply does not apply, which looks identical to not having set it." + common
	}

	// vmx/svm present but no device: the module is simply not loaded. WSL's boot-time
	// module list is hardcoded to tun, ip_tables and br_netfilter, and CONFIG_KVM is =m on
	// current WSL kernels, so this is the ordinary case rather than an unusual one.
	return "This is WSL, and the vCPU does expose virtualisation extensions, so nested\n" +
		"virtualisation is working — the KVM module is simply not loaded. WSL loads only\n" +
		"tun, ip_tables and br_netfilter at boot, and KVM is built as a module.\n" +
		"\n" +
		"Load it for this session:\n" +
		"    sudo modprobe kvm_amd     # or kvm_intel on an Intel CPU\n" +
		"\n" +
		"Boks also needs the erofs module, which is missing for the same reason:\n" +
		"    sudo modprobe erofs\n" +
		"\n" +
		"To make both persist, add to %UserProfile%\\.wslconfig on the Windows side and\n" +
		"run 'wsl --shutdown':\n" +
		"\n" +
		"    [wsl2]\n" +
		"    loadKernelModules=kvm_amd,erofs\n" +
		"\n" +
		"(loadKernelModules is present in WSL's source but undocumented, so treat it as\n" +
		"best-effort and keep modprobe as the fallback.)\n" +
		"\n" +
		"Loadable modules need WSL 2.5.1 or newer, which introduced the modules image.\n" +
		"'nested=1' on the KVM module is NOT needed — that governs a third level of\n" +
		"nesting and is widely cargo-culted." + common
}

// wslKVMPermissionRemedy explains an unopenable /dev/kvm inside WSL.
//
// WSL runs no udev, so the devtmpfs node is created root:root 0600 and no rule ever relaxes
// it. This is the single most common way Boks would fail on an otherwise correct WSL setup.
func wslKVMPermissionRemedy() string {
	return "This is WSL, where /dev/kvm exists but is root-owned and mode 0600: WSL runs no\n" +
		"udev, so the rule that would widen it on an ordinary distribution never runs.\n" +
		"\n" +
		"Create the group if it is absent (check with 'getent group kvm' — on Debian and\n" +
		"Ubuntu it arrives with qemu/libvirt rather than the base system):\n" +
		"    sudo groupadd -r kvm\n" +
		"\n" +
		"Add yourself to it:\n" +
		"    sudo usermod -aG kvm $USER\n" +
		"\n" +
		"Then fix the node on every boot, in /etc/wsl.conf inside the distribution:\n" +
		"\n" +
		"    [boot]\n" +
		"    command = /bin/bash -c 'chown root:kvm /dev/kvm && chmod 660 /dev/kvm'\n" +
		"\n" +
		"and run 'wsl --shutdown' in Windows so the change takes effect.\n" +
		"\n" +
		"Do NOT use 'chmod 666 /dev/kvm', which many guides suggest: it lets every local\n" +
		"account create virtual machines on the host.\n" +
		"\n" +
		"With systemd enabled ([boot] systemd=true) udev runs and the stock rules should\n" +
		"give root:kvm 0660 without the command above — but systemd is off by default in\n" +
		"many images."
}
