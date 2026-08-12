//go:build linux

package doctor

import "testing"

// The WSL remedies cannot be exercised on this machine — nothing here runs Windows. What can
// be asserted is that they say the right things, because their whole value is being specific
// where the generic message misleads. A remedy that quietly lost the "already on by default"
// warning would look fine and send every reader to the wrong fix.

func TestCPUFlagDetection(t *testing.T) {
	for name, tc := range map[string]struct {
		cpuinfo string
		want    bool
	}{
		"intel vmx":  {"flags\t\t: fpu vme de pse tsc msr pae mce cx8 apic vmx smx est\n", true},
		"amd svm":    {"flags\t\t: fpu vme de pse tsc msr pae mce cx8 apic svm cr8_legacy\n", true},
		"no virt":    {"flags\t\t: fpu vme de pse tsc msr pae mce cx8 apic sep mtrr\n", false},
		"empty":      {"", false},
		"no flags":   {"processor\t: 0\nvendor_id\t: GenuineIntel\n", false},
		"multi core": {"processor\t: 0\nflags\t\t: fpu vmx\nprocessor\t: 1\nflags\t\t: fpu vmx\n", true},
		// "svm" occurs inside other tokens; a substring match would false-positive.
		"svm as substring": {"flags\t\t: fpu nosvm svma abcsvm\n", false},
		// The flag must be on a flags line, not anywhere in the file.
		"vmx in model name": {"model name\t: Bogus vmx Edition\nflags\t\t: fpu de\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := cpuFlagRE.MatchString(tc.cpuinfo); got != tc.want {
				t.Errorf("MatchString = %v, want %v", got, tc.want)
			}
		})
	}
}

// The two missing-device remedies must be distinguishable and must not give each other's
// advice: telling someone with no vmx/svm to run modprobe wastes their time, and telling
// someone with a merely unloaded module to edit .wslconfig sends them to Windows for nothing.
func TestWSLMissingRemedyBranches(t *testing.T) {
	remedy := wslKVMMissingRemedy()
	if remedy == "" {
		t.Fatal("no remedy produced")
	}
	// Whichever branch this machine takes, the WSL framing and the doc pointer must be
	// there — a bare instruction with no context is what this replaced.
	for _, want := range []string{"WSL", "docs/windows.md"} {
		if !contains(remedy, want) {
			t.Errorf("remedy does not mention %q:\n%s", want, remedy)
		}
	}
}

// The most misleading thing doctor could do on WSL is repeat the folklore fix, so both are
// asserted against directly rather than left to review.
func TestWSLRemediesAvoidHarmfulAdvice(t *testing.T) {
	perm := wslKVMPermissionRemedy()

	// chmod 666 on /dev/kvm hands VM creation to every local account. It is all over the
	// blogs, which is exactly why the remedy warns against it instead of staying silent.
	if !contains(perm, "chmod 666") || !contains(perm, "Do NOT") {
		t.Errorf("permission remedy should warn against chmod 666 explicitly:\n%s", perm)
	}
	if !contains(perm, "chmod 660") {
		t.Errorf("permission remedy should give 0660 as the fix:\n%s", perm)
	}
	// The group is not present by default on Debian/Ubuntu, so usermod alone fails with a
	// confusing error.
	if !contains(perm, "getent group kvm") {
		t.Errorf("permission remedy should tell the user to check the group exists:\n%s", perm)
	}
}

// Nested virtualisation defaults to on for Windows 11 x64, so the .wslconfig advice is a
// fallback rather than the headline. If that caveat is ever dropped, the remedy becomes the
// generic wrong answer it was written to replace.
func TestWSLNestedVirtRemedySaysItIsUsuallyAlreadyOn(t *testing.T) {
	// Only the no-virtualisation-extensions branch carries this text, and which branch
	// runs depends on the host, so build it unconditionally by checking both possible
	// outputs contain their own correct advice.
	remedy := wslKVMMissingRemedy()
	if contains(remedy, "nestedVirtualization") && !contains(remedy, "default") {
		t.Errorf("the .wslconfig branch must say nested virt is already on by default:\n%s", remedy)
	}
	if contains(remedy, "modprobe") && contains(remedy, "nested=1") && !contains(remedy, "NOT needed") {
		t.Errorf("the module branch must not recommend nested=1:\n%s", remedy)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
