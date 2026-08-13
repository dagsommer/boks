package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

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
sandbox. The host proxy attaches them to requests for the hosts the credential names; the
guest holds a placeholder shaped like the real thing.

A credential stored under a service boks knows — %s —
needs no further configuration: boks already has that vendor's hosts, header, environment
variable and key shape, and every sandbox you run attaches it. 'boks secret services'
prints the list. Anything else is stored under a name of your own and attached by a
'boks run --inject' rule.

The file is encrypted with a passphrase taken from %s. Without an OS
keychain that is exactly as strong as the passphrase, and no stronger.

There is no recovery for a forgotten passphrase — that is what encryption means — so
'boks secret reset' deletes the file and everything in it, which is the only way out and
is spelled out wherever the store fails to decrypt.`,
			strings.Join(knownServices.Names(), ", "), secret.PassphraseEnv),
	}
	cmd.AddCommand(newSecretSetCommand(env), newSecretAdoptCommand(env), newSecretImportCommand(env),
		newSecretLsCommand(env), newSecretServicesCommand(env), newSecretRmCommand(env),
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

// newSecretAdoptCommand takes over a credential an agent on this machine already holds.
//
// It used to be called `import`, and the rename is not cosmetic. sbx's `secret import` reads
// **host environment variables** and offers to store what it finds; this reads a *stored OAuth
// credential* out of the Keychain or an agent's own file. Two commands, one verb, opposite
// jobs — and since the whole point of the naming here is that a habit formed in sbx works in
// boks, the one that had to move is ours. `import` now means what it means there.
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
func newSecretAdoptCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "adopt [flags] [NAME]",
		Short: "Adopt an OAuth credential an agent on this machine already has",
		Long: `Adopts an existing OAuth credential — an access token, a refresh token and an expiry —
into the encrypted store, and mints the sentinels the guest will hold in its place.

Where it reads from, unless --from says otherwise:

  macOS      the Keychain item "` + secret.ClaudeCodeKeychainService + `", via the 'security' CLI
  elsewhere  ~/.claude/.credentials.json

--from - reads the credential document on standard input, which is the portable path and
works anywhere.

The value never leaves this machine. The sandbox receives sentinels shaped like real tokens;
the proxy substitutes the real access token on requests to the resource hosts, and refreshes
the pair on the host when it expires. Naming a host here means boks will terminate TLS for
it — see 'boks proxy --help'.

Once adopted it is used by every sandbox you run, and it takes precedence over an API key
covering the same hosts.`,
		Example: `  boks secret adopt claude-code
  boks secret adopt --from ~/.claude/.credentials.json claude-code
  cat creds.json | boks secret adopt --from - claude-code`,
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

		source, err := adoptSource(env, profile, from, account)
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
			return explainSecretFailure(store.Path(), err)
		}

		fmt.Fprintf(env.Stdout, "adopted %q from %s into %s\n", name, source.Describe(), store.Path())
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
		// The allow rules are spelled out rather than left to be discovered. Naming a host
		// for a credential says where a token may go; it does not make the host reachable,
		// and the default preset does not allow these — so a run without them fails at the
		// network layer with no hint that a credential was ever involved.
		var allows strings.Builder
		for _, h := range append(append([]string{}, record.ResourceHosts...), record.TokenHost) {
			fmt.Fprintf(&allows, "  boks policy allow %s:443\n", h)
		}
		fmt.Fprintf(env.Stdout, "\nEvery sandbox you run now uses it. Make the hosts reachable:\n%s", allows.String())
		fmt.Fprint(env.Stdout, "\nThose rules are separate on purpose: a credential rule says where a token may go,\n"+
			"not what is reachable. Without them the default policy denies these hosts.\n")
		return nil
	}
	return cmd
}

// adoptSource turns --from into somewhere to read.
func adoptSource(env Env, profile secret.OAuthProfile, from, account string) (secret.CredentialSource, error) {
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
		Use:   "set [flags] SERVICE",
		Short: "Store a credential for a service, read from stdin or --value",
		Long: `Stores a credential under a name. Prefer stdin over --value: an argument is visible in the
process list and in your shell history.

If the name is a service boks knows, that is the whole configuration. Boks already has the
vendor's hosts, the header the key rides in, the environment variable the guest's own client
reads it from, and the shape a convincing placeholder has — so every sandbox you run
attaches it and no --inject is needed. 'boks secret services' lists them.

Any other name is stored just the same and attached by nothing until a 'boks run --inject'
rule says where it goes.`,
		Example: `  echo -n "$ANTHROPIC_API_KEY" | boks secret set anthropic
  echo -n "$GITHUB_TOKEN"     | boks secret set github
  echo -n "$KEY"              | boks secret set my-internal-api`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usagef("a service name is required; 'boks secret services' lists the ones boks knows")
			}
			return nil
		},
	}
	var value, path string
	var oauth bool
	cmd.Flags().StringVar(&value, "value", "", "the credential; omit to read it from stdin")
	cmd.Flags().BoolVar(&oauth, "oauth", false, "acquire the credential by logging in (see 'boks secret set --help')")
	storeFlag(cmd, &path)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if oauth {
			return errOAuthAcquisition(name)
		}

		// The name is judged before anything is read. A service boks knows but has no
		// rule for cannot be stored under that name, and finding that out *after* a
		// credential has been slurped off a pipe would leave the user retyping it.
		if service, ok := knownServices.Lookup(name); ok {
			if err := secret.RequireConfigured(service); err != nil {
				return err
			}
		}

		store, err := openSecretStore(path)
		if err != nil {
			return err
		}
		if err := refuseToShadowOAuth(store, name); err != nil {
			return err
		}

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
		if err := store.Set(name, secret.NewValue(raw)); err != nil {
			return explainSecretFailure(store.Path(), err)
		}
		fmt.Fprintf(env.Stdout, "stored %q in %s\n", name, store.Path())
		describeStoredService(env.Stdout, name)
		return nil
	}
	return cmd
}

// refuseToShadowOAuth stops an API key from being stored over a service that already has an
// OAuth credential.
//
// sbx does the same, and the reasoning is worth keeping: an OAuth credential is the one the
// user acquired by logging in, and it takes precedence at runtime. Storing a key over it
// would either replace a working login with a value that may be stale, or — if the store
// kept both — leave a key that is never used and no way to tell why. Skipping and saying so
// is the only outcome the user can act on.
func refuseToShadowOAuth(store *secret.FileStore, name string) error {
	entries, err := store.Entries()
	if err != nil {
		return explainSecretFailure(store.Path(), err)
	}
	for _, e := range entries {
		if e.Name != name || !e.OAuth {
			continue
		}
		return fmt.Errorf("%q already holds an OAuth credential, and an API key is not being stored over it.\n\n"+
			"An OAuth credential is the login you performed, it refreshes itself, and it takes\n"+
			"precedence over a key for the same hosts — so a key stored here would sit unused,\n"+
			"with nothing to tell you why.\n\n"+
			"If you mean to switch to a key, remove the login first:\n"+
			"  boks secret rm %s\n"+
			"Or store the key under a different name and point a --inject rule at it.", name, name)
	}
	return nil
}

// describeStoredService says what a stored credential will now do, so that the answer to
// "and how does the sandbox get it" is on the screen rather than in the documentation.
func describeStoredService(w io.Writer, name string) {
	service, ok := knownServices.Lookup(name)
	if !ok {
		fmt.Fprintf(w, "\nboks has no service called %q, so nothing attaches it on its own. Say where it\n"+
			"goes when you use it:\n\n"+
			"  boks run --inject '%s@api.example.com=Authorization:Bearer %%s' \\\n"+
			"           --guest-credential '%s=MY_API_KEY=placeholder' ...\n\n"+
			"Services boks knows: %s\n",
			name, name, name, strings.Join(knownServices.Names(), ", "))
		return
	}
	fmt.Fprintf(w, "\n  %s\n", service.Summary)
	for _, rule := range service.Inject {
		fmt.Fprintf(w, "  attached to:  %s\n", strings.Join(rule.Hosts, ", "))
		fmt.Fprintf(w, "                %s — %s\n", rule.Describe(), rule.Why)
	}
	if service.EnvName != "" {
		fmt.Fprintf(w, "  the guest gets a placeholder in $%s\n", service.EnvName)
	}
	fmt.Fprintf(w, "\nEvery sandbox you run now attaches it; no --inject is needed. Those hosts are also\n"+
		"the ones whose TLS boks terminates, and they still have to be reachable:\n\n")
	for _, spec := range service.AllowSpecs() {
		fmt.Fprintf(w, "  boks policy allow %s\n", spec)
	}
}

// errOAuthAcquisition answers `boks secret set NAME --oauth`.
//
// sbx has this flag and runs an OAuth flow from the host. Boks does not, and what follows is
// the reason rather than a promise: the missing piece is not an afternoon's work, it is a
// registration boks does not hold. A flag that opened a browser and did something
// approximately login-shaped would be worse than this message.
func errOAuthAcquisition(name string) error {
	return fmt.Errorf("--oauth is not implemented, and the reason is worth having in full.\n\n"+
		"Every flow that could acquire a token here — authorization code with PKCE, or the\n"+
		"device flow — begins by identifying *this program* to the vendor with a client id the\n"+
		"vendor issues to a registered application. Boks is registered with none of the\n"+
		"services it knows, so it has nothing to send. Of those it carries a rule for, only\n"+
		"GitHub and Google publish a flow a third-party CLI could drive at all, and both start\n"+
		"with that same client id; Anthropic, OpenAI, xAI, Groq, Mistral, Nebius and OpenRouter\n"+
		"publish no device flow for third parties.\n\n"+
		"Reusing another product's client id — so the vendor is told the login is Claude Code's,\n"+
		"or gh's — would work and is not something boks will do on your behalf.\n\n"+
		"What boks does instead is take a login you already performed:\n\n"+
		"  boks secret adopt claude-code\n\n"+
		"which reads Claude Code's own credential from the macOS Keychain, or from\n"+
		"~/.claude/.credentials.json, and stores the token pair without it ever entering a\n"+
		"sandbox. That covers the case --oauth exists for on a machine you have logged in on;\n"+
		"it does nothing for a fresh one, and nothing here pretends otherwise.\n\n"+
		"To store an API key for %q instead, drop the flag.", name)
}

// newSecretImportCommand offers the credentials already in this shell's environment.
//
// This is sbx's `import`, and it is here under sbx's name because the registry makes it
// possible: every configured service knows which environment variable its vendor's tooling
// reads, so "which of the keys already in this shell does boks know what to do with" is a
// question the registry answers directly.
//
// It is the only place in Boks that prints any part of a credential, and it prints four
// characters. That is a deliberate exception with a job: a shell can hold two keys for the
// same vendor, and "store the one ending 4f2a" is the only way a person can tell which one
// they are about to keep. Anything longer would be a leak into a terminal's scrollback.
func newSecretImportCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import [flags] [SERVICE...]",
		Short: "Store credentials found in this shell's environment, with a prompt for each",
		Long: `Looks at the environment variables this shell already has — ANTHROPIC_API_KEY,
GITHUB_TOKEN, OPENAI_API_KEY and the rest — and offers to store the ones boks knows a
service for. Name services to consider only those.

Each is shown with the last four characters of its value, which is enough to tell two keys
for the same vendor apart and is the only fragment of any credential boks ever prints.

Nothing is stored without a "yes", unless --all is given. A service that already has a
credential is skipped unless --force is given, and one that already has an OAuth credential
is skipped whatever is given: a login takes precedence over a key, so storing one over it
would leave a credential that is never used.`,
		Example: `  boks secret import                 # walk everything found, with a prompt each
  boks secret import anthropic github
  boks secret import --all --dry-run`,
		Args: cobra.ArbitraryArgs,
	}
	var (
		all       bool
		force     bool
		dryRun    bool
		storePath string
	)
	cmd.Flags().BoolVar(&all, "all", false, "store everything found without prompting")
	cmd.Flags().BoolVar(&force, "force", false, "replace a credential that is already stored")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "say what would be stored and store nothing")
	storeFlag(cmd, &storePath)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		// The old name for `adopt` was `import`. Someone with the habit gets an answer
		// rather than an argument error, since `claude-code` is a credential format and
		// never a service name.
		for _, arg := range args {
			if _, err := secret.Profile(arg); err == nil {
				return fmt.Errorf("%q is a credential format, not a service: 'boks secret import' now reads\n"+
					"environment variables, as sbx's does. To adopt a stored OAuth credential:\n\n"+
					"  boks secret adopt %s", arg, arg)
			}
		}
		found := environmentCredentials(args)
		if len(found) == 0 {
			fmt.Fprintf(env.Stdout, "no credentials found in the environment for %s.\n",
				strings.Join(configuredServiceNames(), ", "))
			return nil
		}

		store, err := openSecretStore(storePath)
		if err != nil {
			return err
		}
		entries, err := store.Entries()
		if err != nil {
			return explainSecretFailure(store.Path(), err)
		}
		held := map[string]secret.Entry{}
		for _, e := range entries {
			held[e.Name] = e
		}

		interactive := !all && !dryRun
		if interactive && !isTerminal(env.Stdin) {
			return errors.New("nothing to prompt with: standard input is not a terminal.\n" +
				"Use --all to store everything found, or --dry-run to see what that would be.")
		}
		reader := bufio.NewReader(env.Stdin)

		stored := 0
		for _, c := range found {
			existing, have := held[c.service.Name]
			switch {
			case have && existing.OAuth:
				fmt.Fprintf(env.Stdout, "skip %-11s $%s is set, but %q already holds an OAuth credential.\n"+
					"                 A login takes precedence over a key; 'boks secret rm %s' first if you\n"+
					"                 mean to switch.\n",
					c.service.Name, c.env, c.service.Name, c.service.Name)
				continue
			case have && !force:
				fmt.Fprintf(env.Stdout, "skip %-11s already stored (--force to replace it with $%s)\n",
					c.service.Name, c.env)
				continue
			}
			if dryRun {
				fmt.Fprintf(env.Stdout, "would store %s from $%s (%s)\n", c.service.Name, c.env, c.preview())
				continue
			}
			if interactive {
				yes, err := confirmDefaultYes(env.Stdout, reader,
					fmt.Sprintf("Store $%s (%s) as %q?", c.env, c.preview(), c.service.Name))
				if err != nil {
					return err
				}
				if !yes {
					fmt.Fprintf(env.Stdout, "skip %s\n", c.service.Name)
					continue
				}
			}
			if err := store.Set(c.service.Name, secret.NewValue(c.value)); err != nil {
				return explainSecretFailure(store.Path(), err)
			}
			fmt.Fprintf(env.Stdout, "stored %q from $%s\n", c.service.Name, c.env)
			stored++
		}
		if dryRun {
			fmt.Fprintf(env.Stdout, "\n--dry-run: nothing was stored.\n")
			return nil
		}
		if stored > 0 {
			fmt.Fprintf(env.Stdout, "\n%d credential(s) in %s. Every sandbox you run attaches them;\n"+
				"'boks secret ls' shows what goes where.\n", stored, store.Path())
		}
		return nil
	}
	return cmd
}

// environmentCredential is one credential found in this shell's environment.
type environmentCredential struct {
	service secret.Service
	env     string
	value   string
}

// preview is the last four characters, and never more.
//
// Four is the number a bank statement uses, for the same reason: it distinguishes two
// credentials a person holds without being enough to reconstruct either. A short value gets
// no preview at all, because four characters of a twelve-character secret is a third of it.
func (c environmentCredential) preview() string {
	if len(c.value) < 12 {
		return "too short to preview"
	}
	return "…" + c.value[len(c.value)-4:]
}

// environmentCredentials finds the credentials this shell holds for services boks knows.
func environmentCredentials(only []string) []environmentCredential {
	want := map[string]bool{}
	for _, name := range only {
		want[name] = true
	}
	var out []environmentCredential
	for _, s := range knownServices.All() {
		if !s.Configured() || s.EnvName == "" {
			continue
		}
		if len(want) > 0 && !want[s.Name] {
			continue
		}
		value := strings.TrimSpace(os.Getenv(s.EnvName))
		if value == "" {
			continue
		}
		out = append(out, environmentCredential{service: s, env: s.EnvName, value: value})
	}
	return out
}

func configuredServiceNames() []string {
	var out []string
	for _, s := range knownServices.All() {
		if s.Configured() && s.EnvName != "" {
			out = append(out, s.EnvName)
		}
	}
	sort.Strings(out)
	return out
}

// confirmDefaultYes asks a yes/no question, defaulting to yes on a bare newline.
//
// It differs from confirm() in policystore.go on both counts, and both are deliberate. The
// default is yes because the question is "keep this credential you already have", where the
// cost of a stray Enter is one stored key rather than a deleted rule. And it takes the reader
// rather than the Env because it is asked in a loop: a fresh bufio.Reader per question would
// swallow whatever the user had already typed ahead.
func confirmDefaultYes(w io.Writer, r *bufio.Reader, question string) (bool, error) {
	fmt.Fprintf(w, "%s [Y/n] ", question)
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("reading your answer: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true, nil
	}
	return false, nil
}

func newSecretLsCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls [flags]",
		Short: "List the stored credentials and where each one goes",
		// Listing needs the passphrase, and that is a property of the format rather
		// than an oversight: the store is one AES-GCM envelope over a JSON map, so the
		// names live inside the ciphertext with the values. Keeping them outside would
		// mean publishing, in cleartext next to the file, which services this machine
		// holds credentials for — which is exactly the metadata an attacker who cannot
		// decrypt the file would like to have.
		Long: `Lists the stored credentials, never their values: the name, whether it is an API key or
an OAuth login, and the hosts it is attached to.

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
		entries, err := store.Entries()
		if err != nil {
			return explainSecretFailure(store.Path(), err)
		}
		if len(entries) == 0 {
			fmt.Fprintf(env.Stdout, "no secrets stored in %s\n", store.Path())
			return nil
		}
		// Names, kinds and destinations. There is no subcommand that prints a value,
		// and there should not be. The kind is worth showing because the two are used
		// differently, and because an OAuth login takes precedence over a key.
		w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tKIND\tATTACHED TO")
		for _, e := range entries {
			kind, where := "key", "nothing until a --inject rule names it"
			switch {
			case e.OAuth:
				kind = "oauth"
				record, err := store.LookupOAuthRecord(cmd.Context(), e.Name)
				if err != nil {
					where = "unreadable: " + err.Error()
					break
				}
				where = strings.Join(record.ResourceHosts, ", ") +
					"  (refreshed at " + record.TokenHost + ", on the host)"
			default:
				if service, ok := knownServices.Lookup(e.Name); ok && service.Configured() {
					where = strings.Join(service.Hosts(), ", ")
					if service.EnvName != "" {
						where += "  ($" + service.EnvName + " in the guest)"
					}
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", e.Name, kind, where)
		}
		return w.Flush()
	}
	return cmd
}

// newSecretServicesCommand prints the registry, including the rows that are empty.
//
// The empty rows are the point of printing it at all. "boks knows this name and has no rule
// for it" is a fact a user needs before they go looking for why their key did nothing, and
// it is not discoverable from a list that shows only what works.
func newSecretServicesCommand(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "services",
		Short: "List the services boks knows, and what it knows about each",
		Long: `Lists the services a credential can be stored under by name alone.

A service with no rule is one whose vendor documentation did not name both the host its
credential is sent to and the header that carries it. Boks does not guess at either: a
guessed rule sends the wrong header, or the right header to the wrong host, and either way
the placeholder in the guest reaches the real API instead of your credential. Store such a
credential under a name of your own and attach it with 'boks run --inject'.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SERVICE\tGUEST VARIABLE\tATTACHED TO")
			for _, s := range knownServices.All() {
				if !s.Configured() {
					fmt.Fprintf(w, "%s\t-\tno rule yet — see 'boks secret set %s'\n", s.Name, s.Name)
					continue
				}
				where := strings.Join(s.Hosts(), ", ")
				if s.HasOAuth() {
					where += "  (oauth: " + s.TokenEndpoint.Host + ")"
				}
				variable := s.EnvName
				if variable == "" {
					variable = "-"
				}
				fmt.Fprintf(w, "%s\t$%s\t%s\n", s.Name, variable, where)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(env.Stdout, "\nStore one with:  echo -n \"$ANTHROPIC_API_KEY\" | boks secret set anthropic\n")
			return nil
		},
	}
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
