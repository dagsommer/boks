package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/dagsommer/boks/internal/secret"
)

// secretCommand manages the host-side credential store.
//
// Every subcommand here runs on the host, against a local encrypted file. There is no
// network protocol, no socket and no daemon — deliberately. The moment a guest can ask for
// a secret, the guarantee that the guest never holds the value is gone.
func secretCommand(ctx context.Context, env Env) error {
	if len(env.Args) == 0 {
		secretUsage(env.Stderr)
		return errors.New("a subcommand is required")
	}
	sub := Env{Args: env.Args[1:], Stdin: env.Stdin, Stdout: env.Stdout, Stderr: env.Stderr}
	switch env.Args[0] {
	case "-h", "--help", "help":
		secretUsage(env.Stdout)
		return nil
	case "set":
		return secretSet(ctx, sub)
	case "ls":
		return secretLs(ctx, sub)
	case "rm":
		return secretRm(ctx, sub)
	}
	secretUsage(env.Stderr)
	return fmt.Errorf("unknown secret subcommand %q", env.Args[0])
}

func secretUsage(w io.Writer) {
	fmt.Fprintf(w, `Usage: boks secret <set|ls|rm> [flags]

  set <name>   store a credential, read from stdin or -value
  ls           list the names of stored credentials
  rm <name>    remove a credential

Credentials live in an encrypted file on this machine and are never written into a
sandbox. The host proxy attaches them to requests for the hosts named by a -secret rule;
see 'boks proxy -h'.

The file is encrypted with a passphrase taken from %s. Without an OS
keychain that is exactly as strong as the passphrase, and no stronger.
`, secret.PassphraseEnv)
}

func secretSet(_ context.Context, env Env) error {
	fset := flag.NewFlagSet("boks secret set", flag.ContinueOnError)
	fset.SetOutput(env.Stderr)
	var (
		value = fset.String("value", "", "the credential; omit to read it from stdin")
		path  = fset.String("store", "", "encrypted store file")
	)
	fset.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks secret set [flags] <name>

Stores a credential under a name. Prefer stdin over -value: an argument is visible in the
process list and in your shell history.

  echo -n "$TOKEN" | boks secret set github

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
	if fset.NArg() != 1 {
		fset.Usage()
		return errors.New("a secret name is required")
	}

	raw := *value
	if raw == "" {
		data, err := io.ReadAll(env.Stdin)
		if err != nil {
			return fmt.Errorf("reading the credential from stdin: %w", err)
		}
		raw = strings.TrimRight(string(data), "\r\n")
	}
	if raw == "" {
		return errors.New("the credential is empty")
	}

	store, err := openSecretStore(*path)
	if err != nil {
		return err
	}
	if err := store.Set(fset.Arg(0), secret.NewValue(raw)); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "stored %q in %s\n", fset.Arg(0), store.Path())
	return nil
}

func secretLs(_ context.Context, env Env) error {
	fset := flag.NewFlagSet("boks secret ls", flag.ContinueOnError)
	fset.SetOutput(env.Stderr)
	path := fset.String("store", "", "encrypted store file")
	if err := fset.Parse(env.Args); err != nil {
		if err == flag.ErrHelp {
			return flagErrHelp
		}
		return err
	}
	store, err := openSecretStore(*path)
	if err != nil {
		return err
	}
	names, err := store.Names()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintf(env.Stdout, "no secrets stored in %s\n", store.Path())
		return nil
	}
	// Names only. There is no subcommand that prints a value, and there should not be.
	for _, n := range names {
		fmt.Fprintln(env.Stdout, n)
	}
	return nil
}

func secretRm(_ context.Context, env Env) error {
	fset := flag.NewFlagSet("boks secret rm", flag.ContinueOnError)
	fset.SetOutput(env.Stderr)
	path := fset.String("store", "", "encrypted store file")
	if err := fset.Parse(env.Args); err != nil {
		if err == flag.ErrHelp {
			return flagErrHelp
		}
		return err
	}
	if fset.NArg() != 1 {
		return errors.New("a secret name is required")
	}
	store, err := openSecretStore(*path)
	if err != nil {
		return err
	}
	if err := store.Delete(fset.Arg(0)); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "removed %q\n", fset.Arg(0))
	return nil
}
