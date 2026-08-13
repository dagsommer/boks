package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/dagsommer/boks/internal/secret"
)

// knownServices is the registry of services Boks recognises by name. It is built once: the
// definitions are ours, and Services panics on a bad one, so a failure here is a build that
// should never have shipped rather than a condition to handle.
var knownServices = secret.Services()

// credentialPlan is the set of credentials a run ends up with: the ones the user spelled out,
// plus the ones the store already holds for services Boks knows by name.
//
// # Why a stored credential applies without being asked for
//
// This is the other half of `boks secret set anthropic`. Storing a key under a service's name
// is the whole configuration — sbx works this way too, and a credential nobody can use is a
// credential nobody stored on purpose. The alternative, a flag naming the service on every
// run, would put the ceremony back one command later.
//
// Two properties keep that from being a quiet expansion of what a sandbox can do:
//
//   - **It is announced.** A host whose TLS Boks is about to terminate for the first time is
//     printed in full, before anything runs, and `--quiet` does not suppress it. Interception
//     arriving because of a stored credential is announced exactly like interception arriving
//     because of a flag.
//   - **It is not reachability.** A credential rule says where a value may go, never what a
//     sandbox may reach. The network policy still has to allow the host, and the default
//     preset does not.
//
// `--no-secrets` turns the whole of it off for one run, which is the only way to run a
// sandbox that does not carry credentials you have stored.
type credentialPlan struct {
	// inject and guest are the --inject and --guest-credential specs, the user's own
	// first and the registry's after.
	inject []string
	guest  []string
	// oauth names the stored OAuth credentials this run uses.
	oauth []string
	// adopted names the services taken from the store rather than from a flag, for a note
	// that says what a run is doing on the user's behalf.
	adopted []string
	// shadowed names the API-key credentials an OAuth credential took precedence over.
	shadowed []string
}

// planCredentials assembles the plan.
//
// A store that cannot be opened is not an error here, and that is deliberate: for a run with
// no credential flags the answer to "is there a passphrase" is simply "then there are no
// stored credentials to attach", and failing `boks run` over a store the user was not asking
// for would be the wrong trade. A run that *did* name a credential still fails, later and by
// name, when the value it needs cannot be read.
func (f *policyFlags) planCredentials(store *secret.FileStore) (credentialPlan, error) {
	plan := credentialPlan{
		inject: slices.Clone(f.inject),
		guest:  slices.Clone(f.guest),
		oauth:  slices.Clone(f.oauth),
	}
	if store == nil || f.noSecrets {
		return plan, nil
	}
	entries, err := store.Entries()
	if err != nil {
		return plan, err
	}

	// A service the user spelled out is left entirely alone. `--inject` still overrides
	// the registry, which is what makes the registry a default rather than a ceiling.
	named := map[string]bool{}
	for _, spec := range f.inject {
		if service, _, perr := secret.ParseInject(spec); perr == nil {
			named[service] = true
		}
	}
	for _, name := range f.oauth {
		named[name] = true
	}

	for _, e := range entries {
		if named[e.Name] {
			continue
		}
		if e.OAuth {
			// An OAuth credential carries its own shape — token endpoint, resource
			// hosts, sentinels — so there is nothing for the registry to supply and
			// nothing for the user to type.
			plan.oauth = append(plan.oauth, e.Name)
			plan.adopted = append(plan.adopted, e.Name+" (oauth)")
			continue
		}
		service, ok := knownServices.Lookup(e.Name)
		if !ok || !service.Configured() {
			// A credential stored under a name Boks does not know, or knows and has
			// no rule for, is used only when a --inject rule names it. There is
			// nowhere else it could go.
			continue
		}
		plan.inject = append(plan.inject, service.InjectSpecs()...)
		if g := service.GuestSpec(); g != "" {
			plan.guest = append(plan.guest, g)
		}
		plan.adopted = append(plan.adopted, e.Name)
	}
	return plan, nil
}

// preferOAuth applies the precedence rule: for a destination an OAuth credential already
// covers, an API key is dropped rather than attached beside it.
//
// The reason is in secret.PreferOAuth. What belongs here is that the outcome is *printed*: a
// credential silently not being used is exactly the failure this rule exists to prevent, and
// replacing one silent choice with another would be no improvement.
func (p *credentialPlan) preferOAuth(records map[string]secret.OAuthRecord) error {
	if len(records) == 0 || len(p.inject) == 0 {
		return nil
	}
	credentials, err := secret.ParseCredentials(p.inject, p.guest)
	if err != nil {
		return err
	}
	for _, name := range p.oauth {
		c, cerr := records[name].Credential()
		if cerr != nil {
			return cerr
		}
		credentials = append(credentials, c)
	}
	_, dropped := secret.PreferOAuth(credentials)
	if len(dropped) == 0 {
		return nil
	}
	p.shadowed = dropped
	drop := map[string]bool{}
	for _, name := range dropped {
		drop[name] = true
	}
	p.inject = filterSpecs(p.inject, drop, func(spec string) string {
		service, _, _ := secret.ParseInject(spec)
		return service
	})
	p.guest = filterSpecs(p.guest, drop, func(spec string) string {
		service, _, _, _ := secret.ParseGuestCredential(spec)
		return service
	})
	return nil
}

// filterSpecs removes the specs belonging to the named services.
func filterSpecs(specs []string, drop map[string]bool, service func(string) string) []string {
	out := specs[:0:0]
	for _, spec := range specs {
		if drop[service(spec)] {
			continue
		}
		out = append(out, spec)
	}
	return out
}

// services returns the union of the credential names this plan needs a value for.
func (p credentialPlan) services() ([]string, error) {
	credentials, err := secret.ParseCredentials(p.inject, p.guest)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(credentials))
	for _, c := range credentials {
		out = append(out, c.Service)
	}
	return out, nil
}

// describe prints what the plan did on the user's behalf, and nothing about any value.
//
// The adoption line is one line, on every run, for the same reason the steady-state policy
// line is: what a sandbox carries is worth restating rather than mentioning once. The loud
// part — a host whose TLS is about to be terminated for the first time — is printed by
// describeNetwork, which knows what this sandbox has already been told.
func (p credentialPlan) describe(w io.Writer) {
	if len(p.adopted) > 0 {
		fmt.Fprintf(w, "credentials: %s, from the store · --no-secrets leaves them out\n",
			strings.Join(p.adopted, ", "))
	}
	for _, name := range p.shadowed {
		fmt.Fprintf(w, "note: the API key %q is NOT being attached: an OAuth credential already covers\n"+
			"      every host it names, and a login takes precedence over a key. Remove the OAuth\n"+
			"      credential with 'boks secret rm' if you meant to use the key instead.\n", name)
	}
}

// openSecretStoreIfAvailable opens the store when there is a passphrase to open it with, and
// reports no store rather than an error when there is not.
//
// The distinction matters for a command that did not ask for a credential: without a
// passphrase there is nothing in the store that could be attached, so "no store" and "an
// empty store" are the same thing, and only one of them is worth failing over.
func openSecretStoreIfAvailable(path string) (*secret.FileStore, error) {
	if os.Getenv(secret.PassphraseEnv) == "" {
		return nil, nil
	}
	return openSecretStore(path)
}

// resolveCredentials builds the plan for a run: what the flags asked for, what the store adds,
// and the OAuth records the whole of it needs.
//
// A store that will not decrypt is reported and stepped over when this run named no
// credential of its own, and returned as an error when it did. The warning is not optional
// and not quiet: a user whose passphrase is wrong is running without the credentials they
// think they have, and finding that out from a 401 inside a sandbox is worse than a line of
// stderr.
func (f *policyFlags) resolveCredentials(ctx context.Context, stderr io.Writer) (credentialPlan, map[string]secret.OAuthRecord, error) {
	asked := len(f.inject) > 0 || len(f.guest) > 0 || len(f.oauth) > 0
	store, err := openSecretStoreIfAvailable("")
	if err != nil {
		if asked {
			return credentialPlan{}, nil, err
		}
		fmt.Fprintf(stderr, "warning: the credential store could not be opened, so no stored credential is\n"+
			"         attached to this sandbox: %v\n", err)
		store = nil
	}
	if store == nil && asked {
		// The flags name a credential and there is no store to read it from. Fail with
		// the message that says which variable is missing.
		if _, err := openSecretStore(""); err != nil {
			return credentialPlan{}, nil, err
		}
	}

	plan, err := f.planCredentials(store)
	if err != nil {
		if asked {
			return credentialPlan{}, nil, explainSecretFailure(storePath(store), err)
		}
		fmt.Fprintf(stderr, "warning: the credential store could not be read, so no stored credential is\n"+
			"         attached to this sandbox: %v\n", err)
		plan = credentialPlan{inject: slices.Clone(f.inject), guest: slices.Clone(f.guest)}
		return plan, nil, nil
	}

	var records map[string]secret.OAuthRecord
	if len(plan.oauth) > 0 {
		if records, err = oauthRecords(ctx, store, plan.oauth); err != nil {
			return credentialPlan{}, nil, err
		}
		if err := plan.preferOAuth(records); err != nil {
			return credentialPlan{}, nil, err
		}
	}
	return plan, records, nil
}

// storePath is the store's path, or a placeholder when there is no store to name.
func storePath(store *secret.FileStore) string {
	if store == nil {
		return "the credential store"
	}
	return store.Path()
}
