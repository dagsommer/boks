package cli

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/agent"
	"github.com/dagsommer/boks/internal/sandbox"
)

func TestParseLeadingFlags(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantFirst   string
		wantRest    []string
		wantTTY     bool
		wantCombine bool
	}{
		{"name and command", []string{"web", "ls"}, "web", []string{"ls"}, false, false},
		{
			// Guest flags must reach the guest, not our flag set.
			"guest flags", []string{"-t", "web", "ls", "-l", "-t"},
			"web", []string{"ls", "-l", "-t"}, true, false,
		},
		{"combined shorthand", []string{"-it", "web", "sh"}, "web", []string{"sh"}, false, true},
		{"explicit separator", []string{"web", "--", "git", "status"}, "web", []string{"git", "status"}, false, false},
		{"name only", []string{"web"}, "web", nil, false, false},
		{"nothing", nil, "", nil, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			tty := fs.Bool("t", false, "")
			both := fs.Bool("it", false, "")

			first, rest, err := parseLeadingFlags(fs, tt.args)
			if err != nil {
				t.Fatalf("parseLeadingFlags(%v): %v", tt.args, err)
			}
			if first != tt.wantFirst {
				t.Errorf("first = %q, want %q", first, tt.wantFirst)
			}
			if !slices.Equal(rest, tt.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tt.wantRest)
			}
			if *tty != tt.wantTTY {
				t.Errorf("-t = %v, want %v", *tty, tt.wantTTY)
			}
			if *both != tt.wantCombine {
				t.Errorf("-it = %v, want %v", *both, tt.wantCombine)
			}
		})
	}
}

// The rule that makes `boks run [agent] [workspace...]` parseable without asking containerd
// anything: the first positional is the agent exactly when it names one.
func TestSplitAgent(t *testing.T) {
	agents := agent.Builtin()
	tests := []struct {
		name           string
		positional     []string
		wantAgent      string
		wantWorkspaces []string
	}{
		{"nothing", nil, "", nil},
		{"agent only", []string{"shell"}, "shell", []string{}},
		{"agent and workspace", []string{"shell", "."}, "shell", []string{"."}},
		{"agent and several workspaces", []string{"claude", ".", "~/lib:ro"}, "claude", []string{".", "~/lib:ro"}},
		{"workspace only", []string{"."}, "", []string{"."}},
		{"an agent name is not read as a workspace", []string{"./shell"}, "", []string{"./shell"}},
		{"unknown first positional is a workspace", []string{"cladue"}, "", []string{"cladue"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotAgent, gotWorkspaces := splitAgent(agents, tt.positional)
			if gotAgent != tt.wantAgent {
				t.Errorf("agent = %q, want %q", gotAgent, tt.wantAgent)
			}
			if !slices.Equal(gotWorkspaces, tt.wantWorkspaces) {
				t.Errorf("workspaces = %v, want %v", gotWorkspaces, tt.wantWorkspaces)
			}
		})
	}
}

// With no workspace argument the sandbox is for the current directory, which is where the
// user already is.
func TestWorkspacesDefaultToTheCurrentDirectory(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()

	workspaces, err := (&sandboxFlags{}).workspaces(nil, agent.Builtin())
	if err != nil {
		t.Fatalf("workspaces: %v", err)
	}
	if len(workspaces) != 1 {
		t.Fatalf("got %d workspaces, want 1", len(workspaces))
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if workspaces[0].HostPath != resolved {
		t.Errorf("workspace = %q, want the current directory %q", workspaces[0].HostPath, resolved)
	}
}

// Extra workspaces mount alongside the first, and the :ro suffix keeps working.
func TestWorkspacesTakeSeveralPathsAndModes(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	flags := &sandboxFlags{}
	workspaces, err := flags.workspaces([]string{first, second + ":ro"}, agent.Builtin())
	if err != nil {
		t.Fatalf("workspaces: %v", err)
	}
	if len(workspaces) != 2 {
		t.Fatalf("got %d workspaces, want 2", len(workspaces))
	}
	if !workspaces[1].ReadOnly() {
		t.Error("the :ro suffix was not honoured")
	}
	if workspaces[0].ReadOnly() {
		t.Error("the primary workspace became read-only")
	}
}

// A mistyped agent is read as a directory, so the error has to say what the agents are.
func TestWorkspaceErrorMentionsAgents(t *testing.T) {
	_, err := (&sandboxFlags{}).workspaces([]string{"cladue"}, agent.Builtin())
	if err == nil {
		t.Fatal("a non-existent workspace was accepted")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error = %q, want it to list the agents", err)
	}
}

// A non-VM runtime must never be presented as a sandbox by accident.
func TestRequireIsolation(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flags := registerSandboxFlags(fs)
	if err := fs.Parse([]string{"-runtime", "io.containerd.runc.v2"}); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if err := flags.requireIsolation(&stderr); err == nil {
		t.Fatal("requireIsolation allowed a container runtime without the opt-out")
	}

	optOut := flag.NewFlagSet("test", flag.ContinueOnError)
	optOut.SetOutput(io.Discard)
	optOutFlags := registerSandboxFlags(optOut)
	if err := optOut.Parse([]string{"-runtime", "io.containerd.runc.v2", "-i-know-this-is-not-isolated"}); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if err := optOutFlags.requireIsolation(&stderr); err != nil {
		t.Fatalf("requireIsolation with the opt-out: %v", err)
	}
	if !strings.Contains(stderr.String(), "NOT an isolation boundary") {
		t.Errorf("stderr = %q, want a warning that this is not isolation", stderr.String())
	}
}

// The agent decides what a sandbox contains; the flags only override it.
func TestConfigTakesImageAndCommandFromTheAgent(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flags := registerSandboxFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}

	shell, _ := agent.Builtin().Lookup("shell")
	cfg, err := flags.config(invocation{agent: shell, name: "shell-foo"}, nil)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.Image != shell.Image {
		t.Errorf("image = %q, want the agent's %q", cfg.Image, shell.Image)
	}
	if cfg.Agent != "shell" {
		t.Errorf("agent = %q, want %q", cfg.Agent, "shell")
	}
	if !slices.Equal(cfg.Command, shell.Argv(nil)) {
		t.Errorf("command = %v, want the agent's %v", cfg.Command, shell.Argv(nil))
	}
	if cfg.CPUs < 1 || cfg.MemoryMiB < 64 {
		t.Errorf("auto sizing gave %d vCPUs and %d MiB", cfg.CPUs, cfg.MemoryMiB)
	}

	// -template overrides the agent's image; arguments after -- become the command,
	// because that is what arguments to a shell are. The agent's init prefix goes with the
	// image it belongs to — a Debian image has no /usr/bin/tini and no Boks entrypoint.
	override := flag.NewFlagSet("test", flag.ContinueOnError)
	override.SetOutput(io.Discard)
	overrideFlags := registerSandboxFlags(override)
	if err := override.Parse([]string{"-t", "debian:stable", "-cpus", "3", "-m", "512m"}); err != nil {
		t.Fatal(err)
	}
	cfg, err = overrideFlags.config(invocation{agent: shell, name: "shell-foo"}, []string{"uname", "-a"})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.Image != "debian:stable" {
		t.Errorf("image = %q, want the -template value", cfg.Image)
	}
	if !slices.Equal(cfg.Command, []string{"uname", "-a"}) {
		t.Errorf("command = %v, want the arguments after --", cfg.Command)
	}
	if cfg.CPUs != 3 || cfg.MemoryMiB != 512 {
		t.Errorf("sizing = %d vCPUs, %d MiB; want 3 and 512", cfg.CPUs, cfg.MemoryMiB)
	}
}

// `boks create shell . -- npm run dev` records that command; a later `boks run shell .`
// must run it rather than the agent's bare command line.
func TestConfigKeepsAnExistingSandboxesRecordedCommand(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flags := registerSandboxFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	shell, _ := agent.Builtin().Lookup("shell")

	cfg, err := flags.config(invocation{agent: shell, name: "shell-foo", exists: true}, nil)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if len(cfg.Command) != 0 {
		t.Errorf("command = %v, want none so the sandbox's own is used", cfg.Command)
	}

	// Arguments given now still win over the recorded ones.
	cfg, err = flags.config(invocation{agent: shell, name: "shell-foo", exists: true}, []string{"ls"})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if !slices.Equal(cfg.Command, shell.Argv([]string{"ls"})) {
		t.Errorf("command = %v, want the arguments after --", cfg.Command)
	}
}

func TestWriteTable(t *testing.T) {
	var out bytes.Buffer
	writeTable(&out, []sandbox.Info{{
		Name:       "claude-boks",
		Agent:      "claude",
		Status:     sandbox.StatusRunning,
		Image:      "docker.io/library/alpine:latest",
		Workspaces: []sandbox.WorkspaceRef{{HostPath: "/home/alice/src/foo"}},
	}})

	got := out.String()
	header := strings.SplitN(got, "\n", 2)[0]
	for _, want := range []string{"SANDBOX", "AGENT", "STATUS", "PORTS", "WORKSPACE"} {
		if !strings.Contains(header, want) {
			t.Errorf("header = %q, want a %s column", header, want)
		}
	}
	if strings.Contains(header, "IMAGE") {
		t.Errorf("header = %q, want sbx's columns only", header)
	}
	for _, want := range []string{"claude-boks", "claude", "running", "/home/alice/src/foo"} {
		if !strings.Contains(got, want) {
			t.Errorf("table = %q, want it to contain %q", got, want)
		}
	}
}

// A sandbox from an older Boks has no agent recorded, and one may have no workspace. Both
// still have to render a row.
func TestWriteTableWithoutAgentOrWorkspace(t *testing.T) {
	var out bytes.Buffer
	writeTable(&out, []sandbox.Info{{Name: "bare", Status: sandbox.StatusStopped}})
	if !strings.Contains(out.String(), "bare") {
		t.Errorf("table = %q, want a row for the sandbox", out.String())
	}
}

func chdir(t *testing.T, dir string) func() {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Chdir(previous) }
}
