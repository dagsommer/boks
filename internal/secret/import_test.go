package secret

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// claudeCodeDocumentJSON is the document Claude Code writes: the macOS Keychain item
// "Claude Code-credentials" holds exactly this, and ~/.claude/.credentials.json is the same
// bytes in a file. Boks does not invent a format, so the importer is tested against the one
// the agent actually produces.
const claudeCodeDocumentJSON = `{"claudeAiOauth":{"accessToken":"` + accessCanary +
	`","refreshToken":"` + refreshCanary +
	`","expiresAt":1786000000000,"scopes":["user:inference","user:profile"],"subscriptionType":"max"}}`

func claudeProfile(t *testing.T) OAuthProfile {
	t.Helper()
	p, err := Profile("claude-code")
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	return p
}

// TestImportFromFile is the path this machine can actually execute, and it is deliberately
// the same code the Keychain path runs once the bytes are in hand: only the CredentialSource
// differs.
func TestImportFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(path, []byte(claudeCodeDocumentJSON), 0o600); err != nil {
		t.Fatalf("writing the document: %v", err)
	}

	record, err := Import(context.Background(), claudeProfile(t), FileSource{Path: path}, "claude-code")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	if record.AccessToken != accessCanary || record.RefreshToken != refreshCanary {
		t.Error("the token pair did not survive the import")
	}
	if got := time.UnixMilli(record.ExpiresAt); got.Unix() != 1786000000 {
		t.Errorf("expiry = %s, want the document's own", got)
	}
	if record.Subscription != "max" || len(record.Scopes) != 2 {
		t.Errorf("the non-secret metadata was lost: %+v", record.Scopes)
	}
	if record.TokenHost != "console.anthropic.com" || record.TokenPath != "/v1/oauth/token" {
		t.Errorf("token endpoint = %s%s", record.TokenHost, record.TokenPath)
	}
	if len(record.ResourceHosts) != 1 || record.ResourceHosts[0] != "api.anthropic.com" {
		t.Errorf("resource hosts = %v", record.ResourceHosts)
	}
	if record.EnvName != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Errorf("env = %q; the agent reads its token from that variable", record.EnvName)
	}
	// The sentinels must look like what they stand for, and must not be the real thing.
	if !strings.HasPrefix(record.AccessSentinel, "sk-ant-oat01-") {
		t.Errorf("access sentinel %q has the wrong shape", record.AccessSentinel)
	}
	if !strings.HasPrefix(record.RefreshSentinel, "sk-ant-ort01-") {
		t.Errorf("refresh sentinel %q has the wrong shape", record.RefreshSentinel)
	}
	if record.AccessSentinel == record.AccessToken || record.RefreshSentinel == record.RefreshToken {
		t.Fatal("a sentinel equals the real token")
	}
	if err := record.Validate(); err != nil {
		t.Errorf("the imported record is not usable: %v", err)
	}
}

// TestImportFromStdin is the portable path, which works on every platform including the ones
// with no keychain at all.
func TestImportFromStdin(t *testing.T) {
	source := ReaderSource{R: strings.NewReader(claudeCodeDocumentJSON)}
	record, err := Import(context.Background(), claudeProfile(t), source, "subscription")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if record.Service != "subscription" {
		t.Errorf("service = %q", record.Service)
	}
	// The sentinel is derived from the name, so two credentials cannot collide.
	other, err := Import(context.Background(), claudeProfile(t), ReaderSource{R: strings.NewReader(claudeCodeDocumentJSON)}, "work")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if other.AccessSentinel == record.AccessSentinel {
		t.Error("two credentials were given the same sentinel")
	}
}

// TestImportRejectsWhatIsNotACredential, without ever quoting the document back — a
// malformed credential file is still a file full of tokens.
func TestImportRejectsWhatIsNotACredential(t *testing.T) {
	cases := map[string]string{
		"empty":          "",
		"not json":       "this is not json " + accessCanary,
		"wrong shape":    `{"someOtherThing":{"accessToken":"` + accessCanary + `"}}`,
		"no accessToken": `{"claudeAiOauth":{"refreshToken":"` + refreshCanary + `"}}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Import(context.Background(), claudeProfile(t), ReaderSource{R: strings.NewReader(doc)}, "x")
			if err == nil {
				t.Fatal("accepted something that is not a credential")
			}
			assertNoCanary(t, err.Error())
		})
	}
}

// TestKeychainSourceOffDarwin covers the branch this machine can reach. The read itself is
// the one piece of this feature that has never run: see the comment on KeychainSource.
func TestKeychainSourceOffDarwin(t *testing.T) {
	source := KeychainSource{Service: ClaudeCodeKeychainService}
	if !strings.Contains(source.Describe(), ClaudeCodeKeychainService) {
		t.Errorf("Describe = %q; it should name the item so a failure is actionable", source.Describe())
	}
	if runtime.GOOS == "darwin" {
		t.Skip("this asserts the refusal on a platform with no Keychain")
	}
	_, err := source.Read(context.Background())
	if err == nil {
		t.Fatal("reading a Keychain succeeded on a platform that has none")
	}
	if !strings.Contains(err.Error(), "--from") && !strings.Contains(err.Error(), "file") {
		t.Errorf("the refusal does not point at the portable path: %v", err)
	}
}

// TestDefaultSourceIsPlatformShaped: macOS reads the Keychain, everything else reads the
// file the agent writes there.
func TestDefaultSourceIsPlatformShaped(t *testing.T) {
	source := claudeProfile(t).DefaultSource()
	switch runtime.GOOS {
	case "darwin":
		if _, ok := source.(KeychainSource); !ok {
			t.Errorf("on macOS the default source is %T, want the Keychain", source)
		}
	default:
		f, ok := source.(FileSource)
		if !ok {
			t.Fatalf("the default source is %T, want a file", source)
		}
		if !strings.HasSuffix(f.Path, filepath.Join(".claude", ".credentials.json")) {
			t.Errorf("the default file is %q, which is not where the agent writes one", f.Path)
		}
	}
}

// TestFileStoreOAuthRoundTrip: an OAuth credential lives in the same encrypted file as every
// other secret, is not readable as a header value, and rotates in place.
func TestFileStoreOAuthRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	store, err := OpenFile(path, []byte("a passphrase"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	record := testRecord(t, time.Now().Add(time.Hour))
	if err := store.SetOAuth("claude-code", record); err != nil {
		t.Fatalf("SetOAuth: %v", err)
	}
	if err := store.Set("api-key", NewValue("sk-live-plain")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// The file is ciphertext: neither token is in it, and neither is the sentinel.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the store: %v", err)
	}
	for _, canary := range []string{accessCanary, refreshCanary, record.AccessSentinel, "claude-code"} {
		if strings.Contains(string(raw), canary) {
			t.Errorf("%q is readable in the store file", canary)
		}
	}

	back, err := store.LookupOAuthRecord(context.Background(), "claude-code")
	if err != nil {
		t.Fatalf("LookupOAuthRecord: %v", err)
	}
	if back.AccessToken != accessCanary || back.AccessSentinel != record.AccessSentinel {
		t.Error("the record did not round-trip")
	}

	// An OAuth credential is not a header value, and asking for it as one must fail loudly
	// rather than return a JSON blob for an origin to reject.
	if _, err := store.Lookup(context.Background(), "claude-code"); err == nil {
		t.Error("an oauth credential was handed out as a header value")
	}
	// And the reverse.
	if _, err := store.LookupOAuthRecord(context.Background(), "api-key"); err == nil {
		t.Error("a plain secret was read as an oauth credential")
	}

	// Rotation replaces the pair and leaves the shape alone.
	rotated := OAuthTokens{
		Access:  NewValue(rotatedAccess),
		Refresh: NewValue("sk-ant-ort01-ROTATED-REFRESH"),
		Expiry:  time.Now().Add(2 * time.Hour),
	}
	if err := store.SaveOAuth(context.Background(), "claude-code", rotated); err != nil {
		t.Fatalf("SaveOAuth: %v", err)
	}
	after, err := store.LookupOAuthRecord(context.Background(), "claude-code")
	if err != nil {
		t.Fatalf("LookupOAuthRecord: %v", err)
	}
	if after.AccessToken != rotatedAccess {
		t.Error("the rotated token was not persisted")
	}
	if after.AccessSentinel != record.AccessSentinel || after.TokenHost != record.TokenHost {
		t.Error("a rotation changed the credential's shape; the guest's sentinel must not move")
	}

	// Listing says what kind each is, and never a value.
	entries, err := store.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	for _, e := range entries {
		if e.Name == "claude-code" && !e.OAuth {
			t.Error("the oauth credential is not listed as one")
		}
		if e.Name == "api-key" && e.OAuth {
			t.Error("a plain secret is listed as oauth")
		}
	}
}

// TestSaveOAuthOnAMissingCredentialFails: a rotation must not create a credential out of
// nothing, because the shape it would be missing is what says where the token may go.
func TestSaveOAuthOnAMissingCredentialFails(t *testing.T) {
	store, err := OpenFile(filepath.Join(t.TempDir(), "secrets.json"), []byte("pass"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	err = store.SaveOAuth(context.Background(), "absent", OAuthTokens{Access: NewValue("x")})
	if err == nil {
		t.Fatal("saving a rotation for a credential that does not exist succeeded")
	}
	assertNoCanary(t, err.Error())
}
