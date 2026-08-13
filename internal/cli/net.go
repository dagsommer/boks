package cli

import (
	"context"
	"fmt"
	"io"
	"maps"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/enforce"
	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/sandbox"
	"github.com/dagsommer/boks/internal/secret"
)

// newNetCommand inspects and controls the per-sandbox network stacks.
//
// A sandbox's network lives in a process of its own, because the stack terminates the
// guest's NIC and has to last exactly as long as the VM does — see internal/enforce. That
// process is spawned on demand by `run`, `exec` and `start`, and it ends when the sandbox
// stops. This command is how a person sees it and ends it by hand: a background process
// nobody can list or stop is a thing users are right to distrust.
func newNetCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "net",
		Short: "Inspect or stop the network stack serving a sandbox",
		Long: `A sandbox's network is a host-side stack that terminates the guest's virtual NIC, with a
filtering proxy listening inside the sandbox's own virtual network. It runs in a process of
its own so that it lasts as long as the sandbox's VM does rather than as long as the command
that started it: a build running in a sandbox does not lose the network because you pressed
Ctrl-C in another terminal.

One process per running sandbox, started on demand and never at boot. It exits when the
sandbox's task exits, so 'boks stop' takes it with the sandbox.

'boks net serve' is what the others spawn. It is a normal command rather than a hidden one
so that the background process can be reproduced, watched and debugged.`,
	}
	cmd.AddCommand(newNetLsCommand(env), newNetStopCommand(env), newNetServeCommand(env))
	return cmd
}

func newNetLsCommand(env Env) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the running sandbox network stacks",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			states := enforce.List(policy.StateDir())
			if len(states) == 0 {
				fmt.Fprintln(env.Stderr, "no sandbox network stacks are running")
				return nil
			}
			w := tabwriter.NewWriter(env.Stdout, 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "SANDBOX\tMODE\tPROXY\tINTERCEPT\tPID\tUPTIME")
			for _, st := range states {
				proxy := st.ProxyURL
				if proxy == "" {
					proxy = "-"
				}
				intercept := "no"
				if st.Intercept {
					intercept = "yes"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n",
					st.Sandbox, st.Mode, proxy, intercept, st.PID,
					time.Since(st.Started).Round(time.Second))
			}
			return w.Flush()
		},
	}
}

func newNetStopCommand(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "stop SANDBOX...",
		Short: "End a sandbox's network stack",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usagef("a sandbox name is required; run 'boks net ls' to see what is running")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, name := range args {
				if err := enforce.Stop(policy.StateDir(), name); err != nil {
					return err
				}
				fmt.Fprintf(env.Stderr, "stopped the network stack for %s\n", name)
			}
			return nil
		},
	}
}

// newNetServeCommand is the supervisor process. It reads its specification from stdin rather
// than from flags so that the secret values it needs never appear in a command line, where
// every other process on the host could read them.
func newNetServeCommand(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "serve < spec.json",
		Short: "Run one stack in the foreground, reading its configuration from stdin",
		Long: `Runs one sandbox's network stack in the foreground: the host-side stack that terminates the
guest's NIC, and the filtering proxy inside the sandbox's virtual network. It exits when the
sandbox's task exits, or on SIGTERM.

The specification arrives on stdin as JSON, because it carries the credential values the
proxy attaches to requests and those must not be visible in the process table.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := enforce.ReadSpec(env.Stdin)
			if err != nil {
				return err
			}
			return enforce.Serve(cmd.Context(), spec, env.Stdout, func(ctx context.Context) error {
				// The stack's life is the VM's life. Watching the task is what
				// makes that true without a second supervision mechanism:
				// containerd already knows.
				return sandbox.WaitUntilStopped(ctx, spec.Address, spec.Sandbox,
					enforce.TaskAppearTimeout, enforce.TaskPollInterval)
			})
		},
	}
}

// networkFor decides which network a sandbox gets, honouring what an existing sandbox was
// already wired for, and tells the user when the two differ.
//
// The mode is fixed when a sandbox is created, because it lives in annotations the runtime
// reads at boot. Silently applying `-net none` to a sandbox created with a network would be
// the worst kind of wrong: the flag would appear to be obeyed while the container stayed
// connected.
func networkFor(inv invocation, requested network.Mode, explicit bool, stderr io.Writer) (mode network.Mode, wired bool) {
	if !inv.exists {
		return requested, true
	}
	existing, wired := network.ModeFromAnnotations(inv.info.Annotations)
	if !wired {
		fmt.Fprintf(stderr, "%s", unwiredSandboxWarning(inv.name))
		return existing, false
	}
	if explicit && existing != requested {
		fmt.Fprintf(stderr,
			"note: sandbox %q is wired for -net %s, which is fixed when a sandbox is created.\n"+
				"      Remove it and run again to change it: boks rm %s\n",
			inv.name, existing, inv.name)
	}
	return existing, true
}

// unwiredSandboxWarning covers a sandbox created before Boks wired networking, or by
// something else. It is worth a warning rather than a note: such a sandbox is not merely
// unenforced, it is on the runtime's default transport, where the guest reaches host
// loopback services.
func unwiredSandboxWarning(name string) string {
	return fmt.Sprintf(
		"WARNING: sandbox %q was created without Boks network annotations, so it runs on the\n"+
			"         runtime's default transport (libkrun's TSI). There, the guest's 127.0.0.1 is\n"+
			"         the host's, and no policy can be applied to it at all.\n"+
			"         Recreate it to get an enforced network: boks rm %s\n\n", name, name)
}

// attachSandboxNetwork decides, describes and starts the network for a sandbox that is
// about to run, and fills in what the sandbox has to be created with.
//
// The order is forced by the runtime: the annotations must be on the container when it is
// created, and the host-side stack must already hold the link socket when the VM boots,
// because the VM connects to it during boot. It returns whether this invocation started the
// stack, which is what tells the caller whether it owns cleaning it up.
func attachSandboxNetwork(ctx context.Context, flags *policyFlags, inv invocation, cfg *sandbox.Config,
	requested network.Mode, quiet bool, env Env) (bool, error) {

	mode, wired := networkFor(inv, requested, flags.mode != "", env.Stderr)
	if !wired {
		// A sandbox Boks did not wire has no link socket to hold and no stack that
		// could reach it. networkFor has already said so, loudly.
		return false, nil
	}

	// A sandbox that exists carries its own policy selection; a new one is about to be
	// created with this run's. Either way the record travels with the container, so that
	// `start` and `exec` — which have no policy flags — serve the same containment.
	record := flags.sandboxRecord()
	if inv.exists {
		record = inv.info.Policy
		if flags.policySpecified() {
			fmt.Fprintf(env.Stderr,
				"note: sandbox %q was created with %s. The policy flags on this run apply to\n"+
					"      the stack it is about to get, and are not recorded on the sandbox:\n"+
					"      a later 'boks start' will serve it what it was created with.\n",
				inv.name, record.String())
			record = flags.sandboxRecord()
		}
	}

	spec, err := flags.enforceSpec(ctx, inv.name, cfg.Address, mode, record)
	if err != nil {
		return false, err
	}
	if err := describeNetwork(flags, spec, mode, quiet, env.Stderr); err != nil {
		return false, err
	}
	guest, err := spec.Prepare()
	if err != nil {
		return false, err
	}
	if !inv.exists {
		cfg.Policy = record
	}
	cfg.Annotations = withNetworkAnnotations(guest.Annotations, cfg.Annotations)
	cfg.Env = append(cfg.Env, guest.Env...)
	cfg.Mounts = guest.Mounts

	running := inv.exists && inv.info.Status == sandbox.StatusRunning
	_, started, err := attachNetwork(ctx, spec, running, env.Stderr)
	return started, err
}

// withNetworkAnnotations merges the network's annotations into whatever the user asked for.
//
// An explicit -annotation wins, the same way it wins over the computed resource
// annotations: the flag exists to try a runtime capability Boks has no first-class flag
// for, and that is worth nothing if Boks overrides it.
func withNetworkAnnotations(fromNetwork, user map[string]string) map[string]string {
	merged := make(map[string]string, len(fromNetwork)+len(user))
	maps.Copy(merged, fromNetwork)
	maps.Copy(merged, user)
	return merged
}

// attachNetwork brings up a sandbox's network stack, and reports whether this invocation is
// the one that started it.
//
// It must be called before the sandbox's task starts: the VM connects to the link socket
// while it boots, so a socket that appears afterwards is a boot failure rather than a retry.
func attachNetwork(ctx context.Context, spec enforce.Spec, running bool, stderr io.Writer) (enforce.State, bool, error) {
	if state, alive := enforce.Lookup(spec.StateDir, spec.Sandbox); alive {
		// Reuse, and say nothing: this is the ordinary case of a second command
		// attaching to a sandbox that is already up. The rules it is running under are
		// the ones it was started with, which `boks policy log` and `boks net ls` can
		// both be asked about.
		return state, false, nil
	}
	if running {
		fmt.Fprint(stderr, orphanedStackWarning(spec.Sandbox))
	}
	state, err := enforce.Ensure(ctx, spec, stderr)
	if err != nil {
		return enforce.State{}, false, err
	}
	return state, true, nil
}

// orphanedStackWarning covers a sandbox that is running while the process serving its
// network is gone — a crashed or killed supervisor.
//
// This used to say that whether a running guest re-attaches to a new link socket was
// "unverified", and to hope out loud. It was measured on 2026-08-12: it does not. The VMM
// connects to the link socket once, while the VM boots, and a socket bound at the same path
// afterwards is a different socket that nothing in the guest will ever speak to. Saying
// "unverified" was optimistic in the direction that costs the user an afternoon: they read
// it as "probably fine", and then debug an agent whose network is simply gone.
//
// So this states the outcome and gives the command that fixes it. Boks does not do the
// restart itself, and that is a deliberate choice rather than a missing feature: the sandbox
// is *running*, and restarting it kills whatever is in it — a build, a test run, an agent
// half way through a task — to repair something the user may not even need on this
// invocation. A fresh stack is still started, because it costs nothing and is what the
// sandbox will attach to when it next boots.
//
// It is a WARNING rather than a note because the sandbox is, right now, not doing the thing
// it appears to be doing.
func orphanedStackWarning(name string) string {
	return fmt.Sprintf(
		"WARNING: sandbox %q is running, but the process serving its network is gone.\n"+
			"         A running guest does NOT re-attach to a new link socket — measured on\n"+
			"         2026-08-12 — so this sandbox has no network until it is restarted, and\n"+
			"         nothing inside it can reach anything. A fresh stack is being started for\n"+
			"         the next boot; it will not help the VM that is up now.\n"+
			"         Restart it to get the network back:\n"+
			"           boks stop %s && boks start %s\n"+
			"         That kills whatever is running inside, which is why boks does not do it\n"+
			"         for you.\n\n", name, name, name)
}

// enforceSpec turns the policy flags into the specification the network stack runs from.
//
// Credential *values* are resolved here, in the foreground process that has the passphrase,
// and handed to the stack on a pipe. The stack therefore never learns the passphrase and
// can attach only the credentials this sandbox was configured with.
// The record is what the sandbox remembers about its own policy, or nil for a sandbox that
// does not exist yet. Passing it here rather than reading it inside is what makes `start`,
// `exec` and `run` produce the same policy for the same sandbox.
func (f *policyFlags) enforceSpec(ctx context.Context, name, address string, mode network.Mode,
	record *policy.SandboxPolicy) (enforce.Spec, error) {

	plan, err := f.planFor(name, mode)
	if err != nil {
		return enforce.Spec{}, err
	}
	resolution, err := f.resolution(name, record)
	if err != nil {
		return enforce.Spec{}, err
	}
	credentials, err := f.credentialRules()
	if err != nil {
		return enforce.Spec{}, err
	}

	secrets := map[string]string{}
	if len(credentials) > 0 {
		store, err := openSecretStore("")
		if err != nil {
			return enforce.Spec{}, err
		}
		for _, c := range credentials {
			value, err := store.Lookup(ctx, c.Service)
			if err != nil {
				return enforce.Spec{}, fmt.Errorf("credential %q: %w\nStore it first: boks secret set %s", c.Service, err, c.Service)
			}
			secrets[c.Service] = value.Reveal()
		}
	}

	return enforce.Spec{
		Sandbox:          name,
		Plan:             plan,
		Resolution:       &resolution,
		Inject:           f.inject,
		GuestCredentials: f.guest,
		Secrets:          secrets,
		Intercept:        true,
		CADir:            caDir(""),
		StateDir:         policy.StateDir(),
		LogPath:          policy.DefaultLogPath(),
		Address:          address,
	}, nil
}

// describeNetwork prints what the sandbox's network will do, before anything runs in it.
//
// A user is entitled to know which destinations are permitted and which of their flows will
// be decrypted *at the moment they ask for it*, not from a certificate error later. What
// decides how much is said is not whether they passed a flag but whether they have been told
// before: see internal/cli/notice.go for the rule and why it is that one. The short version
// is that the first encounter with anything consequential is loud, an unchanged policy is two
// lines, and an interception host that has never been announced is loud whatever else is
// true — including under --quiet.
func describeNetwork(f *policyFlags, spec enforce.Spec, mode network.Mode, quiet bool, stderr io.Writer) error {
	shown := loadNotices(policy.StateDir(), spec.Sandbox)

	if mode == network.ModeNone {
		// Nothing leaves this sandbox, so there is no policy to show and no traffic to
		// decrypt. The mode is fixed when a sandbox is created, so this is the same
		// statement on every run: worth making once in full.
		if quiet {
			return nil
		}
		if shown.Policy == digest(noNetworkNotice) {
			fmt.Fprintf(stderr, "network: none — nothing leaves sandbox %s.\n", spec.Sandbox)
			return nil
		}
		fmt.Fprint(stderr, noNetworkNotice)
		if len(f.allow) > 0 || len(f.deny) > 0 || len(f.inject) > 0 || f.preset != "" {
			fmt.Fprint(stderr, "         The policy flags are not applied: nothing leaves this sandbox to judge.\n")
		}
		fmt.Fprintln(stderr)
		shown.Policy = digest(noNetworkNotice)
		shown.save(policy.StateDir(), spec.Sandbox)
		return nil
	}

	pol, err := spec.Policy()
	if err != nil {
		return err
	}
	credentials, err := spec.Credentials()
	if err != nil {
		return err
	}

	table := pol.Describe()
	if spec.Resolution != nil {
		table = spec.Resolution.Describe()
	}
	hosts := secret.CredentialHosts(credentials)
	fresh := shown.newHosts(hosts)

	// A host whose traffic boks is about to decrypt, and that this sandbox has not been
	// told about, is announced before anything else and regardless of --quiet. Asking for
	// less output is not consent to silent interception.
	if len(fresh) > 0 {
		if len(shown.Intercept) > 0 {
			fmt.Fprintf(stderr, "NEW: interception now covers %s.\n\n", strings.Join(fresh, ", "))
		}
		fmt.Fprint(stderr, interceptionNotice(credentials))
		fmt.Fprintln(stderr)
	}

	switch {
	case quiet:
		// Everything else here describes a state the user can ask for at any time
		// with `boks policy ls`. The one thing that is not — a host about to be
		// decrypted for the first time — has already been printed above.
		//
		// Nothing is recorded as shown, because nothing was: a quiet run must not
		// consume the one loud explanation a sandbox gets.
	case digest(table) != shown.Policy:
		fmt.Fprint(stderr, table)
		if !shown.Enforcement {
			fmt.Fprintf(stderr, "\n%s\n", enforcementNote)
			shown.Enforcement = true
		} else {
			fmt.Fprint(stderr, "\n")
		}
		shown.Policy = digest(table)
	default:
		fmt.Fprint(stderr, steadyStateLine(pol, mode, spec.Sandbox, hosts))
	}

	// The announced hosts are recorded whether or not this run was quiet, because they
	// were announced whether or not it was.
	shown.withHosts(hosts).save(policy.StateDir(), spec.Sandbox)
	return nil
}

// steadyStateLine is what a run prints when it has nothing new to say: what the sandbox is
// running under, and where to look for the detail rather than the detail itself.
//
// It is two lines, and it still names the interception state, because "boks is decrypting
// two of your flows" is not a thing to mention once and then leave out. The rules and the
// decisions are one command away, and naming those commands is worth more than reprinting
// what they would say.
func steadyStateLine(pol policy.Policy, mode network.Mode, sandbox string, hosts []string) string {
	allows, denies := 0, 0
	for _, r := range pol.Rules {
		if r.Action == policy.Deny {
			denies++
			continue
		}
		allows++
	}
	tls := "no TLS interception"
	if len(hosts) > 0 {
		tls = fmt.Sprintf("TLS decrypted for %d host(s): %s", len(hosts), strings.Join(hosts, ", "))
	}
	return fmt.Sprintf("network: %s · policy %s · %d allow, %d deny · %s\n"+
		"         unchanged since this sandbox last ran — 'boks policy ls --sandbox %s' for the\n"+
		"         rules, 'boks policy log --sandbox %s' for what they decided.\n",
		mode, pol.Name, allows, denies, tls, sandbox, sandbox)
}

// stopNetworkQuietly and forgetNetworkQuietly are the cleanup paths. Failures are reported
// rather than returned: they run while a command is already failing or exiting, and the
// original outcome is the more useful one to end up as the error.
func stopNetworkQuietly(name string, stderr io.Writer) {
	if err := enforce.Stop(policy.StateDir(), name); err != nil {
		fmt.Fprintf(stderr, "warning: %v\n", err)
	}
}

func forgetNetworkQuietly(name string, stderr io.Writer) {
	if err := enforce.Forget(policy.StateDir(), name); err != nil {
		fmt.Fprintf(stderr, "warning: %v\n", err)
	}
}
