package secret

// Acquisition: how a credential *enters* the system when there is nothing on the host to
// adopt.
//
// # The problem this solves, and why the obvious answer is not available
//
// `boks secret adopt` reads a login the user already performed on this machine. On a fresh
// machine there is nothing to read. The obvious fix — have Boks run the OAuth flow itself —
// is refused in internal/cli/secret.go and stays refused: every flow begins by identifying
// *this program* to the vendor with a client id issued to a registered application, and Boks
// is registered with none.
//
// Docker Sandboxes does not have that registration either, for Anthropic. Its help text says
// a token is configured "after `sbx run claude … -- auth login`": it runs the **real client**
// inside the sandbox and lets that client perform *its own* OAuth with *its own* client id,
// which is legitimate because it is that program. sbx merely ends up holding the result.
// (Its one host-side flow, `--oauth`, is documented "openai/global only" — what having
// exactly one registered client id looks like.)
//
// Boks does the same. The agent logs in; the exchange goes to the vendor's token endpoint,
// which is already a host Boks terminates TLS for; Boks keeps the tokens and hands the guest
// sentinels.
//
// # Two paths through the same endpoint, and they are opposites
//
// Both a refresh and an initial exchange are a POST to the same host and path. What Boks does
// with them is deliberately opposite, and the difference is worth stating in one place:
//
//	refresh (oauth.go)     NEVER FORWARD. The guest's bytes are drained and discarded. Boks
//	                       composes its own request from the stored refresh token, on the
//	                       host, and answers the guest with a body it composed from sentinels.
//
//	acquisition (here)     FORWARD, THEN KEEP THE RESPONSE. The guest's bytes are relayed
//	                       verbatim, because the authorization code and PKCE verifier in them
//	                       exist only inside the guest — they came from a redirect Boks never
//	                       saw. The origin's response is read by Boks, the tokens are taken
//	                       out of it and stored, and what reaches the guest is the same JSON
//	                       with sentinels where the tokens were.
//
// Which one applies is decided by the *stored record*, never by anything in the request: a
// credential relays exactly once, while it is Pending, and the first token it acquires closes
// that door permanently (OAuthRecord.WithTokens clears Pending). A guest cannot ask for the
// relay path, cannot re-open it, and cannot make Boks forward anything for a credential that
// already has a token.
//
// Docker's kit format documents `passthrough: boolean to skip sentinel masking` on its oauth
// block, and this is very likely the same distinction seen from the other side: a response
// that is relayed rather than composed is the only kind that *needs* masking, and
// `passthrough` is the switch that turns the masking off. Boks has no such switch, and should
// not: masking is what keeps the token out of the guest.
//
// # The authorization code is not a token
//
// This is the property that makes relaying acceptable. What the guest holds before the
// exchange is an authorization code and a PKCE verifier — single-use, redeemable only at the
// token endpoint, and worth nothing once redeemed. What comes back *is* a token, and it is
// intercepted before the guest sees a byte of it. So there is no moment at which a real token
// exists inside the guest: not in memory, not in a credential file. The credential file the
// agent then writes for itself is written from the response it received, which is sentinels.
//
// The ordering matters and is enforced below: capture and persist first, answer the guest
// second. The reverse would lose the credential entirely if the store failed to write, and an
// authorization code cannot be spent twice.
//
// # Masking is a refusal, not a best effort
//
// maskTokenResponse replaces every occurrence of the real values, anywhere in the body — not
// only in the two named fields — and then asserts that neither value survives anywhere in the
// bytes about to be written. If one does, the request fails. Failing a login is recoverable;
// leaking a subscription token into a sandbox is not.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// MaxTokenResponseBody bounds what Boks will read from a token endpoint on the acquisition
// path. A real token response is a few hundred bytes.
//
// It is a cap rather than a stream because this is the one response Boks has to understand
// before it can be forwarded, and a body it cannot hold entire is a body it cannot mask. The
// limit is therefore also a refusal: over it, the exchange fails rather than being passed
// through unread.
const MaxTokenResponseBody = 1 << 20

// TokenAcquisition is the outcome of a relayed token exchange: what the guest is given, and
// the fact — never the value — of what was kept.
type TokenAcquisition struct {
	// Service is the credential that was acquired, for the log. Never a value.
	Service string
	// Status is the origin's status code, passed through unchanged.
	Status int
	// Body is what the guest receives: the origin's own JSON with sentinels where the
	// tokens were. It is safe to write and safe to print.
	Body []byte
	// Acquired reports whether a token pair was actually taken out of this response. A
	// failed login — a spent code, a bad verifier — leaves it false and passes the origin's
	// answer through so the agent can say what went wrong.
	Acquired bool
	// Expiry is when the acquired access token dies, or the zero time. Not a secret.
	Expiry time.Time
}

// NeedsAcquisition reports whether this credential's next token request must be relayed and
// captured rather than answered from the host.
//
// It is true in exactly one situation: an OAuth credential whose store holds no access token.
// That is the armed-and-not-yet-acquired state `boks secret login` creates. Everything else —
// including a credential whose token has expired, or whose store lookup failed — is false, so
// the failure mode of this function is "answer locally", which forwards nothing.
func (i *Injector) NeedsAcquisition(ctx context.Context, c Credential) bool {
	if i == nil || c.OAuth == nil || i.oauth == nil {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	tokens, ok := i.tokens[c.Service]
	if !ok {
		var err error
		if tokens, err = i.oauth.LookupOAuth(ctx, c.Service); err != nil {
			return false
		}
		i.tokens[c.Service] = tokens
	}
	return tokens.IsZero()
}

// AcquireToken takes the tokens out of a token endpoint's response, stores them, and returns
// the body the guest should receive in place of it.
//
// Nothing is written back to the guest by this function; it hands the caller a body. That
// separation is what lets the caller keep the ordering the package comment insists on — the
// store is written before the guest is answered.
func (i *Injector) AcquireToken(ctx context.Context, c Credential, status int, body []byte) (TokenAcquisition, error) {
	if i == nil || c.OAuth == nil {
		return TokenAcquisition{}, fmt.Errorf("credential %q is not an oauth credential", c.Service)
	}
	out := TokenAcquisition{Service: c.Service, Status: status, Body: body}

	tokens, err := ParseTokenResponse(body, c.OAuth.ResponseFields, i.now())
	if err != nil {
		// Not an error for the caller. An origin that named no access token issued none —
		// a spent code, a bad verifier, a rejected client — and the agent is the thing that
		// can explain that to the user, so its answer is passed through untouched. There is
		// no token in it to mask: ParseTokenResponse fails on exactly the two cases, a body
		// that is not a JSON object and a body with no access token, and neither can hold a
		// credential Boks was supposed to keep.
		return out, nil
	}

	masked, err := c.OAuth.maskTokenResponse(body, tokens)
	if err != nil {
		return TokenAcquisition{}, fmt.Errorf("acquiring the oauth credential %q: %w", c.Service, err)
	}

	// Store first. If this fails the guest is told the login failed, which is true and
	// recoverable — the alternative is a guest holding a sentinel for a token nothing on the
	// host kept, and an authorization code that cannot be spent again.
	if i.saver == nil {
		return TokenAcquisition{}, fmt.Errorf("the oauth credential %q was acquired but nothing here can store it", c.Service)
	}
	if err := i.saver.SaveOAuth(ctx, c.Service, tokens); err != nil {
		return TokenAcquisition{}, fmt.Errorf("storing the acquired oauth credential %q: %w", c.Service, err)
	}
	i.mu.Lock()
	i.tokens[c.Service] = tokens
	i.mu.Unlock()

	out.Body, out.Acquired, out.Expiry = masked, true, tokens.Expiry
	return out, nil
}

// maskTokenResponse rewrites a token response so that no real token value survives in it.
//
// Two properties, and both are deliberate:
//
//   - **The shape is the origin's.** Every field the origin sent is kept, in place: an agent
//     reads `scope`, `expires_in`, an account object, whatever the vendor added last month,
//     and writes them into its own credential file. Composing a reply from scratch — which is
//     right for a refresh, where Boks knows what it is answering — would silently drop them.
//   - **Replacement is by value, not by field name.** Every string in the document is
//     scanned, so a token echoed into a second field, or embedded in a longer string, is
//     replaced too. Masking only `access_token` would leave a token in a field nobody thought
//     to name.
func (o *OAuth) maskTokenResponse(body []byte, tokens OAuthTokens) ([]byte, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		// Never the body: it is the token.
		return nil, errors.New("the token endpoint's response is not JSON")
	}
	access, refresh := tokens.Access.Reveal(), tokens.Refresh.Reveal()
	if access == "" {
		return nil, errors.New("nothing to mask: the response carried no access token")
	}
	// An empty pattern in a Replacer matches at every position, so a credential with no
	// refresh token has to build a one-rule replacer rather than a two-rule one with a hole
	// in it.
	pairs := []string{access, o.Sentinels.Access}
	if refresh != "" {
		pairs = append(pairs, refresh, o.refreshSentinel())
	}
	masked, err := json.Marshal(maskStrings(doc, strings.NewReplacer(pairs...)))
	if err != nil {
		return nil, errors.New("re-encoding the masked token response failed")
	}
	// The assertion the whole path rests on. Nothing derived from the body is named in the
	// error, because what would be named is the leak.
	if leak := findLeak(masked, access, refresh); leak != "" {
		return nil, fmt.Errorf("a real %s token survived masking; refusing to answer the guest", leak)
	}
	return masked, nil
}

// refreshSentinel is what a real refresh token is replaced with. A credential that minted no
// refresh sentinel would otherwise map it to the empty string, which deletes rather than
// masks and hands the guest a credential file with a field it cannot use; the access sentinel
// is at least token-shaped, and it is still a fake.
func (o *OAuth) refreshSentinel() string {
	if o.Sentinels.Refresh != "" {
		return o.Sentinels.Refresh
	}
	return o.Sentinels.Access
}

// maskStrings walks a decoded JSON document and rewrites every string it contains, keys
// included. Keys are covered because a vendor keying a map by token would otherwise slip
// through, and because it costs nothing to be sure.
func maskStrings(v any, r *strings.Replacer) any {
	switch t := v.(type) {
	case string:
		return r.Replace(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = maskStrings(e, r)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[r.Replace(k)] = maskStrings(e, r)
		}
		return out
	default:
		return v
	}
}

// findLeak reports which token, if either, still appears in body. It names the kind and never
// the value, so its result is safe to put in an error.
func findLeak(body []byte, access, refresh string) string {
	if access != "" && bytes.Contains(body, []byte(access)) {
		return "access"
	}
	if refresh != "" && bytes.Contains(body, []byte(refresh)) {
		return "refresh"
	}
	return ""
}

// StripForRelay removes the headers that would stop Boks from reading a token response, and
// reports which it removed.
//
// Only one matters in practice: a guest that advertises `Accept-Encoding: gzip` gets a
// compressed body, and a compressed body is one Boks cannot mask. Rather than teach the
// inspected path to decompress — it decodes nothing else, deliberately — the request is asked
// for an identity encoding. That is the single modification Boks makes to a relayed request,
// it applies to this one request on this one path, and the alternative is a token reaching the
// guest inside bytes nobody looked at.
func StripForRelay(h http.Header) []string {
	var removed []string
	for _, name := range []string{"Accept-Encoding", "Range", "If-None-Match", "If-Modified-Since"} {
		if h.Get(name) != "" {
			h.Del(name)
			removed = append(removed, name)
		}
	}
	h.Set("Accept-Encoding", "identity")
	return removed
}
