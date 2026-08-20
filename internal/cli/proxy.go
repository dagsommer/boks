package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/ca"
	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/proxy"
	"github.com/dagsommer/boks/internal/secret"
)

// newProxyCommand runs the host forward proxy on its own.
//
// It is separate from `boks run` on purpose. The proxy is not wired into a sandbox's
// datapath, because doing so would mean setting HTTP_PROXY in the guest and calling the
// result a network policy — and a guest that ignores the variable would be unaffected.
// Running it standalone lets the policy engine, the filtering and the credential path be
// exercised for real while the enforcement question is settled elsewhere.
func newProxyCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proxy [flags]",
		Short: "Run the host forward proxy on its own, outside any sandbox",
		Long: `Runs the host forward proxy: HTTP and HTTPS (via CONNECT) are filtered against a network
policy, and credentials are attached to requests for the hosts they name, without the value
ever existing inside a sandbox.

The credentials are the ones in the store — anything stored under a service boks knows is
attached with no flag at all, exactly as it would be in a sandbox — plus whatever --inject
names. --no-secrets leaves the store out.

Hosts a credential names are the only ones whose TLS is terminated: for those, and only
those, the proxy presents a certificate from the local boks CA, verifies the origin itself,
and can read the traffic. Every other destination is tunnelled untouched, with the origin's
own certificate chain intact. 'boks policy log' shows which was which.

Point a client at it with HTTP_PROXY/HTTPS_PROXY. Nothing is wired into 'boks run'.`,
		Example: `  boks proxy --policy standard
  boks proxy --policy locked --allow api.example.com:443 -v
  boks proxy --inject 'my-api@api.example.com=header:x-api-key'`,
		Args: noArgs,
	}
	var (
		listen  string
		logPath string
		verbose bool
		store   string
		caPath  string
		noMITM  bool
	)
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:0", "address to listen on")
	cmd.Flags().StringVar(&logPath, "log", policy.DefaultLogPath(), "append decisions to this file")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "print every decision as it is made")
	cmd.Flags().StringVar(&store, "secrets", "", "encrypted secret store (default: the one 'boks secret' uses)")
	cmd.Flags().StringVar(&caPath, "ca", "", "certificate authority directory (default: the one 'boks ca' uses)")
	cmd.Flags().BoolVar(&noMITM, "no-intercept", false,
		"never terminate TLS; credential rules then apply to plaintext HTTP only")

	var flags policyFlags
	flags.register(cmd.Flags())

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		pol, err := flags.resolve()
		if err != nil {
			return err
		}
		// The credential set is the flags plus whatever the store already holds under a
		// service name; `boks proxy` and `boks run` resolve it identically, because a
		// user debugging a credential with the standalone proxy is debugging the one
		// their sandbox will get.
		// The records are re-read below, from the store this command opens for itself:
		// `boks proxy` keeps the store rather than handing values to another process, so
		// it has no use for the copy resolveCredentials made.
		plan, _, err := flags.resolveCredentials(cmd.Context(), env.Stderr)
		if err != nil {
			return err
		}
		rules, err := secret.ParseCredentials(plan.inject, plan.guest)
		if err != nil {
			return err
		}
		plan.describe(env.Stderr)

		var provider secret.Provider
		if len(rules) > 0 || len(plan.oauth) > 0 {
			// The file store is the provider for both kinds. For OAuth that matters
			// beyond convenience: it is also the OAuthSaver, so a refresh performed
			// here is written back durably. A sandbox's supervisor has no passphrase
			// and cannot do that — see internal/secret's MemoryStore.
			fileStore, err := openSecretStore(store)
			if err != nil {
				return err
			}
			oauthRules, err := oauthCredentials(cmd.Context(), fileStore, plan.oauth)
			if err != nil {
				return err
			}
			rules = append(rules, oauthRules...)
			provider = fileStore
		}
		injector, err := secret.NewInjector(provider, rules...)
		if err != nil {
			return err
		}

		// The authority is created only when a credential rule exists to justify it. A
		// proxy with no injection configured never terminates anything and has no
		// reason to own a signing key.
		var authority *ca.Authority
		if len(rules) > 0 && !noMITM {
			authority, err = ca.OpenOrCreate(caDir(caPath))
			if err != nil {
				return err
			}
		}

		decisions := policy.NewLog(policy.DefaultCapacity)
		if logPath != "" {
			sink, err := policy.NewFileSink(logPath)
			if err != nil {
				return err
			}
			defer sink.Close()
			decisions.AddSink(sink)
		}
		if verbose {
			decisions.AddSink(stderrSink{w: env.Stderr})
		}

		srv, err := proxy.New(proxy.Config{
			Engine:   policy.NewEngine(pol, decisions),
			Injector: injector,
			CA:       authority,
			ErrorLog: log.New(env.Stderr, "boks proxy: ", 0),
		})
		if err != nil {
			return err
		}

		l, err := net.Listen("tcp", listen)
		if err != nil {
			return fmt.Errorf("listening on %s: %w", listen, err)
		}
		defer l.Close()

		fmt.Fprint(env.Stderr, pol.Describe())
		fmt.Fprintf(env.Stderr, "\nlistening on http://%s\n", l.Addr())
		fmt.Fprintf(env.Stderr, "  export HTTP_PROXY=http://%s HTTPS_PROXY=http://%s\n", l.Addr(), l.Addr())
		if logPath != "" {
			fmt.Fprintf(env.Stderr, "decisions: %s  (boks policy log)\n", logPath)
		}
		switch {
		case authority != nil:
			fmt.Fprintf(env.Stderr, "\n%s", interceptionNotice(rules))
			fmt.Fprintf(env.Stderr, "CA: %s\n    sha256 %s\n", authority.CertPath(), authority.Fingerprint())
		case len(rules) > 0:
			fmt.Fprint(env.Stderr, "\nNOTE: --no-intercept is set, so TLS is never terminated and the credential rules\n"+
				"      above apply to plaintext HTTP only. Requests to those hosts over HTTPS go out\n"+
				"      unauthenticated, carrying whatever placeholder the client sent.\n")
		}
		fmt.Fprintf(env.Stderr, "\n%s\n", proxyCaveat)

		go func() {
			<-cmd.Context().Done()
			srv.Close()
		}()
		return srv.Serve(l)
	}
	return cmd
}

// stderrSink prints decisions as they happen, for watching a workload and building an
// allowlist from what it actually asks for.
type stderrSink struct{ w io.Writer }

func (s stderrSink) Record(d policy.Decision) { fmt.Fprintln(s.w, d) }

// oauthCredentials reads the named OAuth credentials out of the store and turns them into
// credentials the injector can run from.
//
// The shape — token endpoint, sentinels, resource hosts, credential file — comes from the
// stored record rather than from a flag, because it is a property of the credential that was
// imported and not of this run. `--oauth NAME` is therefore all a user has to type, and two
// commands cannot disagree about what a credential means.
func oauthCredentials(ctx context.Context, store secret.Store, names []string) ([]secret.Credential, error) {
	var out []secret.Credential
	for _, name := range names {
		record, err := store.LookupOAuthRecord(ctx, name)
		if err != nil {
			if errors.Is(err, secret.ErrNotFound) {
				return nil, fmt.Errorf("no oauth credential named %q; import one with 'boks secret import %s'", name, name)
			}
			return nil, err
		}
		c, err := record.Credential()
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// oauthRecords reads the named OAuth credentials out of the store whole — tokens included —
// for handing to a sandbox's network supervisor. Nothing prints its result.
func oauthRecords(ctx context.Context, store secret.Store, names []string) (map[string]secret.OAuthRecord, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make(map[string]secret.OAuthRecord, len(names))
	for _, name := range names {
		record, err := store.LookupOAuthRecord(ctx, name)
		if err != nil {
			if errors.Is(err, secret.ErrNotFound) {
				return nil, fmt.Errorf("no oauth credential named %q; import one with 'boks secret import %s'", name, name)
			}
			return nil, err
		}
		out[name] = record
	}
	return out, nil
}

// openSecretStore opens the encrypted store, reporting clearly when the passphrase is
// missing rather than failing later inside a request.
func openSecretStore(path string) (secret.Store, error) {
	// An explicit --secret-store path names a FILE, so it selects the file store and the
	// passphrase with it. Someone who points Boks at a particular file is not asking for
	// the OS keyring.
	if path != "" {
		return openFileStore(path)
	}

	// The keyring wins whenever there is one, and the presence of BOKS_SECRETS_PASSPHRASE
	// does NOT change that.
	//
	// It used to. The rule was "passphrase set means use the file", which made the store a
	// function of the environment — and two terminals on one machine then used two
	// different stores. Reported exactly that way: a credential set in a shell that had
	// the passphrase exported went into secrets.json, and `boks secret ls` in a fresh
	// shell read the keyring and showed nothing. Both were behaving as designed, which is
	// what made it baffling.
	//
	// A store must be a property of the machine, not of whichever variables a shell
	// happens to carry. So: the keyring if the host has one, the file only when it does
	// not — or when the user says so with BOKS_NO_KEYRING, which is a deliberate choice
	// rather than a leftover export.
	// Asked for the file explicitly. Checked before the keyring is probed, so that on a
	// machine whose keychain prompts, saying "not the keyring" does not raise the prompt
	// on the way to honouring that.
	if os.Getenv(secret.DisableKeyringEnv) != "" {
		return openFileStore(secret.DefaultPath(policy.StateDir()))
	}

	ring, err := secret.OpenKeyring(context.Background())
	if err == nil {
		return secret.NewKeyringStore(ring, secret.DefaultIndexPath(policy.StateDir())), nil
	}
	if !errors.Is(err, secret.ErrNoKeyring) {
		// The keyring exists and refused — a locked keychain, a denied prompt. Falling
		// back here would answer that by reading a different store, and report the
		// credentials it holds as the whole truth.
		return nil, err
	}
	// No keyring, and no automatic fallback.
	//
	// The encrypted file still exists, and is still the only option on a host with no
	// keyring at all — a container, a headless server, an SSH session with no session bus.
	// But it is never selected implicitly: a fallback that happens on its own is how two
	// machines, or two shells, end up using different stores without anyone choosing.
	// BOKS_NO_KEYRING is how you choose it, and it has to be said out loud.
	return nil, fmt.Errorf("%w.\n"+
		"This host has no usable OS keyring: %v.\n"+
		"Boks keeps credentials in the OS keyring and does not fall back on its own. To use "+
		"the encrypted file instead, set %s=1 and %s.",
		secret.ErrNoStore, err, secret.DisableKeyringEnv, secret.PassphraseEnv)
}

// openFileStore opens the passphrase-encrypted file, reporting clearly when the passphrase is
// missing rather than failing later inside a request.
func openFileStore(path string) (secret.Store, error) {
	if path == "" {
		path = secret.DefaultPath(policy.StateDir())
	}
	pass := os.Getenv(secret.PassphraseEnv)
	if pass == "" {
		// ErrNoStore, because this IS the no-store condition in its other shape: the
		// file was chosen and cannot be opened, so there is nothing to read.
		return nil, fmt.Errorf("%w: the encrypted file store needs %s",
			secret.ErrNoStore, secret.PassphraseEnv)
	}
	return secret.OpenFile(path, []byte(pass))
}
