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
	preset  string
	mode    string
	allow   stringList
	deny    stringList
	secrets stringList
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
	fs.Var(&f.secrets, "secret", "credential rule host[,host]=name:scheme[:extra] (repeatable)")
}

// specified reports whether the user set any of them.
func (f *policyFlags) specified() bool {
	return f.preset != "" || f.mode != "" || len(f.allow) > 0 || len(f.deny) > 0 || len(f.secrets) > 0
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

// credentialRules parses the credential rules. Values are not touched here: a rule names a
// secret, and the value is fetched from the provider at request time.
func (f *policyFlags) credentialRules() ([]secret.Rule, error) {
	var rules []secret.Rule
	for _, spec := range f.secrets {
		r, err := secret.ParseRule(spec)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, nil
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

// proxyCaveat is printed by anything that starts the proxy for real.
const proxyCaveat = `NOTE: a forward proxy filters only the traffic a client sends to it.
      A guest that ignores HTTP_PROXY and opens a raw socket is not affected.
      This is a cooperating-client mechanism, not an enforcement boundary.
`
