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

// Argv is the command a sandbox running this agent should execute.
func (a Agent) Argv(extra []string) []string {
	if len(extra) == 0 {
		return slices.Clone(a.Command)
	}
	if a.Args == ArgsCommand || len(a.Command) == 0 {
		return slices.Clone(extra)
	}
	return append(slices.Clone(a.Command), extra...)
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

// Builtin returns the agents Boks knows about.
//
// The names are sbx's, so that a habit formed there works here. Only the shell agent has an
// image: the others are placeholders that make `boks run claude` fail with an explanation
// rather than with "unknown agent", and that become real by giving them an image — which is
// also exactly what a user-defined agent will do.
func Builtin() *Registry {
	r := &Registry{}
	for _, a := range []Agent{
		{
			Name:    "shell",
			Summary: "a plain shell in a minimal Linux image",
			// Alpine is small, widely mirrored and has a shell at a known path.
			// It is a starting point for proving the runtime works, not a curated
			// development environment.
			Image:   "docker.io/library/alpine:latest",
			Command: []string{"/bin/sh"},
			Args:    ArgsCommand,
		},
		{Name: "claude", Summary: "Claude Code"},
		{Name: "codex", Summary: "OpenAI Codex"},
		{Name: "copilot", Summary: "GitHub Copilot CLI"},
		{Name: "cursor", Summary: "Cursor CLI"},
		{Name: "docker-agent", Summary: "Docker Agent"},
		{Name: "droid", Summary: "Factory Droid"},
		{Name: "gemini", Summary: "Google Gemini CLI"},
		{Name: "kiro", Summary: "Kiro"},
		{Name: "opencode", Summary: "OpenCode"},
	} {
		if err := r.Add(a); err != nil {
			// A built-in definition is ours, so a bad one is a programming error.
			panic("agent: invalid built-in definition: " + err.Error())
		}
	}
	return r
}
