package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"

	"github.com/dagsommer/boks/internal/ca"
	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/proxy"
	"github.com/dagsommer/boks/internal/secret"
)

// proxyCommand runs the host forward proxy on its own.
//
// It is separate from `boks run` on purpose. The proxy is not wired into a sandbox's
// datapath, because doing so would mean setting HTTP_PROXY in the guest and calling the
// result a network policy — and a guest that ignores the variable would be unaffected.
// Running it standalone lets the policy engine, the filtering and the credential path be
// exercised for real while the enforcement question is settled elsewhere.
func proxyCommand(ctx context.Context, env Env) error {
	fset := flag.NewFlagSet("boks proxy", flag.ContinueOnError)
	fset.SetOutput(env.Stderr)
	var (
		listen  = fset.String("listen", "127.0.0.1:0", "address to listen on")
		logPath = fset.String("log", policy.DefaultLogPath(), "append decisions to this file")
		verbose = fset.Bool("v", false, "print every decision as it is made")
		store   = fset.String("secrets", "", "encrypted secret store (default: the one 'boks secret' uses)")
		caPath  = fset.String("ca", "", "certificate authority directory (default: the one 'boks ca' uses)")
		noMITM  = fset.Bool("no-intercept", false,
			"never terminate TLS; credential rules then apply to plaintext HTTP only")
	)
	var flags policyFlags
	flags.register(fset)

	fset.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks proxy [flags]

Runs the host forward proxy: HTTP and HTTPS (via CONNECT) are filtered against a network
policy, and credentials configured with -secret are attached to requests for the hosts
they name, without the value ever existing inside a sandbox.

Hosts named by a -secret rule are the only ones whose TLS is terminated: for those, and
only those, the proxy presents a certificate from the local boks CA, verifies the origin
itself, and can read the traffic. Every other destination is tunnelled untouched, with
the origin's own certificate chain intact. 'boks policy log' shows which was which.

Point a client at it with HTTP_PROXY/HTTPS_PROXY. Nothing is wired into 'boks run'.

Examples:
  boks proxy -policy standard
  boks proxy -policy locked -allow api.example.com:443 -v
  boks proxy -secret 'api.anthropic.com=anthropic:header:x-api-key'

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

	pol, err := flags.resolve()
	if err != nil {
		return err
	}
	rules, err := flags.credentialRules()
	if err != nil {
		return err
	}

	var provider secret.Provider
	if len(rules) > 0 {
		provider, err = openSecretStore(*store)
		if err != nil {
			return err
		}
	}
	injector, err := secret.NewInjector(provider, rules...)
	if err != nil {
		return err
	}

	// The authority is created only when a credential rule exists to justify it. A proxy
	// with no injection configured never terminates anything and has no reason to own a
	// signing key.
	var authority *ca.Authority
	if len(rules) > 0 && !*noMITM {
		authority, err = ca.OpenOrCreate(caDir(*caPath))
		if err != nil {
			return err
		}
	}

	decisions := policy.NewLog(policy.DefaultCapacity)
	if *logPath != "" {
		sink, err := policy.NewFileSink(*logPath)
		if err != nil {
			return err
		}
		defer sink.Close()
		decisions.AddSink(sink)
	}
	if *verbose {
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

	l, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", *listen, err)
	}
	defer l.Close()

	fmt.Fprint(env.Stderr, pol.Describe())
	fmt.Fprintf(env.Stderr, "\nlistening on http://%s\n", l.Addr())
	fmt.Fprintf(env.Stderr, "  export HTTP_PROXY=http://%s HTTPS_PROXY=http://%s\n", l.Addr(), l.Addr())
	if *logPath != "" {
		fmt.Fprintf(env.Stderr, "decisions: %s  (boks policy log)\n", *logPath)
	}
	switch {
	case authority != nil:
		fmt.Fprintf(env.Stderr, "\n%s", interceptionNotice(rules))
		fmt.Fprintf(env.Stderr, "CA: %s\n    sha256 %s\n", authority.CertPath(), authority.Fingerprint())
	case len(rules) > 0:
		fmt.Fprint(env.Stderr, "\nNOTE: -no-intercept is set, so TLS is never terminated and the credential rules\n"+
			"      above apply to plaintext HTTP only. Requests to those hosts over HTTPS go out\n"+
			"      unauthenticated, carrying whatever placeholder the client sent.\n")
	}
	fmt.Fprintf(env.Stderr, "\n%s\n", proxyCaveat)

	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	return srv.Serve(l)
}

// stderrSink prints decisions as they happen, for watching a workload and building an
// allowlist from what it actually asks for.
type stderrSink struct{ w io.Writer }

func (s stderrSink) Record(d policy.Decision) { fmt.Fprintln(s.w, d) }

// openSecretStore opens the encrypted store, reporting clearly when the passphrase is
// missing rather than failing later inside a request.
func openSecretStore(path string) (*secret.FileStore, error) {
	if path == "" {
		path = secret.DefaultPath(policy.StateDir())
	}
	pass := os.Getenv(secret.PassphraseEnv)
	if pass == "" {
		return nil, fmt.Errorf("credential rules need the secret store, but %s is not set", secret.PassphraseEnv)
	}
	return secret.OpenFile(path, []byte(pass))
}
