package kit

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// Command is the mode-specific argument tail appended after Sandbox.Entrypoint.
//
// Effective launch arguments are Entrypoint[1:] plus Default for a non-interactive run, and
// Entrypoint[1:] plus Interactive for a TTY session, with Interactive falling back to Default
// when unset. When Command is omitted entirely, both modes run Entrypoint as-is.
//
// One consequence worth stating because it is a trap rather than a detail: for a kit that uses
// Extends, Command REPLACES the whole inherited argument tail, including any flags the
// parent put after the binary in its Entrypoint. It does not append. A child of a parent whose
// entrypoint is [claude, --dangerously-skip-permissions] that sets a Command must respell
// --dangerously-skip-permissions itself or silently lose it.
type Command struct {
	// Default is the tail for a non-interactive launch.
	Default []string

	// Interactive is the tail for a TTY session. Nil means "fall back to Default", which is
	// why this is a nil-vs-empty distinction that matters: `interactive: []` means an
	// explicitly empty tail and does NOT fall back.
	Interactive []string
}

// InteractiveTail returns the tail for a TTY session, applying the documented fallback to
// Default when Interactive was not set at all. It exists so that the nil-vs-empty distinction
// on Interactive is resolved in one place rather than at every call site that might get it
// wrong.
func (c Command) InteractiveTail() []string {
	if c.Interactive == nil {
		return c.Default
	}
	return c.Interactive
}

// commandMappingKeys are the only keys the mapping form of sandbox.command accepts. It exists
// because of a yaml.v3 behaviour verified by TestCommandMappingRejectsUnknownKey: a type with
// a custom UnmarshalYAML that calls node.Decode does NOT inherit the outer decoder's
// KnownFields(true) setting. node.Decode builds a fresh decoder with default (permissive)
// settings, so an unknown key inside sandbox.command would be silently dropped while the same
// typo one level up is a hard error.
//
// That hole is invisible in review — the struct passed to node.Decode looks strict because
// every other struct in the grammar is — so the key check below is done by hand against this
// list. Any future field added to the mapping form must be added here too, which is the cost
// of the workaround and the reason it is documented rather than merely written.
var commandMappingKeys = map[string]bool{"default": true, "interactive": true}

// UnmarshalYAML decodes the polymorphic sandbox.command field: a sequence is shorthand for
// Default alone, and a mapping carries default and interactive separately.
func (c *Command) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.SequenceNode:
		return node.Decode(&c.Default)

	case yaml.MappingNode:
		// Mapping content alternates key, value, key, value.
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if !commandMappingKeys[key.Value] {
				return fmt.Errorf("line %d: field %s not found in sandbox.command "+
					"(want \"default\" or \"interactive\")", key.Line, key.Value)
			}
		}
		var m struct {
			Default     []string `yaml:"default"`
			Interactive []string `yaml:"interactive"`
		}
		if err := node.Decode(&m); err != nil {
			return err
		}
		c.Default = m.Default
		c.Interactive = m.Interactive
		return nil

	case 0:
		// A zero Kind is an absent node. yaml.v3 does not call UnmarshalYAML for a missing
		// key, so this is reached only for an explicit `command:` with no value, which is
		// a null node in practice; treat it as "not set" rather than an error.
		return nil

	default:
		return fmt.Errorf("line %d: sandbox.command must be a list (shorthand for the "+
			"default tail) or a mapping with default/interactive keys", node.Line)
	}
}

// decodeV2 decodes schemaVersion "2" spec.yaml bytes into a Spec with strict field checking.
//
// Strictness is the whole point of this function. The v2 grammar has no legacy shims, so a v1
// field name in a v2 spec must be a hard decode error and not a silently ignored key: the
// reference is explicit that "legacy v1 fields in a schemaVersion: '2' spec are rejected
// during decode instead of being folded into the v2 model". Because Spec's yaml tags spell out
// exactly the v2 grammar, KnownFields(true) delivers that for every v1 key at once —
// agent:, network:, commands:, memory:, agentContext:, caps:, settings:, tmpfs:, kitDir:,
// persistence:, publishedPorts:, sandbox.aiFilename, sandbox.entrypoint.run and the rest —
// without a blocklist that could fall out of sync with the v1 grammar.
//
// The one v1 field a v2 spec may still carry is oauth.skipIfEnv, which is in the v2 grammar
// on purpose; see OAuth.SkipIfEnv.
//
// yaml.v3 has no UnmarshalStrict; Decoder.KnownFields(true) is its equivalent, and it does
// recurse into nested structs and into structs inside slices (verified). It does NOT reach
// inside a custom UnmarshalYAML — see commandMappingKeys.
func decodeV2(data []byte) (*Spec, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var s Spec
	if err := dec.Decode(&s); err != nil {
		if errors.Is(err, io.EOF) {
			// yaml.v3 reports an empty or comment-only document as EOF, which as a
			// bare error message ("EOF") tells the author nothing about what is wrong.
			return nil, errors.New("spec.yaml is empty")
		}
		return nil, err
	}

	// A second YAML document is silently ignored by a single Decode call, so a spec.yaml
	// with a stray --- separator would have half its content dropped without complaint.
	// This package rejects that instead. Docker's reference implementation does not — it
	// decodes the first document and stops — so this is a deliberate divergence, on the
	// grounds that silently discarding an entire document is the same class of mistake
	// KnownFields exists to prevent. No contrib kit contains a --- separator, so nothing in
	// the corpus is affected.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("spec.yaml has more than one YAML document (second starts at "+
			"line %d); a kit spec is a single document", extra.Line)
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}

	return &s, nil
}
