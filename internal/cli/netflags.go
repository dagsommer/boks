package cli

import (
	"flag"
	"path/filepath"
	"strings"

	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

// policyFlags are the network-policy flags shared by `boks run`, `boks proxy` and
// `boks policy ls`, so that all three resolve a policy the same way and a rule that is
// valid for one is valid for the others.
type policyFlags struct {
	preset string
	mode   string
	allow  stringList
	deny   stringList
	inject stringList
	guest  stringList
}

// register adds the flags to a flag set. The preset default is empty rather than
// "standard" so that a caller can tell "the user asked for the default" apart from "the
// user said nothing", which matters while the policy is not enforced.
func (f *policyFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.preset, "policy", "", "network policy preset: "+strings.Join(policy.PresetNames(), ", ")+
		" (default "+policy.DefaultPreset+")")
	fs.StringVar(&f.mode, "net", "", "network mode: none (no network at all) or nat (default "+
		string(network.DefaultMode)+")")
	fs.Var(&f.allow, "allow", "allow a destination, host[:ports] (repeatable)")
	fs.Var(&f.deny, "deny", "deny a destination, host[:ports] (repeatable); deny always wins")
	fs.Var(&f.inject, "inject", "attach a credential: service@host[,host]=bearer|basic[:user]|header[:format] (repeatable)")
	fs.Var(&f.guest, "guest-credential", "what the guest holds instead: service=[ENV_NAME=]placeholder (repeatable)")
}

// specified reports whether the user set any of them.
func (f *policyFlags) specified() bool {
	return f.preset != "" || f.mode != "" || len(f.allow) > 0 || len(f.deny) > 0 ||
		len(f.inject) > 0 || len(f.guest) > 0
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

// resolve builds the effective policy.
func (f *policyFlags) resolve() (policy.Policy, error) {
	preset := f.preset
	if preset == "" {
		preset = policy.DefaultPreset
	}
	return policy.Resolve(preset, f.allow, f.deny)
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
// fails. What is still unproven is stated in the same breath, because this project has
// never seen a policy applied to a real VM.
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

Measured against a real guest on 2026-08-12, on macOS with a hypervisor: a sandbox
with every proxy variable unset was refused a denied host and a denied address, and
reached an allowed address end to end with the origin's own certificate. Both showed
up in the log as transparent. See docs/verification.md for the transcript, and
docs/security-model.md for what is still open — Linux is not covered by that run.`

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
