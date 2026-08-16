package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// The archive layout: mkfs.erofs beside boks.exe, and that directory on nobody's PATH.
//
// Measured on Windows from the v0.1.0 archive on 2026-08-16. `boks doctor` said
// `snapshotter tools ok` naming the file; `boks daemon start`, two commands later, wrote a
// config omitting the erofs differ because it asked the shell's PATH; every image pull then
// died in the Windows differ. containerd would have found it — ContainerdPath hands it that
// directory — so the config was refusing a capability the daemon actually had.
func TestHasEROFSFindsTheBundledTool(t *testing.T) {
	bundle := t.TempDir()
	name := "mkfs.erofs"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(bundle, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A PATH that deliberately excludes the bundle, which is the whole point: no installer
	// puts Boks' private runtime directory on the user's PATH.
	t.Setenv("PATH", t.TempDir())
	if _, err := exec.LookPath(name); err == nil {
		t.Fatal("the tool is already on the shell PATH, so this test would prove nothing")
	}

	t.Setenv(runtimeDirEnv, bundle)
	if !HasEROFS() {
		t.Error("HasEROFS() = false with mkfs.erofs in the runtime directory; the generated " +
			"config would omit the erofs differ and every image pull would fail in the " +
			"platform differ")
	}
}
