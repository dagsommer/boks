package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/policy"
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
keychain that is exactly as strong as the passphrase, and no stronger.

There is no recovery for a forgotten passphrase — that is what encryption means — so
'boks secret reset' deletes the file and everything in it, which is the only way out and
is spelled out wherever the store fails to decrypt.`, secret.PassphraseEnv),
	}
	cmd.AddCommand(newSecretSetCommand(env), newSecretLsCommand(env), newSecretRmCommand(env),
		newSecretResetCommand(env))
	return cmd
}

// explainSecretFailure adds the way out to the one error every subcommand shares.
//
// A forgotten passphrase used to be a dead end reachable in one step: every subcommand has
// to decrypt the store, `rm` included, so the remedy for "wrong passphrase" was the command
// that had just failed. `ls` and `rm` failed identically, which left no move inside the CLI
// at all — you had to know to go and delete a file whose path nothing had told you.
//
// So the error names the file, says what deleting it costs, and gives the command. It does
// not offer to try another passphrase, because there is nothing to try it against: the store
// is one AES-GCM envelope, and a key that does not open it is indistinguishable from a file
// that has been damaged.
func explainSecretFailure(path string, err error) error {
	if !errors.Is(err, secret.ErrWrongPassphrase) {
		return err
	}
	return fmt.Errorf("%w\n\n"+
		"%s is the only copy, and nothing but the passphrase can open it. If it is\n"+
		"lost, the way forward is to throw the store away and store the credentials again:\n\n"+
		"  boks secret reset --force        # deletes %s and every credential in it\n\n"+
		"Each one then has to be set again with 'boks secret set NAME'. Until they are, a\n"+
		"sandbox whose policy injects a credential will refuse to start rather than run\n"+
		"without it.", err, path, path)
}

func newSecretResetCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset [flags]",
		Short: "Delete the credential store, for a passphrase that is lost",
		Long: `Deletes the encrypted credential store and everything in it.

This exists because a forgotten passphrase is otherwise a dead end: every other subcommand
has to decrypt the store to do its work, 'rm' included, so there is no way to remove a
credential you can no longer read. This one does not decrypt anything and does not need the
passphrase — it removes the file.

Nothing is recoverable afterwards. Every credential has to be stored again with
'boks secret set', and sandboxes configured to inject one will refuse to start until it is.`,
		Args: noArgs,
	}
	var path string
	var force bool
	storeFlag(cmd, &path)
	// No prompt: boks commands run in scripts and in agents' terminals, and a command
	// that destroys credentials on a bare invocation is worse than one that asks for a
	// flag. --force is the confirmation.
	cmd.Flags().BoolVar(&force, "force", false, "actually delete it; without this the command only says what it would do")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if path == "" {
			path = secret.DefaultPath(policy.StateDir())
		}
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(env.Stdout, "no credential store at %s; there is nothing to reset\n", path)
			return nil
		}
		if !force {
			return fmt.Errorf("this would delete %s and every credential in it, irreversibly.\n"+
				"Nothing else can decrypt that file, so there is no undo and no export.\n"+
				"Run it again with --force if that is what you want.", path)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("removing the credential store: %w", err)
		}
		fmt.Fprintf(env.Stdout, "deleted %s. Store credentials again with 'boks secret set NAME'.\n", path)
		return nil
	}
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
			return explainSecretFailure(store.Path(), err)
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
		// Listing needs the passphrase, and that is a property of the format rather
		// than an oversight: the store is one AES-GCM envelope over a JSON map, so the
		// names live inside the ciphertext with the values. Keeping them outside would
		// mean publishing, in cleartext next to the file, which services this machine
		// holds credentials for — which is exactly the metadata an attacker who cannot
		// decrypt the file would like to have.
		Long: `Lists the names of stored credentials, never their values.

This needs the passphrase, because the names are inside the encrypted envelope with the
values. That is deliberate: a plaintext index of which services you hold credentials for is
useful to somebody who cannot read the credentials themselves.`,
		Args: noArgs,
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
			return explainSecretFailure(store.Path(), err)
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
			return explainSecretFailure(store.Path(), err)
		}
		fmt.Fprintf(env.Stdout, "removed %q\n", args[0])
		return nil
	}
	return cmd
}
