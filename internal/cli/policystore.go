package cli

// The `boks policy` subcommands that read and write the durable store.
//
// They are kept in a file of their own, and each one is a plain `func(ctx, Env) error` with
// its own flag set, so that the command table in policy.go is the only thing that has to
// change when the CLI moves to a different dispatcher. The decisions — precedence, scoping,
// what a rule means — live in internal/policy; nothing here decides anything about a policy
// beyond how to print it.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

// openPolicyStore loads the durable policy, failing rather than falling back.
//
// A store that cannot be read is not an invitation to use the defaults: the defaults may be
// wider than what is written down. Every caller of this treats the error as fatal.
func openPolicyStore() (*policy.Store, error) {
	return policy.LoadStore(policy.DefaultStorePath())
}

// scopeFlags registers the two flags that pick which scope a rule is written to. They are
// spelled as sbx spells them, and Go's flag package accepts both `-sandbox` and `--sandbox`.
func scopeFlags(fs *flag.FlagSet) (sandbox, profile *string) {
	sandbox = fs.String("sandbox", "", "scope the rule to one sandbox instead of all of them")
	profile = fs.String("profile", "", "scope the rule to a stored policy profile")
	return sandbox, profile
}

// parsePolicyArgs is the shared preamble: parse flags wherever they appear, translate -h,
// and hand back the positional arguments.
//
// Interspersed parsing matters more here than it looks. `boks policy allow github.com:443
// -note "git over HTTPS"` is how a person types it, and the standard flag package stops at
// the first positional — which would silently store "-note" and "git over HTTPS" as two
// destinations rather than reporting a mistake.
func parsePolicyArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil, flagErrHelp
		}
		return nil, err
	}
	return positional, nil
}

func policyAllow(ctx context.Context, env Env) error { return policyAdd(ctx, env, policy.Allow) }
func policyDeny(ctx context.Context, env Env) error  { return policyAdd(ctx, env, policy.Deny) }

// policyAdd writes an allow or a deny rule into a scope.
func policyAdd(_ context.Context, env Env, action policy.Action) error {
	fs := flag.NewFlagSet("boks policy "+action.String(), flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	sandbox, profile := scopeFlags(fs)
	note := fs.String("note", "", "why this rule exists; shown by 'boks policy ls'")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, `Usage: boks policy %s [flags] <destination>...

Stores a rule. It applies to every sandbox unless -sandbox or -profile scopes it, and it
survives the command that wrote it: this is the policy 'boks run', 'boks start' and
'boks exec' all serve a sandbox.

A deny in any scope beats an allow in any scope. A sandbox-scoped rule can add access the
machine's policy already tolerates and can take access away, but it can never widen past a
deny someone wrote down.

Destinations:
  github.com              any port
  github.com:443          one port
  api.example.com:80,443  several
  *.example.com:443       any subdomain, not the apex
  10.0.0.0/8              an address range
  [::1]:8080              an IPv6 literal with a port

Examples:
  boks policy %s github.com:443 -note "git over HTTPS"
  boks policy %s -sandbox claude-myproject api.example.com:443

Flags:
`, action, action, action)
		fs.PrintDefaults()
	}
	destinations, err := parsePolicyArgs(fs, env.Args)
	if err != nil {
		return err
	}
	if len(destinations) == 0 {
		fs.Usage()
		return errors.New("a destination is required")
	}
	scope, err := policy.ParseScope(*sandbox, *profile)
	if err != nil {
		return err
	}

	store, err := openPolicyStore()
	if err != nil {
		return err
	}
	for _, spec := range destinations {
		rule := policy.RuleSpec{Action: action, Spec: spec, Note: *note}
		added, err := store.Add(scope, rule)
		if err != nil {
			return err
		}
		if !added {
			fmt.Fprintf(env.Stdout, "already stored: %s %s in %s\n", action, spec, scope)
			continue
		}
		fmt.Fprintf(env.Stdout, "added: %s %s to %s\n", action, spec, scope)
	}
	if err := store.Save(); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "stored in %s\n", store.Path())

	if action == policy.Allow {
		warnIfStillDenied(env, store, *sandbox, destinations)
	}
	return nil
}

// warnIfStillDenied tells a user who has just written an allow that a deny still covers the
// destination, so that a rule with no effect is not mistaken for one that worked.
//
// It asks the engine rather than reasoning about the rules, which is the only way to be
// right about it — and it can only ask about destinations that name a single place, so a
// wildcard or a CIDR is passed over in silence rather than answered wrongly.
func warnIfStillDenied(env Env, store *policy.Store, sandbox string, specs []string) {
	res, err := (policy.Request{Store: store, Sandbox: sandbox}).Resolve()
	if err != nil {
		return
	}
	pol, err := res.Policy()
	if err != nil {
		return
	}
	for _, spec := range specs {
		target, ok := policy.ProbeTarget(spec)
		if !ok {
			continue
		}
		if v := pol.Evaluate(target); !v.Allowed && v.Rule != "" {
			fmt.Fprintf(env.Stderr,
				"note: %s is still denied — %s, written in %s. Deny always wins;\n"+
					"      remove that rule if you meant to permit this: boks policy rm %s\n",
				target, v.Reason, v.Scope, v.Rule)
		}
	}
}

func policyRm(_ context.Context, env Env) error {
	fs := flag.NewFlagSet("boks policy rm", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	sandbox, profile := scopeFlags(fs)
	action := fs.String("action", "", "remove only the allow or only the deny for this destination")
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks policy rm [flags] <destination>...

Removes stored rules. Without -action, both dispositions for the destination go; with it,
only the one named. A destination is matched as the engine sees it, so "GitHub.com:443"
removes the rule stored as "github.com:443".

Examples:
  boks policy rm github.com:443
  boks policy rm -sandbox web -action allow api.example.com:443

Flags:
`)
		fs.PrintDefaults()
	}
	destinations, err := parsePolicyArgs(fs, env.Args)
	if err != nil {
		return err
	}
	if len(destinations) == 0 {
		fs.Usage()
		return errors.New("a destination is required")
	}
	scope, err := policy.ParseScope(*sandbox, *profile)
	if err != nil {
		return err
	}
	var only *policy.Action
	if *action != "" {
		parsed, err := policy.ParseAction(*action)
		if err != nil {
			return err
		}
		only = &parsed
	}

	store, err := openPolicyStore()
	if err != nil {
		return err
	}
	for _, spec := range destinations {
		removed, err := store.Remove(scope, only, spec)
		if err != nil {
			return err
		}
		for _, r := range removed {
			fmt.Fprintf(env.Stdout, "removed: %s %s from %s\n", r.Action, r.Spec, scope)
		}
	}
	return store.Save()
}

func policyInit(_ context.Context, env Env) error {
	fs := flag.NewFlagSet("boks policy init", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	preset := fs.String("preset", policy.DefaultPreset, "base posture: "+strings.Join(policy.PresetNames(), ", "))
	force := fs.Bool("force", false, "overwrite an existing policy store, destroying every rule in it")
	fs.BoolVar(force, "f", false, "alias for -force")
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks policy init [flags]

Creates the durable policy store. Boks works without one — an uninitialised machine resolves
to the built-in deny-by-default preset — so this exists to choose a base posture and to give
you a file to read.

Flags:
`)
		fs.PrintDefaults()
	}
	if _, err := parsePolicyArgs(fs, env.Args); err != nil {
		return err
	}
	if _, err := policy.Preset(*preset); err != nil {
		return err
	}

	store, err := openPolicyStore()
	if err != nil {
		return err
	}
	if store.Exists() && !*force {
		return fmt.Errorf("a policy store already exists at %s with %d rule(s).\n"+
			"Use 'boks policy allow/deny' to change it, or 'boks policy init -force' to start again.",
			store.Path(), store.Count())
	}
	fresh := policy.NewStore(store.Path())
	fresh.Preset = *preset
	if err := fresh.Save(); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "initialised %s with the %s preset\n", fresh.Path(), *preset)
	fmt.Fprintf(env.Stdout, "  %s\n", policy.PresetDescription(*preset))
	return nil
}

func policyReset(_ context.Context, env Env) error {
	fs := flag.NewFlagSet("boks policy reset", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	sandbox, profile := scopeFlags(fs)
	force := fs.Bool("force", false, "do not ask for confirmation")
	fs.BoolVar(force, "f", false, "alias for -force")
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks policy reset [flags]

Restores the defaults, destroying stored rules. With no scope it clears everything: the
global rules, every sandbox's rules and every profile, and returns the base posture to the
default preset. With -sandbox or -profile it clears only that scope.

It asks first, as 'sbx reset' does, unless -f is given. Sandboxes that are already running
keep the policy they started with; this changes what the next run resolves to.

Flags:
`)
		fs.PrintDefaults()
	}
	if _, err := parsePolicyArgs(fs, env.Args); err != nil {
		return err
	}
	scope, err := policy.ParseScope(*sandbox, *profile)
	if err != nil {
		return err
	}
	all := *sandbox == "" && *profile == ""

	store, err := openPolicyStore()
	if err != nil {
		return err
	}
	if !store.Exists() {
		fmt.Fprintf(env.Stdout, "nothing to reset: no policy store at %s\n", store.Path())
		return nil
	}

	target := scope.String()
	if all {
		target = "every scope"
	}
	rules, _ := store.Rules(scope)
	count := len(rules)
	if all {
		count = store.Count()
	}
	if count == 0 {
		fmt.Fprintf(env.Stdout, "nothing to reset: %s has no rules\n", target)
		return nil
	}
	if !*force {
		ok, err := confirm(env, fmt.Sprintf(
			"This deletes %d stored rule(s) from %s in %s.\nThis cannot be undone. Continue? [y/N] ",
			count, target, store.Path()))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(env.Stdout, "cancelled; nothing was changed")
			return nil
		}
	}
	n := store.Reset(scope, all)
	if err := store.Save(); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "reset %s: %d rule(s) removed\n", target, n)
	return nil
}

// confirm asks a yes/no question. Anything but an explicit yes is a no, and a stdin that is
// not a terminal — a script, a pipe — gets the same treatment rather than a silent yes.
func confirm(env Env, prompt string) (bool, error) {
	fmt.Fprint(env.Stderr, prompt)
	if env.Stdin == nil {
		return false, nil
	}
	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil && line == "" {
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(env.Stderr)
			return false, nil
		}
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// policyCheck answers "would this be permitted?" without making the request.
//
// It is the command that turns a policy into something testable instead of something you
// discover by being blocked, and its value is entirely in agreeing with the engine — so it
// asks the engine, over the policy a run would resolve, rather than reasoning about rules.
// Nothing here contacts the network, a sandbox or containerd.
func policyCheck(_ context.Context, env Env) error {
	fs := flag.NewFlagSet("boks policy check", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	sandbox := fs.String("sandbox", "", "check as this sandbox, including rules scoped to it")
	var flags policyFlags
	flags.register(fs)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks policy check [flags] <destination>...

Reports whether a destination would be permitted, which rule decides it, and how the flow
would be carried. Nothing is contacted: this reads the stored policy and answers from the
same engine the sandbox's network stack uses.

  boks policy check github.com:443
  boks policy check -sandbox web api.example.com:443
  boks policy check -policy locked -allow example.com:443 example.com:443

A destination with no port is checked on 443.

The flow mode assumes a client that uses the proxy, which is what HTTP and HTTPS clients in a
sandbox do by default. A client that ignores HTTP_PROXY is judged in the network stack
instead — mode transparent, on the address in the packet — where hostname rules cannot apply
at all, so a hostname-only policy denies it.

Credential rules are not recorded on a sandbox, so pass -inject to see the mode a
credential-bearing host would get.

Flags:
`)
		fs.PrintDefaults()
	}
	destinations, err := parsePolicyArgs(fs, env.Args)
	if err != nil {
		return err
	}
	if len(destinations) == 0 {
		fs.Usage()
		return errors.New("a destination is required")
	}

	resolution, err := flags.resolution(*sandbox, nil)
	if err != nil {
		return err
	}
	pol, err := resolution.Policy()
	if err != nil {
		return err
	}
	credentials, err := flags.credentialRules()
	if err != nil {
		return err
	}
	mode, err := network.ParseMode(flags.mode)
	if err != nil {
		return err
	}

	for i, arg := range destinations {
		if i > 0 {
			fmt.Fprintln(env.Stdout)
		}
		target, err := policy.ParseTarget(arg, 443)
		if err != nil {
			return err
		}
		if err := writeCheck(env.Stdout, pol, resolution, target, credentials, mode); err != nil {
			return err
		}
	}
	return nil
}

// writeCheck prints one verdict. The first line is deliberately terse and greppable; the
// rest says which rule decided, where that rule is written, and what a script would have to
// change to get a different answer.
func writeCheck(w io.Writer, pol policy.Policy, res policy.Resolution, target policy.Target,
	credentials []secret.Credential, mode network.Mode) error {

	if mode == network.ModeNone {
		fmt.Fprintf(w, "DENY  %s\n", target)
		fmt.Fprint(w, "  policy: -net none\n")
		fmt.Fprint(w, "  reason: this sandbox has no network at all; no destination is reachable and no rule applies\n")
		return nil
	}

	verdict := pol.Evaluate(target)
	outcome := "DENY "
	if verdict.Allowed {
		outcome = "ALLOW"
	}
	fmt.Fprintf(w, "%s %s\n", outcome, target)
	fmt.Fprintf(w, "  policy: %s (default %s)\n", res.Name, res.Default)
	rule := verdict.Rule
	if rule == "" {
		rule = "(none matched)"
	}
	fmt.Fprintf(w, "  rule:   %s\n", rule)
	fmt.Fprintf(w, "  scope:  %s\n", scopeOrDefault(verdict.Scope))
	flowMode, why := flowModeFor(target, credentials)
	fmt.Fprintf(w, "  mode:   %s — %s\n", flowMode, why)
	fmt.Fprintf(w, "  reason: %s\n", verdict.Reason)

	if verdict.Allowed && !target.IsIP() {
		fmt.Fprint(w, "  note:   a raw connection to the address this name resolves to is judged as\n"+
			"          transparent, where hostname rules cannot apply. Add an address or CIDR\n"+
			"          rule if a non-HTTP protocol needs this destination.\n")
	}
	return nil
}

func scopeOrDefault(scope string) string {
	if scope == "" {
		return "default"
	}
	return scope
}

// flowModeFor says how a cooperating client's flow to this destination would be carried, in
// the same three words the decision log uses.
//
// The rule is the proxy's own: plaintext HTTP is read because there is nothing to break, a
// TLS flow is terminated exactly when its host has a credential rule, and anything that is
// not an HTTP port never reaches the proxy at all.
func flowModeFor(target policy.Target, credentials []secret.Credential) (policy.Mode, string) {
	switch target.Port {
	case 80:
		return policy.ModeForward, "plaintext HTTP through the proxy, which boks reads"
	case 443:
		if credentialCovers(credentials, target) {
			return policy.ModeForward, "a credential rule names this host, so boks terminates TLS for it and can read the flow"
		}
		return policy.ModeForwardBypass, "tunnelled through the proxy; end-to-end TLS, boks sees ciphertext only"
	}
	return policy.ModeTransparent, "not an HTTP port, so it is judged in the network stack by address and port"
}

// credentialCovers reports whether any credential rule names this destination — which is
// exactly the set of hosts boks decrypts. It asks the rules' own patterns rather than
// comparing hostnames, so a wildcard domain answers correctly.
func credentialCovers(credentials []secret.Credential, target policy.Target) bool {
	for _, c := range credentials {
		for _, rule := range c.Inject {
			if rule.Domain.Match(target) {
				return true
			}
		}
	}
	return false
}

// policyInspect shows the detail behind a scope or a single rule.
func policyInspect(_ context.Context, env Env) error {
	fs := flag.NewFlagSet("boks policy inspect", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	sandbox, profile := scopeFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks policy inspect [flags] [destination]

With no arguments, describes the stored policy: where it lives, what version it is, the base
posture and every scope in it.

With a destination, describes that one rule: every scope that defines it, and what the engine
would decide for it once the scopes are put together.

Examples:
  boks policy inspect
  boks policy inspect -sandbox web
  boks policy inspect github.com:443

Flags:
`)
		fs.PrintDefaults()
	}
	destinations, err := parsePolicyArgs(fs, env.Args)
	if err != nil {
		return err
	}
	store, err := openPolicyStore()
	if err != nil {
		return err
	}
	if len(destinations) > 0 {
		return inspectRule(env, store, *sandbox, destinations)
	}
	scope, err := policy.ParseScope(*sandbox, *profile)
	if err != nil {
		return err
	}
	return inspectScope(env, store, scope)
}

func inspectScope(env Env, store *policy.Store, scope policy.ScopeRef) error {
	fmt.Fprintf(env.Stdout, "store:   %s\n", store.Path())
	if !store.Exists() {
		fmt.Fprint(env.Stdout, "state:   not initialised — the built-in defaults apply ('boks policy init' to create it)\n")
	} else {
		fmt.Fprintf(env.Stdout, "version: %d\n", store.Version)
	}
	base := store.Preset
	if base == "" {
		base = policy.DefaultPreset + " (built-in default)"
	}
	fmt.Fprintf(env.Stdout, "base:    %s\n", base)
	fmt.Fprintf(env.Stdout, "scope:   %s\n\n", scope)

	if scope.Kind == policy.ScopeProfile {
		p, ok := store.Profiles[scope.Name]
		if !ok {
			return fmt.Errorf("no policy profile named %q", scope.Name)
		}
		if p.Description != "" {
			fmt.Fprintf(env.Stdout, "description: %s\n", p.Description)
		}
		preset := p.Preset
		if preset == "" {
			preset = policy.DefaultPreset + " (default)"
		}
		fmt.Fprintf(env.Stdout, "preset:      %s\n\n", preset)
	}

	rules, ok := store.Rules(scope)
	if !ok || len(rules) == 0 {
		fmt.Fprintf(env.Stdout, "no rules stored in %s\n\n", scope)
	} else {
		writeStoredRules(env.Stdout, rules)
	}

	// The rules are only half the answer; what they resolve to is the other half, and it
	// is the half a user is actually asking about.
	res, err := (policy.Request{Store: store, Sandbox: sandboxOf(scope), Profile: profileOf(scope)}).Resolve()
	if err != nil {
		return err
	}
	fmt.Fprint(env.Stdout, res.Describe())
	return nil
}

func sandboxOf(scope policy.ScopeRef) string {
	if scope.Kind == policy.ScopeSandbox {
		return scope.Name
	}
	return ""
}

func profileOf(scope policy.ScopeRef) string {
	if scope.Kind == policy.ScopeProfile {
		return scope.Name
	}
	return ""
}

// inspectRule answers "what does this destination do, and who decided that".
func inspectRule(env Env, store *policy.Store, sandbox string, specs []string) error {
	res, err := (policy.Request{Store: store, Sandbox: sandbox}).Resolve()
	if err != nil {
		return err
	}
	pol, err := res.Policy()
	if err != nil {
		return err
	}

	for i, spec := range specs {
		if i > 0 {
			fmt.Fprintln(env.Stdout)
		}
		fmt.Fprintf(env.Stdout, "destination: %s\n", spec)

		var matches []policy.RuleSpec
		for _, r := range res.Rules {
			if r.SameDestination(spec) {
				matches = append(matches, r)
			}
		}
		if len(matches) == 0 {
			fmt.Fprint(env.Stdout, "  no rule is written for exactly this destination\n")
		}
		for _, m := range matches {
			line := fmt.Sprintf("  %s in %s", m.Action, m.Scope)
			if m.Note != "" {
				line += "  — " + m.Note
			}
			fmt.Fprintln(env.Stdout, line)
		}

		target, ok := policy.ProbeTarget(spec)
		if !ok {
			fmt.Fprint(env.Stdout, "  (a pattern names a set of destinations, so there is no single verdict to report;\n"+
				"   use 'boks policy check <host>' for a specific one)\n")
			continue
		}
		v := pol.Evaluate(target)
		outcome := "denied"
		if v.Allowed {
			outcome = "allowed"
		}
		fmt.Fprintf(env.Stdout, "  effect: %s is %s — %s (%s)\n", target, outcome, v.Reason, scopeOrDefault(v.Scope))
	}
	return nil
}

// writeStoredRules prints a scope's rules as they are written down, denies first.
func writeStoredRules(w io.Writer, rules []policy.RuleSpec) {
	sorted := append([]policy.RuleSpec(nil), rules...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Action != sorted[j].Action {
			return sorted[i].Action == policy.Deny
		}
		return sorted[i].Spec < sorted[j].Spec
	})
	width := 0
	for _, r := range sorted {
		if n := len(r.Spec); n > width {
			width = n
		}
	}
	for _, r := range sorted {
		if r.Note == "" {
			fmt.Fprintf(w, "  %-5s %s\n", r.Action, r.Spec)
			continue
		}
		fmt.Fprintf(w, "  %-5s %-*s  %s\n", r.Action, width, r.Spec, r.Note)
	}
	fmt.Fprintln(w)
}

// policyProfile manages the named policies a run can select.
func policyProfile(ctx context.Context, env Env) error {
	if len(env.Args) == 0 {
		profileUsage(env.Stderr)
		return errors.New("a subcommand is required")
	}
	sub := Env{Args: env.Args[1:], Stdin: env.Stdin, Stdout: env.Stdout, Stderr: env.Stderr}
	switch env.Args[0] {
	case "-h", "--help", "help":
		profileUsage(env.Stdout)
		return nil
	case "ls", "list":
		return profileLs(ctx, sub)
	case "show":
		return profileShow(ctx, sub)
	case "create":
		return profileCreate(ctx, sub)
	case "rm":
		return profileRm(ctx, sub)
	}
	profileUsage(env.Stderr)
	return fmt.Errorf("unknown profile subcommand %q", env.Args[0])
}

func profileUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: boks policy profile <ls|show|create|rm> [flags]

A profile is a named policy: a base preset plus rules. 'boks run -profile <name>' selects
one, so a posture worth reusing is written once instead of retyped as a wall of flags.

  ls       list the stored profiles
  show     print one profile and what it resolves to
  create   create a profile
  rm       delete a profile

Rules are added to a profile with the ordinary commands:
  boks policy allow -profile ci proxy.golang.org:443

A profile decides the posture a run starts from. It cannot unsay a deny: the global and
per-sandbox rules still apply on top of it, and a deny in any of them wins.
`)
}

func profileLs(_ context.Context, env Env) error {
	fs := flag.NewFlagSet("boks policy profile ls", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	if _, err := parsePolicyArgs(fs, env.Args); err != nil {
		return err
	}
	store, err := openPolicyStore()
	if err != nil {
		return err
	}
	names := store.ProfileNames()
	if len(names) == 0 {
		fmt.Fprintf(env.Stdout, "no policy profiles are stored in %s\n", store.Path())
		fmt.Fprint(env.Stdout, "Create one with 'boks policy profile create <name>'.\n")
		return nil
	}
	for _, name := range names {
		p := store.Profiles[name]
		preset := p.Preset
		if preset == "" {
			preset = policy.DefaultPreset
		}
		fmt.Fprintf(env.Stdout, "%-16s preset %-9s %2d rule(s)", name, preset, len(p.Rules))
		if p.Description != "" {
			fmt.Fprintf(env.Stdout, "  %s", p.Description)
		}
		fmt.Fprintln(env.Stdout)
	}
	return nil
}

func profileShow(_ context.Context, env Env) error {
	fs := flag.NewFlagSet("boks policy profile show", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	destinations, err := parsePolicyArgs(fs, env.Args)
	if err != nil {
		return err
	}
	if len(destinations) == 0 {
		return errors.New("a profile name is required; list them with 'boks policy profile ls'")
	}
	store, err := openPolicyStore()
	if err != nil {
		return err
	}
	return inspectScope(env, store, policy.ProfileScope(destinations[0]))
}

func profileCreate(_ context.Context, env Env) error {
	fs := flag.NewFlagSet("boks policy profile create", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	preset := fs.String("preset", "", "base preset: "+strings.Join(policy.PresetNames(), ", ")+
		" (default "+policy.DefaultPreset+")")
	description := fs.String("description", "", "what this profile is for")
	var allow, deny stringList
	fs.Var(&allow, "allow", "allow a destination in this profile (repeatable)")
	fs.Var(&deny, "deny", "deny a destination in this profile (repeatable)")
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks policy profile create [flags] <name>

Creates a named policy. Rules can be given here or added later with
'boks policy allow -profile <name>'.

Example:
  boks policy profile create ci -preset locked -allow proxy.golang.org:443 \
      -description "dependency fetch only"

Flags:
`)
		fs.PrintDefaults()
	}
	destinations, err := parsePolicyArgs(fs, env.Args)
	if err != nil {
		return err
	}
	if len(destinations) == 0 {
		fs.Usage()
		return errors.New("a profile name is required")
	}
	name := destinations[0]

	p := policy.Profile{Preset: *preset, Description: *description}
	for _, spec := range deny {
		p.Rules = append(p.Rules, policy.RuleSpec{Action: policy.Deny, Spec: spec})
	}
	for _, spec := range allow {
		p.Rules = append(p.Rules, policy.RuleSpec{Action: policy.Allow, Spec: spec})
	}

	store, err := openPolicyStore()
	if err != nil {
		return err
	}
	if err := store.AddProfile(name, p); err != nil {
		return err
	}
	if err := store.Save(); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "created profile %s with %d rule(s) in %s\n", name, len(p.Rules), store.Path())
	fmt.Fprintf(env.Stdout, "Select it with: boks run -profile %s\n", name)
	return nil
}

func profileRm(_ context.Context, env Env) error {
	fs := flag.NewFlagSet("boks policy profile rm", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	destinations, err := parsePolicyArgs(fs, env.Args)
	if err != nil {
		return err
	}
	if len(destinations) == 0 {
		return errors.New("a profile name is required")
	}
	store, err := openPolicyStore()
	if err != nil {
		return err
	}
	for _, name := range destinations {
		if err := store.RemoveProfile(name); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "removed profile %s\n", name)
	}
	return store.Save()
}
