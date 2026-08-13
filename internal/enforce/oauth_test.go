package enforce

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/secret"
)

const (
	oauthAccessCanary  = "sk-ant-oat01-REAL-ACCESS-CANARY"
	oauthRefreshCanary = "sk-ant-ort01-REAL-REFRESH-CANARY"
)

func oauthSpec(t *testing.T) Spec {
	t.Helper()
	spec := testSpec(t, network.ModeNAT)
	profile, err := secret.Profile("claude-code")
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	record, err := profile.Record("claude-code", secret.ImportedTokens{
		Access:       oauthAccessCanary,
		Refresh:      oauthRefreshCanary,
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		Scopes:       []string{"user:inference", "user:profile"},
		Subscription: "max",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	spec.OAuth = map[string]secret.OAuthRecord{"claude-code": record}
	spec.Intercept = true
	return spec
}

// TestOAuthGuestGetsSentinelsAndNeverATokenID is the guest-facing half of the OAuth
// guarantee, and the acceptance criterion for the whole feature: everything the sandbox can
// read — its environment, its annotations, the files shared into it — holds a sentinel, and
// the subscription token is in none of them.
func TestOAuthGuestGetsSentinelsAndNeverAToken(t *testing.T) {
	spec := oauthSpec(t)
	record := spec.OAuth["claude-code"]

	guest, err := spec.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	env := envMap(guest.Env)
	if env["CLAUDE_CODE_OAUTH_TOKEN"] != record.AccessSentinel {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %q, want the access sentinel", env["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	if env["BOKS_CREDENTIAL_FILE_CLAUDE_CODE"] != record.FilePath {
		t.Errorf("the credential file is not named in the environment: %q", env["BOKS_CREDENTIAL_FILE_CLAUDE_CODE"])
	}
	// An OAuth credential means TLS is terminated for the API and for the token endpoint,
	// so the guest has to be given the CA as well.
	if env["NODE_EXTRA_CA_CERTS"] == "" {
		t.Error("an oauth credential did not bring the CA with it; nothing would be intercepted")
	}

	for _, kv := range guest.Env {
		assertNoTokenCanary(t, kv)
	}
	for k, v := range guest.Annotations {
		assertNoTokenCanary(t, k+"="+v)
	}
}

// TestOAuthCredentialFileIsSharedReadOnly checks the mechanism against the one it copies:
// the CA is a host-side directory shared read-only, and so is this.
func TestOAuthCredentialFileIsSharedReadOnly(t *testing.T) {
	spec := oauthSpec(t)
	record := spec.OAuth["claude-code"]

	guest, err := spec.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	guestDir := path.Dir(record.FilePath)
	var found bool
	for _, m := range guest.Mounts {
		if m.GuestPath != guestDir {
			continue
		}
		found = true
		if !m.ReadOnly() {
			t.Errorf("the credential directory is mounted %s; it must be read-only", m.Mode)
		}
		// What is in the shared directory matters more than what is in the environment.
		entries, err := os.ReadDir(m.HostPath)
		if err != nil {
			t.Fatalf("reading the shared credential directory: %v", err)
		}
		if len(entries) != 1 || entries[0].Name() != path.Base(record.FilePath) {
			t.Fatalf("the shared directory holds %v; it should hold only the credential file", entries)
		}
		data, err := os.ReadFile(filepath.Join(m.HostPath, entries[0].Name()))
		if err != nil {
			t.Fatal(err)
		}
		assertNoTokenCanary(t, string(data))

		// It has to be the document the agent expects, with sentinels where the tokens
		// were, or the agent simply will not read it.
		var doc struct {
			OAuth struct {
				AccessToken      string   `json:"accessToken"`
				RefreshToken     string   `json:"refreshToken"`
				ExpiresAt        int64    `json:"expiresAt"`
				Scopes           []string `json:"scopes"`
				SubscriptionType string   `json:"subscriptionType"`
			} `json:"claudeAiOauth"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("the credential file is not a Claude Code document: %v\n%s", err, data)
		}
		if doc.OAuth.AccessToken != record.AccessSentinel || doc.OAuth.RefreshToken != record.RefreshSentinel {
			t.Error("the credential file does not carry the sentinels")
		}
		if doc.OAuth.SubscriptionType != "max" || len(doc.OAuth.Scopes) != 2 {
			t.Errorf("the agent-visible metadata was lost: %+v", doc.OAuth)
		}
	}
	if !found {
		t.Fatalf("no mount for %s; the guest would find no credential file. Mounts: %+v", guestDir, guest.Mounts)
	}
}

// TestOAuthCredentialsAreCredentialHosts: an OAuth credential decides interception exactly as
// an injection rule does, and for the same two reasons — the substitution and the answered
// token request both need the flow terminated.
func TestOAuthCredentialsAreCredentialHosts(t *testing.T) {
	spec := oauthSpec(t)
	if !spec.intercepts() {
		t.Fatal("a sandbox with an oauth credential does not intercept anything, so nothing would work")
	}
	credentials, err := spec.Credentials()
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	hosts := secret.CredentialHosts(credentials)
	want := map[string]bool{"api.anthropic.com": true, "platform.claude.com": true}
	if len(hosts) != len(want) {
		t.Fatalf("credential hosts = %v, want exactly %v", hosts, want)
	}
	for _, h := range hosts {
		if !want[h] {
			t.Errorf("%s would be decrypted and should not be", h)
		}
	}
}

// TestSpecNeverPrintsAToken: a Spec carries live tokens now, and error paths print structs.
func TestSpecNeverPrintsAToken(t *testing.T) {
	spec := oauthSpec(t)
	assertNoTokenCanary(t, spec.String())
	assertNoTokenCanary(t, spec.GoString())

	// The JSON form must *not* redact: it is how the supervisor is given the credential.
	// This is the one place the values are supposed to be present, and a redacting
	// MarshalText on the record would silently break it.
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(encoded), oauthAccessCanary) {
		t.Fatal("the spec's JSON form dropped the token; the supervisor would get nothing to inject")
	}
	var back Spec
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if back.OAuth["claude-code"].AccessToken != oauthAccessCanary {
		t.Error("the record did not survive the pipe")
	}
}

func assertNoTokenCanary(t *testing.T, s string) {
	t.Helper()
	for _, canary := range []string{oauthAccessCanary, oauthRefreshCanary} {
		if strings.Contains(s, canary) {
			t.Fatalf("a real token appears where it must not:\n%s", s)
		}
	}
}
