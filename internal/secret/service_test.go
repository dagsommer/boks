package secret

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/policy"
)

// Every built-in row has to survive its own validation, and the two names Boks could not
// source have to still be *there* — a service registered with no rule is what turns "unknown
// service" into "the name is right, the rule is missing", which is the whole reason those two
// rows exist.
func TestBuiltinServicesAreValidAndSourced(t *testing.T) {
	r := Services()

	// sbx's list, verbatim from its own help, in its own order. A name missing here is a
	// name a user's habit will not find.
	want := []string{"anthropic", "cursor", "droid", "github", "google", "groq",
		"mistral", "nebius", "openai", "openrouter", "xai"}
	got := r.Names()
	if len(got) != len(want) {
		t.Fatalf("services = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("services = %v, want %v", got, want)
		}
	}

	for _, s := range r.All() {
		if err := s.Validate(); err != nil {
			t.Errorf("service %q does not validate: %v", s.Name, err)
		}
		if !s.Configured() {
			continue
		}
		if s.Source == "" {
			t.Errorf("service %q carries a rule and no citation", s.Name)
		}
		if s.Placeholder() == "" {
			t.Errorf("service %q has no placeholder for the guest to hold", s.Name)
		}
		// A placeholder that does not carry the vendor's own prefix fails a client's
		// local format check before any request reaches the proxy.
		if !strings.HasPrefix(s.Placeholder(), s.KeyPrefix) {
			t.Errorf("service %q: placeholder %q does not start with %q", s.Name, s.Placeholder(), s.KeyPrefix)
		}
		if s.KeyLength > 0 && len(s.Placeholder()) != s.KeyLength {
			t.Errorf("service %q: placeholder is %d characters, want %d",
				s.Name, len(s.Placeholder()), s.KeyLength)
		}
	}
}

// The headers most likely to be guessed wrong, pinned to what the vendor documents. A wrong
// one here is a request that fails for a reason nobody can see.
func TestKnownServiceResolvesToTheDocumentedRule(t *testing.T) {
	cases := []struct {
		service string
		host    string
		header  string
		value   string // %s stands for the credential
		env     string
	}{
		// Not Authorization: platform.claude.com documents `x-api-key: YOUR_API_KEY`,
		// and reserves Authorization for OAuth tokens.
		{"anthropic", "api.anthropic.com", "X-Api-Key", "%s", "ANTHROPIC_API_KEY"},
		// Not Authorization either: ai.google.dev documents `x-goog-api-key`.
		{"google", "generativelanguage.googleapis.com", "X-Goog-Api-Key", "%s", "GEMINI_API_KEY"},
		{"openai", "api.openai.com", "Authorization", "Bearer %s", "OPENAI_API_KEY"},
		{"groq", "api.groq.com", "Authorization", "Bearer %s", "GROQ_API_KEY"},
		{"mistral", "api.mistral.ai", "Authorization", "Bearer %s", "MISTRAL_API_KEY"},
		{"nebius", "api.tokenfactory.nebius.com", "Authorization", "Bearer %s", "NEBIUS_API_KEY"},
		{"openrouter", "openrouter.ai", "Authorization", "Bearer %s", "OPENROUTER_API_KEY"},
		{"xai", "api.x.ai", "Authorization", "Bearer %s", "XAI_API_KEY"},
		{"github", "api.github.com", "Authorization", "Bearer %s", "GITHUB_TOKEN"},
	}
	for _, tc := range cases {
		t.Run(tc.service, func(t *testing.T) {
			service, err := Services().Resolve(tc.service)
			if err != nil {
				t.Fatal(err)
			}
			credential, err := service.Credential()
			if err != nil {
				t.Fatal(err)
			}
			inj, err := NewInjector(MapProvider{tc.service: canary}, credential)
			if err != nil {
				t.Fatal(err)
			}
			target, err := policy.NewTarget(tc.host, 443)
			if err != nil {
				t.Fatal(err)
			}
			h := http.Header{}
			used, err := inj.Apply(context.Background(), target, h, FlowTLS)
			if err != nil {
				t.Fatal(err)
			}
			if len(used) != 1 || used[0] != tc.service {
				t.Fatalf("used = %v, want [%s]", used, tc.service)
			}
			if got, want := h.Get(tc.header), fmt.Sprintf(tc.value, canary); got != want {
				t.Errorf("%s = %q, want %q", tc.header, got, want)
			}
			if credential.EnvName != tc.env {
				t.Errorf("guest variable = %q, want %q", credential.EnvName, tc.env)
			}
			if credential.Placeholder == canary || strings.Contains(credential.Placeholder, canary) {
				t.Error("the guest's placeholder carries the real value")
			}
		})
	}
}

// A GitHub token rides two ways — basic auth on a Git fetch, bearer on the REST API — and the
// difference is per host. It is the one entry that proves a service can hold more than one
// attachment.
func TestGitHubCarriesTwoAttachments(t *testing.T) {
	service, _ := Services().Lookup("github")
	credential, err := service.Credential()
	if err != nil {
		t.Fatal(err)
	}
	inj, err := NewInjector(MapProvider{"github": canary}, credential)
	if err != nil {
		t.Fatal(err)
	}
	git, err := policy.NewTarget("github.com", 443)
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	if _, err := inj.Apply(context.Background(), git, h, FlowTLS); err != nil {
		t.Fatal(err)
	}
	got := h.Get("Authorization")
	if !strings.HasPrefix(got, "Basic ") {
		t.Fatalf("git over HTTPS got %q, want basic auth", got)
	}
	if strings.Contains(got, canary) {
		t.Error("the token was sent unencoded where base64 was expected")
	}
}

// A service Boks knows and has no rule for must fail by naming what is missing. "Unknown
// service" would be wrong — the name is right — and storing it silently would leave a
// credential nothing ever attaches.
func TestUnconfiguredServiceNamesWhatIsMissing(t *testing.T) {
	for _, name := range []string{"cursor", "droid"} {
		service, err := Services().Resolve(name)
		if err != nil {
			t.Fatalf("%s is not registered at all: %v", name, err)
		}
		if service.Configured() {
			t.Fatalf("%s is configured; this test is out of date", name)
		}
		err = RequireConfigured(service)
		if err == nil {
			t.Fatalf("%s: no error for a service with no rule", name)
		}
		// The message has to name both halves of what is missing and give the way
		// round it, or it is just a refusal.
		for _, want := range []string{"host", "header", "--inject", name} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the message does not mention %q:\n%v", name, want, err)
			}
		}
		if _, err := service.Credential(); err == nil {
			t.Errorf("%s produced a credential out of no rule", name)
		}
	}
}

func TestUnknownServiceIsNotAnError(t *testing.T) {
	r := Services()
	if r.Known("my-internal-api") {
		t.Fatal("an arbitrary name is registered")
	}
	_, err := r.Resolve("my-internal-api")
	if err == nil {
		t.Fatal("Resolve accepted an unknown name")
	}
	// The error is what a user sees when they mistype a service, so it has to list what
	// they could have meant.
	if !strings.Contains(err.Error(), "anthropic") || !strings.Contains(err.Error(), "--inject") {
		t.Errorf("the message neither lists the services nor points at --inject:\n%v", err)
	}
}

// The Add seam is what a user-defined service will arrive through, and what someone holding
// documentation Boks could not find uses to fill an empty row without waiting for a release.
func TestAddOverridesAnEmptyRow(t *testing.T) {
	r := Services()
	before := len(r.All())
	err := r.Add(Service{
		Name:      "cursor",
		Summary:   "Cursor, configured by hand",
		Inject:    []ServiceInject{{Hosts: []string{"api2.cursor.sh"}, Scheme: SchemeBearer, Why: "measured"}},
		EnvName:   "CURSOR_API_KEY",
		KeyPrefix: "crsr_",
		KeyLength: 69,
		Source:    "the user's own observation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.All()) != before {
		t.Errorf("Add appended instead of replacing: %d services, want %d", len(r.All()), before)
	}
	s, _ := r.Lookup("cursor")
	if !s.Configured() || s.Hosts()[0] != "api2.cursor.sh" {
		t.Errorf("the override did not take: %+v", s.Inject)
	}
	// Position is preserved, so a listing does not reshuffle when a row is overridden.
	if r.Names()[1] != "cursor" {
		t.Errorf("the override moved: %v", r.Names())
	}
}

func TestAddRejectsAnUnsourcedOrUnshapedRule(t *testing.T) {
	rule := []ServiceInject{{Hosts: []string{"api.example.com"}, Scheme: SchemeBearer, Why: "because"}}
	cases := map[string]Service{
		"no source":    {Name: "x", Inject: rule, KeyPrefix: "x-", KeyLength: 8},
		"no key shape": {Name: "x", Inject: rule, Source: "somewhere"},
		"no why":       {Name: "x", Inject: []ServiceInject{{Hosts: []string{"a.example.com"}}}, Source: "s", KeyLength: 8},
		"bad name":     {Name: "not a name", Inject: rule, Source: "s", KeyLength: 8},
		"catch-all host": {Name: "x", Source: "s", KeyLength: 8,
			Inject: []ServiceInject{{Hosts: []string{"*"}, Scheme: SchemeBearer, Why: "w"}}},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			r := &ServiceRegistry{}
			if err := r.Add(s); err == nil {
				t.Fatalf("Add accepted a service with %s", name)
			}
		})
	}
}

// The registry renders itself into the flag grammar, so that one parser and one validator
// govern a built-in row and a hand-typed rule alike. This is what keeps a registry entry from
// expressing something a user could not have typed.
func TestServiceRendersToTheFlagGrammar(t *testing.T) {
	service, _ := Services().Lookup("anthropic")
	specs := service.InjectSpecs()
	if len(specs) != 1 || specs[0] != "anthropic@api.anthropic.com=x-api-key" {
		t.Fatalf("inject specs = %v", specs)
	}
	guest := service.GuestSpec()
	if !strings.HasPrefix(guest, "anthropic=ANTHROPIC_API_KEY=sk-ant-api03-") {
		t.Fatalf("guest spec = %q", guest)
	}
	fromSpecs, err := ParseCredentials(specs, []string{guest})
	if err != nil {
		t.Fatal(err)
	}
	direct, err := service.Credential()
	if err != nil {
		t.Fatal(err)
	}
	if fromSpecs[0].Placeholder != direct.Placeholder || fromSpecs[0].EnvName != direct.EnvName {
		t.Error("the rendered specs and the built credential disagree")
	}
	if service.AllowSpecs()[0] != "api.anthropic.com:443" {
		t.Errorf("allow specs = %v", service.AllowSpecs())
	}
}

// An OAuth credential is the login the user performed; it refreshes itself and it is usually
// attached to the subscription they are paying for. An API key silently taking its place
// would succeed against the wrong account, which is the failure nobody thinks to look for.
func TestOAuthTakesPrecedenceOverAnAPIKey(t *testing.T) {
	key, err := mustService(t, "anthropic").Credential()
	if err != nil {
		t.Fatal(err)
	}
	login := oauthCredentialFor(t, "claude-code", "api.anthropic.com")
	other, err := mustService(t, "openai").Credential()
	if err != nil {
		t.Fatal(err)
	}

	kept, dropped := PreferOAuth([]Credential{key, other, login})
	if len(dropped) != 1 || dropped[0] != "anthropic" {
		t.Fatalf("dropped = %v, want [anthropic]", dropped)
	}
	names := make([]string, 0, len(kept))
	for _, c := range kept {
		names = append(names, c.Service)
	}
	// The key for a host the login does not cover is untouched: it is doing something
	// the login is not.
	if len(names) != 2 || names[0] != "openai" || names[1] != "claude-code" {
		t.Fatalf("kept = %v, want [openai claude-code]", names)
	}
}

// A key covering even one host the login does not is kept. Precedence is about a collision at
// a destination, not about the word "oauth" appearing anywhere in the set.
func TestPreferOAuthKeepsAKeyWithAHostOfItsOwn(t *testing.T) {
	key, err := mustService(t, "github").Credential() // github.com and api.github.com
	if err != nil {
		t.Fatal(err)
	}
	login := oauthCredentialFor(t, "gh-login", "api.github.com")
	kept, dropped := PreferOAuth([]Credential{key, login})
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v, want nothing", dropped)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d credentials, want 2", len(kept))
	}
}

func TestPreferOAuthIsANoopWithoutALogin(t *testing.T) {
	key, err := mustService(t, "groq").Credential()
	if err != nil {
		t.Fatal(err)
	}
	kept, dropped := PreferOAuth([]Credential{key})
	if len(kept) != 1 || len(dropped) != 0 {
		t.Fatalf("kept = %v, dropped = %v", kept, dropped)
	}
}

// Nothing the registry adds may put a credential in a message. The rows carry prefixes and
// lengths of *real* credentials, so a placeholder printed beside a value would be a way to
// learn the shape of the thing it stands for.
func TestServiceValuesNeverReachAnErrorOrALog(t *testing.T) {
	service, _ := Services().Lookup("anthropic")
	credential, err := service.Credential()
	if err != nil {
		t.Fatal(err)
	}
	// A store that does not hold the value, so the injector has to fail.
	inj, err := NewInjector(MapProvider{"something-else": canary}, credential)
	if err != nil {
		t.Fatal(err)
	}
	target, err := policy.NewTarget("api.anthropic.com", 443)
	if err != nil {
		t.Fatal(err)
	}
	_, err = inj.Apply(context.Background(), target, http.Header{}, FlowTLS)
	if err == nil {
		t.Fatal("a missing credential did not fail")
	}
	surfaces := []string{
		err.Error(),
		fmt.Sprint(service), fmt.Sprintf("%v", service), fmt.Sprintf("%+v", service),
		fmt.Sprint(credential), fmt.Sprintf("%v", credential), fmt.Sprintf("%+v", credential),
		fmt.Sprint(inj.Credentials()),
	}
	for _, s := range surfaces {
		if strings.Contains(s, canary) {
			t.Errorf("a credential value appeared in %q", s)
		}
	}
	// The failure still has to name the secret and the command that fixes it.
	for _, want := range []string{"anthropic", "boks secret set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q:\n%v", want, err)
		}
	}
}

func mustService(t *testing.T, name string) Service {
	t.Helper()
	s, err := Services().Resolve(name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// oauthCredentialFor builds a minimal OAuth credential for a resource host, for the
// precedence tests. It holds no token: PreferOAuth decides on destinations alone.
func oauthCredentialFor(t *testing.T, service, host string) Credential {
	t.Helper()
	pattern, err := policy.ParsePattern(host)
	if err != nil {
		t.Fatal(err)
	}
	return Credential{
		Service:      service,
		ProxyManaged: true,
		OAuth: &OAuth{
			TokenEndpoint: Endpoint{Host: "console.example.com", Path: "/oauth/token"},
			ResourceHosts: []policy.Pattern{pattern},
			Sentinels: Sentinels{
				Access:  NewSentinel("sk-ant-oat01-", service+"-access", 64),
				Refresh: NewSentinel("sk-ant-ort01-", service+"-refresh", 64),
			},
		},
	}
}
