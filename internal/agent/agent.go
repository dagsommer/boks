// Package agent maps an agent name to the environment it runs in.
//
// sbx is agent-first: `sbx run claude` names a prepared environment, not a command. Boks
// follows that shape, because a sandbox is nearly always created to run one of a handful of
// coding agents, and the name is the only thing the user should have to know.
//
// An agent is data — a name, an image, a startup command — never a branch in the CLI. A
// live `sbx ls` shows user-defined agents (`udi-copilot-default`, `udi-copilot-yolo`)
// alongside the built-in ones, so custom agents are a real capability rather than a fixed
// menu. Registry.Add is the seam that capability will arrive through: a loader for a
// declarative agent definition has only to call it, and nothing above this package changes.
// That loader and its file format are deliberately not designed yet — see the kit rows in
// docs/docker-sandbox-parity.md.
package agent

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Default is the agent used when none is named. A shell is the one agent Boks can supply
// entirely from a public base image, and it is what makes the agent grammar cover the plain
// "run a command in a sandbox" case rather than sitting beside it.
const Default = "shell"

// ArgsMode says what arguments after `--` mean for an agent.
type ArgsMode string

const (
	// ArgsAppend adds them to the agent's own command line, which is sbx's model:
	// `boks run claude -- --continue` starts the agent with that flag.
	ArgsAppend ArgsMode = "append"
	// ArgsCommand makes them the command instead. The shell agent works this way: to
	// someone typing `boks run shell . -- uname -a`, "arguments to a shell" and "a
	// command to run in the sandbox" are the same thing, and appending them to `/bin/sh`
	// would run a script named `uname`.
	ArgsCommand ArgsMode = "command"
)

// Agent is one runnable environment.
type Agent struct {
	// Name is how the user asks for it, and the first half of the derived sandbox name.
	Name string
	// Summary is one line for help output.
	Summary string
	// Image is the OCI reference providing the guest root filesystem. An empty Image
	// means Boks knows the agent's name but has no environment for it yet; such an agent
	// is still usable with an explicit -template.
	Image string
	// Init is a prefix put in front of everything else in Argv — an init and whatever
	// preparation the image needs before the agent runs.
	//
	// It exists because a sandbox does not use the image's ENTRYPOINT: the OCI spec is
	// built with containerd's WithProcessArgs, which replaces the whole argv. An image
	// that installs the Boks CA on the way in would therefore be bypassed. Carrying the
	// prefix in the definition puts it back on every path that goes through Argv,
	// including `boks run shell . -- cmd`, where the user supplies the command.
	Init []string
	// Command is the argv the sandbox starts with. Empty means the image's own default.
	Command []string
	// Args says how arguments after `--` combine with Command.
	Args ArgsMode
	// Env are environment variables the agent needs, in KEY=VALUE form. Nothing is
	// inherited from the host: an agent gets exactly what its definition asks for.
	Env []string
}

// Runnable reports whether Boks can start this agent without being told an image.
func (a Agent) Runnable() bool { return a.Image != "" }

// Bare returns the agent without its Init prefix, for running it somewhere other than the
// image Boks ships for it. The prefix names paths that only a Boks image has, so an image
// supplied with -template must not inherit it.
func (a Agent) Bare() Agent {
	a.Init = nil
	return a
}

// Argv is the command a sandbox running this agent should execute.
func (a Agent) Argv(extra []string) []string {
	argv := slices.Clone(a.Init)
	if len(extra) == 0 {
		return append(argv, a.Command...)
	}
	if a.Args == ArgsCommand || len(a.Command) == 0 {
		return append(argv, extra...)
	}
	return append(append(argv, a.Command...), extra...)
}

// Registry is an ordered set of agents. Order is preserved so that help and listings are
// stable, and so a user-defined agent appears where it was added rather than wherever a map
// iteration puts it.
type Registry struct {
	agents []Agent
}

// nameRe is the grammar an agent name has to satisfy. It is containerd's identifier grammar,
// because the agent name becomes the first segment of the sandbox name — see
// sandbox.DeriveName. Catching it here means a bad definition fails when it is registered
// rather than when someone tries to run it.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9]+(?:[._-][A-Za-z0-9]+)*$`)

// Add registers an agent, replacing any existing one with the same name.
//
// Replacement rather than rejection is deliberate: it is how a user-defined agent will be
// able to override a built-in one, which is the point of having user-defined agents at all.
func (r *Registry) Add(a Agent) error {
	if !nameRe.MatchString(a.Name) {
		return fmt.Errorf("agent name %q is invalid: use letters and digits, separated by '.', '_' or '-'", a.Name)
	}
	if len(a.Name) > maxAgentNameLength {
		return fmt.Errorf("agent name %q is longer than %d characters", a.Name, maxAgentNameLength)
	}
	if a.Args == "" {
		a.Args = ArgsAppend
	}
	for i, existing := range r.agents {
		if existing.Name == a.Name {
			r.agents[i] = a
			return nil
		}
	}
	r.agents = append(r.agents, a)
	return nil
}

// maxAgentNameLength keeps room in a containerd identifier (76 characters) for the workspace
// half of a derived sandbox name. A name longer than this is a mistake, not a preference.
const maxAgentNameLength = 32

// Lookup returns the named agent.
func (r *Registry) Lookup(name string) (Agent, bool) {
	for _, a := range r.agents {
		if a.Name == name {
			return a, true
		}
	}
	return Agent{}, false
}

// Known reports whether a positional argument names an agent.
//
// This is what makes `boks run <agent> [workspace...]` unambiguous without a lookahead
// table: the first positional is the agent exactly when it is a name the registry knows,
// and otherwise it is the first workspace. A directory that happens to share a name with an
// agent is reachable as `./shell`, which is not a registered name.
func (r *Registry) Known(name string) bool {
	_, ok := r.Lookup(name)
	return ok
}

// All returns the registered agents in registration order.
func (r *Registry) All() []Agent { return slices.Clone(r.agents) }

// Names returns the registered agent names, for help text and error messages.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.agents))
	for _, a := range r.agents {
		names = append(names, a.Name)
	}
	return names
}

// Resolve returns the named agent, or an error naming what the user can actually run.
func (r *Registry) Resolve(name string) (Agent, error) {
	a, ok := r.Lookup(name)
	if !ok {
		return Agent{}, fmt.Errorf("unknown agent %q.\nAgents: %s.\n"+
			"The agent comes first: 'boks run %s [workspace...]'. To run a command in a\n"+
			"plain sandbox, use the shell agent: 'boks run shell . -- %s'",
			name, strings.Join(r.Names(), ", "), Default, name)
	}
	return a, nil
}

// RequireRunnable reports why an agent cannot be started as it stands.
//
// The named agents Boks cannot supply are registered anyway, so that asking for one gives
// this answer instead of "unknown agent" — the name is right, the environment is missing.
func RequireRunnable(a Agent) error {
	if a.Runnable() {
		return nil
	}
	return fmt.Errorf("agent %q has no image yet: Boks knows the name but does not ship an\n"+
		"environment for it. Point it at one with -template, for example\n"+
		"  boks run %s -template ghcr.io/example/%s:latest\n"+
		"or use the %s agent, which needs nothing installed.",
		a.Name, a.Name, a.Name, Default)
}

// ImageRepo is where the Boks agent images are published. The images are built from
// images/<name>/Dockerfile in this repository; see images/README.md.
const ImageRepo = "ghcr.io/dagsommer/boks"

// ImageTag is the tag every Boks agent image is published under.
//
// One constant, moved by a release, rather than a version spelled ten times: the images are
// built and pushed together from a single workflow, so a tag that differed between them
// could only ever be a mistake. It is exported because the build tooling and the release
// workflow need the same value, and deriving it from one place is what keeps them in step.
const ImageTag = "0.1.0"

// Image returns the published reference for one of the images in this repository. The name
// is the agent's, except for the shell agent, which runs the base image itself.
func Image(name string) string { return ImageRepo + "/" + name + ":" + ImageTag }

// initArgv is what every Boks agent image expects in front of the agent's own command.
//
// tini is there to be PID 1: a sandbox that lives for hours accumulates zombies without one,
// and a real Docker Sandboxes guest was observed running tini as PID 1 for the same reason.
// boks-entrypoint installs the Boks CA — see internal/ca — when BOKS_CA_CERT_B64 is in the
// environment, and execs straight through when it is not.
//
// This is a property of the images in images/, so an agent pointed at some other image with
// -template gets whatever that image does instead.
var initArgv = []string{"/usr/bin/tini", "--", "/usr/local/bin/boks-entrypoint"}

// Builtin returns the agents Boks knows about.
//
// The names are sbx's, so that a habit formed there works here. Nine of the ten have an
// image; `kiro` does not, and is registered anyway so that asking for it says "no image yet"
// rather than "unknown agent" — which is also the shape a user-defined agent overrides.
func Builtin() *Registry {
	r := &Registry{}
	for _, a := range []Agent{
		{
			Name:    "shell",
			Summary: "a plain shell in the Boks base image",
			// The shell agent is the base image itself: it is the one agent whose
			// environment is "everything the others share and nothing more".
			Image:   Image("base"),
			Command: []string{"/bin/bash"},
			Args:    ArgsCommand,
		},
		{Name: "claude", Summary: "Claude Code", Image: Image("claude"), Command: []string{"claude"}},
		{Name: "codex", Summary: "OpenAI Codex", Image: Image("codex"), Command: []string{"codex"}},
		{Name: "copilot", Summary: "GitHub Copilot CLI", Image: Image("copilot"), Command: []string{"copilot"}},
		{Name: "cursor", Summary: "Cursor CLI", Image: Image("cursor"), Command: []string{"cursor-agent"}},
		{Name: "docker-agent", Summary: "Docker Agent", Image: Image("docker-agent"), Command: []string{"docker-agent"}},
		{Name: "droid", Summary: "Factory Droid", Image: Image("droid"), Command: []string{"droid"}},
		{Name: "gemini", Summary: "Google Gemini CLI", Image: Image("gemini"), Command: []string{"gemini"}},
		// Kiro is the one name here Boks ships nothing for. Its CLI is distributed as a
		// ~500 MB archive per architecture, which would roughly triple the size of an
		// agent image, and its installer resolves the download through a "latest"
		// manifest with no documented version-pinned URL — so there is no artifact to
		// pin and checksum the way every other image here does. Both would have to
		// change before this becomes an image.
		{Name: "kiro", Summary: "Kiro"},
		{Name: "opencode", Summary: "OpenCode", Image: Image("opencode"), Command: []string{"opencode"}},
	} {
		// Every image Boks ships carries the same init and entrypoint, so the prefix is
		// applied here rather than repeated in ten definitions. An agent with no image
		// gets none: it will run in an image Boks knows nothing about.
		if a.Image != "" {
			a.Init = initArgv
		}
		if err := r.Add(a); err != nil {
			// A built-in definition is ours, so a bad one is a programming error.
			panic("agent: invalid built-in definition: " + err.Error())
		}
	}
	return r
}
