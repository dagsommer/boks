package kit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Limits on a composition. Both are about a kit that was fetched: the chain and the list are
// followed automatically, so each is bounded rather than trusted to be short.
const (
	// MaxExtendsDepth is how many parents an `extends` chain may climb (docs/kits-design.md
	// §1.2: "max 5 levels").
	MaxExtendsDepth = 5
	// MaxMembers caps the kits in one composition, extends and mixins included.
	MaxMembers = 32
)

// Loader reads one kit by reference. Load is the real one; a test supplies its own.
type Loader func(reference string) (*Spec, []string, error)

// Member is one kit in a composition after its `extends` chain has been flattened into it.
type Member struct {
	// Ref is the reference the kit was loaded from, as typed or as declared in a spec.
	Ref string
	// Spec has Extends and Mixins cleared: they have been followed. For a kit that extends,
	// Spec is the merge of its whole chain, with the leaf's name.
	Spec *Spec
	// Lineage names the extends ancestors, nearest parent first.
	Lineage []string
	// Via says why a kit is here when it was not on the command line: "mixin of NAME".
	Via string
}

// Composition is the result of stacking kits: the members in application order and the single
// spec they merge into.
type Composition struct {
	Members []Member
	// Merged is what the rest of Boks consumes. Its Name is the member names joined by a comma,
	// which is a label for messages and the policy table and not a kit name.
	Merged   *Spec
	Warnings []string
}

// Resolve loads each reference in order, follows `extends` and `mixins`, and composes the
// result. The members are applied in the order the design gives (docs/kits-design.md §3.1):
// for each reference, its extends chain flattened into one kit, then the mixins its chain
// declares (an ancestor's before a descendant's), then the next reference.
func Resolve(refs []string, load Loader) (*Composition, error) {
	if load == nil {
		load = Load
	}
	r := &resolver{load: load}
	for _, ref := range refs {
		if err := r.root(ref); err != nil {
			return nil, err
		}
	}
	merged, err := Compose(r.members)
	if err != nil {
		return nil, err
	}
	return &Composition{Members: r.members, Merged: merged, Warnings: r.warnings}, nil
}

// loaded is one kit read from a reference, before its chain is flattened.
type loaded struct {
	ref  string
	spec *Spec
	// dir is the directory relative references in this spec resolve against, "" for a kit that
	// was fetched.
	dir string
	// key identifies the kit for cycle detection.
	key string
}

type resolver struct {
	load     Loader
	members  []Member
	warnings []string
}

// fetch loads and validates one kit. from is the kit that referred to it, nil for the command
// line.
func (r *resolver) fetch(ref string, from *loaded) (*loaded, error) {
	path := ref
	remote := strings.HasPrefix(ref, "git+")
	if from != nil && !remote {
		// Only a kit on disk may point at the disk. A fetched kit's clone is gone by the time
		// anything could be resolved against it, and letting one name an arbitrary local path
		// would be a remote file deciding what Boks reads from this machine.
		if from.dir == "" {
			return nil, fmt.Errorf("kit %s refers to %q, a local path: a kit that was fetched may "+
				"only refer to other git+ references", from.spec.Name, ref)
		}
		if _, isRemote := remoteForm(ref); !isRemote && !filepath.IsAbs(ref) {
			path = filepath.Join(from.dir, ref)
		}
	}
	spec, warnings, err := r.load(path)
	if err != nil {
		return nil, err
	}
	if err := Validate(spec); err != nil {
		return nil, fmt.Errorf("kit %s: %w", ref, err)
	}
	for _, w := range warnings {
		r.warnings = append(r.warnings, fmt.Sprintf("kit %s: %s", ref, w))
	}
	l := &loaded{ref: ref, spec: spec, key: path}
	if !remote {
		l.dir, l.key = localKit(path)
	}
	return l, nil
}

// localKit returns the directory a local reference's spec lives in and a canonical key for it,
// so `./a` and `./a/spec.yaml` are one kit to the cycle check.
func localKit(path string) (dir, key string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		return abs, filepath.Join(abs, SpecFileName)
	}
	return filepath.Dir(abs), abs
}

// root resolves one --kit reference into members.
func (r *resolver) root(ref string) error {
	leaf, err := r.fetch(ref, nil)
	if err != nil {
		return err
	}

	// Climb extends, leaf first. The key set is the cycle check; the length is the depth cap.
	chain := []*loaded{leaf}
	seen := map[string]bool{leaf.key: true}
	for cur := leaf; cur.spec.Extends != ""; {
		if cur.spec.Kind != KindSandbox {
			return fmt.Errorf("kit %s: only a kind %q kit may extend another", cur.spec.Name, KindSandbox)
		}
		if len(chain) > MaxExtendsDepth {
			return fmt.Errorf("kit %s: extends chain is deeper than %d levels (at %s)",
				leaf.spec.Name, MaxExtendsDepth, cur.spec.Name)
		}
		parent, err := r.fetch(cur.spec.Extends, cur)
		if err != nil {
			return fmt.Errorf("kit %s extends %s: %w", cur.spec.Name, cur.spec.Extends, err)
		}
		if seen[parent.key] {
			return fmt.Errorf("kit %s: extends cycle: %s is already in the chain",
				leaf.spec.Name, cur.spec.Extends)
		}
		if parent.spec.Kind != KindSandbox {
			return fmt.Errorf("kit %s extends %s, which is kind %q: a parent must be kind %q",
				cur.spec.Name, parent.spec.Name, parent.spec.Kind, KindSandbox)
		}
		seen[parent.key] = true
		chain = append(chain, parent)
		cur = parent
	}

	flat := leaf.spec
	if len(chain) > 1 {
		if flat, err = flatten(chain); err != nil {
			return err
		}
	}
	lineage := make([]string, 0, len(chain)-1)
	for _, p := range chain[1:] {
		lineage = append(lineage, p.spec.Name)
	}
	r.members = append(r.members, Member{Ref: ref, Spec: flat, Lineage: lineage})

	// Mixins, ancestors' first. A mixin may not itself extend or carry mixins (validate.go), so
	// there is nothing further to follow and no cycle to find.
	for i := len(chain) - 1; i >= 0; i-- {
		for _, mref := range chain[i].spec.Mixins {
			m, err := r.fetch(mref, chain[i])
			if err != nil {
				return fmt.Errorf("kit %s mixes in %s: %w", chain[i].spec.Name, mref, err)
			}
			if m.spec.Kind != KindMixin {
				return fmt.Errorf("kit %s lists %s in mixins, and it is kind %q: a mixin must be kind %q",
					chain[i].spec.Name, m.spec.Name, m.spec.Kind, KindMixin)
			}
			r.members = append(r.members, Member{Ref: mref, Spec: m.spec, Via: "mixin of " + leaf.spec.Name})
		}
	}
	if len(r.members) > MaxMembers {
		return fmt.Errorf("more than %d kits in one composition", MaxMembers)
	}
	return nil
}

// flatten merges an extends chain, given leaf first, into one spec, parent first.
//
// A child may not set anything its ancestors lock. The design says nothing enforces `locked`
// upstream and no source says what a violation does, so this takes the conservative reading:
// a path an ancestor locked is one a descendant may not set at all, and doing so is an error
// rather than a silent override.
func flatten(chain []*loaded) (*Spec, error) {
	out := &Spec{}
	var locked []string
	owners := map[string]string{}
	for i := len(chain) - 1; i >= 0; i-- {
		l := chain[i]
		if len(locked) > 0 {
			if path, err := overrides(l.spec, locked); err != nil {
				return nil, err
			} else if path != "" {
				return nil, fmt.Errorf("kit %s sets %s, which an ancestor locks", l.spec.Name, path)
			}
		}
		if err := overlay(out, l.spec, false, owners); err != nil {
			return nil, err
		}
		locked = appendUnique(locked, l.spec.Locked...)
	}
	leaf := chain[0].spec
	out.SchemaVersion, out.Kind, out.Name = leaf.SchemaVersion, KindSandbox, leaf.Name
	if err := Validate(out); err != nil {
		return nil, fmt.Errorf("kit %s, with its extends chain merged: %w", leaf.Name, err)
	}
	return out, nil
}

// overrides returns the first of the locked dotted paths that s sets, or "".
func overrides(s *Spec, locked []string) (string, error) {
	data, err := yaml.Marshal(s)
	if err != nil {
		return "", err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", err
	}
	for _, path := range locked {
		var cur any = doc
		for _, part := range strings.Split(path, ".") {
			m, ok := cur.(map[string]any)
			if !ok {
				cur = nil
				break
			}
			if cur, ok = m[part]; !ok {
				break
			}
		}
		if cur != nil {
			return path, nil
		}
	}
	return "", nil
}

// Compose stacks kits, in order, into one spec: the merge rules of docs/kits-design.md §3.2 for
// `--kit`. Names must be unique, at most one kit may be kind sandbox, and the same credential
// service in two kits is an error even when the two are identical.
func Compose(members []Member) (*Spec, error) {
	merged := &Spec{Kind: KindMixin}
	names := map[string]bool{}
	owners := map[string]string{}
	var labels []string
	sandbox := ""
	for _, m := range members {
		s := m.Spec
		if names[s.Name] {
			return nil, fmt.Errorf("duplicate kit name %s: each kit in a composition must have a unique name", s.Name)
		}
		names[s.Name] = true
		labels = append(labels, s.Name)
		if s.Kind == KindSandbox {
			if sandbox != "" {
				return nil, fmt.Errorf("kit %s must be kind %q, got %q: %s already defines the agent, "+
					"and a composition has at most one", s.Name, KindMixin, KindSandbox, sandbox)
			}
			sandbox = s.Name
			merged.Kind, merged.SchemaVersion = KindSandbox, s.SchemaVersion
		}
		if err := overlay(merged, s, true, owners); err != nil {
			return nil, err
		}
	}
	if merged.SchemaVersion == "" && len(members) > 0 {
		merged.SchemaVersion = members[0].Spec.SchemaVersion
	}
	merged.Name = strings.Join(labels, ",")
	return merged, nil
}

// CheckAgent reports a mixin whose requires.agent is not the agent the composition runs on.
//
// The agent is matched by name. A kit-defined agent also answers to the names in its extends
// lineage, because upstream leaves "family" matching to the consumer's extends-lineage check
// and that is the one reading that does not widen the match to unrelated agents. A built-in
// agent answers to its own name only: `claude-vertex` is not `claude`.
func (c *Composition) CheckAgent(agent string) error {
	names := map[string]bool{agent: true}
	for _, m := range c.Members {
		if m.Spec.Kind == KindSandbox && m.Spec.Name == agent {
			for _, n := range m.Lineage {
				names[n] = true
			}
		}
	}
	for _, m := range c.Members {
		if r := m.Spec.Requires; r != nil && r.Agent != "" && !names[r.Agent] {
			return fmt.Errorf("kit %s requires agent %q, and this sandbox runs %q", m.Spec.Name, r.Agent, agent)
		}
	}
	return nil
}

// overlay merges src onto dst in place. stack selects the `--kit` rules, otherwise the
// `extends` ones; they differ in three places: a repeated volume path is an error under extends
// and last-wins when stacked, scalars and the sandbox block only travel through extends, and
// stacked, only a kind sandbox contributes a sandbox block and agent instructions.
//
// owners records which kit declared each credential service and volume path, to name both in
// the error.
func overlay(dst, src *Spec, stack bool, owners map[string]string) error {
	if !stack {
		dst.Version = pick(dst.Version, src.Version)
		dst.DisplayName = pick(dst.DisplayName, src.DisplayName)
		dst.Description = pick(dst.Description, src.Description)
		dst.SourceURL = pick(dst.SourceURL, src.SourceURL)
		for k, v := range src.Args {
			if dst.Args == nil {
				dst.Args = map[string]Arg{}
			}
			dst.Args[k] = v
		}
	}
	dst.Licenses = appendUnique(dst.Licenses, src.Licenses...)
	if !stack {
		dst.Locked = appendUnique(dst.Locked, src.Locked...)
	}
	if src.Security != nil && src.Security.Privileged {
		dst.Security = &Security{Privileged: true}
	}
	if src.Sandbox != nil && (!stack || src.Kind == KindSandbox) {
		sb := *src.Sandbox
		dst.Sandbox = &sb
	}
	if ai := src.AgentInstructions; ai != nil && (!stack || src.Kind == KindSandbox) {
		c := *ai
		dst.AgentInstructions = &c
	}
	if n := src.Permissions; n != nil && n.Network != nil {
		if dst.Permissions == nil {
			dst.Permissions = &Permissions{}
		}
		if dst.Permissions.Network == nil {
			dst.Permissions.Network = &NetworkPermissions{}
		}
		nw := dst.Permissions.Network
		nw.Allow = appendUnique(nw.Allow, n.Network.Allow...)
		nw.Deny = appendUnique(nw.Deny, n.Network.Deny...)
	}
	dst.Ports = append(dst.Ports, src.Ports...)

	for _, c := range src.Credentials {
		if prev, ok := owners["credential\x00"+c.Service]; ok {
			if stack {
				return fmt.Errorf("credential %s defined in both %s and %s", c.Service, prev, src.Name)
			}
			return fmt.Errorf("credential %s is declared by both %s and %s, and a merge cannot decide "+
				"which shape wins", c.Service, prev, src.Name)
		}
		owners["credential\x00"+c.Service] = src.Name
		dst.Credentials = append(dst.Credentials, c)
	}
	for _, v := range src.Volumes {
		prev, ok := owners["volume\x00"+v.Path]
		if ok && !stack {
			return fmt.Errorf("volume %s is declared by both %s and %s", v.Path, prev, src.Name)
		}
		owners["volume\x00"+v.Path] = src.Name
		replaced := false
		for i := range dst.Volumes {
			if dst.Volumes[i].Path == v.Path {
				dst.Volumes[i], replaced = v, true
			}
		}
		if !replaced {
			dst.Volumes = append(dst.Volumes, v)
		}
	}

	if src.Environment != nil && len(src.Environment.Variables) > 0 {
		if dst.Environment == nil {
			dst.Environment = &Environment{Variables: map[string]string{}}
		}
		for k, v := range src.Environment.Variables {
			dst.Environment.Variables[k] = v
		}
	}
	if s := src.Setup; s != nil {
		if dst.Setup == nil {
			dst.Setup = &Setup{}
		}
		dst.Setup.Install = append(dst.Setup.Install, s.Install...)
		dst.Setup.Files = append(dst.Setup.Files, s.Files...)
		dst.Setup.Startup = append(dst.Setup.Startup, s.Startup...)
	}
	if t := src.Tree; !t.Empty() {
		if dst.Tree == nil {
			dst.Tree = &Tree{}
		}
		dst.Tree.Home = overlayFiles(dst.Tree.Home, t.Home)
		dst.Tree.Workspace = overlayFiles(dst.Tree.Workspace, t.Workspace)
	}
	return nil
}

// overlayFiles adds files, a later file replacing an earlier one at the same path. It matters
// for files/workspace/, which is never overwritten on disk: left as two entries, the earlier
// kit would win there, the opposite of the overlay rule.
func overlayFiles(dst, add []TreeFile) []TreeFile {
	for _, f := range add {
		replaced := false
		for i := range dst {
			if dst[i].Path == f.Path {
				dst[i], replaced = f, true
			}
		}
		if !replaced {
			dst = append(dst, f)
		}
	}
	return dst
}

func pick(old, new string) string {
	if new != "" {
		return new
	}
	return old
}

// appendUnique appends the values not already present, keeping first-seen order.
func appendUnique(dst []string, add ...string) []string {
	for _, v := range add {
		found := false
		for _, d := range dst {
			if d == v {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, v)
		}
	}
	return dst
}
