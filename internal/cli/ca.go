package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/ca"
	"github.com/dagsommer/boks/internal/policy"
)

// newCaCommand manages the local certificate authority used for TLS interception.
//
// It is a top-level command rather than a flag on `boks proxy` because the authority
// outlives any one proxy run and because the questions a user has about it — what is it,
// what is its fingerprint, how do I stop trusting it — are questions about a stored
// artefact, not about a running process. `show`, `export` and `regenerate` are the three
// things you can want: inspect it, hand the public half to a guest, and throw it away.
//
// There is deliberately no subcommand that prints the private key.
func newCaCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ca",
		Short: "Inspect or replace the local CA used for TLS interception",
		Long: fmt.Sprintf(`Boks generates one local certificate authority, on this machine, the first time a run
needs to attach a credential to an HTTPS request. Injecting a header means reading the
request, which means terminating TLS, which means a certificate the guest accepts.

Only hosts named by a credential rule are ever intercepted. Everything else is tunnelled
with the origin's own certificate chain untouched.

The private key never leaves this machine and is never written into a guest. Install the
certificate in a sandbox, never in your host trust store: in a sandbox its reach is that
sandbox, in your login keychain it is every TLS connection you make.

'boks ca env' prints %s=<base64 certificate>, for runtimes with their own
trust store (Node, Python) that ignore the system one.`, ca.CertEnvVar),
	}
	cmd.AddCommand(
		newCaShowCommand(env),
		newCaExportCommand(env),
		newCaEnvCommand(env),
		newCaRegenerateCommand(env),
	)
	return cmd
}

// dirFlag is the authority-directory flag every ca subcommand shares.
func dirFlag(cmd *cobra.Command, dir *string) {
	cmd.Flags().StringVar(dir, "dir", "", "authority directory")
}

// caDir resolves where the authority lives, honouring an override for tests and for people
// who keep state somewhere unusual.
func caDir(override string) string {
	if override != "" {
		return override
	}
	return ca.DefaultDir(policy.StateDir())
}

func newCaShowCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show [flags]",
		Short: "Print the authority's fingerprint, validity and location",
		Args:  noArgs,
	}
	var (
		dir    string
		create bool
	)
	dirFlag(cmd, &dir)
	cmd.Flags().BoolVar(&create, "create", false, "generate the authority if it does not exist yet")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		authority, err := ca.Open(caDir(dir))
		if errors.Is(err, ca.ErrNotFound) && create {
			authority, err = ca.Create(caDir(dir))
		}
		if errors.Is(err, ca.ErrNotFound) {
			fmt.Fprintf(env.Stdout, "no certificate authority in %s\n", caDir(dir))
			fmt.Fprint(env.Stdout, "One is generated on first use, or now with 'boks ca show --create'.\n"+
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
	return cmd
}

func newCaExportCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export [flags]",
		Short: "Write the CA certificate (the public half) somewhere",
		Long: `Writes the CA certificate — the public half, safe to copy anywhere — in PEM form. Most
tooling takes it through an environment variable:

  SSL_CERT_FILE, CURL_CA_BUNDLE, REQUESTS_CA_BUNDLE, NODE_EXTRA_CA_CERTS`,
		Args: noArgs,
	}
	var dir, out string
	dirFlag(cmd, &dir)
	cmd.Flags().StringVarP(&out, "output", "o", "", "write to this file instead of stdout")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		authority, err := ca.OpenOrCreate(caDir(dir))
		if err != nil {
			return err
		}
		if out == "" {
			_, err := env.Stdout.Write(authority.CertPEM())
			return err
		}
		if err := os.WriteFile(out, authority.CertPEM(), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", out, err)
		}
		fmt.Fprintf(env.Stderr, "wrote the boks CA certificate to %s\n", out)
		fmt.Fprintf(env.Stderr, "sha256: %s\n", authority.Fingerprint())
		return nil
	}
	return cmd
}

// newCaEnvCommand prints the certificate in the form a guest's setup can consume without a
// file.
//
// Two distribution routes exist because one is not enough: a certificate in the guest's
// system trust store is invisible to Node and to Python's certifi, which carry their own.
// The environment variable lets a guest write the certificate wherever each runtime looks.
func newCaEnvCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env [flags]",
		Short: "Print the certificate as an environment variable, for runtimes with their own trust store",
		Args:  noArgs,
	}
	var (
		dir    string
		export bool
	)
	dirFlag(cmd, &dir)
	cmd.Flags().BoolVar(&export, "export", false, "prefix with 'export ' so the output can be sourced")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		authority, err := ca.OpenOrCreate(caDir(dir))
		if err != nil {
			return err
		}
		prefix := ""
		if export {
			prefix = "export "
		}
		fmt.Fprintf(env.Stdout, "%s%s=%s\n", prefix, ca.CertEnvVar, authority.CertBase64())
		return nil
	}
	return cmd
}

func newCaRegenerateCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "regenerate [flags]",
		Short: "Replace the authority; anything trusting the old one stops working",
		Long: `Generates a new authority and discards the old one. This is what revocation means here:
there is no revocation list for a guest to check, so retiring an authority is deleting
its key. Certificates already issued chain to something nothing trusts any more.

Anything you gave the old certificate to must be given the new one.`,
		Args: noArgs,
	}
	var (
		dir   string
		yes   bool
		quiet bool
	)
	dirFlag(cmd, &dir)
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only the new fingerprint")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		target := caDir(dir)
		if !yes {
			// Confirmation is on stdin so that a script can pipe it, and refusal is
			// the default: this breaks every guest already carrying the old
			// certificate.
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
		if quiet {
			fmt.Fprintln(env.Stdout, authority.Fingerprint())
			return nil
		}
		fmt.Fprintf(env.Stdout, "generated a new certificate authority in %s\n\n", target)
		fmt.Fprint(env.Stdout, authority.Info().String())
		fmt.Fprint(env.Stdout, "\nRe-export it to anything that trusted the old one: boks ca export -o boks-ca.pem\n")
		return nil
	}
	return cmd
}
