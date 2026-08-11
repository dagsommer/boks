package agent

import (
	"slices"
	"strings"
	"testing"
)

// The built-in set is sbx's, so that a habit formed there works here.
func TestBuiltinNames(t *testing.T) {
	want := []string{"claude", "codex", "copilot", "cursor", "docker-agent", "droid", "gemini", "kiro", "opencode", "shell"}
	got := Builtin().Names()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("agents = %v, want %v", got, want)
	}
}

// Every agent Boks ships an image for runs without being told one, and every one of those
// images is a Boks image at the published tag.
func TestRunnableAgentsPointAtBoksImages(t *testing.T) {
	r := Builtin()
	shell, ok := r.Lookup("shell")
	if !ok {
		t.Fatal("the default agent is not registered")
	}
	// The shell agent is the base image itself, not an image of its own.
	if shell.Image != Image("base") {
		t.Errorf("shell image = %q, want the base image %q", shell.Image, Image("base"))
	}
	if err := RequireRunnable(shell); err != nil {
		t.Errorf("RequireRunnable(shell) = %v, want nil", err)
	}

	for _, a := range r.All() {
		if !a.Runnable() {
			continue
		}
		if !strings.HasPrefix(a.Image, ImageRepo+"/") {
			t.Errorf("agent %q runs %q, which is not a Boks image", a.Name, a.Image)
		}
		if !strings.HasSuffix(a.Image, ":"+ImageTag) {
			t.Errorf("agent %q runs %q, which is not at the published tag %q", a.Name, a.Image, ImageTag)
		}
		// An image Boks builds carries tini and the CA entrypoint, and a sandbox
		// bypasses the image's own ENTRYPOINT, so the definition has to name them.
		if len(a.Init) == 0 {
			t.Errorf("agent %q has an image but no init prefix", a.Name)
		}
	}
}

// Kiro is registered without an image. That has to read as "the name is right, the
// environment is missing", which is a different answer from "no such agent".
func TestAnAgentWithoutAnImageExplainsItself(t *testing.T) {
	kiro, ok := Builtin().Lookup("kiro")
	if !ok {
		t.Fatal("kiro is not registered")
	}
	if kiro.Runnable() {
		t.Fatal("kiro was reported as runnable; update this test if an image now exists")
	}
	err := RequireRunnable(kiro)
	if err == nil {
		t.Fatal("an agent with no image was reported as runnable")
	}
	if !strings.Contains(err.Error(), "-template") {
		t.Errorf("error = %q, want it to say how to supply an image", err)
	}
}

// -template puts the agent in an image Boks knows nothing about, where the paths in Init do
// not exist. Bare is what keeps the command runnable there.
func TestBareDropsTheInitPrefix(t *testing.T) {
	claude, _ := Builtin().Lookup("claude")
	if got := claude.Argv(nil); !slices.Equal(got, append(slices.Clone(initArgv), "claude")) {
		t.Errorf("Argv = %v, want the init prefix in front of the command", got)
	}
	if got := claude.Bare().Argv(nil); !slices.Equal(got, []string{"claude"}) {
		t.Errorf("Bare().Argv = %v, want just the command", got)
	}
}

func TestResolveUnknownAgentListsTheKnownOnes(t *testing.T) {
	_, err := Builtin().Resolve("cladue")
	if err == nil {
		t.Fatal("an unknown agent was accepted")
	}
	for _, want := range []string{"cladue", "claude", "shell"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// What `--` means depends on the agent: arguments to an agent that has its own command,
// and the command itself for a shell.
func TestArgv(t *testing.T) {
	tests := []struct {
		name  string
		agent Agent
		extra []string
		want  []string
	}{
		{"no extra", Agent{Command: []string{"claude"}}, nil, []string{"claude"}},
		{"appended", Agent{Command: []string{"claude"}, Args: ArgsAppend}, []string{"--continue"},
			[]string{"claude", "--continue"}},
		{"a shell's arguments are the command", Agent{Command: []string{"/bin/sh"}, Args: ArgsCommand},
			[]string{"uname", "-a"}, []string{"uname", "-a"}},
		{"no command of its own", Agent{Args: ArgsAppend}, []string{"ls"}, []string{"ls"}},
		{"image default", Agent{}, nil, nil},
		{"init comes first", Agent{Init: []string{"tini", "--"}, Command: []string{"claude"}}, nil,
			[]string{"tini", "--", "claude"}},
		{"init survives appended arguments",
			Agent{Init: []string{"tini", "--"}, Command: []string{"claude"}, Args: ArgsAppend},
			[]string{"--continue"}, []string{"tini", "--", "claude", "--continue"}},
		{"init survives a replaced command",
			Agent{Init: []string{"tini", "--"}, Command: []string{"/bin/bash"}, Args: ArgsCommand},
			[]string{"uname", "-a"}, []string{"tini", "--", "uname", "-a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.agent.Argv(tt.extra); !slices.Equal(got, tt.want) {
				t.Errorf("Argv(%v) = %v, want %v", tt.extra, got, tt.want)
			}
		})
	}
}

// Add is the seam a user-defined agent will arrive through, so it has to behave like one
// even before a loader exists: register, override, and reject a name that could not become
// a sandbox name.
func TestAddUserDefinedAgent(t *testing.T) {
	r := Builtin()
	custom := Agent{Name: "udi-copilot-yolo", Image: "example.test/copilot:latest", Command: []string{"copilot"}}
	if err := r.Add(custom); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, ok := r.Lookup("udi-copilot-yolo")
	if !ok || got.Image != custom.Image {
		t.Fatalf("looked up %+v, want the added agent", got)
	}
	if got.Args != ArgsAppend {
		t.Errorf("args mode = %q, want the default %q", got.Args, ArgsAppend)
	}
	if !r.Known("udi-copilot-yolo") {
		t.Error("an added agent is not recognised as a first positional")
	}

	// Overriding a built-in is how a user replaces an environment Boks ships.
	before := len(r.All())
	if err := r.Add(Agent{Name: "shell", Image: "example.test/shell:latest"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(r.All()) != before {
		t.Errorf("overriding an agent changed the count from %d to %d", before, len(r.All()))
	}
	if shell, _ := r.Lookup("shell"); shell.Image != "example.test/shell:latest" {
		t.Errorf("shell image = %q, want the override", shell.Image)
	}

	for _, bad := range []string{"", "has space", "slash/name", "-leading", strings.Repeat("a", 40)} {
		if err := r.Add(Agent{Name: bad}); err == nil {
			t.Errorf("Add accepted the name %q", bad)
		}
	}
}
