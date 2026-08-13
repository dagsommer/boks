package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

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
	cmd.AddCommand(newSecretSetCommand(env), newSecretImportCommand(env), newSecretLsCommand(env), newSecretRmCommand(env))
	return cmd
}

// newSecretImportCommand reads a credential an agent already holds.
//
// This is the command that decides whether Boks is usable at all by the people who actually
// run the flagship agent. A Claude.ai subscription user has no API key to type: `claude
// /login` left an OAuth token pair in the macOS Keychain, under `Claude Code-credentials`,
// and nothing else. So Boks reads what is there rather than asking for something that does
// not exist.
//
// Nothing here prints a token, and nothing here prints a sentinel either. A sentinel is a
// fake by construction, but it reaches the guest through an environment variable and a
// credential file — never through a human's clipboard — so there is no reason for it to be
// on a terminal, and one fewer place for it to be mistaken for the real thing.
func newSecretImportCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import [flags] [NAME]",
		Short: "Import an OAuth credential an agent already has (Claude Code's subscription login)",
		Long: `Imports an existing OAuth credential — an access token, a refresh token and an expiry —
into the encrypted store, and mints the sentinels the guest will hold in its place.

Where it reads from, unless --from says otherwise:

  macOS      the Keychain item "` + secret.ClaudeCodeKeychainService + `", via the 'security' CLI
  elsewhere  ~/.claude/.credentials.json

--from - reads the credential document on standard input, which is the portable path and
works anywhere.

The value never leaves this machine. The sandbox receives sentinels shaped like real tokens;
the proxy substitutes the real access token on requests to the resource hosts, and refreshes
the pair on the host when it expires. Naming a host here means boks will terminate TLS for
it — see 'boks proxy --help'.`,
		Example: `  boks secret import claude-code
  boks secret import --from ~/.claude/.credentials.json claude-code
  cat creds.json | boks secret import --from - claude-code`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return usagef("at most one credential name")
			}
			return nil
		},
	}
	var (
		format    string
		from      string
		account   string
		hosts     []string
		tokenURL  string
		clientID  string
		envName   string
		filePath  string
		noFile    bool
		storePath string
	)
	cmd.Flags().StringVar(&format, "format", "claude-code", "credential format: "+strings.Join(secret.ProfileNames(), ", "))
	cmd.Flags().StringVar(&from, "from", "", "where to read it: 'keychain', '-' for stdin, or a file path")
	cmd.Flags().StringVar(&account, "account", "", "keychain account, when the item is not stored under your user")
	cmd.Flags().StringArrayVar(&hosts, "resource-host", nil, "override the hosts where the token is used (repeatable)")
	cmd.Flags().StringVar(&tokenURL, "token-url", "", "override the token endpoint URL")
	cmd.Flags().StringVar(&clientID, "client-id", "", "override the OAuth client id")
	cmd.Flags().StringVar(&envName, "env", "", "guest environment variable holding the access-token sentinel")
	cmd.Flags().StringVar(&filePath, "file", "", "guest path for the rendered credential file")
	cmd.Flags().BoolVar(&noFile, "no-file", false, "do not render a credential file into the guest")
	storeFlag(cmd, &storePath)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		profile, err := secret.Profile(format)
		if err != nil {
			return err
		}
		if len(hosts) > 0 {
			profile.ResourceHosts = hosts
		}
		if tokenURL != "" {
			endpoint, err := parseTokenURL(tokenURL)
			if err != nil {
				return err
			}
			profile.TokenEndpoint = endpoint
		}
		if clientID != "" {
			profile.ClientID = clientID
		}
		if envName != "" {
			profile.EnvName = envName
		}
		if filePath != "" {
			profile.FilePath = filePath
		}
		if noFile {
			profile.FilePath = ""
		}

		source, err := importSource(env, profile, from, account)
		if err != nil {
			return err
		}
		name := profile.Name
		if len(args) == 1 {
			name = args[0]
		}
		record, err := secret.Import(cmd.Context(), profile, source, name)
		if err != nil {
			return err
		}
		store, err := openSecretStore(storePath)
		if err != nil {
			return err
		}
		if err := store.SetOAuth(name, record); err != nil {
			return err
		}

		fmt.Fprintf(env.Stdout, "imported %q from %s into %s\n", name, source.Describe(), store.Path())
		fmt.Fprintf(env.Stdout, "  used on:        %s\n", strings.Join(record.ResourceHosts, ", "))
		fmt.Fprintf(env.Stdout, "  refreshed at:   %s%s  (on the host, never in the sandbox)\n",
			record.TokenHost, record.TokenPath)
		if record.ExpiresAt > 0 {
			fmt.Fprintf(env.Stdout, "  access expires: %s\n", time.UnixMilli(record.ExpiresAt).Format(time.RFC3339))
		}
		if record.EnvName != "" {
			fmt.Fprintf(env.Stdout, "  the guest gets a sentinel in $%s\n", record.EnvName)
		}
		if record.FilePath != "" {
			fmt.Fprintf(env.Stdout, "  and a credential file at %s, read-only\n", record.FilePath)
		}
		fmt.Fprintf(env.Stdout, "\nUse it with: boks run --oauth %s ...\n", name)
		return nil
	}
	return cmd
}

// importSource turns --from into somewhere to read.
func importSource(env Env, profile secret.OAuthProfile, from, account string) (secret.CredentialSource, error) {
	switch from {
	case "":
		if profile.DefaultSource == nil {
			return nil, fmt.Errorf("credential format %q has no default location; use --from", profile.Name)
		}
		source := profile.DefaultSource()
		if k, ok := source.(secret.KeychainSource); ok && account != "" {
			k.Account = account
			return k, nil
		}
		return source, nil
	case "-":
		return secret.ReaderSource{R: env.Stdin}, nil
	case "keychain":
		return secret.KeychainSource{Service: secret.ClaudeCodeKeychainService, Account: account}, nil
	default:
		return secret.FileSource{Path: from}, nil
	}
}

// parseTokenURL splits a token endpoint URL into the host that decides interception and the
// path that selects which request boks answers itself.
func parseTokenURL(raw string) (secret.Endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return secret.Endpoint{}, fmt.Errorf("--token-url %q: %w", raw, err)
	}
	if u.Scheme != "https" {
		return secret.Endpoint{}, fmt.Errorf("--token-url %q must be https: a refresh token is the most valuable thing boks holds", raw)
	}
	if u.Host == "" || u.Path == "" {
		return secret.Endpoint{}, fmt.Errorf("--token-url %q needs a host and a path", raw)
	}
	return secret.Endpoint{Host: u.Host, Path: u.Path}, nil
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
		entries, err := store.Entries()
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			fmt.Fprintf(env.Stdout, "no secrets stored in %s\n", store.Path())
			return nil
		}
		// Names and kinds only. There is no subcommand that prints a value, and there
		// should not be. The kind is worth showing because the two are used differently:
		// one is named by --inject, the other by --oauth.
		for _, e := range entries {
			if e.OAuth {
				fmt.Fprintf(env.Stdout, "%s\t(oauth)\n", e.Name)
				continue
			}
			fmt.Fprintln(env.Stdout, e.Name)
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
