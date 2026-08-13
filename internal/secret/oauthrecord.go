package secret

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dagsommer/boks/internal/policy"
)

// OAuthRecord is the serialised form of an OAuth credential: the token pair together with
// the shape needed to use and refresh it.
//
// It exists because two very different consumers need the same bytes. The encrypted store
// holds one per OAuth credential, and the CLI hands one to a sandbox's network supervisor on
// a pipe — the supervisor never learns the store's passphrase, so what it gets has to be
// self-contained.
//
// Its token fields are plain strings, not Values, and that is deliberate rather than an
// oversight: a Value's text form is "[redacted]", so a record built from Values would hand
// the supervisor the word "[redacted]" where a token belongs. The safety is restored at the
// two ends instead — String and GoString are redacted here, Tokens() wraps the values in
// Values the moment they are read, and the only writer of the plain fields is the store.
type OAuthRecord struct {
	// V is the record version, so a future field can be added without silently
	// misreading an old store.
	V       int    `json:"v"`
	Service string `json:"service"`

	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token,omitempty"`
	ExpiresAt    int64    `json:"expires_at,omitempty"` // unix milliseconds; 0 is unknown
	Scopes       []string `json:"scopes,omitempty"`
	Subscription string   `json:"subscription_type,omitempty"`

	TokenHost string   `json:"token_host"`
	TokenPath string   `json:"token_path"`
	ClientID  string   `json:"client_id,omitempty"`
	Encoding  Encoding `json:"encoding,omitempty"`

	ResourceHosts []string `json:"resource_hosts"`
	Headers       []string `json:"headers,omitempty"`

	AccessSentinel  string `json:"access_sentinel"`
	RefreshSentinel string `json:"refresh_sentinel,omitempty"`

	EnvName      string `json:"env_name,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
	FileTemplate string `json:"file_template,omitempty"`

	ResponseAccess  string `json:"response_access,omitempty"`
	ResponseRefresh string `json:"response_refresh,omitempty"`
	ResponseExpires string `json:"response_expires,omitempty"`
}

// OAuthRecordVersion is the current record version.
const OAuthRecordVersion = 1

// oauthMarker prefixes an OAuth record inside the store, so that a string secret and an
// OAuth credential can share one namespace and never be mistaken for one another. A plain
// API key that happened to start with this would be a remarkable coincidence, and the
// failure mode is a clear error rather than a token used as a header.
const oauthMarker = "boks-oauth-v1:"

// String is redacted, because an OAuthRecord is exactly what an error path prints.
func (r OAuthRecord) String() string {
	return fmt.Sprintf("secret.OAuthRecord{service:%s hosts:%v token:%s}",
		r.Service, r.ResourceHosts, Redacted)
}

// GoString covers %#v.
func (r OAuthRecord) GoString() string { return r.String() }

// There is deliberately no MarshalText here, and it is worth saying why: encoding/json
// prefers a TextMarshaler over a struct's fields, so a redacting one would replace the whole
// record with the word "[redacted]" in the two places the values are the entire point — the
// encrypted store, and the pipe to the network supervisor. Redaction lives in String and
// GoString, which is what a log line or a %v actually reaches for.

// Tokens returns the secret half, wrapped.
func (r OAuthRecord) Tokens() OAuthTokens {
	t := OAuthTokens{Access: NewValue(r.AccessToken), Refresh: NewValue(r.RefreshToken)}
	if r.ExpiresAt > 0 {
		t.Expiry = time.UnixMilli(r.ExpiresAt)
	}
	return t
}

// WithTokens returns a copy carrying a rotated pair. Everything else is unchanged: a refresh
// rotates values, never configuration.
func (r OAuthRecord) WithTokens(t OAuthTokens) OAuthRecord {
	out := r
	out.AccessToken = t.Access.Reveal()
	out.RefreshToken = t.Refresh.Reveal()
	out.ExpiresAt = 0
	if !t.Expiry.IsZero() {
		out.ExpiresAt = t.Expiry.UnixMilli()
	}
	return out
}

// OAuth builds the runtime shape from the record, parsing the host patterns.
func (r OAuthRecord) OAuth() (*OAuth, error) {
	o := &OAuth{
		TokenEndpoint:    Endpoint{Host: r.TokenHost, Path: r.TokenPath},
		ClientID:         r.ClientID,
		Encoding:         r.Encoding,
		Headers:          r.Headers,
		Sentinels:        Sentinels{Access: r.AccessSentinel, Refresh: r.RefreshSentinel},
		EnvName:          r.EnvName,
		File:             CredentialFile{Path: r.FilePath, Template: r.FileTemplate},
		ResponseFields:   ResponseFields{AccessToken: r.ResponseAccess, RefreshToken: r.ResponseRefresh, ExpiresIn: r.ResponseExpires},
		Scopes:           r.Scopes,
		SubscriptionType: r.Subscription,
	}
	for _, h := range r.ResourceHosts {
		p, err := policy.ParsePattern(h)
		if err != nil {
			return nil, fmt.Errorf("oauth credential %q: resource host %q: %w", r.Service, h, err)
		}
		o.ResourceHosts = append(o.ResourceHosts, p)
	}
	if err := o.Validate(); err != nil {
		return nil, fmt.Errorf("oauth credential %q: %w", r.Service, err)
	}
	return o, nil
}

// Credential builds the credential an injector runs from. It carries no token: the values
// are fetched from the provider at request time, exactly as they are for an API key.
func (r OAuthRecord) Credential() (Credential, error) {
	if r.Service == "" {
		return Credential{}, errors.New("oauth record has no service name")
	}
	o, err := r.OAuth()
	if err != nil {
		return Credential{}, err
	}
	c := Credential{
		Service:      r.Service,
		EnvName:      r.EnvName,
		ProxyManaged: true,
		OAuth:        o,
	}
	if r.EnvName != "" {
		// Placeholders() keys on the environment variable, falling back to the service
		// name. A credential that asked for no variable must not acquire one named after
		// the service, so the placeholder is set only when there is somewhere to put it —
		// such a credential reaches the guest through its credential file instead.
		c.Placeholder = r.AccessSentinel
	}
	return c, nil
}

// Validate checks a record before it is stored, so that a credential that could never work
// is refused at import time rather than inside a request an hour later.
func (r OAuthRecord) Validate() error {
	if r.AccessToken == "" {
		return errors.New("the oauth credential has no access token")
	}
	if _, err := r.Credential(); err != nil {
		return err
	}
	return nil
}

// encodeOAuth renders a record for the store.
func encodeOAuth(r OAuthRecord) (string, error) {
	if r.V == 0 {
		r.V = OAuthRecordVersion
	}
	data, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("encoding the oauth credential: %w", err)
	}
	return oauthMarker + string(data), nil
}

// decodeOAuth reads a record out of the store.
func decodeOAuth(name, stored string) (OAuthRecord, error) {
	if !strings.HasPrefix(stored, oauthMarker) {
		return OAuthRecord{}, fmt.Errorf("secret %q is a plain credential, not an oauth credential", name)
	}
	var r OAuthRecord
	if err := json.Unmarshal([]byte(strings.TrimPrefix(stored, oauthMarker)), &r); err != nil {
		// Never the payload: it holds the tokens.
		return OAuthRecord{}, fmt.Errorf("the stored oauth credential %q is corrupt", name)
	}
	if r.V != OAuthRecordVersion {
		return OAuthRecord{}, fmt.Errorf("the stored oauth credential %q uses record version %d, which this boks does not understand", name, r.V)
	}
	if r.Service == "" {
		r.Service = name
	}
	return r, nil
}

// IsOAuth reports whether a stored value is an OAuth record.
func IsOAuth(stored string) bool { return strings.HasPrefix(stored, oauthMarker) }
