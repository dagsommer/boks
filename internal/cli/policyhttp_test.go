package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPolicyHTTPRulesRoundTrip: the sbx flags add, list, check and remove an HTTP rule.
func TestPolicyHTTPRulesRoundTrip(t *testing.T) {
	policyState(t)

	out := mustPolicy(t, "allow", "api.example.com", "--method", "get,head", "--path", "/v1/**", "--query", "team=platform")
	if !strings.Contains(out, "added: allow api.example.com --method GET,HEAD --path '/v1/**' --query team=platform to global") {
		t.Errorf("adding an HTTP rule said:\n%s", out)
	}
	mustPolicy(t, "deny", "api.example.com", "--method", "POST", "--path", "/admin/**")

	out = mustPolicy(t, "ls", "--stored")
	for _, want := range []string{
		"allow api.example.com --method GET,HEAD --path '/v1/**' --query team=platform",
		"deny  api.example.com --method POST --path '/admin/**'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("policy ls --stored is missing %q:\n%s", want, out)
		}
	}

	out = mustPolicy(t, "ls", "--wide", "--stored")
	if !strings.Contains(out, "METHODS") || !strings.Contains(out, "GET,HEAD") || !strings.Contains(out, "team=platform") {
		t.Errorf("policy ls --wide does not show the selector columns:\n%s", out)
	}

	var listing struct {
		Stored []struct {
			Decision, Resource, Path string
			Methods, Query           []string
		} `json:"stored"`
		Effective []struct{ Scope, Resource string } `json:"effective"`
	}
	if err := json.Unmarshal([]byte(mustPolicy(t, "ls", "--json")), &listing); err != nil {
		t.Fatalf("policy ls --json is not JSON: %v", err)
	}
	if len(listing.Stored) != 2 || listing.Stored[0].Path != "/v1/**" || len(listing.Stored[0].Methods) != 2 ||
		listing.Stored[0].Query[0] != "team=platform" {
		t.Errorf("policy ls --json lost the selector: %+v", listing.Stored)
	}
	if len(listing.Effective) == 0 {
		t.Error("policy ls --json has no effective rules")
	}

	// check: a URL is a request; a bare host is a connection, which the HTTP rules refuse.
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"check", "https://api.example.com/v1/items?team=platform"}, "ALLOW GET api.example.com:443 /v1/items"},
		{[]string{"check", "--method", "head", "https://api.example.com/v1/items?team=platform"}, "ALLOW HEAD"},
		{[]string{"check", "--method", "POST", "https://api.example.com/v1/items?team=platform"}, "DENY  POST"},
		{[]string{"check", "https://api.example.com/v1/items?team=other"}, "DENY  GET"},
		{[]string{"check", "--method", "POST", "https://api.example.com/v1/../admin/x"}, "denied by HTTP rule"},
		{[]string{"check", "http", "https://api.example.com/v1/x?team=platform"}, "ALLOW GET"},
		{[]string{"check", "network", "https://api.example.com/v1/x"}, "refused"},
		{[]string{"check", "api.example.com"}, "cannot read as HTTP"},
	}
	for _, tc := range cases {
		if out := mustPolicy(t, tc.args...); !strings.Contains(out, tc.want) {
			t.Errorf("policy %s:\n%s\nwant %q", strings.Join(tc.args, " "), out, tc.want)
		}
	}
	if _, _, err := runCLI(t, "", "policy", "check", "--method", "GET", "api.example.com"); err == nil {
		t.Error("--method with a bare host was accepted")
	}
	if _, _, err := runCLI(t, "", "policy", "check", "http", "api.example.com"); err == nil {
		t.Error("check http without a URL was accepted")
	}

	// rm needs the selector; without it the error names the command that works.
	_, errOut, err := runCLI(t, "", "policy", "rm", "api.example.com")
	if err == nil || !strings.Contains(errOut+err.Error(), "boks policy rm --action allow api.example.com --method GET,HEAD") {
		t.Errorf("rm by destination alone: %v\n%s", err, errOut)
	}
	out = mustPolicy(t, "rm", "api.example.com", "--method", "HEAD,GET", "--path", "/v1/**", "--query", "team=platform")
	if !strings.Contains(out, "removed: allow api.example.com --method GET,HEAD") {
		t.Errorf("rm with the selector said:\n%s", out)
	}
}

// TestPolicyHTTPRuleRefusals: sbx's validation and the opposite-decision conflict, through the CLI.
func TestPolicyHTTPRuleRefusals(t *testing.T) {
	policyState(t)
	refused := []struct {
		args []string
		want string
	}{
		{[]string{"allow", "api.example.com", "--path", "/v1/**"}, "--path applies only to HTTP rules; use --method ANY to match all methods"},
		{[]string{"allow", "api.example.com", "--method", "FETCH"}, `unknown HTTP method "FETCH"`},
		{[]string{"allow", "10.0.0.0/8", "--method", "GET"}, "must be a host pattern, not an IP address"},
		{[]string{"allow", "api.example.com", "--method", "GET", "--path", "/v1/../x"}, `http path must not contain ".." segments`},
	}
	for _, tc := range refused {
		_, errOut, err := runCLI(t, "", append([]string{"policy"}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error()+errOut, tc.want) {
			t.Errorf("policy %s: %v\n%s\nwant %q", strings.Join(tc.args, " "), err, errOut, tc.want)
		}
	}

	mustPolicy(t, "allow", "api.example.com", "--method", "GET")
	_, errOut, err := runCLI(t, "", "policy", "deny", "api.example.com", "--method", "GET,POST")
	if err == nil || !strings.Contains(err.Error()+errOut, "conflicts with existing allow rule") {
		t.Errorf("an opposite rule in the same scope was not refused: %v\n%s", err, errOut)
	}
	// Elsewhere it is fine, and so is a connection rule beside an HTTP rule.
	mustPolicy(t, "deny", "--sandbox", "web", "api.example.com", "--method", "GET")
	mustPolicy(t, "deny", "api.example.com", "--method", "DELETE")
}
