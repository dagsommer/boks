package cli

import (
	"os"
	"path/filepath"
	"regexp"
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

// The reference must be the same on every machine, or the check above fails for everyone
// except whoever generated it last. It failed exactly this way the first time it ran in CI:
// two defaults are derived from the environment, and a runner's home directory is not a
// contributor's. Both the generic path and the reader's benefit point the same way — nobody
// needs to know the generator's home directory.
func TestReferenceIsTheSameOnEveryMachine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)         // Unix
	t.Setenv("USERPROFILE", home)  // Windows
	t.Setenv("XDG_STATE_HOME", "") // an unset XDG variable and a set one
	elsewhere := Reference()       // must both come out the same
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	if Reference() != elsewhere {
		t.Error("the reference changes when XDG_STATE_HOME does; a path from the " +
			"generator's environment has reached the document")
	}

	// XDG is only how Linux answers the question. macOS puts the state directory under
	// ~/Library/Application Support and Windows under %LocalAppData%, neither of which is a
	// prefix this generator would otherwise recognise — so the reference came out different
	// on those machines and the check that it is current failed there while the command tree
	// was perfectly fine. BOKS_STATE_DIR reproduces that shape on any host, which is what
	// makes this regression catchable on the Linux box where the file is generated rather
	// than only on the platform that suffers from it.
	t.Setenv("BOKS_STATE_DIR", filepath.Join(t.TempDir(), "AppData", "Local", "boks"))
	if Reference() != elsewhere {
		t.Error("the reference changes when the state directory moves out from under the " +
			"home and XDG paths, as it does on macOS and Windows; docs/cli.md is one " +
			"committed file and must render the same on every platform")
	}

	for _, leak := range []string{home, "/home/", "/Users/", "/root/"} {
		if strings.Contains(elsewhere, leak) {
			t.Errorf("the reference contains %q, which is a path from the machine that "+
				"generated it rather than one a reader has", leak)
		}
	}
	if !strings.Contains(elsewhere, "~/.local/state/boks/") {
		t.Error("the decision log's default is no longer written as a path under ~")
	}
}

// Help text is full of <placeholder> notation, and Markdown reads a run of angle brackets as
// raw HTML. The rendered page turned "The sandbox is named <agent>-<workspace directory> and
// persists" into "The sandbox is named  and persists", and the flag table's `--name` row lost
// everything after "(default: ". Both said something different from the command, which is the
// failure this whole generator exists to prevent — and neither was visible in the Markdown.
func TestReferenceKeepsPlaceholdersAsText(t *testing.T) {
	ref := Reference()

	for _, want := range []string{
		"&lt;agent&gt;-&lt;workspace directory&gt;",
		"BOKS_CA_CERT_B64=&lt;base64 certificate&gt;",
	} {
		if !strings.Contains(ref, want) {
			t.Errorf("the reference does not carry %q as text", want)
		}
	}

	// Outside a fence, and outside an inline code span, no bare angle bracket survives to be
	// read as a tag. Inside either it is already literal.
	inline := regexp.MustCompile("`[^`]*`")
	fence := false
	for i, line := range strings.Split(ref, "\n") {
		if strings.HasPrefix(line, "```") {
			fence = !fence
			continue
		}
		if fence {
			continue
		}
		if prose := inline.ReplaceAllString(line, ""); strings.ContainsAny(prose, "<>") {
			t.Errorf("line %d has a bare angle bracket outside a fence, which Markdown "+
				"will read as a tag: %s", i+1, line)
		}
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
