package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The writable-layer note is the check that was missing for the whole Windows bring-up, and
// the reason macOS looked fine for three days on a host that happened to have e2fsprogs.
//
// Off Linux containerd's erofs snapshotter formats an ext4 image per active snapshot with
// mkfs.ext4, at task start, with no configuration that turns it off. Measured on Windows 11 on
// 2026-08-16 after a complete image pull:
//
//	failed format "...\snapshots\11\rwlayer.img": mkfs.ext4 failed: :
//	exec: "mkfs.ext4": executable file not found in %PATH%
//
// Both directions are asserted, and the negative one carries the weight: a note that fired on
// Linux as well would fail every Linux host for a binary containerd never runs there.
func TestWritableLayerNoteFiresWhereTheLayerIsFormatted(t *testing.T) {
	for _, goos := range []string{"windows", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			note := writableLayerNote(Settings{GOOS: goos, Ext4: false})
			if note == nil {
				t.Fatalf("no note for %s without mkfs.ext4; the daemon would start clean and "+
					"every 'boks run' would die at task start in containerd's mount manager", goos)
			}
			if note.Name != "mkfs.ext4" {
				t.Errorf("note names %q, not the binary the user has to install", note.Name)
			}
			if !strings.Contains(note.Remedy, "rwlayer.img") {
				t.Errorf("remedy does not quote the failure the user will actually see:\n%s", note.Remedy)
			}
		})
	}
}

// Linux is the whole reason blockWritableLayer takes an argument. containerd's
// defaultWritableSize is 0 there (erofs_linux.go), so blockMode is off, so no rwlayer.img is
// ever created and mkfs.ext4 is never invoked.
func TestWritableLayerNoteIsSilentOnLinux(t *testing.T) {
	if note := writableLayerNote(Settings{GOOS: "linux", Ext4: false}); note != nil {
		t.Errorf("Linux without mkfs.ext4 produced a note: %q / %q\nLinux runs the erofs "+
			"snapshotter in ovlfs mode and never formats a writable layer", note.Detail, note.Remedy)
	}
}

// And it must go quiet once the binary is there, or it is not a check, it is a banner.
func TestWritableLayerNoteIsSilentWhenTheToolIsPresent(t *testing.T) {
	for _, goos := range []string{"windows", "darwin", "linux"} {
		if note := writableLayerNote(Settings{GOOS: goos, Ext4: true}); note != nil {
			t.Errorf("%s with mkfs.ext4 present still warned: %q", goos, note.Detail)
		}
	}
}

// macOS is told about the keg, because that is the step that is not obvious: `brew install
// e2fsprogs` succeeds and leaves mkfs.ext4 on no PATH at all. Windows is told about the
// archive, because there is no package manager answer there.
func TestWritableLayerRemedyIsPlatformSpecific(t *testing.T) {
	remedy := func(goos string) string {
		note := writableLayerNote(Settings{GOOS: goos})
		if note == nil {
			t.Fatalf("no note for %s without mkfs.ext4", goos)
		}
		return note.Remedy
	}

	darwin := remedy("darwin")
	if !strings.Contains(darwin, "e2fsprogs") || !strings.Contains(darwin, "keg-only") {
		t.Errorf("macOS remedy does not say the package is keg-only:\n%s", darwin)
	}
	windows := remedy("windows")
	if !strings.Contains(windows, "mkfs.ext4.exe") {
		t.Errorf("Windows remedy does not name the shipped binary:\n%s", windows)
	}
	if strings.Contains(windows, "brew") {
		t.Errorf("Windows remedy tells the user to use Homebrew:\n%s", windows)
	}
}

// The shim-socket note is Unix-only, and Windows is where that was learned the hard way.
//
// `boks daemon start` on Windows 11 warned about C:\ProgramData\containerd\state on
// 2026-08-16, on a machine where the path did not exist and where sandboxes then started, ran,
// enforced policy and stopped. The note is about a Unix domain socket in a directory
// containerd compiles in; on Windows a shim is reached over a named pipe, which is not a file
// in any directory — containerd's pkg/shim/util_windows.go has no socketRoot and no
// writeSocketDir, and its RemoveSocket does nothing.
//
// A warning that fires on a host it does not apply to is worse than no warning: it is the one
// that teaches the reader to skip the next one.
func TestShimSocketNoteIsSilentOnWindows(t *testing.T) {
	if note := shimSocketRootNote("windows"); note != nil {
		t.Errorf("Windows was warned about %q\nremedy:\n%s\nA shim on Windows is a named pipe; "+
			"that directory is not in the path of anything.", note.Detail, note.Remedy)
	}
}

// The other direction, on the platform running this test. It must still be able to fire, or the
// fix above deleted a real check instead of narrowing it: a directory that cannot be created
// and cannot be written is the failure this note exists to move forward in time.
func TestShimSocketNoteStillFiresOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the note does not apply here, which is what the test above is about")
	}
	// The note creates the directory when it can, so afterwards the host is in one of two
	// states and the answer has to match the one it is in. Whichever way this host falls,
	// one of the two branches is exercised.
	note := shimSocketRootNote(runtime.GOOS)
	root := ShimSocketRoot()
	if writableDir(root) {
		if note != nil {
			t.Errorf("a host that can write %s was told it could not:\n%s", root, note.Remedy)
		}
		return
	}
	if note == nil {
		t.Fatalf("%s can be neither created nor written and nothing was said; the failure "+
			"then arrives later as 'creating sandbox process: mkdir %s: permission denied'", root, root)
	}
	if !strings.Contains(note.Remedy, "sudo mkdir") {
		t.Errorf("the remedy does not give the one command that fixes it:\n%s", note.Remedy)
	}
	if strings.Contains(note.Remedy, "elevated") {
		t.Errorf("a Unix host was told to run the daemon elevated:\n%s", note.Remedy)
	}
}

// HasExt4 has to ask containerd's PATH, not the shell's, for exactly the reason HasEROFS does:
// the Windows archive puts mkfs.ext4.exe beside boks.exe and no installer puts that directory
// on anyone's PATH. Asking exec.LookPath would report the binary missing on the one layout
// that ships it.
func TestHasExt4FindsTheBundledTool(t *testing.T) {
	bundle := t.TempDir()
	name := "mkfs.ext4"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(bundle, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := exec.LookPath(name); err == nil {
		t.Fatal("the tool is already on the shell PATH, so this test would prove nothing")
	}

	t.Setenv(runtimeDirEnv, bundle)
	if !HasExt4() {
		t.Error("HasExt4() = false with mkfs.ext4 in the runtime directory; 'boks daemon start' " +
			"would warn that no sandbox can start while containerd could run it perfectly well")
	}
}
