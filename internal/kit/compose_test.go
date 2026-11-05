package kit

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// writeKit writes a kit directory under root and returns it. files is relative-path -> content
// under files/, and spec.yaml is written from spec.
func writeKit(t *testing.T, root, dir, spec string, files map[string]string) string {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, SpecFileName), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	for p, c := range files {
		full := filepath.Join(d, "files", p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

const hdr = "schemaVersion: \"2\"\n"

func mixin(name, body string) string {
	return hdr + "kind: mixin\nname: " + name + "\n" + body
}

func sandboxKit(name, body string) string {
	return hdr + "kind: sandbox\nname: " + name + "\nsandbox:\n  image: img/" + name + ":1\n  entrypoint: [" + name + "]\n" + body
}

func resolve(t *testing.T, refs ...string) *Composition {
	t.Helper()
	c, err := Resolve(refs, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return c
}

func resolveErr(t *testing.T, want string, refs ...string) {
	t.Helper()
	_, err := Resolve(refs, nil)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Resolve error = %v, want it to contain %q", err, want)
	}
}

func TestStackMergesFieldsInKitOrder(t *testing.T) {
	root := t.TempDir()
	a := writeKit(t, root, "a", mixin("a", `permissions:
  network:
    allow: [one.test, shared.test]
    deny: [bad.test]
environment:
  variables: {X: from-a, ONLY_A: "1"}
setup:
  install: [{command: "echo a"}]
  startup: [{command: [a-daemon]}]
  files: [{path: /etc/a, content: a}]
ports: [{container: 80}]
volumes:
  - path: /cache
    size: 1m
`), map[string]string{"home/.rc": "a-rc", "workspace/f": "a-f"})
	b := writeKit(t, root, "b", mixin("b", `permissions:
  network:
    allow: [two.test, shared.test]
    deny: [worse.test]
environment:
  variables: {X: from-b}
setup:
  install: [{command: "echo b"}]
  startup: [{command: [b-daemon]}]
ports: [{container: 80}, {container: 81}]
volumes:
  - path: /cache
    size: 2m
`), map[string]string{"home/.rc": "b-rc", "workspace/f": "b-f"})

	m := resolve(t, a, b).Merged
	if got := strings.Join(m.Permissions.Network.Allow, " "); got != "one.test shared.test two.test" {
		t.Errorf("allow = %s", got)
	}
	if got := strings.Join(m.Permissions.Network.Deny, " "); got != "bad.test worse.test" {
		t.Errorf("deny = %s", got)
	}
	if m.Environment.Variables["X"] != "from-b" || m.Environment.Variables["ONLY_A"] != "1" {
		t.Errorf("env = %v", m.Environment.Variables)
	}
	if len(m.Setup.Install) != 2 || m.Setup.Install[0].Command != "echo a" || m.Setup.Install[1].Command != "echo b" {
		t.Errorf("install = %v", m.Setup.Install)
	}
	if len(m.Setup.Startup) != 2 || len(m.Setup.Files) != 1 {
		t.Errorf("setup = %+v", m.Setup)
	}
	if len(m.Ports) != 3 {
		t.Errorf("ports = %v", m.Ports)
	}
	if len(m.Volumes) != 1 || m.Volumes[0].Size != "2m" {
		t.Errorf("volumes = %v, want the later /cache", m.Volumes)
	}
	if len(m.Tree.Home) != 1 || string(m.Tree.Home[0].Data) != "b-rc" ||
		len(m.Tree.Workspace) != 1 || string(m.Tree.Workspace[0].Data) != "b-f" {
		t.Errorf("tree = %+v", m.Tree)
	}
	if m.Name != "a,b" || m.Kind != KindMixin {
		t.Errorf("name/kind = %s/%s", m.Name, m.Kind)
	}

	// Order is the merge order: reversed, the other kit wins and installs run the other way.
	r := resolve(t, b, a).Merged
	if r.Environment.Variables["X"] != "from-a" || r.Setup.Install[0].Command != "echo b" ||
		string(r.Tree.Home[0].Data) != "a-rc" {
		t.Errorf("reversed stack did not reverse the winner: %+v", r)
	}
}

func TestStackErrors(t *testing.T) {
	root := t.TempDir()
	cred := "credentials:\n  - service: tok\n    apiKey: {name: T, inject: [{domain: x.test, scheme: bearer}]}\npermissions: {network: {allow: [x.test]}}\n"
	a := writeKit(t, root, "a", mixin("a", cred), nil)
	b := writeKit(t, root, "b", mixin("b", cred), nil)
	a2 := writeKit(t, root, "a2", mixin("a", ""), nil)
	s1 := writeKit(t, root, "s1", sandboxKit("s1", ""), nil)
	s2 := writeKit(t, root, "s2", sandboxKit("s2", ""), nil)

	resolveErr(t, "credential tok defined in both a and b", a, b)
	resolveErr(t, "duplicate kit name a", a, a2)
	resolveErr(t, `kit s2 must be kind "mixin"`, s1, s2)
}

func TestSandboxPlusMixinKeepsTheAgent(t *testing.T) {
	root := t.TempDir()
	s := writeKit(t, root, "s", sandboxKit("s", "environment:\n  variables: {A: s}\n"), nil)
	m := writeKit(t, root, "m", mixin("m", "environment:\n  variables: {A: m}\nagentInstructions: {filename: X.md, content: hi}\n"), nil)
	c := resolve(t, m, s)
	if c.Merged.Kind != KindSandbox || c.Merged.Sandbox == nil || c.Merged.Sandbox.Image != "img/s:1" {
		t.Errorf("merged = %+v", c.Merged)
	}
	if c.Merged.Environment.Variables["A"] != "s" {
		t.Errorf("later sandbox kit should win the variable: %v", c.Merged.Environment.Variables)
	}
	if c.Merged.AgentInstructions != nil {
		t.Errorf("a mixin's agent instructions must not become the agent's: %+v", c.Merged.AgentInstructions)
	}
}

func TestRequiresAgent(t *testing.T) {
	root := t.TempDir()
	m := writeKit(t, root, "m", mixin("m", "requires: {agent: claude}\n"), nil)
	writeKit(t, root, "claude-base", sandboxKit("claude-base", ""), nil)
	child := writeKit(t, root, "child", hdr+"kind: sandbox\nname: child\nextends: ../claude-base\n", nil)

	c := resolve(t, m)
	if err := c.CheckAgent("claude"); err != nil {
		t.Errorf("claude: %v", err)
	}
	if err := c.CheckAgent("claude-vertex"); err == nil || !strings.Contains(err.Error(), `requires agent "claude"`) {
		t.Errorf("claude-vertex: %v", err)
	}
	// A kit-defined agent answers to the names of the kits it extends.
	m2 := writeKit(t, root, "m2", mixin("m2", "requires: {agent: claude-base}\n"), nil)
	c = resolve(t, child, m2)
	if err := c.CheckAgent("child"); err != nil {
		t.Errorf("lineage: %v", err)
	}
	if err := c.CheckAgent("other"); err == nil {
		t.Errorf("an unrelated agent satisfied the mixin")
	}
}

func TestExtendsMergesParentFirst(t *testing.T) {
	root := t.TempDir()
	writeKit(t, root, "base", sandboxKit("base", `permissions: {network: {allow: [base.test]}}
environment: {variables: {V: base, B: base}}
setup: {install: [{command: "echo base"}]}
security: {privileged: true}
`), map[string]string{"home/.x": "base"})
	child := writeKit(t, root, "child", hdr+`kind: sandbox
name: child
extends: ../base
permissions: {network: {allow: [child.test], deny: [base.test]}}
environment: {variables: {V: child}}
setup: {install: [{command: "echo child"}]}
`, map[string]string{"home/.x": "child"})

	c := resolve(t, child)
	if len(c.Members) != 1 {
		t.Fatalf("members = %d", len(c.Members))
	}
	m := c.Merged
	if m.Name != "child" || m.Sandbox == nil || m.Sandbox.Image != "img/base:1" {
		t.Errorf("merged = %+v", m)
	}
	if got := strings.Join(m.Permissions.Network.Allow, " "); got != "base.test child.test" {
		t.Errorf("allow = %s", got)
	}
	if m.Environment.Variables["V"] != "child" || m.Environment.Variables["B"] != "base" {
		t.Errorf("env = %v", m.Environment.Variables)
	}
	if len(m.Setup.Install) != 2 || m.Setup.Install[0].Command != "echo base" {
		t.Errorf("install = %v", m.Setup.Install)
	}
	if !m.Security.Privileged || string(m.Tree.Home[0].Data) != "child" {
		t.Errorf("security/tree = %+v / %+v", m.Security, m.Tree)
	}
	if m.Extends != "" {
		t.Errorf("Extends survived flattening: %q", m.Extends)
	}
}

func TestExtendsErrors(t *testing.T) {
	root := t.TempDir()
	cred := "credentials:\n  - service: tok\n    apiKey: {name: T, inject: [{domain: x.test, scheme: bearer}]}\npermissions: {network: {allow: [x.test]}}\n"
	writeKit(t, root, "withcred", sandboxKit("withcred", cred), nil)
	c1 := writeKit(t, root, "c1", hdr+"kind: sandbox\nname: c1\nextends: ../withcred\n"+cred, nil)
	resolveErr(t, "credential tok is declared by both", c1)

	writeKit(t, root, "mx", mixin("mx", ""), nil)
	c2 := writeKit(t, root, "c2", hdr+"kind: sandbox\nname: c2\nextends: ../mx\n", nil)
	resolveErr(t, "a parent must be kind", c2)

	writeKit(t, root, "ca", hdr+"kind: sandbox\nname: ca\nextends: ../cb\n", nil)
	writeKit(t, root, "cb", hdr+"kind: sandbox\nname: cb\nextends: ../ca\n", nil)
	resolveErr(t, "extends cycle", filepath.Join(root, "ca"))

	writeKit(t, root, "l0", sandboxKit("l0", ""), nil)
	prev := "l0"
	for i := 1; i <= MaxExtendsDepth+1; i++ {
		name := "l" + string(rune('0'+i))
		writeKit(t, root, name, hdr+"kind: sandbox\nname: "+name+"\nextends: ../"+prev+"\n", nil)
		prev = name
	}
	resolveErr(t, "deeper than", filepath.Join(root, prev))
	// Exactly at the cap is fine.
	resolve(t, filepath.Join(root, "l5"))

	writeKit(t, root, "missing", hdr+"kind: sandbox\nname: missing\nextends: ../nowhere\n", nil)
	resolveErr(t, "extends ../nowhere", filepath.Join(root, "missing"))
}

func TestLockedPathsCannotBeSetByADescendant(t *testing.T) {
	root := t.TempDir()
	writeKit(t, root, "base", sandboxKit("base", "locked: [permissions.network]\npermissions: {network: {allow: [a.test]}}\n"), nil)
	bad := writeKit(t, root, "bad", hdr+"kind: sandbox\nname: bad\nextends: ../base\npermissions: {network: {allow: [b.test]}}\n", nil)
	resolveErr(t, "which an ancestor locks", bad)
	ok := writeKit(t, root, "ok", hdr+"kind: sandbox\nname: ok\nextends: ../base\nenvironment: {variables: {A: b}}\n", nil)
	resolve(t, ok)
}

func TestMixinsFieldIsFollowed(t *testing.T) {
	root := t.TempDir()
	writeKit(t, root, "base", sandboxKit("base", "mixins: [../mx1]\n"), nil)
	writeKit(t, root, "mx1", mixin("mx1", "setup: {install: [{command: one}]}\n"), nil)
	writeKit(t, root, "mx2", mixin("mx2", "setup: {install: [{command: two}]}\n"), nil)
	top := writeKit(t, root, "top", hdr+"kind: sandbox\nname: top\nextends: ../base\nmixins: [../mx2]\n", nil)
	extra := writeKit(t, root, "extra", mixin("extra", "setup: {install: [{command: three}]}\n"), nil)

	c := resolve(t, top, extra)
	var names []string
	for _, m := range c.Members {
		names = append(names, m.Spec.Name)
	}
	if got := strings.Join(names, " "); got != "top mx1 mx2 extra" {
		t.Errorf("members = %s", got)
	}
	var cmds []string
	for _, i := range c.Merged.Setup.Install {
		cmds = append(cmds, i.Command)
	}
	if got := strings.Join(cmds, " "); got != "one two three" {
		t.Errorf("install order = %s", got)
	}

	writeKit(t, root, "notmixin", sandboxKit("notmixin", ""), nil)
	bad := writeKit(t, root, "bad", sandboxKit("bad", "mixins: [../notmixin]\n"), nil)
	resolveErr(t, `must be kind "mixin"`, bad)
}

func TestFetchedKitCannotNameALocalPath(t *testing.T) {
	loader := func(ref string) (*Spec, []string, error) {
		return ParseSpec([]byte(sandboxKit("remote", "mixins: [./local]\n")))
	}
	_, err := Resolve([]string{"git+https://example.test/r.git"}, loader)
	if err == nil || !strings.Contains(err.Error(), "may only refer to other git+ references") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanGolden(t *testing.T) {
	root := t.TempDir()
	base := writeKit(t, root, "agent", sandboxKit("agent", `permissions: {network: {allow: [agent.test]}}
environment: {variables: {MODE: base}}
setup:
  install: [{command: "apt-get install -y jq"}]
  startup: [{command: [agentd], background: true}]
`), map[string]string{"home/.agentrc": "x", "workspace/AGENTS.md": "agent"})
	m1 := writeKit(t, root, "tools", mixin("tools", `permissions: {network: {allow: [tools.test], deny: [telemetry.test]}}
environment: {variables: {MODE: tools}}
setup:
  install: [{command: "curl -fsSL tools.test | sh", user: agent}]
  files: [{path: /etc/tools.conf, content: x, onlyIfMissing: true}]
`), map[string]string{"home/.agentrc": "tools", "workspace/AGENTS.md": "tools"})
	m2 := writeKit(t, root, "lint", mixin("lint", `setup:
  install: [{command: "pip install lint"}]
  startup: [{command: [lintd, --watch]}]
`), nil)

	c := resolve(t, base, m1, m2)
	got := RenderPlan(Plan(c.Members))
	golden := filepath.Join("testdata", "plan.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("plan differs from %s (go test -update):\n%s", golden, got)
	}
}
