package cli

import (
	"flag"
	"io"
	"slices"
	"testing"
)

func TestSplitAtDoubleDash(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantOwn     []string
		wantCommand []string
	}{
		{"no separator", []string{".", "-t"}, []string{".", "-t"}, nil},
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			own, command := splitAtDoubleDash(tt.args)
			if !slices.Equal(own, tt.wantOwn) {
				t.Errorf("own = %v, want %v", own, tt.wantOwn)
			}
			if !slices.Equal(command, tt.wantCommand) {
				t.Errorf("command = %v, want %v", command, tt.wantCommand)
			}
		})
	}
}

func TestParseInterspersed(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantPositional []string
		wantFlag       string
		wantBool       bool
	}{
		{"flags first", []string{"-template", "img", "."}, []string{"."}, "img", false},
		{"flags after positional", []string{".", "-template", "img"}, []string{"."}, "img", false},
		{"flags on both sides", []string{"-t", ".", "-template", "img"}, []string{"."}, "img", true},
		{"positional only", []string{"."}, []string{"."}, "", false},
		{"multiple positionals", []string{"a", "-t", "b"}, []string{"a", "b"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			image := fs.String("template", "", "")
			tty := fs.Bool("t", false, "")

			positional, err := parseInterspersed(fs, tt.args)
			if err != nil {
				t.Fatalf("parseInterspersed(%v): %v", tt.args, err)
			}
			if !slices.Equal(positional, tt.wantPositional) {
				t.Errorf("positional = %v, want %v", positional, tt.wantPositional)
			}
			if *image != tt.wantFlag {
				t.Errorf("-template = %q, want %q", *image, tt.wantFlag)
			}
			if *tty != tt.wantBool {
				t.Errorf("-t = %v, want %v", *tty, tt.wantBool)
			}
		})
	}
}

func TestParseInterspersedReportsBadFlags(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	if _, err := parseInterspersed(fs, []string{".", "-nope"}); err == nil {
		t.Fatal("parseInterspersed accepted an unknown flag")
	}
}
