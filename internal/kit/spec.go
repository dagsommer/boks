// Package kit decodes and validates Docker Sandboxes kit specs (spec.yaml).
//
// A kit is a directory with a required spec.yaml and an optional files/ tree. This package
// covers spec.yaml only: the files/ tree, composition (extends / mixins), and anything that
// actually launches a container are out of scope here. See the "Not implemented" list at the
// bottom of this file for the precise boundary.
//
// Two schema versions exist. Spec below is the schemaVersion "2" grammar, and it is also the
// canonical in-memory model: a schemaVersion "1" spec.yaml is decoded by a separate legacy
// grammar (specV1, decode_v1.go) and translated onto this same Spec, so every consumer sees
// one shape regardless of what was on disk. That mirrors how Docker's own spec library works,
// and it is the reason SchemaVersion is preserved verbatim on the translated result rather
// than being rewritten to "2" — downstream behaviour (notably credential resolution, which is
// binding-driven for v2 and host-discovery-driven for v1) keys on which grammar the author
// actually wrote.
//
// # Sources
//
// Field names and shapes here follow, in decreasing authority:
//
//  1. the real spec.yaml files of the ~35 kits in docker/sbx-kits-contrib (copied into
//     testdata/contrib/ and parsed by every run of TestContribKitsParse),
//  2. Docker's Go reference implementation of the spec library (spec/types.go, spec/v2.go,
//     spec/normalize.go, spec/validate.go in that same repository),
//  3. the rendered "Kit spec reference" documentation page,
//  4. the kit-author skill's prose topics (spec-anatomy.md, v1-migration.md).
//
// Where those disagree, the lower number wins and the disagreement is recorded in a comment
// at the field. Every such comment in this package names a specific artefact that was read;
// none of them is a guess. The disagreements found are collected on Spec.Kind, Sandbox.Build,
// StartupCommand.Command, InstallCommand.User and Volume.Type.
package kit

// Schema versions this package decodes. Both produce a *Spec.
//
// These are strings, not integers, in the YAML: `schemaVersion: "2"`. An unquoted `2` decodes
// as an integer and yaml.v3 will not assign it to a string field, so it fails with a type
// error rather than being silently coerced. That is the behaviour the reference has and it is
// worth keeping — a spec that says `schemaVersion: 2` is a spec whose author has not read the
// grammar, and quietly accepting it would let the rest of the file be misread too.
const (
	SchemaVersion1 = "1"
	SchemaVersion2 = "2"
)

// SupportedSchemaVersions is the set of schemaVersion values this package accepts, in the
// order they are reported in error messages.
var SupportedSchemaVersions = []string{SchemaVersion1, SchemaVersion2}

// Kit kinds.
//
// KindAgent is the v1 spelling of KindSandbox. It is not valid in a schemaVersion "2" spec —
// translateV1 rewrites it — but it is accepted on the v1 path, and the constant exists so the
// translation and its test can name it rather than repeating a bare string.
const (
	KindSandbox = "sandbox"
	KindMixin   = "mixin"
	KindAgent   = "agent"
)

// Spec is a decoded kit spec.yaml in its canonical (schemaVersion "2") shape.
//
// The yaml tags on this struct ARE the v2 grammar: decodeV2 uses Spec directly as its strict
// decode target, so a field that is not spelled here is a decode error, and every legacy v1
// key is therefore rejected in a v2 spec for free rather than by an explicit blocklist. That
// is deliberate — an explicit blocklist would have to be kept in sync with the v1 grammar by
// hand and would silently stop rejecting anything the author of the blocklist forgot.
type Spec struct {
	// SchemaVersion is "1" or "2". Preserved verbatim through v1 translation; see the
	// package comment for why it is not rewritten to "2".
	SchemaVersion string `yaml:"schemaVersion"`

	// Kind is KindSandbox or KindMixin.
	//
	// The rendered reference calls this required and lists only mixin|sandbox. All 35
	// contrib kits set it, so "required" is consistent with the corpus. KindAgent is
	// accepted only on the v1 path.
	Kind string `yaml:"kind"`

	// Name is the kit identifier: lowercase alphanumeric with interior hyphens, 1-64
	// characters. See namePattern in validate.go for the exact charset.
	Name string `yaml:"name"`

	// Version is the KIT's version, not the version of the tool the kit installs. Optional;
	// only 3 of 35 contrib kits set it. droid/spec.yaml spells out the distinction at
	// length and CI there refuses a release tag that disagrees with it.
	Version string `yaml:"version,omitempty"`

	// DisplayName is the human-readable name. Optional per the reference, but set by all 35
	// contrib kits.
	DisplayName string `yaml:"displayName,omitempty"`

	// Description is a short description. Optional per the reference, but set by all 35
	// contrib kits.
	Description string `yaml:"description,omitempty"`

	// SourceURL is the kit's source repository or documentation URL. No contrib kit sets
	// it, so its shape is documented but not corpus-verified.
	SourceURL string `yaml:"sourceURL,omitempty"`

	// Licenses are SPDX license identifiers. The reference notes implementations SHOULD
	// warn on unrecognised identifiers; this package does not, because doing so needs an
	// embedded SPDX list. Duplicates and empty entries are rejected.
	Licenses []string `yaml:"licenses,omitempty"`

	// Locked lists dotted paths a child kit may not override. This package validates only
	// that each entry is a well-formed dotted path and that there are no duplicates —
	// whether a path is meaningful is a question for the composition step, which this
	// package does not implement.
	Locked []string `yaml:"locked,omitempty"`

	// Security carries container security settings.
	Security *Security `yaml:"security,omitempty"`

	// Extends names a single parent kit to inherit from. Sandbox-only: a kind: mixin with
	// extends is rejected for schemaVersion "2". A sandbox that extends a parent inherits
	// the parent's image and may therefore omit its own sandbox: block entirely.
	//
	// Resolving the reference (a bare name, a local path, a git+https URL or an oci:// ref)
	// is composition, which this package does not implement: the string is decoded and
	// carried, not followed.
	Extends string `yaml:"extends,omitempty"`

	// Mixins lists kits to layer on, sandbox-only. Accepted by the reference parser with
	// runtime support pending, and likewise carried-not-followed here.
	Mixins []string `yaml:"mixins,omitempty"`

	// Requires carries composition preconditions — today only base-agent affinity. Valid
	// only on kind: mixin; on a kind: sandbox it is a validation error rather than being
	// ignored, because a sandbox IS a base agent and affinity there would never be
	// enforced. 6 of 35 contrib kits set it.
	Requires *Requires `yaml:"requires,omitempty"`

	// Args declares the arguments a kit installer supplies, referenced elsewhere in the
	// spec as ${{ kit.args.<name> }}.
	//
	// Substitution happens BEFORE the spec is decoded, so by the time this struct is
	// populated every placeholder is already a literal. This package does not perform that
	// substitution: it decodes and validates the declarations only. A spec.yaml still
	// carrying unsubstituted placeholders will therefore decode with the placeholder text
	// as the field value, and no error is raised for it.
	//
	// Documented in the kit-author skill's spec-anatomy.md and present in the reference
	// implementation, but absent from the rendered reference page's field tables. No
	// contrib kit uses it, so the shape here is from the reference implementation only.
	Args map[string]Arg `yaml:"args,omitempty"`

	// Sandbox is the container-launch block. Required for kind: sandbox unless Extends
	// supplies the image; forbidden for kind: mixin.
	Sandbox *Sandbox `yaml:"sandbox,omitempty"`

	// AgentInstructions groups the agent-instruction fields. This is a TOP-LEVEL block, not
	// a member of Sandbox — a v1 spec spelled the filename half as sandbox.aiFilename and
	// the content half as a top-level memory:/agentContext:, and v2 moved both here.
	AgentInstructions *AgentInstructions `yaml:"agentInstructions,omitempty"`

	// Permissions is the capability-grant block; today it carries only network egress.
	Permissions *Permissions `yaml:"permissions,omitempty"`

	// Ports are container ports to publish on the host. Inbound service exposure, a
	// separate concern from the outbound egress under Permissions.Network.
	Ports []Port `yaml:"ports,omitempty"`

	// Credentials declares what the kit needs and how the proxy injects it. It does NOT
	// declare where the value comes from: v2 moved credential discovery out of the kit and
	// into a user-side bindings file, which is why there is no env/file/parser shape here
	// the way v1 had under credentials.sources.
	Credentials []Credential `yaml:"credentials,omitempty"`

	// Environment carries static container environment variables.
	Environment *Environment `yaml:"environment,omitempty"`

	// Setup seeds files and runs install/startup commands.
	Setup *Setup `yaml:"setup,omitempty"`

	// Volumes are mount entries. One list for both block-backed and tmpfs mounts, selected
	// by each entry's Type — v1 had a separate top-level tmpfs: block, which v2 removed.
	Volumes []Volume `yaml:"volumes,omitempty"`
}

// Security holds container security settings.
type Security struct {
	// Privileged runs the container in privileged mode.
	Privileged bool `yaml:"privileged,omitempty"`
}

// Requires carries a kit's composition preconditions.
type Requires struct {
	// Agent is the single base-agent name a mixin is designed for. Deliberately one name
	// rather than a set: the point of affinity is to prevent misapplication, and an
	// "any of these" list would defeat that. This package validates only that the name is
	// well-formed; whether a given base satisfies it is decided at composition time.
	Agent string `yaml:"agent,omitempty"`
}

// Arg declares one installer-supplied argument.
type Arg struct {
	// Default is the value used when the installer supplies none.
	//
	// A pointer because an empty-string default is a real default and must be
	// distinguishable from "no default declared" — with a plain string, `default: ""` and
	// an absent default would both read as "". The reference implementation makes the same
	// choice for the same reason.
	Default *string `yaml:"default,omitempty"`

	// Required marks an argument the installer must supply. It is the explicit spelling of
	// "no default", so declaring it alongside Default is an error.
	Required bool `yaml:"required,omitempty"`

	// Description is the human-readable explanation.
	Description string `yaml:"description,omitempty"`

	// Enum restricts the value to an exact set. Mutually exclusive with Pattern.
	Enum []string `yaml:"enum,omitempty"`

	// Pattern restricts the value to a Go (RE2) regexp matched against the whole value.
	// Mutually exclusive with Enum.
	Pattern string `yaml:"pattern,omitempty"`
}

// Sandbox is the container-launch block of a kind: sandbox kit.
type Sandbox struct {
	// Image is the container image reference. Required for a root sandbox kit; a sandbox
	// that sets Extends inherits its parent's image and may omit this.
	Image string `yaml:"image,omitempty"`

	// Build describes building the image from a Dockerfile.
	//
	// The reference page and spec-anatomy.md both state that build is accepted by the
	// parser but not wired to the runtime, and that a kit setting build must ALSO set
	// image, a build-only kit being rejected at load. validateSandbox enforces exactly
	// that. It is not a validation this package invented: the reference implementation
	// returns the same rejection from two places (spec/v2.go and spec/normalize.go).
	//
	// No contrib kit uses build:, so this shape is documented-and-referenced but not
	// corpus-verified. Note that the rendered reference page describes build as an
	// alternative to image ("either sandbox{...} or extends") while spec-anatomy.md calls
	// it "an *additional*, forward-compatible block — not an alternative". The latter
	// matches the reference implementation, so that is what is implemented.
	Build *BuildConfig `yaml:"build,omitempty"`

	// Entrypoint is the fixed process prefix as a FLAT string array: element 0 is the agent
	// binary and the rest are always-on arguments applied in both launch modes. v1 spelled
	// this as a mapping with run/args/ttyArgs/pipeMode; see specV1Entrypoint.
	Entrypoint []string `yaml:"entrypoint,omitempty"`

	// Command is the mode-specific argument tail appended after Entrypoint. Polymorphic in
	// YAML — a bare list is shorthand for the default tail. See Command.UnmarshalYAML.
	Command Command `yaml:"command,omitempty"`

	// Resources optionally constrains CPU, memory and GPU.
	Resources *Resources `yaml:"resources,omitempty"`
}

// BuildConfig describes building the sandbox image from a Dockerfile.
type BuildConfig struct {
	// Context is the build context directory, relative to spec.yaml. Defaults to "." where
	// builds are implemented; this package does not apply that default, because writing a
	// default into a field the runtime ignores would make an un-set value indistinguishable
	// from an explicit "." for no benefit.
	Context string `yaml:"context,omitempty"`

	// Dockerfile is the Dockerfile path relative to Context. Defaults to "Dockerfile" where
	// builds are implemented; not defaulted here, same reason as Context.
	Dockerfile string `yaml:"dockerfile,omitempty"`

	// Args are docker build --build-arg values. Unrelated to Spec.Args, which are kit
	// installer arguments; the two share a name in the grammar and nothing else.
	Args map[string]string `yaml:"args,omitempty"`

	// Target selects a multi-stage build target.
	Target string `yaml:"target,omitempty"`

	// Platforms lists target platforms, e.g. linux/amd64.
	Platforms []string `yaml:"platforms,omitempty"`
}

// Resources constrains the container's CPU, memory and GPU.
type Resources struct {
	// CPU is a core count; fractional values are allowed. Must be non-negative.
	CPU float64 `yaml:"cpu,omitempty"`

	// Memory is a byte-size string such as "4g" or "4096m". Kept as the author's string
	// rather than parsed into a number at decode time so that a validation error can quote
	// what was actually written; validateResources checks that it parses.
	Memory string `yaml:"memory,omitempty"`

	// GPU is a consumer-defined allocation string, e.g. "1" or "all". Deliberately a
	// string and not validated: the reference calls the format consumer-defined, so there
	// is no set of legal values to check against.
	GPU string `yaml:"gpu,omitempty"`
}

// AgentInstructions groups the agent-instruction fields.
type AgentInstructions struct {
	// Filename is the AI profile filename the sandbox owns, e.g. "AGENTS.md".
	//
	// Meaningful only for kind: sandbox. A mixin contributes TO a base sandbox's profile
	// rather than owning one, so a filename on a mixin is ignored with a warning and not an
	// error — see warnMixinFilename. That asymmetry (warn, don't reject) is the
	// reference's, and it matters: rejecting would break mixins that set it harmlessly.
	Filename string `yaml:"filename,omitempty"`

	// Content is markdown instructions. For a sandbox kit it is inlined into the profile
	// file; for a mixin it is written to a separate per-kit file that the profile points
	// at. This package decodes the text and does not render either form.
	Content string `yaml:"content,omitempty"`
}

// Permissions is the top-level capability-grant block.
type Permissions struct {
	// Network is the egress policy. The only capability the grammar carries today.
	Network *NetworkPermissions `yaml:"network,omitempty"`
}

// NetworkPermissions is the egress allow/deny policy.
//
// Entry syntax spans exact hosts, host:port, single- and multi-label wildcards, port ranges,
// port wildcards and CIDR blocks, of which the reference marks only the first three as
// enforced today. This package does not parse or classify the entries at all — see the
// "Not implemented" note in validate.go for why that is a deliberate omission and not an
// oversight.
type NetworkPermissions struct {
	// Allow lists domains the sandbox may reach.
	Allow []string `yaml:"allow,omitempty"`

	// Deny lists domains the sandbox must not reach. Deny wins over Allow, including
	// across composed kits, so overlap between the two lists is legal and intentional.
	Deny []string `yaml:"deny,omitempty"`
}

// Port is one container port to publish on the host.
//
// There is no host-port field, and that is by design rather than an omission: the host port
// is always allocated ephemerally on 127.0.0.1, because two kits asking for the same host
// port would collide on the user's machine. Pinning is a user-side operation.
type Port struct {
	// Container is the in-container port, 1..65535.
	Container int `yaml:"container"`

	// Protocol is "tcp" or "udp". Empty means tcp.
	Protocol string `yaml:"protocol,omitempty"`

	// Name is an informational label, not an identifier — two kits may declare the same
	// name without conflict.
	Name string `yaml:"name,omitempty"`
}

// Credential is one service's credential requirement and injection configuration.
type Credential struct {
	// Service is the credential identifier the user-side binding is matched on.
	//
	// The v2 specification gives it a lowercase-kebab charset, and this package does NOT
	// enforce that. The reason is specific and is the reference's own: the v1 translation
	// synthesises service keys from environment variable names, which keeps an underscore
	// for any multi-word variable (SAMPLE_PROXY_TOKEN yields "sample_proxy"), so enforcing
	// the pattern would reject v1 kits that load today. Non-empty is enforced.
	Service string `yaml:"service"`

	// Description is shown to the user when approving a binding.
	Description string `yaml:"description,omitempty"`

	// Required marks the credential as essential. The reference page and spec-anatomy.md
	// disagree on the consequence — the page says sbx warns and starts with the credential
	// withheld, spec-anatomy says sandbox creation fails. Both are runtime behaviour that
	// this package does not implement, so the disagreement does not need resolving here;
	// it is recorded because a caller acting on this field will have to pick one.
	Required bool `yaml:"required,omitempty"`

	// Provider is reserved for a future provider registry. Accepted with a warning and no
	// effect. See warnCredentialProvider.
	Provider string `yaml:"provider,omitempty"`

	// APIKey configures API-key injection.
	//
	// The YAML key is "apiKey" (lower a). The Go name is APIKey per Go initialism style,
	// which is why the tag is spelled out rather than relying on any name-derived default.
	APIKey *APIKey `yaml:"apiKey,omitempty"`

	// OAuth configures OAuth interception.
	OAuth *OAuth `yaml:"oauth,omitempty"`
}

// APIKey is the API-key half of a credential.
type APIKey struct {
	// Name is the environment variable name for the credential, e.g. ANTHROPIC_API_KEY.
	// Authors never put a real value in a spec; the engine sets this variable to a
	// proxy-managed sentinel in the container.
	Name string `yaml:"name,omitempty"`

	// ProxyManaged, when true, makes the engine set Name to the sentinel value inside the
	// container. Default false, in which case the proxy still injects on the domains below
	// but the variable is not set in-container. This field is what v1 spelled as a
	// separate top-level environment.proxyManaged list of variable names.
	ProxyManaged bool `yaml:"proxyManaged,omitempty"`

	// Inject lists the domains the credential is injected into, and how.
	Inject []APIKeyInject `yaml:"inject,omitempty"`
}

// APIKeyInject is one domain the proxy injects a credential into.
type APIKeyInject struct {
	// Domain is the domain to inject into. It must also appear in
	// Permissions.Network.Allow, or the sandbox cannot reach it whatever this says. That
	// cross-check is NOT performed here — see validateCredentials for why.
	Domain string `yaml:"domain,omitempty"`

	// Header is the HTTP header the proxy sets, e.g. x-api-key or Authorization.
	Header string `yaml:"header,omitempty"`

	// Format is the header value format with exactly one %s placeholder, e.g. "Bearer %s".
	// Mutually exclusive with Scheme.
	Format string `yaml:"format,omitempty"`

	// Username is the username for HTTP Basic auth — the proxy uses it as the username and
	// the resolved credential as the password. Required with Scheme "basic". The canonical
	// example is the literal x-access-token for git over HTTPS.
	Username string `yaml:"username,omitempty"`

	// Scheme is decode-time shorthand for Header+Format: "bearer" or "basic".
	//
	// ExpandSchemes rewrites it into the canonical fields and clears it, so a Spec that has
	// been through ParseSpec never carries a Scheme. It is spelled out here rather than
	// hidden because a caller constructing a Spec in Go can set it, and must then know that
	// ExpandSchemes is what makes it take effect.
	Scheme string `yaml:"scheme,omitempty"`
}

// Recognised APIKeyInject.Scheme values.
const (
	// SchemeBearer expands to Header "Authorization" and Format "Bearer %s".
	SchemeBearer = "bearer"

	// SchemeBasic marks HTTP Basic auth, which is username-driven at the proxy rather than
	// a header encoding.
	SchemeBasic = "basic"
)

// OAuth is the OAuth half of a credential.
//
// The proxy intercepts the token endpoint's responses, replaces the real tokens with
// sentinels, and swaps the real token back in on outbound requests to the resource hosts. The
// effect is that the real token never enters the sandbox unless Passthrough opts out.
type OAuth struct {
	// TokenEndpoint is the endpoint the proxy intercepts. Both host and path are required.
	TokenEndpoint OAuthTokenEndpoint `yaml:"tokenEndpoint"`

	// Sentinels are the values written into the container in place of the real tokens.
	// Required unless Passthrough is set, in which case there is nothing to mask.
	Sentinels OAuthSentinels `yaml:"sentinels,omitempty"`

	// ResourceHosts are the API hosts where the proxy attaches the token on outbound
	// requests. Distinct from TokenEndpoint.Host, which is only where the token is
	// refreshed. Domains only, with no per-host header configuration, because the bearer
	// header is uniform and supplied by the OAuth engine.
	ResourceHosts []string `yaml:"resourceHosts,omitempty"`

	// CredentialFile optionally writes the credential to a file in the container.
	CredentialFile *OAuthCredentialFile `yaml:"credentialFile,omitempty"`

	// SkipIfEnv is accepted and IGNORED for schemaVersion "2".
	//
	// This is the one v1 field that a v2 spec may still carry, and it is not an oversight:
	// v2 credential resolution is binding-driven, so a host environment variable can never
	// gate OAuth, but the field is kept in the grammar so migrated specs load unchanged.
	// droid/spec.yaml is a live example — it carries skipIfEnv: [FACTORY_API_KEY] with a
	// comment saying so. Making this a decode error would break that kit today.
	//
	// warnSkipIfEnv records the fact that it was seen and dropped. To get conditional
	// behaviour, declare both APIKey and OAuth on one credential: the API key wins when it
	// resolves and OAuth is the fallback.
	SkipIfEnv []string `yaml:"skipIfEnv,omitempty"`

	// ResponseFields overrides the JSON field names the proxy reads from the token
	// response, for providers that do not use the OAuth 2.0 RFC spellings.
	ResponseFields *OAuthResponseFields `yaml:"responseFields,omitempty"`

	// Passthrough sends the real token response into the sandbox instead of masking it with
	// sentinels. A security downgrade, and the reason Sentinels stops being required.
	// The v1 spelling of this field was passthroughResponse.
	Passthrough bool `yaml:"passthrough,omitempty"`
}

// OAuthTokenEndpoint is the OAuth token endpoint the proxy intercepts.
type OAuthTokenEndpoint struct {
	Host string `yaml:"host,omitempty"`
	Path string `yaml:"path,omitempty"`
}

// OAuthSentinels are the placeholder token values written into the container.
type OAuthSentinels struct {
	AccessToken  string `yaml:"accessToken,omitempty"`
	RefreshToken string `yaml:"refreshToken,omitempty"`
}

// OAuthCredentialFile describes a credential file written inside the container.
type OAuthCredentialFile struct {
	// Path is where to write the file in the container; a leading ~ expands.
	Path string `yaml:"path,omitempty"`

	// Template is a Go text/template rendering the file, supporting .AccessToken,
	// .RefreshToken, .ExpiresAt, .Scopes and .ScopesJSON. Deprecated in favour of
	// Structure, and slated for removal, but still the only one that works — see Structure.
	Template string `yaml:"template,omitempty"`

	// Structure is a declarative JSON shape with the same placeholders, which the engine
	// encodes as JSON and then substitutes, guaranteeing well-formed output.
	//
	// The rendered reference page is explicit that structure is "defined by schema v2 but
	// not supported by the sbx engine" and that "a structure-only kit fails validation",
	// while spec-anatomy.md calls structure "preferred" and shows it in its examples. Those
	// cannot both describe the same release. This package sides with the reference page's
	// statement about validation only to the extent of requiring that one of Template or
	// Structure be present — it does NOT reject a structure-only credential file, because
	// the reference implementation's ValidateOAuth accepts either, and rejecting would
	// contradict the code over a docs sentence. Recorded rather than resolved.
	Structure map[string]any `yaml:"structure,omitempty"`
}

// OAuthResponseFields maps logical OAuth token fields to the endpoint's actual JSON field
// names. An empty field means "use the OAuth 2.0 RFC default"; ResolvedResponseFields applies
// those defaults.
type OAuthResponseFields struct {
	AccessToken  string `yaml:"accessToken,omitempty"`
	RefreshToken string `yaml:"refreshToken,omitempty"`
	ExpiresIn    string `yaml:"expiresIn,omitempty"`
	Scope        string `yaml:"scope,omitempty"`
}

// ResolvedResponseFields returns o's response-field mapping with the OAuth 2.0 RFC defaults
// filled in for anything the spec left empty. It is defined on OAuth rather than on
// OAuthResponseFields so that a nil ResponseFields — the common case — still yields the
// defaults instead of needing a nil check at every call site.
func (o *OAuth) ResolvedResponseFields() OAuthResponseFields {
	f := OAuthResponseFields{
		AccessToken:  "access_token",
		RefreshToken: "refresh_token",
		ExpiresIn:    "expires_in",
		Scope:        "scope",
	}
	if o == nil || o.ResponseFields == nil {
		return f
	}
	if v := o.ResponseFields.AccessToken; v != "" {
		f.AccessToken = v
	}
	if v := o.ResponseFields.RefreshToken; v != "" {
		f.RefreshToken = v
	}
	if v := o.ResponseFields.ExpiresIn; v != "" {
		f.ExpiresIn = v
	}
	if v := o.ResponseFields.Scope; v != "" {
		f.Scope = v
	}
	return f
}

// Environment carries static container environment variables.
//
// v1 also had environment.proxyManaged here, a list of variable names. v2 removed it: the
// proxy-managed semantic moved onto APIKey.ProxyManaged, per credential, so there is no
// separate list to keep in sync. translateV1 folds the v1 list onto the credentials.
type Environment struct {
	// Variables are set directly in the container. Keys must be valid shell identifiers.
	Variables map[string]string `yaml:"variables,omitempty"`
}

// Setup seeds files and runs commands. All three lists are optional and, under composition,
// concatenate in kit order — which is why an install command must be idempotent.
type Setup struct {
	// Install runs synchronously when the kit is applied.
	Install []InstallCommand `yaml:"install,omitempty"`

	// Startup runs at every sandbox start. Must be idempotent: it replays on container
	// restart.
	Startup []StartupCommand `yaml:"startup,omitempty"`

	// Files are written at sandbox start. v1 spelled this commands.initFiles.
	Files []SetupFile `yaml:"files,omitempty"`
}

// InstallCommand is one synchronous install step.
type InstallCommand struct {
	// Command is a shell command STRING, passed to sh -c, so shell metacharacters work as
	// written. A list here is a type error, deliberately: the reference is explicit that
	// install takes the string form only, and 28 contrib kits use it that way.
	Command string `yaml:"command,omitempty"`

	// User is the user to run as, defaulting to "0" (root).
	//
	// The reference documents this as a uid ("0" = root, "1000" = agent) and this package
	// deliberately does NOT validate it as numeric, because the corpus contradicts the
	// documentation: across the contrib kits the observed values are "0" (29), "1000"
	// (19), "root" (3), "agent" (2) and one unquoted root. droid/spec.yaml uses
	// user: "root" on a startup command. Enforcing a uid would reject four shipping kits,
	// so this is a free-form user reference and only emptiness-by-omission is meaningful.
	User string `yaml:"user,omitempty"`

	// Description is human-readable.
	Description string `yaml:"description,omitempty"`
}

// StartupCommand is one command run at every sandbox start.
type StartupCommand struct {
	// Command is argv as a string array, run without a shell.
	//
	// List-only, which is a documentation conflict worth stating: the rendered reference
	// page says "String array, not interpreted by a shell", the reference implementation's
	// type is []string, and all 12 startup commands across the contrib corpus are lists.
	// Only the kit-author skill's spec-anatomy.md claims a bare string is also accepted.
	// Three sources against one, including the code and the corpus, so list-only it is.
	// A spec relying on the string form will fail to decode with a type error naming this
	// field, which is the right outcome for a form nothing implements.
	//
	// The canonical way to get a shell here is an explicit ["sh", "-c", "..."], which is
	// what 8 of the 12 corpus entries do.
	Command []string `yaml:"command,omitempty"`

	// User is the user to run as, defaulting to "1000" (agent). Free-form; see
	// InstallCommand.User.
	User string `yaml:"user,omitempty"`

	// Background, when true, lets later startup commands run without waiting for this one.
	//
	// It does not gate the agent's entrypoint either way: the agent launches once startup
	// commands have been dispatched. background: false waits inside the startup dispatcher
	// before the next command, and that is all it does.
	Background bool `yaml:"background,omitempty"`

	// Description is human-readable.
	Description string `yaml:"description,omitempty"`
}

// SetupFile is one file written at sandbox start.
type SetupFile struct {
	// Path is an absolute container path.
	Path string `yaml:"path,omitempty"`

	// Content is the file content. ${WORKDIR} expands to the workspace path at write time;
	// this package does not perform that expansion and does not reject other placeholders.
	Content string `yaml:"content,omitempty"`

	// Mode is octal permissions, defaulting to "0644".
	Mode string `yaml:"mode,omitempty"`

	// OnlyIfMissing skips the write if the file already exists — the way to stay idempotent
	// across restarts and across a persistent volume.
	OnlyIfMissing bool `yaml:"onlyIfMissing,omitempty"`

	// Description is human-readable.
	Description string `yaml:"description,omitempty"`
}

// MountType selects a volume's backing storage.
type MountType string

// Recognised MountType values.
const (
	// MountTypeBlock is a block-backed volume. Encoded as the empty string, so an entry
	// that omits type: gets it.
	MountTypeBlock MountType = ""

	// MountTypeTmpfs is a RAM-backed mount.
	MountTypeTmpfs MountType = "tmpfs"
)

// Volume is one mount entry.
//
// Volumes are applied only at sandbox creation, so a kit added to a running sandbox cannot
// attach one. No contrib kit declares a volume, so this shape is documented-and-referenced
// but not corpus-verified.
type Volume struct {
	// Path is the required absolute container path.
	Path string `yaml:"path"`

	// Type is the backing storage: empty for block-backed, "tmpfs" for RAM-backed.
	Type MountType `yaml:"type,omitempty"`

	// Size is an optional byte-size string.
	Size string `yaml:"size,omitempty"`

	// Mode is optional octal permissions.
	Mode string `yaml:"mode,omitempty"`
}

// Not implemented by this package, stated explicitly so a caller does not assume otherwise:
//
//   - The files/ tree. Nothing here reads a kit directory; ParseSpec takes spec.yaml bytes.
//     The path rules for files/home/ and files/workspace/ (no absolute paths, no ..
//     traversal, symlinks confined to the artifact root, other subdirectories ignored with a
//     warning) are therefore unenforced.
//   - Composition. Extends, Mixins and Requires.Agent are decoded and validated for
//     well-formedness only. No reference is resolved, no parent is merged, and no affinity is
//     checked against an actual base agent.
//   - Argument substitution. Spec.Args declarations are decoded and validated; the
//     ${{ kit.args.<name> }} substitution that would consume them is not performed, and an
//     undeclared reference is not detected.
//   - Network pattern parsing. Allow and deny entries are carried as written.
//   - The credential/egress cross-check. See validateCredentials.
//   - Reserved environment variable prefixes and the reserved-name warnings. See
//     validateEnvironment.
//   - Any runtime behaviour: image pulls, builds, command execution, proxy injection, OAuth
//     interception, file writes, volume creation, port publishing.
//   - Serialisation back to YAML. The yaml tags carry omitempty and are shaped for
//     round-tripping, but no marshalling path is tested, so emitting a Spec is unverified.
//     In particular Command has no MarshalYAML, so it would emit as a mapping and never as
//     the list shorthand.
