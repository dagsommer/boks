package cli

import (
	"context"
	"strings"
	"testing"
)

// TestSecretLoginArmsACredentialAndPrintsWhatToRunNext: the acquisition path's entry point.
// It stores a credential-shaped hole, says what to do with it, and contacts nothing.
func TestSecretLoginArmsACredentialAndPrintsWhatToRunNext(t *testing.T) {
	store := secretStore(t)

	out, _, err := runCLI(t, "", "secret", "login", "claude-code")
	if err != nil {
		t.Fatalf("secret login: %v", err)
	}
	for _, want := range []string{
		"holds no token yet",
		"platform.claude.com/v1/oauth/token",
		"api.anthropic.com",
		"boks policy allow platform.claude.com:443",
		"boks run claude -- auth login",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not mention %q:\n%s", want, out)
		}
	}

	record, err := store.LookupOAuthRecord(context.Background(), "claude-code")
	if err != nil {
		t.Fatalf("LookupOAuthRecord: %v", err)
	}
	if !record.Pending || record.AccessToken != "" || record.RefreshToken != "" {
		t.Fatal("secret login stored something other than an armed, empty credential")
	}
	if record.AccessSentinel == "" || record.RefreshSentinel == "" {
		t.Error("the sentinels the guest will hold were not minted")
	}
	// A sentinel is a fake, and it still has no business on a terminal: this command's
	// output must not make a token-shaped string in a log look ordinary.
	for _, sentinel := range []string{record.AccessSentinel, record.RefreshSentinel} {
		if strings.Contains(out, sentinel) {
			t.Errorf("a sentinel was printed:\n%s", out)
		}
	}

	// And `secret ls` says the credential is not usable yet, rather than listing it beside
	// the ones that are.
	listed, _, err := runCLI(t, "", "secret", "ls")
	if err != nil {
		t.Fatalf("secret ls: %v", err)
	}
	if !strings.Contains(listed, "awaiting a login") {
		t.Errorf("secret ls does not say the credential is still empty:\n%s", listed)
	}
}

// TestSecretLoginRefusesToEmptyAWorkingLogin: arming clears the tokens, so doing it silently
// over a credential that works would log every sandbox out with no undo.
func TestSecretLoginRefusesToEmptyAWorkingLogin(t *testing.T) {
	store := secretStore(t)
	storeLogin(t, store, "claude-code", "api.anthropic.com")

	_, _, err := runCLI(t, "", "secret", "login", "claude-code")
	if err == nil {
		t.Fatal("arming over an existing credential was allowed")
	}
	for _, want := range []string{"would empty it", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}

	if _, _, err := runCLI(t, "", "secret", "login", "--force", "claude-code"); err != nil {
		t.Fatalf("secret login --force: %v", err)
	}
	record, err := store.LookupOAuthRecord(context.Background(), "claude-code")
	if err != nil {
		t.Fatalf("LookupOAuthRecord: %v", err)
	}
	if !record.Pending {
		t.Error("--force did not arm the credential")
	}
}

// TestSecretSetOAuthPointsAtBothAcquisitionPaths: the refusal of --oauth stands — boks still
// holds no client id — and it now names the route that works on a machine with no login on it.
func TestSecretSetOAuthPointsAtBothAcquisitionPaths(t *testing.T) {
	secretStore(t)

	_, _, err := runCLI(t, "", "secret", "set", "--oauth", "anthropic")
	if err == nil {
		t.Fatal("--oauth was accepted")
	}
	for _, want := range []string{
		"client id",
		"boks secret adopt claude-code",
		"boks secret login claude-code",
		"boks run claude -- auth login",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
}
