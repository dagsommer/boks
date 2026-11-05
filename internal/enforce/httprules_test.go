package enforce

import (
	"testing"

	"github.com/dagsommer/boks/internal/policy"
)

// TestHTTPRulesMakeASandboxIntercept: an HTTP policy rule needs the flow read just as a
// credential does, so a sandbox whose policy holds one gets the CA — and one with neither
// still gets none.
func TestHTTPRulesMakeASandboxIntercept(t *testing.T) {
	plain := Spec{Intercept: true, Resolution: &policy.Resolution{Rules: []policy.RuleSpec{
		{Action: policy.Allow, Spec: "api.test"},
	}}}
	if plain.intercepts() {
		t.Error("a sandbox with neither credentials nor HTTP rules intercepts")
	}
	withHTTP := Spec{Intercept: true, Resolution: &policy.Resolution{Rules: []policy.RuleSpec{
		{Action: policy.Deny, Spec: "api.test", Methods: []string{"POST"}},
	}}}
	if !withHTTP.intercepts() {
		t.Error("a sandbox with an HTTP rule does not intercept; its HTTPS requests could not be judged")
	}
	withHTTP.Intercept = false
	if withHTTP.intercepts() {
		t.Error("Intercept=false must still mean no CA")
	}
}
