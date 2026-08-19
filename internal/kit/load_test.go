package kit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A kit directory and a direct path to its spec.yaml are the two local forms, and both have to
// work: `--kit ./my-kit` is what the documentation shows, and `--kit ./my-kit/spec.yaml` is
// what someone types after tab-completion.
func TestLoadLocalForms(t *testing.T) {
	dir := t.TempDir()
	spec := `schemaVersion: "2"
kind: sandbox
name: local-kit
sandbox:
  image: example/image:latest
permissions:
  network:
    allow: [example.com]
`
	if err := os.WriteFile(filepath.Join(dir, SpecFileName), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, ref := range []string{dir, filepath.Join(dir, SpecFileName)} {
		got, _, err := Load(ref)
		if err != nil {
			t.Fatalf("Load(%q): %v", ref, err)
		}
		if got.Name != "local-kit" {
			t.Errorf("Load(%q).Name = %q", ref, got.Name)
		}
	}
}

// A reference Boks cannot fetch must say so, and say which kind it is. Before this, `oci://…`
// reached os.Stat and came back "no such file or directory", which describes a filename that
// was never a filename and sends the reader looking for a typo.
func TestLoadRefusesRemoteFormsByName(t *testing.T) {
	for _, tc := range []struct{ ref, want string }{
		{"git+https://example.com/repo.git#ref=abc", "git"},
		{"git+ssh://git@example.com/repo.git", "git"},
		{"https://example.com/kit.tar.gz", "HTTPS"},
		{"oci://ghcr.io/org/kit@sha256:abc", "OCI"},
		{"ghcr.io/org/kit:1.0", "OCI"},
		{"./my-kit-1.0.zip", "ZIP archive"},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			_, _, err := Load(tc.ref)
			if err == nil {
				t.Fatalf("Load(%q) succeeded; a form Boks cannot fetch must be refused", tc.ref)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the form %q", err, tc.want)
			}
			// The refusal has to say it is unimplemented ON PURPOSE, or the next
			// person reads it as a bug and "fixes" it by adding an unpinned fetch.
			if !strings.Contains(err.Error(), "pinned") {
				t.Errorf("error %q does not say why it is refused", err)
			}
		})
	}
}

// The control for the refusals above: a local path that merely looks unusual must NOT be
// mistaken for a registry reference. A kit directory called `my-kit` has no dot in its first
// segment; one called `v1.2/kit` does, and is still a path.
func TestLoadDoesNotMistakePathsForRegistries(t *testing.T) {
	for _, ref := range []string{"my-kit", "./my-kit", "/abs/my-kit", "../my-kit"} {
		if form, remote := remoteForm(ref); remote {
			t.Errorf("remoteForm(%q) = %q, want it treated as a local path", ref, form)
		}
	}
}

// NetworkRules must not invent rules for a kit that declares none, and must keep allow and deny
// apart. Conflating them would be the worst possible bug in this function: a denied domain
// returned as an allow inverts the author's intent.
func TestNetworkRules(t *testing.T) {
	spec := `schemaVersion: "2"
kind: sandbox
name: net-kit
sandbox:
  image: example/image:latest
permissions:
  network:
    allow: [allowed.example.com]
    deny: [denied.example.com]
`
	s, _, err := ParseSpec([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	allow, deny := NetworkRules(s)
	if len(allow) != 1 || allow[0] != "allowed.example.com" {
		t.Errorf("allow = %v", allow)
	}
	if len(deny) != 1 || deny[0] != "denied.example.com" {
		t.Errorf("deny = %v", deny)
	}

	bare, _, err := ParseSpec([]byte("schemaVersion: \"2\"\nkind: mixin\nname: bare\n"))
	if err != nil {
		t.Fatal(err)
	}
	if allow, deny := NetworkRules(bare); allow != nil || deny != nil {
		t.Errorf("a kit with no permissions block produced allow=%v deny=%v", allow, deny)
	}
}
