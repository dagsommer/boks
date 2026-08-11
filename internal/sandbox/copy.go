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

	extractErr := extractTar(pr, destDir, entryName)
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
// The stream comes from inside a sandbox and is therefore untrusted: an entry naming
// "../../etc/cron.d/x" would otherwise write outside the destination. Every path is checked
// after cleaning, and anything that escapes is refused.
func extractTar(r io.Reader, destDir, entryName string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", destDir, err)
	}
	root, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}

	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the copied archive: %w", err)
		}

		target, err := resolveEntry(root, header.Name, entryName)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode).Perm()); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode).Perm())
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
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		default:
			// Devices, fifos and hard links are not worth carrying across the
			// boundary, and creating them from untrusted input is a bad idea.
			continue
		}
	}
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
