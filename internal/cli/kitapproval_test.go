package cli

import (
	"bufio"
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/kit"
	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

var testKitSpecs = []string{
	"ghe-token@api.ghe.example,ghe.example=bearer",
	"confluence-token@wiki.example=Authorization:Bearer %s",
}

// soleUnit is the one kit of a flags value built with a spec and no composition.
func soleUnit(f *policyFlags) kitUnit { return f.kitUnits()[0] }

func kitFlags(t *testing.T) (*policyFlags, *bytes.Buffer) {
	t.Helper()
	t.Setenv(policy.StateDirEnv, t.TempDir())
	var stderr bytes.Buffer
	return &policyFlags{kitRefs: []string{"./kit"}, kitSpec: &kit.Spec{Name: "team"}}, &stderr
}

func TestKitCredentialAsksGroupByService(t *testing.T) {
	specs := append(slices.Clone(testKitSpecs), "ghe-token@npm.ghe.example=bearer")
	asks, err := kitCredentialAsks(specs, map[string]bool{"ghe-token": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(asks) != 2 || asks[0].service != "ghe-token" || !asks[0].stored || asks[1].stored {
		t.Fatalf("asks = %+v", asks)
	}
	if got := strings.Join(asks[0].hosts, ","); got != "api.ghe.example,ghe.example,npm.ghe.example" {
		t.Errorf("hosts = %s", got)
	}
}

func TestAskKitCredentialsAnswers(t *testing.T) {
	f, _ := kitFlags(t)
	asks, _ := kitCredentialAsks(testKitSpecs, nil)
	cases := map[string]int{"a\n": 2, "approve all\n": 2, "r\ny\nn\n": 1, "r\nn\ny\n": 1, "n\n": 0, "\n": 0, "": 0, "yes\n": 0}
	for input, want := range cases {
		var out bytes.Buffer
		got, err := f.askKitCredentials(soleUnit(f), asks, bufio.NewReader(strings.NewReader(input)), &out)
		if err != nil || len(got) != want {
			t.Errorf("input %q: approved %d, err %v; want %d", input, len(got), err, want)
		}
	}
}

func TestGateWithoutTerminalLeavesCredentialsOut(t *testing.T) {
	f, stderr := kitFlags(t)
	got, err := f.gateKitCredentials(soleUnit(f), testKitSpecs, nil, stderr)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want nothing attached", got, err)
	}
	for _, want := range []string{"ghe-token", "ghe.example", "not set", "--approve-kit-credentials"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, stderr.String())
		}
	}
}

func TestGateFlagApprovesOnlyThisRun(t *testing.T) {
	f, stderr := kitFlags(t)
	f.approveKit = true
	got, _ := f.gateKitCredentials(soleUnit(f), testKitSpecs, nil, stderr)
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	f.approveKit = false
	if got, _ := f.gateKitCredentials(soleUnit(f), testKitSpecs, nil, stderr); len(got) != 0 {
		t.Errorf("the flag was remembered: %v", got)
	}
}

func TestGateRemembersAnApprovalUntilTheKitAsksForSomethingElse(t *testing.T) {
	f, stderr := kitFlags(t)
	asks, _ := kitCredentialAsks(testKitSpecs, nil)
	approvals, _ := loadKitApprovals(kitApprovalsPath())
	key := "team\x00./kit"
	approvals.record(key, asks[0])
	if err := approvals.save(); err != nil {
		t.Fatal(err)
	}

	got, _ := f.gateKitCredentials(soleUnit(f), testKitSpecs, nil, stderr)
	if len(got) != 1 || !strings.HasPrefix(got[0], "ghe-token@") {
		t.Fatalf("approved service only: got %v", got)
	}

	// The same kit now also sends the token to another host: a different request.
	widened := append([]string{"ghe-token@evil.example=bearer"}, testKitSpecs...)
	if got, _ := f.gateKitCredentials(soleUnit(f), widened, nil, stderr); len(got) != 0 {
		t.Errorf("a widened request was covered by the old approval: %v", got)
	}

	// And another kit, or the same name from elsewhere, has no approval at all.
	f.kitRefs = []string{"git+https://elsewhere/kit.git"}
	if got, _ := f.gateKitCredentials(soleUnit(f), testKitSpecs, nil, stderr); len(got) != 0 {
		t.Errorf("approval leaked to another reference: %v", got)
	}
}

func TestKitGuestSpecsMintAPlaceholderPerProxyManagedCredential(t *testing.T) {
	spec := &kit.Spec{Name: "team", Credentials: []kit.Credential{
		{Service: "ghe-token", APIKey: &kit.APIKey{Name: "GHE_TOKEN", ProxyManaged: true}},
		{Service: "anthropic", APIKey: &kit.APIKey{Name: "ANTHROPIC_API_KEY", ProxyManaged: true}},
		{Service: "quiet", APIKey: &kit.APIKey{Name: "QUIET"}},
		{Service: "nameless", APIKey: &kit.APIKey{ProxyManaged: true}},
	}}
	got := kitGuestSpecs(spec)
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	_, env, placeholder, err := secret.ParseGuestCredential(got[0])
	if err != nil || env != "GHE_TOKEN" || len(placeholder) != 40 || !strings.Contains(placeholder, "boksproxymanaged") {
		t.Errorf("unknown service: %q %q %v", env, placeholder, err)
	}
	if _, _, placeholder, _ := secret.ParseGuestCredential(got[1]); !strings.HasPrefix(placeholder, "sk-ant-") {
		t.Errorf("a known service should use its registry shape: %q", placeholder)
	}
}

func TestPlanCredentialsGivesAnUnapprovedKitCredentialNoGuestVariable(t *testing.T) {
	f, _ := kitFlags(t)
	f.kitSpec = &kit.Spec{Name: "team", Credentials: []kit.Credential{{
		Service: "ghe-token",
		APIKey: &kit.APIKey{Name: "GHE_TOKEN", ProxyManaged: true,
			Inject: []kit.APIKeyInject{{Domain: "ghe.example", Scheme: "bearer"}}},
	}}}
	plan, err := f.planCredentials(nil)
	if err != nil || len(plan.inject) != 0 || len(plan.guest) != 0 {
		t.Fatalf("unapproved: %+v, %v", plan, err)
	}
	f.approveKit = true
	plan, err = f.planCredentials(nil)
	if err != nil || len(plan.inject) != 1 || len(plan.guest) != 1 {
		t.Fatalf("approved: %+v, %v", plan, err)
	}
}
