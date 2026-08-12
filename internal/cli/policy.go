package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/policy"
)

func policyCommand(ctx context.Context, env Env) error {
	if len(env.Args) == 0 {
		policyUsage(env.Stderr)
		return errors.New("a subcommand is required")
	}
	sub := Env{Args: env.Args[1:], Stdin: env.Stdin, Stdout: env.Stdout, Stderr: env.Stderr}
	switch env.Args[0] {
	case "-h", "--help", "help":
		policyUsage(env.Stdout)
		return nil
	case "ls":
		return policyLs(ctx, sub)
	case "log":
		return policyLog(ctx, sub)
	}
	policyUsage(env.Stderr)
	return fmt.Errorf("unknown policy subcommand %q", env.Args[0])
}

func policyUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: boks policy <ls|log> [flags]

  ls    show the rules a policy resolves to
  log   show recent policy decisions

Run 'boks policy ls -h' or 'boks policy log -h' for flags.
`)
}

func policyLs(_ context.Context, env Env) error {
	fset := flag.NewFlagSet("boks policy ls", flag.ContinueOnError)
	fset.SetOutput(env.Stderr)
	var flags policyFlags
	flags.register(fset)
	fset.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks policy ls [flags]

Resolves a preset plus any -allow/-deny rules and prints the result, deny rules first
because they always win. Nothing here contacts the network or a sandbox.

Presets:
`)
		for _, name := range policy.PresetNames() {
			fmt.Fprintf(env.Stderr, "  %-9s %s\n", name, policy.PresetDescription(name))
		}
		fmt.Fprint(env.Stderr, "\nFlags:\n")
		fset.PrintDefaults()
	}
	if err := fset.Parse(env.Args); err != nil {
		if err == flag.ErrHelp {
			return flagErrHelp
		}
		return err
	}

	p, err := flags.resolve()
	if err != nil {
		return err
	}
	fmt.Fprint(env.Stdout, p.Describe())

	// The network mode decides whether there is a network for the policy to apply to at
	// all, so show it alongside the rules rather than in a separate command.
	plan, err := flags.networkPlan("boks-example")
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "\nnetwork mode: %s\n", plan.Mode)
	if plan.Mode == network.ModeNone {
		fmt.Fprint(env.Stdout, "  the sandbox gets loopback and nothing else; no rule can apply\n")
	} else {
		fmt.Fprintf(env.Stdout, "  guest %s via gateway %s, resolver %s\n",
			plan.GuestAddr, plan.Gateway, plan.Gateway)
		fmt.Fprint(env.Stdout, hostnameRuleCaveat(p))
	}
	keys := make([]string, 0, len(plan.Annotations()))
	for k := range plan.Annotations() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(env.Stdout, "  %s=%s\n", k, plan.Annotations()[k])
	}

	rules, err := flags.credentialRules()
	if err != nil {
		return err
	}
	if len(rules) > 0 {
		fmt.Fprint(env.Stdout, "\ncredentials (host-side only; the guest never receives a value):\n")
		for _, c := range rules {
			fmt.Fprintf(env.Stdout, "  %s\n", c)
			if c.Placeholder != "" {
				name := c.EnvName
				if name == "" {
					name = "(no environment variable configured)"
				}
				// The placeholder is not a secret — that is the whole point of it — so
				// printing it is safe and useful: it is what you set in the guest.
				fmt.Fprintf(env.Stdout, "    guest holds %s=%s\n", name, c.Placeholder)
			}
		}
		fmt.Fprintf(env.Stdout, "\n%s", interceptionNotice(rules))
	}
	fmt.Fprintf(env.Stdout, "\n%s\n", enforcementNote)
	return nil
}

func policyLog(_ context.Context, env Env) error {
	fset := flag.NewFlagSet("boks policy log", flag.ContinueOnError)
	fset.SetOutput(env.Stderr)
	var (
		limit = fset.Int("n", 500, "read at most this many decisions (0 for all)")
		path  = fset.String("file", policy.DefaultLogPath(), "decision log file")
		raw   = fset.Bool("raw", false, "one line per decision instead of one per destination")
	)
	fset.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks policy log [flags]

Shows recent network policy decisions: what was allowed or denied, how the flow was
carried, and why. Decisions are written by the sandbox's network stack and by the proxy
inside it. The log stays on this machine and is never uploaded anywhere.

Identical decisions are collapsed into one row with a count, because a single dependency
install produces hundreds of them and the one denial that explains a failure should not be
buried. Use -raw for the unaggregated form.

The PROXY column is the part to read when you care about confidentiality:

  forward          boks handled this at the HTTP level and could read it — plaintext
                   HTTP, or HTTPS terminated because the host has a credential rule
  forward-bypass   tunnelled untouched; end-to-end TLS, boks saw ciphertext only
  transparent      judged in the network stack, by address and port, without the proxy
                   being involved at all — what a raw socket or a non-HTTP protocol
                   produces. boks saw a destination, and nothing else.

Flags:
`)
		fset.PrintDefaults()
	}
	if err := fset.Parse(env.Args); err != nil {
		if err == flag.ErrHelp {
			return flagErrHelp
		}
		return err
	}

	f, err := os.Open(*path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(env.Stdout, "no decisions recorded yet (%s does not exist)\n", *path)
			fmt.Fprint(env.Stdout, "Decisions appear once traffic passes through 'boks proxy'.\n")
			return nil
		}
		return fmt.Errorf("opening decision log: %w", err)
	}
	defer f.Close()

	decisions, err := policy.ReadDecisions(f, *limit)
	if err != nil {
		return err
	}
	if len(decisions) == 0 {
		fmt.Fprintf(env.Stdout, "no decisions recorded in %s\n", *path)
		return nil
	}
	if *raw {
		for _, d := range decisions {
			fmt.Fprintln(env.Stdout, d)
		}
		return nil
	}
	writeDecisionTable(env.Stdout, policy.Aggregated(decisions), time.Now())
	return nil
}

// writeDecisionTable prints aggregated decisions, blocked first.
//
// Blocked requests come first because they are the ones someone is looking for: a run that
// failed did so for a reason that is in that section. The columns follow Docker Sandboxes'
// own layout, so that a person who knows one log can read the other.
func writeDecisionTable(w io.Writer, rows []policy.Aggregate, now time.Time) {
	var blocked, allowed []policy.Aggregate
	for _, r := range rows {
		if r.Allowed {
			allowed = append(allowed, r)
			continue
		}
		blocked = append(blocked, r)
	}

	section := func(title string, rows []policy.Aggregate) {
		fmt.Fprintf(w, "%s\n", title)
		if len(rows) == 0 {
			fmt.Fprint(w, "  (none)\n\n")
			return
		}
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprint(tw, "  SANDBOX\tTYPE\tHOST\tPROXY\tRULE\tREASON\tLAST SEEN\tCOUNT\n")
		for _, r := range rows {
			sandbox := r.Sandbox
			if sandbox == "" {
				sandbox = "-"
			}
			mode := string(r.Mode)
			if mode == "" {
				mode = "-"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n",
				sandbox, r.Type, r.Destination(), mode, r.Rule, r.Reason, since(now, r.LastSeen), r.Count)
		}
		tw.Flush()
		fmt.Fprintln(w)
	}
	section("Blocked requests:", blocked)
	section("Allowed requests:", allowed)
	fmt.Fprint(w, "PROXY: forward = boks read this flow · forward-bypass = tunnelled, ciphertext only · "+
		"transparent = judged in the network stack, by address\n")
}

// since renders an age the way a person reads one.
func since(now, then time.Time) string {
	d := now.Sub(then)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return then.Format(time.RFC3339)
}
