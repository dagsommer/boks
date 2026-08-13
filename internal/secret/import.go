package secret

// Importing a credential an agent already has.
//
// Boks does not invent a credential format. The modal Claude Code user logged in with
// `claude /login` months ago, has no API key, and has a token pair sitting in the macOS
// Keychain under the service name `Claude Code-credentials`. Asking that user to paste a
// token they have never seen is not an onboarding path; reading what is already there is.
//
// Reading is behind CredentialSource for one reason: the Keychain cannot be exercised on
// every machine Boks builds on, and an untestable code path should be as small as possible
// and should sit behind the same interface as a testable one. FileSource and ReaderSource
// carry the same bytes and are covered by tests; KeychainSource differs only in where the
// bytes come from. **The Keychain path has never been executed** — see docs/security-model.md.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// CredentialSource yields the raw credential document an agent wrote.
type CredentialSource interface {
	// Read returns the document. Implementations must not log or print it.
	Read(ctx context.Context) ([]byte, error)
	// Describe says where it came from, for a message that must not contain the document.
	Describe() string
}

// FileSource reads a credential document from a file.
type FileSource struct{ Path string }

// Read implements CredentialSource.
func (s FileSource) Read(context.Context) ([]byte, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, fmt.Errorf("reading the credential file: %w", err)
	}
	return data, nil
}

// Describe implements CredentialSource.
func (s FileSource) Describe() string { return s.Path }

// ReaderSource reads a credential document from an open stream, which is how stdin arrives.
type ReaderSource struct {
	R    io.Reader
	Name string
}

// Read implements CredentialSource.
func (s ReaderSource) Read(context.Context) ([]byte, error) {
	// Bounded: a credential document is a few hundred bytes, and a stdin that never ends
	// should not be able to exhaust memory.
	data, err := io.ReadAll(io.LimitReader(s.R, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading the credential from %s: %w", s.Describe(), err)
	}
	return data, nil
}

// Describe implements CredentialSource.
func (s ReaderSource) Describe() string {
	if s.Name == "" {
		return "standard input"
	}
	return s.Name
}

// KeychainSource reads a generic password from the macOS Keychain with the `security` CLI.
//
// The CLI rather than a cgo binding, on purpose: a cgo dependency would make every build of
// Boks platform-specific for the sake of one read, and `security` is present on every macOS
// install. The trade is that the password crosses a pipe between two processes on the same
// machine — visible to that user and to root, which is who could read the Keychain item
// anyway.
//
// **This has never been run.** The machine this was written on is Linux and has no Keychain.
// What is tested is everything on either side of it: the parsing of the document it returns,
// and the refusal on a platform that has no Keychain.
type KeychainSource struct {
	// Service is the Keychain item's service name, e.g. "Claude Code-credentials".
	Service string
	// Account is the item's account. Empty means the current user, which is what Claude
	// Code stores it under.
	Account string
}

// ErrNoKeychain reports that this platform has no Keychain to read.
var ErrNoKeychain = errors.New("the macOS Keychain is only available on macOS")

// Read implements CredentialSource.
func (s KeychainSource) Read(ctx context.Context) ([]byte, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("%w (this is %s); export the credential to a file and import that instead",
			ErrNoKeychain, runtime.GOOS)
	}
	if s.Service == "" {
		return nil, errors.New("no keychain service name")
	}
	args := []string{"find-generic-password", "-s", s.Service, "-w"}
	if s.Account != "" {
		args = append(args, "-a", s.Account)
	}
	cmd := exec.CommandContext(ctx, "security", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	// Only stdout carries the password. Stderr is diagnostics — "could not be found" — and
	// is quoted back capped, because a message a user cannot see is a support ticket.
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 200 {
			detail = detail[:200]
		}
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("reading %q from the keychain: %s", s.Service, detail)
	}
	return out, nil
}

// Describe implements CredentialSource.
func (s KeychainSource) Describe() string { return "the macOS Keychain item " + s.Service }

// ImportedTokens is what a provider's own credential document yields.
type ImportedTokens struct {
	Access       string
	Refresh      string
	ExpiresAt    int64 // unix milliseconds; 0 when the document did not say
	Scopes       []string
	Subscription string
}

// OAuthProfile is what Boks knows about one provider's OAuth arrangement: where its
// credential lives, how its document is shaped, where its tokens are used, and what a
// convincing sentinel for it looks like.
//
// It is data rather than code so that adding a provider is adding a row, and so that the
// one thing that must not be guessed — the shape of a sentinel — is stated per provider by
// whoever knows the provider.
type OAuthProfile struct {
	Name        string
	Description string

	TokenEndpoint Endpoint
	ClientID      string
	Encoding      Encoding

	ResourceHosts []string
	Headers       []string

	EnvName      string
	FilePath     string
	FileTemplate string

	AccessPrefix   string
	RefreshPrefix  string
	SentinelLength int

	// DefaultSource is where this provider's credential normally lives on this machine.
	DefaultSource func() CredentialSource

	// Parse reads the provider's own credential document.
	Parse func(raw []byte) (ImportedTokens, error)
}

// ClaudeCodeKeychainService is where Claude Code keeps its subscription credential on macOS.
const ClaudeCodeKeychainService = "Claude Code-credentials"

// claudeCodeClientID is Claude Code's public OAuth client identifier. A client id is not a
// secret — it is sent in the clear on every token request — but it is provider-specific, so
// it is overridable.
const claudeCodeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// claudeCodeTemplate reproduces the file Claude Code writes for itself, field for field.
// Boks does not invent a format: an agent that reads its own credential file has to find
// what it expects to find, with sentinels where the tokens were.
const claudeCodeTemplate = `{"claudeAiOauth":{"accessToken":"{{.AccessToken}}","refreshToken":"{{.RefreshToken}}",` +
	`"expiresAt":{{.ExpiresAt}},"scopes":[{{range $i, $s := .Scopes}}{{if $i}},{{end}}"{{$s}}"{{end}}],` +
	`"subscriptionType":"{{.SubscriptionType}}"}}` + "\n"

// GuestCredentialDir is the guest directory a rendered credential file is shared into.
//
// A dedicated directory, not the agent's own config directory, and the reason is a real
// constraint rather than taste: Boks shares *directories*, read-only (internal/workspace
// refuses to share a single file, because the runtime implements a file bind mount by
// exposing its parent). Rendering straight to `~/.claude/.credentials.json` would therefore
// mount the whole of `~/.claude` read-only and break every other thing the agent keeps
// there. So the file is placed here and the guest is told where it is; putting it where an
// agent looks by default needs a copy at start-up, which belongs to the image.
const GuestCredentialDir = "/etc/boks/credentials"

var profiles = map[string]OAuthProfile{
	"claude-code": {
		Name:        "claude-code",
		Description: "Claude Code's Claude.ai subscription login (OAuth), from the macOS Keychain or its own credential file",
		TokenEndpoint: Endpoint{
			Host: "console.anthropic.com",
			Path: "/v1/oauth/token",
		},
		ClientID:      claudeCodeClientID,
		Encoding:      EncodingJSON,
		ResourceHosts: []string{"api.anthropic.com"},
		Headers:       []string{"Authorization"},
		EnvName:       "CLAUDE_CODE_OAUTH_TOKEN",
		FilePath:      GuestCredentialDir + "/claude-code.json",
		FileTemplate:  claudeCodeTemplate,
		// The real shapes: an access token is sk-ant-oat01-… and a refresh token
		// sk-ant-ort01-…, both around 110 characters. Claude Code checks the prefix
		// before it will send one, which is exactly why a sentinel has to carry it.
		AccessPrefix:   "sk-ant-oat01-",
		RefreshPrefix:  "sk-ant-ort01-",
		SentinelLength: 108,
		DefaultSource:  claudeCodeDefaultSource,
		Parse:          parseClaudeCode,
	},
}

// claudeCodeDefaultSource is where Claude Code's credential lives on this machine.
//
// macOS keeps it in the Keychain; everywhere else it is a file in the agent's own
// directory. The split matters for what can be tested: the file is ordinary bytes this
// project reads and parses under test, and the Keychain is the one branch that has never
// run here.
func claudeCodeDefaultSource() CredentialSource {
	if runtime.GOOS == "darwin" {
		return KeychainSource{Service: ClaudeCodeKeychainService}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return KeychainSource{Service: ClaudeCodeKeychainService}
	}
	return FileSource{Path: filepath.Join(home, ".claude", ".credentials.json")}
}

// Profile returns a provider profile by name.
func Profile(name string) (OAuthProfile, error) {
	p, ok := profiles[name]
	if !ok {
		return OAuthProfile{}, fmt.Errorf("unknown credential format %q; known formats: %s", name, strings.Join(ProfileNames(), ", "))
	}
	return p, nil
}

// ProfileNames lists the known providers, sorted.
func ProfileNames() []string {
	out := make([]string, 0, len(profiles))
	for name := range profiles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// claudeCodeDocument is the document Claude Code stores, in the Keychain on macOS and in
// ~/.claude/.credentials.json elsewhere.
type claudeCodeDocument struct {
	OAuth struct {
		AccessToken      string   `json:"accessToken"`
		RefreshToken     string   `json:"refreshToken"`
		ExpiresAt        int64    `json:"expiresAt"`
		Scopes           []string `json:"scopes"`
		SubscriptionType string   `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

func parseClaudeCode(raw []byte) (ImportedTokens, error) {
	var doc claudeCodeDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Never the document: it is the token.
		return ImportedTokens{}, errors.New("this is not a Claude Code credential document (expected JSON with a claudeAiOauth object)")
	}
	if doc.OAuth.AccessToken == "" {
		return ImportedTokens{}, errors.New("the credential document has no claudeAiOauth.accessToken")
	}
	return ImportedTokens{
		Access:       doc.OAuth.AccessToken,
		Refresh:      doc.OAuth.RefreshToken,
		ExpiresAt:    doc.OAuth.ExpiresAt,
		Scopes:       doc.OAuth.Scopes,
		Subscription: doc.OAuth.SubscriptionType,
	}, nil
}

// Import reads a credential from src in the given provider's format and builds the record to
// store. It performs no network access and prints nothing.
//
// service is the name the credential is stored under; empty means the profile's name.
func Import(ctx context.Context, profile OAuthProfile, src CredentialSource, service string) (OAuthRecord, error) {
	if profile.Parse == nil {
		return OAuthRecord{}, fmt.Errorf("credential format %q cannot be imported", profile.Name)
	}
	if service == "" {
		service = profile.Name
	}
	raw, err := src.Read(ctx)
	if err != nil {
		return OAuthRecord{}, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return OAuthRecord{}, fmt.Errorf("%s is empty", src.Describe())
	}
	tokens, err := profile.Parse(raw)
	if err != nil {
		return OAuthRecord{}, fmt.Errorf("%s: %w", src.Describe(), err)
	}
	return profile.Record(service, tokens)
}

// Record turns imported tokens into the record the store holds, minting the sentinels.
func (p OAuthProfile) Record(service string, tokens ImportedTokens) (OAuthRecord, error) {
	if service == "" {
		service = p.Name
	}
	if tokens.Access == "" {
		return OAuthRecord{}, errors.New("the credential has no access token")
	}
	length := p.SentinelLength
	if length == 0 {
		length = 64
	}
	r := OAuthRecord{
		V:               OAuthRecordVersion,
		Service:         service,
		AccessToken:     tokens.Access,
		RefreshToken:    tokens.Refresh,
		ExpiresAt:       tokens.ExpiresAt,
		Scopes:          tokens.Scopes,
		Subscription:    tokens.Subscription,
		TokenHost:       p.TokenEndpoint.Host,
		TokenPath:       p.TokenEndpoint.Path,
		ClientID:        p.ClientID,
		Encoding:        p.Encoding,
		ResourceHosts:   append([]string(nil), p.ResourceHosts...),
		Headers:         append([]string(nil), p.Headers...),
		AccessSentinel:  NewSentinel(p.AccessPrefix, service+"-access", length),
		RefreshSentinel: NewSentinel(p.RefreshPrefix, service+"-refresh", length),
		EnvName:         p.EnvName,
		FilePath:        p.FilePath,
		FileTemplate:    p.FileTemplate,
	}
	if err := r.Validate(); err != nil {
		return OAuthRecord{}, err
	}
	// A sentinel that collides with the real token would be substituted onto itself and,
	// worse, would mean the guest holds the real value. It cannot happen with a derived
	// sentinel, and checking costs nothing next to being wrong about it.
	if r.AccessSentinel == r.AccessToken || (r.RefreshToken != "" && r.RefreshSentinel == r.RefreshToken) {
		return OAuthRecord{}, errors.New("the generated sentinel equals the real token; refusing to store it")
	}
	return r, nil
}

// Expiry reports when an imported access token dies, or the zero time if it did not say.
func (t ImportedTokens) Expiry() time.Time {
	if t.ExpiresAt <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(t.ExpiresAt)
}
