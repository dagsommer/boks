package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/dagsommer/boks/internal/agent"
	"github.com/dagsommer/boks/internal/runtimecfg"
	"github.com/dagsommer/boks/internal/sandbox"
	"github.com/dagsommer/boks/internal/workspace"
)

// devFlags are the four flags that exist for developing Boks itself: which containerd to
// talk to, which runtime and snapshotter to ask it for, and the opt-out that permits a
// runtime with no VM boundary.
//
// They are registered once, on the root command, and hidden. They are not part of the
// product surface — nobody running an agent in a sandbox should have to know a runtime
// handler exists — and a help page that lists them alongside -t/--template implies they are
// ordinary choices. Hidden is not disabled: they parse everywhere, they are named in the
// root command's help, and the isolation refusal names its opt-out in the error itself,
// which is the only place someone needs to find it.
type devFlags struct {
	runtimeID   string
	snapshotter string
	address     string
	insecure    bool
}

func (d *devFlags) register(fs *pflag.FlagSet) {
	fs.StringVar(&d.runtimeID, "runtime", runtimecfg.Runtime, "containerd runtime handler")
	fs.StringVar(&d.snapshotter, "snapshotter", runtimecfg.Snapshotter, "containerd snapshotter")
	fs.StringVar(&d.address, "containerd-address", runtimecfg.DefaultAddress(), "containerd socket")
	fs.BoolVar(&d.insecure, "i-know-this-is-not-isolated", false,
		"permit a non-VM runtime; for developing Boks itself, never for running untrusted code")
	for _, name := range []string{"runtime", "snapshotter", "containerd-address", "i-know-this-is-not-isolated"} {
		_ = fs.MarkHidden(name)
	}
}

// requireIsolation refuses to present a container-only runtime as a sandbox, unless the
// caller has explicitly said they are developing Boks itself.
func (d *devFlags) requireIsolation(stderr io.Writer) error {
	if runtimecfg.IsolatedRuntime(d.runtimeID) {
		return nil
	}
	if !d.insecure {
		return fmt.Errorf(
			"runtime %q does not provide a virtual machine boundary.\n"+
				"Boks refuses to present it as a sandbox. The isolating runtime is %q.\n"+
				"If you are developing Boks itself and want to exercise the containerd path\n"+
				"without a hypervisor, pass --i-know-this-is-not-isolated.",
			d.runtimeID, runtimecfg.Runtime)
	}
	fmt.Fprintf(stderr,
		"WARNING: running with runtime %q, which shares the host kernel.\n"+
			"         This is NOT an isolation boundary. Do not run untrusted code.\n\n",
		d.runtimeID)
	return nil
}

// sandboxFlags are the flags that describe a sandbox to create. `run` and `create` build the
// same thing and must accept the same options, so they register them from one place.
//
// Flag names follow sbx where sbx has one, including its short forms: someone with the
// habit should not have to learn a second spelling. That is why the image flag is
// `-t, --template` — sbx's word for "the image an agent runs in" — even though `-t` means a
// terminal in almost every other container tool.
type sandboxFlags struct {
	dev         *devFlags
	image       string
	memory      string
	name        string
	cpus        int
	clone       bool
	env         []string
	annotations []string
}

func registerSandboxFlags(fs *pflag.FlagSet, dev *devFlags) *sandboxFlags {
	f := &sandboxFlags{dev: dev}
	fs.StringVarP(&f.image, "template", "t", "",
		"OCI image for the guest root filesystem (default: the agent's image)")
	fs.BoolVar(&f.clone, "clone", false, cloneFlagHelp)
	fs.StringVarP(&f.memory, "memory", "m", "",
		"guest memory, binary units (1024m, 8g) (default: half the host's, max 32g)")
	fs.StringVar(&f.name, "name", "", "sandbox name (default: <agent>-<workspace directory>)")
	fs.IntVar(&f.cpus, "cpus", 0, "vCPUs for the guest (0: all host CPUs)")
	// StringArray, never StringSlice: a value may contain a comma — an -inject rule
	// naming two hosts does — and splitting on it would silently mangle the rule.
	fs.StringArrayVar(&f.env, "env", nil, "extra environment variable KEY=VALUE (repeatable)")
	fs.StringArrayVar(&f.annotations, "annotation", nil,
		"extra OCI annotation KEY=VALUE passed to the runtime (repeatable)")
	return f
}

// splitAtDash separates boks' own positional arguments from the ones meant for the agent.
//
// pflag stops parsing flags at "--" and records how many positionals came before it, so the
// guest's own flags never reach our flag set and nothing has to be split by hand. A second
// "--" is left where it is, in the agent's arguments, because it is the agent's.
func splitAtDash(cmd *cobra.Command, args []string) (positional, agentArgs []string) {
	n := cmd.ArgsLenAtDash()
	if n < 0 {
		return args, nil
	}
	return args[:n], args[n:]
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

// defaultWorkspace is the workspace used when none is given. sbx defaults to the current
// directory, which is where a user standing in a project already is.
const defaultWorkspace = "."

// workspaces resolves the workspace arguments into guest shares. The first argument is the
// primary workspace: it is the process's working directory, and the one the sandbox is named
// after. A `:ro` suffix on any of them makes that share read-only.
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
// `boks run --name existing` all unambiguous without a lookahead into containerd.
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
	if f.name != "" {
		if err := sandbox.ValidateName(f.name); err != nil {
			return invocation{}, err
		}
		inv.name = f.name
		info, exists, err := sandbox.Find(ctx, f.dev.address, inv.name)
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
					"or choose another --name.", inv.name, info.Agent, agentName, inv.name)
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
		choice, err := sandbox.Choose(ctx, f.dev.address, agentName, inv.workspaces[0].HostPath)
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
	cpus := f.cpus
	if cpus == 0 {
		cpus = autoCPUs()
	}

	// An agent's Init prefix names paths that exist in the image Boks ships for it —
	// /usr/bin/tini and the Boks entrypoint. --template points somewhere Boks knows nothing
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
		Runtime:     f.dev.runtimeID,
		Snapshotter: f.dev.snapshotter,
		Address:     f.dev.address,
		Annotations: annotations,
	}, nil
}
