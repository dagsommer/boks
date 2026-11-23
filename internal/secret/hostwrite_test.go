package secret

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const hostDoc = `{"claudeAiOauth":{"accessToken":"old-access","refreshToken":"old-refresh","expiresAt":1000,` +
	`"scopes":["user:inference"],"subscriptionType":"max","rateLimitTier":"x"},"mcpOAuth":{"k":{"v":1}}}`

func hostFile(t *testing.T) FileSource {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(p, []byte(hostDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	return FileSource{Path: p}
}

// The refreshed pair replaces the host's tokens and nothing else: the agent's other fields,
// including ones this code has never heard of, survive.
func TestWriteHostLoginReplacesOnlyTheTokens(t *testing.T) {
	src := hostFile(t)
	profile, _ := Profile("claude-code")
	fresh := OAuthTokens{Access: NewValue("new-access"), Refresh: NewValue("new-refresh"), Expiry: time.UnixMilli(2000)}
	wrote, err := writeHostLogin(context.Background(), profile, src, NewValue("old-refresh"), fresh)
	if err != nil || !wrote {
		t.Fatalf("wrote = %v, err = %v", wrote, err)
	}
	raw, _ := os.ReadFile(src.Path)
	got, err := profile.Parse(raw)
	if err != nil || got.Access != "new-access" || got.Refresh != "new-refresh" || got.ExpiresAt != 2000 {
		t.Fatalf("tokens after the write = %+v, %v", got, err)
	}
	for _, keep := range []string{`"subscriptionType":"max"`, `"rateLimitTier":"x"`, `"scopes":["user:inference"]`, `"mcpOAuth":{"k":{"v":1}}`} {
		if !strings.Contains(string(raw), keep) {
			t.Errorf("the write dropped %s:\n%s", keep, raw)
		}
	}
	// Windows has no permission bits to assert on: it reports 0666 for any writable file.
	if fi, _ := os.Stat(src.Path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// If the agent has refreshed or the user has logged in since, the host holds the newer login
// and it is left alone.
func TestWriteHostLoginLeavesANewerLoginAlone(t *testing.T) {
	src := hostFile(t)
	profile, _ := Profile("claude-code")
	fresh := OAuthTokens{Access: NewValue("new-access"), Refresh: NewValue("new-refresh")}
	wrote, err := writeHostLogin(context.Background(), profile, src, NewValue("some-other-refresh"), fresh)
	if err != nil || wrote {
		t.Fatalf("wrote = %v, err = %v; want a quiet no-op", wrote, err)
	}
	if raw, _ := os.ReadFile(src.Path); string(raw) != hostDoc {
		t.Errorf("the host's login changed:\n%s", raw)
	}
}

// Every refresh the injector makes itself is offered to the writer, with the refresh token it
// spent; one the host supplied is not refreshed here, so nothing is offered for it.
func TestInjectorWritesWhatItRefreshesBack(t *testing.T) {
	record := testRecord(t, time.Now().Add(-time.Hour))
	inj, _, _ := testInjector(t, record)
	inj.SetRefresher(&stubRefresher{access: "fresh-access", refresh: "fresh-refresh", ttl: time.Hour})
	var gotFrom Value
	var gotFresh OAuthTokens
	inj.SetHostWriter(func(_ context.Context, _ string, from Value, fresh OAuthTokens) { gotFrom, gotFresh = from, fresh })

	h := http.Header{}
	h.Set("Authorization", "Bearer "+record.AccessSentinel)
	if _, err := inj.Apply(context.Background(), mustTarget(t, "api.creds.test:443"), h, FlowTLS); err != nil {
		t.Fatal(err)
	}
	if gotFresh.Access.Reveal() != "fresh-access" || gotFrom.IsZero() {
		t.Errorf("writer saw from=%v fresh=%v; want the refreshed pair and the token it spent", !gotFrom.IsZero(), gotFresh.Access.Reveal())
	}
}

// The Keychain write, against a throwaway item of our own — never the real login. It also
// pins the property the hex-on-stdin form exists for: the document is not an argument.
func TestKeychainWriteUpdatesTheItemInPlace(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("needs the macOS Keychain")
	}
	svc := fmt.Sprintf("boks-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	acct := "boks-test"
	if out, err := exec.Command("security", "add-generic-password", "-s", svc, "-a", acct, "-w", "seed").CombinedOutput(); err != nil {
		t.Skipf("cannot create a scratch keychain item: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("security", "delete-generic-password", "-s", svc).Run() })

	src := KeychainSource{Service: svc}
	doc := []byte(`{"claudeAiOauth":{"accessToken":"a b","refreshToken":"r"},"x":"quote \" and 'single'"}`)
	if err := src.Write(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	got, err := src.Read(context.Background())
	if err != nil || strings.TrimSpace(string(got)) != string(doc) {
		t.Fatalf("read back %q, %v", got, err)
	}
	out, _ := exec.Command("security", "dump-keychain").CombinedOutput()
	if n := strings.Count(string(out), `"svce"<blob>="`+svc+`"`); n > 1 {
		t.Errorf("%d items named %s; the write must update, not add", n, svc)
	}
}
