package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	"github.com/dagsommer/boks/internal/agent"
	"github.com/dagsommer/boks/internal/kit"
	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

// policyFlags are the network-policy flags shared by `boks run`, `boks proxy` and
// `boks policy ls`, so that all three resolve a policy the same way and a rule that is
// valid for one is valid for the others.
type policyFlags struct {
	preset  string
	profile string
	mode    string
	allow   []string
	deny    []string
	inject  []string
	guest   []string
	publish []string
	oauth   []string
	// noSecrets leaves the credential store out of this run entirely. A credential
	// stored under a service's name applies without being named again — see
	// credentialPlan — and this is how a sandbox is run without one.
	noSecrets bool
	// agent is the agent the sandbox runs. It is not a flag on `run` — the agent is a
	// positional there — but it decides a layer of the policy, so it travels with the
	// rest of what decides one. `boks policy ls` and `boks policy check` set it from
	// their own --agent flag; `run` and `create` from the resolved invocation; `start`
	// and `exec` from what the sandbox recorded when it was created.
	agent agent.Agent
	// kitRef is the --kit reference, and kitSpec is what it loaded. The reference is kept
	// so that an error can name what the user typed rather than the kit's internal name.
	kitRef  string
	kitSpec *kit.Spec
}

// forAgent labels the flags with the agent whose allowlist applies.
func (f *policyFlags) forAgent(a agent.Agent) *policyFlags {
	f.agent = a
	return f
}

// register adds the flags to a flag set. The preset default is empty rather than
// "standard" so that a caller can tell "the user asked for the default" apart from "the
// user said nothing", which matters while the policy is not enforced.
//
// Every repeatable flag is a StringArray rather than a StringSlice: a StringSlice splits its
// value on commas, and an -inject rule naming two hosts contains one. Splitting there would
// turn a valid rule into two invalid ones.
func (f *policyFlags) register(fs *pflag.FlagSet) {
	fs.StringVar(&f.preset, "policy", "", "network policy preset: "+strings.Join(policy.PresetNames(), ", ")+
		" (default "+policy.DefaultPreset+")")
	fs.StringVar(&f.profile, "profile", "", "stored policy profile to apply ('boks policy profile ls')")
	// The help says what it DOES, not what a kit is, because a kit declares an image, an
	// entrypoint, setup commands and credentials too and none of those are applied yet. A
	// flag advertised as "apply this kit" would be read as all of it.
	fs.StringVar(&f.kitRef, "kit", "", "apply a kit's network rules: a directory containing "+
		kit.SpecFileName+", or a path to one (local only)")
	fs.StringVar(&f.mode, "net", "", "network mode: none (no network at all) or nat (default "+
		string(network.DefaultMode)+")")
	fs.StringArrayVar(&f.allow, "allow", nil, "allow a destination, host[:ports] (repeatable)")
	fs.StringArrayVar(&f.deny, "deny", nil, "deny a destination, host[:ports] (repeatable); deny always wins")
	fs.StringArrayVar(&f.inject, "inject", nil,
		"attach a credential: service@host[,host]=bearer|basic[:user]|header[:format] (repeatable)")
	fs.StringArrayVar(&f.guest, "guest-credential", nil,
		"what the guest holds instead: service=[ENV_NAME=]placeholder (repeatable)")
	fs.StringArrayVar(&f.oauth, "oauth", nil,
		"name a stored OAuth credential; stored ones apply anyway, this pins one (repeatable)")
	fs.BoolVar(&f.noSecrets, "no-secrets", false,
		"do not attach credentials from the store; only what --inject names")
}

// registerPublish adds `-p/--publish`, separately from the rest.
//
// It is separate because the rest of these flags are shared with `boks proxy` and
// `boks policy ls`, which describe a policy and have no sandbox to publish a port into. A
// flag that appears in a command's help and does nothing is worse than a missing one.
func (f *policyFlags) registerPublish(fs *pflag.FlagSet) {
	fs.StringArrayVarP(&f.publish, "publish", "p", nil,
		"publish a sandbox port on the host, bound to loopback (repeatable): "+
			"[[HOST_IP:]HOST_PORT:]SANDBOX_PORT[/PROTOCOL]")
}

// checkPublish rejects a malformed --publish before anything is created or pulled.
func (f *policyFlags) checkPublish() error { return checkPublishSpecs(f.publish) }

// specified reports whether the user set any of them.
func (f *policyFlags) specified() bool {
	return f.policySpecified() || f.mode != "" || len(f.inject) > 0 || len(f.guest) > 0 || len(f.oauth) > 0
}

// policySpecified reports whether this run asked for a policy of its own, as opposed to the
// one its sandbox and the store already decide.
func (f *policyFlags) policySpecified() bool {
	return f.preset != "" || f.profile != "" || len(f.allow) > 0 || len(f.deny) > 0
}

// sandboxNameFor supplies a placeholder when the real name has not been generated yet, so
// that flag validation — which only needs the name's length, for the socket path — can run
// before the sandbox exists.
func sandboxNameFor(name string) string {
	if name == "" {
		return "boks-0123456789"
	}
	return name
}

// networkPlan computes the network a sandbox would get. It is separate from resolve()
// because the two answer different questions: resolve() says which destinations are
// permitted, networkPlan says what kind of network exists to permit them on.
func (f *policyFlags) networkPlan(sandbox string) (network.Plan, error) {
	mode, err := network.ParseMode(f.mode)
	if err != nil {
		return network.Plan{}, err
	}
	return f.planFor(sandbox, mode)
}

// planFor computes the plan for a mode that has already been decided — by the flags, or by
// what an existing sandbox was created with.
//
// The runtime directory is the one the network supervisor looks in, and that is not a
// coincidence to be maintained by hand: a plan whose socket lands somewhere else would
// produce a VM connected to a socket nobody is holding.
func (f *policyFlags) planFor(sandbox string, mode network.Mode) (network.Plan, error) {
	return network.NewPlan(network.Config{
		Mode:       mode,
		Sandbox:    sandbox,
		RuntimeDir: filepath.Join(policy.StateDir(), "net"),
	})
}

// resolve builds the effective policy for no particular sandbox. It is what `boks proxy`
// runs under and what `boks run` uses to reject a malformed rule before it creates anything.
func (f *policyFlags) resolve() (policy.Policy, error) {
	res, err := f.resolution("", nil)
	if err != nil {
		return policy.Policy{}, err
	}
	return res.Policy()
}

// resolution assembles a sandbox's policy from the three things that decide it: the durable
// store, what the sandbox recorded when it was created, and this run's flags.
//
// The merge rule between the record and the flags is deliberately asymmetric, and it is the
// same principle the scopes follow — nothing may quietly widen:
//
//   - the preset and the profile are *replaced* by a flag that names one, because choosing a
//     posture for one run is what those flags are for;
//   - `--allow` *replaces* the sandbox's recorded allow list, so a one-off run can narrow;
//   - `--deny` is *added to* the recorded denies. A prohibition a sandbox was created with
//     does not disappear because this invocation typed a different one.
//
// A run that names no policy flags at all gets exactly what the sandbox was created with,
// which is the whole point: `boks start` and `boks exec` no longer serve a sandbox the
// default preset in place of its own policy.
func (f *policyFlags) resolution(sandbox string, record *policy.SandboxPolicy) (policy.Resolution, error) {
	store, err := policy.LoadStore(policy.DefaultStorePath())
	if err != nil {
		return policy.Resolution{}, err
	}
	req := record.Request(store, sandbox)
	// The agent's own allowlist is re-derived from the registry rather than read back
	// from the sandbox, so that it is always this build's definition of what that agent
	// needs — including when an entry is removed.
	req.Agent, req.AgentAllow = f.agent.Name, f.agent.AllowRules()
	// A kit's network rules enter as their own layer, labelled with the kit's name so that
	// `boks policy ls` can point at the file a destination came from. They are added, never
	// subtracted: a deny in any scope still beats a kit's allow, which is the engine's
	// invariant rather than this function's — see internal/policy.
	if f.kitSpec != nil {
		allow, deny := kit.NetworkRules(f.kitSpec)
		req.Kit = f.kitSpec.Name
		req.KitAllow = kitRules(policy.Allow, allow, f.kitSpec.Name)
		req.KitDeny = kitRules(policy.Deny, deny, f.kitSpec.Name)
	}
	if f.preset != "" {
		req.Preset = f.preset
	}
	if f.profile != "" {
		req.Profile = f.profile
	}
	if len(f.allow) > 0 {
		req.Allow = f.allow
	}
	req.Deny = unionDenies(req.Deny, f.deny)
	return req.Resolve()
}

// kitRules turns a kit's destination list into policy rules, labelled so that a reader of
// `boks policy ls` can tell where each came from.
//
// The note names the kit rather than describing the rule, because that is the question a
// destination in this layer raises: an agent's allowlist is in the Boks binary and auditable by
// reading the release, while a kit is a file on the user's disk that Boks was pointed at.
func kitRules(action policy.Action, specs []string, name string) []policy.RuleSpec {
	if len(specs) == 0 {
		return nil
	}
	rules := make([]policy.RuleSpec, 0, len(specs))
	for _, spec := range specs {
		rules = append(rules, policy.RuleSpec{
			Action: action,
			Spec:   spec,
			Note:   "declared by kit " + name,
		})
	}
	return rules
}

// loadKit reads the --kit reference, if one was given, and reports what the kit warned about.
//
// Called before anything is created, so a kit that does not parse costs nothing: the failure a
// user gets is "this file is wrong", not a half-built sandbox holding rules from a spec that
// turned out to be invalid.
func (f *policyFlags) loadKit(stderr io.Writer) error {
	if f.kitRef == "" {
		return nil
	}
	spec, warnings, err := kit.Load(f.kitRef)
	if err != nil {
		return err
	}
	if err := kit.Validate(spec); err != nil {
		return fmt.Errorf("kit %s: %w", f.kitRef, err)
	}
	// Warnings are the loader's way of saying a field parsed and will not be honoured —
	// a v1 field accepted for compatibility, or a composition key whose runtime support is
	// pending upstream. Silence there would be a promise Boks cannot keep.
	for _, w := range warnings {
		fmt.Fprintf(stderr, "kit %s: %s\n", f.kitRef, w)
	}
	f.kitSpec = spec
	return nil
}

// unionDenies merges a sandbox's recorded denies with this run's, keeping each destination
// once.
//
// It is a union rather than a replacement because a prohibition must not disappear because
// this invocation typed a different one. It is deduplicated because a `boks create --deny x`
// supplies the same rule twice — once as the flag and once as the record built from it — and
// a policy that lists the same deny twice is a policy a user cannot check against what they
// wrote.
func unionDenies(recorded, flags []string) []string {
	out := append([]string(nil), recorded...)
	for _, spec := range flags {
		probe := policy.RuleSpec{Action: policy.Deny, Spec: spec}
		duplicate := false
		for _, existing := range out {
			if probe.SameDestination(existing) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, spec)
		}
	}
	return out
}

// sandboxRecord is what this invocation's flags say a new sandbox should remember. It holds
// the selection only — presets, profiles and destinations — and never a credential: a
// credential rule names a service, and its value lives in the encrypted store.
func (f *policyFlags) sandboxRecord() *policy.SandboxPolicy {
	if !f.policySpecified() {
		return nil
	}
	return &policy.SandboxPolicy{
		V:       policy.SandboxPolicyVersion,
		Profile: f.profile,
		Preset:  f.preset,
		Allow:   append([]string(nil), f.allow...),
		Deny:    append([]string(nil), f.deny...),
	}
}

// credentialRules assembles the credentials from the flags. Values are not touched here: a
// credential names a secret, and the value is fetched from the provider at request time.
//
// The assembly lives in internal/secret because the process that runs the proxy builds the
// same credentials from the same strings, and the two must not be able to disagree about
// what the user asked for.
func (f *policyFlags) credentialRules() ([]secret.Credential, error) {
	return secret.ParseCredentials(f.inject, f.guest)
}

// enforcementNote replaces the warning that used to say a policy was not applied to a
// running sandbox. It now is — so what a user needs from this text has changed, from "do
// not trust this" to "here is precisely how far the trust goes".
//
// The distinction it draws is the one that matters: the network stack is the control, and
// it holds whether or not the guest cooperates; the proxy variables are how a cooperating
// guest gets hostname rules, credentials and a readable refusal instead of a socket that
// fails.
//
// It used to close with the transcript that established all of this — the date, the host, the
// hypervisor, a pointer to docs/verification.md. That belongs in the evidence record and not
// in front of a user, who can act on none of it: they need to know what boks does to their
// traffic, not which machine proved it. The claim's provenance is docs/verification.md's job.
// What survives is the pointer to docs/security-model.md, because "here is what this boundary
// does not cover" is something a reader decides with.
const enforcementNote = `The sandbox's network is terminated on the host: the guest's NIC ends in a network
stack in a boks process, which judges every TCP connection the guest opens against
these rules — from the address and port in the packet — before it dials anything.
A denied destination is refused there, whether or not the guest cooperated, and both
outcomes appear in 'boks policy log' as transparent. UDP and ICMP are dropped, apart
from DNS to the sandbox's own resolver.

HTTP_PROXY and HTTPS_PROXY point the guest at the filtering proxy inside its own
virtual network. Ignoring them costs the guest hostname rules, credential injection
and readable refusals, and gains it nothing: a raw socket is still judged, just on
addresses rather than names. A policy written only in hostnames therefore denies raw
flows rather than permitting the addresses those names resolve to.

See docs/security-model.md for the limits of this boundary and what is still open.`

// noNetworkNotice is what -net none says for itself. It is the strongest containment Boks
// offers and the only one whose enforcement does not depend on code that has yet to meet a
// real guest, so it is worth stating plainly rather than passing over in silence.
const noNetworkNotice = `NETWORK: none. The VM gets a NIC — which is what turns the runtime's own transport
         off, and with it the guest's access to the host's loopback — and the container
         is never wired to it. No stack, no proxy, no listener, nothing to reach.
`

// interceptionNotice says, in as many words, which traffic Boks will decrypt.
//
// It is printed wherever credential rules are accepted, because a user who configures an
// injection has bought TLS interception for those hosts whether or not they knew it, and
// finding that out from a certificate error later is not acceptable. Nothing here is
// conditional on a verbosity flag.
func interceptionNotice(credentials []secret.Credential) string {
	hosts := secret.CredentialHosts(credentials)
	if len(hosts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("TLS INTERCEPTION: boks will DECRYPT traffic to these hosts:\n")
	for _, h := range hosts {
		b.WriteString("  " + h + "\n")
	}
	b.WriteString(`
Attaching a credential to an HTTPS request means reading the request, which means
terminating TLS. For these hosts — and only these — boks presents its own certificate,
verifies the origin's certificate itself, and can read requests and responses. It keeps
none of it: no body, header value or URL is written to any log.

Every other destination is tunnelled untouched, with the origin's own certificate chain
intact. 'boks policy log' marks which was which: forward means boks read the flow,
forward-bypass means it only carried it.

A guest must trust the boks CA for these hosts to work at all:
  boks ca show
  boks ca export -o boks-ca.pem      # install it in the guest, never on your host
  boks ca env                        # for runtimes that ignore the system trust store
`)
	return b.String()
}

// proxyCaveat is printed by 'boks proxy', which runs the filtering proxy on the host with no
// sandbox behind it.
//
// It still says what it always said, because for *this* command it is still true: there is
// no network stack under a standalone proxy, so a client that declines to use it is not
// filtered by anything. Inside a sandbox the picture is different — the stack judges the
// flow whether the client cooperates or not — and that difference is exactly why this note
// is worded for the command it is printed by rather than for proxies in general.
const proxyCaveat = `NOTE: a forward proxy filters only the traffic a client sends to it.
      A client that ignores HTTP_PROXY and opens a raw socket is not affected.
      Run standalone like this, it is a cooperating-client mechanism and not an
      enforcement boundary. Inside a sandbox ('boks run') it is layered on one:
      there, the sandbox's network stack judges every connection by address
      before it is dialled, cooperating client or not.
`

// hostnameRuleCaveat warns when a policy would permit destinations only by name.
//
// A raw socket carries no hostname, so the netstack judges those flows on the address alone
// and a name-only allow rule can never match one. The effect surprised a careful tester:
// `-allow example.com` denied a direct-by-IP connection *to example.com*, which is correct
// and fail-closed, but reads like a bug unless you already know how the two layers differ.
// Docker Sandboxes has the same property and documents it; this says it at the moment the
// rules are shown, which is where someone writing them is looking.
func hostnameRuleCaveat(p policy.Policy) string {
	names, addresses := 0, 0
	for _, r := range p.Rules {
		if r.Action != policy.Allow {
			continue
		}
		if r.Host.MatchesNameOnly() {
			names++
		} else {
			addresses++
		}
	}
	if names == 0 || addresses > 0 {
		return ""
	}
	return "\n  Every allow rule here names a host. Traffic through the proxy is matched by\n" +
		"  name, but a connection made straight to an address carries none — so a guest\n" +
		"  that dials an IP is refused even when that IP belongs to an allowed host.\n" +
		"  Add an address or CIDR rule if a sandbox needs to reach one directly.\n\n"
}
