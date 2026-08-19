package kit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The kits under testdata/contrib are verbatim copies of the spec.yaml files of every kit in
// Docker's sbx-kits-contrib repository at the time this package was written. They are the only
// evidence available that the grammar implemented here matches the grammar real kits are written
// in, so they are copied into the repository rather than read from wherever they were downloaded:
// a test that depends on a path outside the module is a test that stops running.
//
// Any kit in this corpus failing to parse is a bug in this package until proven otherwise. That
// is the whole premise, and it is why this test names the file and quotes the error rather than
// just counting failures.

const contribDir = "testdata/contrib"

// contribKits returns the corpus, failing if it is missing or suspiciously small. The count
// check is what stops this test from silently becoming vacuous: without it, a testdata directory
// that got emptied would turn "every real kit parses" into "zero kits parsed, all fine".
func contribKits(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(contribDir)
	if err != nil {
		t.Fatalf("reading the contrib corpus: %v", err)
	}

	var specs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(contribDir, e.Name(), SpecFileName)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s has no %s", e.Name(), SpecFileName)
			continue
		}
		specs = append(specs, p)
	}

	// 35 kits were copied in. A lower bound rather than an equality so that adding a kit to
	// the corpus does not require editing this number, but a wholesale loss does fail.
	const minKits = 35
	if len(specs) < minKits {
		t.Fatalf("found %d kits in %s, want at least %d — the corpus looks incomplete, "+
			"and every assertion below would pass vacuously", len(specs), contribDir, minKits)
	}
	return specs
}

// TestContribKitsParse asserts that every real kit in the corpus decodes and validates.
func TestContribKitsParse(t *testing.T) {
	for _, path := range contribKits(t) {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}

			spec, warnings, err := ParseSpec(data)
			if err != nil {
				t.Fatalf("ParseSpec(%s) failed: %v", path, err)
			}

			// Every kit in this corpus is schemaVersion "2". If one ever is not, the v1
			// path is being exercised here by accident and the v1 tests are not the only
			// place it runs — worth knowing rather than passing quietly.
			if spec.SchemaVersion != SchemaVersion2 {
				t.Errorf("schemaVersion = %q, want %q — this corpus is expected to be "+
					"all v2", spec.SchemaVersion, SchemaVersion2)
			}

			// Assert the parse actually populated something rather than returning a
			// zero-value Spec that trivially validates. Name and kind are required, so
			// they cannot be empty on a successful parse; checking them here is what
			// makes this more than a "did not error" test.
			if spec.Name == "" {
				t.Error("Name is empty after a successful parse")
			}
			if spec.Kind != KindSandbox && spec.Kind != KindMixin {
				t.Errorf("Kind = %q, want %q or %q", spec.Kind, KindSandbox, KindMixin)
			}
			// The corpus was surveyed when it was copied in: all 35 kits set
			// displayName and description. Asserted so that a decode bug which
			// silently dropped a top-level string field would be caught, which a
			// name-and-kind-only check would miss.
			if spec.DisplayName == "" {
				t.Error("DisplayName is empty, but every kit in this corpus sets it")
			}
			if spec.Description == "" {
				t.Error("Description is empty, but every kit in this corpus sets it")
			}

			// A v2 spec should not produce translation warnings; the only warnings it can
			// legitimately produce are the accepted-but-inert ones from warnV2.
			for _, w := range warnings {
				if strings.Contains(w, "deprecated") || strings.Contains(w, "dropped") {
					t.Errorf("unexpected translation warning on a v2 spec: %s", w)
				}
			}

			// Scheme expansion must have run: nothing downstream reads Scheme.
			for i, c := range spec.Credentials {
				for j, inj := range c.APIKey.injectOrNil() {
					if inj.Scheme != "" {
						t.Errorf("credentials[%d].apiKey.inject[%d].scheme = %q "+
							"survived ParseSpec", i, j, inj.Scheme)
					}
				}
			}
		})
	}
}

// injectOrNil returns k's inject list, tolerating a nil receiver so callers iterating
// credentials do not need a nil check per entry.
func (k *APIKey) injectOrNil() []APIKeyInject {
	if k == nil {
		return nil
	}
	return k.Inject
}

// TestContribKitsCoverExpectedFields pins the specific kits that exercise the fields most likely
// to be decoded wrongly, so that a regression in one of them is reported as a missing feature
// rather than as an unexplained drop in coverage.
//
// Each expectation below was read out of the corpus by hand before being written down, and the
// comment says what was observed. Without this test, TestContribKitsParse would still pass if a
// field like sandbox.command or ports silently decoded to nothing — "it parsed" is not "it
// parsed correctly".
func TestContribKitsCoverExpectedFields(t *testing.T) {
	load := func(t *testing.T, kit string) *Spec {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(contribDir, kit, SpecFileName))
		if err != nil {
			t.Fatalf("reading kit %s: %v", kit, err)
		}
		s, _, err := ParseSpec(data)
		if err != nil {
			t.Fatalf("ParseSpec(%s): %v", kit, err)
		}
		return s
	}

	// crush is the only kit in the corpus that uses sandbox.command, and it uses the mapping
	// form with a default tail of ["--yolo"] and no interactive key.
	t.Run("crush/command", func(t *testing.T) {
		s := load(t, "crush")
		if got, want := s.Sandbox.Command.Default, []string{"--yolo"}; !equalStrings(got, want) {
			t.Errorf("sandbox.command.default = %v, want %v", got, want)
		}
		if s.Sandbox.Command.Interactive != nil {
			t.Errorf("sandbox.command.interactive = %v, want nil (the key is absent, so "+
				"it must fall back to default)", s.Sandbox.Command.Interactive)
		}
		if got, want := s.Sandbox.Command.InteractiveTail(), []string{"--yolo"}; !equalStrings(got, want) {
			t.Errorf("InteractiveTail() = %v, want the default tail %v", got, want)
		}
	})

	// droid is the only kit in the corpus with an oauth block. It carries skipIfEnv, which
	// must decode rather than being rejected as a v1 field, and it declares both apiKey and
	// oauth on one credential.
	t.Run("droid/oauth", func(t *testing.T) {
		s := load(t, "droid")
		var c *Credential
		for i := range s.Credentials {
			if s.Credentials[i].Service == "droid" {
				c = &s.Credentials[i]
			}
		}
		if c == nil {
			t.Fatal("no credential with service \"droid\"")
		}
		if c.OAuth == nil {
			t.Fatal("credential has no oauth block")
		}
		if got, want := c.OAuth.TokenEndpoint.Host, "api.workos.com"; got != want {
			t.Errorf("oauth.tokenEndpoint.host = %q, want %q", got, want)
		}
		if got, want := c.OAuth.SkipIfEnv, []string{"FACTORY_API_KEY"}; !equalStrings(got, want) {
			t.Errorf("oauth.skipIfEnv = %v, want %v — the field must be accepted in a "+
				"v2 spec, not rejected as legacy", got, want)
		}
		if c.APIKey == nil || c.APIKey.Name != "FACTORY_API_KEY" {
			t.Errorf("apiKey.name = %v, want FACTORY_API_KEY on the same credential as "+
				"oauth", c.APIKey)
		}
		if !c.APIKey.ProxyManaged {
			t.Error("apiKey.proxyManaged = false, want true")
		}
		if len(c.APIKey.Inject) != 3 {
			t.Errorf("apiKey.inject has %d entries, want 3", len(c.APIKey.Inject))
		}
	})

	// code-server declares a port with an explicit protocol and name; the corpus survey found
	// 6 kits with ports and only 2 of them setting protocol.
	t.Run("code-server/ports", func(t *testing.T) {
		s := load(t, "code-server")
		if len(s.Ports) != 1 {
			t.Fatalf("ports has %d entries, want 1", len(s.Ports))
		}
		p := s.Ports[0]
		if p.Container != 8080 || p.Protocol != "tcp" || p.Name != "code-server" {
			t.Errorf("ports[0] = %+v, want {Container:8080 Protocol:tcp Name:code-server}", p)
		}
	})

	// paperclip declares a port with NO protocol, which must stay empty rather than being
	// defaulted to "tcp" in the model.
	t.Run("paperclip/port-without-protocol", func(t *testing.T) {
		s := load(t, "paperclip")
		if len(s.Ports) == 0 {
			t.Fatal("ports is empty")
		}
		if s.Ports[0].Protocol != "" {
			t.Errorf("ports[0].protocol = %q, want \"\" — an omitted protocol is not "+
				"rewritten in the model", s.Ports[0].Protocol)
		}
	})

	// 6 kits declare requires.agent, all of them mixins. claude-acp is one.
	t.Run("claude-acp/requires", func(t *testing.T) {
		s := load(t, "claude-acp")
		if s.Kind != KindMixin {
			t.Fatalf("kind = %q, want %q", s.Kind, KindMixin)
		}
		if s.Requires == nil || s.Requires.Agent == "" {
			t.Fatalf("requires.agent is unset, want a base-agent name")
		}
	})

}

// warnings2 adapts a []string back to warnings so tests can use its has helper.
func warnings2(w []string) warnings { return warnings(w) }

// equalStrings compares two string slices, treating nil and empty as different so that the
// nil-vs-empty distinction on Command.Interactive can be asserted.
func equalStrings(a, b []string) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
