package release

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoFile reads a file relative to the repository root.
func repoFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", ".."}, parts...)...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(raw)
}

// makefileTargets reads RELEASE_TARGETS out of the Makefile.
func makefileTargets(t *testing.T) []string {
	t.Helper()
	mk := repoFile(t, "Makefile")
	re := regexp.MustCompile(`(?m)^RELEASE_TARGETS\s*:=\s*(.*)$`)
	m := re.FindStringSubmatch(mk)
	if m == nil {
		t.Fatal("no RELEASE_TARGETS assignment in the Makefile — if it was renamed, this " +
			"test has to be renamed with it rather than deleted")
	}
	return strings.Fields(m[1])
}

// workflowTargets reads the goos/goarch pairs out of release.yml's build matrix.
//
// Parsed with a regexp rather than a YAML library. The pairing is positional in the file —
// `- goos: X` immediately followed by `goarch: Y` — and reading it that way keeps this test
// free of a dependency whose only user would be this test.
func workflowTargets(t *testing.T) []string {
	t.Helper()
	wf := repoFile(t, ".github", "workflows", "release.yml")
	re := regexp.MustCompile(`-\s*goos:\s*(\S+)\s*\n\s*goarch:\s*(\S+)`)
	matches := re.FindAllStringSubmatch(wf, -1)
	if len(matches) == 0 {
		t.Fatal("no `- goos:`/`goarch:` pairs found in release.yml; the matrix shape changed " +
			"and this test no longer reads it")
	}
	var out []string
	for _, m := range matches {
		out = append(out, m[1]+"/"+m[2])
	}
	return out
}

// `make dist` exists so that a release can be reproduced without GitHub Actions. It cannot do
// that if it builds a different set of platforms than the release does, and the difference is
// invisible short of comparing two directories — so it is asserted instead.
func TestMakefileAndWorkflowShipTheSamePlatforms(t *testing.T) {
	mk := makefileTargets(t)
	wf := workflowTargets(t)
	sort.Strings(mk)
	sort.Strings(wf)

	if strings.Join(mk, " ") != strings.Join(wf, " ") {
		t.Errorf("`make dist` and the release workflow build different platforms.\n"+
			"  Makefile RELEASE_TARGETS: %s\n"+
			"  release.yml build matrix: %s\n"+
			"Change both, or a release cannot be reproduced locally.",
			strings.Join(mk, " "), strings.Join(wf, " "))
	}
}

// A platform is shipped because it was measured, and docs/verification.md is where the
// measurement lives. This does not check that the evidence is *good* — no test can — only
// that a platform cannot be added to the release without the evidence file mentioning it at
// all, which is the cheap half and the half that catches an optimistic edit.
func TestEveryShippedPlatformAppearsInTheEvidence(t *testing.T) {
	evidence := strings.ToLower(repoFile(t, "docs", "verification.md"))
	// verification.md calls darwin "macOS"; every other GOOS it calls by its own name.
	names := map[string]string{"darwin": "macos"}
	for _, target := range makefileTargets(t) {
		goos, _, ok := strings.Cut(target, "/")
		if !ok {
			t.Errorf("RELEASE_TARGETS entry %q is not goos/goarch", target)
			continue
		}
		name := names[goos]
		if name == "" {
			name = goos
		}
		if !strings.Contains(evidence, name) {
			t.Errorf("release ships %s but docs/verification.md never mentions %q — a "+
				"platform is shipped because it was measured", target, name)
		}
	}
}
