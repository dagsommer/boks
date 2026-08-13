package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The generated reference has to describe the CLI that exists, and a generated file that
// nobody regenerates is worse than a hand-written one, because it looks authoritative. This
// test is the thing that makes `make docs` non-optional: rename a flag and it fails, naming
// the command that fixes it.
func TestReferenceIsCurrent(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "cli.md")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(got) != Reference() {
		t.Errorf("docs/cli.md is out of date with the command tree.\n" +
			"It is generated from internal/cli/reference.go — run `make docs` and commit the result.")
	}
}

// Properties of the rendering itself, so that a change to the generator that silently drops
// half the tree is caught by something other than a reader noticing.
func TestReferenceDescribesTheWholeTree(t *testing.T) {
	ref := Reference()

	// One heading per command, at the depth the command sits at.
	for _, want := range []string{
		"\n## boks run\n",
		"\n## boks doctor\n",
		"\n## boks ports\n",
		"\n## boks policy\n",
		"\n### boks policy allow\n",
		"\n### boks policy profile\n",
		"\n#### boks policy profile create\n",
		"\n## boks secret\n",
		"\n### boks secret set\n",
		"\n### boks secret services\n",
		"\n## boks net\n",
		"\n## boks ca\n",
	} {
		if !strings.Contains(ref, want) {
			t.Errorf("the reference has no heading %q", strings.TrimSpace(want))
		}
	}

	// Flags a reader comes looking for, spelled the way the CLI spells them.
	for _, want := range []string{
		"`-t`, `--template string`",
		"`--rm`",
		"`-m`, `--memory string`",
		"`--no-secrets`",
		"`-p`, `--publish stringArray`",
	} {
		if !strings.Contains(ref, want) {
			t.Errorf("the reference does not document %s", want)
		}
	}

	// The development flags are hidden from --help; documenting them would present an
	// escape hatch as an interface. The root command's own long help mentions them, which
	// is where the explanation belongs.
	for _, hidden := range []string{
		"| `--i-know-this-is-not-isolated`",
		"| `--snapshotter string`",
		"| `--containerd-address string`",
	} {
		if strings.Contains(ref, hidden) {
			t.Errorf("a hidden development flag reached the reference as a documented one: %s", hidden)
		}
	}

	// cobra's `help` command is cobra's, not this project's.
	if strings.Contains(ref, "\n## boks help\n") {
		t.Error("cobra's own help command is documented as if it were part of the CLI")
	}
}

// A table cell that contains an unescaped pipe eats the rest of the row, which turns a flag
// table into a wrong flag table rather than a broken-looking one.
func TestReferenceTableCellsAreIntact(t *testing.T) {
	for _, line := range strings.Split(Reference(), "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		// Three columns means four delimiters: leading, two internal, trailing.
		bare := strings.Count(line, "|") - strings.Count(line, "\\|")
		if bare != 4 {
			t.Errorf("flag row has %d unescaped pipes, want 4: %s", bare, line)
		}
	}
}
