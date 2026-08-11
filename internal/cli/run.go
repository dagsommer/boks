package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"

	"github.com/dagsommer/boks/internal/sandbox"
)

func runCommand(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("boks run", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)

	flags := registerSandboxFlags(fs)
	tty := fs.Bool("t", false, "allocate a pseudo-terminal")
	ephemeral := fs.Bool("rm", false, "destroy the sandbox when the command exits")

	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks run [flags] <workspace> [-- command [args...]]

Runs a command inside an isolated microVM. The workspace directory is shared into the
guest at the same absolute path it has on the host, and becomes the process's working
directory. Nothing above it is exposed.

The sandbox persists. Running again for the same workspace re-attaches to it, so packages
installed and files written inside it are still there; remove it with 'boks rm'. Pass -rm
for a sandbox that is destroyed when the command exits.

Examples:
  boks run . -- uname -a
  boks run . -- sh -lc 'pwd && ls'
  boks run -rm /home/alice/src/foo -- go test ./...

Flags:
`)
		fs.PrintDefaults()
	}

	args, command := splitAtDoubleDash(env.Args)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return flagErrHelp
		}
		return err
	}

	if len(positional) < 1 {
		fs.Usage()
		return fmt.Errorf("a workspace directory is required")
	}
	if len(positional) > 1 {
		return fmt.Errorf("unexpected argument %q; put the guest command after '--'", positional[1])
	}

	workspaces, err := flags.workspaces(positional[0])
	if err != nil {
		return err
	}
	if err := flags.requireIsolation(env.Stderr); err != nil {
		return err
	}

	name, err := flags.sandboxName(workspaces[0])
	if err != nil {
		return err
	}
	if *ephemeral && *flags.name == "" {
		// An ephemeral sandbox must not collide with the persistent one for the same
		// workspace, nor with another ephemeral run of it.
		if name, err = generateName(); err != nil {
			return err
		}
	}

	cfg, err := flags.config(name, workspaces)
	if err != nil {
		return err
	}
	cfg.Command = command
	cfg.TTY = *tty
	cfg.Ephemeral = *ephemeral
	cfg.Stdin = env.Stdin
	cfg.Stdout = env.Stdout
	cfg.Stderr = env.Stderr

	code, err := sandbox.Run(ctx, cfg)
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

// generateName produces a unique sandbox name for a run that has no lasting identity.
// containerd identifiers must be unique within a namespace; a random suffix avoids
// collisions between concurrent runs.
func generateName() (string, error) {
	buf := make([]byte, 5)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating sandbox name: %w", err)
	}
	return sandbox.NamePrefix + hex.EncodeToString(buf), nil
}
