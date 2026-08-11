package cli

import (
	"flag"
	"fmt"
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
// Injection rules for the same service accumulate onto one credential, which is the point
// of the two-level model: four hosts sharing one enterprise token are four rules and one
// secret, not four copies of the same scheme.
func (f *policyFlags) credentialRules() ([]secret.Credential, error) {
	var order []string
	byService := map[string]*secret.Credential{}
	for _, spec := range f.inject {
		service, rules, err := secret.ParseInject(spec)
		if err != nil {
			return nil, err
		}
		if _, ok := byService[service]; !ok {
			byService[service] = &secret.Credential{Service: service}
			order = append(order, service)
		}
		byService[service].Inject = append(byService[service].Inject, rules...)
	}
	for _, spec := range f.guest {
		service, env, placeholder, err := secret.ParseGuestCredential(spec)
		if err != nil {
			return nil, err
		}
		c, ok := byService[service]
		if !ok {
			return nil, fmt.Errorf("-guest-credential %s: no -inject rule mentions service %q, so nothing would ever replace that placeholder", spec, service)
		}
		c.EnvName, c.Placeholder, c.ProxyManaged = env, placeholder, true
	}
	out := make([]secret.Credential, 0, len(order))
	for _, name := range order {
		out = append(out, *byService[name])
	}
	return out, nil
}

// notEnforcedWarning is printed wherever a policy is accepted but cannot yet be applied to
// a sandbox. It is deliberately blunt. A user who believes a flag is protecting them when
// it is not is worse off than a user with no flag at all.
const notEnforcedWarning = `WARNING: network policy is NOT enforced yet.
         These flags are parsed and validated, and 'boks policy ls' shows what they
         resolve to, but nothing applies them to a running sandbox.
         The transport that makes enforcement possible — a host-side network stack
         terminating the guest's NIC instead of libkrun's TSI — has been verified on
         real hardware, and internal/network builds the configuration for it. The
         path from a policy to a dropped packet is not finished.
         Until it is, treat these flags as documentation of intent.
         See docs/security-model.md.
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

// proxyCaveat is printed by anything that starts the proxy for real.
const proxyCaveat = `NOTE: a forward proxy filters only the traffic a client sends to it.
      A guest that ignores HTTP_PROXY and opens a raw socket is not affected.
      This is a cooperating-client mechanism, not an enforcement boundary.
`
