package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/dagsommer/boks/internal/kit"
	"github.com/dagsommer/boks/internal/secret"
)

// kitOAuth is what a kit's `oauth:` block says about one service: the shape of the login, not
// the login.
//
// # A kit carries the shape, the store carries the tokens
//
// The same split as an API-key credential: a kit declaring `oauth` for `my-service` asks for
// whatever `boks secret adopt my-service` (or `boks secret login`) put in the store under that
// name, and says how to use it — which endpoint refreshes it, which hosts receive it, what the
// guest holds in its place. So it is expressed as the shape half of a secret.OAuthRecord, the
// form everything downstream already consumes and validates, rather than as a second OAuth
// type that would have to be judged by a second set of rules. apply lays it over the stored
// record; the tokens in that record are never touched.
//
// # What is not mapped
//
//   - passthrough is SKIPPED with a warning. It sends the real token into the guest, which is the one thing
//     Boks' OAuth handling exists to prevent, and a kit — someone else's file — must not be
//     able to switch that off for the person running it.
//   - credentialFile.structure is refused: the reference page says the sbx engine does not
//     support it either, and a kit that needs a file has a template to say so.
//   - skipIfEnv is ignored with a warning when the kit loads. See kit.OAuth.SkipIfEnv.
type kitOAuth struct {
	service string
	shape   secret.OAuthRecord
}

// kitOAuthShapes renders the oauth blocks of a kit's credentials.
//
// # An entry with both apiKey and oauth uses the API key
//
// That is the kit spec's own rule ("the API key takes precedence and OAuth acts as the
// fallback"), restricted to ONE entry of the kit: it does not decide between a kit's key and
// a login the user adopted separately, which secret.PreferOAuth settles on destinations as it
// does for everything else. An entry's oauth is therefore dropped whenever the same entry has
// an apiKey, whether or not a key is stored — Boks has no per-run "did that resolve" to fall
// back on, and silently choosing by what happens to be in the store would make the same kit
// behave differently on two machines.
func kitOAuthShapes(spec *kit.Spec, warn io.Writer) ([]kitOAuth, error) {
	if spec == nil {
		return nil, nil
	}
	var out []kitOAuth
	for _, c := range spec.Credentials {
		if c.OAuth == nil || c.APIKey != nil {
			continue
		}
		// passthrough asks for the real token to enter the guest, which Boks never does. The
		// credential is skipped with a warning rather than failing the run: the rest of the
		// kit is still usable, and the user can see exactly what was left out.
		if c.OAuth.Passthrough {
			if warn != nil {
				fmt.Fprintf(warn, "kit %q: credential %q asks for passthrough (the real OAuth token inside the sandbox); "+
					"Boks keeps tokens on the host, so this credential is skipped\n", spec.Name, c.Service)
			}
			continue
		}
		shape, err := kitOAuthShape(c.Service, c.OAuth)
		if err != nil {
			return nil, fmt.Errorf("kit %q, credential %q: %w", spec.Name, c.Service, err)
		}
		out = append(out, kitOAuth{service: c.Service, shape: shape})
	}
	return out, nil
}

// kitOAuthShape maps one oauth block onto the shape fields of an OAuthRecord, and runs the
// result through the record's own validation so a kit is judged by the rules a stored
// credential is.
func kitOAuthShape(service string, o *kit.OAuth) (secret.OAuthRecord, error) {
	r := secret.OAuthRecord{
		V:               secret.OAuthRecordVersion,
		Service:         service,
		TokenHost:       o.TokenEndpoint.Host,
		TokenPath:       o.TokenEndpoint.Path,
		ResourceHosts:   o.ResourceHosts,
		AccessSentinel:  o.Sentinels.AccessToken,
		RefreshSentinel: o.Sentinels.RefreshToken,
	}
	// The defaults are applied rather than left empty, so a kit that spells the RFC names
	// out and one that omits them are the same request — and the same fingerprint.
	if o.ResponseFields != nil {
		f := o.ResolvedResponseFields()
		r.ResponseAccess, r.ResponseRefresh, r.ResponseExpires = f.AccessToken, f.RefreshToken, f.ExpiresIn
	}
	if f := o.CredentialFile; f != nil {
		if f.Template == "" {
			return secret.OAuthRecord{}, fmt.Errorf("oauth.credentialFile.structure is not supported; " +
				"give a template")
		}
		r.FilePath = kitCredentialFilePath(service, f.Path)
		r.FileTemplate = f.Template
	}
	// Validate on a copy carrying a placeholder token: the shape is what is being judged.
	probe := r
	probe.AccessToken = "x"
	if err := probe.Validate(); err != nil {
		return secret.OAuthRecord{}, err
	}
	return r, nil
}

// kitCredentialFilePath redirects a kit's credential file into the directory Boks shares into
// the guest.
//
// A kit names the path its tool reads — `~/.my-tool/credentials.json` — and Boks cannot put a
// file there: it shares whole directories, read-only, and mounting the tool's own config
// directory would break everything else the tool keeps in it (see secret.GuestCredentialDir).
// The file lands in the credential directory instead, and BOKS_CREDENTIAL_FILE_<SERVICE>
// names where, which is what an entrypoint or a setup step uses to put it where the tool
// looks. The service leads the name so two kits' files cannot overwrite one another.
func kitCredentialFilePath(service, kitPath string) string {
	base := path.Base(strings.ReplaceAll(kitPath, "\\", "/"))
	return secret.GuestCredentialDir + "/" + safeName(service) + "-" + base
}

// safeName keeps a service name to characters that are safe in a file name.
func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, s)
}

// apply lays the kit's shape over the stored credential's.
//
// Only what the kit declares is replaced. The client id, request encoding, headers and
// environment variable come from the stored record, because the kit grammar has no spelling
// for them and they belong to whoever obtained the login; the tokens are never read here.
func (k kitOAuth) apply(stored secret.OAuthRecord) (secret.OAuthRecord, error) {
	r := stored
	r.TokenHost, r.TokenPath = k.shape.TokenHost, k.shape.TokenPath
	r.ResourceHosts = k.shape.ResourceHosts
	r.AccessSentinel, r.RefreshSentinel = k.shape.AccessSentinel, k.shape.RefreshSentinel
	if k.shape.ResponseAccess != "" {
		r.ResponseAccess, r.ResponseRefresh, r.ResponseExpires =
			k.shape.ResponseAccess, k.shape.ResponseRefresh, k.shape.ResponseExpires
	}
	if k.shape.FilePath != "" {
		r.FilePath, r.FileTemplate = k.shape.FilePath, k.shape.FileTemplate
	}
	if _, err := r.Credential(); err != nil {
		return secret.OAuthRecord{}, fmt.Errorf("kit credential %q: %w", k.service, err)
	}
	return r, nil
}

// fingerprint identifies exactly what the kit asked for. The shape holds no token, so it is
// safe to store; it is JSON of a struct, whose field order is fixed.
func (k kitOAuth) fingerprint() string {
	data, _ := json.Marshal(k.shape)
	return "oauth " + string(data)
}

// hosts are the hosts the credential goes to: where it is used, and where it is refreshed.
// The second is a destination of the real refresh token, so the user is told about it.
func (k kitOAuth) hosts() []string {
	hosts := append([]string{}, k.shape.ResourceHosts...)
	for _, h := range hosts {
		if h == k.shape.TokenHost {
			return hosts
		}
	}
	return append(hosts, k.shape.TokenHost)
}
