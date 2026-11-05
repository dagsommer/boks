package policy

import (
	"os"
	"strings"
	"testing"
)

func mustHTTPRule(t *testing.T, action Action, spec string, methods []string, path string, query ...string) Rule {
	t.Helper()
	r, err := ParseHTTPRule(action, spec, methods, path, query)
	if err != nil {
		t.Fatalf("ParseHTTPRule(%s %s %v %q %v): %v", action, spec, methods, path, query, err)
	}
	return r
}

// TestHTTPRuleValidation pins the refusals, with sbx's own messages where sbx has one.
func TestHTTPRuleValidation(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		methods []string
		path    string
		query   []string
		want    string
	}{
		{"path without method", "api.test", nil, "/v1/**", nil, "--path applies only to HTTP rules; use --method ANY to match all methods"},
		{"query without method", "api.test", nil, "", []string{"a=1"}, "--query applies only to HTTP rules"},
		{"unknown method", "api.test", []string{"FETCH"}, "", nil, `unknown HTTP method "FETCH": must be one of GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS, CONNECT, TRACE, ANY`},
		{"empty method", "api.test", []string{"GET,"}, "", nil, "HTTP method must not be empty"},
		{"relative path", "api.test", []string{"GET"}, "v1/**", nil, `http path must start with "/"`},
		{"query in path", "api.test", []string{"GET"}, "/v1?a=1", nil, "http path must not contain a query or fragment"},
		{"fragment in path", "api.test", []string{"GET"}, "/v1#x", nil, "http path must not contain a query or fragment"},
		{"percent in path", "api.test", []string{"GET"}, "/v1/%2e", nil, "http path must not contain percent-encoding"},
		{"dotdot", "api.test", []string{"GET"}, "/v1/../admin", nil, `http path must not contain ".." segments`},
		{"dot", "api.test", []string{"GET"}, "/v1/./x", nil, "http path must be canonical"},
		{"repeated slash", "api.test", []string{"GET"}, "/v1//x", nil, "http path must be canonical"},
		{"trailing slash", "api.test", []string{"GET"}, "/v1/", nil, "http path must be canonical"},
		{"ip resource", "203.0.113.7", []string{"GET"}, "", nil, `HTTP rule resource "203.0.113.7" must be a host pattern, not an IP address`},
		{"cidr resource", "10.0.0.0/8", []string{"GET"}, "", nil, "must be a host pattern, not an IP address"},
		{"query without =", "api.test", []string{"GET"}, "", []string{"team"}, "must be name=value"},
		{"query empty name", "api.test", []string{"GET"}, "", []string{"=x"}, "--query name must be non-empty and must not contain '*'"},
		{"query star name", "api.test", []string{"GET"}, "", []string{"t*=x"}, "--query name must be non-empty and must not contain '*'"},
		{"query bad name", "api.test", []string{"GET"}, "", []string{"a%zz=x"}, "--query name must be a valid URL-query-encoded name without '='"},
		{"query backslash", "api.test", []string{"GET"}, "", []string{`a=x\y`}, `query value pattern must percent-encode "\" as %5C`},
		{"query bad utf8", "api.test", []string{"GET"}, "", []string{"a=%ff"}, "query value pattern must decode to valid UTF-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseHTTPRule(Allow, tc.spec, tc.methods, tc.path, tc.query)
			if err == nil {
				t.Fatalf("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestHTTPRuleCanonicalForm(t *testing.T) {
	r := mustHTTPRule(t, Allow, "API.test:443", []string{"head", "GET", "get"}, "", "team=platform", "q=a+b*")
	if got, want := r.Describe(), "api.test:443 --method GET,HEAD --path '/**' --query 'q=a+b*' --query team=platform"; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}
	anyRule := mustHTTPRule(t, Deny, "api.test", []string{"GET", "any"}, "/x")
	if got := anyRule.HTTP.Methods(); len(got) != 1 || got[0] != MethodAny {
		t.Errorf("ANY beside other methods should be ANY, got %v", got)
	}
	// A literal star in a query value is written %2A and stays distinct from a wildcard.
	lit := mustHTTPRule(t, Allow, "api.test", []string{"GET"}, "", "q=%2A")
	if got := lit.HTTP.Query()[0]; got != "q=%2A" {
		t.Errorf("literal star rendered as %q", got)
	}
}

// TestPathGlobs: '*' stays inside one segment, '**' spans any number of them, including none.
func TestPathGlobs(t *testing.T) {
	cases := []struct {
		glob string
		path string
		want bool
	}{
		{"/**", "/", true},
		{"/**", "/a/b/c", true},
		{"/v1/**", "/v1", true},
		{"/v1/**", "/v1/", true},
		{"/v1/**", "/v1/items/7", true},
		{"/v1/**", "/v10/items", false},
		{"/v1/*", "/v1/items", true},
		{"/v1/*", "/v1/items/7", false},
		{"/v1/*/x", "/v1/a/x", true},
		{"/v1/*/x", "/v1/a/b/x", false},
		{"/v1/**/x", "/v1/a/b/x", true},
		{"/v1/**/x", "/v1/x", true},
		{"/repos/*/*/pulls", "/repos/o/r/pulls", true},
		{"/files/*.json", "/files/a.json", true},
		{"/files/*.json", "/files/a.yaml", false},
		{"/exact", "/exact", true},
		{"/exact", "/exact/more", false},
		{"/exact", "/Exact", false},
	}
	for _, tc := range cases {
		g, err := parsePathGlob(tc.glob)
		if err != nil {
			t.Fatalf("parsePathGlob(%q): %v", tc.glob, err)
		}
		if got := g.match(tc.path); got != tc.want {
			t.Errorf("%q matching %q = %v, want %v", tc.glob, tc.path, got, tc.want)
		}
	}
}

// TestRequestPathIsCanonicalised is the bypass test: every spelling an origin might route to
// /admin must be seen as /admin by a deny, and none of them may be seen inside /v1 by an allow.
func TestRequestPathIsCanonicalised(t *testing.T) {
	deny := mustHTTPRule(t, Deny, "api.test", []string{"ANY"}, "/admin/**")
	allow := mustHTTPRule(t, Allow, "api.test", []string{"ANY"}, "/v1/**")
	tgt := target(t, "api.test", 443)
	for _, p := range []string{
		"/admin", "/admin/", "/admin/x", "//admin/x", "/./admin", "/v1/../admin", "/v1/%2e%2e/admin",
		"/v1%2F..%2Fadmin", `/v1\..\admin`, "/%61dmin", "/ADMIN/x", "/v1/x/../../admin",
	} {
		req := HTTPRequest{Method: "GET", Path: p}
		if !deny.MatchRequest(tgt, req) {
			t.Errorf("deny /admin/** did not match %q (canonical %q)", p, req.CanonicalPath())
		}
		if allow.MatchRequest(tgt, req) {
			t.Errorf("allow /v1/** matched %q (canonical %q)", p, req.CanonicalPath())
		}
	}
	// And an ordinary request is what it looks like.
	if !allow.MatchRequest(tgt, HTTPRequest{Method: "GET", Path: "/v1/items"}) {
		t.Error("allow /v1/** did not match /v1/items")
	}
	// Case is folded for a deny only: an allow does not widen to /V1.
	if allow.MatchRequest(tgt, HTTPRequest{Method: "GET", Path: "/V1/items"}) {
		t.Error("allow /v1/** matched /V1/items")
	}
}

func TestMethodMatching(t *testing.T) {
	r := mustHTTPRule(t, Allow, "api.test", []string{"GET,HEAD"}, "")
	tgt := target(t, "api.test", 443)
	for method, want := range map[string]bool{"GET": true, "get": true, "HEAD": true, "POST": false, "DELETE": false} {
		if got := r.MatchRequest(tgt, HTTPRequest{Method: method, Path: "/"}); got != want {
			t.Errorf("GET,HEAD matching %s = %v, want %v", method, got, want)
		}
	}
	anyRule := mustHTTPRule(t, Allow, "api.test", []string{"ANY"}, "")
	if !anyRule.MatchRequest(tgt, HTTPRequest{Method: "PROPFIND", Path: "/"}) {
		t.Error("ANY did not match an unusual method")
	}
}

func TestQueryMatching(t *testing.T) {
	tgt := target(t, "api.test", 443)
	allow := mustHTTPRule(t, Allow, "api.test", []string{"ANY"}, "", "team=platform", "q=*")
	deny := mustHTTPRule(t, Deny, "api.test", []string{"ANY"}, "", "team=platform")
	cases := []struct {
		query      string
		allowWants bool
		denyWants  bool
	}{
		{"team=platform&q=x", true, true},
		{"q=x&team=platform&other=1", true, true},
		{"team=platform&q=", true, true},              // q=* matches the empty value
		{"team=platform", false, true},                // q is required
		{"team=other&q=x", false, false},              // wrong value
		{"team=platform&team=other&q=x", false, true}, // repeated: allow needs all, deny any
		{"team=platform;evil=1&q=x", false, true},     // ';' reading: allow needs both readings
		{"team=plat%66orm&q=x", true, true},           // decoded before matching
		{"team=platform&q=%zz", false, true},          // undecodable: allow no, deny yes
		{"TEAM=platform&q=x", false, false},           // names are case-sensitive
	}
	for _, tc := range cases {
		req := HTTPRequest{Method: "GET", Path: "/", RawQuery: tc.query}
		if got := allow.MatchRequest(tgt, req); got != tc.allowWants {
			t.Errorf("allow matching %q = %v, want %v", tc.query, got, tc.allowWants)
		}
		if got := deny.MatchRequest(tgt, req); got != tc.denyWants {
			t.Errorf("deny matching %q = %v, want %v", tc.query, got, tc.denyWants)
		}
	}

	glob := mustHTTPRule(t, Allow, "api.test", []string{"GET"}, "", "name=ab*cd", "lit=%2A", "sp=a+b")
	for q, want := range map[string]bool{
		"name=abXXcd&lit=*&sp=a+b":        true,
		"name=abcd&lit=%2A&sp=a%20b":      true,
		"name=abXXce&lit=*&sp=a+b":        false,
		"name=abXXcd&lit=anything&sp=a+b": false, // %2A is a literal star, not a wildcard
	} {
		if got := glob.MatchRequest(tgt, HTTPRequest{Method: "GET", Path: "/", RawQuery: q}); got != want {
			t.Errorf("glob query matching %q = %v, want %v", q, got, want)
		}
	}
}

// TestHTTPRulesNeverOpenRawAccess is the property the whole design rests on: an HTTP allow does
// not permit a connection Boks cannot read, and an HTTP deny on an otherwise allowed host
// refuses one.
func TestHTTPRulesNeverOpenRawAccess(t *testing.T) {
	tgt := target(t, "api.test", 443)

	httpOnly := Policy{Name: "t", Default: Deny, Rules: []Rule{
		mustHTTPRule(t, Allow, "api.test", []string{"GET"}, "/v1/**"),
	}}
	v := httpOnly.Evaluate(tgt)
	if v.Allowed || !v.NeedsHTTP {
		t.Fatalf("a GET-only allow permitted a raw connection: %+v", v)
	}
	if !strings.Contains(v.Reason, "cannot read as HTTP") {
		t.Errorf("the refusal does not explain itself: %s", v.Reason)
	}
	if got := httpOnly.EvaluateHTTP(tgt, HTTPRequest{Method: "GET", Path: "/v1/x"}); !got.Allowed {
		t.Errorf("the matching request was refused: %+v", got)
	}
	if got := httpOnly.EvaluateHTTP(tgt, HTTPRequest{Method: "POST", Path: "/v1/x"}); got.Allowed {
		t.Errorf("a POST passed a GET-only allow: %+v", got)
	}
	if got := httpOnly.EvaluateHTTP(tgt, HTTPRequest{Method: "GET", Path: "/v2/x"}); got.Allowed {
		t.Errorf("a path outside the allow passed: %+v", got)
	}

	allowedButHTTPDeny := Policy{Name: "t", Default: Deny, Rules: []Rule{
		MustRule(Allow, "api.test", ""),
		mustHTTPRule(t, Deny, "api.test", []string{"POST"}, "/admin/**"),
	}}
	v = allowedButHTTPDeny.Evaluate(tgt)
	if v.Allowed || !v.NeedsHTTP {
		t.Fatalf("an HTTP deny on an allowed host let a raw connection through: %+v", v)
	}
	if got := allowedButHTTPDeny.EvaluateHTTP(tgt, HTTPRequest{Method: "POST", Path: "/admin/users"}); got.Allowed {
		t.Errorf("the denied request was allowed: %+v", got)
	}
	if got := allowedButHTTPDeny.EvaluateHTTP(tgt, HTTPRequest{Method: "GET", Path: "/admin/users"}); !got.Allowed {
		t.Errorf("a request the deny does not cover was refused: %+v", got)
	}

	// The same with a default-allow policy: the HTTP deny still makes a raw flow a refusal.
	open := Policy{Name: "t", Default: Allow, Rules: []Rule{mustHTTPRule(t, Deny, "api.test", []string{"POST"}, "")}}
	if v := open.Evaluate(tgt); v.Allowed || !v.NeedsHTTP {
		t.Errorf("default allow with an HTTP deny let a raw flow through: %+v", v)
	}
	if v := open.Evaluate(target(t, "other.test", 443)); !v.Allowed {
		t.Errorf("an HTTP deny for one host affected another: %+v", v)
	}

	// Deny wins over an HTTP allow whichever kind the deny is.
	both := Policy{Name: "t", Default: Deny, Rules: []Rule{
		mustHTTPRule(t, Allow, "api.test", []string{"ANY"}, ""),
		MustRule(Deny, "api.test", ""),
	}}
	if v := both.Evaluate(tgt); v.Allowed || v.NeedsHTTP {
		t.Errorf("a connection deny did not win outright over an HTTP allow: %+v", v)
	}
	if v := both.EvaluateHTTP(tgt, HTTPRequest{Method: "GET", Path: "/"}); v.Allowed {
		t.Errorf("a connection deny did not win over an HTTP allow for a request: %+v", v)
	}
	overlap := Policy{Name: "t", Default: Deny, Rules: []Rule{
		mustHTTPRule(t, Allow, "*.test", []string{"ANY"}, "/**"),
		mustHTTPRule(t, Deny, "api.test", []string{"DELETE"}, "/**"),
	}}
	if v := overlap.EvaluateHTTP(tgt, HTTPRequest{Method: "DELETE", Path: "/x"}); v.Allowed {
		t.Errorf("an HTTP deny did not win over an HTTP allow: %+v", v)
	}
}

// TestHTTPRulesIgnoreAddresses: an HTTP rule is about named hosts; even "**" never covers a flow
// addressed by IP, which carries no request Boks reads, and the resolved-address check ignores
// HTTP rules entirely.
func TestHTTPRulesIgnoreAddresses(t *testing.T) {
	p := Policy{Name: "t", Default: Deny, Rules: []Rule{
		MustRule(Allow, "10.0.0.0/8", ""),
		mustHTTPRule(t, Deny, "**", []string{"POST"}, ""),
	}}
	ip := target(t, "10.1.2.3", 22)
	if v := p.Evaluate(ip); !v.Allowed {
		t.Errorf("an HTTP deny on ** refused a raw flow to an allowed address: %+v", v)
	}
	if v := p.EvaluateDeny(ip); !v.Allowed {
		t.Errorf("the resolved-address check applied an HTTP rule: %+v", v)
	}
	if !p.HasHTTPRules(target(t, "api.test", 443)) || p.HasHTTPRules(ip) {
		t.Error("HasHTTPRules disagrees with Match")
	}
}

// TestEngineDefersAndRecordsRequests: the engine lets a readable flow through with Deferred set,
// refuses an unreadable one, and records method and path only for a host with HTTP rules —
// never the query.
func TestEngineDefersAndRecordsRequests(t *testing.T) {
	p := Policy{Name: "t", Default: Deny, Rules: []Rule{
		mustHTTPRule(t, Allow, "api.test", []string{"GET"}, "/v1/**"),
		MustRule(Allow, "plain.test", ""),
	}}
	e := NewEngine(p, NewLog(16))
	tgt := target(t, "api.test", 443)

	if d := e.CheckFlow(StageConnect, tgt, ModeForwardBypass, false); d.Allowed || d.Deferred {
		t.Errorf("an unreadable flow to an HTTP-only host was let through: %+v", d)
	}
	d := e.CheckFlow(StageConnect, tgt, ModeForward, true)
	if !d.Allowed || !d.Deferred {
		t.Errorf("a readable flow was not deferred to its requests: %+v", d)
	}
	d = e.CheckRequest(StageRequest, tgt, ModeForward, HTTPRequest{Method: "get", Path: "/v1/%69tems", RawQuery: "token=CANARY"})
	if !d.Allowed || d.Method != "GET" || d.Path != "/v1/items" || d.Action != ActionHTTPRequest {
		t.Errorf("request decision = %+v", d)
	}
	if strings.Contains(d.String(), "CANARY") || strings.Contains(d.Path, "CANARY") {
		t.Errorf("the query reached the log: %s", d)
	}
	d = e.CheckRequest(StageRequest, tgt, ModeForward, HTTPRequest{Method: "POST", Path: "/v1/items"})
	if d.Allowed {
		t.Errorf("a POST was allowed by a GET rule: %+v", d)
	}
	plain := e.CheckRequest(StageHTTP, target(t, "plain.test", 80), ModeForward, HTTPRequest{Method: "GET", Path: "/secret-path"})
	if !plain.Allowed || plain.Method != "" || plain.Path != "" {
		t.Errorf("a host without HTTP rules had its request recorded: %+v", plain)
	}
}

// TestStoreRoundTripWithHTTPRules: the selector survives the file, the file says version 2, and a
// store without HTTP rules stays version 1 (covered in TestStoreRoundTrip).
func TestStoreRoundTripWithHTTPRules(t *testing.T) {
	s := tempStore(t)
	mustAdd(t, s, GlobalScope(), RuleSpec{Action: Allow, Spec: "api.test", Methods: []string{"GET", "HEAD"}, Path: "/v1/**", Query: []string{"team=platform"}})
	mustAdd(t, s, SandboxScope("web"), RuleSpec{Action: Deny, Spec: "api.test", Methods: []string{"POST"}, Path: "/admin/**"})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"version": 2`, `"methods": [`, `"path": "/v1/**"`, `"query": [`, `"team=platform"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("stored file does not contain %s:\n%s", want, data)
		}
	}
	got, err := LoadStore(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	r, err := got.Global[0].Rule()
	if err != nil {
		t.Fatal(err)
	}
	if r.HTTP == nil || r.Describe() != "api.test --method GET,HEAD --path '/v1/**' --query team=platform" {
		t.Errorf("selector lost in the round trip: %s", r.Describe())
	}

	// Resolution carries the selector to the sandbox's engine.
	res, err := (Request{Store: got, Sandbox: "web"}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := res.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if v := pol.EvaluateHTTP(target(t, "api.test", 443), HTTPRequest{Method: "POST", Path: "/admin/x", RawQuery: "team=platform"}); v.Allowed {
		t.Errorf("the sandbox-scoped HTTP deny did not survive resolution: %+v", v)
	}
	if !strings.Contains(res.Describe(), "--method POST --path '/admin/**'") {
		t.Errorf("Describe does not show the selector:\n%s", res.Describe())
	}

	// A hand-edited store with a bad selector is refused whole, like any other bad rule.
	bad := `{"version": 2, "global": [{"action": "deny", "spec": "api.test", "path": "/x"}]}`
	if err := os.WriteFile(s.Path(), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStore(s.Path()); err == nil || !strings.Contains(err.Error(), "unusable rule") {
		t.Errorf("a selector without a method was accepted: %v", err)
	}
}
