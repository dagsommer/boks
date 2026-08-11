package sandbox

import (
	"slices"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/workspace"
)

// The naming rule itself, which is also the re-attach rule: the same agent and the same
// directory must always produce the same name, and it must be one a person can read.
func TestDeriveName(t *testing.T) {
	tests := []struct {
		name  string
		agent string
		path  string
		want  string
	}{
		{"plain", "claude", "/Users/d/git_repos/boks", "claude-boks"},
		{"dots kept", "claude", "/Users/d/git_repos/finndato.no", "claude-finndato.no"},
		{"hyphens kept in both halves", "udi-copilot-yolo", "/Users/d/efm-integrasjonspunkt",
			"udi-copilot-yolo-efm-integrasjonspunkt"},
		{"case kept", "shell", "/home/alice/MyProject", "shell-MyProject"},
		{"underscores kept", "shell", "/home/alice/my_project", "shell-my_project"},
		{"spaces folded", "shell", "/home/alice/my project", "shell-my-project"},
		{"repeated separators collapse", "shell", "/home/alice/my  --project", "shell-my-project"},
		{"separators trimmed at the edges", "shell", "/home/alice/-project-", "shell-project"},
		{"characters containerd rejects", "shell", "/home/alice/foo@bar!", "shell-foo-bar"},
		{"filesystem root", "shell", "/", "shell-root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DeriveName(tt.agent, tt.path)
			if err != nil {
				t.Fatalf("DeriveName(%q, %q): %v", tt.agent, tt.path, err)
			}
			if got != tt.want {
				t.Errorf("DeriveName(%q, %q) = %q, want %q", tt.agent, tt.path, got, tt.want)
			}
			if err := ValidateName(got); err != nil {
				t.Errorf("DeriveName produced a name containerd would reject: %v", err)
			}
			again, _ := DeriveName(tt.agent, tt.path)
			if again != got {
				t.Errorf("DeriveName is not stable: %q then %q", got, again)
			}
		})
	}
}

// A basename with nothing containerd will take is not an error: it falls back to a digest,
// which is unreadable but still correct and still re-attaches.
func TestDeriveNameFallsBackToADigest(t *testing.T) {
	got, err := DeriveName("shell", "/home/alice/日本語")
	if err != nil {
		t.Fatalf("DeriveName: %v", err)
	}
	if err := ValidateName(got); err != nil {
		t.Errorf("name %q is not usable: %v", got, err)
	}
	if !strings.HasPrefix(got, "shell-") || len(got) != len("shell-")+digestChars {
		t.Errorf("DeriveName = %q, want a shell- prefix and a short digest", got)
	}
	other, _ := DeriveName("shell", "/home/alice/中文")
	if other == got {
		t.Errorf("two different unnameable directories both derive %q", got)
	}
}

// containerd caps identifiers at 76 characters, so a long directory name has to be cut —
// and cutting it is exactly what could make two directories share a name, so the digest
// comes along.
func TestDeriveNameHonoursTheLengthLimit(t *testing.T) {
	long := strings.Repeat("a", 200)
	first, err := DeriveName("shell", "/home/alice/"+long+"-one")
	if err != nil {
		t.Fatalf("DeriveName: %v", err)
	}
	second, err := DeriveName("shell", "/home/alice/"+long+"-two")
	if err != nil {
		t.Fatalf("DeriveName: %v", err)
	}
	for _, name := range []string{first, second} {
		if len(name) > maxNameLength {
			t.Errorf("name %q is %d characters, over the %d limit", name, len(name), maxNameLength)
		}
		if err := ValidateName(name); err != nil {
			t.Errorf("name %q is not usable: %v", name, err)
		}
	}
	if first == second {
		t.Errorf("two directories sharing a long prefix both derive %q", first)
	}
}

// An agent name that leaves no room for a workspace is refused rather than silently
// truncated into something the user never asked for.
func TestDeriveNameRefusesAnOversizedAgent(t *testing.T) {
	if _, err := DeriveName(strings.Repeat("a", maxNameLength), "/home/alice/foo"); err == nil {
		t.Error("DeriveName accepted an agent name that fills the whole identifier")
	}
	if _, err := DeriveName("not an agent", "/home/alice/foo"); err == nil {
		t.Error("DeriveName accepted an agent name containerd would reject")
	}
}

// Two directories with the same basename can now collide, which the old path-digest scheme
// made impossible. The second one must get its own sandbox, deterministically, and be told.
func TestChooseName(t *testing.T) {
	const agent = "claude"
	mine, theirs := "/home/alice/src/foo", "/home/bob/src/foo"
	qualified, err := QualifiedName(agent, mine)
	if err != nil {
		t.Fatal(err)
	}

	none := func(string) (Info, bool) { return Info{}, false }
	held := func(name string, workspace string) func(string) (Info, bool) {
		return func(candidate string) (Info, bool) {
			if candidate != name {
				return Info{}, false
			}
			return Info{Name: name, Workspaces: []WorkspaceRef{{HostPath: workspace}}}, true
		}
	}

	tests := []struct {
		name         string
		lookup       func(string) (Info, bool)
		wantName     string
		wantExists   bool
		wantCollided string
	}{
		{"nothing exists", none, "claude-foo", false, ""},
		{"re-attach", held("claude-foo", mine), "claude-foo", true, ""},
		{"another directory holds the readable name", held("claude-foo", theirs), qualified, false, theirs},
		{"already bumped once", held(qualified, mine), qualified, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ChooseName(agent, mine, tt.lookup)
			if err != nil {
				t.Fatalf("ChooseName: %v", err)
			}
			if got.Name != tt.wantName {
				t.Errorf("name = %q, want %q", got.Name, tt.wantName)
			}
			if got.Exists != tt.wantExists {
				t.Errorf("exists = %v, want %v", got.Exists, tt.wantExists)
			}
			if got.CollidedWith != tt.wantCollided {
				t.Errorf("collided with = %q, want %q", got.CollidedWith, tt.wantCollided)
			}
		})
	}
}

// A sandbox created before Boks recorded workspaces has no path to compare against.
// Re-attaching to it is better than orphaning it behind a qualified name.
func TestChooseNameReattachesToASandboxWithoutAWorkspace(t *testing.T) {
	got, err := ChooseName("shell", "/home/alice/foo", func(name string) (Info, bool) {
		return Info{Name: name}, name == "shell-foo"
	})
	if err != nil {
		t.Fatalf("ChooseName: %v", err)
	}
	if got.Name != "shell-foo" || !got.Exists {
		t.Errorf("choice = %+v, want the readable name, re-attached", got)
	}
}

// An ephemeral run must never take the persistent sandbox's name, nor another run's.
func TestEphemeralName(t *testing.T) {
	first, err := EphemeralName("shell", "/home/alice/foo")
	if err != nil {
		t.Fatalf("EphemeralName: %v", err)
	}
	second, _ := EphemeralName("shell", "/home/alice/foo")
	derived, _ := DeriveName("shell", "/home/alice/foo")
	if first == second {
		t.Errorf("two ephemeral runs share the name %q", first)
	}
	if first == derived {
		t.Errorf("an ephemeral run took the persistent name %q", derived)
	}
	if err := ValidateName(first); err != nil {
		t.Errorf("name %q is not usable: %v", first, err)
	}
}

func TestValidateName(t *testing.T) {
	valid := []string{"web", "boks-1a2b", "a", "my.sandbox_1", "A-B.c_d"}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",                  // nothing to name
		"has space",         // containerd rejects it
		"slash/name",        // would be read as a path by cp
		"-leading",          // separator at the edge
		"trailing-",         //
		"double--separator", //
		strings.Repeat("a", maxNameLength+1),
	}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", name)
		}
	}
}

// Labels are the only place workspaces are recorded, so a round trip has to be lossless.
func TestWorkspaceLabelRoundTrip(t *testing.T) {
	workspaces := []workspace.Workspace{
		{HostPath: "/a", GuestPath: "/a", Mode: workspace.ModeReadWrite},
		{HostPath: "/b:c", GuestPath: "/b:c", Mode: workspace.ModeReadOnly},
	}
	encoded, err := encodeLabel(workspaceRefs(workspaces))
	if err != nil {
		t.Fatalf("encodeLabel: %v", err)
	}

	got := decodeWorkspaces(map[string]string{LabelWorkspaces: encoded})
	want := []WorkspaceRef{
		{HostPath: "/a", GuestPath: "/a", Mode: "rw"},
		{HostPath: "/b:c", GuestPath: "/b:c", Mode: "ro"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("workspaces = %v, want %v", got, want)
	}
}

func TestCommandLabelRoundTrip(t *testing.T) {
	encoded, err := encodeLabel([]string{"sh", "-lc", "echo hi && exit 0"})
	if err != nil {
		t.Fatalf("encodeLabel: %v", err)
	}
	got := decodeCommand(map[string]string{LabelCommand: encoded})
	if !slices.Equal(got, []string{"sh", "-lc", "echo hi && exit 0"}) {
		t.Errorf("command = %v", got)
	}
}

// A sandbox created by an older or newer Boks may not carry the labels this one expects.
// Missing or corrupt metadata must degrade to "unknown", never to a failure.
func TestLabelDecodingToleratesGarbage(t *testing.T) {
	labels := map[string]string{LabelWorkspaces: "{not json", LabelCommand: "[1,2"}
	if got := decodeWorkspaces(labels); len(got) != 0 {
		t.Errorf("workspaces = %v, want none", got)
	}
	if got := decodeCommand(labels); len(got) != 0 {
		t.Errorf("command = %v, want none", got)
	}
	if got := decodeWorkspaces(nil); len(got) != 0 {
		t.Errorf("workspaces = %v, want none", got)
	}
}

func TestInfoWorkspace(t *testing.T) {
	info := Info{Workspaces: []WorkspaceRef{{HostPath: "/first"}, {HostPath: "/second"}}}
	if got := info.Workspace(); got != "/first" {
		t.Errorf("Workspace() = %q, want the primary workspace", got)
	}
	if got := (Info{}).Workspace(); got != "" {
		t.Errorf("Workspace() = %q, want empty for a sandbox with none", got)
	}
}
