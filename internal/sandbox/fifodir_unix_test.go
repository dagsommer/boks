//go:build !windows

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The stdio FIFOs must not depend on root's /var/run/containerd, which macOS empties at
// every boot: every new sandbox failed after a restart until this chose a directory of the
// user's own.
func TestFIFODirIsThisUsersOwn(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	opts, err := fifoDirOpts()
	if err != nil {
		t.Fatal(err)
	}
	if len(opts) != 1 {
		t.Fatalf("got %d options, want the FIFO directory", len(opts))
	}
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("boks-%d", os.Getuid()), "fifo")
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("%s has mode %v, want 0700", dir, fi.Mode().Perm())
	}
}

// A directory someone else could write in is refused rather than used: on Linux the parent
// is a shared /tmp, where another user could have put it there first.
func TestFIFODirRefusesADirectoryOthersCanWrite(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	parent := filepath.Join(os.TempDir(), fmt.Sprintf("boks-%d", os.Getuid()))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := fifoDirOpts(); err == nil {
		t.Fatal("a world-writable directory was accepted for the sandbox's stdio")
	}
}
