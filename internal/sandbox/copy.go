package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Copy moves files between the host and a running sandbox.
//
// The transfer is a tar stream through an exec'd `tar` in the guest, which is the only
// channel Boks has into a sandbox that does not depend on a guest agent. It therefore needs
// `tar` in the guest image — every usual base image, including busybox-based ones, has it —
// and a running sandbox.
//
// Host-side extraction treats the archive as hostile: entries that would land outside the
// destination are refused rather than written.
type CopyConfig struct {
	Address string
	// Name is the sandbox on one side of the copy.
	Name string
	// ToSandbox selects the direction: host to guest, or guest to host.
	ToSandbox bool
	// HostPath and GuestPath are the two ends, source and destination decided by
	// ToSandbox.
	HostPath  string
	GuestPath string
	// Warn receives a note for each symlink that was skipped because its target leaves the
	// destination. Nil discards them.
	Warn io.Writer
}

func Copy(ctx context.Context, cfg CopyConfig) error {
	if cfg.ToSandbox {
		return copyToSandbox(ctx, cfg)
	}
	return copyFromSandbox(ctx, cfg)
}

// copyToSandbox packs the host path and unpacks it inside the guest.
func copyToSandbox(ctx context.Context, cfg CopyConfig) error {
	info, err := os.Lstat(cfg.HostPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", cfg.HostPath, err)
	}

	// Destination semantics follow cp(1): copying into an existing directory keeps the
	// source's name, otherwise the destination path is the new name.
	targetDir, entryName := path.Dir(cfg.GuestPath), path.Base(cfg.GuestPath)
	isDir, err := guestIsDir(ctx, cfg, cfg.GuestPath)
	if err != nil {
		return err
	}
	if isDir {
		targetDir, entryName = cfg.GuestPath, filepath.Base(cfg.HostPath)
	}

	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeTar(pw, cfg.HostPath, entryName, info))
	}()

	var stderr bytes.Buffer
	code, err := Exec(ctx, ExecConfig{
		Address: cfg.Address,
		Name:    cfg.Name,
		// Arguments are passed as argv rather than interpolated into the script, so
		// a path with spaces or quotes cannot become shell syntax.
		Command: []string{"/bin/sh", "-c", `mkdir -p "$1" && exec tar -xmf - -C "$1"`, "sh", targetDir},
		Stdin:   pr,
		Stdout:  io.Discard,
		Stderr:  &stderr,
	})
	pr.Close()
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("unpacking into sandbox %q failed (exit %d): %s",
			cfg.Name, code, guestMessage(stderr.String()))
	}
	return nil
}

// copyFromSandbox packs the guest path and unpacks it on the host.
func copyFromSandbox(ctx context.Context, cfg CopyConfig) error {
	// Work out the destination name before starting the stream, so a bad destination
	// fails before anything is written.
	destDir, entryName := filepath.Dir(cfg.HostPath), filepath.Base(cfg.HostPath)
	if info, err := os.Stat(cfg.HostPath); err == nil && info.IsDir() {
		destDir, entryName = cfg.HostPath, path.Base(cfg.GuestPath)
	}

	pr, pw := io.Pipe()
	var stderr bytes.Buffer
	errC := make(chan error, 1)
	go func() {
		code, err := Exec(ctx, ExecConfig{
			Address: cfg.Address,
			Name:    cfg.Name,
			Command: []string{"/bin/sh", "-c", `exec tar -cf - -C "$(dirname "$1")" "$(basename "$1")"`, "sh", cfg.GuestPath},
			Stdout:  pw,
			Stderr:  &stderr,
		})
		if err == nil && code != 0 {
			err = fmt.Errorf("reading %s from sandbox %q failed (exit %d): %s",
				cfg.GuestPath, cfg.Name, code, guestMessage(stderr.String()))
		}
		pw.CloseWithError(err)
		errC <- err
	}()

	extractErr := extractTar(pr, destDir, entryName, cfg.Warn)
	pr.CloseWithError(extractErr)
	if err := <-errC; err != nil {
		return err
	}
	return extractErr
}

// guestIsDir reports whether a path inside the sandbox is an existing directory.
func guestIsDir(ctx context.Context, cfg CopyConfig, guestPath string) (bool, error) {
	code, err := Exec(ctx, ExecConfig{
		Address: cfg.Address,
		Name:    cfg.Name,
		Command: []string{"/bin/sh", "-c", `test -d "$1"`, "sh", guestPath},
		Stdout:  io.Discard,
		Stderr:  io.Discard,
	})
	if err != nil {
		return false, err
	}
	return code == 0, nil
}

// guestMessage tidies a guest error for display, and never returns an empty string, since
// "failed: " with nothing after it tells the user nothing.
func guestMessage(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "no output from the guest"
	}
	return s
}

// writeTar packs a host file or directory tree as entryName.
func writeTar(w io.Writer, srcPath, entryName string, info os.FileInfo) error {
	tw := tar.NewWriter(w)

	if !info.IsDir() {
		if err := writeTarEntry(tw, srcPath, entryName, info); err != nil {
			return err
		}
		return tw.Close()
	}

	err := filepath.Walk(srcPath, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcPath, p)
		if err != nil {
			return err
		}
		name := entryName
		if rel != "." {
			name = path.Join(entryName, filepath.ToSlash(rel))
		}
		return writeTarEntry(tw, p, name, fi)
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

func writeTarEntry(tw *tar.Writer, srcPath, name string, fi os.FileInfo) error {
	link := ""
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(srcPath)
		if err != nil {
			return err
		}
		link = target
	}

	header, err := tar.FileInfoHeader(fi, link)
	if err != nil {
		return err
	}
	header.Name = name
	if fi.IsDir() {
		header.Name += "/"
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return nil
	}

	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}

// extractTar unpacks a tar stream into destDir, renaming the archive's top-level component
// to entryName.
//
// The stream comes from inside a sandbox and is therefore untrusted. Two things are checked,
// and both are needed:
//
//   - Every entry's path is cleaned and must stay under destDir (resolveEntry). That alone is
//     not enough: it is a statement about the spelling of a path, and the archive can first
//     create a symlink and then name a file beneath it — `x/link -> /home/me/.ssh`, then
//     `x/link/authorized_keys` — which is inside the destination by every lexical test and
//     outside it on disk.
//   - So every write goes through an os.Root on destDir, which refuses to resolve any path
//     out of it, symlinks included; and a symlink whose target leaves the destination is
//     refused when it is made (checkLinkTarget), so the copy never plants a link that points
//     at the host's files for something else to follow later.
func extractTar(r io.Reader, destDir, entryName string, warn io.Writer) error {
	// skipped holds the places of symlinks left out, so an entry named beneath one is left
	// out too rather than creating a plain directory where the link would have been.
	skipped := map[string]bool{}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", destDir, err)
	}
	abs, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}
	dest, err := os.OpenRoot(abs)
	if err != nil {
		return fmt.Errorf("opening %s: %w", abs, err)
	}
	defer dest.Close()

	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the copied archive: %w", err)
		}

		target, err := resolveEntry(abs, header.Name, entryName)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(abs, target)
		if err != nil {
			return err
		}

		for p := rel; p != "." && p != "/" && p != ""; p = filepath.Dir(p) {
			if skipped[p] {
				rel = ""
				break
			}
		}
		if rel == "" {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := dest.MkdirAll(rel, os.FileMode(header.Mode).Perm()); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := dest.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
				return err
			}
			f, err := dest.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode).Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := checkLinkTarget(header.Name, rel, header.Linkname); err != nil {
				// Left out with a note, not an error: one stray link in a tree should not
				// make the rest of the copy fail. Nothing is written for it.
				skipped[rel] = true
				if warn != nil {
					fmt.Fprintf(warn, "boks: skipped %v\n", err)
				}
				continue
			}
			if err := dest.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
				return err
			}
			_ = dest.Remove(rel)
			if err := dest.Symlink(header.Linkname, rel); err != nil {
				return err
			}
		default:
			// Devices, fifos and hard links are not worth carrying across the
			// boundary, and creating them from untrusted input is a bad idea.
			continue
		}
	}
}

// checkLinkTarget refuses a symlink that points outside the copy destination: an absolute
// target, or a relative one that climbs out of it from where the link sits. rel is the link's
// own place under the destination.
func checkLinkTarget(name, rel, linkname string) error {
	slashed := filepath.ToSlash(linkname)
	if path.IsAbs(slashed) || filepath.IsAbs(linkname) || strings.HasPrefix(linkname, `\`) ||
		strings.ContainsRune(linkname, 0) {
		return fmt.Errorf("symlink %q: it points at %q, outside the copy destination",
			name, linkname)
	}
	resolved := path.Join(path.Dir(filepath.ToSlash(rel)), slashed)
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("symlink %q: it points at %q, outside the copy destination",
			name, linkname)
	}
	return nil
}

// resolveEntry maps an archive entry name onto a host path under root, replacing the
// archive's top-level component with entryName.
func resolveEntry(root, name, entryName string) (string, error) {
	slashed := filepath.ToSlash(name)
	if path.IsAbs(slashed) {
		return "", fmt.Errorf("refusing to write %q: the copied archive names an absolute path", name)
	}
	clean := path.Clean(slashed)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("refusing to write %q: it points outside the copy destination", name)
	}
	if clean == "." {
		return "", fmt.Errorf("the copied archive contains an entry with no name")
	}

	// Renaming the top-level component is what makes the destination path the new
	// name, as cp(1) does.
	parts := strings.Split(clean, "/")
	parts[0] = entryName

	target := filepath.Join(root, filepath.Join(parts...))
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing to write %q: it is outside the copy destination", name)
	}
	return target, nil
}
