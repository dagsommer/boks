package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dagsommer/boks/internal/agent"
	"github.com/dagsommer/boks/internal/runtimecfg"
	"github.com/dagsommer/boks/internal/sandbox"
	"github.com/dagsommer/boks/internal/workspace"
)

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// sandboxFlags are the flags that describe a sandbox to create. `run` and `create` build the
// same thing and must accept the same options, so they register them from one place.
//
// Flag names follow sbx where sbx has one, including its short aliases: someone with the
// habit should not have to learn a second spelling. That is why the image flag is
// `-t/-template` — sbx's word for "the image an agent runs in" — even though `-t` means a
// terminal in almost every other container tool.
type sandboxFlags struct {
	image       string
	memory      string
	name        *string
	cpus        *int
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
		name:        fs.String("name", "", "sandbox name (default: <agent>-<workspace directory>)"),
		cpus:        fs.Int("cpus", 0, "vCPUs for the guest (0: all host CPUs)"),
		runtimeID:   fs.String("runtime", runtimecfg.Runtime, "containerd runtime handler"),
		snapshotter: fs.String("snapshotter", runtimecfg.Snapshotter, "containerd snapshotter"),
		address:     addressFlag(fs),
		insecure: fs.Bool("i-know-this-is-not-isolated", false,
			"permit a non-VM runtime; for developing Boks itself, never for running untrusted code"),
	}
	fs.StringVar(&f.image, "template", "", "OCI image for the guest root filesystem (default: the agent's image)")
	fs.StringVar(&f.image, "t", "", "alias for -template")
	fs.StringVar(&f.memory, "memory", "", "guest memory, binary units (1024m, 8g) (default: half the host's, max 32g)")
	fs.StringVar(&f.memory, "m", "", "alias for -memory")
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

// defaultWorkspace is the workspace used when none is given. sbx defaults to the current
// directory, which is where a user standing in a project already is.
const defaultWorkspace = "."

// workspaces resolves the workspace arguments plus any -mount into guest shares. The first
// argument is the primary workspace: it is the process's working directory, and the one the
// sandbox is named after.
func (f *sandboxFlags) workspaces(args []string, agents *agent.Registry) ([]workspace.Workspace, error) {
	if len(args) == 0 {
		args = []string{defaultWorkspace}
	}
	var workspaces []workspace.Workspace
	for i, arg := range args {
		ws, err := workspace.Parse(arg)
		if err != nil {
			if i == 0 {
				return nil, describeWorkspaceError(arg, agents, err)
			}
			return nil, err
		}
		workspaces = append(workspaces, ws)
	}
	for _, m := range f.mounts {
		ws, err := workspace.Parse(m)
		if err != nil {
			return nil, err
		}
		workspaces = append(workspaces, ws)
	}
	return workspaces, nil
}

// describeWorkspaceError adds the one piece of context a first positional argument can be
// wrong about now that the agent comes first: a mistyped agent name is read as a directory,
// and "no such directory" alone would not say why.
func describeWorkspaceError(arg string, agents *agent.Registry, err error) error {
	if strings.ContainsAny(arg, `/\.`) {
		return err
	}
	return fmt.Errorf("%w\nIf %q was meant as an agent, the agents are: %s",
		err, arg, strings.Join(agents.Names(), ", "))
}

// invocation is what `run` and `create` work out from their arguments: which agent, which
// sandbox, and which host directories go into it.
type invocation struct {
	agent      agent.Agent
	name       string
	exists     bool
	info       sandbox.Info
	workspaces []workspace.Workspace
}

// splitAgent separates the agent positional from the workspace positionals.
//
// The first positional is the agent exactly when it names one. Anything else is a workspace
// and the agent is decided elsewhere — from the named sandbox, or the default. This keeps
// `boks run`, `boks run .`, `boks run shell`, `boks run shell . ~/lib` and
// `boks run -name existing` all unambiguous without a lookahead into containerd.
func splitAgent(agents *agent.Registry, positional []string) (name string, workspaceArgs []string) {
	if len(positional) > 0 && agents.Known(positional[0]) {
		return positional[0], positional[1:]
	}
	return "", positional
}

// resolve works out the sandbox an invocation is about.
//
// Naming and re-attach are one mechanism: the name is derived from the agent and the
// workspace, so running again in the same directory with the same agent finds the same
// sandbox. Everything this function adds to sandbox.ChooseName is the part that needs to
// know what already exists — reading the agent back from a named sandbox, and stepping
// around a readable name another directory got to first.
func (f *sandboxFlags) resolve(ctx context.Context, agents *agent.Registry, positional []string, env Env) (invocation, error) {
	agentName, workspaceArgs := splitAgent(agents, positional)
	if agentName != "" {
		// Fail on an unknown agent before touching containerd, so a typo does not
		// look like a daemon problem.
		if _, err := agents.Resolve(agentName); err != nil {
			return invocation{}, err
		}
	}

	var inv invocation
	if *f.name != "" {
		if err := sandbox.ValidateName(*f.name); err != nil {
			return invocation{}, err
		}
		inv.name = *f.name
		info, exists, err := sandbox.Find(ctx, *f.address, inv.name)
		if err != nil {
			return invocation{}, err
		}
		inv.exists, inv.info = exists, info

		switch {
		case agentName == "" && exists:
			// The agent positional is optional for a sandbox that exists: it
			// already knows what it runs.
			agentName = info.Agent
		case agentName != "" && exists && info.Agent != "" && info.Agent != agentName:
			return invocation{}, fmt.Errorf(
				"sandbox %q runs the %q agent, not %q.\n"+
					"An agent is fixed when a sandbox is created; remove it with 'boks rm %s'\n"+
					"or choose another -name.", inv.name, info.Agent, agentName, inv.name)
		}
	}
	if agentName == "" {
		agentName = agent.Default
	}

	// A sandbox that already exists has its workspaces fixed, so an invocation that
	// named none is not asking for the current directory — it is asking for that
	// sandbox, from wherever the user happens to be standing.
	if !(inv.exists && len(workspaceArgs) == 0) {
		workspaces, err := f.workspaces(workspaceArgs, agents)
		if err != nil {
			return invocation{}, err
		}
		inv.workspaces = workspaces
	}

	resolved, err := agents.Resolve(agentName)
	if err != nil {
		return invocation{}, err
	}
	inv.agent = resolved

	if inv.name == "" {
		choice, err := sandbox.Choose(ctx, *f.address, agentName, inv.workspaces[0].HostPath)
		if err != nil {
			return invocation{}, err
		}
		if choice.CollidedWith != "" {
			fmt.Fprintf(env.Stderr,
				"note: the name %s-%s belongs to a sandbox for %s, so this one is %s.\n",
				agentName, filepath.Base(inv.workspaces[0].HostPath), choice.CollidedWith, choice.Name)
		}
		inv.name, inv.exists, inv.info = choice.Name, choice.Exists, choice.Info
	}

	// The image is only needed to create a sandbox. An agent Boks has no image for can
	// still be re-attached to, and can be created with an image the user supplies.
	if !inv.exists && f.image == "" {
		if err := agent.RequireRunnable(inv.agent); err != nil {
			return invocation{}, err
		}
	}
	return inv, nil
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

// config turns the flags and the resolved invocation into a sandbox definition. The agent
// supplies what the user did not: the image, the command, and the environment it needs.
func (f *sandboxFlags) config(inv invocation, agentArgs []string) (sandbox.Config, error) {
	annotations, err := parseKeyValues(f.annotations)
	if err != nil {
		return sandbox.Config{}, err
	}

	memoryMiB := autoMemoryMiB()
	if f.memory != "" {
		if memoryMiB, err = parseMemory(f.memory); err != nil {
			return sandbox.Config{}, err
		}
	}
	cpus := *f.cpus
	if cpus == 0 {
		cpus = autoCPUs()
	}

	// An agent's Init prefix names paths that exist in the image Boks ships for it —
	// /usr/bin/tini and the Boks entrypoint. -template points somewhere Boks knows nothing
	// about, so the prefix has to come off with the image it belongs to; otherwise
	// `boks run shell -t debian:stable -- uname -a` fails on a missing tini.
	run := inv.agent
	image := f.image
	if image == "" {
		image = run.Image
	} else if image != run.Image {
		run = run.Bare()
	}

	// A sandbox that already exists recorded what it runs when it was created, agent
	// arguments included. An invocation that gives none of its own is asking for that,
	// not for the agent's bare command line — otherwise `boks create shell . -- npm run
	// dev` followed by `boks run shell .` would quietly drop the interesting half.
	command := run.Argv(agentArgs)
	if inv.exists && len(agentArgs) == 0 {
		command = nil
	}
	return sandbox.Config{
		Name:        inv.name,
		Agent:       inv.agent.Name,
		Image:       image,
		Command:     command,
		Workspaces:  inv.workspaces,
		Env:         append(slices.Clone(inv.agent.Env), f.env...),
		CPUs:        cpus,
		MemoryMiB:   memoryMiB,
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
