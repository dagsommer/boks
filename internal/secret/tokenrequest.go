package secret

import (
	"encoding/json"
	"mime"
	"net/url"
)

// TokenRequestKind is what a request to a credential's token endpoint is, as far as Boks is
// concerned.
type TokenRequestKind int

const (
	// TokenRequestForeign is someone else's: another OAuth client using the same endpoint.
	// It is forwarded exactly as the guest wrote it and its answer returned unread.
	TokenRequestForeign TokenRequestKind = iota
	// TokenRequestRefresh renews this credential: it carries the refresh-token sentinel.
	TokenRequestRefresh
	// TokenRequestLogin is this credential's own client exchanging a login code.
	TokenRequestLogin
)

// ClassifyTokenRequest decides whether a token request concerns this credential.
//
// # Why it has to look
//
// The token endpoint is shared. Anthropic's serves Claude Code and also every other OAuth
// client of the same authorisation server — Claude Design's MCP server among them. Boks used
// to answer every POST to that path itself, from the stored Claude Code login, so on
// 2026-10-04 Claude Design's own code exchange came back carrying Claude Code's token and
// scopes and Design reported "the authorization server did not grant the design scopes
// (missing: user:design:read, user:design:write)".
//
// So the request is read — only to classify it — and only two shapes are taken:
//
//   - a refresh_token grant whose refresh token is this credential's sentinel. Nothing else
//     can carry it, and it is the one request that must never reach the endpoint as written.
//   - an authorization_code grant from this credential's own client ID: the agent logging
//     in, which Boks captures (see acquire.go) or answers.
//
// Everything else is foreign and goes through untouched. Forwarding it cannot disclose
// anything of Boks': a request that does not carry the sentinel carries nothing Boks holds,
// and the token it brings back belongs to the client that asked for it.
func (c Credential) ClassifyTokenRequest(contentType string, body []byte) TokenRequestKind {
	if c.OAuth == nil {
		return TokenRequestForeign
	}
	grant, refresh, client := tokenRequestFields(contentType, body)
	switch grant {
	case "refresh_token":
		if refresh != "" && c.OAuth.Sentinels.Refresh != "" && refresh == c.OAuth.Sentinels.Refresh {
			return TokenRequestRefresh
		}
	case "authorization_code":
		if client != "" && client == c.OAuth.ClientID {
			return TokenRequestLogin
		}
	}
	return TokenRequestForeign
}

// tokenRequestFields reads grant_type, refresh_token and client_id out of a JSON or form
// body. A body in neither shape yields nothing, and is therefore foreign.
func tokenRequestFields(contentType string, body []byte) (grant, refresh, client string) {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType == "application/x-www-form-urlencoded" {
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return "", "", ""
		}
		return form.Get("grant_type"), form.Get("refresh_token"), form.Get("client_id")
	}
	var fields struct {
		Grant   string `json:"grant_type"`
		Refresh string `json:"refresh_token"`
		Client  string `json:"client_id"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", "", ""
	}
	return fields.Grant, fields.Refresh, fields.Client
}
