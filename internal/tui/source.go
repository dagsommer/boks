// Package tui is the dashboard bare `boks` shows on a terminal: a live view of every
// sandbox, its network log and its network rules, with the actions the CLI has for them.
//
// It is Boks' counterpart of the dashboard Docker Sandboxes' `sbx` shows with no arguments,
// and its layout and keys follow that one: a card per sandbox on the left, the selected
// sandbox's network log and rules on the right, a full-width network view behind `n`.
//
// The package is three layers, kept apart so that the first two can be tested without a
// terminal, a containerd or a policy store:
//
//   - Source (this file) is everything the dashboard reads and does. The real one lives in
//     internal/cli, next to the commands whose code paths it reuses; tests use a fake.
//   - Model (model.go) is the state and the key handling: pure Update, no rendering and no
//     I/O except through Source, which it only ever calls from inside a tea.Cmd.
//   - View (view.go, table.go) renders a Model to a string for a given size.
package tui

import (
	"context"
	"os/exec"
	"time"
)

// Sandbox is one card in the left column: what `boks ls` prints for a sandbox.
type Sandbox struct {
	Name      string
	Agent     string
	Status    string // "running", "stopped", ...
	Workspace string
	// Mode is how the workspace reaches the guest: "direct" or "clone".
	Mode      string
	Ephemeral bool
	// Ports is what the sandbox publishes right now, in `boks ls`'s notation; "" for none.
	Ports string
}

// Running reports whether the sandbox's VM is up.
func (s Sandbox) Running() bool { return s.Status == "running" }

// LogEntry is one row of the network log: the decisions for one destination of one
// sandbox, collapsed, as `boks policy log` collapses them.
type LogEntry struct {
	Sandbox string
	Host    string
	Allowed bool
	Hits    int
	Last    time.Time
	// Rule and Reason are why it was decided as it was, for the status line.
	Rule   string
	Reason string
}

// Rule is one row of the network rules tab.
type Rule struct {
	// Scope is where the rule comes from, in `boks policy ls`'s words: "global",
	// "sandbox NAME", "profile NAME", "preset NAME", "agent NAME", "kit NAME".
	Scope string
	// Sandbox is set for a rule stored in a sandbox's own scope.
	Sandbox  string
	Allow    bool
	Resource string
	Methods  []string
	Path     string
	Query    []string
	Note     string
	// Stored is true for a rule written in the policy store's global or sandbox scope:
	// one `boks policy rm` can remove. Preset, agent and kit rules are not.
	Stored bool
}

// IsHTTP reports whether the rule is an HTTP rule rather than a connection rule.
func (r Rule) IsHTTP() bool { return len(r.Methods) > 0 || r.Path != "" || len(r.Query) > 0 }

// Snapshot is everything the dashboard shows, read in one go.
type Snapshot struct {
	Sandboxes []Sandbox
	Log       []LogEntry
	// Rules maps a sandbox name to the rules that apply to it; the "" key holds the
	// all-sandboxes view: what applies everywhere, and every sandbox's own rules.
	Rules map[string][]Rule
	// Notice is a line worth showing when there is nothing wrong but nothing to show
	// either, like a daemon that is not running.
	Notice string
}

// NewRule is what the add-rule form produces.
type NewRule struct {
	// Sandbox scopes the rule to one sandbox; "" stores it globally.
	Sandbox  string
	Allow    bool
	Resource string
	// Methods and Path make it an HTTP rule; both empty is a connection rule.
	Methods []string
	Path    string
}

// NewSandbox is what the create wizard produces.
type NewSandbox struct {
	Agent     string
	Workspace string
	Name      string // "" lets Boks derive one, as `boks create` does
}

// AgentChoice is one entry in the create wizard's agent list.
type AgentChoice struct {
	Name    string
	Summary string
}

// Source is what the dashboard reads and does. Every method is called from a tea.Cmd, off
// the UI goroutine, and may block.
//
// The mutating methods return what the corresponding `boks` command printed, so the status
// line can show it; an error means the command failed, and its text is what it said.
type Source interface {
	Snapshot(ctx context.Context) (Snapshot, error)
	Agents() []AgentChoice

	Start(ctx context.Context, name string) (string, error)
	Stop(ctx context.Context, name string) (string, error)
	// Remove deletes a sandbox; force is `boks rm --force`, for a running one.
	Remove(ctx context.Context, name string, force bool) (string, error)
	Create(ctx context.Context, s NewSandbox) (string, error)

	AddRule(ctx context.Context, r NewRule) (string, error)
	RemoveRule(ctx context.Context, r Rule) (string, error)

	// Attach and Shell return the command that hands the terminal to the sandbox's
	// agent, or to a shell in it. The dashboard releases the terminal, runs it, and takes
	// the terminal back when it exits.
	Attach(name string) *exec.Cmd
	Shell(name string) *exec.Cmd
}

// Info is static information for the header, footer and settings panel.
type Info struct {
	Version  string
	StateDir string
	Address  string
}
