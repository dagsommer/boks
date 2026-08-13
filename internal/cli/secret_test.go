package cli

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

// theKey is a canary. Nothing in this file may print it, and several assertions exist only to
// say so.
const theKey = "sk-ant-api03-CANARY-must-never-be-printed-0000"

// secretStore prepares an isolated store and returns a handle on it.
func secretStore(t *testing.T) *secret.FileStore {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BOKS_STATE_DIR", dir)
	t.Setenv(secret.PassphraseEnv, "test-passphrase")
	store, err := secret.OpenFile(secret.DefaultPath(policy.StateDir()), []byte("test-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// The whole point of the registry, end to end: a name and a value, and the sandbox knows what
// to do with them.
func TestSecretSetForAKnownServiceNeedsNoInject(t *testing.T) {
	store := secretStore(t)

	out, _, err := runCLI(t, theKey+"\n", "secret", "set", "anthropic")
	if err != nil {
		t.Fatalf("secret set anthropic: %v", err)
	}
	if strings.Contains(out, theKey) {
		t.Fatalf("secret set printed the credential:\n%s", out)
	}
	// What was stored is only half of it: the user has to be told what will happen with
	// it, including the interception and the allow rule it does not grant.
	for _, want := range []string{
		"api.anthropic.com", "x-api-key", "ANTHROPIC_API_KEY",
		"no --inject is needed", "boks policy allow api.anthropic.com:443",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("secret set does not mention %q:\n%s", want, out)
		}
	}

	// And a run picks it up with no flags at all.
	var flags policyFlags
	plan, err := flags.planCredentials(store)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(plan.inject, "anthropic@api.anthropic.com=x-api-key") {
		t.Fatalf("the run did not adopt the stored service: %v", plan.inject)
	}
	if len(plan.guest) != 1 || !strings.HasPrefix(plan.guest[0], "anthropic=ANTHROPIC_API_KEY=sk-ant-api03-") {
		t.Fatalf("guest credential = %v", plan.guest)
	}
	if strings.Contains(strings.Join(plan.guest, " "), theKey) {
		t.Fatal("the guest was given the real value")
	}
	if !slices.Contains(plan.adopted, "anthropic") {
		t.Errorf("the plan does not report what it adopted: %v", plan.adopted)
	}

	// The credentials the sandbox ends up with resolve to the documented rule, and the
	// hosts it will decrypt are exactly the service's.
	credentials, err := secret.ParseCredentials(plan.inject, plan.guest)
	if err != nil {
		t.Fatal(err)
	}
	if hosts := secret.CredentialHosts(credentials); len(hosts) != 1 || hosts[0] != "api.anthropic.com" {
		t.Errorf("interception hosts = %v", hosts)
	}
}

// The registry is a default, never a ceiling: a --inject rule naming the same service replaces
// it outright rather than being added to it.
func TestInjectOverridesTheRegistry(t *testing.T) {
	store := secretStore(t)
	if err := store.Set("anthropic", secret.NewValue(theKey)); err != nil {
		t.Fatal(err)
	}

	flags := policyFlags{
		inject: []string{"anthropic@api.internal.example.com=Authorization:token %s"},
		guest:  []string{"anthropic=ANTHROPIC_API_KEY=my-own-placeholder"},
	}
	plan, err := flags.planCredentials(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.inject) != 1 || plan.inject[0] != flags.inject[0] {
		t.Fatalf("inject = %v, want only the user's own rule", plan.inject)
	}
	if len(plan.guest) != 1 || plan.guest[0] != flags.guest[0] {
		t.Fatalf("guest = %v, want only the user's own", plan.guest)
	}
	if len(plan.adopted) != 0 {
		t.Errorf("the registry was applied on top of an explicit rule: %v", plan.adopted)
	}
	credentials, err := secret.ParseCredentials(plan.inject, plan.guest)
	if err != nil {
		t.Fatal(err)
	}
	if hosts := secret.CredentialHosts(credentials); len(hosts) != 1 || hosts[0] != "api.internal.example.com" {
		t.Errorf("the registry's host survived the override: %v", hosts)
	}
}

// --no-secrets is the only way to run a sandbox that does not carry a credential you have
// stored, so it has to be exact rather than approximate.
func TestNoSecretsLeavesTheStoreOut(t *testing.T) {
	store := secretStore(t)
	if err := store.Set("anthropic", secret.NewValue(theKey)); err != nil {
		t.Fatal(err)
	}
	flags := policyFlags{noSecrets: true}
	plan, err := flags.planCredentials(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.inject) != 0 || len(plan.guest) != 0 {
		t.Fatalf("--no-secrets still attached %v / %v", plan.inject, plan.guest)
	}
}

// A credential stored under a name boks does not know is inert until a --inject rule names it.
// It must not be guessed at, and it must not vanish either.
func TestAnUnknownNameIsStoredAndNotAttached(t *testing.T) {
	store := secretStore(t)

	out, _, err := runCLI(t, theKey+"\n", "secret", "set", "my-internal-api")
	if err != nil {
		t.Fatalf("secret set: %v", err)
	}
	if strings.Contains(out, theKey) {
		t.Fatal("secret set printed the credential")
	}
	for _, want := range []string{"no service called", "--inject", "anthropic"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not mention %q:\n%s", want, out)
		}
	}
	var flags policyFlags
	plan, err := flags.planCredentials(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.inject) != 0 {
		t.Errorf("an unknown name was attached anyway: %v", plan.inject)
	}
}

// A name boks knows and has no rule for is refused, and the refusal has to say what is
// missing. Storing it would leave a credential nothing ever attaches, which is the failure
// mode with no symptom.
func TestUnconfiguredServiceIsRefusedByName(t *testing.T) {
	secretStore(t)

	_, _, err := runCLI(t, theKey+"\n", "secret", "set", "cursor")
	if err == nil {
		t.Fatal("secret set cursor succeeded")
	}
	for _, want := range []string{"knows the service", "host", "header", "boks secret set my-cursor", "--inject"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), theKey) {
		t.Fatal("the refusal printed the credential")
	}
	// Nothing was stored, so `ls` has nothing to show.
	out, _, lsErr := runCLI(t, "", "secret", "ls")
	if lsErr != nil {
		t.Fatal(lsErr)
	}
	if strings.Contains(out, "cursor") {
		t.Errorf("a refused credential was stored anyway:\n%s", out)
	}
}

// sbx skips rather than shadows, and so does this: an OAuth credential is the login the user
// performed, it takes precedence at runtime, and a key stored over it would sit unused with
// nothing to say why.
func TestAnAPIKeyDoesNotShadowALogin(t *testing.T) {
	store := secretStore(t)
	storeLogin(t, store, "anthropic", "api.anthropic.com")

	_, _, err := runCLI(t, theKey+"\n", "secret", "set", "anthropic")
	if err == nil {
		t.Fatal("an API key was stored over an OAuth credential")
	}
	for _, want := range []string{"OAuth credential", "precedence", "boks secret rm anthropic"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), theKey) {
		t.Fatal("the refusal printed the credential")
	}
	// And the login is untouched: the store still holds an OAuth record under that name.
	entries, err := store.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].OAuth {
		t.Fatalf("the login was replaced: %+v", entries)
	}
}

// The runtime half of the same rule: a login and a key that reach the same host are not both
// attached, and the run says which one it dropped.
func TestALoginTakesPrecedenceOverAStoredKeyAtRunTime(t *testing.T) {
	store := secretStore(t)
	// Two different names, which is the usual shape: a login adopted as `claude-code`
	// and a key stored as `anthropic`, both ending at api.anthropic.com.
	if err := store.Set("anthropic", secret.NewValue(theKey)); err != nil {
		t.Fatal(err)
	}
	storeLogin(t, store, "claude-code", "api.anthropic.com")

	var flags policyFlags
	plan, err := flags.planCredentials(store)
	if err != nil {
		t.Fatal(err)
	}
	records, err := oauthRecords(context.Background(), store, plan.oauth)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.preferOAuth(records); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(plan.shadowed, "anthropic") {
		t.Fatalf("the key was not dropped in favour of the login: %+v", plan)
	}
	for _, spec := range plan.inject {
		if strings.HasPrefix(spec, "anthropic@") {
			t.Errorf("the shadowed key is still attached: %q", spec)
		}
	}
	for _, spec := range plan.guest {
		if strings.HasPrefix(spec, "anthropic=") {
			t.Errorf("the shadowed key still reaches the guest: %q", spec)
		}
	}
	// Dropping it silently would be the very failure the rule exists to prevent.
	var said strings.Builder
	plan.describe(&said)
	for _, want := range []string{"anthropic", "OAuth", "boks secret rm"} {
		if !strings.Contains(said.String(), want) {
			t.Errorf("the run does not say what it dropped (%q):\n%s", want, said.String())
		}
	}
}

// sbx's `import`, under sbx's name: the keys already in this shell, offered one at a time.
func TestImportWalksTheEnvironment(t *testing.T) {
	secretStore(t)
	t.Setenv("ANTHROPIC_API_KEY", theKey)
	t.Setenv("OPENAI_API_KEY", "sk-openai-canary-value-here")
	t.Setenv("CURSOR_API_KEY", "crsr_not_a_configured_service")

	out, _, err := runCLI(t, "", "secret", "import", "--all")
	if err != nil {
		t.Fatalf("secret import --all: %v", err)
	}
	for _, want := range []string{"anthropic", "openai"} {
		if !strings.Contains(out, want) {
			t.Errorf("import did not take %q:\n%s", want, out)
		}
	}
	// cursor has no rule, so there is nowhere for its key to go and nothing to offer.
	if strings.Contains(out, "cursor") {
		t.Errorf("import offered a service with no rule:\n%s", out)
	}
	if strings.Contains(out, theKey) || strings.Contains(out, "sk-openai-canary-value-here") {
		t.Fatalf("import printed a credential:\n%s", out)
	}

	out, _, err = runCLI(t, "", "secret", "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"anthropic", "openai", "api.anthropic.com", "api.openai.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("secret ls does not show %q:\n%s", want, out)
		}
	}
}

// The last four characters are the only fragment of a credential boks prints anywhere. This
// pins both halves of that: that it is four, and that it is not more.
func TestImportPreviewsFourCharactersAndNoMore(t *testing.T) {
	secretStore(t)
	t.Setenv("ANTHROPIC_API_KEY", theKey)

	out, _, err := runCLI(t, "", "secret", "import", "--dry-run")
	if err != nil {
		t.Fatalf("secret import --dry-run: %v", err)
	}
	if !strings.Contains(out, "…"+theKey[len(theKey)-4:]) {
		t.Errorf("no last-four preview:\n%s", out)
	}
	if strings.Contains(out, theKey[:len(theKey)-4]) || strings.Contains(out, theKey) {
		t.Fatalf("the preview showed more than four characters:\n%s", out)
	}
	if !strings.Contains(out, "nothing was stored") {
		t.Errorf("--dry-run did not say it stored nothing:\n%s", out)
	}
	// And it really stored nothing.
	out, _, err = runCLI(t, "", "secret", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no secrets stored") {
		t.Errorf("--dry-run stored something:\n%s", out)
	}
}

// A shell holding two keys for one vendor is exactly why the preview exists; a value too
// short for four characters to be a hint rather than a leak gets none.
func TestImportRefusesToPreviewAShortValue(t *testing.T) {
	secretStore(t)
	t.Setenv("GROQ_API_KEY", "gsk_short")

	out, _, err := runCLI(t, "", "secret", "import", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "…") {
		t.Errorf("a short value was previewed:\n%s", out)
	}
	if !strings.Contains(out, "too short to preview") {
		t.Errorf("the refusal to preview is not explained:\n%s", out)
	}
}

// A credential already stored is skipped, and one that is a login is skipped whatever flags
// are given — the same rule `set` enforces, applied in a loop.
func TestImportSkipsWhatIsAlreadyThere(t *testing.T) {
	store := secretStore(t)
	if err := store.Set("openai", secret.NewValue("an-older-key")); err != nil {
		t.Fatal(err)
	}
	storeLogin(t, store, "anthropic", "api.anthropic.com")
	t.Setenv("ANTHROPIC_API_KEY", theKey)
	t.Setenv("OPENAI_API_KEY", "a-newer-key")

	out, _, err := runCLI(t, "", "secret", "import", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "already stored") || !strings.Contains(out, "--force") {
		t.Errorf("an existing credential was not reported as skipped:\n%s", out)
	}
	if !strings.Contains(out, "already holds an OAuth credential") {
		t.Errorf("a login was not protected:\n%s", out)
	}
	entries, err := store.Entries()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == "anthropic" && !e.OAuth {
			t.Fatal("the login was replaced by a key")
		}
	}

	// --force replaces the key and still leaves the login alone.
	if _, _, err := runCLI(t, "", "secret", "import", "--all", "--force"); err != nil {
		t.Fatal(err)
	}
	value, err := store.Lookup(context.Background(), "openai")
	if err != nil {
		t.Fatal(err)
	}
	if value.Reveal() != "a-newer-key" {
		t.Error("--force did not replace the older key")
	}
	entries, err = store.Entries()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == "anthropic" && !e.OAuth {
			t.Fatal("--force replaced the login")
		}
	}
}

// The old spelling of `adopt` was `import`, and `import` now does something else entirely.
// Someone with the habit gets an answer rather than a puzzle.
func TestImportPointsAtAdoptForACredentialFormat(t *testing.T) {
	secretStore(t)
	_, _, err := runCLI(t, "", "secret", "import", "claude-code")
	if err == nil {
		t.Fatal("secret import claude-code was accepted")
	}
	if !strings.Contains(err.Error(), "boks secret adopt claude-code") {
		t.Errorf("the error does not point at adopt:\n%v", err)
	}
}

// --oauth exists because sbx has it. It must say precisely what is missing rather than
// pretending, and it must point at the thing boks does do instead.
func TestOAuthAcquisitionSaysWhatIsMissing(t *testing.T) {
	secretStore(t)
	_, _, err := runCLI(t, "", "secret", "set", "openai", "--oauth")
	if err == nil {
		t.Fatal("--oauth pretended to work")
	}
	for _, want := range []string{"client id", "registered", "boks secret adopt claude-code"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the explanation does not mention %q:\n%v", want, err)
		}
	}
}

// `secret services` has to show the empty rows, because "boks knows this name and has no rule
// for it" is the fact a user needs before they go looking for why nothing happened.
func TestSecretServicesShowsTheEmptyRows(t *testing.T) {
	out, _, err := runCLI(t, "", "secret", "services")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"anthropic", "$ANTHROPIC_API_KEY", "api.anthropic.com", "console.anthropic.com",
		"cursor", "droid", "no rule yet",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("secret services does not show %q:\n%s", want, out)
		}
	}
}

// Every surface a stored credential passes through on its way to a run, checked for the value
// itself. The registry adds placeholders, hosts and headers; none of them is a value, and
// none of them may carry one.
func TestNothingAboutAServiceCredentialLeaksTheValue(t *testing.T) {
	store := secretStore(t)

	out, _, err := runCLI(t, theKey+"\n", "secret", "set", "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	surfaces := []string{out}

	ls, _, err := runCLI(t, "", "secret", "ls")
	if err != nil {
		t.Fatal(err)
	}
	surfaces = append(surfaces, ls)

	var flags policyFlags
	plan, err := flags.planCredentials(store)
	if err != nil {
		t.Fatal(err)
	}
	var said strings.Builder
	plan.describe(&said)
	surfaces = append(surfaces, said.String(),
		strings.Join(plan.inject, " "), strings.Join(plan.guest, " "), strings.Join(plan.adopted, " "))

	// A failure path too: the credential is gone from the store, and the run has to
	// complain about it without quoting anything.
	if err := store.Delete("anthropic"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Lookup(context.Background(), "anthropic"); err != nil {
		surfaces = append(surfaces, err.Error())
	}

	for _, s := range surfaces {
		if strings.Contains(s, theKey) {
			t.Errorf("a credential value appeared in %q", s)
		}
	}
}

// A store that will not decrypt is a warning and no credentials for a run that asked for
// none, and an error for one that did. Failing every `boks run` because a passphrase is wrong
// would be the wrong trade; doing it quietly would be worse.
func TestAnUnreadableStoreDoesNotFailARunThatAskedForNothing(t *testing.T) {
	store := secretStore(t)
	if err := store.Set("anthropic", secret.NewValue(theKey)); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secret.PassphraseEnv, "the-wrong-one")

	var stderr strings.Builder
	var flags policyFlags
	plan, _, err := flags.resolveCredentials(context.Background(), &stderr)
	if err != nil {
		t.Fatalf("a run with no credential flags failed over the store: %v", err)
	}
	if len(plan.inject) != 0 {
		t.Errorf("credentials were attached from a store that did not decrypt: %v", plan.inject)
	}
	if !strings.Contains(stderr.String(), "warning") {
		t.Errorf("the failure was silent:\n%s", stderr.String())
	}

	// A run that named a credential still fails, with the way out.
	flags = policyFlags{inject: []string{"anthropic@api.anthropic.com=x-api-key"}}
	if _, _, err := flags.resolveCredentials(context.Background(), &stderr); err == nil {
		t.Error("a run that named a credential succeeded without one")
	} else if !strings.Contains(err.Error(), "boks secret reset --force") {
		t.Errorf("the failure gives no way out:\n%v", err)
	}
}

// With no passphrase at all there is nothing in the store that could be attached, so a run
// with no credential flags proceeds in silence rather than failing.
func TestNoPassphraseMeansNoStoredCredentials(t *testing.T) {
	t.Setenv("BOKS_STATE_DIR", t.TempDir())
	t.Setenv(secret.PassphraseEnv, "")

	var stderr strings.Builder
	var flags policyFlags
	plan, _, err := flags.resolveCredentials(context.Background(), &stderr)
	if err != nil {
		t.Fatalf("a run without a passphrase failed: %v", err)
	}
	if len(plan.inject) != 0 || stderr.Len() != 0 {
		t.Errorf("plan = %v, stderr = %q", plan.inject, stderr.String())
	}

	// Naming one still fails, and says which variable is missing.
	flags = policyFlags{inject: []string{"x@api.example.com=bearer"}}
	if _, _, err := flags.resolveCredentials(context.Background(), &stderr); err == nil {
		t.Fatal("a credential rule was accepted with no store to read it from")
	} else if !strings.Contains(err.Error(), secret.PassphraseEnv) {
		t.Errorf("the failure does not name the passphrase variable:\n%v", err)
	}
}

// storeLogin puts an OAuth record in the store, for the precedence and shadowing tests. The
// tokens are obvious fakes; none of them is asserted on, and none is printed.
func storeLogin(t *testing.T, store *secret.FileStore, name, host string) {
	t.Helper()
	profile, err := secret.Profile("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	profile.ResourceHosts = []string{host}
	record, err := profile.Record(name, secret.ImportedTokens{
		Access:  "sk-ant-oat01-" + strings.Repeat("a", 90),
		Refresh: "sk-ant-ort01-" + strings.Repeat("b", 90),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetOAuth(name, record); err != nil {
		t.Fatal(err)
	}
}

// isTerminal is false for the readers these tests use, so the interactive path refuses rather
// than blocking. That refusal is itself the contract for a scripted run.
func TestImportRefusesToPromptWithoutATerminal(t *testing.T) {
	secretStore(t)
	t.Setenv("ANTHROPIC_API_KEY", theKey)
	if _, ok := os.LookupEnv("ANTHROPIC_API_KEY"); !ok {
		t.Fatal("the environment was not set")
	}
	_, _, err := runCLI(t, "", "secret", "import")
	if err == nil {
		t.Fatal("import prompted into a pipe")
	}
	for _, want := range []string{"not a terminal", "--all", "--dry-run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
}
