package policy

// The durable policy store: rules that outlive the invocation that wrote them.
//
// # Why this exists at all
//
// Boks began with policy in the command line — `-policy`, `-allow`, `-deny` — which made a
// sandbox's containment a property of the *invocation* rather than of the sandbox. That is
// not merely a parity gap with Docker Sandboxes, whose `sbx policy allow/deny/rm` write
// durable rules; it was a bug. `boks start` and `boks exec` had no flags to carry, so a
// sandbox created under `-policy locked` came back up under the default preset. A
// containment that silently widens when you restart a sandbox is worse than one that is
// merely narrow.
//
// # Format and layout
//
//	<state>/policy/policy.json
//
// One file, JSON, with an explicit integer `version` at the top. Each of those choices is a
// decision rather than a default:
//
//   - **One file, not a directory of them.** The whole store is read on every decision path
//     and rewritten as a unit; splitting it into per-scope files would buy nothing but a
//     window in which half a policy is on disk. A rule set is small — a few dozen lines —
//     and the interesting operations (resolve, reset, inspect) all want the whole thing.
//   - **JSON, not a bespoke line format or YAML.** No dependency may be added, which rules
//     out YAML and TOML. Between JSON and an invented text grammar, JSON wins because every
//     other durable artefact in Boks is already JSON (container labels, the decision log,
//     the supervisor's spec), so there is one thing to learn and one parser to trust. The
//     cost is real and worth stating: JSON has no comments, so a rule's justification lives
//     in a `note` field instead. Rules are written through `boks policy allow/deny` far more
//     often than by hand, and hand edits are validated on the next read.
//   - **Versioned, and strictly.** An unknown version is refused rather than ignored. A
//     newer Boks may add a field that *narrows* access — a scope, a condition, an expiry —
//     and an older Boks that skipped what it did not understand would enforce a policy
//     weaker than the one written down. Refusing to run is the correct failure.
//
// # Fail closed
//
// A store that cannot be read is an error, everywhere, and every caller propagates it: no
// sandbox starts, no policy resolves. A *missing* store is not an error — it is the
// uninitialised state and resolves to the built-in defaults, which are deny-by-default. A
// store that parses but contains a rule that does not is also an error, because dropping the
// unparseable rule might drop a deny.
//
// # What is never in here
//
// No secret, ever. The store holds destinations and dispositions; credential *rules* name a
// service and live in the sandbox's own configuration, and credential *values* live in the
// encrypted store in internal/secret and are handed to the supervisor on a pipe. Nothing in
// this file has a field a value could be written into.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// StoreVersion is the schema version this build writes and is the highest it will read.
const StoreVersion = 1

// storeFileName is the file under <state>/policy that holds everything.
const storeFileName = "policy.json"

// MarshalJSON writes an action as "allow" or "deny" rather than as the integer it is, so a
// hand-edited store reads like the command that would have produced it.
func (a Action) MarshalJSON() ([]byte, error) { return json.Marshal(a.String()) }

// UnmarshalJSON reads "allow" or "deny". An unknown word is an error rather than a silent
// deny: a typo in a rule the user believes is an allow should be reported, and a typo in one
// they believe is a deny must never be read as an allow.
func (a *Action) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("policy action: %w", err)
	}
	parsed, err := ParseAction(s)
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}

// RuleSpec is a rule in the form it is stored and displayed in: the two strings a user
// typed, plus the note they attached and the scope it came from.
//
// It is deliberately not a Rule. A Rule holds a compiled Pattern and PortSet, which are the
// right shape for matching and the wrong shape for a file: they cannot round-trip through
// JSON without exporting their internals, and a stored rule must survive being read by a
// build whose matcher has changed.
type RuleSpec struct {
	Action Action `json:"action"`
	// Spec is the destination in the syntax ParseRule accepts: "host", "host:ports",
	// "10.0.0.0/8", "[::1]:8080", "*:22".
	Spec string `json:"spec"`
	// Note is why the rule exists. JSON has no comments; this is the replacement.
	Note string `json:"note,omitempty"`
	// Scope is filled in during resolution and is not stored: a rule inside the global
	// scope knows it is global by where it sits.
	Scope string `json:"-"`
}

// Rule compiles the stored strings into a matchable rule.
func (r RuleSpec) Rule() (Rule, error) {
	rule, err := ParseRule(r.Action, r.Spec)
	if err != nil {
		return Rule{}, err
	}
	rule.Why = r.Note
	rule.Scope = r.Scope
	return rule, nil
}

func (r RuleSpec) String() string {
	if r.Note == "" {
		return r.Action.String() + " " + r.Spec
	}
	return r.Action.String() + " " + r.Spec + "  # " + r.Note
}

// canonical returns the rule's destination as the engine will see it, so that
// "GitHub.com:443" and "github.com:443" are recognised as the same rule when adding and
// removing. Falls back to the raw text for a spec that does not parse, which only a
// hand-edited store can produce and which loading rejects anyway.
func (r RuleSpec) canonical() string {
	rule, err := ParseRule(r.Action, r.Spec)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(r.Spec))
	}
	return rule.Spec()
}

// SameDestination reports whether the rule names the destination in spec, comparing them as
// the engine would rather than as text: "GitHub.com" and "github.com:*" are one destination.
func (r RuleSpec) SameDestination(spec string) bool {
	other := RuleSpec{Action: r.Action, Spec: spec}
	return r.canonical() == other.canonical()
}

// Profile is a named policy a run can select: a base preset plus rules.
//
// A profile is not a new concept — it is a policy with a name — and it is kept that way
// deliberately. It exists so that "the posture we use for CI" or "the posture for untrusted
// pull requests" can be written once and selected with `boks run -profile ci`, rather than
// retyped as a wall of flags that nobody will get right twice.
type Profile struct {
	Description string     `json:"description,omitempty"`
	Preset      string     `json:"preset,omitempty"`
	Rules       []RuleSpec `json:"rules,omitempty"`
}

// Store is the whole durable policy: the global posture, the global rules, the per-sandbox
// rules, and the profiles.
type Store struct {
	Version int `json:"version"`
	// Preset is the base posture every run starts from unless a profile or a -policy flag
	// replaces it. Empty means DefaultPreset.
	Preset string `json:"preset,omitempty"`
	// Global holds rules that apply to every sandbox.
	Global []RuleSpec `json:"global,omitempty"`
	// Sandboxes holds rules scoped to one sandbox, keyed by sandbox name.
	Sandboxes map[string][]RuleSpec `json:"sandboxes,omitempty"`
	// Profiles are named policies a run can select.
	Profiles map[string]Profile `json:"profiles,omitempty"`

	// path is where the store was loaded from and where Save writes. Not serialised.
	path string
	// exists records whether the file was there, so that `ls` can distinguish an empty
	// store from an uninitialised one.
	exists bool
}

// DefaultStorePath is where the durable policy lives.
func DefaultStorePath() string { return filepath.Join(StateDir(), "policy", storeFileName) }

// NewStore returns an empty store bound to a path, as `boks policy init` would write it.
func NewStore(path string) *Store {
	return &Store{Version: StoreVersion, Preset: DefaultPreset, path: path}
}

// Path is where the store lives.
func (s *Store) Path() string { return s.path }

// Exists reports whether a store file was found. A store that does not exist still resolves
// — to the built-in defaults — so this is a display concern rather than a control one.
func (s *Store) Exists() bool { return s.exists }

// LoadStore reads the durable policy.
//
// A missing file yields an empty store rather than an error: that is the state of a machine
// where nobody has written a rule yet, and it resolves to the built-in deny-by-default
// preset. Everything else — unreadable, malformed, an unknown version, a rule that does not
// parse — is an error, and callers must not proceed past it. See the package comment.
func LoadStore(path string) (*Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s := NewStore(path)
			s.Preset = "" // "" means DefaultPreset; nothing has been chosen yet
			return s, nil
		}
		return nil, fmt.Errorf("reading the policy store %s: %w\n"+
			"Boks will not run a sandbox with a policy it cannot read.", path, err)
	}

	var s Store
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("the policy store %s is not valid: %w\n"+
			"Fix it by hand, or start again with 'boks policy init -force'.", path, err)
	}
	s.path = path
	s.exists = true
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// validate rejects a store this build cannot enforce faithfully.
func (s *Store) validate() error {
	if s.Version <= 0 {
		return fmt.Errorf("the policy store %s has no version; it cannot be read safely", s.path)
	}
	if s.Version > StoreVersion {
		return fmt.Errorf("the policy store %s is version %d, and this boks understands version %d.\n"+
			"Refusing to enforce a policy written by a newer version: it may contain restrictions\n"+
			"this build would silently ignore. Upgrade boks, or move the file aside.",
			s.path, s.Version, StoreVersion)
	}
	if s.Preset != "" {
		if _, err := Preset(s.Preset); err != nil {
			return fmt.Errorf("the policy store %s names an unknown preset: %w", s.path, err)
		}
	}
	if err := validateRules(s.path, "global", s.Global); err != nil {
		return err
	}
	for _, name := range sortedKeys(s.Sandboxes) {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("the policy store %s has a sandbox scope with no name", s.path)
		}
		if err := validateRules(s.path, "sandbox "+name, s.Sandboxes[name]); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(s.Profiles) {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("the policy store %s has a profile with no name", s.path)
		}
		p := s.Profiles[name]
		if p.Preset != "" {
			if _, err := Preset(p.Preset); err != nil {
				return fmt.Errorf("profile %q in %s: %w", name, s.path, err)
			}
		}
		if err := validateRules(s.path, "profile "+name, p.Rules); err != nil {
			return err
		}
	}
	return nil
}

// validateRules refuses a scope containing a rule that does not compile.
//
// Dropping the bad rule and carrying on would be the friendlier behaviour and the wrong
// one: the rule that failed to parse may be a deny, and a policy silently missing a deny is
// exactly the failure this package exists to prevent.
func validateRules(path, scope string, rules []RuleSpec) error {
	for i, r := range rules {
		if _, err := r.Rule(); err != nil {
			return fmt.Errorf("the policy store %s has an unusable rule in %s (entry %d): %w\n"+
				"Boks refuses the whole store rather than enforce it with a rule missing.",
				path, scope, i+1, err)
		}
	}
	return nil
}

// ScopeKind is which of the three places a rule can live in.
type ScopeKind int

const (
	// ScopeGlobal applies to every sandbox on this machine.
	ScopeGlobal ScopeKind = iota
	// ScopeSandbox applies to one named sandbox.
	ScopeSandbox
	// ScopeProfile applies to any run that selects the profile.
	ScopeProfile
)

// ScopeRef names a scope: a kind, and for the two named kinds, a name.
type ScopeRef struct {
	Kind ScopeKind
	Name string
}

// GlobalScope, SandboxScope and ProfileScope build the three references.
func GlobalScope() ScopeRef             { return ScopeRef{Kind: ScopeGlobal} }
func SandboxScope(name string) ScopeRef { return ScopeRef{Kind: ScopeSandbox, Name: name} }
func ProfileScope(name string) ScopeRef { return ScopeRef{Kind: ScopeProfile, Name: name} }

func (s ScopeRef) String() string {
	switch s.Kind {
	case ScopeSandbox:
		return "sandbox " + s.Name
	case ScopeProfile:
		return "profile " + s.Name
	}
	return "global"
}

// ParseScope builds a scope reference from the -sandbox and -profile flags, refusing the
// combination that would be ambiguous.
func ParseScope(sandbox, profile string) (ScopeRef, error) {
	sandbox, profile = strings.TrimSpace(sandbox), strings.TrimSpace(profile)
	switch {
	case sandbox != "" && profile != "":
		return ScopeRef{}, errors.New("-sandbox and -profile name two different scopes; give one")
	case sandbox != "":
		return SandboxScope(sandbox), nil
	case profile != "":
		return ProfileScope(profile), nil
	}
	return GlobalScope(), nil
}

// Rules returns the rules in a scope, and whether the scope exists at all.
func (s *Store) Rules(scope ScopeRef) ([]RuleSpec, bool) {
	switch scope.Kind {
	case ScopeSandbox:
		rules, ok := s.Sandboxes[scope.Name]
		return rules, ok
	case ScopeProfile:
		p, ok := s.Profiles[scope.Name]
		return p.Rules, ok
	}
	return s.Global, true
}

// Add stores a rule in a scope, returning false if an identical rule was already there.
//
// Identity is the compiled destination plus the action, so adding "GitHub.com:443" twice
// stores one rule. A note on a repeated add replaces the old one, because the alternative is
// a rule you cannot re-explain without deleting it first.
func (s *Store) Add(scope ScopeRef, spec RuleSpec) (bool, error) {
	if _, err := spec.Rule(); err != nil {
		return false, err
	}
	if scope.Kind == ScopeProfile {
		if _, ok := s.Profiles[scope.Name]; !ok {
			return false, fmt.Errorf("no profile named %q; create it with 'boks policy profile create %s'", scope.Name, scope.Name)
		}
	}
	rules, _ := s.Rules(scope)
	for i := range rules {
		if rules[i].Action == spec.Action && rules[i].canonical() == spec.canonical() {
			if spec.Note != "" && rules[i].Note != spec.Note {
				rules[i].Note = spec.Note
				s.setRules(scope, rules)
				return true, nil
			}
			return false, nil
		}
	}
	s.setRules(scope, append(rules, spec))
	return true, nil
}

// Remove deletes rules from a scope by destination. A nil action removes both dispositions,
// which is what a user who typed one destination usually means; passing an action is how the
// ambiguous case is resolved.
func (s *Store) Remove(scope ScopeRef, action *Action, spec string) ([]RuleSpec, error) {
	probe := RuleSpec{Action: Deny, Spec: spec}
	if action != nil {
		probe.Action = *action
	}
	if _, err := probe.Rule(); err != nil {
		return nil, err
	}
	want := probe.canonical()

	rules, ok := s.Rules(scope)
	if !ok {
		return nil, fmt.Errorf("no rules are stored for %s", scope)
	}
	kept := make([]RuleSpec, 0, len(rules))
	var removed []RuleSpec
	for _, r := range rules {
		if r.canonical() == want && (action == nil || r.Action == *action) {
			removed = append(removed, r)
			continue
		}
		kept = append(kept, r)
	}
	if len(removed) == 0 {
		return nil, fmt.Errorf("no rule for %q is stored in %s", spec, scope)
	}
	s.setRules(scope, kept)
	return removed, nil
}

// setRules writes a scope's rules back, dropping an empty per-sandbox scope entirely so that
// removing the last rule leaves no trace of the sandbox in the file.
func (s *Store) setRules(scope ScopeRef, rules []RuleSpec) {
	switch scope.Kind {
	case ScopeSandbox:
		if len(rules) == 0 {
			delete(s.Sandboxes, scope.Name)
			return
		}
		if s.Sandboxes == nil {
			s.Sandboxes = map[string][]RuleSpec{}
		}
		s.Sandboxes[scope.Name] = rules
	case ScopeProfile:
		p := s.Profiles[scope.Name]
		p.Rules = rules
		if s.Profiles == nil {
			s.Profiles = map[string]Profile{}
		}
		s.Profiles[scope.Name] = p
	default:
		s.Global = rules
	}
}

// AddProfile creates a named profile, refusing to overwrite one that exists.
func (s *Store) AddProfile(name string, p Profile) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("a profile needs a name")
	}
	if _, ok := s.Profiles[name]; ok {
		return fmt.Errorf("a profile named %q already exists; remove it first with 'boks policy profile rm %s'", name, name)
	}
	if p.Preset != "" {
		if _, err := Preset(p.Preset); err != nil {
			return err
		}
	}
	for _, r := range p.Rules {
		if _, err := r.Rule(); err != nil {
			return err
		}
	}
	if s.Profiles == nil {
		s.Profiles = map[string]Profile{}
	}
	s.Profiles[name] = p
	return nil
}

// RemoveProfile deletes a profile.
func (s *Store) RemoveProfile(name string) error {
	if _, ok := s.Profiles[name]; !ok {
		return fmt.Errorf("no profile named %q", name)
	}
	delete(s.Profiles, name)
	return nil
}

// Reset empties a scope, or the whole store when scope is the global one and all is set.
//
// It returns how many rules were destroyed, so the caller can say so before doing it: this
// is the one operation in the package that loses information.
func (s *Store) Reset(scope ScopeRef, all bool) int {
	if all {
		n := len(s.Global)
		for _, rules := range s.Sandboxes {
			n += len(rules)
		}
		for _, p := range s.Profiles {
			n += len(p.Rules)
		}
		s.Preset = DefaultPreset
		s.Global = nil
		s.Sandboxes = nil
		s.Profiles = nil
		return n
	}
	rules, _ := s.Rules(scope)
	n := len(rules)
	s.setRules(scope, nil)
	return n
}

// SandboxNames lists the sandboxes with rules of their own, sorted.
func (s *Store) SandboxNames() []string { return sortedKeys(s.Sandboxes) }

// ProfileNames lists the stored profiles, sorted.
func (s *Store) ProfileNames() []string { return sortedKeys(s.Profiles) }

// Count reports how many rules are stored in total.
func (s *Store) Count() int {
	n := len(s.Global)
	for _, rules := range s.Sandboxes {
		n += len(rules)
	}
	for _, p := range s.Profiles {
		n += len(p.Rules)
	}
	return n
}

// Save writes the store atomically: a temporary file in the same directory, then a rename.
//
// The rename matters more than it looks. The store is read on the path that decides whether
// a sandbox may reach the network, and a half-written file there is a policy that fails to
// load — which fails closed, but fails a user's run for no reason. A rename is atomic on
// every filesystem Boks targets, so a reader sees the old policy or the new one.
//
// Owner-only, in an owner-only directory: the file says which destinations this machine
// permits, which is not something to leave world-readable, and a policy other users can
// rewrite is not a policy.
func (s *Store) Save() error {
	if s.path == "" {
		return errors.New("policy store has no path to save to")
	}
	s.Version = StoreVersion
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the policy store: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".policy-*.json")
	if err != nil {
		return fmt.Errorf("writing the policy store: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename has succeeded
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("writing the policy store: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing the policy store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing the policy store: %w", err)
	}
	if err := os.Rename(name, s.path); err != nil {
		return fmt.Errorf("replacing the policy store %s: %w", s.path, err)
	}
	s.exists = true
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
