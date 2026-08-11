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
		fmt.Fprint(env.Stdout, "\ncredential injection (host-side only; the guest never receives a value):\n")
		for _, r := range rules {
			fmt.Fprintf(env.Stdout, "  %s\n", r)
		}
	}
	fmt.Fprintf(env.Stdout, "\n%s", notEnforcedWarning)
	return nil
}

func policyLog(_ context.Context, env Env) error {
	fset := flag.NewFlagSet("boks policy log", flag.ContinueOnError)
	fset.SetOutput(env.Stderr)
	var (
		limit = fset.Int("n", 50, "show at most this many decisions (0 for all)")
		path  = fset.String("file", policy.DefaultLogPath(), "decision log file")
	)
	fset.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks policy log [flags]

Shows recent network policy decisions: what was allowed or denied, and why. Decisions are
written by the host proxy ('boks proxy'). The log stays on this machine and is never
uploaded anywhere.

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
	for _, d := range decisions {
		fmt.Fprintln(env.Stdout, d)
	}
	return nil
}
