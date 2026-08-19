package kit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file is the schemaVersion "1" grammar and its translation onto the canonical v2 Spec.
//
// The v1 grammar is frozen: nothing new goes in it. It exists so that kits written before the
// v2 cutover keep loading, and every field below is followed by where it lands in v2. The
// mapping implemented here is the change table from the rendered "Kit spec reference" page,
// which agrees with the kit-author skill's v1-migration.md table on every row.
//
// Two things about v1 are easy to get wrong and are worth stating up front.
//
// First, v1 is not "the old spelling" — it is a grammar that accepts BOTH spellings for
// several surfaces. `kind: sandbox` and `kind: agent` are both v1-legal; a `sandbox:` block and
// an `agent:` block are both v1-legal; `credentials:` accepts the v1 mapping-with-sources form
// and the v2 sequence form; `volumes:` accepts the v1 mapping form and the v2 sequence form.
// testdata/v1/sample-agent.yaml, taken from Docker's own spec-package testdata, is a
// schemaVersion "1" spec that already uses `kind: sandbox` and a `sandbox:` block. So the
// translation cannot assume it is looking at the legacy spelling; it has to handle either.
//
// Second, v1 had FOUR overlapping surfaces describing one credential — credentials.sources,
// network.serviceDomains, network.serviceAuth and environment.proxyManaged, plus a standalone
// top-level oauth: block. Collapsing those into one Credential per service is the bulk of
// translateV1 and the only genuinely intricate part of this file.

// specV1 is the on-disk YAML schema for a schemaVersion "1" spec.yaml.
//
// It is a separate decode target from Spec rather than a superset of it, so that neither
// grammar can leak into the other: a v2 key that v1 never had is a decode error here, and
// every v1 key is a decode error in Spec. Both paths are strict.
type specV1 struct {
	SchemaVersion string `yaml:"schemaVersion"`

	// Kind is "agent", "sandbox" or "mixin" in v1. "agent" becomes "sandbox".
	Kind string `yaml:"kind"`

	Name        string `yaml:"name,omitempty"`
	Version     string `yaml:"version,omitempty"`
	DisplayName string `yaml:"displayName,omitempty"`
	Description string `yaml:"description,omitempty"`
	SourceURL   string `yaml:"sourceURL,omitempty"`

	Extends  string    `yaml:"extends,omitempty"`
	Mixins   []string  `yaml:"mixins,omitempty"`
	Requires *Requires `yaml:"requires,omitempty"`
	Locked   []string  `yaml:"locked,omitempty"`
	Licenses []string  `yaml:"licenses,omitempty"`
	Security *Security `yaml:"security,omitempty"`

	// Sandbox is the v1 `sandbox:` block; LegacyAgent is the older `agent:` spelling of the
	// same block. When both appear, Sandbox wins and the agent block is dropped — that is
	// the reference implementation's precedence, and a spec with both is contradicting
	// itself, so either choice is arbitrary and matching the reference is the useful one.
	Sandbox     *sandboxV1 `yaml:"sandbox,omitempty"`
	LegacyAgent *sandboxV1 `yaml:"agent,omitempty"`

	// Network is the v1 top-level network: block, which v2 split three ways:
	// allowedDomains/deniedDomains to permissions.network.allow/deny, serviceDomains and
	// serviceAuth into credentials[].apiKey.inject, and publishedPorts to top-level ports.
	Network *networkV1 `yaml:"network,omitempty"`

	// PublishedPorts is the v1 TOP-LEVEL publishedPorts: list, which is distinct from
	// network.publishedPorts. Both existed and both become v2's top-level ports:; the
	// reference concatenates them, so this translation does too.
	PublishedPorts []Port `yaml:"publishedPorts,omitempty"`

	// Credentials is polymorphic: a mapping with sources: (v1) or a sequence (v2 shape,
	// already legal in a v1 spec). See credentialsFieldV1.
	Credentials credentialsFieldV1 `yaml:"credentials,omitempty"`

	// OAuth is the v1 standalone top-level oauth: block, which carried its own service:
	// field. v2 folds it into the matching credentials[] entry.
	OAuth *oauthV1 `yaml:"oauth,omitempty"`

	// Environment carries variables plus the removed proxyManaged list.
	Environment *environmentV1 `yaml:"environment,omitempty"`

	// Commands is the v1 name for v2's setup:, and commands.initFiles is v2's setup.files.
	Commands *commandsV1 `yaml:"commands,omitempty"`

	// AgentContext and LegacyMemory are two spellings of the same field, both of which
	// become agentInstructions.content. AgentContext wins when both are set, matching the
	// reference.
	AgentContext string `yaml:"agentContext,omitempty"`
	LegacyMemory string `yaml:"memory,omitempty"`

	// Volumes is polymorphic: a mapping of path to size string (v1) or a sequence (v2
	// shape). See volumesFieldV1.
	Volumes volumesFieldV1 `yaml:"volumes,omitempty"`

	// Tmpfs is the v1 top-level tmpfs: block, a mapping of container path to size string.
	// It becomes volumes: entries with type: tmpfs.
	Tmpfs map[string]string `yaml:"tmpfs,omitempty"`

	// Caps is an EARLIER v2 DRAFT spelling of permissions:, not a v1 field at all. It is
	// admitted here because the reference's v1 decoder admits it — kits were written
	// against the draft before the block was renamed — and translated to Permissions with a
	// warning. Anyone reading the published v1 documentation will not find it; it is in the
	// reference implementation's SpecFile and in v1-migration.md's parenthetical note.
	Caps *capsV1 `yaml:"caps,omitempty"`

	// The four fields below are accepted only so that a v1 spec still carrying them decodes
	// instead of being rejected outright. They have no v2 home and are dropped with a
	// warning.
	//
	// The history matters for judging whether dropping is right: Settings hardcoded
	// agent-specific setup and was replaced by setup.files. Persistence and KitDir were
	// declared, parsed, inherited and displayed in v1 but never consumed by any runtime
	// decision — they were removed at the same time strict decoding was turned on, which
	// turned a silent no-op into a hard error for every kit that still had the line. So
	// these are not fields whose behaviour is being discarded; there was no behaviour.
	Settings    *settingsV1 `yaml:"settings,omitempty"`
	Persistence string      `yaml:"persistence,omitempty"`
	KitDir      string      `yaml:"kitDir,omitempty"`

	// Secrets and Egress are pre-v1 fields the reference's v1 decoder still admits. It
	// rejects any non-empty value for them rather than translating anything, and so does
	// this package; they are here to produce that specific error instead of a confusing
	// unknown-field error.
	Secrets []string          `yaml:"secrets,omitempty"`
	Egress  map[string]string `yaml:"egress,omitempty"`
}

// sandboxV1 is the v1 sandbox:/agent: block.
type sandboxV1 struct {
	Image string       `yaml:"image,omitempty"`
	Build *BuildConfig `yaml:"build,omitempty"`

	// Entrypoint is a MAPPING in v1, not the flat array v2 uses.
	Entrypoint *entrypointV1 `yaml:"entrypoint,omitempty"`

	// AIFilename becomes agentInstructions.filename, moving out of the sandbox block.
	AIFilename string `yaml:"aiFilename,omitempty"`

	// Resources takes memory as an integer memoryMB in v1, not v2's byte-size string.
	Resources *resourcesV1 `yaml:"resources,omitempty"`

	// Persistence is the nested spelling of the removed persistence field; see
	// specV1.Persistence. It lived both at the spec root and inside this block.
	Persistence string `yaml:"persistence,omitempty"`

	// Binary, RunOptions and InteractiveOptions are the OTHER v1 spelling of a command,
	// and omitting them made every kit that uses them fail to load with "field binary not
	// found" — a v1 kit refused by the v1 decoder.
	//
	// They come from `kind: agent`, where the executable and its arguments are three
	// fields rather than an entrypoint mapping. The normative spec library says exactly
	// how they map: "In v2 this is entrypoint[1:] plus sandbox.command.default", with
	// InteractiveOptions becoming command.interactive and callers falling back to
	// RunOptions when it is empty. So Binary is the entrypoint's head.
	//
	// Found by diffing this package's yaml tags against the field set in Docker's own
	// spec library rather than by meeting a kit that used them, which is why the mapping
	// is quoted rather than inferred.
	Binary             string   `yaml:"binary,omitempty"`
	RunOptions         []string `yaml:"runOptions,omitempty"`
	InteractiveOptions []string `yaml:"interactiveOptions,omitempty"`
}

// entrypointV1 is the v1 entrypoint mapping. v2 flattens run into sandbox.entrypoint and
// splits args/ttyArgs into sandbox.command.default/interactive.
type entrypointV1 struct {
	Run     []string `yaml:"run,omitempty"`
	Args    []string `yaml:"args,omitempty"`
	TtyArgs []string `yaml:"ttyArgs,omitempty"`

	// PipeMode has no v2 equivalent and is dropped. Both the reference page's change table
	// and v1-migration.md say so explicitly ("pipeMode has no v2 home and is dropped").
	PipeMode string `yaml:"pipeMode,omitempty"`
}

// resourcesV1 is the v1 resources block. Memory is an integer count of mebibytes; v2 takes a
// byte-size string, so translation has to render it back into one.
type resourcesV1 struct {
	CPU      float64 `yaml:"cpu,omitempty"`
	MemoryMB int64   `yaml:"memoryMB,omitempty"`
	GPU      string  `yaml:"gpu,omitempty"`
}

// networkV1 is the v1 top-level network: block.
type networkV1 struct {
	AllowedDomains []string               `yaml:"allowedDomains,omitempty"`
	DeniedDomains  []string               `yaml:"deniedDomains,omitempty"`
	ServiceDomains map[string]string      `yaml:"serviceDomains,omitempty"`
	ServiceAuth    map[string]serviceAuth `yaml:"serviceAuth,omitempty"`
	PublishedPorts []Port                 `yaml:"publishedPorts,omitempty"`
}

// serviceAuth is one v1 network.serviceAuth entry: the header name and value format for a
// service, which v2 moved onto each apiKey.inject entry.
type serviceAuth struct {
	HeaderName  string `yaml:"headerName,omitempty"`
	ValueFormat string `yaml:"valueFormat,omitempty"`
}

// capsV1 is the earlier v2-draft caps: block; see specV1.Caps.
type capsV1 struct {
	Network *NetworkPermissions `yaml:"network,omitempty"`
}

// environmentV1 is the v1 environment: block.
type environmentV1 struct {
	Variables map[string]string `yaml:"variables,omitempty"`

	// ProxyManaged is a list of environment variable names the proxy populates in the
	// container. v2 removed it in favour of the per-credential apiKey.proxyManaged flag.
	ProxyManaged []string `yaml:"proxyManaged,omitempty"`
}

// commandsV1 is the v1 commands: block, renamed to setup: in v2.
type commandsV1 struct {
	Install []InstallCommand `yaml:"install,omitempty"`
	Startup []StartupCommand `yaml:"startup,omitempty"`

	// InitFiles is v2's setup.files. Same entry shape, different key.
	InitFiles []SetupFile `yaml:"initFiles,omitempty"`
}

// settingsV1 is the removed v1 settings: block; see specV1.Settings.
type settingsV1 struct {
	ContainerSettings map[string]bool `yaml:"containerSettings,omitempty"`
}

// oauthV1 is the v1 standalone top-level oauth: block.
type oauthV1 struct {
	// Service is what makes this block foldable: it names which credential the OAuth
	// configuration belongs to. In v2 the credential entry owns the service and the oauth
	// block sits under it, so this field disappears.
	Service string `yaml:"service,omitempty"`

	TokenEndpoint  OAuthTokenEndpoint   `yaml:"tokenEndpoint,omitempty"`
	Sentinels      OAuthSentinels       `yaml:"sentinels,omitempty"`
	CredentialFile *OAuthCredentialFile `yaml:"credentialFile,omitempty"`
	SkipIfEnv      []string             `yaml:"skipIfEnv,omitempty"`
	ResponseFields *OAuthResponseFields `yaml:"responseFields,omitempty"`

	// PassthroughResponse is the v1 spelling of v2's passthrough. Renamed, same semantics.
	PassthroughResponse bool `yaml:"passthroughResponse,omitempty"`
}

// credentialSourceV1 is one v1 credentials.sources entry.
//
// Everything here except Required is credential DISCOVERY — where on the host to find the
// value. v2 deletes that half from the kit entirely and moves it to a user-side bindings file,
// on the principle that a kit declares what it needs and the user declares where it lives.
// So Env, File and Priority have no v2 destination and are dropped, with one exception:
// Env[0] is kept as apiKey.name, because in practice the first environment variable a v1 kit
// looked in is the variable name the container expects. That is the reference
// implementation's behaviour, and it is a heuristic rather than an equivalence — a v1 kit that
// listed several variables loses all but the first.
type credentialSourceV1 struct {
	Env      []string      `yaml:"env,omitempty"`
	File     *fileSourceV1 `yaml:"file,omitempty"`
	Priority string        `yaml:"priority,omitempty"`
	Required bool          `yaml:"required,omitempty"`
}

// fileSourceV1 is a v1 file-based credential source. Discovery only; dropped in v2.
type fileSourceV1 struct {
	Path   string `yaml:"path,omitempty"`
	Parser string `yaml:"parser,omitempty"`
}

// credentialsFieldV1 decodes the polymorphic v1 credentials: key, which is a mapping with
// sources: under it in the v1 spelling and a sequence in the v2 spelling. Both are legal in a
// schemaVersion "1" spec, so the shape has to be detected rather than assumed.
//
// A custom UnmarshalYAML is the only way to tell them apart: unlike the other v1/v2 field
// renames, these two share the same YAML key with different value kinds, so the
// two-yaml-tags-on-two-fields trick used for memory/agentContext and agent/sandbox does not
// apply.
type credentialsFieldV1 struct {
	// List is set when credentials: was a sequence.
	List []Credential

	// Sources is set when credentials: was a mapping with sources: under it.
	Sources map[string]credentialSourceV1
}

func (c *credentialsFieldV1) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.SequenceNode:
		return node.Decode(&c.List)
	case yaml.MappingNode:
		// Checked by hand rather than by KnownFields, because a custom UnmarshalYAML does
		// not inherit the outer decoder's strictness — see commandMappingKeys in
		// decode_v2.go for the verified behaviour.
		for i := 0; i+1 < len(node.Content); i += 2 {
			if key := node.Content[i]; key.Value != "sources" {
				return fmt.Errorf("line %d: field %s not found in credentials "+
					"(the v1 mapping form takes only \"sources\")", key.Line, key.Value)
			}
		}
		var v1 struct {
			Sources map[string]credentialSourceV1 `yaml:"sources"`
		}
		if err := node.Decode(&v1); err != nil {
			return err
		}
		c.Sources = v1.Sources
		return nil
	case 0:
		return nil
	default:
		return fmt.Errorf("line %d: credentials must be a list or a mapping with "+
			"sources: (schemaVersion 1)", node.Line)
	}
}

// volumesFieldV1 decodes the polymorphic v1 volumes: key: a mapping of container path to size
// string in the v1 spelling, a sequence of entries in the v2 spelling.
//
// Note that the v1 mapping form fails strict decoding against the v2 grammar with a TYPE
// error rather than an unknown-field error, because the key volumes: exists in both grammars
// and only its value kind differs. That is why a v2 spec using the mapping form gets a
// confusing message and the v1-migration guide calls this row "manual" rather than mechanical.
type volumesFieldV1 struct {
	// List is set when volumes: was a sequence.
	List []Volume

	// Sizes is set when volumes: was a mapping: key is the container path, value is the
	// size string, which may be empty when the v1 spec gave no size.
	Sizes map[string]string
}

func (v *volumesFieldV1) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.SequenceNode:
		return node.Decode(&v.List)
	case yaml.MappingNode:
		var m map[string]string
		if err := node.Decode(&m); err != nil {
			return err
		}
		v.Sizes = m
		return nil
	case 0:
		return nil
	default:
		return fmt.Errorf("line %d: volumes must be a list or a mapping of path to size "+
			"(schemaVersion 1)", node.Line)
	}
}

// decodeV1 decodes schemaVersion "1" spec.yaml bytes with strict field checking.
//
// Strict on the v1 path too, deliberately. It would be tempting to be permissive here on the
// grounds that v1 is legacy, but the reference turned strict decoding on for v1 as well, and
// the whole reason several removed fields (persistence, kitDir, settings, tmpfs) are still in
// specV1 above is that strictness made them hard errors and shims had to be added back. Being
// permissive instead would hide typos in exactly the specs least likely to be re-tested.
func decodeV1(data []byte) (*specV1, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var sf specV1
	if err := dec.Decode(&sf); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("spec.yaml is empty")
		}
		return nil, err
	}

	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("spec.yaml has more than one YAML document (second starts at "+
			"line %d); a kit spec is a single document", extra.Line)
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}

	return &sf, nil
}

// translateV1 folds a decoded v1 spec onto the canonical v2 Spec, appending one warning per
// legacy surface consumed.
//
// SchemaVersion is carried through as "1" and NOT rewritten to "2". The result is a v2-shaped
// model of a v1 spec, not a v2 spec, and the distinction is load-bearing downstream: v2
// credential resolution is binding-driven while v1's was host-discovery-driven, so a consumer
// that lost track of which grammar the author wrote would apply the wrong regime.
func translateV1(sf *specV1) (*Spec, []string, error) {
	var w warnings

	s := &Spec{
		SchemaVersion: sf.SchemaVersion,
		Kind:          sf.Kind,
		Name:          sf.Name,
		Version:       sf.Version,
		DisplayName:   sf.DisplayName,
		Description:   sf.Description,
		SourceURL:     sf.SourceURL,
		Licenses:      sf.Licenses,
		Locked:        sf.Locked,
		Security:      sf.Security,
		Extends:       sf.Extends,
		Mixins:        sf.Mixins,
		Requires:      sf.Requires,
		Ports:         sf.PublishedPorts,
	}

	// Pre-v1 fields with no translation at all. Rejected rather than dropped, because unlike
	// persistence/kitDir these did describe intended behaviour, and silently discarding a
	// secrets: list would hand the author a sandbox that quietly has no secrets.
	if len(sf.Secrets) > 0 {
		return nil, nil, fmt.Errorf("secrets: has no equivalent in the current grammar; " +
			"declare each one under credentials: instead")
	}
	if len(sf.Egress) > 0 {
		return nil, nil, fmt.Errorf("egress: has no equivalent in the current grammar; " +
			"use permissions.network.allow instead")
	}

	if sf.Kind == KindAgent {
		s.Kind = KindSandbox
		w.deprecate("kind: "+KindAgent, "use 'kind: "+KindSandbox+"' instead")
	}

	if err := translateV1Sandbox(sf, s, &w); err != nil {
		return nil, nil, err
	}
	translateV1AgentInstructions(sf, s, &w)
	translateV1Network(sf, s, &w)
	if err := translateV1Credentials(sf, s, &w); err != nil {
		return nil, nil, err
	}
	translateV1Environment(sf, s, &w)
	translateV1Setup(sf, s, &w)
	translateV1Volumes(sf, s, &w)
	translateV1Removed(sf, &w)

	return s, w, nil
}

// translateV1Sandbox folds the sandbox:/agent: block, including the entrypoint mapping split
// and the memoryMB-to-byte-size-string conversion.
func translateV1Sandbox(sf *specV1, s *Spec, w *warnings) error {
	block := sf.Sandbox
	if sf.LegacyAgent != nil {
		if block == nil {
			block = sf.LegacyAgent
		}
		w.deprecate("agent:", "use the 'sandbox:' block instead")
	}
	if block == nil {
		return nil
	}

	if block.Persistence != "" {
		w.drop("sandbox.persistence", "the field was parsed but never wired to any runtime "+
			"behaviour; declare volumes: explicitly instead")
	}

	sb := &Sandbox{Image: block.Image, Build: block.Build}

	if e := block.Entrypoint; e != nil {
		// run -> the flat entrypoint array; args -> command.default;
		// ttyArgs -> command.interactive.
		//
		// ttyArgs is worth a note. Both change tables map it to command.interactive, and
		// that is what is implemented here. Docker's reference normalizer does NOT do it:
		// its normalizeSandbox reads Entrypoint.Run and Entrypoint.Args and never touches
		// TtyArgs, so a v1 kit's ttyArgs is decoded and then silently discarded there.
		// Following the documented table over the code is the right call for this one
		// field: dropping an author's interactive arguments is a behaviour change they
		// cannot see, whereas carrying them across is what both tables promise. Recorded
		// because it is a real divergence from the reference implementation.
		sb.Entrypoint = e.Run
		sb.Command.Default = e.Args
		sb.Command.Interactive = e.TtyArgs
		if e.PipeMode != "" {
			w.drop("sandbox.entrypoint.pipeMode", "the field has no equivalent in the "+
				"current grammar")
		}
	}

	// The other v1 spelling of the same thing: `kind: agent` gives the executable and its
	// arguments as three flat fields instead of an entrypoint mapping. The mapping is the
	// normative spec library's own — "in v2 this is entrypoint[1:] plus
	// sandbox.command.default" — so binary is the entrypoint's head.
	//
	// Only when the mapping form did not already set them. A kit carrying both spellings
	// is not something either grammar defines, and preferring the one this decoder already
	// documents beats picking by field order.
	if block.Binary != "" && len(sb.Entrypoint) == 0 {
		sb.Entrypoint = []string{block.Binary}
		w.deprecate("agent.binary", "use 'sandbox.entrypoint' instead")
	}
	if len(block.RunOptions) > 0 && len(sb.Command.Default) == 0 {
		sb.Command.Default = block.RunOptions
		w.deprecate("agent.runOptions", "use 'sandbox.command.default' instead")
	}
	if len(block.InteractiveOptions) > 0 && len(sb.Command.Interactive) == 0 {
		sb.Command.Interactive = block.InteractiveOptions
		w.deprecate("agent.interactiveOptions", "use 'sandbox.command.interactive' instead")
	}

	if r := block.Resources; r != nil {
		sb.Resources = &Resources{CPU: r.CPU, GPU: r.GPU}
		if r.MemoryMB != 0 {
			if r.MemoryMB < 0 {
				return fmt.Errorf("sandbox.resources.memoryMB must be non-negative "+
					"(got %d)", r.MemoryMB)
			}
			// Rendered as a mebibyte string rather than converted to gibibytes even
			// when it divides evenly, so the number the author wrote stays visible in
			// the translated model. "4096m" and "4g" parse to the same size.
			sb.Resources.Memory = fmt.Sprintf("%dm", r.MemoryMB)
			w.deprecate("sandbox.resources.memoryMB", "use 'sandbox.resources.memory' "+
				"with a byte-size string such as \""+sb.Resources.Memory+"\"")
		}
	}

	s.Sandbox = sb
	return nil
}

// translateV1AgentInstructions moves sandbox.aiFilename and memory:/agentContext: into the
// top-level agentInstructions block.
func translateV1AgentInstructions(sf *specV1, s *Spec, w *warnings) {
	ai := AgentInstructions{}

	block := sf.Sandbox
	if block == nil {
		block = sf.LegacyAgent
	}
	if block != nil && block.AIFilename != "" {
		ai.Filename = block.AIFilename
		w.deprecate("sandbox.aiFilename", "use 'agentInstructions.filename' instead")
	}

	// agentContext: and memory: are two spellings of one field. agentContext wins when both
	// are present, matching the reference; memory is the older of the two.
	switch {
	case sf.AgentContext != "":
		ai.Content = sf.AgentContext
		w.deprecate("agentContext:", "use 'agentInstructions.content' instead")
		if sf.LegacyMemory != "" {
			w.drop("memory:", "'agentContext:' is also set and takes precedence")
		}
	case sf.LegacyMemory != "":
		ai.Content = sf.LegacyMemory
		w.deprecate("memory:", "use 'agentInstructions.content' instead")
	}

	if ai != (AgentInstructions{}) {
		s.AgentInstructions = &ai
	}
}

// translateV1Network folds network.allowedDomains/deniedDomains into
// permissions.network.allow/deny, promotes network.publishedPorts to the top-level ports, and
// absorbs the earlier caps: draft spelling.
//
// serviceDomains and serviceAuth are NOT handled here: they describe credential injection, not
// egress, and belong to translateV1Credentials.
func translateV1Network(sf *specV1, s *Spec, w *warnings) {
	var allow, deny []string

	if sf.Caps != nil && sf.Caps.Network != nil {
		allow = append(allow, sf.Caps.Network.Allow...)
		deny = append(deny, sf.Caps.Network.Deny...)
		w.deprecate("caps:", "use the 'permissions:' block instead")
	}

	if n := sf.Network; n != nil {
		if len(n.AllowedDomains) > 0 {
			allow = append(allow, n.AllowedDomains...)
			w.deprecate("network.allowedDomains", "use 'permissions.network.allow' instead")
		}
		if len(n.DeniedDomains) > 0 {
			deny = append(deny, n.DeniedDomains...)
			w.deprecate("network.deniedDomains", "use 'permissions.network.deny' instead")
		}
		if len(n.PublishedPorts) > 0 {
			// Appended after the top-level publishedPorts already copied in
			// translateV1, so a spec using both keys keeps both lists in a stable
			// order rather than one clobbering the other.
			s.Ports = append(s.Ports, n.PublishedPorts...)
			w.deprecate("network.publishedPorts", "use the top-level 'ports:' field instead")
		}
	}

	if len(allow) > 0 || len(deny) > 0 {
		s.Permissions = &Permissions{Network: &NetworkPermissions{Allow: allow, Deny: deny}}
	}
}

// translateV1Credentials collapses v1's four credential surfaces plus the standalone oauth:
// block into one Credential per service.
//
// The four surfaces and what each contributes:
//
//	credentials.sources.<svc>        -> service id, apiKey.name (from env[0]), required
//	network.serviceDomains{dom: svc} -> one apiKey.inject entry per domain
//	network.serviceAuth.<svc>        -> that entry's header and format
//	environment.proxyManaged[]       -> apiKey.proxyManaged, and apiKey.name as a fallback
//
// An entry already written in the v2 sequence form wins outright: any service named by a v2
// entry is skipped by every legacy fold below, so a v1 spec that has begun migrating is not
// given a duplicate synthesised entry alongside the one its author wrote.
func translateV1Credentials(sf *specV1, s *Spec, w *warnings) error {
	s.Credentials = sf.Credentials.List

	// Services the author already spelled in the v2 form.
	explicit := make(map[string]bool, len(s.Credentials))
	for _, c := range s.Credentials {
		explicit[c.Service] = true
	}

	// One accumulator per service, materialised into Credentials at the end so that the
	// four surfaces can contribute in any order.
	type pending struct {
		required     bool
		name         string
		proxyManaged bool
		inject       []APIKeyInject
	}
	byService := map[string]*pending{}
	get := func(svc string) *pending {
		p, ok := byService[svc]
		if !ok {
			p = &pending{}
			byService[svc] = p
		}
		return p
	}

	// One warning per legacy surface rather than one per service, so a kit with ten
	// services does not emit ten identical lines.
	var sawSources, sawServiceDomains, sawServiceAuth, sawProxyManaged bool

	// Surface 1: credentials.sources.
	//
	// Iterated in sorted service order. Go randomises map iteration, so without this two
	// byte-identical specs would translate to Credentials lists in different orders across
	// runs, which makes any equality assertion on the result flaky.
	for _, svc := range sortedKeys(sf.Credentials.Sources) {
		if explicit[svc] {
			continue
		}
		sawSources = true
		src := sf.Credentials.Sources[svc]
		p := get(svc)
		if src.Required {
			p.required = true
		}
		if len(src.Env) > 0 {
			// env[0] only; see credentialSourceV1 for why this is a heuristic.
			p.name = src.Env[0]
		}
	}

	// Surfaces 2 and 3: network.serviceDomains, keyed by domain, joined to
	// network.serviceAuth, keyed by service.
	if n := sf.Network; n != nil {
		for _, domain := range sortedKeys(n.ServiceDomains) {
			svc := n.ServiceDomains[domain]
			if explicit[svc] {
				continue
			}
			sawServiceDomains = true
			// Format defaults to "%s" — a bare credential value with no prefix —
			// because a v1 serviceDomain with no matching serviceAuth still injected
			// something, and an empty format would inject an empty header value.
			inj := APIKeyInject{Domain: domain, Format: "%s"}
			if auth, ok := n.ServiceAuth[svc]; ok {
				sawServiceAuth = true
				inj.Header = auth.HeaderName
				if auth.ValueFormat != "" {
					inj.Format = auth.ValueFormat
				}
			}
			get(svc).inject = append(get(svc).inject, inj)
		}
		// A serviceAuth entry with no serviceDomains partner contributes no inject row —
		// there is no domain to inject into — but it was still consumed, so it earns the
		// warning. Without this the author of a serviceAuth-only block would get no
		// signal that the block did nothing.
		if !sawServiceAuth && len(n.ServiceAuth) > 0 {
			sawServiceAuth = true
		}
	}

	// Surface 4: environment.proxyManaged, a flat list of environment variable names with no
	// service attached. The service has to be recovered by looking the variable up in
	// credentials.sources; when that fails there is nothing left to derive it from, so the
	// entry is dropped with a warning rather than guessed at.
	//
	// The reference implementation guesses here instead, synthesising a service key from the
	// variable name (SAMPLE_PROXY_TOKEN becomes "sample_proxy"). That is not copied: it
	// invents a service identity that the user-side bindings file will never match, and it
	// is the sole reason the reference cannot enforce its own documented lowercase-kebab
	// charset on Credential.Service. Dropping with a warning tells the author to write the
	// credential out properly, which is the only thing that can actually work.
	if e := sf.Environment; e != nil && len(e.ProxyManaged) > 0 {
		sawProxyManaged = true
		envToService := map[string]string{}
		for svc, src := range sf.Credentials.Sources {
			for _, name := range src.Env {
				envToService[name] = svc
			}
		}
		for _, name := range e.ProxyManaged {
			svc, ok := envToService[name]
			if !ok {
				w.drop(fmt.Sprintf("environment.proxyManaged entry %q", name),
					"no credentials.sources entry lists it, so the service it "+
						"belongs to cannot be determined; declare a "+
						"credentials[] entry with apiKey.name: "+name+
						" and apiKey.proxyManaged: true")
				continue
			}
			if explicit[svc] {
				continue
			}
			p := get(svc)
			if p.name == "" {
				p.name = name
			}
			p.proxyManaged = true
		}
	}

	for _, svc := range sortedKeys(byService) {
		p := byService[svc]
		c := Credential{Service: svc, Required: p.required}
		// No apiKey block at all when nothing was learned about one: a credential with
		// neither a variable name nor an injection target would validate but do nothing,
		// and an empty apiKey: block is more misleading than its absence.
		if p.name != "" || len(p.inject) > 0 {
			c.APIKey = &APIKey{Name: p.name, ProxyManaged: p.proxyManaged, Inject: p.inject}
		}
		s.Credentials = append(s.Credentials, c)
	}

	if sawSources {
		w.deprecate("credentials.sources", "use the top-level 'credentials:' list with "+
			"'apiKey:' sub-blocks; host discovery (env/file/priority) moved out of the "+
			"kit into the user's credential bindings")
	}
	if sawServiceDomains {
		w.deprecate("network.serviceDomains", "use 'credentials[].apiKey.inject[].domain'")
	}
	if sawServiceAuth {
		w.deprecate("network.serviceAuth", "use 'credentials[].apiKey.inject[].header' and "+
			"'.format'")
	}
	if sawProxyManaged {
		w.deprecate("environment.proxyManaged", "use 'credentials[].apiKey.name' with "+
			"'credentials[].apiKey.proxyManaged: true'")
	}

	return translateV1OAuth(sf, s, w)
}

// translateV1OAuth folds the standalone top-level oauth: block onto the credential entry whose
// service matches, synthesising an entry when none exists yet.
func translateV1OAuth(sf *specV1, s *Spec, w *warnings) error {
	o := sf.OAuth
	if o == nil {
		return nil
	}
	if o.Service == "" {
		// Without a service there is nothing to attach the block to. v2 has no home for
		// a serviceless OAuth configuration, so this cannot be translated at all.
		return fmt.Errorf("oauth.service is required to fold the standalone 'oauth:' block " +
			"into a credentials[] entry")
	}

	v2 := &OAuth{
		TokenEndpoint:  o.TokenEndpoint,
		Sentinels:      o.Sentinels,
		CredentialFile: o.CredentialFile,
		SkipIfEnv:      o.SkipIfEnv,
		ResponseFields: o.ResponseFields,
		Passthrough:    o.PassthroughResponse,
	}
	if o.PassthroughResponse {
		w.deprecate("oauth.passthroughResponse", "renamed to 'passthrough' with the same "+
			"meaning")
	}
	if len(o.SkipIfEnv) > 0 {
		// Carried onto the v2 field, which exists precisely so migrated specs load, but
		// it has no effect there. See OAuth.SkipIfEnv.
		w.ignore("oauth.skipIfEnv", "credential resolution is binding-driven, so a host "+
			"environment variable cannot gate OAuth; to prefer an API key when one is "+
			"available, declare both apiKey and oauth on the credential")
	}

	for i := range s.Credentials {
		if s.Credentials[i].Service != o.Service {
			continue
		}
		// A synthesised apiKey that carries injection domains but no variable name is
		// not really an API key: it came from the v1 serviceDomains fold, which had no
		// way to express "route these hosts for OAuth". Those domains are OAuth resource
		// hosts, so they move to resourceHosts and the fake apiKey is removed. Without
		// this the credential would claim an API-key mechanism that has no value to
		// inject.
		if c := s.Credentials[i]; isRoutingOnlyAPIKey(c.APIKey) {
			for _, inj := range c.APIKey.Inject {
				// The token endpoint host is already named by tokenEndpoint and is
				// not a resource host, so it is not duplicated here.
				if inj.Domain == "" || inj.Domain == v2.TokenEndpoint.Host {
					continue
				}
				v2.ResourceHosts = append(v2.ResourceHosts, inj.Domain)
			}
			sort.Strings(v2.ResourceHosts)
			s.Credentials[i].APIKey = nil
		}
		if s.Credentials[i].OAuth != nil {
			return fmt.Errorf("oauth: the standalone block names service %q, which already "+
				"declares an 'oauth:' block under credentials[]; keep only one",
				o.Service)
		}
		s.Credentials[i].OAuth = v2
		w.deprecate("oauth: (standalone block)", "use 'credentials[].oauth' instead")
		return nil
	}

	s.Credentials = append(s.Credentials, Credential{Service: o.Service, OAuth: v2})
	w.deprecate("oauth: (standalone block)", "use 'credentials[].oauth' instead")
	return nil
}

// isRoutingOnlyAPIKey reports whether k is an apiKey that exists only to route domains: it has
// injection targets but no variable name to populate, which is what the v1 serviceDomains fold
// produces for a service that turns out to be OAuth-only.
func isRoutingOnlyAPIKey(k *APIKey) bool {
	return k != nil && k.Name == "" && len(k.Inject) > 0
}

// translateV1Environment copies environment.variables. proxyManaged is consumed by
// translateV1Credentials and has no home here — v2's Environment has no such field.
func translateV1Environment(sf *specV1, s *Spec, _ *warnings) {
	if sf.Environment == nil || len(sf.Environment.Variables) == 0 {
		return
	}
	s.Environment = &Environment{Variables: sf.Environment.Variables}
}

// translateV1Setup renames commands: to setup: and commands.initFiles to setup.files.
func translateV1Setup(sf *specV1, s *Spec, w *warnings) {
	c := sf.Commands
	if c == nil {
		return
	}
	s.Setup = &Setup{Install: c.Install, Startup: c.Startup, Files: c.InitFiles}
	w.deprecate("commands:", "renamed to 'setup:'")
	if len(c.InitFiles) > 0 {
		w.deprecate("commands.initFiles", "renamed to 'setup.files'")
	}
}

// translateV1Volumes folds the v1 volumes mapping form and the removed top-level tmpfs: block
// into the single v2 volumes list.
func translateV1Volumes(sf *specV1, s *Spec, w *warnings) {
	s.Volumes = append(s.Volumes, sf.Volumes.List...)

	if len(sf.Volumes.Sizes) > 0 {
		for _, path := range sortedKeys(sf.Volumes.Sizes) {
			s.Volumes = append(s.Volumes, Volume{Path: path, Size: sf.Volumes.Sizes[path]})
		}
		w.deprecate("volumes: (mapping form)", "use the sequence form: '- path: <path>' entries")
	}

	if len(sf.Tmpfs) > 0 {
		for _, path := range sortedKeys(sf.Tmpfs) {
			s.Volumes = append(s.Volumes, Volume{
				Path: path,
				Type: MountTypeTmpfs,
				Size: sf.Tmpfs[path],
			})
		}
		w.deprecate("tmpfs:", "use 'volumes:' entries with 'type: "+
			string(MountTypeTmpfs)+"' instead")
	}
}

// translateV1Removed warns about the v1 fields that have no v2 home and are dropped. See the
// comment on specV1.Settings for why dropping rather than rejecting is right for these.
func translateV1Removed(sf *specV1, w *warnings) {
	if sf.Settings != nil {
		w.drop("settings:", "the block hardcoded agent-specific setup; write the same "+
			"configuration with 'setup.files' entries instead")
	}
	if sf.Persistence != "" {
		w.drop("persistence:", "the field was parsed but never wired to any runtime "+
			"behaviour; declare volumes: explicitly instead")
	}
	if sf.KitDir != "" {
		w.drop("kitDir:", "the field was parsed but never used")
	}
}

// sortedKeys returns m's keys in sorted order. Every fold in this file iterates maps through
// it, because Go randomises map iteration order and the translated Spec would otherwise differ
// between runs for byte-identical input — which is both untestable and, for a model that gets
// hashed or compared, wrong.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// warnings collects the non-fatal notes a load produces. A slice rather than a set: order is
// meaningful (it follows the order surfaces were consumed) and duplicates are already
// prevented at each call site.
type warnings []string

// deprecate records a legacy field that WAS translated. The distinction from drop matters to
// the reader: a deprecation is safe to ignore until the field stops loading, a drop has
// already changed what the kit does.
func (w *warnings) deprecate(field, advice string) {
	*w = append(*w, fmt.Sprintf("%s is deprecated: %s", field, advice))
}

// drop records a field that was accepted and then discarded, taking whatever it described
// with it.
func (w *warnings) drop(field, advice string) {
	*w = append(*w, fmt.Sprintf("%s was dropped: %s", field, advice))
}

// ignore records a field that was kept in the model but has no effect.
func (w *warnings) ignore(field, advice string) {
	*w = append(*w, fmt.Sprintf("%s is ignored: %s", field, advice))
}

// has reports whether any warning contains substr. Used by tests to assert that a particular
// legacy surface was noticed, without pinning the exact wording of the message.
func (w warnings) has(substr string) bool {
	for _, m := range w {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}
