//go:build !windows

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/containerd/containerd/v2/pkg/cio"
)

// fifoDirOpts puts the FIFOs that carry a sandbox process's stdio in a directory of this
// user's, rather than containerd's default.
//
// cio's default is defaults.DefaultFIFODir: /var/run/containerd/fifo on macOS and
// /run/containerd/fifo on Linux, both root's. Boks runs unprivileged, so the default worked
// only on a machine where somebody had created /var/run/containerd with sudo — which Boks
// itself told people to do until 2026-10-02, for the shim socket. When that advice went, this
// dependency stayed, unseen on every machine still carrying the directory, until a restart
// emptied /var/run and every new sandbox failed with "mkdir /var/run/containerd: permission
// denied".
//
// The directory is under os.TempDir — $TMPDIR, private to the user, on macOS — and per uid,
// for Linux, where that is a shared /tmp. FIFOs are created by this process and opened by the
// shim, which runs as the same user, so nothing else needs to reach them; the directory is
// checked to be this user's and not a link before anything is created in it.
func fifoDirOpts() ([]cio.Opt, error) {
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("boks-%d", os.Getuid()), "fifo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating the directory for the sandbox's stdio: %w", err)
	}
	for _, d := range []string{filepath.Dir(dir), dir} {
		fi, err := os.Lstat(d)
		if err != nil {
			return nil, fmt.Errorf("checking %s: %w", d, err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() || fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("%s is not a private directory of this user's; remove it and try again", d)
		}
	}
	return []cio.Opt{cio.WithFIFODir(dir)}, nil
}
