package cli

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/kit"
	"github.com/dagsommer/boks/internal/secret"
)

const (
	kitAccessSentinel  = "kit-access-sentinel-0123456789"
	kitRefreshSentinel = "kit-refresh-sentinel-0123456789"
)

// kitOAuthCredential is a kit credential the way a spec file would declare it.
func kitOAuthCredential() kit.Credential {
	return kit.Credential{
		Service: "ghe-login",
		OAuth: &kit.OAuth{
			TokenEndpoint: kit.OAuthTokenEndpoint{Host: "auth.ghe.example", Path: "/oauth/token"},
			Sentinels:     kit.OAuthSentinels{AccessToken: kitAccessSentinel, RefreshToken: kitRefreshSentinel},
			ResourceHosts: []string{"api.ghe.example"},
			CredentialFile: &kit.OAuthCredentialFile{
				Path:     "~/.ghe/credentials.json",
				Template: `{"access":"{{.AccessToken}}","refresh":"{{.RefreshToken}}","scopes":{{.ScopesJSON}}}`,
			},
		},
	}
}

func kitWith(creds ...kit.Credential) *kit.Spec {
	return &kit.Spec{Name: "team", Credentials: creds}
}

func TestKitOAuthShapeMapsTheBlockOntoARecord(t *testing.T) {
	c := kitOAuthCredential()
	c.OAuth.ResponseFields = &kit.OAuthResponseFields{AccessToken: "token"}
	shapes, err := kitOAuthShapes(kitWith(c))
	if err != nil || len(shapes) != 1 {
		t.Fatalf("shapes = %+v, err = %v", shapes, err)
	}
	r := shapes[0].shape
	if r.TokenHost != "auth.ghe.example" || r.TokenPath != "/oauth/token" ||
		r.AccessSentinel != kitAccessSentinel || r.RefreshSentinel != kitRefreshSentinel ||
		len(r.ResourceHosts) != 1 || r.ResourceHosts[0] != "api.ghe.example" {
		t.Errorf("shape = %+v", r)
	}
	// A field the kit overrides is its name; one it leaves out is the RFC default.
	if r.ResponseAccess != "token" || r.ResponseRefresh != "refresh_token" || r.ResponseExpires != "expires_in" {
		t.Errorf("response fields = %q %q %q", r.ResponseAccess, r.ResponseRefresh, r.ResponseExpires)
	}
	// The kit's path is where its tool reads; the file goes where Boks can share it.
	if r.FilePath != secret.GuestCredentialDir+"/ghe-login-credentials.json" {
		t.Errorf("file path = %q", r.FilePath)
	}
}

func TestKitOAuthRefusesWhatItWillNotDo(t *testing.T) {
	cases := map[string]func(*kit.Credential){
		"passthrough": func(c *kit.Credential) { c.OAuth.Passthrough = true },
		"structure": func(c *kit.Credential) {
			c.OAuth.CredentialFile = &kit.OAuthCredentialFile{Path: "/x", Structure: map[string]any{"a": 1}}
		},
		"no hosts":        func(c *kit.Credential) { c.OAuth.ResourceHosts = nil },
		"wildcard host":   func(c *kit.Credential) { c.OAuth.ResourceHosts = []string{"*"} },
		"short sentinel":  func(c *kit.Credential) { c.OAuth.Sentinels.AccessToken = "short" },
		"template syntax": func(c *kit.Credential) { c.OAuth.CredentialFile.Template = "{{.Nope" },
		"unknown field":   func(c *kit.Credential) { c.OAuth.CredentialFile.Template = "{{.RealToken}}" },
	}
	for name, mutate := range cases {
		c := kitOAuthCredential()
		mutate(&c)
		_, err := kitOAuthShapes(kitWith(c))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), `"ghe-login"`) {
			t.Errorf("%s: the error does not name the credential: %v", name, err)
		}
	}
}

func TestKitOAuthAPIKeyInTheSameEntryWins(t *testing.T) {
	both := kitOAuthCredential()
	both.APIKey = &kit.APIKey{Name: "GHE_TOKEN", Inject: []kit.APIKeyInject{{Domain: "api.ghe.example", Scheme: "bearer"}}}
	spec := kitWith(both)

	shapes, err := kitOAuthShapes(spec)
	if err != nil || len(shapes) != 0 {
		t.Fatalf("the oauth half was applied beside an apiKey: %+v, %v", shapes, err)
	}
	// ...and the key half is what is rendered.
	if specs, err := kitInjectSpecs(spec); err != nil || len(specs) != 1 {
		t.Fatalf("inject specs = %v, %v", specs, err)
	}
	// A passthrough block beside a key is therefore never looked at: nothing of it is used.
	both.OAuth.Passthrough = true
	if _, err := kitOAuthShapes(kitWith(both)); err != nil {
		t.Errorf("an unused oauth block was judged: %v", err)
	}
}

// storedLoginFor stores a login whose shape differs from the kit's in every field the kit
// sets, so apply has something to replace.
func storedLoginFor(t *testing.T, store *secret.FileStore, name string) secret.OAuthRecord {
	t.Helper()
	storeLogin(t, store, name, "api.other.example")
	record, err := store.LookupOAuthRecord(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestKitOAuthApplyKeepsTheStoredTokens(t *testing.T) {
	store := secretStore(t)
	stored := storedLoginFor(t, store, "ghe-login")
	shapes, err := kitOAuthShapes(kitWith(kitOAuthCredential()))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := shapes[0].apply(stored)
	if err != nil {
		t.Fatal(err)
	}
	if merged.AccessToken != stored.AccessToken || merged.RefreshToken != stored.RefreshToken {
		t.Error("apply changed the tokens")
	}
	if merged.TokenHost != "auth.ghe.example" || merged.AccessSentinel != kitAccessSentinel ||
		merged.ResourceHosts[0] != "api.ghe.example" {
		t.Errorf("the kit's shape was not applied: %+v", merged)
	}
	// What the kit has no spelling for stays the stored login's.
	if merged.ClientID != stored.ClientID || merged.EnvName != stored.EnvName {
		t.Errorf("client id / env name were replaced: %q %q", merged.ClientID, merged.EnvName)
	}

	// The file the guest reads is rendered from the kit's template and holds sentinels only.
	c, err := merged.Credential()
	if err != nil {
		t.Fatal(err)
	}
	files, err := secret.CredentialFiles([]secret.Credential{c})
	if err != nil || len(files) != 1 {
		t.Fatalf("files = %v, %v", files, err)
	}
	content := string(files[0].Content)
	if !strings.Contains(content, kitAccessSentinel) || !strings.Contains(content, kitRefreshSentinel) {
		t.Errorf("the file lacks the kit's sentinels: %s", content)
	}
	if strings.Contains(content, stored.AccessToken) || strings.Contains(content, stored.RefreshToken) {
		t.Errorf("a real token reached the credential file: %s", content)
	}
	if !strings.HasPrefix(files[0].Path, secret.GuestCredentialDir+"/") {
		t.Errorf("file path = %q", files[0].Path)
	}
}

func TestKitOAuthFingerprintCoversEveryDeclaredField(t *testing.T) {
	base := func() kitOAuth {
		shapes, err := kitOAuthShapes(kitWith(kitOAuthCredential()))
		if err != nil {
			t.Fatal(err)
		}
		return shapes[0]
	}
	want := base().fingerprint()
	if base().fingerprint() != want {
		t.Fatal("the fingerprint is not stable")
	}
	cases := map[string]func(*kit.Credential){
		"resource host":  func(c *kit.Credential) { c.OAuth.ResourceHosts = append(c.OAuth.ResourceHosts, "x.ghe.example") },
		"token endpoint": func(c *kit.Credential) { c.OAuth.TokenEndpoint.Host = "evil.example" },
		"sentinel":       func(c *kit.Credential) { c.OAuth.Sentinels.AccessToken = kitAccessSentinel + "2" },
		"file template":  func(c *kit.Credential) { c.OAuth.CredentialFile.Template = `{{.AccessToken}}` },
		"file path":      func(c *kit.Credential) { c.OAuth.CredentialFile.Path = "/other.json" },
		"response field": func(c *kit.Credential) { c.OAuth.ResponseFields = &kit.OAuthResponseFields{AccessToken: "t"} },
	}
	for name, mutate := range cases {
		c := kitOAuthCredential()
		mutate(&c)
		shapes, err := kitOAuthShapes(kitWith(c))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if shapes[0].fingerprint() == want {
			t.Errorf("changing the %s did not change the fingerprint", name)
		}
	}
}

func TestKitOAuthIsAskedAboutAndNotAppliedWithoutApproval(t *testing.T) {
	f, stderr := kitFlags(t)
	shapes, _ := kitOAuthShapes(kitWith(kitOAuthCredential()))

	_, got, err := f.gateKit(soleUnit(f), nil, shapes, nil, stderr)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want nothing applied", got, err)
	}
	// The prompt says what kind of credential it is and every host it goes to, the token
	// endpoint included.
	for _, want := range []string{"ghe-login", "OAuth login", "api.ghe.example", "auth.ghe.example", "--approve-kit-credentials"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, stderr.String())
		}
	}

	f.approveKit = true
	if _, got, _ := f.gateKit(soleUnit(f), nil, shapes, nil, stderr); len(got) != 1 {
		t.Errorf("--approve-kit-credentials did not approve: %v", got)
	}
}

func TestKitOAuthApprovalIsForExactlyWhatWasShown(t *testing.T) {
	f, _ := kitFlags(t)
	shapes, _ := kitOAuthShapes(kitWith(kitOAuthCredential()))

	// The interactive prompt itself.
	asks := kitOAuthAsks(shapes, nil)
	var out bytes.Buffer
	answered, err := f.askKitCredentials(soleUnit(f), asks, bufio.NewReader(strings.NewReader("a\n")), &out)
	if err != nil || len(answered) != 1 {
		t.Fatalf("interactive approval: %v, %v", answered, err)
	}
	if !strings.Contains(out.String(), "OAuth login") {
		t.Errorf("the prompt does not say what kind it is:\n%s", out.String())
	}

	// Remembered: a later run with no terminal still applies it.
	approvals, _ := loadKitApprovals(kitApprovalsPath())
	approvals.record("team\x00./kit", answered[0])
	if err := approvals.save(); err != nil {
		t.Fatal(err)
	}
	if _, got, _ := f.gateKit(soleUnit(f), nil, shapes, nil, &bytes.Buffer{}); len(got) != 1 {
		t.Error("the approval was not remembered")
	}

	// A kit that later names another resource host is a different request.
	c := kitOAuthCredential()
	c.OAuth.ResourceHosts = []string{"api.ghe.example", "exfil.example"}
	widened, _ := kitOAuthShapes(kitWith(c))
	if _, got, _ := f.gateKit(soleUnit(f), nil, widened, nil, &bytes.Buffer{}); len(got) != 0 {
		t.Error("an approval carried over to a wider request")
	}

	// And approving the login is not approving an API key of the same name.
	approvals, _ = loadKitApprovals(kitApprovalsPath())
	key := "team\x00./kit"
	keyAsk := kitCredentialAsk{service: "ghe-login", specs: []string{"ghe-login@api.ghe.example=bearer"}}
	if approvals.approved(key, keyAsk) {
		t.Error("an oauth approval satisfied an API-key ask")
	}
}

func TestPlanCredentialsAppliesAnApprovedKitLogin(t *testing.T) {
	store := secretStore(t)
	stored := storedLoginFor(t, store, "ghe-login")
	f, stderr := kitFlags(t)
	f.kitSpec = kitWith(kitOAuthCredential())
	f.approveKit = true

	plan, err := f.planCredentials(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.oauth) != 1 || plan.oauth[0] != "ghe-login" || len(plan.kitOAuth) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	records := map[string]secret.OAuthRecord{"ghe-login": stored}
	if err := plan.applyKitOAuth(records); err != nil {
		t.Fatal(err)
	}
	if records["ghe-login"].AccessSentinel != kitAccessSentinel {
		t.Error("the kit's sentinel did not reach the record handed to the sandbox")
	}

	// The proxy path builds the same credential from the store.
	creds, err := oauthCredentials(t.Context(), store, plan.oauth, plan.kitOAuth)
	if err != nil || len(creds) != 1 || creds[0].OAuth.Sentinels.Access != kitAccessSentinel ||
		creds[0].OAuth.TokenEndpoint.Host != "auth.ghe.example" {
		t.Errorf("proxy credential = %+v, %v", creds, err)
	}
	_ = stderr
}

func TestPlanCredentialsLeavesAnUnapprovedOrUnstoredKitLoginOut(t *testing.T) {
	store := secretStore(t)
	storedLoginFor(t, store, "ghe-login")
	f, _ := kitFlags(t)
	f.kitSpec = kitWith(kitOAuthCredential())

	// Not approved, and no terminal to ask on.
	plan, err := f.planCredentials(store)
	if err != nil || len(plan.oauth) != 0 || len(plan.kitOAuth) != 0 {
		t.Fatalf("unapproved: %+v, %v", plan, err)
	}

	// Approved, but nothing is stored under the name: noted, and the run goes on.
	f.approveKit = true
	empty := secretStore(t)
	plan, err = f.planCredentials(empty)
	if err != nil || len(plan.oauth) != 0 || len(plan.notStored) != 1 {
		t.Fatalf("not stored: %+v, %v", plan, err)
	}
	var out bytes.Buffer
	plan.describe(&out)
	if !strings.Contains(out.String(), "boks secret adopt ghe-login") {
		t.Errorf("the note does not say how to store one:\n%s", out.String())
	}

}

func TestPlanCredentialsUserOAuthFlagWinsOverTheKit(t *testing.T) {
	store := secretStore(t)
	storedLoginFor(t, store, "ghe-login")
	f, _ := kitFlags(t)
	f.kitSpec = kitWith(kitOAuthCredential())
	f.approveKit = true
	f.oauth = []string{"ghe-login"}

	plan, err := f.planCredentials(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.kitOAuth) != 0 || len(plan.oauth) != 1 {
		t.Errorf("the kit reshaped a login the user named: %+v", plan)
	}
}
