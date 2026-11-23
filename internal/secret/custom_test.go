package secret

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
)

// Two secrets on one host, told apart by their placeholders: each is swapped only where its own
// placeholder appears, and a request carrying neither is left alone.
func TestSubstituteKeysOnThePlaceholder(t *testing.T) {
	creds, err := ParseCredentials(
		[]string{"copilot@ghe.example.com=substitute", "repo@ghe.example.com=substitute"},
		[]string{"copilot=COPILOT_GITHUB_TOKEN=sbx-cs-copilot", "repo=GH_TOKEN=sbx-cs-repo"})
	if err != nil {
		t.Fatal(err)
	}
	inj, err := NewInjector(MapProvider{"copilot": "REAL-COPILOT", "repo": "REAL-REPO"}, creds...)
	if err != nil {
		t.Fatal(err)
	}
	target := mustTarget(t, "ghe.example.com:443")
	apply := func(h http.Header) {
		t.Helper()
		if _, err := inj.Apply(context.Background(), target, h, FlowTLS); err != nil {
			t.Fatal(err)
		}
	}

	h := http.Header{"Authorization": {"Bearer sbx-cs-copilot"}, "X-Other": {"sbx-cs-repo"}}
	apply(h)
	if got := h.Get("Authorization"); got != "Bearer REAL-COPILOT" {
		t.Errorf("Authorization = %q", got)
	}
	if got := h.Get("X-Other"); got != "REAL-REPO" {
		t.Errorf("X-Other = %q", got)
	}

	h = http.Header{"Authorization": {"Bearer something-the-agent-owns"}}
	apply(h)
	if got := h.Get("Authorization"); got != "Bearer something-the-agent-owns" {
		t.Errorf("a request without a placeholder was changed: %q", got)
	}

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:sbx-cs-repo"))
	h = http.Header{"Authorization": {basic}}
	apply(h)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:REAL-REPO"))
	if got := h.Get("Authorization"); got != want {
		t.Errorf("basic auth = %q, want %q", got, want)
	}

	// Another host is never touched.
	other := http.Header{"Authorization": {"Bearer sbx-cs-copilot"}}
	if _, err := inj.Apply(context.Background(), mustTarget(t, "evil.example.com:443"), other, FlowTLS); err != nil {
		t.Fatal(err)
	}
	if got := other.Get("Authorization"); got != "Bearer sbx-cs-copilot" {
		t.Errorf("placeholder substituted on an unconfigured host: %q", got)
	}
}

func TestCustomRoundTripAndResolve(t *testing.T) {
	c := Custom{Hosts: []string{"a.example.com"}, Env: "API_KEY", Placeholder: "sbx-cs-x", Command: "echo hunter2"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	enc, err := c.Encode()
	if err != nil || !IsCustom(enc) {
		t.Fatalf("encode: %v %q", err, enc)
	}
	back, err := DecodeCustom(enc)
	if err != nil {
		t.Fatal(err)
	}
	v, err := back.Resolve(context.Background())
	if err != nil || strings.TrimSpace(v) != "hunter2" {
		t.Fatalf("resolve = %q, %v", v, err)
	}
	if CustomName("API_KEY", []string{"b", "a"}) != CustomName("API_KEY", []string{"a", "b"}) {
		t.Error("the name depends on host order")
	}
	if (Custom{Hosts: []string{"a"}, Env: "bad name", Value: "v"}).Validate() == nil {
		t.Error("an invalid env name was accepted")
	}
}

func TestExpandPlaceholder(t *testing.T) {
	p, err := ExpandPlaceholder("github_pat_{rand}", "")
	if err != nil || !strings.HasPrefix(p, "github_pat_") || len(p) <= len("github_pat_") {
		t.Fatalf("got %q, %v", p, err)
	}
	if again, _ := ExpandPlaceholder("github_pat_{rand}", p); again != p {
		t.Errorf("rotation changed the placeholder: %q -> %q", p, again)
	}
	if other, _ := ExpandPlaceholder("ghp_{rand}", p); other == p || !strings.HasPrefix(other, "ghp_") {
		t.Errorf("a different template kept the old placeholder: %q", other)
	}
	if lit, _ := ExpandPlaceholder("gho_sbxproxymanaged000", "x"); lit != "gho_sbxproxymanaged000" {
		t.Errorf("literal placeholder = %q", lit)
	}
	if ValidatePlaceholder("a b") == nil || ValidatePlaceholder("a=b") == nil {
		t.Error("bad placeholder accepted")
	}
}
