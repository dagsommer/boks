package secret

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/policy"
)

const canary = "sk-live-CANARY-must-never-be-printed"

// TestValueNeverRendersItself is the guard the whole package rests on: if a Value can be
// coaxed into printing itself, every log line in Boks becomes a potential leak.
func TestValueNeverRendersItself(t *testing.T) {
	v := NewValue(canary)

	type config struct {
		Name  string
		Token Value
	}
	c := config{Name: "anthropic", Token: v}

	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	logger.Printf("value=%v string=%s quoted=%q gostring=%#v struct=%+v structgo=%#v json=%s",
		v, v, v, v, c, c, encoded)
	// Errors are a common accidental path for a secret to reach a log.
	logger.Printf("err=%v", fmt.Errorf("using %v: %w", v, errors.New("upstream refused")))

	if strings.Contains(buf.String(), canary) {
		t.Fatalf("a Value rendered its contents:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), Redacted) {
		t.Errorf("expected the redaction marker in:\n%s", buf.String())
	}
	if v.Reveal() != canary {
		t.Error("Reveal did not return the value")
	}
}

func mustCredential(t *testing.T, service string, specs ...string) Credential {
	t.Helper()
	c := Credential{Service: service}
	for _, spec := range specs {
		got, rules, err := ParseInject(spec)
		if err != nil {
			t.Fatalf("ParseInject(%q): %v", spec, err)
		}
		if got != service {
			t.Fatalf("ParseInject(%q) is for service %q, want %q", spec, got, service)
		}
		c.Inject = append(c.Inject, rules...)
	}
	return c
}

func TestParseInject(t *testing.T) {
	tests := []struct {
		spec        string
		wantService string
		wantDomains []string
		wantHeader  string
		wantFormat  string
	}{
		{"anthropic@api.anthropic.com=x-api-key", "anthropic", []string{"api.anthropic.com"}, "x-api-key", "%s"},
		{"gh@github.com,api.github.com=bearer", "gh", []string{"github.com", "api.github.com"}, "Authorization", "Bearer %s"},
		{"ado@pkgs.dev.azure.com=basic:x-access-token", "ado", []string{"pkgs.dev.azure.com"}, "Authorization", "Basic %s"},
		{"odd@api.example.com=Authorization:token %s", "odd", []string{"api.example.com"}, "Authorization", "token %s"},
		{"wild@*.internal.example.com=bearer", "wild", []string{"*.internal.example.com"}, "Authorization", "Bearer %s"},
	}
	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			service, rules, err := ParseInject(tc.spec)
			if err != nil {
				t.Fatalf("ParseInject: %v", err)
			}
			if service != tc.wantService {
				t.Errorf("service = %q, want %q", service, tc.wantService)
			}
			if len(rules) != len(tc.wantDomains) {
				t.Fatalf("got %d rules, want %d", len(rules), len(tc.wantDomains))
			}
			for i, want := range tc.wantDomains {
				if rules[i].Domain.String() != want {
					t.Errorf("domain %d = %q, want %q", i, rules[i].Domain, want)
				}
				if rules[i].header() != tc.wantHeader {
					t.Errorf("header = %q, want %q", rules[i].header(), tc.wantHeader)
				}
				if rules[i].effectiveFormat() != tc.wantFormat {
					t.Errorf("format = %q, want %q", rules[i].effectiveFormat(), tc.wantFormat)
				}
			}
		})
	}
}

// TestParseInjectRejectsDangerousRules covers security properties, not parsing details: a
// catch-all domain would send the token wherever the guest chose *and* decrypt everything,
// and a format that can carry a newline turns one header into two.
func TestParseInjectRejectsDangerousRules(t *testing.T) {
	bad := []string{
		"tok@*=bearer",
		"tok@github.com,*=bearer",
		"tok@github.com=bearer:extra",          // bearer takes no further field
		"tok@github.com=Authorization:no-verb", // no %s at all
		"tok@github.com=Authorization:%s %s",   // two of them
		"tok@github.com=Authorization:%d",      // wrong verb
		"tok@github.com=",                      // no attachment
		"@github.com=bearer",                   // no service
		"tok@=bearer",                          // no host
		"github.com=bearer",                    // no service@
		"tok@github.com",                       // no attachment at all
	}
	for _, spec := range bad {
		t.Run(spec, func(t *testing.T) {
			if service, rules, err := ParseInject(spec); err == nil {
				t.Errorf("ParseInject(%q) = %s %v, want an error", spec, service, rules)
			}
		})
	}
	// A newline inside a value format would let a credential forge a second header.
	r := Inject{Domain: policy.MustPattern("api.example.com"), Header: "Authorization", Format: "Bearer %s\r\nX-Evil: 1"}
	if err := r.Validate(); err == nil {
		t.Error("a value format containing CRLF was accepted")
	}
	// So would one in a header name.
	if err := (Inject{Domain: policy.MustPattern("api.example.com"), Header: "X\r\nEvil", Format: "%s"}).Validate(); err == nil {
		t.Error("a header name containing CRLF was accepted")
	}
	// Format and scheme are alternatives, never both.
	both := Inject{Domain: policy.MustPattern("api.example.com"), Scheme: SchemeBearer, Format: "Bearer %s"}
	if err := both.Validate(); err == nil {
		t.Error("a rule setting both a scheme and a format was accepted")
	}
	// The zero pattern matches everything; it must not be usable as a domain.
	if err := (Inject{Scheme: SchemeBearer}).Validate(); err == nil {
		t.Error("a rule with no domain was accepted")
	}
}

func TestParseGuestCredential(t *testing.T) {
	service, env, placeholder, err := ParseGuestCredential("gh=GH_TOKEN=gho_sbxproxymanaged000000000000000000000")
	if err != nil {
		t.Fatalf("ParseGuestCredential: %v", err)
	}
	if service != "gh" || env != "GH_TOKEN" || placeholder != "gho_sbxproxymanaged000000000000000000000" {
		t.Errorf("got %q %q %q", service, env, placeholder)
	}
	service, env, placeholder, err = ParseGuestCredential("gh=gho_something")
	if err != nil {
		t.Fatalf("ParseGuestCredential: %v", err)
	}
	if service != "gh" || env != "" || placeholder != "gho_something" {
		t.Errorf("got %q %q %q", service, env, placeholder)
	}
	// A placeholder that is empty is worse than none: the guest's own client then fails
	// in a way that looks like a boks bug.
	if _, _, _, err := ParseGuestCredential("gh="); err == nil {
		t.Error("an empty placeholder was accepted")
	}
}

func mustTarget(t *testing.T, hostport string) policy.Target {
	t.Helper()
	tgt, err := policy.ParseTarget(hostport, 443)
	if err != nil {
		t.Fatalf("ParseTarget(%q): %v", hostport, err)
	}
	return tgt
}

func TestInjectorAttachmentForms(t *testing.T) {
	provider := MapProvider{"tok": canary}

	tests := []struct {
		spec       string
		wantHeader string
		wantValue  string
	}{
		{"tok@api.example.com=bearer", "Authorization", "Bearer " + canary},
		{"tok@api.example.com=x-api-key", "X-Api-Key", canary},
		{"tok@api.example.com=Authorization:token %s", "Authorization", "token " + canary},
		{"tok@api.example.com=basic:x-access-token", "Authorization",
			"Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+canary))},
	}
	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			inj, err := NewInjector(provider, mustCredential(t, "tok", tc.spec))
			if err != nil {
				t.Fatalf("NewInjector: %v", err)
			}
			h := http.Header{}
			used, err := inj.Apply(context.Background(), mustTarget(t, "api.example.com:443"), h, FlowTLS)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if len(used) != 1 || used[0] != "tok" {
				t.Errorf("used = %v, want [tok]", used)
			}
			if got := h.Get(tc.wantHeader); got != tc.wantValue {
				t.Errorf("%s = %q, want %q", tc.wantHeader, got, tc.wantValue)
			}
		})
	}
}

// TestOneSecretManyHosts is the reason the model has two levels: several destinations share
// one stored secret, and the attachment is written once per destination group rather than
// the secret being repeated.
func TestOneSecretManyHosts(t *testing.T) {
	cred := mustCredential(t, "ghe",
		"ghe@ghe.example.com,api.ghe.example.com=bearer",
		"ghe@pkgs.example.com=basic:x-access-token")
	inj, err := NewInjector(MapProvider{"ghe": canary}, cred)
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	for _, host := range []string{"ghe.example.com:443", "api.ghe.example.com:443"} {
		h := http.Header{}
		if _, err := inj.Apply(context.Background(), mustTarget(t, host), h, FlowTLS); err != nil {
			t.Fatalf("Apply(%s): %v", host, err)
		}
		if h.Get("Authorization") != "Bearer "+canary {
			t.Errorf("%s got %q", host, h.Get("Authorization"))
		}
	}
	h := http.Header{}
	if _, err := inj.Apply(context.Background(), mustTarget(t, "pkgs.example.com:443"), h, FlowTLS); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+canary))
	if h.Get("Authorization") != want {
		t.Errorf("the same secret attached the other way = %q, want %q", h.Get("Authorization"), want)
	}
}

func TestInjectorScoping(t *testing.T) {
	inj, err := NewInjector(MapProvider{"tok": canary},
		mustCredential(t, "tok", "tok@api.example.com,*.svc.example.com=bearer"))
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}

	tests := []struct {
		target string
		want   bool
	}{
		{"api.example.com:443", true},
		{"a.svc.example.com:443", true},
		{"API.example.com.:443", true}, // normalisation must not open a hole or close one
		{"svc.example.com:443", false}, // wildcard does not cover the apex
		{"example.com:443", false},
		{"api.example.com.evil.test:443", false},
		{"evil.test:443", false},
	}
	for _, tc := range tests {
		t.Run(tc.target, func(t *testing.T) {
			h := http.Header{}
			used, err := inj.Apply(context.Background(), mustTarget(t, tc.target), h, FlowTLS)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			got := h.Get("Authorization") != ""
			if got != tc.want {
				t.Errorf("injected = %v, want %v (header %q)", got, tc.want, h.Get("Authorization"))
			}
			if got != (len(used) > 0) {
				t.Errorf("used = %v but header injected = %v", used, got)
			}
			// Whatever decides injection also decides interception, or a host gets
			// decrypted for nothing.
			if inj.Handles(mustTarget(t, tc.target)) != tc.want {
				t.Errorf("Handles disagrees with injection for %s", tc.target)
			}
		})
	}
}

func TestInjectorReportsMissingSecretWithoutLeaking(t *testing.T) {
	inj, err := NewInjector(MapProvider{"other": canary},
		mustCredential(t, "absent", "absent@api.example.com=bearer"))
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	h := http.Header{}
	_, err = inj.Apply(context.Background(), mustTarget(t, "api.example.com:443"), h, FlowTLS)
	if err == nil {
		t.Fatal("Apply succeeded with a missing secret")
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Errorf("error %q should name the missing secret", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Errorf("error leaked another secret: %v", err)
	}
	if h.Get("Authorization") != "" {
		t.Error("a header was set despite the failure")
	}
}

// TestCredentialWithNoRulesIsRejected: a credential nothing can use is a configuration
// mistake, and accepting it silently means a secret that never arrives anywhere.
func TestCredentialWithNoRulesIsRejected(t *testing.T) {
	if _, err := NewInjector(MapProvider{"tok": canary}, Credential{Service: "tok"}); err == nil {
		t.Error("a credential with no injection rules was accepted")
	}
	dup := mustCredential(t, "tok", "tok@api.example.com=bearer")
	if _, err := NewInjector(MapProvider{"tok": canary}, dup, dup); err == nil {
		t.Error("the same service was accepted twice")
	}
}

func TestPlaceholdersAreKeyedByEnvironmentVariable(t *testing.T) {
	cred := mustCredential(t, "gh", "gh@github.com=bearer")
	cred.EnvName, cred.Placeholder, cred.ProxyManaged = "GH_TOKEN", "gho_sbxproxymanaged000000000000000000000", true
	other := mustCredential(t, "anthropic", "anthropic@api.anthropic.com=x-api-key")
	other.Placeholder = "sk-ant-api03-placeholder"

	inj, err := NewInjector(MapProvider{"gh": canary, "anthropic": canary}, cred, other)
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	got := inj.Placeholders()
	if got["GH_TOKEN"] != cred.Placeholder {
		t.Errorf("placeholders = %v, want it keyed by the environment variable", got)
	}
	if got["anthropic"] != other.Placeholder {
		t.Errorf("a credential with no environment variable should be keyed by service: %v", got)
	}
	// A placeholder is not a secret, and must not be redacted into uselessness: the guest
	// has to be given the literal value.
	if strings.Contains(fmt.Sprint(got), Redacted) {
		t.Errorf("placeholders were redacted: %v", got)
	}
}

func TestHostsListsWhatWillBeDecrypted(t *testing.T) {
	first := mustCredential(t, "tok", "tok@b.test,a.test=bearer")
	second := mustCredential(t, "other", "other@a.test=x-key")
	inj, err := NewInjector(MapProvider{"tok": canary, "other": canary}, first, second)
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	want := []string{"a.test", "b.test"}
	got := inj.Hosts()
	if len(got) != len(want) {
		t.Fatalf("Hosts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Hosts = %v, want %v (sorted and deduplicated)", got, want)
		}
	}
	var nilInjector *Injector
	if nilInjector.Hosts() != nil || nilInjector.Handles(mustTarget(t, "a.test:443")) {
		t.Error("a nil injector claims to handle something")
	}
}

func TestNilInjectorIsANoop(t *testing.T) {
	var inj *Injector
	h := http.Header{}
	used, err := inj.Apply(context.Background(), mustTarget(t, "api.example.com:443"), h, FlowTLS)
	if err != nil || len(used) != 0 || len(h) != 0 {
		t.Errorf("nil injector did something: used=%v err=%v h=%v", used, err, h)
	}
	if inj.Credentials() != nil {
		t.Error("nil injector reported credentials")
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "secrets.json")
	store, err := OpenFile(path, []byte("correct horse battery staple"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	if _, err := store.Lookup(context.Background(), "absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Lookup on an empty store = %v, want ErrNotFound", err)
	}

	if err := store.Set("anthropic", NewValue(canary)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Set("github", NewValue("ghp_example")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// A fresh handle proves the value survived the file, not just the process.
	reopened, err := OpenFile(path, []byte("correct horse battery staple"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	v, err := reopened.Lookup(context.Background(), "anthropic")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if v.Reveal() != canary {
		t.Errorf("round-tripped value did not match")
	}
	names, err := reopened.Names()
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	if strings.Join(names, ",") != "anthropic,github" {
		t.Errorf("Names = %v", names)
	}

	if err := reopened.Delete("github"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := reopened.Lookup(context.Background(), "github"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted secret is still present: %v", err)
	}
}

func TestFileStoreIsEncryptedAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	store, err := OpenFile(path, []byte("passphrase"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if err := store.Set("anthropic", NewValue(canary)); err != nil {
		t.Fatalf("Set: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if bytes.Contains(raw, []byte(canary)) {
		t.Errorf("the secret is present in plaintext on disk:\n%s", raw)
	}
	// Names are inside the ciphertext too: which services a machine holds credentials
	// for is itself worth not disclosing.
	if bytes.Contains(raw, []byte("anthropic")) {
		t.Errorf("secret names are stored in the clear:\n%s", raw)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&fs.FileMode(0o077) != 0 {
		t.Errorf("permissions = %v, want owner-only", perm)
	}
}

func TestFileStoreWrongPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	store, err := OpenFile(path, []byte("right"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if err := store.Set("k", NewValue(canary)); err != nil {
		t.Fatalf("Set: %v", err)
	}

	wrong, err := OpenFile(path, []byte("wrong"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_, err = wrong.Lookup(context.Background(), "k")
	if err == nil {
		t.Fatal("the wrong passphrase decrypted the store")
	}
	if !strings.Contains(err.Error(), "wrong passphrase") {
		t.Errorf("error %q should tell the user what to check", err)
	}
}

func TestFileStoreDetectsTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	store, err := OpenFile(path, []byte("passphrase"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if err := store.Set("k", NewValue(canary)); err != nil {
		t.Fatalf("Set: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	data, _ := base64.StdEncoding.DecodeString(env["data"].(string))
	data[0] ^= 0xff
	env["data"] = base64.StdEncoding.EncodeToString(data)
	out, _ := json.Marshal(env)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := store.Lookup(context.Background(), "k"); err == nil {
		t.Fatal("a tampered store decrypted successfully; the AEAD is not doing its job")
	}
}

func TestOpenFileRequiresAPassphrase(t *testing.T) {
	_, err := OpenFile(filepath.Join(t.TempDir(), "s.json"), nil)
	if !errors.Is(err, ErrNoPassphrase) {
		t.Fatalf("err = %v, want ErrNoPassphrase", err)
	}
	if !strings.Contains(err.Error(), PassphraseEnv) {
		t.Errorf("error %q should say where the passphrase comes from", err)
	}
}
