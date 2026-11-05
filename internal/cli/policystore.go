package cli

// The `boks policy` subcommands that read and write the durable store.
//
// They are kept in a file of their own, and each one is a constructor returning a
// cobra.Command, registered in policy.go and nowhere else. The decisions — precedence,
// scoping, what a rule means — live in internal/policy; nothing here decides anything about
// a policy beyond how to print it.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/dagsommer/boks/internal/enforce"
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

// httpRuleHelp explains the HTTP selector flags, in the words sbx uses for the same flags plus
// what Boks does with a flow it cannot read.
const httpRuleHelp = `HTTP rules:
  Use --method to add an HTTP rule. --method accepts comma-separated HTTP methods or ANY;
  --path defaults to /**. Repeat --query name=value to require query parameters. A path glob's
  '*' matches within one segment and '**' across segments; a query value is URL-query-encoded,
  with '*' as a wildcard (%2A is a literal star). The destination must be a host pattern, not
  an IP address or a range.

  An HTTP rule is applied to requests boks reads: plaintext HTTP through the proxy, and HTTPS
  that boks decrypts with its CA, which it does for a host with an HTTP rule as it does for a
  host with a credential. A flow boks cannot read — a raw socket, a tunnel without the CA — is
  refused if an HTTP rule decides it: an HTTP allow never opens raw access to the host, and an
  HTTP deny on an otherwise allowed host closes raw access to it, because the deny could not be
  enforced there. A sandbox gets the CA when it is created with a credential or an HTTP rule.`

// httpSelector is the --method, --path and --query flags, as sbx spells them.
type httpSelector struct {
	methods []string
	path    string
	query   []string
}

func (h *httpSelector) register(fs *pflag.FlagSet, verb string) {
	fs.StringSliceVar(&h.methods, "method", nil,
		verb+" the rule to these HTTP methods (comma-separated); use ANY to match all methods")
	fs.StringVar(&h.path, "path", "",
		verb+" the rule to this HTTP path glob (e.g. /repos/**); defaults to /**")
	fs.StringArrayVar(&h.query, "query", nil,
		"require an HTTP query parameter (name=value pattern); repeatable")
}

// spec builds the stored form of a rule for one destination.
func (h *httpSelector) spec(action policy.Action, dest, note string) policy.RuleSpec {
	r := policy.RuleSpec{Action: action, Spec: dest, Note: note}
	if len(h.methods) > 0 || h.path != "" || len(h.query) > 0 {
		// Validated, and the path default filled in, by compiling it: what is stored is the
		// canonical form, so the file reads the way 'boks policy ls' prints it.
		rule, err := policy.ParseHTTPRule(action, dest, h.methods, h.path, h.query)
		if err == nil && rule.HTTP != nil {
			r.Methods, r.Path, r.Query = rule.HTTP.Methods(), rule.HTTP.Path(), rule.HTTP.Query()
			return r
		}
		// Keep what was typed, so the error from the store names the user's own input.
		r.Methods, r.Path, r.Query = h.methods, h.path, h.query
	}
	return r
}

func (h *httpSelector) isSet() bool { return len(h.methods) > 0 || h.path != "" || len(h.query) > 0 }

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

A rule is refused, and nothing is added, when the same scope already holds the opposite
decision for the same destination (for HTTP rules, the same methods, path and query
constraints). Remove the existing rule first; rules in different scopes can coexist, and the
deny takes precedence.

` + ruleGrammar + "\n\n" + httpRuleHelp,
		Example: fmt.Sprintf("  boks policy %s github.com:443 --note \"git over HTTPS\"\n"+
			"  boks policy %s --sandbox claude-myproject api.example.com:443\n", action, action) +
			map[policy.Action]string{
				policy.Allow: "  boks policy allow api.example.com --method GET,HEAD --path '/v1/**'\n" +
					"  boks policy allow api.example.com --method ANY --query team=platform",
				policy.Deny: "  boks policy deny api.example.com --method POST --path '/admin/**'",
			}[action],
		Args: needsDestination,
	}
	var (
		scope    scopeSelector
		note     string
		selector httpSelector
	)
	scope.register(cmd.Flags())
	cmd.Flags().StringVar(&note, "note", "", "why this rule exists; shown by 'boks policy ls'")
	selector.register(cmd.Flags(), "restrict")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		ref, err := scope.scope()
		if err != nil {
			return err
		}
		store, err := openPolicyStore()
		if err != nil {
			return err
		}
		warnCount := 0
		skippedWarns := 0
		capWarnings := len(args) > 3
		for _, spec := range args {
			rule := selector.spec(action, spec, note)
			added, err := store.Add(ref, rule)
			if err != nil {
				return err
			}
			if !added {
				fmt.Fprintf(env.Stdout, "already stored: %s %s in %s\n", action, rule.Describe(), ref)
				continue
			}
			fmt.Fprintf(env.Stdout, "added: %s %s to %s\n", action, rule.Describe(), ref)
			if action == policy.Allow {
				if rule, parseErr := policy.ParseRule(action, spec); parseErr == nil && rule.WarnSingleLabel() {
					if !capWarnings || warnCount < 2 {
						fmt.Fprintf(env.Stderr,
							"warning: %q looks like a local name or shell glob expansion mistake — single-label hostnames are rarely real network destinations.\n"+
								"If you meant to allow all traffic, use: boks policy %s '*'\n"+
								"Remove it with 'boks policy rm %s' if it was a mistake.\n",
							spec, action, spec)
						warnCount++
					} else {
						skippedWarns++
					}
				}
			}
		}
		if skippedWarns > 0 {
			fmt.Fprintf(env.Stderr, "warning: %d more single-label hostname(s) skipped (same issue)\n", skippedWarns)
		}
		if err := store.Save(); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "stored in %s\n", store.Path())
		if selector.isSet() {
			fmt.Fprint(env.Stderr, "note: an HTTP rule is applied to requests boks reads. For HTTPS that means boks\n"+
				"      decrypts flows to this host with its CA; a sandbox created with neither a\n"+
				"      credential nor an HTTP rule has no Boks CA and must be recreated (boks rm) to\n"+
				"      reach the host. A flow boks cannot read is refused when this rule decides it.\n")
		}
		if action == policy.Allow {
			warnIfStillDenied(env, store, scope.sandbox, args)
			hotReloadAllows(cmd.Context(), env, scope.sandbox)
		} else {
			// Deny rules are not applied to running sandboxes — they take effect at
			// the next start. Let the user know if any sandboxes are currently up.
			stateDir := policy.StateDir()
			running := enforce.List(stateDir)
			if len(running) > 0 {
				fmt.Fprintln(env.Stderr, "note: deny rules take effect when the sandbox is next started. Running sandboxes are not affected.")
			}
		}
		return nil
	}
	return cmd
}

// hotReloadAllows attempts to apply new allow rules to running sandboxes without a restart.
//
// For a sandbox-scoped rule, only that sandbox is reloaded. For a global rule, every
// running sandbox is reloaded. Sandboxes that are not running are silently skipped.
// Any other reload failure prints a warning but does not fail the command: the rule is
// already durable in the store and takes effect at the next start.
func hotReloadAllows(ctx context.Context, env Env, sandbox string) {
	stateDir := policy.StateDir()

	if sandbox != "" {
		// Scoped rule: reload only the named sandbox.
		warnings, err := enforce.PolicyReload(ctx, stateDir, sandbox)
		if err != nil {
			if !errors.Is(err, enforce.ErrNoSupervisor) {
				fmt.Fprintf(env.Stderr, "warning: could not apply the new rule to the running sandbox %s: %v\n"+
					"  It will take effect when the sandbox is next started.\n", sandbox, err)
				return
			}
			return // sandbox not running; silently skip
		}
		for _, w := range warnings {
			fmt.Fprintf(env.Stderr, "note: %s\n", w)
		}
		fmt.Fprintf(env.Stdout, "rule applied to running sandbox %s\n", sandbox)
		return
	}

	// Global rule: reload every running sandbox.
	for _, st := range enforce.List(stateDir) {
		// -net none sandboxes have no policy engine; the call() function returns a
		// plain error (not ErrNoSupervisor) for them, so skip them explicitly here.
		if st.Mode == string(network.ModeNone) {
			continue
		}
		warnings, err := enforce.PolicyReload(ctx, stateDir, st.Sandbox)
		if err != nil {
			if !errors.Is(err, enforce.ErrNoSupervisor) {
				fmt.Fprintf(env.Stderr, "warning: could not apply the new rule to the running sandbox %s: %v\n"+
					"  It will take effect when the sandbox is next started.\n", st.Sandbox, err)
			}
			continue
		}
		for _, w := range warnings {
			fmt.Fprintf(env.Stderr, "note: %s\n", w)
		}
		fmt.Fprintf(env.Stdout, "rule applied to running sandbox %s\n", st.Sandbox)
	}
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
		// A NeedsHTTP refusal is not "still denied": it is what an HTTP rule is supposed to do
		// to a flow boks cannot read, and saying otherwise would send the user to remove the
		// rule they just wrote.
		if v := pol.Evaluate(target); !v.Allowed && v.Rule != "" && !v.NeedsHTTP {
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
removes the rule stored as "github.com:443".

An HTTP rule is identified by its selector as well as its destination: give the same
--method, --path and --query it was added with (the path defaults to /**, as when adding).
Without --method, only the destination's connection rules are removed; if it has only HTTP
rules, the error lists the command that removes each.`,
		Example: "  boks policy rm github.com:443\n" +
			"  boks policy rm --sandbox web --action allow api.example.com:443\n" +
			"  boks policy rm api.example.com --method GET --path '/v1/**'",
		Args: needsDestination,
	}
	var (
		scope    scopeSelector
		action   string
		selector httpSelector
	)
	scope.register(cmd.Flags())
	cmd.Flags().StringVar(&action, "action", "",
		"remove only the allow or only the deny for this destination")
	selector.register(cmd.Flags(), "identify")

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
			removed, err := store.RemoveRule(ref, only, selector.spec(policy.Deny, spec, ""))
			if err != nil {
				return err
			}
			for _, r := range removed {
				fmt.Fprintf(env.Stdout, "removed: %s %s from %s\n", r.Action, r.Describe(), ref)
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

It asks first unless -f is given. Sandboxes that are already running keep the policy they
started with; this changes what the next run resolves to.`,
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
credential-bearing host would get.

An http:// or https:// URL is checked as an HTTP request (GET unless --method says
otherwise): network access first, then the HTTP method, path and query rules when the host
has any, as 'sbx policy check http' does. Any other destination is checked as a connection;
a host whose verdict depends on HTTP rules is reported as refused to a flow boks cannot read.
The 'http' and 'network' subcommands are sbx's spellings of the two.`,
		Example: "  boks policy check github.com:443\n" +
			"  boks policy check --sandbox web api.example.com:443\n" +
			"  boks policy check --policy locked --allow example.com:443 example.com:443\n" +
			"  boks policy check --method POST https://api.example.com/v1/items\n" +
			"  boks policy check http 'https://api.example.com/search?team=platform'",
		Args: needsDestination,
	}
	cmd.RunE = policyCheckRunner(env, cmd, checkAuto)
	cmd.AddCommand(newPolicyCheckKindCommand(env, checkHTTP), newPolicyCheckKindCommand(env, checkNetwork))
	return cmd
}

// checkKind says how `policy check` reads its arguments.
type checkKind int

const (
	checkAuto    checkKind = iota // a URL is an HTTP request, anything else a connection
	checkHTTP                     // sbx's `check http URL`
	checkNetwork                  // sbx's `check network TARGET`
)

func newPolicyCheckKindCommand(env Env, kind checkKind) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "http [flags] URL...",
		Short: "Check an HTTP request to a URL, without making it",
		Long: `Evaluates the policy for an HTTP request to URL. URL must be an absolute HTTP or HTTPS URL.
The check evaluates network access first, then the HTTP method, path and query rules when the
host has any. Nothing is contacted.`,
		Example: "  boks policy check http https://api.example.com/v1/items\n" +
			"  boks policy check http --method POST https://api.example.com/v1/items",
		Args: needsDestination,
	}
	if kind == checkNetwork {
		cmd.Use = "network [flags] TARGET..."
		cmd.Short = "Check network access to a host, without contacting it"
		cmd.Long = `Checks whether the policy allows a connection to TARGET. TARGET may be a hostname, host:port,
IP literal, or URL; a bare host is checked on 443, and a URL supplies only its host and port:
this evaluates network authorization, not HTTP method or path.`
		cmd.Example = "  boks policy check network api.example.com\n" +
			"  boks policy check network https://api.example.com/v1"
	}
	cmd.RunE = policyCheckRunner(env, cmd, kind)
	return cmd
}

// policyCheckRunner registers the check flags on cmd and returns its RunE.
func policyCheckRunner(env Env, cmd *cobra.Command, kind checkKind) func(*cobra.Command, []string) error {
	var (
		flags     policyFlags
		sandbox   string
		agentName string
		method    string
	)
	cmd.Flags().StringVar(&sandbox, "sandbox", "", "check as this sandbox, including rules scoped to it")
	registerAgentFlag(cmd.Flags(), &agentName)
	flags.register(cmd.Flags())
	if kind != checkNetwork {
		cmd.Flags().StringVar(&method, "method", "", `HTTP method to evaluate for a URL (default "GET")`)
	}

	return func(cmd *cobra.Command, args []string) error {
		if err := applyAgentFlag(&flags, agentName); err != nil {
			return err
		}
		// So that `boks policy check --kit ./x https://host` answers the question the
		// user actually has — would this destination be reachable WITH that kit — rather
		// than silently answering it without.
		if err := flags.loadKit(env.Stderr); err != nil {
			return err
		}
		if err := flags.checkKitAgent(flags.agent.Name); err != nil {
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
			target, req, isRequest, err := parseCheckArg(arg, kind, method)
			if err != nil {
				return err
			}
			if isRequest {
				writeCheckHTTP(env.Stdout, pol, resolution, target, req, credentials, mode)
				continue
			}
			writeCheck(env.Stdout, pol, resolution, target, credentials, mode)
		}
		return nil
	}
}

// parseCheckArg reads one `policy check` argument: a URL becomes an HTTP request unless the
// kind is network, and anything else is a connection target.
func parseCheckArg(arg string, kind checkKind, method string) (policy.Target, policy.HTTPRequest, bool, error) {
	u, err := url.Parse(arg)
	isURL := err == nil && u.Scheme != "" && u.Host != ""
	isHTTPURL := isURL && (strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https"))

	if kind == checkHTTP && !isHTTPURL {
		return policy.Target{}, policy.HTTPRequest{}, false, usagef("URL must be an absolute HTTP or HTTPS URL, got %q", arg)
	}
	if kind == checkAuto && method != "" && !isHTTPURL {
		return policy.Target{}, policy.HTTPRequest{}, false,
			usagef("--method needs an http:// or https:// URL to check, got %q", arg)
	}
	if !isURL {
		t, err := policy.ParseTarget(arg, 443)
		return t, policy.HTTPRequest{}, false, err
	}
	port := 0
	switch {
	case u.Port() != "":
		port, err = strconv.Atoi(u.Port())
		if err != nil {
			return policy.Target{}, policy.HTTPRequest{}, false, fmt.Errorf("port %q in %q is not a number", u.Port(), arg)
		}
	case isHTTPURL:
		port = defaultPortFor(u.Scheme)
	default:
		return policy.Target{}, policy.HTTPRequest{}, false,
			usagef("a %s:// URL must include an explicit port", u.Scheme)
	}
	t, err := policy.NewTarget(u.Hostname(), port)
	if err != nil {
		return policy.Target{}, policy.HTTPRequest{}, false, err
	}
	if !isHTTPURL || kind == checkNetwork {
		return t, policy.HTTPRequest{}, false, nil
	}
	if method == "" {
		method = http.MethodGet
	}
	return t, policy.HTTPRequest{Method: strings.ToUpper(method), Path: u.EscapedPath(), RawQuery: u.RawQuery}, true, nil
}

func defaultPortFor(scheme string) int {
	if strings.EqualFold(scheme, "http") {
		return 80
	}
	return 443
}

// writeCheckHTTP prints the verdict on one HTTP request: the connection first, then — when the
// host has HTTP rules — the request, which is what decides it.
func writeCheckHTTP(w io.Writer, pol policy.Policy, res policy.Resolution, target policy.Target,
	req policy.HTTPRequest, credentials []secret.Credential, mode network.Mode) {

	if mode == network.ModeNone || !pol.HasHTTPRules(target) {
		writeCheck(w, pol, res, target, credentials, mode)
		fmt.Fprintf(w, "  request: %s %s — no HTTP rule covers this host, so the connection's verdict applies to every request\n",
			req.Method, req.CanonicalPath())
		return
	}
	verdict := pol.EvaluateHTTP(target, req)
	outcome := "DENY "
	if verdict.Allowed {
		outcome = "ALLOW"
	}
	fmt.Fprintf(w, "%s %s %s %s\n", outcome, req.Method, target, req.CanonicalPath())
	fmt.Fprintf(w, "  policy: %s (default %s)\n", res.Name, res.Default)
	rule := verdict.Rule
	if rule == "" {
		rule = policy.NoRuleFor(target)
	}
	fmt.Fprintf(w, "  rule:   %s\n", rule)
	fmt.Fprintf(w, "  scope:  %s\n", scopeOrDefault(verdict.Scope))
	flowMode, why := flowModeFor(target, credentials, pol)
	fmt.Fprintf(w, "  mode:   %s — %s\n", flowMode, why)
	fmt.Fprintf(w, "  reason: %s\n", verdict.Reason)
	if conn := pol.Evaluate(target); conn.NeedsHTTP {
		fmt.Fprint(w, "  note:   HTTP rules decide this host, so only requests boks reads are judged this way;\n"+
			"          a flow it cannot read (a raw socket, or HTTPS in a sandbox without the Boks CA)\n"+
			"          is refused.\n")
	}
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
	flowMode, why := flowModeFor(target, credentials, pol)
	fmt.Fprintf(w, "  mode:   %s — %s\n", flowMode, why)
	fmt.Fprintf(w, "  reason: %s\n", verdict.Reason)

	if verdict.NeedsHTTP {
		host := target.String()
		if target.Port == 443 {
			host = target.Host
		}
		fmt.Fprintf(w, "  note:   HTTP rules decide this host; a request boks reads is judged on its method,\n"+
			"          path and query. Check one with: boks policy check --method GET https://%s/PATH\n", host)
		return
	}
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
// TLS flow is terminated exactly when its host has a credential rule or an HTTP policy rule,
// and anything that is not an HTTP port never reaches the proxy at all.
func flowModeFor(target policy.Target, credentials []secret.Credential, pol policy.Policy) (policy.Mode, string) {
	switch target.Port {
	case 80:
		return policy.ModeForward, "plaintext HTTP through the proxy, which boks reads"
	case 443:
		if credentialCovers(credentials, target) {
			return policy.ModeForward, "a credential rule names this host, so boks terminates TLS for it and can read the flow"
		}
		if pol.HasHTTPRules(target) {
			return policy.ModeForward, "an HTTP rule names this host, so boks terminates TLS for it and reads each request " +
				"(the sandbox needs the Boks CA, which it gets when created with a credential or an HTTP rule)"
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
			line := fmt.Sprintf("  %s %s in %s", m.Action, m.Describe(), m.Scope)
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
		switch {
		case v.Allowed:
			outcome = "allowed"
		case v.NeedsHTTP:
			outcome = "decided request by request by HTTP rules, and refused to a flow boks cannot read"
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
		return sorted[i].Describe() < sorted[j].Describe()
	})
	width := 0
	for _, r := range sorted {
		if n := len(r.Describe()); n > width {
			width = n
		}
	}
	for _, r := range sorted {
		if r.Note == "" {
			fmt.Fprintf(w, "  %-5s %s\n", r.Action, r.Describe())
			continue
		}
		fmt.Fprintf(w, "  %-5s %-*s  %s\n", r.Action, width, r.Describe(), r.Note)
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
