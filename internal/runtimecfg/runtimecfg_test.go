package runtimecfg

import (
	"runtime"
	"testing"

	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestShimBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("binary names carry a .exe suffix on Windows")
	}
	tests := []struct {
		handler string
		want    string
	}{
		{"io.containerd.nerdbox.v1", "containerd-shim-nerdbox-v1"},
		{"io.containerd.runc.v2", "containerd-shim-runc-v2"},
		// containerd joins interior components, so a dotted name round-trips.
		{"io.containerd.runhcs.wcow.v1", "containerd-shim-runhcs.wcow-v1"},
		// Handlers that do not follow containerd's convention yield no binary
		// rather than a guess.
		{"nerdbox", ""},
		{"io.containerd.v1", ""},
		{"", ""},
		{"com.example.runtime.v1", ""},
	}
	for _, tt := range tests {
		if got := ShimBinary(tt.handler); got != tt.want {
			t.Errorf("ShimBinary(%q) = %q, want %q", tt.handler, got, tt.want)
		}
	}
}

// The default runtime must be the isolating one: this is the check that stops Boks
// presenting a shared-kernel runtime as a sandbox.
func TestIsolatedRuntime(t *testing.T) {
	if !IsolatedRuntime(Runtime) {
		t.Errorf("IsolatedRuntime(%q) = false, want true for the default runtime", Runtime)
	}
	for _, handler := range []string{"io.containerd.runc.v2", "io.containerd.runsc.v1", ""} {
		if IsolatedRuntime(handler) {
			t.Errorf("IsolatedRuntime(%q) = true, want false: it shares the host kernel", handler)
		}
	}
}

// The guest platform is Linux on every host. The architecture follows the host, because that
// is what the hardware can execute; the operating system does not, because a microVM booting
// a Linux kernel runs Linux binaries wherever it was started from.
func TestGuestPlatformIsLinuxOnTheHostArchitecture(t *testing.T) {
	want := "linux/" + runtime.GOARCH
	if got := GuestPlatform(); got != want {
		t.Errorf("GuestPlatform() = %q, want %q", got, want)
	}
}

// The bug this replaced, reconstructed rather than waited for: Boks pulled against
// platforms.Default(), which on a Windows host is windows/<arch>. The two assertions below are
// the two halves of that — a Windows-shaped matcher refuses the manifest Boks needs, and the
// guest matcher accepts it — and both run on any host, because neither asks what the host is.
func TestGuestPlatformMatcherIgnoresTheHostOS(t *testing.T) {
	guest := ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH}
	matcher := guestPlatformMatcher()

	if !matcher.Match(guest) {
		t.Fatalf("the guest matcher rejects %s/%s, which is the only thing a sandbox can run",
			guest.OS, guest.Architecture)
	}

	// What a Windows host's default resolves to. Boks must never be built on this.
	hostShaped := platforms.Only(ocispec.Platform{OS: "windows", Architecture: runtime.GOARCH})
	if hostShaped.Match(guest) {
		t.Fatal("a windows/<arch> matcher matched a Linux manifest; " +
			"the regression this test guards cannot be reproduced, so it is not being tested")
	}

	// And the guest matcher must not drift the other way either: a Windows or Darwin
	// manifest cannot be executed by a Linux kernel, so silently accepting one would only
	// move the failure into the VM.
	for _, foreign := range []ocispec.Platform{
		{OS: "windows", Architecture: runtime.GOARCH},
		{OS: "darwin", Architecture: runtime.GOARCH},
	} {
		if matcher.Match(foreign) {
			t.Errorf("the guest matcher accepted %s/%s", foreign.OS, foreign.Architecture)
		}
	}
}

// The change must be invisible on the hosts Boks already runs on. It is, and for a concrete
// reason: on Linux platforms.Default() *is* linux/<arch>, and on macOS it is darwin/<arch>
// followed by linux/<arch> — both already accept exactly the platform asked for here, so
// asking for it explicitly cannot change which manifest is chosen.
func TestGuestPlatformChangesNothingOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the whole point is that the host default is wrong here")
	}
	guest := ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH}
	if !platforms.Default().Match(guest) {
		t.Fatalf("platforms.Default() on %s does not match %s/%s, so this is not a no-op after all",
			runtime.GOOS, guest.OS, guest.Architecture)
	}

	// Agreement, not merely overlap, for the platforms a sandbox could plausibly be
	// offered. Architectures the host cannot execute must be refused by both.
	matcher := guestPlatformMatcher()
	for _, candidate := range []ocispec.Platform{
		guest,
		{OS: "linux", Architecture: "ppc64le"},
		{OS: "linux", Architecture: "s390x"},
	} {
		if got, want := matcher.Match(candidate), platforms.Default().Match(candidate); got != want {
			t.Errorf("%s/%s: guest matcher = %v, platforms.Default() = %v",
				candidate.OS, candidate.Architecture, got, want)
		}
	}
}

// The snapshotter is not the host's either. The Windows and LCOW snapshotters produce layers
// a Windows kernel mounts; a Boks guest mounts its image as an EROFS filesystem inside the VM,
// so the answer is the same on every host and there is no platform switch to get wrong.
func TestSnapshotterIsGuestSideOnEveryHost(t *testing.T) {
	if Snapshotter != "erofs" {
		t.Errorf("Snapshotter = %q, want \"erofs\": the guest mounts the image, not the host", Snapshotter)
	}
}

func TestDefaultAddressHonoursEnv(t *testing.T) {
	t.Setenv("BOKS_CONTAINERD_ADDRESS", "/custom/containerd.sock")
	if got := DefaultAddress(); got != "/custom/containerd.sock" {
		t.Errorf("DefaultAddress() = %q, want the value from BOKS_CONTAINERD_ADDRESS", got)
	}

	t.Setenv("BOKS_CONTAINERD_ADDRESS", "")
	if got := DefaultAddress(); got == "" {
		t.Error("DefaultAddress() = \"\", want a platform default")
	}
}
