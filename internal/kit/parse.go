package kit

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// SpecFileName is the required filename of a kit's spec.
const SpecFileName = "spec.yaml"

// ParseSpec decodes spec.yaml bytes into a canonical Spec and validates it.
//
// The schemaVersion in the document selects the grammar. It has to be read before the document
// can be decoded — the two grammars are different decode targets and a v1 key is a hard error
// in v2 — so the version is peeked first with a throwaway decode of just that one field.
//
// A v1 document is translated onto the same Spec a v2 document produces, so callers do not
// branch on version. The returned warnings are the legacy surfaces that were folded, dropped or
// ignored during that translation; a v2 document produces warnings only for fields the grammar
// accepts without effect. Warnings are advisory and never accompany an error: on failure the
// Spec is nil and any warnings collected so far are discarded, because a spec that did not load
// has nothing actionable to say about its deprecated fields.
func ParseSpec(data []byte) (*Spec, []string, error) {
	version, err := peekSchemaVersion(data)
	if err != nil {
		return nil, nil, err
	}

	var s *Spec
	var w []string

	switch version {
	case SchemaVersion2:
		s, err = decodeV2(data)
		if err != nil {
			return nil, nil, err
		}
		w = warnV2(s)

	case SchemaVersion1:
		sf, derr := decodeV1(data)
		if derr != nil {
			return nil, nil, derr
		}
		var wv warnings
		s, wv, err = translateV1(sf)
		if err != nil {
			return nil, nil, err
		}
		w = append(wv, warnV2(s)...)

	default:
		// Naming the version that was found is the whole point of this branch. Without it
		// the author of a spec that says `schemaVersion: "3"`, or one that omits the field
		// and gets "", is told only that something is unsupported.
		return nil, nil, fmt.Errorf("schemaVersion %q is not supported (supported: %s)",
			version, strings.Join(SupportedSchemaVersions, ", "))
	}

	// Scheme expansion runs before validation, not after, because validation checks Header
	// and Format and the whole purpose of a scheme is to supply them.
	if err := ExpandSchemes(s); err != nil {
		return nil, nil, err
	}

	if err := Validate(s); err != nil {
		return nil, nil, err
	}

	return s, w, nil
}

// peekSchemaVersion reads just the schemaVersion field, without strict decoding, so that the
// grammar can be chosen before the document is decoded properly.
//
// Non-strict on purpose: at this point every other key in the document is expected to be
// unknown to the probe struct, so strictness here would reject everything. Errors from the
// probe are still surfaced, because a document that does not parse as YAML at all should say so
// rather than fall through to "schemaVersion "" is not supported", which describes the symptom
// instead of the cause.
func peekSchemaVersion(data []byte) (string, error) {
	// A document with no content at all cannot select a grammar, and saying
	// `schemaVersion "" is not supported` about an empty file describes the symptom while
	// hiding the cause — the likeliest of which is a path that pointed at the wrong place
	// and produced an empty read rather than an error.
	if len(bytes.TrimSpace(stripYAMLComments(data))) == 0 {
		return "", fmt.Errorf("%s is empty: a kit spec must at least set schemaVersion, "+
			"kind and name", SpecFileName)
	}

	var probe struct {
		SchemaVersion string `yaml:"schemaVersion"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return "", fmt.Errorf("%s: %w", SpecFileName, err)
	}
	return probe.SchemaVersion, nil
}

// stripYAMLComments removes whole-line comments so that a file of nothing but comments reads
// as empty. It is deliberately naive — it does not understand a `#` inside a quoted scalar —
// because it is used for one question only: is there any content here at all? A document with
// a quoted `#` in it has content by definition, and the naive answer and the correct one agree.
func stripYAMLComments(data []byte) []byte {
	var kept [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("#")) {
			continue
		}
		kept = append(kept, line)
	}
	return bytes.Join(kept, []byte("\n"))
}

// ExpandSchemes rewrites the apiKey.inject shorthand `scheme:` into the canonical Header,
// Format and Username fields and clears Scheme, so that every consumer reads one shape.
//
// It is exported because a caller that builds a Spec in Go rather than decoding one still has
// to run it for a Scheme to take effect; ParseSpec calls it for the decode path.
//
// The two schemes are not symmetric, and the asymmetry is the reference's:
//
//   - bearer is a whole-header shorthand, so it supplies Format "Bearer %s" AND Header
//     "Authorization" — but only when Header was left empty, so an explicit header still wins
//     and an author can point Bearer-formatted values at a non-standard header.
//   - basic is username-driven at the proxy rather than a header encoding, so it sets NO
//     header at all. Format is set to "%s" only so that the one-placeholder check in
//     validateAPIKey has something well-formed to look at; the proxy does not use it to build
//     a header.
func ExpandSchemes(s *Spec) error {
	if s == nil {
		return nil
	}
	for i := range s.Credentials {
		k := s.Credentials[i].APIKey
		if k == nil {
			continue
		}
		for j := range k.Inject {
			inj := &k.Inject[j]
			if inj.Scheme == "" {
				continue
			}
			path := fmt.Sprintf("credentials[%d].apiKey.inject[%d]", i, j)
			if inj.Format != "" {
				return fmt.Errorf("%s: scheme and format are mutually exclusive; "+
					"scheme %q already implies a format", path, inj.Scheme)
			}
			switch inj.Scheme {
			case SchemeBearer:
				if inj.Username != "" {
					return fmt.Errorf("%s: username is not valid with scheme: %s "+
						"(only scheme: %s uses a username)",
						path, SchemeBearer, SchemeBasic)
				}
				inj.Format = "Bearer %s"
				if inj.Header == "" {
					inj.Header = "Authorization"
				}
			case SchemeBasic:
				if inj.Username == "" {
					return fmt.Errorf("%s: scheme: %s requires a username "+
						"(the proxy uses it as the HTTP Basic username and the "+
						"credential as the password)", path, SchemeBasic)
				}
				inj.Format = "%s"
			default:
				return fmt.Errorf("%s: unknown scheme %q (want %q or %q)",
					path, inj.Scheme, SchemeBasic, SchemeBearer)
			}
			inj.Scheme = ""
		}
	}
	return nil
}

// warnV2 reports the fields the v2 grammar accepts but does not act on. These are not
// translation warnings — nothing was rewritten — so they are produced for both grammars, after
// v1 translation has run, and a v2 spec can emit them too.
func warnV2(s *Spec) []string {
	var w warnings

	if len(s.Mixins) > 0 {
		w.ignore("mixins", "the field is accepted by the grammar but composition is not "+
			"applied by this package")
	}
	if s.Sandbox != nil && s.Sandbox.Build != nil {
		w.ignore("sandbox.build", "Dockerfile builds are accepted by the grammar but not "+
			"performed; the image comes from sandbox.image")
	}
	// A filename names the AI profile the sandbox OWNS, and a mixin contributes to a base
	// sandbox's profile rather than owning one. Warned rather than rejected: the reference is
	// explicit that a mixin's filename is ignored with a warning, and rejecting would break
	// mixins that set it harmlessly.
	if s.Kind == KindMixin && s.AgentInstructions != nil && s.AgentInstructions.Filename != "" {
		w.ignore("agentInstructions.filename", fmt.Sprintf(
			"it is only used for kind %q; a %s contributes content to the base agent's "+
				"profile rather than naming one", KindSandbox, KindMixin))
	}
	for i, c := range s.Credentials {
		if c.Provider != "" {
			w.ignore(fmt.Sprintf("credentials[%d].provider", i),
				"the field is reserved for a future provider registry and has no effect")
		}
		// Only warned for a v2 spec here; the v1 path already warns from
		// translateV1OAuth, which has the extra context that the field was carried
		// across a translation.
		if s.SchemaVersion == SchemaVersion2 && c.OAuth != nil && len(c.OAuth.SkipIfEnv) > 0 {
			w.ignore(fmt.Sprintf("credentials[%d].oauth.skipIfEnv", i),
				"it is accepted for compatibility but ignored for schemaVersion "+
					SchemaVersion2+": resolution is binding-driven, so a host "+
					"environment variable cannot gate OAuth")
		}
		if c.OAuth != nil && c.OAuth.CredentialFile != nil &&
			c.OAuth.CredentialFile.Template != "" &&
			len(c.OAuth.CredentialFile.Structure) > 0 {
			w.ignore(fmt.Sprintf("credentials[%d].oauth.credentialFile.template", i),
				"structure is also set and takes precedence")
		}
	}
	return w
}
