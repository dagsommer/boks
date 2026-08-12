//go:build darwin

package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/dagsommer/boks/internal/runtimecfg"
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

// hypervisorEntitlement is the code-signing entitlement libkrun needs in order to use
// Hypervisor.framework.
const hypervisorEntitlement = "com.apple.security.hypervisor"

// extraChecks adds the macOS-only requirements.
func extraChecks() []Check {
	return []Check{runtimeEntitlementCheck()}
}

// runtimeEntitlementCheck verifies the shim binary carries the hypervisor entitlement.
//
// This is worth a dedicated check because the failure is both silent and opaque: an
// unsigned shim starts normally and dies inside libkrun with `krun_start_enter failed: -22`,
// which names neither code signing nor the entitlement. Build systems that produce the shim
// without a codesign step hit this every time.
//
// The probe never hard-fails on an inconclusive result: if codesign cannot be run, or its
// output cannot be interpreted, that is reported as a warning rather than blocking a host
// that may well be fine.
func runtimeEntitlementCheck() Check {
	return Check{
		Name: "runtime entitlement",
		Run: func(ctx context.Context, env Env) Result {
			binary := runtimecfg.ShimBinary(env.Runtime)
			if binary == "" {
				return Result{Status: StatusSkip, Detail: "unrecognised runtime " + env.Runtime}
			}
			path, err := exec.LookPath(binary)
			if err != nil {
				// The shim check already reports this; do not repeat the failure.
				return Result{Status: StatusSkip, Detail: "shim not found (see vm runtime)"}
			}

			ctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()

			// codesign writes the entitlement plist to stdout and its commentary to
			// stderr, so both are captured.
			out, err := exec.CommandContext(ctx, "codesign", "-d", "--entitlements", "-", path).CombinedOutput()

			// codesign exits non-zero both for an unsigned binary and for a genuine
			// inspection failure, so the exit status alone cannot tell them apart. They
			// are not the same thing: an unsigned shim definitely cannot boot a VM.
			// Treating it as ambiguity let doctor conclude a host was ready when it was
			// not, which is the one thing this command must never do.
			if unsignedBinary(string(out)) {
				return Result{
					Status: StatusFail,
					Detail: "shim is not signed at all",
					Remedy: fmt.Sprintf("%s carries no code signature, so it has no %s\n"+
						"entitlement and cannot use Hypervisor.framework. A sandbox will die inside\n"+
						"libkrun with 'krun_start_enter failed: -22', which names nothing.\n"+
						"nerdbox's build:shim task signs it; a plain image build does not.",
						path, hypervisorEntitlement),
				}
			}
			if err != nil {
				return Result{
					Status: StatusWarn,
					Detail: "could not inspect the shim's signature",
					Remedy: fmt.Sprintf("Running codesign against %s failed: %v\n"+
						"Boks could not confirm the shim carries the %s entitlement,\n"+
						"which libkrun needs to use Hypervisor.framework. If sandboxes fail to\n"+
						"boot with 'krun_start_enter failed: -22', this is the likely cause.",
						path, err, hypervisorEntitlement),
				}
			}
			if !strings.Contains(string(out), hypervisorEntitlement) {
				return Result{
					Status: StatusFail,
					Detail: "shim lacks " + hypervisorEntitlement,
					Remedy: fmt.Sprintf("%s is not signed with the %s entitlement.\n"+
						"libkrun needs it to use Hypervisor.framework; without it a sandbox dies\n"+
						"inside libkrun with 'krun_start_enter failed: -22', which names nothing.\n"+
						"Sign it with an entitlements plist granting %s — nerdbox's own\n"+
						"build:shim task does this, while a plain image build does not.",
						path, hypervisorEntitlement, hypervisorEntitlement),
				}
			}
			return Result{Status: StatusOK, Detail: hypervisorEntitlement}
		},
	}
}

// unsignedBinary reports whether codesign's output says the file carries no signature at
// all, as opposed to codesign having failed to run.
//
// The exact wording has varied across macOS releases, so this matches the stable part of
// the phrase rather than a whole sentence.
func unsignedBinary(out string) bool {
	lower := strings.ToLower(out)
	return strings.Contains(lower, "not signed at all") ||
		strings.Contains(lower, "code object is not signed")
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
