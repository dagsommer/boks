package sandbox

import (
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
