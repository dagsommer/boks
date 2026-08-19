package kit

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The v1 grammar is a different language that happens to describe the same thing, and this file
// asserts the translation into the v2 model field by field.
//
// It exists because a mutation proved it had to. Deleting the line that carries v1's
// `network.allowedDomains` into `permissions.network.allow` — silently dropping every allow
// rule a v1 kit declares — passed the entire suite. The v1 fixtures were being parsed and
// nothing was reading what came out, so the translation was covered in the sense that it ran
// and in no sense that mattered.
//
// The mapping asserted here is the one in Docker's own v1→v2 table (the reference's "What
// changed in v2", and topics/v1-migration.md in docker/sbx-kits-contrib). Where a row of that
// table has no test below, it is because the field is not yet modelled — see the end of this
// file.

// loadV1 parses a fixture from testdata/v1 and fails the test if it does not translate.
func loadV1(t *testing.T, name string) *Spec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "v1", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	spec, _, err := ParseSpec(data)
	if err != nil {
		t.Fatalf("%s did not translate: %v", name, err)
	}
	return spec
}

// TestV1NetworkRulesSurviveTranslation is the test the mutation demanded. A kit's network rules
// are the reason the sandbox is a sandbox; losing them in translation would not fail loudly, it
// would produce a kit that quietly permits or forbids the wrong things.
func TestV1NetworkRulesSurviveTranslation(t *testing.T) {
	spec := loadV1(t, "sample-agent.yaml")

	if spec.Permissions == nil || spec.Permissions.Network == nil {
		t.Fatal("permissions.network is nil: v1 network.allowedDomains/deniedDomains " +
			"were dropped entirely")
	}
	net := spec.Permissions.Network

	if !slices.Contains(net.Allow, "sample-agent.example.com") {
		t.Errorf("Allow = %v, want it to carry v1's network.allowedDomains", net.Allow)
	}
	if !slices.Contains(net.Deny, "telemetry.sample-agent.example.com") {
		t.Errorf("Deny = %v, want it to carry v1's network.deniedDomains", net.Deny)
	}

	// Allow and deny must not be conflated. A denied domain that arrived in the allow list
	// would invert the author's intent, which is worse than dropping it.
	if slices.Contains(net.Allow, "telemetry.sample-agent.example.com") {
		t.Errorf("Allow = %v contains a domain v1 DENIED", net.Allow)
	}
	if slices.Contains(net.Deny, "sample-agent.example.com") {
		t.Errorf("Deny = %v contains a domain v1 ALLOWED", net.Deny)
	}
}

// The renames that carry an agent's identity. Each of these is a row of the v1→v2 table, and a
// missed one means the agent runs the wrong program or reads no instructions.
func TestV1RenamesAreApplied(t *testing.T) {
	spec := loadV1(t, "sample-agent.yaml")

	// kind: agent → kind: sandbox. This fixture says `sandbox`, which v1 also allows, so
	// what is asserted is that whatever v1 said, v2's spelling comes out.
	if spec.Kind != KindSandbox {
		t.Errorf("Kind = %q, want %q", spec.Kind, KindSandbox)
	}
	if spec.Sandbox == nil {
		t.Fatal("sandbox block is nil after translation")
	}

	// sandbox.entrypoint.run → sandbox.entrypoint, and .args → command.default.
	if got, want := spec.Sandbox.Entrypoint, []string{"sample-bin", "--verbose"}; !slices.Equal(got, want) {
		t.Errorf("Entrypoint = %v, want %v (v1 sandbox.entrypoint.run)", got, want)
	}
	if got, want := spec.Sandbox.Command.Default, []string{"--task-mode"}; !slices.Equal(got, want) {
		t.Errorf("Command.Default = %v, want %v (v1 sandbox.entrypoint.args)", got, want)
	}

	// agentContext → agentInstructions.content, sandbox.aiFilename → .filename.
	if spec.AgentInstructions == nil {
		t.Fatal("agentInstructions is nil: v1 agentContext was dropped")
	}
	if spec.AgentInstructions.Content == "" {
		t.Error("agentInstructions.content is empty, want v1's agentContext")
	}
	if got, want := spec.AgentInstructions.Filename, "SAMPLE.md"; got != want {
		t.Errorf("agentInstructions.filename = %q, want %q (v1 sandbox.aiFilename)", got, want)
	}
}

// commands: → setup:, with the privilege difference intact. install runs once as uid 0 and
// startup runs on every start as uid 1000; a translation that lost `background` or `user` would
// change what runs as root, which is the one mistake here worth catching early.
func TestV1SetupTranslation(t *testing.T) {
	spec := loadV1(t, "sample-agent.yaml")

	if spec.Setup == nil {
		t.Fatal("setup is nil: v1 commands: was dropped")
	}
	if len(spec.Setup.Install) != 1 {
		t.Fatalf("Setup.Install has %d entries, want 1", len(spec.Setup.Install))
	}
	if len(spec.Setup.Startup) != 1 {
		t.Fatalf("Setup.Startup has %d entries, want 1", len(spec.Setup.Startup))
	}
	if got := spec.Setup.Install[0].User; got != "0" {
		t.Errorf("install[0].User = %q, want \"0\" — the privilege must survive", got)
	}
	if got := spec.Setup.Startup[0].User; got != "1000" {
		t.Errorf("startup[0].User = %q, want \"1000\"", got)
	}
	if !spec.Setup.Startup[0].Background {
		t.Error("startup[0].Background = false, want true — a background command that " +
			"runs in the foreground blocks the sandbox from starting")
	}
}

// A v1 spec must not be judged by v2's strictness. v1 fields are legal in a v1 document, and a
// decoder that applied the v2 field set to both would reject every v1 kit in existence.
func TestV1FieldsAreLegalInAV1Document(t *testing.T) {
	doc := `schemaVersion: "1"
kind: agent
name: legacy
sandbox:
  image: example/image:latest
network:
  allowedDomains: [example.com]
agentContext: "instructions"
`
	spec, _, err := ParseSpec([]byte(doc))
	if err != nil {
		t.Fatalf("a v1 document was rejected: %v", err)
	}
	if spec.Kind != KindSandbox {
		t.Errorf("Kind = %q, want kind: agent to become %q", spec.Kind, KindSandbox)
	}

	// The control: the same fields in a v2 document must still be refused, or the fork on
	// schemaVersion is not a fork at all. TestV2RejectsLegacyV1Fields covers this in
	// detail; this asserts the two halves disagree, which is the point of forking.
	v2 := `schemaVersion: "2"
kind: sandbox
name: legacy
sandbox:
  image: example/image:latest
network:
  allowedDomains: [example.com]
`
	if _, _, err := ParseSpec([]byte(v2)); err == nil {
		t.Error("a v1 field was accepted in a v2 document: the loader is not forking on " +
			"schemaVersion")
	}
}

// Rows of the v1→v2 table with no assertion above, recorded rather than left to be discovered:
//
//   - credentials.sources.<id> → credentials[].service, network.serviceDomains/serviceAuth →
//     credentials[].apiKey.inject, standalone oauth: → credentials[].oauth,
//     environment.proxyManaged → credentials[].apiKey.proxyManaged. The credential model is
//     the largest part of the translation and the part that touches secrets; it needs fixtures
//     of its own and a decision about how it meets internal/secret, which is not made yet.
//   - network.publishedPorts → top-level ports. Present in sample-agent.yaml and unasserted.
//   - tmpfs: and the volumes mapping form → volumes: sequence.
//   - settings:/kitDir/persistence, which v2 removes: a v1 kit setting them should say they
//     are gone rather than ignore them silently, and today nothing checks either way.
//
// None of these is claimed to work.
