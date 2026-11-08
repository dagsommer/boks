package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPlanOrdersInstallThenFilesThenStartup(t *testing.T) {
	s := &Setup{
		Startup: []StartupStep{{Command: []string{"daemon", "--x"}, Background: true}},
		Files:   []FileStep{{Path: "/etc/a", Content: "w=${WORKDIR}", Mode: "0600"}},
		Install: []InstallStep{{Command: "apt-get install -y jq"}, {Command: "true", User: "1000"}},
	}
	ops, err := s.Plan("/work/p", "")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, op := range ops {
		kinds = append(kinds, strings.Fields(op.label)[0])
	}
	if got := strings.Join(kinds, ","); got != "install,install,write,startup" {
		t.Fatalf("order = %s", got)
	}
	if ops[0].user != "root" || ops[1].user != "1000" || ops[3].user != "agent" {
		t.Fatalf("users = %q %q %q", ops[0].user, ops[1].user, ops[3].user)
	}
	if ops[2].stdin != "w=/work/p" {
		t.Fatalf("WORKDIR not expanded: %q", ops[2].stdin)
	}
	if !ops[3].perBoot || ops[0].perBoot {
		t.Fatal("only startup runs per boot")
	}
	if ops[3].argv[0] != "/bin/sh" || ops[3].argv[len(ops[3].argv)-1] != "--x" {
		t.Fatalf("background argv = %v", ops[3].argv)
	}
}

func TestPlanRefusesFilesInsideTheClone(t *testing.T) {
	s := &Setup{Files: []FileStep{{Path: "/work/p/.env", Content: "x"}}}
	if _, err := s.Plan("/work/p", "/work/p"); err == nil {
		t.Fatal("a file inside the clone was accepted")
	}
	if _, err := s.Plan("/work/p", ""); err != nil {
		t.Fatalf("without a clone it is fine: %v", err)
	}
	other := &Setup{Files: []FileStep{{Path: "/work/pq/x", Content: "x"}}}
	if _, err := other.Plan("/work/p", "/work/p"); err != nil {
		t.Fatalf("a sibling prefix is not inside: %v", err)
	}
}

func TestPlanRejectsBadSteps(t *testing.T) {
	for name, s := range map[string]*Setup{
		"relative file": {Files: []FileStep{{Path: "etc/a"}}},
		"bad mode":      {Files: []FileStep{{Path: "/a", Mode: "rwx"}}},
		"empty install": {Install: []InstallStep{{Command: " "}}},
		"empty startup": {Startup: []StartupStep{{}}},
	} {
		if _, err := s.Plan("/w", ""); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestResolveUser(t *testing.T) {
	for in, want := range map[string]string{"root": "0:0", "0": "0:0", "agent": "501:20", "1000": "501:20", "": "501:20", "7:8": "7:8"} {
		if got := resolveUser(in, "501:20"); got != want {
			t.Errorf("resolveUser(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFingerprintChangesWithTheSetup(t *testing.T) {
	a := &Setup{Install: []InstallStep{{Command: "a"}}}
	b := &Setup{Install: []InstallStep{{Command: "b"}}}
	if a.fingerprint() == b.fingerprint() || a.fingerprint() != a.fingerprint() {
		t.Fatal("fingerprint is not a function of the content")
	}
}

func TestStaticFilesComeFirstAndLast(t *testing.T) {
	s := &Setup{
		Install:   []InstallStep{{Command: "true"}},
		Home:      []StaticFile{{Path: ".a/b", Mode: 0o600, Data: []byte("h")}},
		Workspace: []StaticFile{{Path: ".c", Mode: 0o644, Data: []byte("w")}},
		Startup:   []StartupStep{{Command: []string{"x"}}},
	}
	ops, err := s.Plan("/w", "")
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, op := range ops {
		labels = append(labels, op.label)
	}
	want := "files/home/.a/b|install 1: true|startup 1: x|files/workspace/.c"
	if got := strings.Join(labels, "|"); got != want {
		t.Fatalf("order = %s", got)
	}
	if _, err := (&Setup{Workspace: s.Workspace}).Plan("", ""); err == nil {
		t.Fatal("workspace files without a workspace were accepted")
	}
}

func TestStaticFileScriptWritesAndKeeps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script runs under /bin/sh in the guest; there is none on Windows")
	}
	base := t.TempDir()
	run := func(rel, content, keep string) {
		t.Helper()
		owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
		cmd := exec.Command("/bin/sh", "-c", staticFileScript, "sh", base, rel, "0640", keep, owner)
		cmd.Stdin = strings.NewReader(content)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	run("a b/c/f.txt", "one", "0")
	got, err := os.ReadFile(filepath.Join(base, "a b/c/f.txt"))
	if err != nil || string(got) != "one" {
		t.Fatalf("%q %v", got, err)
	}
	if fi, _ := os.Stat(filepath.Join(base, "a b/c/f.txt")); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", fi.Mode())
	}
	run("a b/c/f.txt", "two", "1")
	if got, _ := os.ReadFile(filepath.Join(base, "a b/c/f.txt")); string(got) != "one" {
		t.Fatalf("keep overwrote: %q", got)
	}
	run("a b/c/f.txt", "three", "0")
	if got, _ := os.ReadFile(filepath.Join(base, "a b/c/f.txt")); string(got) != "three" {
		t.Fatalf("overwrite failed: %q", got)
	}
}

func TestPlanKeepsAKitsInstallOutputInTheGuest(t *testing.T) {
	s := &Setup{Install: []InstallStep{{Command: "make", Kit: "my kit", N: 2}}}
	ops, err := s.Plan("", "")
	if err != nil || len(ops) != 1 {
		t.Fatalf("ops = %v, err = %v", ops, err)
	}
	got := strings.Join(ops[0].argv, "\x00")
	if !strings.Contains(got, "/var/log/sbx-kits/my_kit/install-2.log") || !strings.HasSuffix(got, "make") {
		t.Errorf("argv = %q", ops[0].argv)
	}
	plain := &Setup{Install: []InstallStep{{Command: "make", Kit: "my kit", N: 2}}}
	other := &Setup{Install: []InstallStep{{Command: "make"}}}
	if plain.fingerprint() != other.fingerprint() {
		t.Error("the log path changed the setup fingerprint, which would re-run installs on existing sandboxes")
	}
}
