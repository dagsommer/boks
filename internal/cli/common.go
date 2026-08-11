package cli

import (
	"flag"
	"fmt"
	"io"
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

// sandboxFlags are the flags that describe a sandbox to create. `run` and `create` build the
// same thing and must accept the same options, so they register them from one place.
type sandboxFlags struct {
	image       *string
	name        *string
	cpus        *int
	memory      *int
	runtimeID   *string
	snapshotter *string
	address     *string
	insecure    *bool
	env         stringList
	mounts      stringList
	annotations stringList
}

func registerSandboxFlags(fs *flag.FlagSet) *sandboxFlags {
	f := &sandboxFlags{
		image:       fs.String("image", defaultImage, "OCI image providing the guest root filesystem"),
		name:        fs.String("name", "", "sandbox name (default: derived from the workspace path)"),
		cpus:        fs.Int("cpus", 2, "vCPUs for the guest"),
		memory:      fs.Int("memory", 2048, "guest memory in MiB"),
		runtimeID:   fs.String("runtime", runtimecfg.Runtime, "containerd runtime handler"),
		snapshotter: fs.String("snapshotter", runtimecfg.Snapshotter, "containerd snapshotter"),
		address:     addressFlag(fs),
		insecure: fs.Bool("i-know-this-is-not-isolated", false,
			"permit a non-VM runtime; for developing Boks itself, never for running untrusted code"),
	}
	fs.Var(&f.env, "env", "extra environment variable KEY=VALUE (repeatable)")
	fs.Var(&f.mounts, "mount", "extra host directory to share, PATH or PATH:ro (repeatable)")
	fs.Var(&f.annotations, "annotation", "extra OCI annotation KEY=VALUE passed to the runtime (repeatable)")
	return f
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

// addressFlag registers the containerd socket flag, which every command needs.
func addressFlag(fs *flag.FlagSet) *string {
	return fs.String("containerd-address", runtimecfg.DefaultAddress(), "containerd socket")
}

// workspaces resolves the primary workspace argument plus any -mount into guest shares.
func (f *sandboxFlags) workspaces(primaryArg string) ([]workspace.Workspace, error) {
	primary, err := workspace.Parse(primaryArg)
	if err != nil {
		return nil, err
	}
	workspaces := []workspace.Workspace{primary}
	for _, m := range f.mounts {
		ws, err := workspace.Parse(m)
		if err != nil {
			return nil, err
		}
		workspaces = append(workspaces, ws)
	}
	return workspaces, nil
}

// sandboxName decides which sandbox this invocation is about: the name given, or the one
// derived from the workspace path so that a second invocation from the same directory finds
// the same sandbox.
func (f *sandboxFlags) sandboxName(primary workspace.Workspace) (string, error) {
	if *f.name == "" {
		return sandbox.DeriveName(primary.HostPath), nil
	}
	if err := sandbox.ValidateName(*f.name); err != nil {
		return "", err
	}
	return *f.name, nil
}

// requireIsolation refuses to present a container-only runtime as a sandbox, unless the
// caller has explicitly said they are developing Boks itself.
func (f *sandboxFlags) requireIsolation(stderr io.Writer) error {
	if runtimecfg.IsolatedRuntime(*f.runtimeID) {
		return nil
	}
	if !*f.insecure {
		return fmt.Errorf(
			"runtime %q does not provide a virtual machine boundary.\n"+
				"Boks refuses to present it as a sandbox. The isolating runtime is %q.\n"+
				"If you are developing Boks itself and want to exercise the containerd path\n"+
				"without a hypervisor, pass -i-know-this-is-not-isolated.",
			*f.runtimeID, runtimecfg.Runtime)
	}
	fmt.Fprintf(stderr,
		"WARNING: running with runtime %q, which shares the host kernel.\n"+
			"         This is NOT an isolation boundary. Do not run untrusted code.\n\n",
		*f.runtimeID)
	return nil
}

func (f *sandboxFlags) config(name string, workspaces []workspace.Workspace) (sandbox.Config, error) {
	annotations, err := parseKeyValues(f.annotations)
	if err != nil {
		return sandbox.Config{}, err
	}
	return sandbox.Config{
		Name:        name,
		Image:       *f.image,
		Workspaces:  workspaces,
		Env:         f.env,
		CPUs:        *f.cpus,
		MemoryMiB:   *f.memory,
		Runtime:     *f.runtimeID,
		Snapshotter: *f.snapshotter,
		Address:     *f.address,
		Annotations: annotations,
	}, nil
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

// parseLeadingFlags parses only the flags before the first positional argument, and returns
// everything after it untouched.
//
// This is the grammar `boks exec` needs: `boks exec -it web ls -l` must send `-l` to the
// guest, not to our own flag set. A "--" immediately after the sandbox name is accepted and
// dropped, so both `boks exec web -- ls -l` and `boks exec web ls -l` work.
func parseLeadingFlags(fs *flag.FlagSet, args []string) (first string, rest []string, err error) {
	if err := fs.Parse(args); err != nil {
		return "", nil, err
	}
	remaining := fs.Args()
	if len(remaining) == 0 {
		return "", nil, nil
	}
	first = remaining[0]
	rest = remaining[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	return first, rest, nil
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
