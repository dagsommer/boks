package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/dagsommer/boks/internal/agent"
	"github.com/dagsommer/boks/internal/sandbox"
)

// TestExecGrammar is what parseLeadingFlags used to be tested for, asserted through the real
// command now that pflag does the parsing: boks' flags come before the sandbox name, and
// everything after it — flags included — belongs to the guest. `-it` is no longer a
// special-cased flag of its own; pflag splits the combined shorthand.
func TestExecGrammar(t *testing.T) {
	tests := []struct {
		name            string
		args            []string
		wantName        string
		wantCommand     []string
		wantTTY         bool
		wantInteractive bool
		wantErr         bool
	}{
		{name: "name and command", args: []string{"web", "ls"}, wantName: "web", wantCommand: []string{"ls"}},
		{
			// Guest flags must reach the guest, not our flag set.
			name: "guest flags", args: []string{"-t", "web", "ls", "-l", "-t"},
			wantName: "web", wantCommand: []string{"ls", "-l", "-t"}, wantTTY: true,
		},
		{
			name: "combined shorthand", args: []string{"-it", "web", "sh"},
			wantName: "web", wantCommand: []string{"sh"}, wantTTY: true, wantInteractive: true,
		},
		{
			name: "explicit separator", args: []string{"web", "--", "git", "status"},
			wantName: "web", wantCommand: []string{"git", "status"},
		},
		{
			name: "flag with a value", args: []string{"-w", "/tmp", "-e", "A=1", "web", "sh"},
			wantName: "web", wantCommand: []string{"sh"},
		},
		{name: "name only", args: []string{"web"}, wantErr: true},
		{name: "nothing", args: []string{}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newExecCommand(Env{Stdout: io.Discard, Stderr: io.Discard}, &devFlags{})
			var gotName string
			var gotCommand []string
			cmd.RunE = func(cmd *cobra.Command, args []string) error {
				gotName, gotCommand = args[0], commandFor(args)
				return nil
			}
			cmd.SetArgs(tt.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)

			err := cmd.Execute()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("boks exec %v was accepted", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("boks exec %v: %v", tt.args, err)
			}
			if gotName != tt.wantName {
				t.Errorf("sandbox = %q, want %q", gotName, tt.wantName)
			}
			if !slices.Equal(gotCommand, tt.wantCommand) {
				t.Errorf("command = %v, want %v", gotCommand, tt.wantCommand)
			}
			tty, _ := cmd.Flags().GetBool("tty")
			interactive, _ := cmd.Flags().GetBool("interactive")
			if tty != tt.wantTTY {
				t.Errorf("--tty = %v, want %v", tty, tt.wantTTY)
			}
			if interactive != tt.wantInteractive {
				t.Errorf("--interactive = %v, want %v", interactive, tt.wantInteractive)
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
	dev := parseDevFlags(t, "--runtime", "io.containerd.runc.v2")
	var stderr bytes.Buffer
	if err := dev.requireIsolation(&stderr); err == nil {
		t.Fatal("requireIsolation allowed a container runtime without the opt-out")
	}

	optOut := parseDevFlags(t, "--runtime", "io.containerd.runc.v2", "--i-know-this-is-not-isolated")
	stderr.Reset()
	if err := optOut.requireIsolation(&stderr); err != nil {
		t.Fatalf("requireIsolation with the opt-out: %v", err)
	}
	if !strings.Contains(stderr.String(), "NOT an isolation boundary") {
		t.Errorf("stderr = %q, want a warning that this is not isolation", stderr.String())
	}
}

// The opt-out is hidden from help, which must not make it harder to find: the refusal names
// it, and that is where someone reads it.
func TestIsolationRefusalNamesItsOptOut(t *testing.T) {
	dev := parseDevFlags(t, "--runtime", "io.containerd.runc.v2")
	err := dev.requireIsolation(io.Discard)
	if err == nil {
		t.Fatal("requireIsolation allowed a container runtime without the opt-out")
	}
	if !strings.Contains(err.Error(), "--i-know-this-is-not-isolated") {
		t.Errorf("the refusal does not name the flag that overrides it: %v", err)
	}
}

// parseDevFlags registers the hidden developer flags on their own set and parses arguments
// into them, the way the root command does for the whole tree.
func parseDevFlags(t *testing.T, args ...string) *devFlags {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dev := &devFlags{}
	dev.register(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return dev
}

// parseSandboxFlags registers the sandbox flags on their own set and parses arguments into
// them, so a test can exercise config() without running a command.
func parseSandboxFlags(t *testing.T, args ...string) *sandboxFlags {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dev := &devFlags{}
	dev.register(fs)
	flags := registerSandboxFlags(fs, dev)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return flags
}

// The agent decides what a sandbox contains; the flags only override it.
func TestConfigTakesImageAndCommandFromTheAgent(t *testing.T) {
	flags := parseSandboxFlags(t)

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

	// --template overrides the agent's image; arguments after -- become the command,
	// because that is what arguments to a shell are. The agent's init prefix goes with the
	// image it belongs to — a Debian image has no /usr/bin/tini and no Boks entrypoint.
	overrideFlags := parseSandboxFlags(t, "-t", "debian:stable", "--cpus", "3", "-m", "512m")
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
	flags := parseSandboxFlags(t)
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
	}}, map[string]string{"claude-boks": "127.0.0.1:8080->3000/tcp"})

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
	for _, want := range []string{"claude-boks", "claude", "running", "127.0.0.1:8080->3000/tcp", "/home/alice/src/foo"} {
		if !strings.Contains(got, want) {
			t.Errorf("table = %q, want it to contain %q", got, want)
		}
	}
}

// A sandbox from an older Boks has no agent recorded, and one may have no workspace. Both
// still have to render a row.
func TestWriteTableWithoutAgentOrWorkspace(t *testing.T) {
	var out bytes.Buffer
	writeTable(&out, []sandbox.Info{{Name: "bare", Status: sandbox.StatusStopped}}, nil)
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
