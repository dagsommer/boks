package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/sandbox"
)

func newCpCommand(env Env, dev *devFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "cp [flags] SRC DST",
		Short: "Copy files between the host and a sandbox",
		Long: `Copies files and directories between the host and a running sandbox. Exactly one of the two
paths carries a SANDBOX: prefix; copying between two sandboxes is not supported.

The sandbox must be running, and its image must contain 'tar'.`,
		Example: `  boks cp ./config.yaml web:/etc/app/config.yaml
  boks cp web:/var/log/app ./logs`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 2 {
				return usagef("cp takes exactly two paths, a source and a destination")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := parseCopyArgs(args[0], args[1])
			if err != nil {
				return err
			}
			cfg.Address = dev.address
			return sandbox.Copy(cmd.Context(), cfg)
		},
	}
}

// copyEnd is one side of a cp argument: a host path, or a path inside a named sandbox.
type copyEnd struct {
	sandbox string
	path    string
}

func (e copyEnd) inSandbox() bool { return e.sandbox != "" }

// parseCopyArgs decides which side of the copy is the sandbox.
func parseCopyArgs(src, dst string) (sandbox.CopyConfig, error) {
	source, err := parseCopyEnd(src)
	if err != nil {
		return sandbox.CopyConfig{}, err
	}
	destination, err := parseCopyEnd(dst)
	if err != nil {
		return sandbox.CopyConfig{}, err
	}

	switch {
	case source.inSandbox() && destination.inSandbox():
		return sandbox.CopyConfig{}, fmt.Errorf(
			"copying between two sandboxes is not supported; copy via the host in two steps")
	case !source.inSandbox() && !destination.inSandbox():
		return sandbox.CopyConfig{}, fmt.Errorf(
			"one path must name a sandbox, as SANDBOX:PATH; neither %q nor %q does", src, dst)
	case destination.inSandbox():
		return sandbox.CopyConfig{
			Name:      destination.sandbox,
			ToSandbox: true,
			HostPath:  hostPath(source.path),
			GuestPath: destination.path,
		}, nil
	default:
		return sandbox.CopyConfig{
			Name:      source.sandbox,
			HostPath:  hostPath(destination.path),
			GuestPath: source.path,
		}, nil
	}
}

// parseCopyEnd splits a "SANDBOX:PATH" argument.
//
// A colon is legal in a host path, so the prefix only counts as a sandbox name if it looks
// like one: no separators, and a valid containerd identifier. "./a:b" and "/tmp/a:b" are
// host paths; "web:/tmp" is a sandbox path. A Windows drive letter ("C:\src") is a host path
// for the same reason — a one-character name would still be valid, so the separator check
// is what distinguishes it.
func parseCopyEnd(arg string) (copyEnd, error) {
	if arg == "" {
		return copyEnd{}, fmt.Errorf("empty path")
	}
	idx := strings.Index(arg, ":")
	if idx <= 0 {
		return copyEnd{path: arg}, nil
	}
	name, rest := arg[:idx], arg[idx+1:]
	if strings.ContainsAny(name, `/\.`) || sandbox.ValidateName(name) != nil {
		return copyEnd{path: arg}, nil
	}
	if rest == "" {
		return copyEnd{}, fmt.Errorf("%q names a sandbox but no path inside it", arg)
	}
	if !strings.HasPrefix(rest, "/") {
		return copyEnd{}, fmt.Errorf("path %q inside sandbox %q must be absolute", rest, name)
	}
	return copyEnd{sandbox: name, path: rest}, nil
}

// hostPath makes the host end absolute, so that the guest side of the copy never has to
// reason about the caller's working directory.
func hostPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
