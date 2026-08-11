package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/dagsommer/boks/internal/ca"
	"github.com/dagsommer/boks/internal/policy"
)

// caCommand manages the local certificate authority used for TLS interception.
//
// It is a top-level command rather than a flag on `boks proxy` because the authority
// outlives any one proxy run and because the questions a user has about it — what is it,
// what is its fingerprint, how do I stop trusting it — are questions about a stored
// artefact, not about a running process. `show`, `export` and `regenerate` are the three
// things you can want: inspect it, hand the public half to a guest, and throw it away.
//
// There is deliberately no subcommand that prints the private key.
func caCommand(ctx context.Context, env Env) error {
	if len(env.Args) == 0 {
		caUsage(env.Stderr)
		return errors.New("a subcommand is required")
	}
	sub := Env{Args: env.Args[1:], Stdin: env.Stdin, Stdout: env.Stdout, Stderr: env.Stderr}
	switch env.Args[0] {
	case "-h", "--help", "help":
		caUsage(env.Stdout)
		return nil
	case "show":
		return caShow(ctx, sub)
	case "export":
		return caExport(ctx, sub)
	case "env":
		return caEnv(ctx, sub)
	case "regenerate":
		return caRegenerate(ctx, sub)
	}
	caUsage(env.Stderr)
	return fmt.Errorf("unknown ca subcommand %q", env.Args[0])
}

func caUsage(w io.Writer) {
	fmt.Fprintf(w, `Usage: boks ca <show|export|env|regenerate> [flags]

  show         print the authority's fingerprint, validity and location
  export       write the CA certificate (the public half) somewhere
  env          print %s=<base64 certificate>, for runtimes with their own
               trust store (Node, Python) that ignore the system one
  regenerate   replace the authority; anything trusting the old one stops working

Boks generates one local certificate authority, on this machine, the first time a run
needs to attach a credential to an HTTPS request. Injecting a header means reading the
request, which means terminating TLS, which means a certificate the guest accepts.

Only hosts named by a credential rule are ever intercepted. Everything else is tunnelled
with the origin's own certificate chain untouched.

The private key never leaves this machine and is never written into a guest. Install the
certificate in a sandbox, never in your host trust store: in a sandbox its reach is that
sandbox, in your login keychain it is every TLS connection you make.
`, ca.CertEnvVar)
}

// caDir resolves where the authority lives, honouring an override for tests and for people
// who keep state somewhere unusual.
func caDir(override string) string {
	if override != "" {
		return override
	}
	return ca.DefaultDir(policy.StateDir())
}

func caShow(_ context.Context, env Env) error {
	fset := flag.NewFlagSet("boks ca show", flag.ContinueOnError)
	fset.SetOutput(env.Stderr)
	dir := fset.String("dir", "", "authority directory")
	create := fset.Bool("create", false, "generate the authority if it does not exist yet")
	if err := fset.Parse(env.Args); err != nil {
		if err == flag.ErrHelp {
			return flagErrHelp
		}
		return err
	}

	authority, err := ca.Open(caDir(*dir))
	if errors.Is(err, ca.ErrNotFound) && *create {
		authority, err = ca.Create(caDir(*dir))
	}
	if errors.Is(err, ca.ErrNotFound) {
		fmt.Fprintf(env.Stdout, "no certificate authority in %s\n", caDir(*dir))
		fmt.Fprint(env.Stdout, "One is generated on first use, or now with 'boks ca show -create'.\n"+
			"Without one, credential rules for HTTPS hosts never fire.\n")
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprint(env.Stdout, authority.Info().String())
	fmt.Fprint(env.Stdout, "\nA guest that trusts this certificate lets boks read traffic to the hosts named by\n"+
		"credential rules, and only those. It cannot mint certificates with it.\n")
	return nil
}

func caExport(_ context.Context, env Env) error {
	fset := flag.NewFlagSet("boks ca export", flag.ContinueOnError)
	fset.SetOutput(env.Stderr)
	var (
		dir = fset.String("dir", "", "authority directory")
		out = fset.String("o", "", "write to this file instead of stdout")
	)
	fset.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks ca export [flags]

Writes the CA certificate — the public half, safe to copy anywhere — in PEM form. Most
tooling takes it through an environment variable:

  SSL_CERT_FILE, CURL_CA_BUNDLE, REQUESTS_CA_BUNDLE, NODE_EXTRA_CA_CERTS

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

	authority, err := ca.OpenOrCreate(caDir(*dir))
	if err != nil {
		return err
	}
	if *out == "" {
		_, err := env.Stdout.Write(authority.CertPEM())
		return err
	}
	if err := os.WriteFile(*out, authority.CertPEM(), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", *out, err)
	}
	fmt.Fprintf(env.Stderr, "wrote the boks CA certificate to %s\n", *out)
	fmt.Fprintf(env.Stderr, "sha256: %s\n", authority.Fingerprint())
	return nil
}

// caEnv prints the certificate in the form a guest's setup can consume without a file.
//
// Two distribution routes exist because one is not enough: a certificate in the guest's
// system trust store is invisible to Node and to Python's certifi, which carry their own.
// The environment variable lets a guest write the certificate wherever each runtime looks.
func caEnv(_ context.Context, env Env) error {
	fset := flag.NewFlagSet("boks ca env", flag.ContinueOnError)
	fset.SetOutput(env.Stderr)
	var (
		dir    = fset.String("dir", "", "authority directory")
		export = fset.Bool("export", false, "prefix with 'export ' so the output can be sourced")
	)
	if err := fset.Parse(env.Args); err != nil {
		if err == flag.ErrHelp {
			return flagErrHelp
		}
		return err
	}
	authority, err := ca.OpenOrCreate(caDir(*dir))
	if err != nil {
		return err
	}
	prefix := ""
	if *export {
		prefix = "export "
	}
	fmt.Fprintf(env.Stdout, "%s%s=%s\n", prefix, ca.CertEnvVar, authority.CertBase64())
	return nil
}

func caRegenerate(_ context.Context, env Env) error {
	fset := flag.NewFlagSet("boks ca regenerate", flag.ContinueOnError)
	fset.SetOutput(env.Stderr)
	var (
		dir  = fset.String("dir", "", "authority directory")
		yes  = fset.Bool("y", false, "do not ask for confirmation")
		show = fset.Bool("q", false, "print only the new fingerprint")
	)
	fset.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks ca regenerate [flags]

Generates a new authority and discards the old one. This is what revocation means here:
there is no revocation list for a guest to check, so retiring an authority is deleting
its key. Certificates already issued chain to something nothing trusts any more.

Anything you gave the old certificate to must be given the new one.

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

	target := caDir(*dir)
	if !*yes {
		// Confirmation is on stdin so that a script can pipe it, and refusal is the
		// default: this breaks every guest already carrying the old certificate.
		fmt.Fprintf(env.Stderr, "Replace the boks certificate authority in %s?\n"+
			"Every guest holding the current certificate stops trusting intercepted hosts. [y/N] ", target)
		var answer string
		fmt.Fscanln(env.Stdin, &answer)
		if answer != "y" && answer != "Y" && answer != "yes" {
			return errors.New("not regenerating the certificate authority")
		}
	}

	authority, err := ca.Create(target)
	if err != nil {
		return err
	}
	if *show {
		fmt.Fprintln(env.Stdout, authority.Fingerprint())
		return nil
	}
	fmt.Fprintf(env.Stdout, "generated a new certificate authority in %s\n\n", target)
	fmt.Fprint(env.Stdout, authority.Info().String())
	fmt.Fprint(env.Stdout, "\nRe-export it to anything that trusted the old one: boks ca export -o boks-ca.pem\n")
	return nil
}
