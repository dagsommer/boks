package secret

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// armedProfile is what `boks secret login` starts from.
func armedProfile(t *testing.T) OAuthProfile {
	t.Helper()
	p, err := Profile("claude-code")
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	return p
}

// TestArmedRecordHoldsNoTokenAndIsValid: a credential-shaped hole is a legal stored record,
// and it carries the same sentinels the filled-in one will.
func TestArmedRecordHoldsNoTokenAndIsValid(t *testing.T) {
	profile := armedProfile(t)
	armed, err := profile.Arm("claude-code")
	if err != nil {
		t.Fatalf("Arm: %v", err)
	}
	if armed.AccessToken != "" || armed.RefreshToken != "" {
		t.Fatal("an armed record carries a token")
	}
	if !armed.Pending {
		t.Fatal("an armed record is not marked pending, so the proxy would never relay for it")
	}
	if err := armed.Validate(); err != nil {
		t.Fatalf("an armed record does not validate: %v", err)
	}

	// The sentinels must not move when the tokens arrive: a sandbox prepared before the
	// login holds the same credential file afterwards.
	filled, err := profile.Record("claude-code", ImportedTokens{Access: "sk-ant-oat01-real"})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if armed.AccessSentinel != filled.AccessSentinel || armed.RefreshSentinel != filled.RefreshSentinel {
		t.Error("the sentinels differ before and after a login")
	}
	if armed.TokenHost != filled.TokenHost || armed.FilePath != filled.FilePath {
		t.Error("an armed credential has a different shape from an adopted one")
	}
}

// TestARecordWithATokenIsNotPending: the two states are exclusive, and the store refuses a
// record that claims both — which is the state in which a guest could ask to be relayed for a
// credential that already works.
func TestARecordWithATokenIsNotPending(t *testing.T) {
	profile := armedProfile(t)
	armed, err := profile.Arm("claude-code")
	if err != nil {
		t.Fatalf("Arm: %v", err)
	}
	bad := armed
	bad.AccessToken = "sk-ant-oat01-real"
	if err := bad.Validate(); err == nil {
		t.Fatal("a record that is pending and holds a token was accepted")
	}

	filled := armed.WithTokens(OAuthTokens{Access: NewValue("sk-ant-oat01-real")})
	if filled.Pending {
		t.Fatal("acquiring a token left the credential pending, so it could be relayed again")
	}
	if err := filled.Validate(); err != nil {
		t.Fatalf("the acquired record does not validate: %v", err)
	}
}

// acquisitionInjector wires an armed record to a store that is provider and saver at once.
func acquisitionInjector(t *testing.T, record OAuthRecord) (*Injector, *MemoryStore, Credential) {
	t.Helper()
	store := NewMemoryStore(nil, map[string]OAuthRecord{record.Service: record})
	credential, err := record.Credential()
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	inj, err := NewInjector(store, credential)
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	return inj, store, credential
}

// TestNeedsAcquisitionIsDecidedByTheStore: the relay path is opened by the absence of a token
// and by nothing else. It is the security-relevant predicate in the whole feature.
func TestNeedsAcquisitionIsDecidedByTheStore(t *testing.T) {
	profile := armedProfile(t)
	armed, err := profile.Arm("claude-code")
	if err != nil {
		t.Fatalf("Arm: %v", err)
	}
	inj, _, credential := acquisitionInjector(t, armed)
	if !inj.NeedsAcquisition(context.Background(), credential) {
		t.Fatal("an armed credential does not need acquisition, so a login would never be captured")
	}

	// An expired credential is a refresh, not an acquisition: it has a token, so its
	// request is answered on the host and nothing is forwarded.
	expired, err := profile.Record("claude-code", ImportedTokens{
		Access:    "sk-ant-oat01-old",
		Refresh:   "sk-ant-ort01-old",
		ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	inj2, _, credential2 := acquisitionInjector(t, expired)
	if inj2.NeedsAcquisition(context.Background(), credential2) {
		t.Fatal("an expired credential would be relayed; only an empty one may be")
	}
}

// TestAcquireMasksATokenEchoedAnywhere: masking is by value, not by field name. An origin
// that repeats the token in a field nobody named must not hand it to the guest.
func TestAcquireMasksATokenEchoedAnywhere(t *testing.T) {
	profile := armedProfile(t)
	armed, err := profile.Arm("claude-code")
	if err != nil {
		t.Fatalf("Arm: %v", err)
	}
	inj, store, credential := acquisitionInjector(t, armed)

	const (
		access  = "sk-ant-oat01-ECHOED-ACCESS-CANARY"
		refresh = "sk-ant-ort01-ECHOED-REFRESH-CANARY"
	)
	body := `{"access_token":"` + access + `","refresh_token":"` + refresh + `",` +
		`"expires_in":3600,"debug":{"echo":["` + access + `"],"note":"issued ` + refresh + ` today"},` +
		`"` + access + `":"a key made of the token"}`

	out, err := inj.AcquireToken(context.Background(), credential, 200, []byte(body))
	if err != nil {
		t.Fatalf("AcquireToken: %v", err)
	}
	if !out.Acquired {
		t.Fatal("the pair was not acquired")
	}
	for name, canary := range map[string]string{"access": access, "refresh": refresh} {
		if strings.Contains(string(out.Body), canary) {
			t.Fatalf("the %s token survived masking:\n%s", name, out.Body)
		}
	}
	var answer map[string]any
	if err := json.Unmarshal(out.Body, &answer); err != nil {
		t.Fatalf("the masked body is not JSON: %v\n%s", err, out.Body)
	}
	if answer["access_token"] != armed.AccessSentinel {
		t.Errorf("access_token = %v, want the sentinel", answer["access_token"])
	}
	if _, ok := answer[armed.AccessSentinel]; !ok {
		t.Error("a token used as an object key was not masked")
	}
	// And the host kept what the guest did not get.
	saved, err := store.LookupOAuth(context.Background(), credential.Service)
	if err != nil {
		t.Fatalf("LookupOAuth: %v", err)
	}
	if saved.Access.Reveal() != access || saved.Refresh.Reveal() != refresh {
		t.Error("the host did not keep the acquired pair")
	}
}

// TestAcquireRefusesWhenMaskingCannotSucceed: the assertion is the point. A sentinel that
// contains the real token would leave it in the body, and the answer is to fail the login
// rather than to hand it over.
func TestAcquireRefusesWhenMaskingCannotSucceed(t *testing.T) {
	const access = "TOKEN"
	record := OAuthRecord{
		V:       OAuthRecordVersion,
		Service: "pathological",
		Pending: true,
		// A sentinel that embeds the real token: every replacement reintroduces it.
		AccessSentinel: "sk-ant-oat01-" + access + "-pathological-sentinel",
		TokenHost:      "token.test",
		TokenPath:      "/oauth/token",
		ResourceHosts:  []string{"api.test"},
	}
	inj, store, credential := acquisitionInjector(t, record)

	_, err := inj.AcquireToken(context.Background(), credential, 200,
		[]byte(`{"access_token":"`+access+`","expires_in":3600}`))
	if err == nil {
		t.Fatal("a body that still holds the real token was accepted")
	}
	if strings.Contains(err.Error(), access) {
		t.Errorf("the refusal names the value it refused to leak: %v", err)
	}
	if held := store.Records()[record.Service]; held.AccessToken != "" {
		t.Error("a refused acquisition still wrote to the store")
	}
}

// TestAcquireWithNothingToStoreFails: capture before answer. A credential that could not be
// persisted must not be reported as acquired, because the code cannot be spent twice.
func TestAcquireWithNothingToStoreFails(t *testing.T) {
	profile := armedProfile(t)
	armed, err := profile.Arm("claude-code")
	if err != nil {
		t.Fatalf("Arm: %v", err)
	}
	credential, err := armed.Credential()
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	// A provider that can supply an OAuth credential and cannot save one.
	inj, err := NewInjector(readOnlyOAuth{armed}, credential)
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	_, err = inj.AcquireToken(context.Background(), credential, 200,
		[]byte(`{"access_token":"sk-ant-oat01-minted","expires_in":3600}`))
	if err == nil {
		t.Fatal("a login was reported as acquired with nowhere to keep it")
	}
	if strings.Contains(err.Error(), "sk-ant-oat01-minted") {
		t.Errorf("the error carries the token: %v", err)
	}
}

// readOnlyOAuth is a Provider and OAuthProvider that is deliberately not an OAuthSaver.
type readOnlyOAuth struct{ record OAuthRecord }

func (r readOnlyOAuth) Lookup(context.Context, string) (Value, error) { return Value{}, ErrNotFound }

func (r readOnlyOAuth) LookupOAuth(_ context.Context, name string) (OAuthTokens, error) {
	if name != r.record.Service {
		return OAuthTokens{}, ErrNotFound
	}
	return r.record.Tokens(), nil
}
