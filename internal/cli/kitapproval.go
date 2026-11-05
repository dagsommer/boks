package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/pflag"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

// A kit names a service and the hosts that service's credential is sent to. The credential
// itself is whatever the user stored under that name — so a kit that says `service: github`
// and lists a host of its own is asking for the user's GitHub token to be sent there. The
// kit is someone else's file, often fetched from a git remote; the user has not read it.
//
// Nothing in the kit says the person running it agreed to that, so something has to. This is
// that something, and it is the same shape Docker Sandboxes uses: before a kit's credentials
// are applied, say which services, which hosts, and whether a value is stored, and ask.
//
// # What an approval is
//
// An approval is for one kit (its name and the reference it was loaded from) and one service,
// and it is for the exact attachment rules the kit declared — hosts, header and format. A
// kit that later adds a host, or changes where a token rides, is a different request and is
// asked about again. That is why the fingerprint is the rendered --inject specs rather than a
// version: a mutable git ref can change what it asks for without changing its name.
//
// # What happens without one
//
// An unapproved kit credential is left out, never attached. A run with no terminal cannot be
// asked, so it leaves them out and prints the way to approve them: --approve-kit-credentials
// for that run, or one interactive run to remember the answer. A denied credential is not an
// error — the sandbox still starts, without the credential, and says so.

// kitCredentialAsk is one service a kit wants a credential attached for.
type kitCredentialAsk struct {
	service string
	// specs are the --inject specs the kit declared for the service, or for an OAuth
	// credential the one line kitOAuth.fingerprint gives.
	specs  []string
	hosts  []string
	stored bool
	oauth  bool
}

// key is what an approval is recorded and looked up under. A login and an API key for one
// service name are different requests, and approving one must not approve the other.
func (a kitCredentialAsk) key() string {
	if a.oauth {
		return a.service + " (oauth)"
	}
	return a.service
}

// fingerprint identifies exactly what the kit asked for, order aside.
func (a kitCredentialAsk) fingerprint() string {
	specs := slices.Clone(a.specs)
	slices.Sort(specs)
	return strings.Join(specs, "\n")
}

// kitCredentialAsks groups a kit's --inject specs by service, in the order the kit gave them.
func kitCredentialAsks(specs []string, stored map[string]bool) ([]kitCredentialAsk, error) {
	var asks []kitCredentialAsk
	index := map[string]int{}
	for _, spec := range specs {
		service, rules, err := secret.ParseInject(spec)
		if err != nil {
			return nil, err
		}
		i, ok := index[service]
		if !ok {
			i = len(asks)
			index[service] = i
			asks = append(asks, kitCredentialAsk{service: service, stored: stored[service]})
		}
		asks[i].specs = append(asks[i].specs, spec)
		for _, r := range rules {
			if h := r.Domain.String(); !slices.Contains(asks[i].hosts, h) {
				asks[i].hosts = append(asks[i].hosts, h)
			}
		}
	}
	return asks, nil
}

// kitOAuthAsks is kitCredentialAsks for a kit's OAuth credentials.
func kitOAuthAsks(shapes []kitOAuth, stored map[string]bool) []kitCredentialAsk {
	var asks []kitCredentialAsk
	for _, k := range shapes {
		asks = append(asks, kitCredentialAsk{
			service: k.service, specs: []string{k.fingerprint()}, hosts: k.hosts(),
			stored: stored[k.service], oauth: true,
		})
	}
	return asks
}

// kitApprovals is the file the answers are kept in: kit key → service → fingerprint.
type kitApprovals struct {
	path string
	Kits map[string]map[string]string `json:"kits"`
}

func kitApprovalsPath() string {
	return filepath.Join(policy.StateDir(), "kit-credential-approvals.json")
}

func loadKitApprovals(path string) (*kitApprovals, error) {
	a := &kitApprovals{path: path, Kits: map[string]map[string]string{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, a); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if a.Kits == nil {
		a.Kits = map[string]map[string]string{}
	}
	return a, nil
}

func (a *kitApprovals) approved(kitKey string, ask kitCredentialAsk) bool {
	return a.Kits[kitKey][ask.key()] == ask.fingerprint()
}

func (a *kitApprovals) record(kitKey string, ask kitCredentialAsk) {
	if a.Kits[kitKey] == nil {
		a.Kits[kitKey] = map[string]string{}
	}
	a.Kits[kitKey][ask.key()] = ask.fingerprint()
}

func (a *kitApprovals) save() error {
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.path, data, 0o600)
}

// registerKitApproval adds the flag that answers the approval non-interactively. Separate
// from register for the reason registerPublish is: `boks proxy` and `policy ls` load no kit
// and have no credentials to ask about.
func (f *policyFlags) registerKitApproval(fs *pflag.FlagSet) {
	fs.BoolVar(&f.approveKit, "approve-kit-credentials", false,
		"approve the credentials a --kit asks for, for this run only, without asking")
}

// gateKitCredentials returns the part of a kit's credential specs the user has approved,
// asking about the rest when there is a terminal to ask on.
func (f *policyFlags) gateKitCredentials(specs []string, store secret.Store, stderr io.Writer) ([]string, error) {
	specs, _, err := f.gateKit(specs, nil, store, stderr)
	return specs, err
}

// gateKit is gateKitCredentials for API-key specs and OAuth shapes together: they are asked
// about in one prompt, and either kind is applied only if approved.
func (f *policyFlags) gateKit(specs []string, shapes []kitOAuth, store secret.Store, stderr io.Writer) ([]string, []kitOAuth, error) {
	if (len(specs) == 0 && len(shapes) == 0) || f.kitSpec == nil {
		return nil, nil, nil
	}
	stored, storedOAuth := map[string]bool{}, map[string]bool{}
	if store != nil && !f.noSecrets {
		if entries, err := store.Entries(); err == nil {
			for _, e := range entries {
				if e.OAuth {
					storedOAuth[e.Name] = true
				} else {
					stored[e.Name] = true
				}
			}
		}
	}
	asks, err := kitCredentialAsks(specs, stored)
	if err != nil {
		return nil, nil, err
	}
	asks = append(asks, kitOAuthAsks(shapes, storedOAuth)...)
	approvals, err := loadKitApprovals(kitApprovalsPath())
	if err != nil {
		return nil, nil, err
	}
	key := f.kitSpec.Name + "\x00" + f.kitRef

	var pending []kitCredentialAsk
	granted := map[string]bool{}
	for _, a := range asks {
		if approvals.approved(key, a) {
			granted[a.key()] = true
		} else {
			pending = append(pending, a)
		}
	}

	switch {
	case len(pending) == 0:
	case f.approveKit:
		for _, a := range pending {
			granted[a.key()] = true
		}
		fmt.Fprintf(stderr, "kit %q: credentials approved for this run by --approve-kit-credentials: %s\n",
			f.kitSpec.Name, askNames(pending))
	case f.stdin != nil && isTerminal(f.stdin):
		answered, err := f.askKitCredentials(pending, bufio.NewReader(f.stdin), stderr)
		if err != nil {
			return nil, nil, err
		}
		for _, a := range answered {
			granted[a.key()] = true
			approvals.record(key, a)
		}
		if len(answered) > 0 {
			if err := approvals.save(); err != nil {
				fmt.Fprintf(stderr, "warning: the approval could not be remembered: %v\n", err)
			}
		}
	default:
		describeKitCredentials(stderr, f.kitSpec.Name, pending)
		fmt.Fprintf(stderr, "These are NOT attached: there is no terminal to ask on. Run once interactively to\n"+
			"approve and remember them, or pass --approve-kit-credentials for this run.\n")
	}

	var out []string
	for _, spec := range specs {
		if service, _, err := secret.ParseInject(spec); err == nil && granted[service] {
			out = append(out, spec)
		}
	}
	var outOAuth []kitOAuth
	for _, k := range shapes {
		if granted[k.service+" (oauth)"] {
			outOAuth = append(outOAuth, k)
		}
	}
	return out, outOAuth, nil
}

func askNames(asks []kitCredentialAsk) string {
	names := make([]string, len(asks))
	for i, a := range asks {
		names[i] = a.key()
	}
	return strings.Join(names, ", ")
}

func describeKitCredentials(w io.Writer, kitName string, asks []kitCredentialAsk) {
	width := 0
	for _, a := range asks {
		width = max(width, len(a.service))
	}
	fmt.Fprintf(w, "\nThe kit %q wants to use these credentials:\n", kitName)
	for _, a := range asks {
		state := "not set"
		if a.stored {
			state = "stored"
		}
		kind := "API key"
		if a.oauth {
			kind = "OAuth login"
		}
		fmt.Fprintf(w, "  %-*s  %s → sent to %s   (%s)\n",
			width, a.service, kind, strings.Join(a.hosts, ", "), state)
	}
}

// askKitCredentials prompts and returns the asks the user approved.
func (f *policyFlags) askKitCredentials(asks []kitCredentialAsk, in *bufio.Reader, w io.Writer) ([]kitCredentialAsk, error) {
	describeKitCredentials(w, f.kitSpec.Name, asks)
	fmt.Fprint(w, "[A]pprove all · [R]eview each · [N]o (default): ")
	switch answer(in) {
	case "a", "approve", "approve all":
		return asks, nil
	case "r", "review", "review each":
		var approved []kitCredentialAsk
		for _, a := range asks {
			fmt.Fprintf(w, "Send %s to %s? [y/N]: ", a.key(), strings.Join(a.hosts, ", "))
			if r := answer(in); r == "y" || r == "yes" {
				approved = append(approved, a)
			}
		}
		return approved, nil
	}
	return nil, nil
}

// answer reads one line, lowercased. End of input is "", which every prompt here treats as
// the default, and the default is always no.
func answer(in *bufio.Reader) string {
	line, _ := in.ReadString('\n')
	return strings.ToLower(strings.TrimSpace(line))
}
