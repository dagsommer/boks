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
			return enforce.Serve(cmd.Context(), spec, env.Stdout, func(ctx context.Context, started func()) error {
				// The stack's life is the VM's life. Watching the task is what
				// makes that true without a second supervision mechanism:
				// containerd already knows. The same watch reports the moment
				// the task starts, which is when a VM that is going to attach
				// to the link socket has to do so.
				return sandbox.WatchTask(ctx, spec.Address, spec.Sandbox,
					enforce.TaskAppearTimeout, enforce.TaskPollInterval, started)
			})
		},
	}
}

// networkFor decides which network a sandbox gets: the mode this invocation asked for when
// the sandbox is about to be created, and the one it was wired for when it already exists.
//
// The mode is fixed when a sandbox is created, because it lives in annotations the runtime
// reads at boot. Silently applying `-net none` to a sandbox created with a network would be
// the worst kind of wrong: the flag would appear to be obeyed while the container stayed
// connected. That disagreement no longer reaches this function — checkFixedAtCreation refuses
// it, along with every other flag a sandbox fixes at creation, before anything is pulled or
// started. `-net` was the first of those to be made a refusal and this is where the refusal
// used to live; it moved so that a user who passes `-net none --cpus 2` to an existing sandbox
// is told about both at once instead of discovering them one command at a time.
//
// What is left here is the part that is not a refusal: honouring the wiring for a run that
// asked for nothing, and saying so — loudly — for a sandbox Boks never wired at all.
func networkFor(inv invocation, requested network.Mode, stderr io.Writer) (mode network.Mode, wired bool) {
	if !inv.exists {
		return requested, true
	}
	existing, wired := network.ModeFromAnnotations(inv.info.Annotations)
	if !wired {
		fmt.Fprintf(stderr, "%s", unwiredSandboxWarning(inv.name))
		return existing, false
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

	mode, wired := networkFor(inv, requested, env.Stderr)
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

	publish := publishFor(flags, inv, env.Stderr)
	spec, err := flags.enforceSpec(ctx, inv.name, cfg.Address, mode, record, publish, env.Stderr)
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
		cfg.Ports = publish
	}
	cfg.Annotations = withNetworkAnnotations(guest.Annotations, cfg.Annotations)
	cfg.Env = append(cfg.Env, guest.Env...)
	cfg.Mounts = guest.Mounts

	running := inv.exists && inv.info.Status == sandbox.StatusRunning
	_, started, err := attachNetwork(ctx, spec, running, env.Stderr)
	return started, err
}

// publishFor decides which ports this run publishes, and says so when the answer is not what
// the flags asked for.
//
// `-p` is **ignored when re-attaching**, which is sbx's documented behaviour and worth
// keeping rather than improving on. A sandbox's published ports are live state of a running
// network, not a property of the invocation that happened to reach it: two terminals running
// `boks run` against one sandbox would otherwise fight over which set of ports it has, and
// the second one would silently win. `boks ports` is the way to change a sandbox that exists,
// and this says so instead of leaving the flag looking obeyed.
//
// A sandbox that exists gets the specifications it was created with, so that `boks run` on a
// stopped sandbox brings its ports back up.
func publishFor(flags *policyFlags, inv invocation, stderr io.Writer) []string {
	if !inv.exists {
		return flags.publish
	}
	if len(flags.publish) > 0 {
		fmt.Fprintf(stderr,
			"note: --publish is ignored when re-attaching to an existing sandbox. Sandbox %q keeps\n"+
				"      the ports it has; change them on the running sandbox instead:\n"+
				"        boks ports %s --publish %s\n",
			inv.name, inv.name, flags.publish[0])
	}
	return inv.info.Ports
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
	// Said before the sandbox is created, on a platform where the link has never carried a
	// frame, and said regardless of --quiet: asking for less output is not consent to a
	// network that may be enforcing nothing.
	//
	// No platform is in that state today — Windows was, and stopped being on 2026-08-14
	// (see network.Unexercised) — so this prints nothing on any host Boks currently runs
	// on. It is the gate, not the claim, and the claim is made in one place.
	if unexercised := network.Unexercised(); warnUnexercisedNetwork(unexercised, spec.Plan.Mode) {
		fmt.Fprint(stderr, unexercisedNetworkWarning(unexercised))
	}
	state, err := enforce.Ensure(ctx, spec, stderr)
	if err != nil {
		return enforce.State{}, false, err
	}
	return state, true, nil
}

// warnUnexercisedNetwork answers whether this run should warn that the sandbox network has
// never been shown to work. Two things have to be true, and the second one is the point.
//
// The platform must be one where nothing has been seen putting a frame on the link, which is
// what unexercised reports. But there must also be a link the guest could dial, and under
// -net none there is not. The VM is still given a NIC — that is exactly what turns the
// runtime's own transport off, and with it the guest's route to host loopback — but the host
// end of it is a blackhole that accepts the connection and discards everything: no stack, no
// proxy, no listener, no policy. Every claim the warning makes is then false. It says boks is
// "attempting a sandbox network" when boks is attempting the opposite; it tells the reader to
// check `boks policy log` for a decision, when nothing in this mode ever reaches a decision;
// and it says a guest that comes up anyway "is not contained", when -net none is the most
// contained a sandbox gets and the only mode whose containment does not rest on the
// unexercised code. Warning here would teach the user to distrust the one thing that works.
//
// It takes the platform answer as an argument, rather than reading it, so a test can construct
// the Windows case on a machine that is not Windows.
func warnUnexercisedNetwork(unexercised error, mode network.Mode) bool {
	return unexercised != nil && mode != network.ModeNone
}

// unexercisedNetworkWarning is printed before a sandbox is created on a platform where
// nothing has ever been seen putting a guest's frames on the link socket. No platform is in
// that state today; Windows was until 2026-08-14, when a guest attached to Boks' own link
// socket there and the policy engine judged real traffic across it.
//
// It is a WARNING rather than a note, and it is not suppressed by --quiet, for the same reason
// the interception notice is not: the thing being announced is a way in which the sandbox may
// not be doing what the user believes it is doing. The failure it describes is silent by
// nature. A shim that does not carry the external network provider ignores the annotations and
// leaves the guest on libkrun's TSI, where its 127.0.0.1 is the *host's* and no policy is in
// the path at all — and from outside, that sandbox looks like one that works.
//
// It takes the reason as an argument rather than reading it, so that the Windows text can be
// rendered and read by a test on a machine that is not Windows.
func unexercisedNetworkWarning(reason error) string {
	return fmt.Sprintf(
		"WARNING: boks is attempting a sandbox network on a platform where it has never been\n"+
			"         shown to work. This is an attempt, not a claim.\n"+
			"         %v\n"+
			"         If nothing connects to the link socket shortly after the task starts, the\n"+
			"         network supervisor exits and says so in the sandbox's stack.log. A guest that\n"+
			"         comes up anyway is not contained: check `boks policy log` for a decision from\n"+
			"         this sandbox before trusting it with anything.\n\n", reason)
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
			"         A running guest does NOT re-attach to a new link socket, so this sandbox\n"+
			"         has no network until it is restarted, and nothing inside it can reach\n"+
			"         anything. A fresh stack is being started for the next boot; it will not\n"+
			"         help the VM that is up now.\n"+
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
//
// The credential set is the flags plus whatever the store already holds under a service
// name — see credentialPlan for why a stored credential applies without being named again,
// and for the two things that keeps it from being a quiet expansion.
func (f *policyFlags) enforceSpec(ctx context.Context, name, address string, mode network.Mode,
	record *policy.SandboxPolicy, publish []string, stderr io.Writer) (enforce.Spec, error) {

	plan, err := f.planFor(name, mode)
	if err != nil {
		return enforce.Spec{}, err
	}
	resolution, err := f.resolution(name, record)
	if err != nil {
		return enforce.Spec{}, err
	}
	credentials, oauth, err := f.resolveCredentials(ctx, stderr)
	if err != nil {
		return enforce.Spec{}, err
	}
	services, err := credentials.services()
	if err != nil {
		return enforce.Spec{}, err
	}
	credentials.describe(stderr)

	secrets := map[string]string{}
	if len(services) > 0 {
		store, err := openSecretStore("")
		if err != nil {
			return enforce.Spec{}, err
		}
		for _, service := range services {
			value, err := store.Lookup(ctx, service)
			if err != nil {
				return enforce.Spec{}, fmt.Errorf("credential %q: %w\nStore it first: boks secret set %s", service, err, service)
			}
			secrets[service] = value.Reveal()
		}
	}

	return enforce.Spec{
		Sandbox:          name,
		Plan:             plan,
		Resolution:       &resolution,
		Inject:           credentials.inject,
		GuestCredentials: credentials.guest,
		Secrets:          secrets,
		OAuth:            oauth,
		Publish:          publish,
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
		fmt.Fprint(stderr, steadyStateLine(pol, mode, spec.Sandbox, f.agent.Name, hosts))
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
func steadyStateLine(pol policy.Policy, mode network.Mode, sandbox, agentName string, hosts []string) string {
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
	// The `policy ls` invocation has to include the agent, or it would print a policy
	// without the agent's own layer — a different policy from the one this sandbox is
	// about to run under, offered as the way to inspect it. `policy ls` contacts nothing,
	// containerd included, so it cannot look the agent up for itself.
	ls := "boks policy ls --sandbox " + sandbox
	if agentName != "" {
		ls += " --agent " + agentName
	}
	return fmt.Sprintf("network: %s · policy %s · %d allow, %d deny · %s\n"+
		"         unchanged since this sandbox last ran. The rules: %s\n"+
		"         What they decided: boks policy log --sandbox %s\n",
		mode, pol.Name, allows, denies, tls, ls, sandbox)
}

// releaseStack ends the network stack this invocation started, unless the sandbox it serves is
// still running.
//
// The rule it enforces is the one at the top of internal/enforce/supervisor.go: **the stack's
// lifetime is the VM's lifetime, and no CLI invocation has that lifetime.** What used to stand
// here did not enforce it. The condition was `ephemeral || err != nil`, and err is this
// command's error — which for `boks run` is the *guest command's exit code*, carried out as an
// ExitError so that boks exits with what ran inside the sandbox. So a command that exited
// non-zero inside a perfectly healthy sandbox read as a failed run, and the stack was torn down
// underneath a VM that was still up. Measured on Windows on 2026-08-16: `boks run shell . --
// cmd /c ver` exited 127, the sandbox stayed `running`, and the process serving its network was
// gone. Boks then detected the orphan correctly on the next command and told the user their
// only remedy was stop/start — accurate, and describing damage boks had done itself.
//
// The same condition took the network away from a sandbox whose run was interrupted with
// Ctrl-C, which is the exact scenario internal/enforce says the supervisor exists to prevent.
//
// So the question is not "did this command succeed" but "is anything still attached to this
// stack". A sandbox that is running keeps its stack; nothing else does. --rm is decided
// without asking, because an ephemeral sandbox is removed by the run that created it.
//
// running is a function rather than a bool so that the two answers can be constructed on a
// host with no hypervisor, where the sandbox this reasons about cannot exist.
func releaseStack(name string, ephemeral bool, running func() bool, stderr io.Writer) {
	if !ephemeral && running() {
		return
	}
	stopNetworkQuietly(name, stderr)
}

// sandboxIsRunning answers releaseStack's question by asking containerd, and answers "yes" when
// it cannot tell.
//
// The two mistakes are not symmetric, and the supervisor's own design is what makes the bias
// safe. Keeping a stack that nothing needs costs one idle process, and not for long: the
// supervisor watches the sandbox's task itself and exits within a poll interval of it stopping,
// or after TaskAppearTimeout if no task ever appears — and if containerd is what is unreachable,
// its watch fails and it exits at once. Stopping a stack that is still needed costs a running
// guest its network until somebody restarts it. So an unreadable answer keeps the stack.
//
// The context is detached from the command's, because the most likely reason a run ended with a
// cancelled context is Ctrl-C — precisely the case where this question must still be answerable.
func sandboxIsRunning(ctx context.Context, address, name string, stderr io.Writer) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sandboxStatusTimeout)
	defer cancel()
	running, err := sandbox.Running(ctx, address, name)
	if err != nil {
		fmt.Fprintf(stderr, "note: could not tell whether sandbox %q is still running (%v),\n"+
			"      so its network stack is being left up rather than taken from a live guest.\n", name, err)
		return true
	}
	return running
}

// sandboxStatusTimeout bounds that question. It is a local gRPC call on a socket this command
// has already been talking to, so the only thing this covers is a containerd that has stopped
// answering — and the answer to that is above.
const sandboxStatusTimeout = 5 * time.Second

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
