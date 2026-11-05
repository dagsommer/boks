package policy

// HTTP rules: a rule that applies to some requests to a host rather than to every connection.
//
// # What sbx does, and what is copied
//
// Docker Sandboxes (sbx 0.50+) lets a network rule carry an HTTP selector:
//
//	sbx policy allow network api.example.com --method GET,HEAD --path '/v1/**'
//	sbx policy deny  network api.example.com --method POST --path '/admin/**'
//	sbx policy allow network api.example.com --method ANY --query team=platform
//
// `--method` is what makes a rule an HTTP rule: `--path` and `--query` without it are refused,
// and `--method ANY` is how to say "every method". The flag names, the default path (`/**`) and
// the validation messages below are sbx's, read out of `sbx policy allow network --help` and the
// sbx 0.51 binary. What could not be read there — the exact method list, how a repeated query
// parameter is treated — is decided conservatively and said so where it is decided.
//
// # The one rule that matters: an HTTP rule never opens raw access
//
// An HTTP rule can only be applied to a request Boks can read: plaintext HTTP through the proxy,
// or HTTPS that the proxy terminates. Everything else — a CONNECT tunnel carried blind, a raw
// socket judged in the network stack — has no method and no path to look at. For those flows
// a host with HTTP rules is decided like this (see Policy.Evaluate):
//
//   - an HTTP **allow** is not an allow. A host reachable only through `allow --method GET` is
//     unreachable to a flow that cannot be read as HTTP, so a GET-only rule cannot be used to
//     open, say, a WebSocket or an SSH session to the same host;
//   - an HTTP **deny** is a deny. A host that is otherwise allowed but carries
//     `deny --method POST` cannot be reached by a flow whose POSTs Boks could not see, because
//     the deny could not be enforced on it and deny always wins.
//
// Either way the verdict is "refused at the connection, unless the flow can be inspected", and
// Verdict.NeedsHTTP says which of the two it was. The proxy inspects such flows (it terminates
// TLS for a host with HTTP rules, as it does for a host with a credential) and judges each
// request with Policy.EvaluateHTTP.
//
// HTTP rules apply to hostnames only, never to an address literal: sbx refuses an HTTP rule
// whose resource is an IP address or a CIDR prefix, and Boks also keeps `**` from matching one,
// so that a raw connection by address — which never carries a request Boks can read — is decided
// by the address rules alone.

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"
)

// MethodAny is the --method value that matches every method.
const MethodAny = "ANY"

// DefaultHTTPPath is the path glob an HTTP rule has when --path is not given.
const DefaultHTTPPath = "/**"

// httpMethods are the methods a rule may name, in the order an error lists them.
var httpMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE"}

// HTTPMatch is the compiled HTTP selector of a rule. A nil *HTTPMatch on a Rule means the rule
// is a connection rule and applies to every flow, HTTP or not.
type HTTPMatch struct {
	// methods is sorted and deduplicated; nil means ANY.
	methods []string
	path    pathGlob
	query   []queryConstraint
}

// ParseHTTPMatch builds a selector from the three flags. It returns nil, nil when none of them
// is set: the rule is then an ordinary connection rule.
//
// The messages are sbx's own wherever sbx has one.
func ParseHTTPMatch(methods []string, path string, query []string) (*HTTPMatch, error) {
	if len(methods) == 0 {
		switch {
		case path != "" && len(query) > 0:
			return nil, fmt.Errorf("--path and --query apply only to HTTP rules; use --method ANY to match all methods")
		case path != "":
			return nil, fmt.Errorf("--path applies only to HTTP rules; use --method ANY to match all methods")
		case len(query) > 0:
			return nil, fmt.Errorf("--query applies only to HTTP rules; use --method ANY to match all methods")
		}
		return nil, nil
	}
	m := &HTTPMatch{}
	var err error
	if m.methods, err = parseMethods(methods); err != nil {
		return nil, err
	}
	if path == "" {
		path = DefaultHTTPPath
	}
	if m.path, err = parsePathGlob(path); err != nil {
		return nil, err
	}
	for _, q := range query {
		c, err := parseQueryConstraint(q)
		if err != nil {
			return nil, err
		}
		m.query = append(m.query, c)
	}
	sort.Slice(m.query, func(i, j int) bool { return m.query[i].String() < m.query[j].String() })
	return m, nil
}

// parseMethods accepts the --method values, each of which may itself be a comma-separated list
// (pflag's StringSlice splits on commas already; a stored rule may not have been through it).
func parseMethods(values []string) ([]string, error) {
	seen := map[string]bool{}
	anyMethod := false
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			m := strings.ToUpper(strings.TrimSpace(part))
			if m == "" {
				return nil, fmt.Errorf("HTTP method must not be empty")
			}
			if m == MethodAny {
				anyMethod = true
				continue
			}
			known := false
			for _, k := range httpMethods {
				if m == k {
					known = true
					break
				}
			}
			if !known {
				return nil, fmt.Errorf("unknown HTTP method %q: must be one of %s, %s",
					strings.TrimSpace(part), strings.Join(httpMethods, ", "), MethodAny)
			}
			seen[m] = true
		}
	}
	if anyMethod {
		// ANY already covers every method; naming some beside it adds nothing and would make
		// two spellings of one rule.
		return nil, nil
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return methodRank(out[i]) < methodRank(out[j]) })
	return out, nil
}

func methodRank(m string) int {
	for i, k := range httpMethods {
		if k == m {
			return i
		}
	}
	return len(httpMethods)
}

// Methods returns the methods the selector matches, or ["ANY"].
func (m *HTTPMatch) Methods() []string {
	if len(m.methods) == 0 {
		return []string{MethodAny}
	}
	return append([]string(nil), m.methods...)
}

// Path returns the path glob as written.
func (m *HTTPMatch) Path() string { return m.path.text }

// Query returns the query constraints in their canonical name=value form.
func (m *HTTPMatch) Query() []string {
	out := make([]string, 0, len(m.query))
	for _, q := range m.query {
		out = append(out, q.String())
	}
	return out
}

// String renders the selector as the flags that would recreate it, which is also how `policy ls`
// shows it: a value printed is a value that can be typed back.
func (m *HTTPMatch) String() string {
	if m == nil {
		return ""
	}
	parts := []string{"--method " + strings.Join(m.Methods(), ","), "--path " + shellQuote(m.path.text)}
	for _, q := range m.query {
		parts = append(parts, "--query "+shellQuote(q.String()))
	}
	return strings.Join(parts, " ")
}

// shellQuote single-quotes a value a shell would otherwise expand — a path glob, above all,
// which zsh refuses to pass through unquoted — so that a printed rule can be pasted back.
func shellQuote(s string) string {
	for _, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.ContainsRune("/_.,=%+:@-", c)
		if !ok {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

// Overlaps reports whether two selectors name the same request set for the purpose of the
// opposite-decision conflict check: an overlapping method set, the same path and the same query
// constraints. sbx describes the check as "the same methods, path, and query constraints"; an
// overlap of methods rather than identity is the stricter reading, and refusing a rule is the
// recoverable direction.
func (m *HTTPMatch) Overlaps(o *HTTPMatch) bool {
	if m == nil || o == nil {
		return m == nil && o == nil
	}
	if m.path.text != o.path.text || len(m.query) != len(o.query) {
		return false
	}
	for i := range m.query {
		if m.query[i].String() != o.query[i].String() {
			return false
		}
	}
	if len(m.methods) == 0 || len(o.methods) == 0 {
		return true
	}
	for _, a := range m.methods {
		for _, b := range o.methods {
			if a == b {
				return true
			}
		}
	}
	return false
}

// Equal reports whether two selectors are the same selector.
func (m *HTTPMatch) Equal(o *HTTPMatch) bool {
	if m == nil || o == nil {
		return m == nil && o == nil
	}
	return m.String() == o.String()
}

// HTTPRequest is the part of a request an HTTP rule is judged on. Nothing else of the request —
// no header, no body — is ever handed to the policy package.
type HTTPRequest struct {
	Method string
	// Path is the request path as it appeared on the wire (escaped), e.g. url.URL.EscapedPath().
	Path string
	// RawQuery is the query string without the '?'.
	RawQuery string
}

// CanonicalPath is the path rules are matched against: percent-decoded, backslashes read as
// slashes, dot segments resolved and repeated slashes collapsed. See canonicalRequestPath.
func (r HTTPRequest) CanonicalPath() string { return canonicalRequestPath(r.Path) }

// matchRequest reports whether the selector covers a request. forDeny selects the reading of
// an ambiguous request that makes the rule match more for a deny and less for an allow.
func (m *HTTPMatch) matchRequest(req HTTPRequest, forDeny bool) bool {
	if len(m.methods) > 0 {
		method := strings.ToUpper(req.Method)
		found := false
		for _, want := range m.methods {
			if method == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	path := canonicalRequestPath(req.Path)
	pathMatch := m.path.match(path)
	if !pathMatch && forDeny {
		// Many origins serve /admin/ and /admin alike, and some ignore case, so a deny
		// written for one spelling covers the others. An allow gets no such widening.
		folded := m.path.fold()
		lower := strings.ToLower(path)
		pathMatch = folded.match(lower) ||
			(len(lower) > 1 && strings.HasSuffix(lower, "/") && folded.match(strings.TrimSuffix(lower, "/")))
	}
	if !pathMatch {
		return false
	}
	if len(m.query) == 0 {
		return true
	}
	return matchQuery(m.query, req.RawQuery, forDeny)
}

// canonicalRequestPath turns a request path into the one form rules are matched against.
//
// This is security-relevant in both directions. The guest writes the path, and the origin is
// what interprets it: `/v1/../admin`, `/v1/%2e%2e/admin` and `/v1%2F..%2Fadmin` all reach
// `/admin` on some server, so a deny on `/admin/**` must see `/admin`, and an allow on `/v1/**`
// must not. Decoding everything and then resolving dot segments is the reading under which
// a deny matches whatever an origin might route to the denied path, and an allow matches only
// what every origin routes inside the allowed one.
//
// A backslash is read as a slash because some servers do. A path that cannot be decoded is
// matched raw after the same cleaning, which a deny on its literal text still catches.
func canonicalRequestPath(p string) string {
	decoded, err := url.PathUnescape(p)
	if err == nil {
		p = decoded
	}
	p = strings.ReplaceAll(p, `\`, "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	trailing := strings.HasSuffix(p, "/") && len(p) > 1
	var out []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, seg)
	}
	s := "/" + strings.Join(out, "/")
	if trailing && s != "/" {
		s += "/"
	}
	return s
}

// pathGlob is a path pattern: '*' matches within one segment, a whole segment of '**' matches
// any number of segments, including none — so `/v1/**` covers `/v1`, `/v1/` and `/v1/a/b`, and
// the default `/**` covers every path. Nothing else is special: sbx refuses '?' and '%' in a
// rule path outright, and so does Boks.
type pathGlob struct {
	text     string
	segments []string
}

func parsePathGlob(p string) (pathGlob, error) {
	if !strings.HasPrefix(p, "/") {
		return pathGlob{}, fmt.Errorf(`http path must start with "/"`)
	}
	if strings.ContainsAny(p, "?#") {
		return pathGlob{}, fmt.Errorf("http path must not contain a query or fragment")
	}
	if strings.Contains(p, "%") {
		return pathGlob{}, fmt.Errorf("http path must not contain percent-encoding")
	}
	if strings.Contains(p, `\`) {
		// Not an sbx message: Boks reads a backslash in a request as a slash, so a rule
		// containing one could never match and is refused rather than stored.
		return pathGlob{}, fmt.Errorf(`http path must not contain "\"; write "/" instead`)
	}
	segs := strings.Split(p[1:], "/")
	if p == "/" {
		segs = nil
	}
	for _, seg := range segs {
		if seg == ".." {
			return pathGlob{}, fmt.Errorf(`http path must not contain ".." segments`)
		}
		if seg == "." || seg == "" {
			return pathGlob{}, fmt.Errorf("http path must be canonical (without repeated, trailing, or dot segments)")
		}
	}
	return pathGlob{text: p, segments: segs}, nil
}

// fold returns the glob lowercased, for the case-insensitive reading a deny gets.
func (g pathGlob) fold() pathGlob {
	out := pathGlob{text: strings.ToLower(g.text), segments: make([]string, len(g.segments))}
	for i, s := range g.segments {
		out.segments[i] = strings.ToLower(s)
	}
	return out
}

func (g pathGlob) match(path string) bool {
	var segs []string
	if path != "/" {
		segs = strings.Split(strings.TrimPrefix(path, "/"), "/")
	}
	return matchSegments(g.segments, segs)
}

func matchSegments(pattern, segs []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			rest := pattern[1:]
			for i := 0; i <= len(segs); i++ {
				if matchSegments(rest, segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 || !matchStar(pattern[0], segs[0]) {
			return false
		}
		pattern, segs = pattern[1:], segs[1:]
	}
	return len(segs) == 0
}

// matchStar matches s against a pattern in which every '*' matches any run of characters
// (a run of several stars is one star). Used for path segments, where s never contains '/'.
func matchStar(pattern, s string) bool {
	return matchTokens(literalStarTokens(pattern), s)
}

// token is one element of a compiled glob: a literal string or a wildcard.
type token struct {
	star bool
	lit  string
}

func literalStarTokens(pattern string) []token {
	var out []token
	for len(pattern) > 0 {
		i := strings.IndexByte(pattern, '*')
		if i < 0 {
			out = append(out, token{lit: pattern})
			break
		}
		if i > 0 {
			out = append(out, token{lit: pattern[:i]})
		}
		if len(out) == 0 || !out[len(out)-1].star {
			out = append(out, token{star: true})
		}
		pattern = pattern[i+1:]
	}
	return out
}

func matchTokens(toks []token, s string) bool {
	if len(toks) == 0 {
		return s == ""
	}
	t := toks[0]
	if !t.star {
		if !strings.HasPrefix(s, t.lit) {
			return false
		}
		return matchTokens(toks[1:], s[len(t.lit):])
	}
	for i := 0; i <= len(s); i++ {
		if matchTokens(toks[1:], s[i:]) {
			return true
		}
	}
	return false
}

// queryConstraint requires a query parameter to be present with a value matching a pattern.
//
// The pattern is written URL-query-encoded, as sbx's is: a raw '*' is a wildcard over any run of
// characters, and everything else is a literal after decoding — so `%2A` is a literal star and
// `+` is a space. A raw backslash is refused and must be written `%5C`, which is sbx's rule too.
type queryConstraint struct {
	name  string // decoded
	value []token
}

func parseQueryConstraint(s string) (queryConstraint, error) {
	rawName, rawValue, ok := strings.Cut(s, "=")
	if !ok {
		return queryConstraint{}, fmt.Errorf("--query %q must be name=value (value is a pattern; use name=* for any value)", s)
	}
	if rawName == "" || strings.Contains(rawName, "*") {
		return queryConstraint{}, fmt.Errorf("--query name must be non-empty and must not contain '*'")
	}
	name, err := url.QueryUnescape(rawName)
	if err != nil || strings.Contains(name, "=") || name == "" {
		return queryConstraint{}, fmt.Errorf("--query name must be a valid URL-query-encoded name without '='")
	}
	if strings.Contains(rawValue, `\`) {
		return queryConstraint{}, fmt.Errorf(`query value pattern must percent-encode "\" as %%5C`)
	}
	var toks []token
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			toks = append(toks, token{lit: lit.String()})
			lit.Reset()
		}
	}
	for _, part := range strings.SplitAfter(rawValue, "*") {
		star := strings.HasSuffix(part, "*")
		part = strings.TrimSuffix(part, "*")
		decoded, err := url.QueryUnescape(part)
		if err != nil {
			return queryConstraint{}, fmt.Errorf("query value pattern %q is not valid URL-query encoding", rawValue)
		}
		lit.WriteString(decoded)
		if star {
			flush()
			if len(toks) == 0 || !toks[len(toks)-1].star {
				toks = append(toks, token{star: true})
			}
		}
	}
	flush()
	for _, t := range toks {
		if !utf8.ValidString(t.lit) {
			return queryConstraint{}, fmt.Errorf("query value pattern must decode to valid UTF-8")
		}
	}
	return queryConstraint{name: name, value: toks}, nil
}

// String is the constraint in canonical encoded form: literals query-escaped (so a literal star
// prints as %2A), wildcards as '*'.
func (q queryConstraint) String() string {
	var b strings.Builder
	b.WriteString(url.QueryEscape(q.name))
	b.WriteByte('=')
	for _, t := range q.value {
		if t.star {
			b.WriteByte('*')
			continue
		}
		b.WriteString(url.QueryEscape(t.lit))
	}
	return b.String()
}

// matchQuery applies every constraint (they are ANDed) to a raw query string.
//
// Two things about a query string are up to the origin, and Boks cannot know which reading it
// uses: whether ';' separates parameters as '&' does, and which of several values for one name
// it acts on. Each is resolved toward refusing:
//
//   - a deny matches if the constraint holds under either separator reading, and if ANY value
//     of the parameter matches;
//   - an allow matches only if the constraint holds under both readings, and only if EVERY
//     value of the parameter matches (and there is at least one).
//
// A query string that cannot be decoded makes a deny with query constraints match and an
// allow with them not match.
func matchQuery(constraints []queryConstraint, raw string, forDeny bool) bool {
	readings := [][]string{{"&"}, {"&", ";"}}
	results := make([]bool, 0, len(readings))
	for _, seps := range readings {
		params, ok := parseQueryParams(raw, seps)
		if !ok {
			return forDeny
		}
		all := true
		for _, c := range constraints {
			if !c.matchParams(params, forDeny) {
				all = false
				break
			}
		}
		results = append(results, all)
	}
	if forDeny {
		return results[0] || results[1]
	}
	return results[0] && results[1]
}

func (q queryConstraint) matchParams(params map[string][]string, forDeny bool) bool {
	values, ok := params[q.name]
	if !ok || len(values) == 0 {
		return false
	}
	for _, v := range values {
		m := matchTokens(q.value, v)
		if forDeny && m {
			return true
		}
		if !forDeny && !m {
			return false
		}
	}
	return !forDeny
}

func parseQueryParams(raw string, seps []string) (map[string][]string, bool) {
	out := map[string][]string{}
	pieces := []string{raw}
	for _, sep := range seps {
		var next []string
		for _, p := range pieces {
			next = append(next, strings.Split(p, sep)...)
		}
		pieces = next
	}
	for _, p := range pieces {
		if p == "" {
			continue
		}
		k, v, _ := strings.Cut(p, "=")
		name, err := url.QueryUnescape(k)
		if err != nil {
			return nil, false
		}
		value, err := url.QueryUnescape(v)
		if err != nil {
			return nil, false
		}
		out[name] = append(out[name], value)
	}
	return out, true
}
