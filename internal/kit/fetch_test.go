package kit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The fragment grammar, which decides which commit is fetched and which directory is read.
// Getting `dir` wrong reads the wrong kit; getting `ref` wrong runs the wrong code.
func TestParseGitRef(t *testing.T) {
	for _, tc := range []struct {
		name, ref             string
		wantURL, wantR, wantD string
		wantErr               string
	}{
		{
			name:    "url only, default branch",
			ref:     "git+https://example.com/org/repo.git",
			wantURL: "https://example.com/org/repo.git",
		},
		{
			name:    "ref and dir",
			ref:     "git+https://example.com/org/repo.git#ref=v1.2.3&dir=vale",
			wantURL: "https://example.com/org/repo.git", wantR: "v1.2.3", wantD: "vale",
		},
		{
			name:    "ssh keeps its scheme",
			ref:     "git+ssh://git@example.com/org/repo.git#ref=main",
			wantURL: "ssh://git@example.com/org/repo.git", wantR: "main",
		},
		{
			name:    "encoded dir",
			ref:     "git+https://example.com/repo.git#dir=kits%2Fvale",
			wantURL: "https://example.com/repo.git", wantD: "kits/vale",
		},
		// A dir that climbs out of the clone would read a file that is not in the
		// repository at all. The clone is a temp directory this process made; nothing in
		// a reference should be able to point outside it.
		{
			name:    "dir cannot escape",
			ref:     "git+https://example.com/repo.git#dir=../../etc",
			wantErr: "leaves the repository",
		},
		{
			name:    "unknown fragment key is refused",
			ref:     "git+https://example.com/repo.git#branch=main",
			wantErr: "unknown fragment key",
		},
		// A URL beginning with a dash would be read by git as a flag rather than a
		// repository.
		{
			name:    "url cannot begin with a dash",
			ref:     "git+--upload-pack=evil",
			wantErr: "cannot begin with",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := parseGitRef(tc.ref)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parseGitRef(%q) succeeded, want an error", tc.ref)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGitRef(%q): %v", tc.ref, err)
			}
			if g.URL != tc.wantURL || g.Ref != tc.wantR || g.Dir != tc.wantD {
				t.Errorf("= {URL:%q Ref:%q Dir:%q}, want {%q %q %q}",
					g.URL, g.Ref, g.Dir, tc.wantURL, tc.wantR, tc.wantD)
			}
		})
	}
}

// Only a full SHA is a fixed commit. A tag looks pinned and is not — it can be moved — which
// is the distinction the warning on a fetched kit rests on.
func TestGitRefImmutable(t *testing.T) {
	for ref, want := range map[string]bool{
		"a1b2c3d4e5f67890abcdef1234567890abcdef12": true,
		"A1B2C3D4E5F67890ABCDEF1234567890ABCDEF12": true,
		"v1.2.3":  false,
		"main":    false,
		"":        false,
		"a1b2c3d": false, // short SHA: names one commit today, ambiguous tomorrow
		"a1b2c3d4e5f67890abcdef1234567890abcdef1":   false, // 39 characters
		"a1b2c3d4e5f67890abcdef1234567890abcdef123": false, // 41
	} {
		if got := (gitRef{Ref: ref}).Immutable(); got != want {
			t.Errorf("Immutable(%q) = %v, want %v", ref, got, want)
		}
	}
}

// The real thing, against a repository made on disk, so it exercises fetch, checkout, the
// subdirectory and the reported commit without reaching the network.
func TestFetchGitFromALocalRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	kitDir := filepath.Join(repo, "vale")
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := `schemaVersion: "2"
kind: mixin
name: vale
permissions:
  network:
    allow: [objects.githubusercontent.com]
`
	if err := os.WriteFile(filepath.Join(kitDir, SpecFileName), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet", "-b", "main"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "add", "."},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "--quiet", "-m", "kit"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.TrimSpace(string(head))

	t.Run("default branch, with the subdirectory", func(t *testing.T) {
		s, warnings, err := Load("git+file://" + repo + "#dir=vale")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if s.Name != "vale" {
			t.Errorf("Name = %q", s.Name)
		}
		// The commit has to be reported whatever the ref was: "which kit ran" has one
		// answer and a branch name is not it.
		if !containsSubstring(warnings, commit) {
			t.Errorf("warnings %v do not report the commit %s", warnings, commit)
		}
		// A mutable ref must say so, and say what to pin it to.
		if !containsSubstring(warnings, "not a fixed commit") {
			t.Errorf("warnings %v do not flag the mutable ref", warnings)
		}
	})

	t.Run("a full SHA is fetched and warns about nothing", func(t *testing.T) {
		_, warnings, err := Load("git+file://" + repo + "#ref=" + commit + "&dir=vale")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if containsSubstring(warnings, "not a fixed commit") {
			t.Errorf("a full SHA was reported as mutable: %v", warnings)
		}
	})

	t.Run("a missing spec names the directory it looked in", func(t *testing.T) {
		_, _, err := Load("git+file://" + repo + "#dir=nope")
		if err == nil {
			t.Fatal("a directory with no spec.yaml was accepted")
		}
		if !strings.Contains(err.Error(), "nope") {
			t.Errorf("error %q does not name the directory", err)
		}
	})

	t.Run("an unknown ref fails with git's own reason", func(t *testing.T) {
		_, _, err := Load("git+file://" + repo + "#ref=no-such-branch")
		if err == nil {
			t.Fatal("an unknown ref was accepted")
		}
		if !strings.Contains(err.Error(), "no-such-branch") {
			t.Errorf("error %q does not name the ref", err)
		}
	})
}

func containsSubstring(all []string, want string) bool {
	for _, s := range all {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
