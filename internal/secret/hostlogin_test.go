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
