package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"strings"

	"github.com/dagsommer/boks/internal/runtimecfg"
	"github.com/dagsommer/boks/internal/sandbox"
	"github.com/dagsommer/boks/internal/workspace"
)

// defaultImage is a small, widely mirrored base image. It is a starting point for proving
// the runtime works, not a curated agent environment.
const defaultImage = "docker.io/library/alpine:latest"

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func runCommand(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("boks run", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)

	var (
		image     = fs.String("image", defaultImage, "OCI image providing the guest root filesystem")
		name      = fs.String("name", "", "sandbox name (default: generated)")
		cpus      = fs.Int("cpus", 2, "vCPUs for the guest")
		memory    = fs.Int("memory", 2048, "guest memory in MiB")
		tty       = fs.Bool("t", false, "allocate a pseudo-terminal")
		address   = fs.String("containerd-address", runtimecfg.DefaultAddress(), "containerd socket")
		runtimeID = fs.String("runtime", runtimecfg.Runtime, "containerd runtime handler")
		snapshot  = fs.String("snapshotter", runtimecfg.Snapshotter, "containerd snapshotter")
		insecure  = fs.Bool("i-know-this-is-not-isolated", false,
			"permit a non-VM runtime; for developing Boks itself, never for running untrusted code")
		envVars     stringList
		mounts      stringList
		annotations stringList
	)
	fs.Var(&envVars, "env", "extra environment variable KEY=VALUE (repeatable)")
	fs.Var(&mounts, "mount", "extra host directory to share, PATH or PATH:ro (repeatable)")
	fs.Var(&annotations, "annotation", "extra OCI annotation KEY=VALUE passed to the runtime (repeatable)")

	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks run [flags] <workspace> [-- command [args...]]

Runs a command inside an isolated microVM. The workspace directory is shared into the
guest at the same absolute path it has on the host, and becomes the process's working
directory. Nothing above it is exposed.

The sandbox is ephemeral: it is removed when the command exits.

Examples:
  boks run . -- uname -a
  boks run /home/alice/src/foo -- sh -lc 'pwd && ls'

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

	primary, err := workspace.Parse(positional[0])
	if err != nil {
		return err
	}
	workspaces := []workspace.Workspace{primary}
	for _, m := range mounts {
		ws, err := workspace.Parse(m)
		if err != nil {
			return err
		}
		workspaces = append(workspaces, ws)
	}

	if !runtimecfg.IsolatedRuntime(*runtimeID) {
		if !*insecure {
			return fmt.Errorf(
				"runtime %q does not provide a virtual machine boundary.\n"+
					"Boks refuses to present it as a sandbox. The isolating runtime is %q.\n"+
					"If you are developing Boks itself and want to exercise the containerd path\n"+
					"without a hypervisor, pass -i-know-this-is-not-isolated.",
				*runtimeID, runtimecfg.Runtime)
		}
		fmt.Fprintf(env.Stderr,
			"WARNING: running with runtime %q, which shares the host kernel.\n"+
				"         This is NOT an isolation boundary. Do not run untrusted code.\n\n",
			*runtimeID)
	}

	parsedAnnotations, err := parseKeyValues(annotations)
	if err != nil {
		return err
	}

	sandboxName := *name
	if sandboxName == "" {
		sandboxName, err = generateName()
		if err != nil {
			return err
		}
	}

	code, err := sandbox.Run(ctx, sandbox.Config{
		Name:        sandboxName,
		Image:       *image,
		Command:     command,
		Workspaces:  workspaces,
		Env:         envVars,
		CPUs:        *cpus,
		MemoryMiB:   *memory,
		Runtime:     *runtimeID,
		Snapshotter: *snapshot,
		Address:     *address,
		Annotations: parsedAnnotations,
		TTY:         *tty,
		Stdin:       env.Stdin,
		Stdout:      env.Stdout,
		Stderr:      env.Stderr,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

// parseKeyValues turns repeated KEY=VALUE flags into a map.
//
// An empty key is rejected rather than silently dropped, since a typo would otherwise
// produce a sandbox that quietly lacks the setting the user asked for.
func parseKeyValues(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, found := strings.Cut(entry, "=")
		if !found || key == "" {
			return nil, fmt.Errorf("invalid KEY=VALUE %q", entry)
		}
		out[key] = value
	}
	return out, nil
}

// parseInterspersed parses flags that may appear before or after positional arguments.
//
// The flag package stops at the first non-flag argument, which would make
// `boks run . -t` treat -t as a positional. Parsing repeatedly, consuming one positional
// each time, accepts flags in any order without hand-rolling a parser that has to know
// which flags take values.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// splitAtDoubleDash separates boks' own arguments from the guest command.
//
// The standard flag package stops at "--" but discards it, which would make
// `boks run . -- ls -l` indistinguishable from a flag error. Splitting first keeps the
// guest's flags out of our parser entirely.
func splitAtDoubleDash(args []string) (own []string, command []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// generateName produces a unique sandbox name. containerd identifiers must be stable and
// unique within a namespace; random suffixes avoid collisions between concurrent runs.
func generateName() (string, error) {
	buf := make([]byte, 5)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating sandbox name: %w", err)
	}
	return "boks-" + hex.EncodeToString(buf), nil
}
