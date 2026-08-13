package cli

// The `boks policy` subcommands that read and write the durable store.
//
// They are kept in a file of their own, and each one is a constructor returning a
// cobra.Command, registered in policy.go and nowhere else. The decisions — precedence,
// scoping, what a rule means — live in internal/policy; nothing here decides anything about
// a policy beyond how to print it.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

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

// scopeSelector is the pair of flags that pick which scope a rule is written to, spelled as
// sbx spells them.
type scopeSelector struct {
	sandbox string
	profile string
}

func (sc *scopeSelector) register(fs *pflag.FlagSet) {
	fs.StringVar(&sc.sandbox, "sandbox", "", "scope the rule to one sandbox instead of all of them")
	fs.StringVar(&sc.profile, "profile", "", "scope the rule to a stored policy profile")
}

func (sc *scopeSelector) scope() (policy.ScopeRef, error) {
	return policy.ParseScope(sc.sandbox, sc.profile)
}

// needsDestination is the argument validator for the commands that take one, so that "you
// have to say which destination" is one message rather than four.
func needsDestination(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return usagef("a destination is required")
	}
	return nil
}

// ruleGrammar is the destination syntax, shown by every command that accepts one.
const ruleGrammar = `Destinations:
  github.com              any port
  github.com:443          one port
  api.example.com:80,443  several
  *.example.com:443       any subdomain, not the apex
  10.0.0.0/8              an address range
  [::1]:8080              an IPv6 literal with a port`

func newPolicyAllowCommand(env Env) *cobra.Command {
	return newPolicyAddCommand(env, policy.Allow)
}

func newPolicyDenyCommand(env Env) *cobra.Command {
	return newPolicyAddCommand(env, policy.Deny)
}

// newPolicyAddCommand builds `policy allow` and `policy deny`, which differ only in the
// disposition they write.
func newPolicyAddCommand(env Env, action policy.Action) *cobra.Command {
	short := "Add an allow rule, globally or scoped to one sandbox"
	if action == policy.Deny {
		short = "Add a deny rule; deny always wins"
	}
	cmd := &cobra.Command{
		Use:   action.String() + " [flags] DESTINATION...",
		Short: short,
		Long: `Stores a rule. It applies to every sandbox unless --sandbox or --profile scopes it, and it
survives the command that wrote it: this is the policy 'boks run', 'boks start' and
'boks exec' all serve a sandbox.

A deny in any scope beats an allow in any scope. A sandbox-scoped rule can add access the
machine's policy already tolerates and can take access away, but it can never widen past a
deny someone wrote down.

` + ruleGrammar,
		Example: fmt.Sprintf("  boks policy %s github.com:443 --note \"git over HTTPS\"\n"+
			"  boks policy %s --sandbox claude-myproject api.example.com:443", action, action),
		Args: needsDestination,
	}
	var (
		scope scopeSelector
		note  string
	)
	scope.register(cmd.Flags())
	cmd.Flags().StringVar(&note, "note", "", "why this rule exists; shown by 'boks policy ls'")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		ref, err := scope.scope()
		if err != nil {
			return err
		}
		store, err := openPolicyStore()
		if err != nil {
			return err
		}
		for _, spec := range args {
			added, err := store.Add(ref, policy.RuleSpec{Action: action, Spec: spec, Note: note})
			if err != nil {
				return err
			}
			if !added {
				fmt.Fprintf(env.Stdout, "already stored: %s %s in %s\n", action, spec, ref)
				continue
			}
			fmt.Fprintf(env.Stdout, "added: %s %s to %s\n", action, spec, ref)
		}
		if err := store.Save(); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "stored in %s\n", store.Path())
		if action == policy.Allow {
			warnIfStillDenied(env, store, scope.sandbox, args)
		}
		return nil
	}
	return cmd
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

func newPolicyRmCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm [flags] DESTINATION...",
		Short: "Remove a stored rule",
		Long: `Removes stored rules. Without --action, both dispositions for the destination go; with it,
only the one named. A destination is matched as the engine sees it, so "GitHub.com:443"
removes the rule stored as "github.com:443".`,
		Example: "  boks policy rm github.com:443\n" +
			"  boks policy rm --sandbox web --action allow api.example.com:443",
		Args: needsDestination,
	}
	var (
		scope  scopeSelector
		action string
	)
	scope.register(cmd.Flags())
	cmd.Flags().StringVar(&action, "action", "",
		"remove only the allow or only the deny for this destination")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		ref, err := scope.scope()
		if err != nil {
			return err
		}
		var only *policy.Action
		if action != "" {
			parsed, err := policy.ParseAction(action)
			if err != nil {
				return err
			}
			only = &parsed
		}
		store, err := openPolicyStore()
		if err != nil {
			return err
		}
		for _, spec := range args {
			removed, err := store.Remove(ref, only, spec)
			if err != nil {
				return err
			}
			for _, r := range removed {
				fmt.Fprintf(env.Stdout, "removed: %s %s from %s\n", r.Action, r.Spec, ref)
			}
		}
		return store.Save()
	}
	return cmd
}

func newPolicyInitCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init [flags]",
		Short: "Create the durable policy store",
		Long: `Creates the durable policy store. Boks works without one — an uninitialised machine
resolves to the built-in deny-by-default preset — so this exists to choose a base posture
and to give you a file to read.`,
		Args: noArgs,
	}
	var (
		preset string
		force  bool
	)
	cmd.Flags().StringVar(&preset, "preset", policy.DefaultPreset,
		"base posture: "+strings.Join(policy.PresetNames(), ", "))
	cmd.Flags().BoolVarP(&force, "force", "f", false,
		"overwrite an existing policy store, destroying every rule in it")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if _, err := policy.Preset(preset); err != nil {
			return err
		}
		store, err := openPolicyStore()
		if err != nil {
			return err
		}
		if store.Exists() && !force {
			return fmt.Errorf("a policy store already exists at %s with %d rule(s).\n"+
				"Use 'boks policy allow/deny' to change it, or 'boks policy init --force' to start again.",
				store.Path(), store.Count())
		}
		fresh := policy.NewStore(store.Path())
		fresh.Preset = preset
		if err := fresh.Save(); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "initialised %s with the %s preset\n", fresh.Path(), preset)
		fmt.Fprintf(env.Stdout, "  %s\n", policy.PresetDescription(preset))
		return nil
	}
	return cmd
}

func newPolicyResetCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset [flags]",
		Short: "Restore the defaults, destroying stored rules",
		Long: `Restores the defaults, destroying stored rules. With no scope it clears everything: the
global rules, every sandbox's rules and every profile, and returns the base posture to the
default preset. With --sandbox or --profile it clears only that scope.

It asks first, as 'sbx reset' does, unless -f is given. Sandboxes that are already running
keep the policy they started with; this changes what the next run resolves to.`,
		Args: noArgs,
	}
	var (
		scope scopeSelector
		force bool
	)
	scope.register(cmd.Flags())
	cmd.Flags().BoolVarP(&force, "force", "f", false, "do not ask for confirmation")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		ref, err := scope.scope()
		if err != nil {
			return err
		}
		all := scope.sandbox == "" && scope.profile == ""

		store, err := openPolicyStore()
		if err != nil {
			return err
		}
		if !store.Exists() {
			fmt.Fprintf(env.Stdout, "nothing to reset: no policy store at %s\n", store.Path())
			return nil
		}

		target := ref.String()
		if all {
			target = "every scope"
		}
		rules, _ := store.Rules(ref)
		count := len(rules)
		if all {
			count = store.Count()
		}
		if count == 0 {
			fmt.Fprintf(env.Stdout, "nothing to reset: %s has no rules\n", target)
			return nil
		}
		if !force {
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
		n := store.Reset(ref, all)
		if err := store.Save(); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "reset %s: %d rule(s) removed\n", target, n)
		return nil
	}
	return cmd
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

// newPolicyCheckCommand answers "would this be permitted?" without making the request.
//
// It is the command that turns a policy into something testable instead of something you
// discover by being blocked, and its value is entirely in agreeing with the engine — so it
// asks the engine, over the policy a run would resolve, rather than reasoning about rules.
// Nothing here contacts the network, a sandbox or containerd.
func newPolicyCheckCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check [flags] DESTINATION...",
		Short: "Report whether a destination would be permitted, without contacting it",
		Long: `Reports whether a destination would be permitted, which rule decides it, and how the flow
would be carried. Nothing is contacted: this reads the stored policy and answers from the
same engine the sandbox's network stack uses.

A destination with no port is checked on 443.

The flow mode assumes a client that uses the proxy, which is what HTTP and HTTPS clients in
a sandbox do by default. A client that ignores HTTP_PROXY is judged in the network stack
instead — mode transparent, on the address in the packet — where hostname rules cannot apply
at all, so a hostname-only policy denies it.

Credential rules are not recorded on a sandbox, so pass --inject to see the mode a
credential-bearing host would get.`,
		Example: "  boks policy check github.com:443\n" +
			"  boks policy check --sandbox web api.example.com:443\n" +
			"  boks policy check --policy locked --allow example.com:443 example.com:443",
		Args: needsDestination,
	}
	var (
		flags     policyFlags
		sandbox   string
		agentName string
	)
	cmd.Flags().StringVar(&sandbox, "sandbox", "", "check as this sandbox, including rules scoped to it")
	registerAgentFlag(cmd.Flags(), &agentName)
	flags.register(cmd.Flags())

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := applyAgentFlag(&flags, agentName); err != nil {
			return err
		}
		resolution, err := flags.resolution(sandbox, nil)
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

		for i, arg := range args {
			if i > 0 {
				fmt.Fprintln(env.Stdout)
			}
			target, err := policy.ParseTarget(arg, 443)
			if err != nil {
				return err
			}
			writeCheck(env.Stdout, pol, resolution, target, credentials, mode)
		}
		return nil
	}
	return cmd
}

// writeCheck prints one verdict. The first line is deliberately terse and greppable; the
// rest says which rule decided, where that rule is written, and what a script would have to
// change to get a different answer.
func writeCheck(w io.Writer, pol policy.Policy, res policy.Resolution, target policy.Target,
	credentials []secret.Credential, mode network.Mode) {

	if mode == network.ModeNone {
		fmt.Fprintf(w, "DENY  %s\n", target)
		fmt.Fprint(w, "  policy: --net none\n")
		fmt.Fprint(w, "  reason: this sandbox has no network at all; no destination is reachable and no rule applies\n")
		return
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
		// The same sentence the decision log writes for a flow nothing matched, so that
		// what check says and what the log says are one string rather than two.
		rule = policy.NoRuleFor(target)
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

// newPolicyInspectCommand shows the detail behind a scope or a single rule.
func newPolicyInspectCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect [flags] [DESTINATION...]",
		Short: "Show a scope or a single rule in detail",
		Long: `With no arguments, describes the stored policy: where it lives, what version it is, the
base posture and every scope in it.

With a destination, describes the rules bearing on it: every scope that covers it, and what
the engine would decide once the scopes are put together.`,
		Example: "  boks policy inspect\n" +
			"  boks policy inspect --sandbox web\n" +
			"  boks policy inspect github.com:443",
	}
	var scope scopeSelector
	scope.register(cmd.Flags())

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		store, err := openPolicyStore()
		if err != nil {
			return err
		}
		if len(args) > 0 {
			return inspectRule(env, store, scope.sandbox, args)
		}
		ref, err := scope.scope()
		if err != nil {
			return err
		}
		return inspectScope(env, store, ref)
	}
	return cmd
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

		target, probe := policy.ProbeTarget(spec)
		matches, err := rulesFor(res, spec, target, probe)
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			fmt.Fprint(env.Stdout, "  no rule applies to this destination\n")
		}
		for _, m := range matches {
			line := fmt.Sprintf("  %s %s in %s", m.Action, m.Spec, m.Scope)
			if m.Note != "" {
				line += "  — " + m.Note
			}
			fmt.Fprintln(env.Stdout, line)
		}

		if !probe {
			fmt.Fprint(env.Stdout, "  (a pattern names a set of destinations, so there is no single verdict to report;\n"+
				"   use 'boks policy check HOST' for a specific one)\n")
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

// rulesFor lists the rules bearing on a destination, from every scope.
//
// For a destination that names one place, that means every rule whose pattern *covers* it —
// a `deny github.com` is what a reader asking about `github.com:443` needs to see, and an
// exact-text comparison would hide it. For a pattern, which names a set, it falls back to
// the rules written with that same text, because there is no target to match against.
func rulesFor(res policy.Resolution, spec string, target policy.Target, probe bool) ([]policy.RuleSpec, error) {
	var out []policy.RuleSpec
	for _, r := range res.Rules {
		if !probe {
			if r.SameDestination(spec) {
				out = append(out, r)
			}
			continue
		}
		rule, err := r.Rule()
		if err != nil {
			return nil, err
		}
		if rule.Match(target) {
			out = append(out, r)
		}
	}
	return out, nil
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

// newPolicyProfileCommand manages the named policies a run can select.
func newPolicyProfileCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Manage named policies a run can select",
		Long: `A profile is a named policy: a base preset plus rules. 'boks run --profile NAME' selects
one, so a posture worth reusing is written once instead of retyped as a wall of flags.

Rules are added to a profile with the ordinary commands:
  boks policy allow --profile ci proxy.golang.org:443

A profile decides the posture a run starts from. It cannot unsay a deny: the global and
per-sandbox rules still apply on top of it, and a deny in any of them wins.`,
	}
	cmd.AddCommand(
		newProfileLsCommand(env),
		newProfileShowCommand(env),
		newProfileCreateCommand(env),
		newProfileRmCommand(env),
	)
	return cmd
}

func newProfileLsCommand(env Env) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the stored profiles",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openPolicyStore()
			if err != nil {
				return err
			}
			names := store.ProfileNames()
			if len(names) == 0 {
				fmt.Fprintf(env.Stdout, "no policy profiles are stored in %s\n", store.Path())
				fmt.Fprint(env.Stdout, "Create one with 'boks policy profile create NAME'.\n")
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
		},
	}
}

func newProfileShowCommand(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "show NAME",
		Short: "Print one profile and what it resolves to",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usagef("a profile name is required; list them with 'boks policy profile ls'")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openPolicyStore()
			if err != nil {
				return err
			}
			return inspectScope(env, store, policy.ProfileScope(args[0]))
		},
	}
}

func newProfileCreateCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create [flags] NAME",
		Short: "Create a profile",
		Long: `Creates a named policy. Rules can be given here or added later with
'boks policy allow --profile NAME'.`,
		Example: "  boks policy profile create ci --preset locked --allow proxy.golang.org:443 \\\n" +
			"      --description \"dependency fetch only\"",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usagef("a profile name is required")
			}
			return nil
		},
	}
	var (
		preset      string
		description string
		allow       []string
		deny        []string
	)
	cmd.Flags().StringVar(&preset, "preset", "", "base preset: "+strings.Join(policy.PresetNames(), ", ")+
		" (default "+policy.DefaultPreset+")")
	cmd.Flags().StringVar(&description, "description", "", "what this profile is for")
	cmd.Flags().StringArrayVar(&allow, "allow", nil, "allow a destination in this profile (repeatable)")
	cmd.Flags().StringArrayVar(&deny, "deny", nil, "deny a destination in this profile (repeatable)")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		p := policy.Profile{Preset: preset, Description: description}
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
		if err := store.AddProfile(args[0], p); err != nil {
			return err
		}
		if err := store.Save(); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "created profile %s with %d rule(s) in %s\n", args[0], len(p.Rules), store.Path())
		fmt.Fprintf(env.Stdout, "Select it with: boks run --profile %s\n", args[0])
		return nil
	}
	return cmd
}

func newProfileRmCommand(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "rm NAME...",
		Short: "Delete a profile",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usagef("a profile name is required")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openPolicyStore()
			if err != nil {
				return err
			}
			for _, name := range args {
				if err := store.RemoveProfile(name); err != nil {
					return err
				}
				fmt.Fprintf(env.Stdout, "removed profile %s\n", name)
			}
			return store.Save()
		},
	}
}
