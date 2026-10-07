package proxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/policy"
)

// A client may half-close its connection once it has sent its request — BusyBox wget does,
// and so does nc. Go cancels a server request's context when that happens, and the proxy used
// to forward under that context, so the request was cancelled under it and the client got
// "502 … upstream request failed: context canceled" (2026-10-07, every Alpine container in a
// sandbox). A half-closed client must still get its answer.
func TestPlainHTTPSurvivesTheClientHalfClosing(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond) // long enough for the half-close to land first
		fmt.Fprint(w, "answered")
	}))
	defer origin.Close()
	p := newTestProxy(t, mustPolicy(t, policy.Allow), nil)

	conn, err := net.DialTimeout("tcp", p.url.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	host := strings.TrimPrefix(origin.URL, "http://")
	fmt.Fprintf(conn, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host, host)
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	reply, _ := io.ReadAll(conn)
	if !strings.HasPrefix(string(reply), "HTTP/1.1 200") || !strings.HasSuffix(string(reply), "answered") {
		t.Fatalf("a half-closed client got:\n%s", reply)
	}
}
