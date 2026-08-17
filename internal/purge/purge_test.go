package purge

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fill writes a state directory with one file under each named entry, so a plan has something
// to classify and something to measure.
func fill(t *testing.T, root string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".jsonl") ||
			strings.HasSuffix(name, ".txt") {
			if err := os.WriteFile(filepath.Join(root, name), []byte("0123456789"), 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "payload"), []byte("0123456789"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// names lists an entry slice by name, which is what most assertions here are about.
func names(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The default scope must give the disk back and keep everything a user would be upset to lose
// without being asked. This is the whole point of there being two scopes, so it is asserted in
// both directions: what goes, and what stays.
func TestReclaimScopeKeepsIdentityConfigurationAndTheLog(t *testing.T) {
	root := filepath.Join(t.TempDir(), "boks")
	fill(t, root, "containerd", "net", "certs", "notices", "update.json",
		"ca", "secrets.json", "policy", "policy-log.jsonl")

	plan, err := Inspect(root, ScopeReclaim)
	if err != nil {
		t.Fatal(err)
	}
	wantRemove := []string{"containerd", "net", "certs", "notices", "update.json"}
	if got := names(plan.Remove); !equal(got, wantRemove) {
		t.Errorf("Remove = %v, want %v", got, wantRemove)
	}
	wantKeep := []string{"ca", "secrets.json", "policy", "policy-log.jsonl"}
	if got := names(plan.Keep); !equal(got, wantKeep) {
		t.Errorf("Keep = %v, want %v", got, wantKeep)
	}
	if plan.Unrecoverable() {
		t.Error("the default scope reports that it takes something unrecoverable; it must not")
	}

	if _, err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	for _, name := range wantRemove {
		if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived a purge that listed it: %v", name, err)
		}
	}
	for _, name := range wantKeep {
		if _, err := os.Lstat(filepath.Join(root, name)); err != nil {
			t.Errorf("%s was removed by the default scope, which promised to keep it: %v", name, err)
		}
	}
}

// --all is the uninstall answer, so it has to leave nothing — including the directory itself.
func TestAllScopeRemovesEverythingAndTheDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "boks")
	fill(t, root, "containerd", "ca", "secrets.json", "policy", "policy-log.jsonl")

	plan, err := Inspect(root, ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Keep) != 0 {
		t.Errorf("--all kept %v; it must keep nothing", names(plan.Keep))
	}
	if !plan.Unrecoverable() {
		t.Error("a plan taking the CA and the credential store does not report itself unrecoverable")
	}
	res, err := Apply(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !res.RootRemoved {
		t.Error("RootRemoved is false after a purge that emptied the directory")
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the state directory survived --all: %v", err)
	}
}

// The safety property that carries the most weight: removal targets come from a fixed list of
// names, never from walking the directory. A file Boks did not write is reported and left,
// and it keeps the directory alive so that --all cannot silently take it with the parent.
func TestFilesBoksDidNotWriteAreNeverRemoved(t *testing.T) {
	root := filepath.Join(t.TempDir(), "boks")
	fill(t, root, "containerd", "ca")
	fill(t, root, "important.txt")
	if err := os.MkdirAll(filepath.Join(root, "someone-elses-dir"), 0o700); err != nil {
		t.Fatal(err)
	}

	plan, err := Inspect(root, ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"important.txt", "someone-elses-dir"}
	if got := names(plan.Unknown); !equal(got, want) {
		t.Fatalf("Unknown = %v, want %v", got, want)
	}
	for _, e := range plan.Unknown {
		if e.Kind != Foreign {
			t.Errorf("%s is classified %v; anything boks did not write must be Foreign", e.Name, e.Kind)
		}
	}
	res, err := Apply(plan)
	if err != nil {
		t.Fatal(err)
	}
	if res.RootRemoved {
		t.Error("the state directory was removed while it still held files boks did not write")
	}
	for _, name := range want {
		if _, err := os.Lstat(filepath.Join(root, name)); err != nil {
			t.Errorf("%s was removed; boks did not write it: %v", name, err)
		}
	}
}

// Root is the guard between a mistyped BOKS_STATE_DIR and somebody's home directory. Each of
// these must be a refusal, and the message must name the reason rather than fail obscurely.
func TestRootRefusesDirectoriesNothingMayPurge(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory on this host: %v", err)
	}
	sep := string(filepath.Separator)
	root := sep
	if runtime.GOOS == "windows" {
		root = filepath.VolumeName(home) + sep
	}

	tests := []struct {
		name string
		dir  string
		want string
	}{
		{"empty", "", "no state directory"},
		{"blank", "   ", "no state directory"},
		{"filesystem root", root, "filesystem root"},
		{"home itself", home, "home directory"},
		{"home's parent", filepath.Dir(home), "home directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Root(tt.dir)
			if err == nil {
				t.Fatalf("Root(%q) = %q with no error; it must refuse", tt.dir, got)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Root(%q) said %q, which does not mention %q", tt.dir, err, tt.want)
			}
		})
	}

	// And the control: a directory that is none of those is accepted, so the refusals
	// above are the guard doing its job rather than the guard refusing everything.
	dir := filepath.Join(t.TempDir(), "boks")
	if _, err := Root(dir); err != nil {
		t.Errorf("Root(%q) refused an ordinary state directory: %v", dir, err)
	}
}

// Inspect must refuse the same directories Root does, rather than only Apply. A plan that
// listed a home directory's contents would already have printed them.
func TestInspectRefusesWhatRootRefuses(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory on this host: %v", err)
	}
	if _, err := Inspect(home, ScopeAll); err == nil {
		t.Fatal("Inspect planned a purge of the home directory")
	}
	if _, err := Inspect("", ScopeAll); !errors.Is(err, ErrNoStateDir) {
		t.Errorf("Inspect(\"\") = %v, want ErrNoStateDir", err)
	}
}

// Apply revalidates rather than trusting the plan it was handed, because a Plan is an
// ordinary struct: nothing stops a caller building one, and the cost of a path outside the
// state directory reaching os.RemoveAll is not recoverable.
func TestApplyRefusesTargetsOutsideTheRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "boks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "keep-me")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		entry Entry
	}{
		{"absolute path elsewhere", Entry{Name: "keep-me", Path: outside}},
		{"parent traversal in the name", Entry{Name: "..", Path: base}},
		{"traversal spelled into the path", Entry{Name: "x", Path: filepath.Join(root, "..", "keep-me")}},
		{"a nested path rather than a name", Entry{Name: "a/b", Path: filepath.Join(root, "a", "b")}},
		{"the root itself", Entry{Name: ".", Path: root}},
		{"no name at all", Entry{Name: "", Path: outside}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Apply(Plan{Root: root, Exists: true, Remove: []Entry{tt.entry}})
			if err == nil {
				t.Fatalf("Apply removed %+v without complaint", tt.entry)
			}
			if !strings.Contains(err.Error(), "refusing") {
				t.Errorf("the error does not read as a refusal: %v", err)
			}
			if _, err := os.Lstat(outside); err != nil {
				t.Errorf("the directory outside the root is gone: %v", err)
			}
		})
	}

	// The control: a legitimate direct child is removed, so the refusals above are not
	// simply Apply refusing everything.
	fill(t, root, "notices")
	plan, err := Inspect(root, ScopeReclaim)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(plan); err != nil {
		t.Fatalf("Apply refused a legitimate plan: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "notices")); !errors.Is(err, os.ErrNotExist) {
		t.Error("notices survived a plan that listed it")
	}
}

// A symlink in the state directory must be measured as the link it is and removed as the link
// it is. Following one would let a link named `containerd` make the plan promise a home
// directory's worth of bytes, and then deliver on it.
func TestSymlinksAreNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink on Windows needs a privilege this test cannot assume")
	}
	base := t.TempDir()
	root := filepath.Join(base, "boks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "elsewhere")
	fill(t, target, "big.txt")
	if err := os.WriteFile(filepath.Join(target, "big.txt"),
		[]byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "containerd")); err != nil {
		t.Fatal(err)
	}

	plan, err := Inspect(root, ScopeReclaim)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Remove) != 1 || !plan.Remove[0].Symlink {
		t.Fatalf("Remove = %+v, want one entry marked as a symlink", plan.Remove)
	}
	if plan.Remove[0].Size >= 4096 {
		t.Errorf("the symlink was measured as %d bytes; it was walked into its target",
			plan.Remove[0].Size)
	}
	if _, err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "containerd")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the symlink itself survived")
	}
	if _, err := os.Lstat(filepath.Join(target, "big.txt")); err != nil {
		t.Errorf("the symlink's target was removed through the link: %v", err)
	}
}

// A state directory that is itself a symlink is legitimate — people move state to another
// disk — but the home-directory refusal has to see through it, or a link is the way around
// every guard in Root.
func TestRootResolvesASymlinkedStateDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink on Windows needs a privilege this test cannot assume")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory on this host: %v", err)
	}
	link := filepath.Join(t.TempDir(), "boks")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Root(link); err == nil {
		t.Fatal("Root accepted a state directory that is a symlink to the home directory")
	} else if !strings.Contains(err.Error(), "home directory") {
		t.Errorf("the refusal does not name the reason: %v", err)
	}
}

// A machine that has never run Boks is not an error, and neither is one whose state directory
// was already removed. `boks purge` after `boks purge` has to exit 0.
func TestMissingStateDirectoryIsNotAnError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "boks")
	plan, err := Inspect(root, ScopeAll)
	if err != nil {
		t.Fatalf("Inspect on a machine with no state failed: %v", err)
	}
	if plan.Exists {
		t.Error("Exists is true for a directory that is not there")
	}
	if !plan.Empty() {
		t.Errorf("Remove = %v for a directory that is not there", names(plan.Remove))
	}
	var out strings.Builder
	plan.Write(&out)
	if !strings.Contains(out.String(), "nothing to purge") {
		t.Errorf("the rendering does not say there is nothing to do:\n%s", out.String())
	}
}

// Sizes are what the user decides on, so they have to be the sizes of the things named.
func TestPlanMeasuresWhatItLists(t *testing.T) {
	root := filepath.Join(t.TempDir(), "boks")
	if err := os.MkdirAll(filepath.Join(root, "containerd", "root"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "containerd", "root", "layer"),
		make([]byte, 3000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secrets.json"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}

	plan, err := Inspect(root, ScopeReclaim)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Freed() != 3000 {
		t.Errorf("Freed = %d, want 3000 — only containerd is in this scope", plan.Freed())
	}
	if plan.Total() != 3100 {
		t.Errorf("Total = %d, want 3100 — kept bytes count towards the total", plan.Total())
	}
	if got := plan.Remove[0].Files; got != 1 {
		t.Errorf("Files = %d, want 1", got)
	}
}

// Boks writes secrets.json and update.json through a temporary file in the same directory, so
// an interrupted process leaves one behind directly under the state directory. Reporting those
// as "not written by boks" would be untrue; taking the credential one under the scope that
// promises to keep credentials would be worse.
func TestInterruptedWritesAreClassifiedNotDisowned(t *testing.T) {
	root := filepath.Join(t.TempDir(), "boks")
	fill(t, root, "update.json.tmp", ".boks-secrets-419273.txt")
	// A file whose name is only *nearly* the prefix must not be claimed.
	fill(t, root, ".boks-secretsomething.txt")

	plan, err := Inspect(root, ScopeReclaim)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(plan.Remove); !equal(got, []string{"update.json.tmp"}) {
		t.Errorf("Remove = %v, want just the stale release check", got)
	}
	if got := names(plan.Keep); !equal(got, []string{".boks-secrets-419273.txt"}) {
		t.Errorf("Keep = %v; a half-written credential store must survive the default scope", got)
	}
	if got := names(plan.Unknown); !equal(got, []string{".boks-secretsomething.txt"}) {
		t.Errorf("Unknown = %v; only the near-miss is boks', er, not boks'", got)
	}

	// The path on a prefix match has to be the file's own name, or Apply refuses it.
	if _, err := Apply(plan); err != nil {
		t.Fatalf("Apply refused a prefix-matched plan: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "update.json.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the stale release check survived")
	}

	// And --all takes the credential leftover with the credentials.
	plan, err = Inspect(root, ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(plan.Remove); !equal(got, []string{".boks-secrets-419273.txt"}) {
		t.Errorf("--all Remove = %v, want the half-written credential store", got)
	}
}

func TestBytesReadsLikeASize(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{105 * 1024 * 1024, "105 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
		{1854427136, "1.7 GiB"}, // the Windows measurement that prompted all of this
	}
	for _, tt := range tests {
		if got := Bytes(tt.n); got != tt.want {
			t.Errorf("Bytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

// Every catalogue entry needs the two things the output is built from, and no duplicates: the
// name is the key Inspect looks entries up by, so a repeat would make one of them unreachable.
func TestCatalogueIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range catalogue {
		if e.Name == "" {
			t.Error("a catalogue entry has no name")
		}
		if e.What == "" {
			t.Errorf("catalogue entry %q has no description; it is what the user decides on", e.Name)
		}
		if e.Kind == Foreign {
			t.Errorf("catalogue entry %q is classified Foreign, so it would never be removed", e.Name)
		}
		if seen[e.Name] {
			t.Errorf("duplicate catalogue entry %q", e.Name)
		}
		seen[e.Name] = true
		if strings.ContainsAny(e.Name, `/\`) {
			t.Errorf("catalogue entry %q is not a plain name directly under the state directory", e.Name)
		}
	}
}
