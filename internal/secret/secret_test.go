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

func TestParseRule(t *testing.T) {
	tests := []struct {
		spec       string
		wantSecret string
		wantScheme Scheme
		wantExtra  string
	}{
		{"api.anthropic.com=anthropic:header:x-api-key", "anthropic", SchemeHeader, "x-api-key"},
		{"github.com,api.github.com=gh:basic:x-access-token", "gh", SchemeBasic, "x-access-token"},
		{"registry.example.com=reg:bearer", "reg", SchemeBearer, ""},
		{"*.internal.example.com=tok:bearer", "tok", SchemeBearer, ""},
	}
	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			r, err := ParseRule(tc.spec)
			if err != nil {
				t.Fatalf("ParseRule: %v", err)
			}
			if r.Secret != tc.wantSecret || r.Scheme != tc.wantScheme {
				t.Errorf("got %s:%s, want %s:%s", r.Secret, r.Scheme, tc.wantSecret, tc.wantScheme)
			}
			extra := r.Header
			if r.Scheme == SchemeBasic {
				extra = r.Username
			}
			if extra != tc.wantExtra {
				t.Errorf("extra = %q, want %q", extra, tc.wantExtra)
			}
			// The rendered form must round-trip, so `boks policy ls` output can be
			// pasted back into a flag.
			back, err := ParseRule(r.String())
			if err != nil {
				t.Fatalf("re-parsing %q: %v", r.String(), err)
			}
			if back.String() != r.String() {
				t.Errorf("round trip: %q -> %q", r.String(), back.String())
			}
		})
	}
}

// TestParseRuleRejectsCatchAll is a security property, not a parsing detail: a credential
// destination of "*" would send the token wherever the guest chose.
func TestParseRuleRejectsCatchAll(t *testing.T) {
	bad := []string{
		"*=tok:bearer",
		"github.com,*=tok:bearer",
		"github.com=tok:header",       // header scheme with no header name
		"github.com=tok:oauth",        // unknown scheme
		"github.com=tok:bearer:extra", // bearer takes no third field
		"github.com=:bearer",          // no secret name
		"github.com",                  // no '=' at all
		"=tok:bearer",
	}
	for _, spec := range bad {
		t.Run(spec, func(t *testing.T) {
			if r, err := ParseRule(spec); err == nil {
				t.Errorf("ParseRule(%q) = %v, want an error", spec, r)
			}
		})
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

func TestInjectorSchemes(t *testing.T) {
	provider := MapProvider{"tok": canary}

	tests := []struct {
		spec       string
		wantHeader string
		wantValue  string
	}{
		{"api.example.com=tok:bearer", "Authorization", "Bearer " + canary},
		{"api.example.com=tok:header:x-api-key", "X-Api-Key", canary},
		{"api.example.com=tok:basic:x-access-token", "Authorization",
			"Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+canary))},
	}
	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			rule, err := ParseRule(tc.spec)
			if err != nil {
				t.Fatalf("ParseRule: %v", err)
			}
			inj, err := NewInjector(provider, rule)
			if err != nil {
				t.Fatalf("NewInjector: %v", err)
			}
			h := http.Header{}
			used, err := inj.Apply(context.Background(), mustTarget(t, "api.example.com:443"), h)
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

func TestInjectorScoping(t *testing.T) {
	rule, err := ParseRule("api.example.com,*.svc.example.com=tok:bearer")
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	inj, err := NewInjector(MapProvider{"tok": canary}, rule)
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
			used, err := inj.Apply(context.Background(), mustTarget(t, tc.target), h)
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
		})
	}
}

func TestInjectorReportsMissingSecretWithoutLeaking(t *testing.T) {
	rule, err := ParseRule("api.example.com=absent:bearer")
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	inj, err := NewInjector(MapProvider{"other": canary}, rule)
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	h := http.Header{}
	_, err = inj.Apply(context.Background(), mustTarget(t, "api.example.com:443"), h)
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

func TestNilInjectorIsANoop(t *testing.T) {
	var inj *Injector
	h := http.Header{}
	used, err := inj.Apply(context.Background(), mustTarget(t, "api.example.com:443"), h)
	if err != nil || len(used) != 0 || len(h) != 0 {
		t.Errorf("nil injector did something: used=%v err=%v h=%v", used, err, h)
	}
	if inj.Rules() != nil {
		t.Error("nil injector reported rules")
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
