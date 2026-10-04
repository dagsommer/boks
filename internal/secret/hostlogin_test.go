package secret

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const hostAccess = "sk-ant-oat01-HOST-ACCESS-must-never-be-printed"

// A refresh the endpoint rejects is a dead login, not a fault: the request goes out with the
// guest's sentinel untouched so the agent sees the origin's 401 and logs in, and the
// credential then reads as awaiting a login so that login is captured on the host. It used to
// be a 502, which Claude Code shows as "Unable to connect … check your proxy".
func TestRejectedRefreshForwardsTheSentinelAndAwaitsALogin(t *testing.T) {
	record := testRecord(t, time.Now().Add(-time.Hour))
	inj, _, c := testInjector(t, record)
	inj.SetRefresher(&stubRefresher{err: fmt.Errorf("endpoint refused the refresh with status 400: %w", ErrRefreshRejected)})

	h := http.Header{}
	h.Set("Authorization", "Bearer "+record.AccessSentinel)
	used, err := inj.Apply(context.Background(), mustTarget(t, "api.creds.test:443"), h, FlowTLS)
	if !errors.Is(err, ErrCredentialStale) {
		t.Fatalf("Apply error = %v, want one wrapping ErrCredentialStale", err)
	}
	if len(used) != 0 {
		t.Errorf("used = %v, but nothing was injected", used)
	}
	if got := h.Get("Authorization"); got != "Bearer "+record.AccessSentinel {
		t.Errorf("Authorization = %q, want the guest's sentinel left as it was", got)
	}
	if !inj.NeedsAcquisition(context.Background(), c) {
		t.Error("after a rejected refresh the credential does not read as awaiting a login, so the agent's re-login would not be captured")
	}
	assertNoCanary(t, err.Error())
}

// A token endpoint that cannot answer is not one that said no: no login is asked for over a
// 5xx or a timeout, and the request still fails rather than going out with a dead token.
func TestUnavailableRefreshStillFails(t *testing.T) {
	record := testRecord(t, time.Now().Add(-time.Hour))
	inj, _, c := testInjector(t, record)
	inj.SetRefresher(&stubRefresher{err: errors.New("endpoint refused the refresh with status 503")})

	h := http.Header{}
	h.Set("Authorization", "Bearer "+record.AccessSentinel)
	_, err := inj.Apply(context.Background(), mustTarget(t, "api.creds.test:443"), h, FlowTLS)
	if err == nil || errors.Is(err, ErrCredentialStale) {
		t.Fatalf("Apply error = %v, want a hard failure that is not ErrCredentialStale", err)
	}
	if inj.NeedsAcquisition(context.Background(), c) {
		t.Error("a 503 dropped the stored login")
	}
}

// The case reported on 2026-10-03: the host's own Claude Code refreshed, the stored copy is
// expired, and the host's login is newer and valid. It is taken as it is — no refresh, so
// the host's copy is not retired in turn — and saved.
func TestExpiredCopyFollowsTheHostsNewerLogin(t *testing.T) {
	record := testRecord(t, time.Now().Add(-time.Hour))
	inj, store, _ := testInjector(t, record)
	stub := &stubRefresher{access: rotatedAccess, refresh: "sk-ant-ort01-ROTATED", ttl: time.Hour}
	inj.SetRefresher(stub)
	inj.SetHostSource(func(context.Context, string) (OAuthTokens, error) {
		return OAuthTokens{Access: NewValue(hostAccess), Refresh: NewValue("sk-ant-ort01-HOST"), Expiry: time.Now().Add(time.Hour)}, nil
	})

	h := http.Header{}
	h.Set("Authorization", "Bearer "+record.AccessSentinel)
	if _, err := inj.Apply(context.Background(), mustTarget(t, "api.creds.test:443"), h, FlowTLS); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stub.calls != 0 {
		t.Errorf("refreshed %d times; a valid host login must be used as it is, or the refresh retires the host's copy", stub.calls)
	}
	if got := h.Get("Authorization"); got != "Bearer "+hostAccess {
		t.Errorf("Authorization = %q, want the host's access token", got)
	}
	saved, err := store.LookupOAuth(context.Background(), record.Service)
	if err != nil || saved.Access.Reveal() != hostAccess {
		t.Errorf("the host's login was not saved over the stale copy (err %v)", err)
	}
}

// A rejected refresh looks at the host again, because that is usually who rotated it.
func TestRejectedRefreshIsRescuedByTheHostsLogin(t *testing.T) {
	record := testRecord(t, time.Now().Add(-time.Hour))
	inj, _, _ := testInjector(t, record)
	inj.SetRefresher(&stubRefresher{err: fmt.Errorf("status 400: %w", ErrRefreshRejected)})

	reads := 0
	inj.SetHostSource(func(context.Context, string) (OAuthTokens, error) {
		reads++
		if reads == 1 {
			// Not newer yet, as when the host has not refreshed at the time of the
			// expiry check.
			return OAuthTokens{Access: NewValue(accessCanary), Refresh: NewValue(refreshCanary)}, nil
		}
		return OAuthTokens{Access: NewValue(hostAccess), Refresh: NewValue("sk-ant-ort01-HOST"), Expiry: time.Now().Add(time.Hour)}, nil
	})

	h := http.Header{}
	h.Set("Authorization", "Bearer "+record.AccessSentinel)
	if _, err := inj.Apply(context.Background(), mustTarget(t, "api.creds.test:443"), h, FlowTLS); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := h.Get("Authorization"); got != "Bearer "+hostAccess {
		t.Errorf("Authorization = %q, want the host's access token after the rejected refresh", got)
	}
}

// A host source that has nothing, or fails, changes nothing: it is an improvement on the
// stored copy, never a requirement.
func TestHostSourceFailureIsIgnored(t *testing.T) {
	record := testRecord(t, time.Now().Add(-time.Hour))
	inj, _, _ := testInjector(t, record)
	stub := &stubRefresher{access: rotatedAccess, refresh: "sk-ant-ort01-ROTATED", ttl: time.Hour}
	inj.SetRefresher(stub)
	inj.SetHostSource(func(context.Context, string) (OAuthTokens, error) { return OAuthTokens{}, ErrNotFound })

	h := http.Header{}
	h.Set("Authorization", "Bearer "+record.AccessSentinel)
	if _, err := inj.Apply(context.Background(), mustTarget(t, "api.creds.test:443"), h, FlowTLS); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stub.calls != 1 || !strings.HasSuffix(h.Get("Authorization"), rotatedAccess) {
		t.Errorf("without a host login the stored copy should refresh as before (calls %d)", stub.calls)
	}
}

// The real refresher tells "this refresh token is dead" (RFC 6749 §5.2: 400 or 401) from
// "the endpoint could not answer", against an actual HTTP server.
func TestHTTPRefresherClassifiesRejections(t *testing.T) {
	for status, rejected := range map[int]bool{400: true, 401: true, 429: false, 500: false, 503: false} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		o := &OAuth{TokenEndpoint: Endpoint{Host: srv.Listener.Addr().String(), Path: "/v1/oauth/token"}}
		_, err := HTTPRefresher{Client: srv.Client()}.Refresh(context.Background(), o, NewValue(refreshCanary))
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: no error", status)
		}
		if got := errors.Is(err, ErrRefreshRejected); got != rejected {
			t.Errorf("status %d: ErrRefreshRejected = %v, want %v (%v)", status, got, rejected, err)
		}
		assertNoCanary(t, err.Error())
	}
}

// A request that carries the agent's own token rather than the sentinel is not Boks'
// business: it is not rewritten, and it does not wait on — or fail over — a refresh of the
// stored credential it is not using.
func TestAgentsOwnTokenIsLeftAlone(t *testing.T) {
	record := testRecord(t, time.Now().Add(-time.Hour))
	inj, _, _ := testInjector(t, record)
	stub := &stubRefresher{err: errors.New("endpoint refused the refresh with status 503")}
	inj.SetRefresher(stub)

	h := http.Header{}
	h.Set("Authorization", "Bearer sk-ant-oat01-the-agents-own-login")
	used, err := inj.Apply(context.Background(), mustTarget(t, "api.creds.test:443"), h, FlowTLS)
	if err != nil {
		t.Fatalf("Apply failed a request that needed nothing from Boks: %v", err)
	}
	if stub.calls != 0 {
		t.Errorf("refreshed %d times for a request that does not carry the sentinel", stub.calls)
	}
	if len(used) != 0 || h.Get("Authorization") != "Bearer sk-ant-oat01-the-agents-own-login" {
		t.Errorf("the agent's own token was touched: used %v, Authorization %q", used, h.Get("Authorization"))
	}
}

// Which host login a stored credential follows. Unmarked follows — that is every record
// stored before the marker existed, nearly all of them plain adoptions — and only the explicit
// opt-out does not.
func TestHostProfileFor(t *testing.T) {
	for _, tc := range []struct {
		record OAuthRecord
		want   string
	}{
		{OAuthRecord{Service: "claude-code"}, "claude-code"},
		{OAuthRecord{Service: "claude-code", HostProfile: "claude-code"}, "claude-code"},
		{OAuthRecord{Service: "work-claude", HostProfile: "claude-code"}, "claude-code"},
		{OAuthRecord{Service: "claude-code", HostProfile: NoHostProfile}, ""},
	} {
		if got := HostProfileFor(tc.record); got != tc.want {
			t.Errorf("HostProfileFor(%+v) = %q, want %q", tc.record, got, tc.want)
		}
	}
}

// A sandbox whose login died recovers without a restart once the host has a live one — and
// does not read the host's login on every request in between.
func TestDeadLoginRecoversFromTheHostWithoutARestart(t *testing.T) {
	record := testRecord(t, time.Now().Add(-time.Hour))
	inj, _, c := testInjector(t, record)
	inj.SetRefresher(&stubRefresher{err: fmt.Errorf("status 400: %w", ErrRefreshRejected)})
	now := time.Now()
	inj.SetClock(func() time.Time { return now })

	hostLoggedIn := false
	reads := 0
	inj.SetHostSource(func(context.Context, string) (OAuthTokens, error) {
		reads++
		if !hostLoggedIn {
			return OAuthTokens{}, ErrNotFound
		}
		return OAuthTokens{Access: NewValue(hostAccess), Refresh: NewValue("sk-ant-ort01-HOST"), Expiry: now.Add(time.Hour)}, nil
	})
	apply := func() (string, error) {
		h := http.Header{}
		h.Set("Authorization", "Bearer "+record.AccessSentinel)
		_, err := inj.Apply(context.Background(), mustTarget(t, "api.creds.test:443"), h, FlowTLS)
		return h.Get("Authorization"), err
	}

	// The refresh is rejected and the host has nothing: the login is dead.
	if _, err := apply(); !errors.Is(err, ErrCredentialStale) {
		t.Fatalf("first request: %v, want ErrCredentialStale", err)
	}
	if !inj.NeedsAcquisition(context.Background(), c) {
		t.Fatal("the dead login does not read as awaiting one")
	}

	// The host logs in. Within the recheck interval nothing reads it again.
	hostLoggedIn = true
	readsBefore := reads
	now = now.Add(10 * time.Second)
	if _, err := apply(); !errors.Is(err, ErrCredentialStale) {
		t.Fatalf("within the interval: %v, want still stale", err)
	}
	if reads != readsBefore {
		t.Errorf("the host login was read %d times within the recheck interval", reads-readsBefore)
	}

	// Past it, the next request finds the host's login and uses it.
	now = now.Add(hostRecheckInterval)
	got, err := apply()
	if err != nil {
		t.Fatalf("after the interval: %v", err)
	}
	if got != "Bearer "+hostAccess {
		t.Errorf("Authorization = %q, want the host's login", got)
	}
	if inj.NeedsAcquisition(context.Background(), c) {
		t.Error("recovered, but still reads as awaiting a login")
	}
}

// Only the credential's own refresh and its own client's login are taken; anything else at
// the shared token endpoint is someone else's.
func TestClassifyTokenRequest(t *testing.T) {
	record := testRecord(t, time.Now().Add(time.Hour))
	c, err := record.Credential()
	if err != nil {
		t.Fatal(err)
	}
	const json = "application/json"
	const form = "application/x-www-form-urlencoded"
	for _, tc := range []struct {
		name, ct, body string
		want           TokenRequestKind
	}{
		{"refresh with the sentinel", json, `{"grant_type":"refresh_token","refresh_token":"` + record.RefreshSentinel + `"}`, TokenRequestRefresh},
		{"refresh with the sentinel, form", form, "grant_type=refresh_token&refresh_token=" + record.RefreshSentinel, TokenRequestRefresh},
		{"refresh with another client's token", json, `{"grant_type":"refresh_token","refresh_token":"sk-ant-ort01-someone-else"}`, TokenRequestForeign},
		{"login by the credential's client", json, `{"grant_type":"authorization_code","code":"c","client_id":"client-id-is-public"}`, TokenRequestLogin},
		{"login by another client", json, `{"grant_type":"authorization_code","code":"c","client_id":"claude-design"}`, TokenRequestForeign},
		{"login with no client id", json, `{"grant_type":"authorization_code","code":"c"}`, TokenRequestForeign},
		{"not a token request at all", json, `not json`, TokenRequestForeign},
	} {
		if got := c.ClassifyTokenRequest(tc.ct, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
