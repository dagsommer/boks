package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestParseResolvesToAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	ws, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse(%q): %v", dir, err)
	}
	if ws.HostPath != real {
		t.Errorf("HostPath = %q, want %q", ws.HostPath, real)
	}
	// The guest path equalling the host path is the exact-path property Boks exists to
	// provide; a regression here silently breaks absolute paths inside the sandbox.
	if ws.GuestPath != ws.HostPath {
		t.Errorf("GuestPath = %q, want it to equal HostPath %q", ws.GuestPath, ws.HostPath)
	}
	if ws.Mode != ModeReadWrite {
		t.Errorf("Mode = %q, want %q", ws.Mode, ModeReadWrite)
	}
}

func TestParseRelativePath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	ws, err := Parse(".")
	if err != nil {
		t.Fatalf("Parse(\".\"): %v", err)
	}
	if !filepath.IsAbs(ws.GuestPath) {
		t.Errorf("GuestPath = %q, want an absolute path", ws.GuestPath)
	}
}

func TestParseModeSuffix(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name     string
		arg      string
		wantMode Mode
		wantRO   bool
	}{
		{"read-only suffix", dir + ":ro", ModeReadOnly, true},
		{"read-write suffix", dir + ":rw", ModeReadWrite, false},
		{"no suffix defaults to rw", dir, ModeReadWrite, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws, err := Parse(tt.arg)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.arg, err)
			}
			if ws.Mode != tt.wantMode {
				t.Errorf("Mode = %q, want %q", ws.Mode, tt.wantMode)
			}
			if ws.ReadOnly() != tt.wantRO {
				t.Errorf("ReadOnly() = %v, want %v", ws.ReadOnly(), tt.wantRO)
			}
		})
	}
}

func TestParseColonInPathIsNotAMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("colons are not valid in Windows path components")
	}
	parent := t.TempDir()
	weird := filepath.Join(parent, "odd:name")
	if err := os.Mkdir(weird, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	ws, err := Parse(weird)
	if err != nil {
		t.Fatalf("Parse(%q): %v", weird, err)
	}
	if filepath.Base(ws.GuestPath) != "odd:name" {
		t.Errorf("GuestPath = %q, want the trailing colon segment preserved", ws.GuestPath)
	}
}

func TestParseRejectsMissingPath(t *testing.T) {
	_, err := Parse(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("Parse succeeded for a path that does not exist")
	}
}

// A file bind mount would expose the file's entire parent directory to the guest, because
// that is how the runtime implements it. Refusing files keeps that surprise out of Boks.
func TestParseRejectsFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Parse(file)
	if err == nil {
		t.Fatal("Parse accepted a regular file; it must share directories only")
	}
}

func TestParseRejectsEmpty(t *testing.T) {
	if _, err := Parse(""); err == nil {
		t.Fatal("Parse accepted an empty path")
	}
}

func TestParseResolvesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	ws, err := Parse(link)
	if err != nil {
		t.Fatalf("Parse(%q): %v", link, err)
	}
	realTarget, _ := filepath.EvalSymlinks(target)
	if ws.HostPath != realTarget {
		t.Errorf("HostPath = %q, want the symlink resolved to %q", ws.HostPath, realTarget)
	}
}

func TestMountOptions(t *testing.T) {
	rw := Workspace{Mode: ModeReadWrite}.MountOptions()
	if !slices.Contains(rw, "rw") {
		t.Errorf("read-write options = %v, want to contain \"rw\"", rw)
	}
	if slices.Contains(rw, "ro") {
		t.Errorf("read-write options = %v, must not contain \"ro\"", rw)
	}

	ro := Workspace{Mode: ModeReadOnly}.MountOptions()
	if !slices.Contains(ro, "ro") {
		t.Errorf("read-only options = %v, want to contain \"ro\"", ro)
	}

	// rprivate keeps mount events inside the sandbox from propagating to the host.
	for _, opts := range [][]string{rw, ro} {
		if !slices.Contains(opts, "rbind") || !slices.Contains(opts, "rprivate") {
			t.Errorf("options = %v, want rbind and rprivate", opts)
		}
	}
}
