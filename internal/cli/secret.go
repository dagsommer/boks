package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/secret"
)

// newSecretCommand manages the host-side credential store.
//
// Every subcommand here runs on the host, against a local encrypted file. There is no
// network protocol, no socket and no daemon — deliberately. The moment a guest can ask for
// a secret, the guarantee that the guest never holds the value is gone.
func newSecretCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage host-side credentials the guest never receives",
		Long: fmt.Sprintf(`Credentials live in an encrypted file on this machine and are never written into a
sandbox. The host proxy attaches them to requests for the hosts named by an --inject rule;
see 'boks proxy --help'.

The file is encrypted with a passphrase taken from %s. Without an OS
keychain that is exactly as strong as the passphrase, and no stronger.`, secret.PassphraseEnv),
	}
	cmd.AddCommand(newSecretSetCommand(env), newSecretLsCommand(env), newSecretRmCommand(env))
	return cmd
}

// storeFlag is the one flag every secret subcommand shares.
func storeFlag(cmd *cobra.Command, path *string) {
	cmd.Flags().StringVar(path, "store", "", "encrypted store file")
}

func newSecretSetCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set [flags] NAME",
		Short: "Store a credential, read from stdin or --value",
		Long: `Stores a credential under a name. Prefer stdin over --value: an argument is visible in the
process list and in your shell history.`,
		Example: `  echo -n "$TOKEN" | boks secret set github`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usagef("a secret name is required")
			}
			return nil
		},
	}
	var value, path string
	cmd.Flags().StringVar(&value, "value", "", "the credential; omit to read it from stdin")
	storeFlag(cmd, &path)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		raw := value
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

		store, err := openSecretStore(path)
		if err != nil {
			return err
		}
		if err := store.Set(args[0], secret.NewValue(raw)); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "stored %q in %s\n", args[0], store.Path())
		return nil
	}
	return cmd
}

func newSecretLsCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls [flags]",
		Short: "List the names of stored credentials",
		Args:  noArgs,
	}
	var path string
	storeFlag(cmd, &path)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		store, err := openSecretStore(path)
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
		// Names only. There is no subcommand that prints a value, and there should not
		// be.
		for _, n := range names {
			fmt.Fprintln(env.Stdout, n)
		}
		return nil
	}
	return cmd
}

func newSecretRmCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm [flags] NAME",
		Short: "Remove a credential",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usagef("a secret name is required")
			}
			return nil
		},
	}
	var path string
	storeFlag(cmd, &path)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		store, err := openSecretStore(path)
		if err != nil {
			return err
		}
		if err := store.Delete(args[0]); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "removed %q\n", args[0])
		return nil
	}
	return cmd
}
