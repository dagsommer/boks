package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The state directory is shortStateDir's and not t.TempDir()'s throughout this file, because
// t.TempDir() embeds the TEST NAME in the path and the names here are long. A sandbox's link
// socket lives under the state directory and must fit the 104-byte sun_path limit; on the
// Windows runner `TestPolicyLsShowsTheKitLayer` alone pushed it to 105 and every test in this
// file failed on a path length rather than on anything it was testing. See shortStateDir.

// writeKit puts a kit on disk and returns the directory to point --kit at.
func writeKit(t *testing.T, spec string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const netKit = `schemaVersion: "2"
kind: sandbox
name: netkit
sandbox:
  image: example/image:latest
permissions:
  network:
    allow: [api.netkit.test]
    deny: [telemetry.netkit.test]
`

// `boks policy ls --kit` has to show the kit's rules, labelled with the kit, in the same table
// as every other layer. Without that a kit is a file that grants network access and cannot be
// audited without running it.
func TestPolicyLsShowsTheKitLayer(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", shortStateDir(t))
	dir := writeKit(t, netKit)

	out, _, err := runCLI(t, "", "policy", "ls", "--kit", dir)
	if err != nil {
		t.Fatalf("policy ls --kit: %v\n%s", err, out)
	}
	for _, want := range []string{"kit netkit", "api.netkit.test", "telemetry.netkit.test"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

// The flag has to change the answer, which is a different claim from "the flag is accepted".
// `policy check` is where a user asks whether one destination is reachable, so it is where a
// kit either takes effect or silently does not.
func TestPolicyCheckHonoursTheKit(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", shortStateDir(t))
	dir := writeKit(t, netKit)

	// The control first: without the kit, the kit's destination is not reachable. If this
	// ever starts passing, the test below proves nothing.
	before, _, err := runCLI(t, "", "policy", "check", "api.netkit.test:443")
	if err == nil && strings.Contains(before, "allowed") {
		t.Fatalf("control failed: api.netkit.test is reachable with no kit applied:\n%s", before)
	}

	after, _, err := runCLI(t, "", "policy", "check", "--kit", dir, "api.netkit.test:443")
	if err != nil {
		t.Fatalf("policy check --kit: %v\n%s", err, after)
	}
	if !strings.Contains(after, "allowed") {
		t.Errorf("the kit's own destination is not reachable with the kit applied:\n%s", after)
	}
	if !strings.Contains(after, "kit netkit") {
		t.Errorf("the verdict does not name the kit as the source:\n%s", after)
	}
}

// A kit cannot widen past a deny, end to end through the CLI. internal/policy asserts this on
// the resolver; this asserts that nothing between the flag and the engine undoes it.
func TestKitCannotOverrideAFlagDeny(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", shortStateDir(t))
	dir := writeKit(t, netKit)

	out, _, err := runCLI(t, "", "policy", "check", "--kit", dir,
		"--deny", "api.netkit.test", "api.netkit.test:443")
	if err != nil && out == "" {
		t.Fatalf("policy check: %v", err)
	}
	if strings.Contains(out, "allowed") {
		t.Errorf("a kit's allow beat an explicit --deny:\n%s", out)
	}
}

// A reference Boks cannot fetch must be refused where the user typed it, not deep inside a
// sandbox creation. The message has to name the form and say the refusal is deliberate.
func TestKitRemoteReferenceIsRefused(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", shortStateDir(t))

	_, _, err := runCLI(t, "", "policy", "ls", "--kit", "oci://ghcr.io/org/kit@sha256:abc")
	if err == nil {
		t.Fatal("an OCI reference was accepted")
	}
	for _, want := range []string{"OCI", "pinned"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// A kit that does not parse must fail before anything is built, naming the file.
func TestKitInvalidSpecIsRefused(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", shortStateDir(t))
	dir := writeKit(t, "schemaVersion: \"2\"\nkind: sandbox\nname: broken\nnetwork:\n  allowedDomains: [x]\n")

	_, _, err := runCLI(t, "", "policy", "ls", "--kit", dir)
	if err == nil {
		t.Fatal("a v1 field in a v2 spec was accepted")
	}
	if !strings.Contains(err.Error(), "spec.yaml") {
		t.Errorf("error %q does not name the file", err)
	}
}
