package sandbox

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTarRoundTripDirectory(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "top.txt"), []byte("top"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "nested.txt"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Creating a symlink on Windows needs SeCreateSymbolicLinkPrivilege, which an
	// unelevated account only has with Developer Mode turned on. Where the privilege is
	// there — every Unix, and a Windows box set up for it — the symlink half of the round
	// trip is tested exactly as before; where it is not, the fixture cannot be built and
	// the rest of the archive is still worth checking. Any other operating system failing
	// to make a symlink is a real failure and stays fatal.
	symlinked := true
	if err := os.Symlink("top.txt", filepath.Join(src, "link")); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		symlinked = false
		t.Logf("not testing symlinks: this account cannot create one (%v)", err)
	}

	info, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeTar(&buf, src, "payload", info); err != nil {
		t.Fatalf("writeTar: %v", err)
	}

	dest := t.TempDir()
	// The archive is unpacked under a different name, which is what makes
	// `boks cp sandbox:/a/b ./c` land in ./c rather than ./c/b.
	if err := extractTar(&buf, dest, "renamed"); err != nil {
		t.Fatalf("extractTar: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "renamed", "sub", "nested.txt"))
	if err != nil {
		t.Fatalf("nested file did not survive the round trip: %v", err)
	}
	if string(got) != "nested" {
		t.Errorf("nested.txt = %q, want %q", got, "nested")
	}
	if symlinked {
		if target, err := os.Readlink(filepath.Join(dest, "renamed", "link")); err != nil || target != "top.txt" {
			t.Errorf("symlink = %q, %v; want top.txt", target, err)
		}
	}
	info, err = os.Stat(filepath.Join(dest, "renamed", "sub", "nested.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// `boks cp` carrying 0600 out of a sandbox and landing it 0644 on the host would
	// disclose a file the guest kept private, so the mode is part of what the archive
	// round-trips. Windows has no POSIX mode to round-trip to: NTFS stores an ACL, and
	// os.FileMode reports 0666 for any writable file, so the extracted file's real
	// readability comes from the ACL it inherits from the destination directory. Boks
	// neither sets nor checks that ACL — an open gap, recorded in docs/windows.md, that
	// this guard makes visible rather than closes.
	if runtime.GOOS != "windows" {
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600 preserved", info.Mode().Perm())
		}
	}
}

func TestTarRoundTripSingleFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "one.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := writeTar(&buf, src, "one.txt", info); err != nil {
		t.Fatalf("writeTar: %v", err)
	}

	dest := t.TempDir()
	if err := extractTar(&buf, dest, "copied.txt"); err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "copied.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("copied.txt = %q, want %q", got, "hello")
	}
}

// The archive comes from inside a sandbox, so it is untrusted input. An entry that climbs
// out of the destination must be refused, not written.
func TestExtractTarRefusesEscape(t *testing.T) {
	for _, name := range []string{"payload/../../escape.txt", "/etc/escape.txt", "../escape.txt"} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Mode:     0o644,
			Size:     int64(len("owned")),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("owned")); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}

		dest := filepath.Join(t.TempDir(), "into")
		if err := extractTar(&buf, dest, "payload"); err == nil {
			t.Errorf("extractTar accepted %q", name)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "escape.txt")); err == nil {
			t.Errorf("extractTar wrote outside the destination for %q", name)
		}
	}
}

func TestResolveEntryRenamesTopLevel(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "dest")
	got, err := resolveEntry(root, "payload/sub/file.txt", "renamed")
	if err != nil {
		t.Fatalf("resolveEntry: %v", err)
	}
	want := filepath.Join(root, "renamed", "sub", "file.txt")
	if got != want {
		t.Errorf("resolveEntry = %q, want %q", got, want)
	}
}
