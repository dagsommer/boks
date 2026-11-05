package cli

import (
	"strings"
	"testing"
)

const stackMixinA = `schemaVersion: "2"
kind: mixin
name: tool-a
permissions:
  network:
    allow: [a.stack.test, shared.stack.test]
environment:
  variables: {WHO: a}
setup:
  install: [{command: "echo a"}]
`

const stackMixinB = `schemaVersion: "2"
kind: mixin
name: tool-b
permissions:
  network:
    allow: [b.stack.test]
    deny: [shared.stack.test]
environment:
  variables: {WHO: b}
setup:
  install: [{command: "echo b"}]
`

// --kit repeats, each kit's rules enter the policy labelled with that kit, and a deny in one
// kit beats an allow in another.
func TestStackedKitsShapeThePolicy(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", shortStateDir(t))
	a, b := writeKit(t, stackMixinA), writeKit(t, stackMixinB)

	out, _, err := runCLI(t, "", "policy", "ls", "--kit", a, "--kit", b)
	if err != nil {
		t.Fatalf("policy ls: %v\n%s", err, out)
	}
	for _, want := range []string{"kit:tool-a,tool-b", "declared by kit tool-a", "declared by kit tool-b"} {
		if !strings.Contains(out, want) && !strings.Contains(out, strings.TrimPrefix(want, "kit:")) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}

	out, _, err = runCLI(t, "", "policy", "check", "--kit", a, "--kit", b, "shared.stack.test:443")
	if err != nil {
		t.Fatalf("policy check: %v", err)
	}
	if !strings.HasPrefix(out, "DENY") {
		t.Errorf("tool-b's deny did not beat tool-a's allow:\n%s", out)
	}
	out, _, _ = runCLI(t, "", "policy", "check", "--kit", a, "--kit", b, "b.stack.test:443")
	if !strings.HasPrefix(out, "ALLOW") || !strings.Contains(out, "kit tool-a,tool-b") {
		t.Errorf("b.stack.test should be allowed, scoped to the whole stack:\n%s", out)
	}
}

func TestStackedKitsRefuseTwoAgents(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", shortStateDir(t))
	_, _, err := runCLI(t, "", "policy", "ls", "--kit", writeKit(t, netKit), "--kit", writeKit(t, strings.Replace(netKit, "netkit", "other", 1)))
	if err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Fatalf("err = %v", err)
	}
}

func TestMixinRequiresTheActualAgent(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", shortStateDir(t))
	dir := writeKit(t, strings.Replace(stackMixinA, "environment:", "requires: {agent: claude}\nenvironment:", 1))
	if _, _, err := runCLI(t, "", "policy", "ls", "--kit", dir, "--agent", "claude"); err != nil {
		t.Errorf("claude: %v", err)
	}
	_, _, err := runCLI(t, "", "policy", "ls", "--kit", dir, "--agent", "codex")
	if err == nil || !strings.Contains(err.Error(), `requires agent "claude"`) {
		t.Errorf("codex: %v", err)
	}
}

func TestKitInspectResolved(t *testing.T) {
	a, b := writeKit(t, stackMixinA), writeKit(t, stackMixinB)

	out, _, err := runCLI(t, "", "kit", "inspect", a)
	if err != nil || !strings.Contains(out, "name: tool-a") || strings.Contains(out, "execution plan") {
		t.Fatalf("inspect: %v\n%s", err, out)
	}

	out, _, err = runCLI(t, "", "kit", "inspect", "--resolved", a, b)
	if err != nil {
		t.Fatalf("inspect --resolved: %v\n%s", err, out)
	}
	for _, want := range []string{
		"1. tool-a (mixin)", "2. tool-b (mixin)", "name: tool-a,tool-b",
		"execution plan:", "[tool-a] env WHO=a (overridden by tool-b)", "[tool-b] deny shared.stack.test",
		"[tool-a] run as root: echo a",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "echo a") > strings.Index(out, "echo b") {
		t.Errorf("install order is not kit order:\n%s", out)
	}
}

// Credentials of stacked kits are approved per kit: the unit list the gate walks names each.
func TestKitUnitsFollowTheComposition(t *testing.T) {
	a, b := writeKit(t, stackMixinA), writeKit(t, stackMixinB)
	f := &policyFlags{kitRefs: []string{a, b}}
	if err := f.loadKit(&strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	units := f.kitUnits()
	if len(units) != 2 || units[0].name != "tool-a" || units[0].ref != a || units[1].name != "tool-b" {
		t.Errorf("units = %+v", units)
	}
	if f.kitSpec.Name != "tool-a,tool-b" {
		t.Errorf("merged name = %q", f.kitSpec.Name)
	}
}

// One kit's remembered approval does not cover another kit's credentials, even for the same
// request: the key is the kit's name and reference.
func TestApprovalIsPerKitInAStack(t *testing.T) {
	f, stderr := kitFlags(t)
	units := []kitUnit{{name: "tool-a", ref: "./a"}, {name: "tool-b", ref: "./b"}}
	asks, _ := kitCredentialAsks(testKitSpecs, nil)
	approvals, _ := loadKitApprovals(kitApprovalsPath())
	approvals.record("tool-a\x00./a", asks[0])
	if err := approvals.save(); err != nil {
		t.Fatal(err)
	}
	a, _ := f.gateKitCredentials(units[0], testKitSpecs, nil, stderr)
	b, _ := f.gateKitCredentials(units[1], testKitSpecs, nil, stderr)
	if len(a) != 1 || len(b) != 0 {
		t.Errorf("tool-a got %v, tool-b got %v; want one and none", a, b)
	}
}
