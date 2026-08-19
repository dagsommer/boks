package kit

import (
	"strings"
	"testing"
)

// minimalV2 is a v2 sandbox spec that parses and validates. Tests that want to check one
// specific rejection build on it by appending or substituting, so that a failure is
// attributable to the thing under test rather than to some other missing field.
const minimalV2 = `schemaVersion: "2"
kind: sandbox
name: minimal
sandbox:
  image: example/image:latest
`

// minimalV2Mixin is the mixin equivalent: a mixin must NOT have a sandbox block.
const minimalV2Mixin = `schemaVersion: "2"
kind: mixin
name: minimal-mixin
`

// mustParse fails the test if the spec does not parse, and returns the result.
func mustParse(t *testing.T, doc string) (*Spec, []string) {
	t.Helper()
	s, w, err := ParseSpec([]byte(doc))
	if err != nil {
		t.Fatalf("ParseSpec: unexpected error: %v\nspec:\n%s", err, doc)
	}
	return s, w
}

// mustNotParse fails the test if the spec parses, or if it fails for a reason whose message
// does not contain wantSubstr.
//
// Checking the message and not merely the failure is the point. A test that asserts only "an
// error happened" passes when the spec is rejected for the wrong reason — a typo in the fixture,
// say — which is how a rejection test ends up proving nothing about the rule it names. Each
// wantSubstr below is chosen to be the part of the message that identifies the specific rule.
func mustNotParse(t *testing.T, doc, wantSubstr string) error {
	t.Helper()
	s, _, err := ParseSpec([]byte(doc))
	if err == nil {
		t.Fatalf("ParseSpec: expected an error containing %q, got a valid spec %+v\nspec:\n%s",
			wantSubstr, s, doc)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("ParseSpec error = %q, want it to contain %q\nspec:\n%s", err, wantSubstr, doc)
	}
	return err
}

// TestMinimalV2Parses is the baseline the rejection tests below depend on. Without it, a fixture
// that is broken for an unrelated reason would make every rejection test pass for free.
func TestMinimalV2Parses(t *testing.T) {
	s, w := mustParse(t, minimalV2)
	if s.Name != "minimal" || s.Kind != KindSandbox {
		t.Errorf("parsed spec = %+v, want name=minimal kind=sandbox", s)
	}
	if s.Sandbox == nil || s.Sandbox.Image != "example/image:latest" {
		t.Errorf("sandbox = %+v, want image example/image:latest", s.Sandbox)
	}
	if len(w) != 0 {
		t.Errorf("warnings = %v, want none for a clean minimal spec", w)
	}

	s, w = mustParse(t, minimalV2Mixin)
	if s.Kind != KindMixin || s.Sandbox != nil {
		t.Errorf("parsed mixin = %+v, want kind=mixin and no sandbox block", s)
	}
	if len(w) != 0 {
		t.Errorf("warnings = %v, want none", w)
	}
}

// TestV2RejectsLegacyV1Fields is the central strictness requirement: a v1 field name in a
// schemaVersion "2" spec must be a hard decode error, not a silently ignored key and not a fold
// into the v2 model.
//
// Every legacy surface from the published v1-to-v2 change table is listed, so that the test is a
// checklist of that table rather than a sample of it. The expected substring is yaml.v3's
// unknown-field wording, which names the offending key — that is what makes the error actionable.
func TestV2RejectsLegacyV1Fields(t *testing.T) {
	// Each case is the legacy YAML appended to a v2 spec, and the key that must be named in
	// the resulting error.
	cases := []struct {
		name    string
		legacy  string
		wantKey string
	}{
		{"agent block", "agent:\n  image: x\n", "agent"},
		{"network block", "network:\n  allowedDomains: [a.example.com]\n", "network"},
		{"commands block", "commands:\n  install:\n    - command: \"true\"\n", "commands"},
		{"memory", "memory: |\n  context\n", "memory"},
		{"agentContext", "agentContext: |\n  context\n", "agentContext"},
		{"caps draft block", "caps:\n  network:\n    allow: [a.example.com]\n", "caps"},
		{"settings block", "settings:\n  containerSettings:\n    x: true\n", "settings"},
		{"tmpfs block", "tmpfs:\n  /tmp/scratch: 512m\n", "tmpfs"},
		{"kitDir", "kitDir: /somewhere\n", "kitDir"},
		{"persistence", "persistence: keep\n", "persistence"},
		{"publishedPorts", "publishedPorts:\n  - container: 8080\n", "publishedPorts"},
		{"standalone oauth", "oauth:\n  service: x\n  tokenEndpoint:\n    host: h\n    path: /p\n", "oauth"},
		{"secrets", "secrets: [TOKEN]\n", "secrets"},
		{"egress", "egress:\n  a.example.com: allow\n", "egress"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// "not valid in schemaVersion" rather than yaml.v3's own "not found in
			// type kit.Spec": what is asserted is that the field is refused and the
			// author is told why, not the decoder's phrasing, which this package
			// rewrites precisely so it can change without breaking anyone.
			err := mustNotParse(t, minimalV2+tc.legacy, "not valid in schemaVersion")
			// The key itself must appear, or the author is told only that "a" field is
			// unknown and has to find which.
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("error %q does not name the offending key %q", err, tc.wantKey)
			}
		})
	}
}

// TestV2RejectsLegacyNestedFields covers the v1 fields that lived INSIDE a block whose name v2
// kept, so the unknown-field error is raised on the nested struct rather than at the top level.
// These are the ones a top-level-only check would miss.
func TestV2RejectsLegacyNestedFields(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		wantKey string
	}{
		{
			// v1 put the AI profile filename inside the sandbox block; v2 moved it to
			// the top-level agentInstructions.
			name: "sandbox.aiFilename",
			doc: `schemaVersion: "2"
kind: sandbox
name: minimal
sandbox:
  image: example/image:latest
  aiFilename: AGENTS.md
`,
			wantKey: "aiFilename",
		},
		{
			// v1's entrypoint was a mapping with run/args/ttyArgs/pipeMode; v2's is a
			// flat array, so the mapping fails on its keys.
			name: "sandbox.entrypoint.run mapping",
			doc: `schemaVersion: "2"
kind: sandbox
name: minimal
sandbox:
  image: example/image:latest
  entrypoint:
    run: [agent]
    args: ["-l"]
    ttyArgs: []
`,
			wantKey: "cannot unmarshal",
		},
		{
			// v1 had environment.proxyManaged as a list of variable names; v2 moved the
			// flag onto each credential's apiKey.
			name: "environment.proxyManaged",
			doc: `schemaVersion: "2"
kind: sandbox
name: minimal
sandbox:
  image: example/image:latest
environment:
  proxyManaged:
    - SOME_TOKEN
`,
			wantKey: "proxyManaged",
		},
		{
			// v1's resources took an integer memoryMB; v2 takes a byte-size string.
			name: "sandbox.resources.memoryMB",
			doc: `schemaVersion: "2"
kind: sandbox
name: minimal
sandbox:
  image: example/image:latest
  resources:
    memoryMB: 4096
`,
			wantKey: "memoryMB",
		},
		{
			// v1's credentials was a mapping with sources: under it; v2's is a sequence,
			// so the mapping form is a type error rather than an unknown-field error.
			name: "credentials.sources mapping form",
			doc: `schemaVersion: "2"
kind: sandbox
name: minimal
sandbox:
  image: example/image:latest
credentials:
  sources:
    example:
      env: [EXAMPLE_API_KEY]
`,
			wantKey: "cannot unmarshal",
		},
		{
			// v1's volumes was a mapping of path to size; v2's is a sequence. Same
			// shared-key/different-kind situation as credentials, which is why the
			// migration guide calls this row manual rather than mechanical.
			name: "volumes mapping form",
			doc: `schemaVersion: "2"
kind: sandbox
name: minimal
sandbox:
  image: example/image:latest
volumes:
  /scratch: 512m
`,
			wantKey: "cannot unmarshal",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustNotParse(t, tc.doc, tc.wantKey)
		})
	}
}

// TestV2RejectsTypos checks that an ordinary misspelling is rejected too, not just the known v1
// names. Strictness is worth having mainly for this case: a typo in a block name would otherwise
// silently disable the whole block.
func TestV2RejectsTypos(t *testing.T) {
	for _, doc := range []string{
		minimalV2 + "permisions:\n  network:\n    allow: [a.example.com]\n",
		minimalV2 + "permissions:\n  netwrok:\n    allow: [a.example.com]\n",
		minimalV2 + "credenta:\n  - service: x\n",
	} {
		mustNotParse(t, doc, "not valid in schemaVersion")
	}
}

// TestCommandMappingRejectsUnknownKey guards the yaml.v3 strictness hole that commandMappingKeys
// exists for: a custom UnmarshalYAML calling node.Decode does not inherit KnownFields(true), so
// without the hand-written key check an unknown key inside sandbox.command would be silently
// dropped while the identical typo one level up is a hard error.
//
// This test is the only thing standing between that hole and a silent regression, because the
// struct passed to node.Decode LOOKS strict.
func TestCommandMappingRejectsUnknownKey(t *testing.T) {
	doc := `schemaVersion: "2"
kind: sandbox
name: minimal
sandbox:
  image: example/image:latest
  command:
    default: ["-l"]
    interactve: []
`
	err := mustNotParse(t, doc, "interactve")
	if !strings.Contains(err.Error(), "sandbox.command") {
		t.Errorf("error %q should say which block the unknown key is in", err)
	}
}

// TestCommandForms covers the polymorphic sandbox.command field in all three shapes, including
// the nil-vs-empty distinction on interactive that decides whether the fallback applies.
func TestCommandForms(t *testing.T) {
	tests := []struct {
		name            string
		yaml            string
		wantDefault     []string
		wantInteractive []string
		wantTail        []string
	}{
		{
			// List shorthand: the whole list is the default tail, and interactive stays
			// nil so it falls back.
			name:            "list shorthand",
			yaml:            "  command: [\"-l\", \"--fast\"]\n",
			wantDefault:     []string{"-l", "--fast"},
			wantInteractive: nil,
			wantTail:        []string{"-l", "--fast"},
		},
		{
			name:            "mapping with both",
			yaml:            "  command:\n    default: [\"-l\"]\n    interactive: [\"-i\"]\n",
			wantDefault:     []string{"-l"},
			wantInteractive: []string{"-i"},
			wantTail:        []string{"-i"},
		},
		{
			// An explicitly empty interactive is NOT the same as an absent one: it means
			// "no arguments in interactive mode" and must not fall back to default.
			// Getting this wrong would silently add the default flags to TTY sessions.
			name:            "mapping with explicitly empty interactive",
			yaml:            "  command:\n    default: [\"-l\"]\n    interactive: []\n",
			wantDefault:     []string{"-l"},
			wantInteractive: []string{},
			wantTail:        []string{},
		},
		{
			name:            "mapping with default only",
			yaml:            "  command:\n    default: [\"-l\"]\n",
			wantDefault:     []string{"-l"},
			wantInteractive: nil,
			wantTail:        []string{"-l"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := "schemaVersion: \"2\"\nkind: sandbox\nname: minimal\nsandbox:\n" +
				"  image: example/image:latest\n" + tc.yaml
			s, _ := mustParse(t, doc)
			c := s.Sandbox.Command
			if !equalStrings(c.Default, tc.wantDefault) {
				t.Errorf("Default = %#v, want %#v", c.Default, tc.wantDefault)
			}
			if !equalStrings(c.Interactive, tc.wantInteractive) {
				t.Errorf("Interactive = %#v, want %#v", c.Interactive, tc.wantInteractive)
			}
			if !equalStrings(c.InteractiveTail(), tc.wantTail) {
				t.Errorf("InteractiveTail() = %#v, want %#v", c.InteractiveTail(), tc.wantTail)
			}
		})
	}
}

// TestCommandRejectsScalar checks the default branch of Command.UnmarshalYAML. A bare string is
// the plausible wrong guess for this field, so the error has to say what the two legal forms are.
func TestCommandRejectsScalar(t *testing.T) {
	doc := minimalV2 + "" // sandbox block already present; append a scalar command to it
	doc = strings.Replace(doc, "  image: example/image:latest\n",
		"  image: example/image:latest\n  command: \"--yolo\"\n", 1)
	mustNotParse(t, doc, "must be a list")
}

// TestEmptyAndMultiDocument covers the two whole-document shapes that yaml.v3 handles in ways a
// caller would not expect: an empty document surfaces as a bare "EOF", and a second document is
// silently discarded.
func TestEmptyAndMultiDocument(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		// Verified: yaml.v3 returns io.EOF here, whose message alone is just "EOF".
		mustNotParse(t, "", "empty")
	})

	t.Run("comments only", func(t *testing.T) {
		mustNotParse(t, "# nothing but a comment\n", "empty")
	})

	t.Run("second document rejected", func(t *testing.T) {
		// A single Decode call reads only the first document, so without an explicit
		// check the second would vanish. This package rejects it; Docker's reference
		// implementation does not.
		mustNotParse(t, minimalV2+"---\n"+minimalV2, "more than one YAML document")
	})
}

// TestSchemaVersionErrors checks that a wrong or missing version names the version that was
// found. "unsupported schemaVersion" without the value leaves the author guessing which of their
// files is at fault.
func TestSchemaVersionErrors(t *testing.T) {
	t.Run("unsupported version is named", func(t *testing.T) {
		err := mustNotParse(t, "schemaVersion: \"3\"\nkind: sandbox\nname: x\n", "\"3\"")
		if !strings.Contains(err.Error(), "1, 2") {
			t.Errorf("error %q should list the supported versions", err)
		}
	})

	t.Run("missing version is named as empty", func(t *testing.T) {
		// An absent schemaVersion peeks as "", which is not a supported version. The
		// error has to distinguish this from a wrong value.
		mustNotParse(t, "kind: sandbox\nname: x\n", "schemaVersion \"\" is not supported")
	})

	t.Run("unquoted version is accepted", func(t *testing.T) {
		// Every kit in Docker's own contrib set writes `schemaVersion: "2"`, quoted, and
		// so does every example in the reference. That is the form to write. It is NOT
		// the only form that loads, and this test exists to stop someone "tightening"
		// that into a rejection on the strength of the docs alone.
		//
		// Measured, because the opposite was assumed here first and was wrong:
		// yaml.v3 coerces an unquoted scalar into a string field silently —
		// `schemaVersion: 2` yields "2" with a nil error, and `2.0` yields "2.0". There
		// is no type error to catch and nothing to report.
		//
		// Being lenient is also the safer direction for the goal of loading any kit that
		// Docker loads. sbx is a Go program, so its loader most likely coerces exactly
		// the same way and accepts this too — inferred, not verified — and a kit that
		// works there and is refused here is the failure that matters. The reverse, a
		// kit written unquoted against Boks and then refused by Docker, is caught the
		// first time it is used with Docker and costs one pair of quotes.
		spec, _ := mustParse(t, "schemaVersion: 2\nkind: sandbox\nname: x\n"+
			"sandbox:\n  image: example/image:latest\n")
		if spec.SchemaVersion != "2" {
			t.Errorf("SchemaVersion = %q, want \"2\" — an unquoted scalar must coerce",
				spec.SchemaVersion)
		}
	})

	t.Run("a version that is not 1 or 2 is still refused", func(t *testing.T) {
		// The control for the leniency above: coercion must not turn every scalar into
		// an accepted version. 2.0 is a float, coerces to "2.0", and is not a version.
		mustNotParse(t, "schemaVersion: 2.0\nkind: sandbox\nname: x\n", "\"2.0\"")
	})
}

// TestSchemeExpansion covers the apiKey.inject scheme sugar, including the two asymmetries
// between bearer and basic that are easy to implement backwards.
func TestSchemeExpansion(t *testing.T) {
	base := `schemaVersion: "2"
kind: sandbox
name: minimal
sandbox:
  image: example/image:latest
credentials:
  - service: example
    apiKey:
      name: EXAMPLE_TOKEN
      inject:
`

	t.Run("bearer supplies header and format", func(t *testing.T) {
		s, _ := mustParse(t, base+"        - domain: api.example.com\n          scheme: bearer\n")
		inj := s.Credentials[0].APIKey.Inject[0]
		if inj.Header != "Authorization" {
			t.Errorf("header = %q, want Authorization", inj.Header)
		}
		if inj.Format != "Bearer %s" {
			t.Errorf("format = %q, want \"Bearer %%s\"", inj.Format)
		}
		if inj.Scheme != "" {
			t.Errorf("scheme = %q, want it cleared after expansion", inj.Scheme)
		}
	})

	t.Run("explicit header beats bearer default", func(t *testing.T) {
		// bearer supplies Authorization only when header was left empty, so an author can
		// send a Bearer-formatted value to a non-standard header.
		s, _ := mustParse(t, base+
			"        - domain: api.example.com\n          scheme: bearer\n          header: X-Auth\n")
		inj := s.Credentials[0].APIKey.Inject[0]
		if inj.Header != "X-Auth" {
			t.Errorf("header = %q, want the explicit X-Auth to win over the bearer default",
				inj.Header)
		}
		if inj.Format != "Bearer %s" {
			t.Errorf("format = %q, want \"Bearer %%s\"", inj.Format)
		}
	})

	t.Run("basic sets no header", func(t *testing.T) {
		// basic is username-driven at the proxy rather than a header encoding, so unlike
		// bearer it must NOT invent a header.
		s, _ := mustParse(t, base+
			"        - domain: github.com\n          scheme: basic\n          username: x-access-token\n")
		inj := s.Credentials[0].APIKey.Inject[0]
		if inj.Header != "" {
			t.Errorf("header = %q, want empty: basic auth is not a header encoding", inj.Header)
		}
		if inj.Format != "%s" {
			t.Errorf("format = %q, want \"%%s\" as the placeholder", inj.Format)
		}
		if inj.Username != "x-access-token" {
			t.Errorf("username = %q, want x-access-token", inj.Username)
		}
	})

	t.Run("basic without username is rejected", func(t *testing.T) {
		mustNotParse(t, base+"        - domain: github.com\n          scheme: basic\n",
			"requires a username")
	})

	t.Run("bearer with username is rejected", func(t *testing.T) {
		mustNotParse(t, base+
			"        - domain: api.example.com\n          scheme: bearer\n          username: nobody\n",
			"username is not valid with scheme")
	})

	t.Run("scheme and format together are rejected", func(t *testing.T) {
		mustNotParse(t, base+
			"        - domain: api.example.com\n          scheme: bearer\n          format: \"Token %s\"\n",
			"mutually exclusive")
	})

	t.Run("unknown scheme is rejected", func(t *testing.T) {
		err := mustNotParse(t, base+
			"        - domain: api.example.com\n          scheme: digest\n", "unknown scheme")
		// The error must name the legal alternatives, or the author has to go read the
		// spec to find out what they are.
		if !strings.Contains(err.Error(), SchemeBasic) || !strings.Contains(err.Error(), SchemeBearer) {
			t.Errorf("error %q should list the accepted schemes", err)
		}
	})
}

// TestResolvedResponseFields covers the OAuth response-field defaulting, including the nil
// receiver path that exists so callers do not need a nil check.
func TestResolvedResponseFields(t *testing.T) {
	var nilOAuth *OAuth
	if got := nilOAuth.ResolvedResponseFields(); got.AccessToken != "access_token" {
		t.Errorf("nil OAuth: accessToken = %q, want the RFC default access_token", got.AccessToken)
	}

	o := &OAuth{ResponseFields: &OAuthResponseFields{AccessToken: "accessToken"}}
	got := o.ResolvedResponseFields()
	if got.AccessToken != "accessToken" {
		t.Errorf("accessToken = %q, want the override accessToken", got.AccessToken)
	}
	// The fields left empty must still get their defaults, not be blanked out by the
	// presence of an override on a sibling.
	if got.RefreshToken != "refresh_token" {
		t.Errorf("refreshToken = %q, want the RFC default refresh_token", got.RefreshToken)
	}
	if got.ExpiresIn != "expires_in" {
		t.Errorf("expiresIn = %q, want expires_in", got.ExpiresIn)
	}
	if got.Scope != "scope" {
		t.Errorf("scope = %q, want scope", got.Scope)
	}
}

// A half-migrated spec is the likeliest reason a v2 decode fails, so the error has to be
// migration advice rather than a decoder's complaint.
//
// yaml.v3 says "field network not found in type kit.Spec", which names a Go type the reader
// has no access to and invites them to go looking for a struct. What they need is: which
// field, which line, and what v2 calls it instead.
func TestUnknownFieldErrorsExplainTheMigration(t *testing.T) {
	t.Run("a v1 field names its v2 spelling", func(t *testing.T) {
		err := mustNotParse(t, "schemaVersion: \"2\"\nkind: mixin\nname: x\nnetwork:\n  allowedDomains: [a]\n",
			"permissions.network.allow")
		for _, want := range []string{`"network"`, "v1", "line 4"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q is missing %q", err, want)
			}
		}
		// The Go type must not appear. This is the whole reason the rewrite exists.
		if strings.Contains(err.Error(), "kit.Spec") {
			t.Errorf("error %q leaks the Go type name", err)
		}
	})

	t.Run("a typo says it is not in the grammar", func(t *testing.T) {
		err := mustNotParse(t, "schemaVersion: \"2\"\nkind: mixin\nname: x\nnotAField: 1\n",
			`"notAField"`)
		if strings.Contains(err.Error(), "kit.Spec") {
			t.Errorf("error %q leaks the Go type name", err)
		}
		// It must NOT claim a typo is a v1 field, which would send the reader to a
		// migration table that does not mention it.
		if strings.Contains(err.Error(), "v1") {
			t.Errorf("error %q calls an unknown field a v1 field", err)
		}
	})
}
