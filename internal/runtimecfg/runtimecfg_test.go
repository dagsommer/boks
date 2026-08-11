package runtimecfg

import (
	"runtime"
	"testing"
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
