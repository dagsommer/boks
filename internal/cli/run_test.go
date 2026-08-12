package cli

import (
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// parseRun runs `boks run` far enough to see what it made of its arguments, and no further:
// the real RunE is replaced, so nothing contacts containerd.
func parseRun(t *testing.T, args []string) (positional, agentArgs []string, flags map[string]string, err error) {
	t.Helper()
	cmd := newRunCommand(Env{Stdout: io.Discard, Stderr: io.Discard}, &devFlags{})
	flags = map[string]string{}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		positional, agentArgs = splitAtDash(cmd, args)
		cmd.Flags().Visit(func(f *pflag.Flag) { flags[f.Name] = f.Value.String() })
		return nil
	}
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err = cmd.Execute()
	return positional, agentArgs, flags, err
}

// Everything after "--" belongs to the agent, and only the first "--" is ours. pflag records
// where the separator fell, so the guest's own flags never reach our flag set — this is what
// splitAtDoubleDash used to do by hand.
func TestRunSplitsAgentArgumentsAtTheSeparator(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantPositional []string
		wantAgentArgs  []string
	}{
		{"no separator", []string{".", "-t", "img"}, []string{"."}, nil},
		{"simple", []string{".", "--", "ls"}, []string{"."}, []string{"ls"}},
		{
			// Guest flags must never reach our parser.
			"guest flags preserved",
			[]string{".", "--", "sh", "-lc", "pwd && ls"},
			[]string{"."},
			[]string{"sh", "-lc", "pwd && ls"},
		},
		{"separator last", []string{".", "--"}, []string{"."}, []string{}},
		{
			// Only the first separator is ours; later ones belong to the guest.
			"second separator belongs to guest",
			[]string{".", "--", "git", "log", "--", "path"},
			[]string{"."},
			[]string{"git", "log", "--", "path"},
		},
		{
			// The agent is a positional like any other, and the separator does not
			// change that.
			"agent, workspace and agent args",
			[]string{"claude", ".", "--", "--continue"},
			[]string{"claude", "."},
			[]string{"--continue"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			positional, agentArgs, _, err := parseRun(t, tt.args)
			if err != nil {
				t.Fatalf("boks run %v: %v", tt.args, err)
			}
			if !slices.Equal(positional, tt.wantPositional) {
				t.Errorf("positional = %v, want %v", positional, tt.wantPositional)
			}
			if !slices.Equal(agentArgs, tt.wantAgentArgs) {
				t.Errorf("agent args = %v, want %v", agentArgs, tt.wantAgentArgs)
			}
		})
	}
}

// Flags may appear before the positionals, after them, or on both sides. The old parser had
// to re-parse in a loop to manage that; pflag does it natively.
func TestRunAcceptsFlagsAnywhereAmongThePositionals(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantPositional []string
		wantTemplate   string
		wantDetached   bool
	}{
		{"flags first", []string{"--template", "img", "."}, []string{"."}, "img", false},
		{"flags after positional", []string{".", "--template", "img"}, []string{"."}, "img", false},
		{"short form after positional", []string{".", "-t", "img"}, []string{"."}, "img", false},
		{"flags on both sides", []string{"-d", ".", "--template", "img"}, []string{"."}, "img", true},
		{"positional only", []string{"."}, []string{"."}, "", false},
		{"multiple positionals", []string{"shell", "-d", "."}, []string{"shell", "."}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			positional, _, flags, err := parseRun(t, tt.args)
			if err != nil {
				t.Fatalf("boks run %v: %v", tt.args, err)
			}
			if !slices.Equal(positional, tt.wantPositional) {
				t.Errorf("positional = %v, want %v", positional, tt.wantPositional)
			}
			if flags["template"] != tt.wantTemplate {
				t.Errorf("--template = %q, want %q", flags["template"], tt.wantTemplate)
			}
			if (flags["detached"] == "true") != tt.wantDetached {
				t.Errorf("--detached = %q, want %v", flags["detached"], tt.wantDetached)
			}
		})
	}
}

func TestRunReportsBadFlags(t *testing.T) {
	if _, _, _, err := parseRun(t, []string{".", "--nope"}); err == nil {
		t.Fatal("boks run accepted an unknown flag")
	}
	// A long flag written the old way, with one dash, is a cluster of shorthands to pflag
	// — and every shorthand in it has to exist. This is the one deliberate incompatibility
	// of the move to cobra, and it fails loudly rather than quietly.
	_, _, _, err := parseRun(t, []string{"-name", "web"})
	if err == nil {
		t.Fatal("boks run accepted a single-dash long flag")
	}
	if !strings.Contains(err.Error(), "shorthand") {
		t.Errorf("error = %q, want it to say the letters were read as shorthands", err)
	}
}
