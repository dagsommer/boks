package kit

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Every error in this file names the field path it is about, in the spelling the author would
// find in their spec.yaml — "credentials[2].apiKey.inject[0].format", not a Go field name. That
// is the difference between an error the author can act on and one they have to bisect the file
// to locate. Where a value is at fault the error quotes it, because "invalid mode" without the
// mode sends the reader looking at the wrong line.
//
// Field paths use the v2 spelling even when the spec that was loaded is v1, because by the time
// Validate runs the model IS v2-shaped. A v1 author whose commands.initFiles entry is bad is
// therefore told about setup.files[i]. That is a real rough edge and it is not worked around
// here: threading provenance through every field to reproduce the original spelling would cost
// more than it returns, and the warnings list already tells the author that commands: was
// renamed to setup:.

// namePattern is the charset for a kit name and for requires.agent: lowercase alphanumeric,
// interior hyphens allowed, 1-64 characters, first and last character not a hyphen.
var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// shellIdentifierPattern is the charset for an environment variable name.
var shellIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// lockedPathPattern is the shape of a dotted path in locked:, e.g. "sandbox.image".
//
// Note that the reference documents locked entries such as "credentials[service=anthropic]",
// with a bracketed selector, which this pattern REJECTS — it admits dotted lowerCamelCase
// segments only. The pattern is the reference implementation's own, so its validator rejects
// the example its documentation gives. Kept as-is rather than widened: no contrib kit uses
// locked at all, so there is no corpus evidence for which is right, and matching the code is
// the safer of two unverified choices.
var lockedPathPattern = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*(\.[a-z][a-zA-Z0-9]*)*$`)

// octalModePattern is the shape of a file or mount mode string, e.g. "0644" or "1777".
var octalModePattern = regexp.MustCompile(`^0?[0-7]{3,4}$`)

// argNamePattern is the charset for a key in args:.
var argNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// Validate checks a Spec for internal consistency.
//
// It assumes ExpandSchemes has already run: an unexpanded scheme would make an inject entry
// look like it has no header or format. ParseSpec sequences the two correctly.
//
// Checks are ordered identity-first — schemaVersion, kind, name — so that a spec which is
// wrong about what it is gets told that rather than being told about the fourth field of its
// third credential.
func Validate(s *Spec) error {
	if s == nil {
		return fmt.Errorf("spec is nil")
	}

	if s.SchemaVersion == "" {
		return fmt.Errorf("schemaVersion is required (supported: %s)",
			strings.Join(SupportedSchemaVersions, ", "))
	}
	if !slices.Contains(SupportedSchemaVersions, s.SchemaVersion) {
		return fmt.Errorf("schemaVersion %q is not supported (supported: %s)",
			s.SchemaVersion, strings.Join(SupportedSchemaVersions, ", "))
	}

	if s.Kind == "" {
		return fmt.Errorf("kind is required (want %q or %q)", KindSandbox, KindMixin)
	}
	// KindAgent is deliberately not accepted here even though it is v1-legal: translateV1
	// rewrites it before Validate ever sees it, so reaching this point with "agent" means a
	// caller constructed the Spec by hand and used the v1 spelling. Accepting it would make
	// the canonical model carry two spellings of one kind, which is what v2 set out to stop.
	if s.Kind != KindSandbox && s.Kind != KindMixin {
		hint := ""
		if s.Kind == KindAgent {
			hint = fmt.Sprintf(" (%q is the schemaVersion %s spelling of %q)",
				KindAgent, SchemaVersion1, KindSandbox)
		}
		return fmt.Errorf("kind %q is not valid (want %q or %q)%s",
			s.Kind, KindSandbox, KindMixin, hint)
	}

	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !namePattern.MatchString(s.Name) {
		return fmt.Errorf("name %q is not valid: use 1-64 characters of lowercase "+
			"alphanumerics and interior hyphens", s.Name)
	}

	if err := validateKindShape(s); err != nil {
		return err
	}
	if err := validateSandbox(s); err != nil {
		return err
	}
	if err := validateStringList("licenses", s.Licenses); err != nil {
		return err
	}
	if err := validateLocked(s.Locked); err != nil {
		return err
	}
	if err := validateArgs(s.Args); err != nil {
		return err
	}
	if err := validatePorts(s.Ports); err != nil {
		return err
	}
	if err := validateEnvironment(s.Environment); err != nil {
		return err
	}
	if err := validateSetup(s.Setup); err != nil {
		return err
	}
	if err := validateCredentials(s.Credentials); err != nil {
		return err
	}
	return validateVolumes(s.Volumes)
}

// validateKindShape enforces the rules about which top-level blocks each kind may carry. These
// are the cross-field rules, which is why they are here and not spread across the per-block
// validators.
func validateKindShape(s *Spec) error {
	switch s.Kind {
	case KindMixin:
		// A mixin layers onto an existing sandbox, so it defines no container of its own.
		if s.Sandbox != nil {
			return fmt.Errorf("sandbox: is not valid for kind %q — a %s layers onto an "+
				"existing sandbox rather than defining one; use kind %q",
				KindMixin, KindMixin, KindSandbox)
		}
		// extends and mixins are single- and multi-parent inheritance, both sandbox-only.
		// The extends prohibition is gated on schemaVersion "2" because the rule postdates
		// v1: a v1 mixin written before it still validates. That gate is the reference's,
		// and it is the right shape — a new rule that retroactively invalidates
		// previously-accepted specs is a breaking change dressed up as a validation.
		if s.Extends != "" && s.SchemaVersion == SchemaVersion2 {
			return fmt.Errorf("extends is not valid for kind %q (schemaVersion %s) — "+
				"mixins are base-agnostic additions; to derive from a parent agent use "+
				"a kind %q kit with extends",
				KindMixin, SchemaVersion2, KindSandbox)
		}
		if len(s.Mixins) > 0 {
			return fmt.Errorf("mixins is not valid for kind %q — a %s cannot compose "+
				"other mixins; only a kind %q kit can",
				KindMixin, KindMixin, KindSandbox)
		}

	case KindSandbox:
		// A sandbox IS a base agent, so base-agent affinity is meaningless on one. This is
		// rejected rather than ignored specifically so an author does not believe it is
		// being enforced when composition would silently skip it.
		if s.Requires != nil && s.Requires.Agent != "" {
			return fmt.Errorf("requires.agent is not valid for kind %q — base-agent "+
				"affinity applies to a %s layered onto an agent, and a %s is itself "+
				"the agent", KindSandbox, KindMixin, KindSandbox)
		}
		// A root sandbox must declare its container. One that extends a parent inherits
		// the parent's image, so the requirement is satisfied on the resolved artifact
		// rather than on this leaf.
		if s.Sandbox == nil && s.Extends == "" {
			return fmt.Errorf("kind %q requires a sandbox: block with at least "+
				"sandbox.image, or an extends: parent to inherit the image from",
				KindSandbox)
		}
		// Both is a contradiction rather than a merge: extends says "inherit the parent's
		// container", sandbox: says "here is mine", and which wins is not something the
		// grammar answers. Note this is stricter than the reference, whose ValidateArtifact
		// permits both and lets the composition step decide — a step this package does not
		// implement, so permitting the combination here would accept a spec nothing
		// downstream could act on.
		if s.Sandbox != nil && s.Extends != "" {
			return fmt.Errorf("sandbox: and extends: are mutually exclusive — extends " +
				"inherits the parent's container configuration, so declaring a " +
				"sandbox: block as well leaves it ambiguous which image applies")
		}
	}

	if r := s.Requires; r != nil && r.Agent != "" && !namePattern.MatchString(r.Agent) {
		return fmt.Errorf("requires.agent %q is not a valid kit name: use 1-64 characters "+
			"of lowercase alphanumerics and interior hyphens", r.Agent)
	}
	return nil
}

// validateSandbox checks the sandbox block, including the image-required-alongside-build rule.
func validateSandbox(s *Spec) error {
	sb := s.Sandbox
	if sb == nil {
		return nil
	}

	if sb.Image == "" && s.Extends == "" {
		// Reached when a sandbox: block exists but carries no image — the
		// build-without-image case included, which is why the error mentions build.
		msg := "sandbox.image is required"
		if sb.Build != nil {
			msg += ": sandbox.build is accepted by the grammar but builds are not " +
				"performed, so an image reference is still needed"
		}
		return fmt.Errorf("%s", msg)
	}

	if r := sb.Resources; r != nil {
		if r.CPU < 0 {
			return fmt.Errorf("sandbox.resources.cpu must be non-negative (got %v)", r.CPU)
		}
		if r.Memory != "" {
			if _, err := parseByteSize(r.Memory); err != nil {
				return fmt.Errorf("sandbox.resources.memory %q is not a valid size: %w",
					r.Memory, err)
			}
		}
	}

	// An entrypoint whose first element is empty has no binary, and element 0 is by
	// definition the binary. Checked because the value survives decoding silently: an
	// entrypoint of ["", "--flag"] is a well-formed string array.
	if len(sb.Entrypoint) > 0 && sb.Entrypoint[0] == "" {
		return fmt.Errorf("sandbox.entrypoint[0] must not be empty: it is the agent binary")
	}
	return nil
}

// validateStringList rejects empty and duplicate entries in a plain string list.
func validateStringList(field string, list []string) error {
	seen := make(map[string]struct{}, len(list))
	for i, v := range list {
		if v == "" {
			return fmt.Errorf("%s[%d] must not be empty", field, i)
		}
		if _, dup := seen[v]; dup {
			return fmt.Errorf("%s[%d] %q is a duplicate", field, i, v)
		}
		seen[v] = struct{}{}
	}
	return nil
}

// validateLocked checks that each locked path is a well-formed dotted path and unique. Whether
// a path is meaningful for inheritance is the composition step's question, not this one's.
func validateLocked(paths []string) error {
	if err := validateStringList("locked", paths); err != nil {
		return err
	}
	for i, p := range paths {
		if !lockedPathPattern.MatchString(p) {
			return fmt.Errorf("locked[%d] %q is not a well-formed dotted path "+
				"(e.g. \"sandbox.image\")", i, p)
		}
	}
	return nil
}

// validateArgs checks the args: declarations. The two mutual exclusions are the substance:
// required is the explicit spelling of "no default", and enum makes pattern redundant.
func validateArgs(args map[string]Arg) error {
	for _, name := range sortedKeys(args) {
		a := args[name]
		path := fmt.Sprintf("args.%s", name)
		if !argNamePattern.MatchString(name) {
			return fmt.Errorf("%s is not a valid argument name: start with a letter or "+
				"underscore, then letters, digits, underscores or hyphens", path)
		}
		if a.Required && a.Default != nil {
			return fmt.Errorf("%s: required and default are mutually exclusive — "+
				"required means the installer must supply a value, so a default "+
				"would never be used", path)
		}
		if !a.Required && a.Default == nil {
			return fmt.Errorf("%s: declare either a default or required: true, so it is "+
				"unambiguous whether the installer must supply a value", path)
		}
		if len(a.Enum) > 0 && a.Pattern != "" {
			return fmt.Errorf("%s: enum and pattern are mutually exclusive — an exact "+
				"set already constrains the value", path)
		}
		if err := validateStringList(path+".enum", a.Enum); err != nil {
			return err
		}
		if a.Pattern != "" {
			if _, err := regexp.Compile(a.Pattern); err != nil {
				return fmt.Errorf("%s.pattern %q is not a valid regular expression: %w",
					path, a.Pattern, err)
			}
		}
		// A default that its own constraints reject is a spec that cannot be installed
		// without supplying the argument, which defeats having a default at all.
		if a.Default != nil {
			if err := a.ValidateValue(*a.Default); err != nil {
				return fmt.Errorf("%s.default: %w", path, err)
			}
		}
	}
	return nil
}

// ValidateValue reports whether v satisfies a's enum or pattern constraint. Exported because
// the constraint belongs to the declaration, and whatever supplies argument values needs to
// apply it; this package never supplies any.
func (a Arg) ValidateValue(v string) error {
	if len(a.Enum) > 0 && !slices.Contains(a.Enum, v) {
		return fmt.Errorf("%q is not one of the permitted values %v", v, a.Enum)
	}
	if a.Pattern != "" {
		re, err := regexp.Compile(a.Pattern)
		if err != nil {
			return fmt.Errorf("pattern %q is not a valid regular expression: %w",
				a.Pattern, err)
		}
		// Anchored, because the constraint is documented as matching the whole value and
		// an unanchored RE2 match would accept any value merely containing a match.
		if !re.MatchString(v) {
			return fmt.Errorf("%q does not match pattern %q", v, a.Pattern)
		}
		if m := re.FindString(v); m != v {
			return fmt.Errorf("%q does not fully match pattern %q (only %q matches; "+
				"anchor the pattern with ^ and $ if that was intended)",
				v, a.Pattern, m)
		}
	}
	return nil
}

// validatePorts checks the published-port declarations.
func validatePorts(ports []Port) error {
	for i, p := range ports {
		if p.Container < 1 || p.Container > 65535 {
			return fmt.Errorf("ports[%d].container must be between 1 and 65535 (got %d)",
				i, p.Container)
		}
		switch p.Protocol {
		case "", "tcp", "udp":
			// Empty is accepted and means tcp at consumption time. It is deliberately
			// not rewritten to "tcp" here: doing so would make an omitted protocol
			// indistinguishable from an explicit one in the model for no gain.
		default:
			return fmt.Errorf("ports[%d].protocol %q is not valid (want \"tcp\", "+
				"\"udp\", or omitted for tcp)", i, p.Protocol)
		}
	}
	return nil
}

// validateEnvironment checks that variable names are usable as shell identifiers.
//
// The reserved-prefix rule is NOT enforced here, and the omission is deliberate rather than
// forgotten. The reference states that a kit declaring a DASH_*, SBX_* or DOCKER_* variable is
// a validation error, and that HOME, USER, SHELL, PATH, LD_PRELOAD and LD_LIBRARY_PATH earn a
// warning. Both belong to the runtime that reserves those names: DASH_ and SBX_ are Docker's
// own runtime internals, not this project's, so hardcoding them here would assert a
// relationship that does not exist. A consumer embedding this package in a runtime that
// reserves names should apply its own list on top.
func validateEnvironment(e *Environment) error {
	if e == nil {
		return nil
	}
	for _, key := range sortedKeys(e.Variables) {
		if key == "" {
			return fmt.Errorf("environment.variables has an empty key")
		}
		if !shellIdentifierPattern.MatchString(key) {
			return fmt.Errorf("environment.variables key %q is not a valid shell "+
				"identifier: start with a letter or underscore, then letters, "+
				"digits or underscores", key)
		}
	}
	return nil
}

// validateSetup checks the install, startup and files lists.
//
// Note what is not checked: the user: field on install and startup entries. The reference
// documents it as a uid and this package treats it as a free-form user reference, because the
// corpus contradicts the documentation — see InstallCommand.User for the counts.
func validateSetup(s *Setup) error {
	if s == nil {
		return nil
	}

	for i, c := range s.Install {
		if strings.TrimSpace(c.Command) == "" {
			return fmt.Errorf("setup.install[%d].command is required (a shell command "+
				"string, run with sh -c)", i)
		}
	}

	for i, c := range s.Startup {
		if len(c.Command) == 0 {
			return fmt.Errorf("setup.startup[%d].command is required (argv as a string "+
				"array; use [\"sh\", \"-c\", \"...\"] when a shell is needed)", i)
		}
		if c.Command[0] == "" {
			return fmt.Errorf("setup.startup[%d].command[0] must not be empty: it is "+
				"the program to run", i)
		}
	}

	for i, f := range s.Files {
		path := fmt.Sprintf("setup.files[%d]", i)
		if f.Path == "" {
			return fmt.Errorf("%s.path is required", path)
		}
		if !strings.HasPrefix(f.Path, "/") {
			return fmt.Errorf("%s.path %q must be absolute", path, f.Path)
		}
		if f.Mode != "" && !octalModePattern.MatchString(f.Mode) {
			return fmt.Errorf("%s.mode %q must be octal, e.g. \"0644\"", path, f.Mode)
		}
	}
	return nil
}

// validateCredentials checks each credential entry and its api-key and OAuth halves.
//
// The all-domains-declared rule — every apiKey.inject[].domain must also appear in
// permissions.network.allow — is NOT checked here. That is the reference's own division of
// labour, stated in spec-anatomy.md: "The spec validator does not cross-check the two lists;
// the engine enforces the rule." The reason it holds is composition: a mixin can legitimately
// declare an inject domain whose allow entry comes from the base agent it is layered onto, so
// the check is only sound once the whole composition is resolved. Applying it to a single
// spec.yaml would reject correct mixins. Since this package does not resolve composition, it
// cannot perform the check soundly and therefore does not perform it at all.
func validateCredentials(creds []Credential) error {
	for i, c := range creds {
		path := fmt.Sprintf("credentials[%d]", i)

		if c.Service == "" {
			return fmt.Errorf("%s.service is required: it is the identity a user's "+
				"credential binding is matched on", path)
		}
		// The lowercase-kebab charset the specification gives for service is not
		// enforced; see Credential.Service for why.

		// A credential that declares neither mechanism describes a need with no way to
		// meet it. The reference qualifies this as applying to services outside its
		// provider registry, which would let a bare `- service: anthropic` be completed
		// from the registry — but the registry does not exist in this release (provider
		// is accepted with no effect), so there is nothing to complete such an entry
		// from and requiring one mechanism is the only checkable rule. Should a registry
		// land, this is the check that has to relax.
		if c.APIKey == nil && c.OAuth == nil {
			return fmt.Errorf("%s (service %q): declare apiKey, oauth, or both — a "+
				"credential with neither has no mechanism to inject it",
				path, c.Service)
		}

		if err := validateAPIKey(path, c.APIKey); err != nil {
			return err
		}
		if err := validateOAuth(path, c.OAuth); err != nil {
			return err
		}
	}
	return nil
}

// validateAPIKey checks one credential's api-key half. Assumes ExpandSchemes has run.
func validateAPIKey(parent string, k *APIKey) error {
	if k == nil {
		return nil
	}
	path := parent + ".apiKey"

	if k.Name != "" && !shellIdentifierPattern.MatchString(k.Name) {
		return fmt.Errorf("%s.name %q is not a valid environment variable name: start "+
			"with a letter or underscore, then letters, digits or underscores",
			path, k.Name)
	}
	// proxyManaged means "set Name to the sentinel inside the container", so there has to be
	// a name to set. Without this the flag is silently inert.
	if k.ProxyManaged && k.Name == "" {
		return fmt.Errorf("%s.proxyManaged requires %s.name: the flag makes the engine set "+
			"that variable to the proxy sentinel in the container, and there is no "+
			"variable to set", path, path)
	}

	for j, inj := range k.Inject {
		ipath := fmt.Sprintf("%s.inject[%d]", path, j)
		if inj.Domain == "" {
			return fmt.Errorf("%s.domain is required: it is the domain the credential "+
				"is injected into", ipath)
		}
		// Scheme must be gone by now. Reaching here with one set means ExpandSchemes was
		// not run, which is a programming error in the caller rather than a bad spec, so
		// the message says so instead of blaming the author's YAML.
		if inj.Scheme != "" {
			return fmt.Errorf("%s.scheme %q was not expanded: call ExpandSchemes before "+
				"Validate", ipath, inj.Scheme)
		}
		// Username without basic auth is inert: nothing else consumes it.
		if inj.Username != "" && inj.Format != "%s" {
			return fmt.Errorf("%s.username is only used for HTTP Basic auth; set "+
				"scheme: %s alongside it", ipath, SchemeBasic)
		}
		if inj.Format == "" {
			return fmt.Errorf("%s: declare format (with one %%s placeholder) or scheme "+
				"(%q or %q) so the proxy knows how to encode the value",
				ipath, SchemeBasic, SchemeBearer)
		}
		// Exactly one %s. Counting placeholders rather than merely requiring one catches
		// "Bearer %s %s", which would render with a stray argument, and "100%" which
		// would render as a verb error. %% is an escaped literal percent and is not a
		// placeholder, so it is removed before counting.
		if n := strings.Count(strings.ReplaceAll(inj.Format, "%%", ""), "%s"); n != 1 {
			return fmt.Errorf("%s.format %q must contain exactly one %%s placeholder "+
				"(found %d)", ipath, inj.Format, n)
		}
	}
	return nil
}

// validateOAuth checks one credential's OAuth half.
func validateOAuth(parent string, o *OAuth) error {
	if o == nil {
		return nil
	}
	path := parent + ".oauth"

	if o.TokenEndpoint.Host == "" {
		return fmt.Errorf("%s.tokenEndpoint.host is required: it is the endpoint whose "+
			"token responses the proxy intercepts", path)
	}
	if o.TokenEndpoint.Path == "" {
		return fmt.Errorf("%s.tokenEndpoint.path is required", path)
	}

	// Sentinels are what the container sees in place of the real tokens, so they are
	// required unless passthrough has opted out of masking altogether and there is nothing
	// to substitute.
	if !o.Passthrough {
		if o.Sentinels.AccessToken == "" {
			return fmt.Errorf("%s.sentinels.accessToken is required unless "+
				"%s.passthrough is set", path, path)
		}
		if o.Sentinels.RefreshToken == "" {
			return fmt.Errorf("%s.sentinels.refreshToken is required unless "+
				"%s.passthrough is set", path, path)
		}
	}

	if f := o.CredentialFile; f != nil {
		if f.Path == "" {
			return fmt.Errorf("%s.credentialFile.path is required", path)
		}
		if f.Template == "" && len(f.Structure) == 0 {
			return fmt.Errorf("%s.credentialFile needs either template or structure to "+
				"know what to write", path)
		}
	}
	return nil
}

// validateVolumes checks the mount entries.
func validateVolumes(volumes []Volume) error {
	seen := make(map[string]int, len(volumes))
	for i, v := range volumes {
		path := fmt.Sprintf("volumes[%d]", i)

		if v.Type != MountTypeBlock && v.Type != MountTypeTmpfs {
			return fmt.Errorf("%s.type %q is not valid (omit it for a block-backed "+
				"volume, or use %q for a RAM-backed mount)",
				path, v.Type, MountTypeTmpfs)
		}
		if v.Path == "" {
			return fmt.Errorf("%s.path is required", path)
		}
		if !strings.HasPrefix(v.Path, "/") {
			return fmt.Errorf("%s.path %q must be an absolute container path", path, v.Path)
		}
		// Two mounts at one path is a spec contradicting itself: only one can win and the
		// grammar does not say which. Under composition the documented rule is
		// last-wins-by-path across kits, but that is a merge of two specs, not one spec
		// declaring the same path twice, which nothing resolves.
		if prev, dup := seen[v.Path]; dup {
			return fmt.Errorf("%s.path %q duplicates volumes[%d].path", path, v.Path, prev)
		}
		seen[v.Path] = i

		if v.Size != "" {
			if _, err := parseByteSize(v.Size); err != nil {
				return fmt.Errorf("%s.size %q is not a valid size: %w", path, v.Size, err)
			}
		}
		if v.Mode != "" && !octalModePattern.MatchString(v.Mode) {
			return fmt.Errorf("%s.mode %q must be octal, e.g. \"1777\"", path, v.Mode)
		}
	}
	return nil
}

// parseByteSize parses a byte-size string such as "512m", "4g" or "4096mib" into bytes.
//
// This exists rather than reusing a library because the reference implementation validates
// these fields with docker/go-units' RAMInBytes, which this repository does not depend on and
// which is not worth adding for one validation. The units are binary, matching RAMInBytes and
// matching this repository's own parseMemory in internal/cli/resources.go: "m" is mebibytes,
// not megabytes.
//
// It differs from internal/cli/resources.go's parseMemory in two ways that are deliberate and
// not an oversight. That function rejects a bare number, because a bare -memory argument that
// means bytes would silently produce a VM too small to boot; here a bare number is accepted as
// bytes, because RAMInBytes accepts it and a spec.yaml that says `size: 1048576` should not be
// rejected by a validator that is only checking well-formedness. And it applies no minimum,
// because a 4 MiB tmpfs is a perfectly reasonable thing for a kit to ask for while a 4 MiB VM
// is not.
func parseByteSize(text string) (int64, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, fmt.Errorf("the size is empty")
	}

	digits := trimmed
	multiplier := int64(1)
	if unit := strings.ToLower(strings.TrimLeft(trimmed, "0123456789")); unit != "" {
		digits = trimmed[:len(trimmed)-len(unit)]
		switch unit {
		case "b":
			multiplier = 1
		case "k", "kb", "kib":
			multiplier = 1 << 10
		case "m", "mb", "mib":
			multiplier = 1 << 20
		case "g", "gb", "gib":
			multiplier = 1 << 30
		case "t", "tb", "tib":
			multiplier = 1 << 40
		default:
			return 0, fmt.Errorf("unknown unit %q; use k, m, g or t (binary)", unit)
		}
	}

	value, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("want a non-negative number with an optional k/m/g/t suffix")
	}
	// Overflow would silently wrap to a negative or small size, which is worse than a
	// rejection: a spec asking for "9999999t" is a mistake either way.
	if multiplier != 1 && value > (1<<62)/multiplier {
		return 0, fmt.Errorf("the size is too large to represent in bytes")
	}
	return value * multiplier, nil
}
