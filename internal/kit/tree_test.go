package kit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadTree(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, data string, mode os.FileMode) {
		p := filepath.Join(dir, "files", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("home/.config/x/y.toml", "a", 0o600)
	write("workspace/.editorconfig", "b", 0o644)
	write("other/z", "c", 0o644)
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "files", "home", "pw")); err != nil {
		t.Fatal(err)
	}

	tree, warnings, err := ReadTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Home) != 1 || tree.Home[0].Path != ".config/x/y.toml" || tree.Home[0].Mode != 0o600 || string(tree.Home[0].Data) != "a" {
		t.Fatalf("home = %+v", tree.Home)
	}
	if len(tree.Workspace) != 1 || tree.Workspace[0].Path != ".editorconfig" {
		t.Fatalf("workspace = %+v", tree.Workspace)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "files/other") || !strings.Contains(joined, "symlink") {
		t.Fatalf("warnings = %q", joined)
	}
}

func TestReadTreeAbsent(t *testing.T) {
	tree, w, err := ReadTree(t.TempDir())
	if tree != nil || w != nil || err != nil {
		t.Fatalf("%v %v %v", tree, w, err)
	}
}
