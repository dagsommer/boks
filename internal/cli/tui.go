package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"

	"github.com/dagsommer/boks/internal/agent"
	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/sandbox"
	"github.com/dagsommer/boks/internal/tui"
)

const tuiLong = `Shows a live view of every sandbox: a card per sandbox with its agent, workspace and
state, and for the selected one its network log and network rules. From it you can attach
to a sandbox's agent, open a shell in it, start, stop and remove it, create a new one, and
add or remove network rules. It is what bare 'boks' shows when it runs in a terminal.

Every action runs the boks command that does the same thing — 'boks run --name', 'boks exec',
'boks start', 'boks stop', 'boks rm', 'boks create', 'boks policy allow|deny|rm' — so it
behaves exactly as that command does. Attaching and opening a shell hand the terminal over
and the dashboard comes back when the session ends. Removing a sandbox or a rule asks first.

Keys (the bottom line shows the ones that apply where you are):
  ↑/↓            select a sandbox, or a row in the network tables
  →, Tab         move into the network pane; ←/→ there switch tabs, Tab/Esc go back
  Enter          attach to the selected sandbox's agent (boks run --name NAME)
  s / x / r      start or stop, open a shell (exec), remove — on the selected card
  c              create a sandbox
  n              the network view for every sandbox; s there picks a sandbox, Esc returns
  a / r          add or remove a network rule, on the Network Rules tab
  Enter          on a network log row: block an allowed host, or allow a blocked one
  o, /           filter by status, search
  Ctrl+s         settings and paths
  q, Ctrl+c      quit

The view refreshes about every second and a half. NO_COLOR turns colour off.`

func newTUICommand(env Env, dev *devFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "tui",
		Aliases: []string{"dashboard"},
		Short:   "Show the interactive dashboard (what bare 'boks' shows on a terminal)",
		Long:    tuiLong,
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !dashboardWanted(env) {
				return errors.New("the dashboard needs a terminal on stdin and stdout; 'boks ls' and 'boks policy log' print the same information")
			}
			return runDashboard(cmd.Context(), env, dev)
		},
	}
}

func runDashboard(ctx context.Context, env Env, dev *devFlags) error {
	src, err := newDashSource(dev)
	if err != nil {
		return err
	}
	info := tui.Info{Version: Version, StateDir: policy.StateDir(), Address: dev.address}
	return tui.Run(ctx, src, info, env.Stdin, env.Stdout, tui.WithNoColor(os.Getenv("NO_COLOR") != ""))
}

// dashSource is the dashboard's view of Boks. It reads through the same functions `boks ls`,
// `boks policy ls` and `boks policy log` read through, and acts by running the boks command
// for the action, so that nothing the dashboard does can drift from what the CLI does.
type dashSource struct {
	dev     *devFlags
	exe     string
	devArgs []string

	mu       sync.Mutex
	logStamp fileStamp
	logRows  []tui.LogEntry
}

type fileStamp struct {
	size int64
	mod  time.Time
}

func newDashSource(dev *devFlags) (*dashSource, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("finding the boks executable to run actions with: %w", err)
	}
	return &dashSource{dev: dev, exe: exe, devArgs: devFlagArgs(dev)}, nil
}

// devFlagArgs repeats the hidden development flags the dashboard was started with, so an
// action talks to the same containerd with the same runtime as the dashboard does.
func devFlagArgs(dev *devFlags) []string {
	var out []string
	if dev.fs == nil {
		return nil
	}
	dev.fs.Visit(func(f *pflag.Flag) {
		switch f.Name {
		case "runtime", "snapshotter", "containerd-address", "i-know-this-is-not-isolated":
			out = append(out, "--"+f.Name+"="+f.Value.String())
		}
	})
	return out
}

// logTail is how many decisions the dashboard reads back from the log. The table collapses
// them per destination, so this bounds the work of a refresh on a log that has grown for
// months, not what can be shown.
const logTail = 20000

func (s *dashSource) Snapshot(ctx context.Context) (tui.Snapshot, error) {
	var snap tui.Snapshot
	stateDir := policy.StateDir()

	var infos []sandbox.Info
	if !s.dev.addressNamed() && !daemonRunning(stateDir) && !daemonServing(ctx, s.dev.address) {
		snap.Notice = "The Boks daemon is not running. Creating, starting or running a sandbox starts it."
	} else {
		var err error
		if infos, err = sandbox.List(ctx, s.dev.address); err != nil {
			return snap, err
		}
	}
	published := publishedPorts()
	for _, info := range infos {
		snap.Sandboxes = append(snap.Sandboxes, tui.Sandbox{
			Name: info.Name, Agent: info.Agent, Status: info.Status, Workspace: info.Workspace(),
			Mode: info.Filesystem.Mode, Ephemeral: info.Ephemeral, Ports: published[info.Name],
		})
	}

	snap.Log = s.readLog()

	rules, err := dashRules(infos)
	if err != nil {
		return snap, err
	}
	snap.Rules = rules
	return snap, nil
}

// readLog reads the decision log, or returns what it read last time if the file has not
// changed since: a refresh with no new traffic costs one stat.
func (s *dashSource) readLog() []tui.LogEntry {
	path := policy.DefaultLogPath()
	s.mu.Lock()
	defer s.mu.Unlock()
	fi, err := os.Stat(path)
	if err != nil {
		s.logStamp, s.logRows = fileStamp{}, nil
		return nil
	}
	stamp := fileStamp{fi.Size(), fi.ModTime()}
	if stamp == s.logStamp {
		return s.logRows
	}
	f, err := os.Open(path)
	if err != nil {
		return s.logRows
	}
	defer f.Close()
	decisions, err := policy.ReadDecisions(f, logTail)
	if err != nil {
		return s.logRows
	}
	s.logStamp, s.logRows = stamp, logEntries(decisions)
	return s.logRows
}

// logEntries collapses decisions to one row per sandbox, host and outcome, newest first:
// the rows Docker Sandboxes' dashboard shows. `boks policy log` keeps the port, the proxy
// mode and the reason apart as well; the dashboard keeps the latest reason for the status
// line and leaves the detail to that command.
func logEntries(decisions []policy.Decision) []tui.LogEntry {
	type key struct {
		sandbox, host string
		allowed       bool
	}
	index := map[key]int{}
	var out []tui.LogEntry
	for _, d := range decisions {
		k := key{d.Sandbox, d.Host, d.Allowed}
		if i, ok := index[k]; ok {
			out[i].Hits++
			if !d.Time.Before(out[i].Last) {
				out[i].Last, out[i].Rule, out[i].Reason = d.Time, d.Rule, d.Reason
			}
			continue
		}
		index[k] = len(out)
		out = append(out, tui.LogEntry{Sandbox: d.Sandbox, Host: d.Host, Allowed: d.Allowed, Hits: 1,
			Last: d.Time, Rule: d.Rule, Reason: d.Reason})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		return out[i].Host < out[j].Host
	})
	return out
}

// dashRules resolves the rules each sandbox runs under, as `boks policy ls --sandbox NAME
// --agent AGENT` resolves them, and the all-sandboxes view: what applies everywhere, then
// every rule stored for one sandbox or one profile.
func dashRules(infos []sandbox.Info) (map[string][]tui.Rule, error) {
	store, err := openPolicyStore()
	if err != nil {
		return nil, err
	}
	agents := agent.Builtin()
	out := map[string][]tui.Rule{}
	for _, info := range infos {
		req := info.Policy.Request(store, info.Name)
		if a, ok := agents.Lookup(info.Agent); ok {
			req.Agent, req.AgentAllow = a.Name, a.Rules()
		}
		res, err := req.Resolve()
		if err != nil {
			// One sandbox whose recorded policy no longer resolves — a deleted profile —
			// should not blank the dashboard; its tab says what is wrong instead.
			out[info.Name] = []tui.Rule{{Scope: "error", Resource: err.Error()}}
			continue
		}
		out[info.Name] = tuiRules(res.Rules)
	}

	res, err := (policy.Request{Store: store}).Resolve()
	if err != nil {
		return nil, err
	}
	all := tuiRules(res.Rules)
	for _, name := range store.SandboxNames() {
		rules, _ := store.Rules(policy.SandboxScope(name))
		for _, r := range rules {
			r.Scope = "sandbox " + name
			all = append(all, tuiRule(r))
		}
	}
	for _, name := range store.ProfileNames() {
		for _, r := range store.Profiles[name].Rules {
			r.Scope = "profile " + name
			all = append(all, tuiRule(r))
		}
	}
	out[""] = all
	return out, nil
}

func tuiRules(specs []policy.RuleSpec) []tui.Rule {
	out := make([]tui.Rule, 0, len(specs))
	for _, r := range specs {
		out = append(out, tuiRule(r))
	}
	return out
}

func tuiRule(r policy.RuleSpec) tui.Rule {
	row := ruleRow(r.Scope, r)
	t := tui.Rule{
		Scope: r.Scope, Allow: r.Action == policy.Allow, Resource: row.Resource,
		Methods: row.Methods, Path: row.Path, Query: row.Query, Note: row.Note,
	}
	switch {
	case r.Scope == "global":
		t.Stored = true
	case strings.HasPrefix(r.Scope, "sandbox "):
		t.Stored, t.Sandbox = true, strings.TrimPrefix(r.Scope, "sandbox ")
	}
	return t
}

func (s *dashSource) Agents() []tui.AgentChoice {
	var out []tui.AgentChoice
	for _, a := range agent.Builtin().All() {
		out = append(out, tui.AgentChoice{Name: a.Name, Summary: a.Summary})
	}
	return out
}

// boks runs a boks command and returns what it printed. An error carries the command's
// last line of output, which is where every boks command puts its error.
func (s *dashSource) boks(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, s.exe, append(append([]string{}, s.devArgs...), args...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.Stdin = nil
	err := cmd.Run()
	text := out.String()
	if err != nil {
		if line := lastOutputLine(text); line != "" {
			return text, errors.New(strings.TrimPrefix(line, "boks: "))
		}
		return text, fmt.Errorf("boks %s: %w", strings.Join(args, " "), err)
	}
	return text, nil
}

func lastOutputLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func (s *dashSource) Start(ctx context.Context, name string) (string, error) {
	out, err := s.boks(ctx, "start", name)
	if err == nil {
		out = "started " + name
	}
	return out, err
}

func (s *dashSource) Stop(ctx context.Context, name string) (string, error) {
	out, err := s.boks(ctx, "stop", name)
	if err == nil {
		out = "stopped " + name
	}
	return out, err
}

func (s *dashSource) Remove(ctx context.Context, name string, force bool) (string, error) {
	args := []string{"rm"}
	if force {
		args = append(args, "--force")
	}
	out, err := s.boks(ctx, append(args, name)...)
	if err == nil {
		out = "removed " + name
	}
	return out, err
}

func (s *dashSource) Create(ctx context.Context, ns tui.NewSandbox) (string, error) {
	args := []string{"create"}
	if ns.Name != "" {
		args = append(args, "--name", ns.Name)
	}
	args = append(args, ns.Agent, ns.Workspace)
	out, err := s.boks(ctx, args...)
	if err == nil {
		out = "created " + lastOutputLine(out)
	}
	return out, err
}

func (s *dashSource) AddRule(ctx context.Context, r tui.NewRule) (string, error) {
	verb := "allow"
	if !r.Allow {
		verb = "deny"
	}
	args := []string{"policy", verb}
	if r.Sandbox != "" {
		args = append(args, "--sandbox", r.Sandbox)
	}
	if len(r.Methods) > 0 {
		args = append(args, "--method", strings.Join(r.Methods, ","))
	}
	if r.Path != "" {
		args = append(args, "--path", r.Path)
	}
	out, err := s.boks(ctx, append(args, "--", r.Resource)...)
	if err == nil {
		out = firstMatching(out, "added:", "already stored:")
	}
	return out, err
}

func (s *dashSource) RemoveRule(ctx context.Context, r tui.Rule) (string, error) {
	action := "allow"
	if !r.Allow {
		action = "deny"
	}
	args := []string{"policy", "rm", "--action", action}
	if r.Sandbox != "" {
		args = append(args, "--sandbox", r.Sandbox)
	}
	if r.IsHTTP() {
		if len(r.Methods) > 0 {
			args = append(args, "--method", strings.Join(r.Methods, ","))
		}
		if r.Path != "" {
			args = append(args, "--path", r.Path)
		}
		for _, q := range r.Query {
			args = append(args, "--query", q)
		}
	}
	out, err := s.boks(ctx, append(args, "--", r.Resource)...)
	if err == nil {
		out = firstMatching(out, "removed:")
		if out == "" {
			out = "nothing was removed"
		}
	}
	return out, err
}

// firstMatching returns the first line of out that starts with one of the prefixes, or the
// last line if none does.
func firstMatching(out string, prefixes ...string) string {
	for _, line := range strings.Split(out, "\n") {
		for _, p := range prefixes {
			if strings.HasPrefix(line, p) {
				return line
			}
		}
	}
	return lastOutputLine(out)
}

func (s *dashSource) interactive(args ...string) *exec.Cmd {
	return exec.Command(s.exe, append(append([]string{}, s.devArgs...), args...)...)
}

// Attach is `boks run --name NAME`: the sandbox's own agent, started first if it is stopped.
func (s *dashSource) Attach(name string) *exec.Cmd {
	return s.interactive("run", "--name", name)
}

// shellScript prefers a login bash and falls back to sh, so the shell works in every image.
const shellScript = `if command -v bash >/dev/null 2>&1; then exec bash -l; else exec sh -l; fi`

// Shell is `boks exec -it NAME`, running a login shell.
func (s *dashSource) Shell(name string) *exec.Cmd {
	return s.interactive("exec", "-it", name, "sh", "-c", shellScript)
}

var _ tui.Source = (*dashSource)(nil)

// dashboardWanted reports whether bare `boks` should show the dashboard rather than its
// usage: only when both ends are a terminal, so a script, a pipe or a test still gets help.
//
// It asks the terminal driver rather than checking for a character device, as isTerminal
// does, because /dev/null is one too: `boks </dev/null >/dev/null` must not start a UI.
func dashboardWanted(env Env) bool {
	return isTTY(env.Stdin) && isTTY(env.Stdout)
}

func isTTY(stream any) bool {
	f, ok := stream.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
