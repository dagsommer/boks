package enforce

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/network/vnettest"
	"github.com/dagsommer/boks/internal/policy"
)

// testSpec builds a spec in a short-lived state directory.
//
// The directory is short on purpose: the link socket path has to fit in sockaddr_un, and
// t.TempDir() on macOS is already long enough to make that interesting.
func testSpec(t *testing.T, mode network.Mode) Spec {
	t.Helper()
	dir, err := os.MkdirTemp("", "be")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	plan, err := network.NewPlan(network.Config{
		Mode:       mode,
		Sandbox:    "boks-test",
		RuntimeDir: filepath.Join(dir, "net"),
	})
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	return Spec{
		Sandbox:  "boks-test",
		Plan:     plan,
		Preset:   policy.PresetOpen,
		StateDir: dir,
		CADir:    filepath.Join(dir, "ca"),
	}
}

// TestTheSocketLandsWhereTheSupervisorLooks pins an invariant that spans two packages and
// would otherwise be maintained by eye: the plan the CLI computes must put the link socket
// in the directory the supervisor treats as that sandbox's. If they ever disagree, the VM
// boots with a NIC connected to a socket nobody is holding, which is a silent loss of
// network rather than an error.
func TestTheSocketLandsWhereTheSupervisorLooks(t *testing.T) {
	spec := testSpec(t, network.ModeNAT)
	if got, want := filepath.Dir(spec.Plan.Socket), StateDir(spec.StateDir, spec.Sandbox); got != want {
		t.Errorf("the link socket is in %s, the supervisor looks in %s", got, want)
	}
}

func TestPrepareBuildsTheGuestEnvironmentAndAnnotations(t *testing.T) {
	spec := testSpec(t, network.ModeNAT)
	guest, err := spec.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Both annotations, or the runtime falls back to TSI and nothing is enforced at all.
	for _, key := range []string{
		"io.containerd.nerdbox.network.0",
		"io.containerd.nerdbox.ctr.network.0",
		"io.containerd.nerdbox.ctr.dns",
	} {
		if guest.Annotations[key] == "" {
			t.Errorf("annotation %s is missing; without it the sandbox is not wired to the stack", key)
		}
	}

	env := envMap(guest.Env)
	want := "http://" + spec.Plan.Gateway.String() + ":" + strconv.Itoa(DefaultProxyPort)
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if env[key] != want {
			t.Errorf("%s = %q, want %q", key, env[key], want)
		}
	}
	// Lowercase as well as uppercase: curl, git and much of the Unix world read only the
	// lowercase spelling.
	if env["NO_PROXY"] == "" || env["no_proxy"] == "" {
		t.Error("NO_PROXY is not set, so a guest would proxy its own loopback")
	}
	// No credential rule means no interception, so nothing about a CA belongs in the
	// guest's environment or its mounts.
	if _, ok := env["BOKS_CA_CERT_B64"]; ok {
		t.Error("a CA was handed to a sandbox that intercepts nothing")
	}
	if len(guest.Mounts) != 0 {
		t.Errorf("unexpected mounts: %+v", guest.Mounts)
	}
}

// TestPrepareShipsTheCAWhenSomethingWillBeDecrypted covers the other half of the bargain:
// a sandbox whose traffic will be intercepted has to be able to verify the certificate it
// is about to be shown.
func TestPrepareShipsTheCAWhenSomethingWillBeDecrypted(t *testing.T) {
	spec := testSpec(t, network.ModeNAT)
	spec.Inject = []string{"anthropic@api.anthropic.com=x-api-key"}
	spec.Intercept = true

	guest, err := spec.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	env := envMap(guest.Env)
	if env["BOKS_CA_CERT_B64"] == "" {
		t.Error("the certificate was not handed over in the environment")
	}
	// Node ignores the system trust store, so the additive variable has to be set: this
	// one is safe to point at a file holding only the Boks CA.
	if env["NODE_EXTRA_CA_CERTS"] != filepath.Join(GuestCADir, certFile) {
		t.Errorf("NODE_EXTRA_CA_CERTS = %q", env["NODE_EXTRA_CA_CERTS"])
	}

	if len(guest.Mounts) != 1 {
		t.Fatalf("expected one mount for the certificate, got %+v", guest.Mounts)
	}
	mount := guest.Mounts[0]
	if mount.GuestPath != GuestCADir || !mount.ReadOnly() {
		t.Errorf("the CA mount is %+v; it must be read-only at %s", mount, GuestCADir)
	}

	// The directory is shared into a hostile guest, so what is in it matters more than
	// what is in the environment: the signing key must not be reachable through it.
	entries, err := os.ReadDir(mount.HostPath)
	if err != nil {
		t.Fatalf("reading the shared certificate directory: %v", err)
	}
	for _, e := range entries {
		if e.Name() != certFile && e.Name() != bundleFile {
			t.Errorf("%s is shared into the guest and should not be", e.Name())
		}
		data, err := os.ReadFile(filepath.Join(mount.HostPath, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "PRIVATE KEY") {
			t.Fatalf("%s contains a private key", e.Name())
		}
	}

	// The replacing variables are only safe when the file also carries public roots:
	// pointing SSL_CERT_FILE at a Boks-only bundle would break every host boks does not
	// intercept, which is nearly all of them.
	bundle := filepath.Join(mount.HostPath, bundleFile)
	if _, err := os.Stat(bundle); err == nil {
		if env["SSL_CERT_FILE"] != filepath.Join(GuestCADir, bundleFile) {
			t.Errorf("a bundle was written but SSL_CERT_FILE = %q", env["SSL_CERT_FILE"])
		}
	} else if env["SSL_CERT_FILE"] != "" {
		t.Errorf("SSL_CERT_FILE = %q with no bundle to point at", env["SSL_CERT_FILE"])
	}
}

// TestGuestGetsThePlaceholderAndNeverTheSecret is the guest-facing half of the credential
// guarantee. The tool inside the sandbox has to find something shaped like a credential, or
// it will not make the request the proxy was waiting to sign — and what it finds must never
// be the credential.
func TestGuestGetsThePlaceholderAndNeverTheSecret(t *testing.T) {
	spec := testSpec(t, network.ModeNAT)
	spec.Inject = []string{"anthropic@api.anthropic.com=x-api-key"}
	spec.GuestCredentials = []string{"anthropic=ANTHROPIC_API_KEY=sk-ant-placeholder"}
	spec.Secrets = map[string]string{"anthropic": "sk-the-real-canary"}
	spec.Intercept = true

	guest, err := spec.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	env := envMap(guest.Env)
	if env["ANTHROPIC_API_KEY"] != "sk-ant-placeholder" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the placeholder", env["ANTHROPIC_API_KEY"])
	}
	for _, kv := range guest.Env {
		if strings.Contains(kv, "sk-the-real-canary") {
			t.Fatalf("the real credential reached the guest's environment: %s", kv)
		}
	}
	// Nor anywhere else the sandbox can read: the annotations are on the container too.
	for k, v := range guest.Annotations {
		if strings.Contains(v, "sk-the-real-canary") {
			t.Fatalf("the real credential reached annotation %s", k)
		}
	}
}

// TestPrepareForNoNetwork: -net none must not advertise a proxy that does not exist, and
// must still attach the NIC to the VM — that annotation is what turns the runtime's own
// transport off, and with it the guest's access to host loopback.
func TestPrepareForNoNetwork(t *testing.T) {
	spec := testSpec(t, network.ModeNone)
	guest, err := spec.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if guest.Annotations["io.containerd.nerdbox.network.0"] == "" {
		t.Error("no VM NIC: the runtime would fall back to TSI, which reaches host loopback")
	}
	for _, key := range []string{"io.containerd.nerdbox.ctr.network.0", "io.containerd.nerdbox.ctr.dns"} {
		if _, ok := guest.Annotations[key]; ok {
			t.Errorf("mode none wired the container with %s", key)
		}
	}
	if len(guest.Env) != 0 {
		t.Errorf("mode none set %v; there is no proxy to point a guest at", guest.Env)
	}
}

// TestSpecStringRedactsSecrets: a Spec carries credential values, and error paths print
// structs. The JSON form must not redact — that is how the values reach the process that
// attaches them — so the printed form has to.
func TestSpecStringRedactsSecrets(t *testing.T) {
	spec := testSpec(t, network.ModeNAT)
	spec.Secrets = map[string]string{"anthropic": "sk-canary-value"}
	for _, rendered := range []string{
		fmt.Sprintf("%v", spec),
		fmt.Sprintf("%s", spec),
		fmt.Sprintf("%#v", spec),
		fmt.Sprintf("%v", fmt.Errorf("failed to start %v", spec)),
	} {
		if strings.Contains(rendered, "sk-canary-value") {
			t.Fatalf("a secret reached a printed form: %s", rendered)
		}
	}
}

// TestNoNetworkHasNoStackAndNoListener is the whole of -net none on the host side: the link
// socket exists so the VM's NIC has somewhere to write, and there is nothing else — no
// stack, no proxy, no listener, nothing to dial.
func TestNoNetworkHasNoStackAndNoListener(t *testing.T) {
	spec := testSpec(t, network.ModeNone)
	session, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	if _, err := os.Stat(spec.Plan.Socket); err != nil {
		t.Errorf("the link socket is missing: %v; the VM would fail to boot", err)
	}
	if _, err := session.Network().Listen(DefaultProxyPort); err == nil {
		t.Fatal("a listener was created inside a sandbox that has no network")
	} else if !strings.Contains(err.Error(), "no virtual network") {
		t.Errorf("unhelpful error: %v", err)
	}
	if _, err := session.Network().Dial(context.Background(), DefaultProxyPort); err == nil {
		t.Error("something was dialable inside a sandbox that has no network")
	}
	if session.ProxyURL() != "" {
		t.Errorf("a sandbox with no network advertised a proxy at %s", session.ProxyURL())
	}
}

// TestProxyAnswersInsideTheVirtualNetwork is the end-to-end datapath test.
//
// A fake guest is attached to the real link socket and speaks HTTP through the proxy at the
// gateway address, exactly as a guest honouring HTTP_PROXY would: real Ethernet frames over
// the real socket, a real ARP exchange, a real TCP handshake, a real HTTP request. An
// allowed destination is fetched and a denied one is refused with a reason.
//
// What this does not show: any of it happening across a hypervisor boundary. A real guest
// reaches this socket through libkrun's virtio-net device and nerdbox's annotations, and
// neither is exercised here — there is no hypervisor on the machine this was written on.
func TestProxyAnswersInsideTheVirtualNetwork(t *testing.T) {
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello from the origin")
	}))
	// The origin must not be on loopback: every preset denies the host's own loopback,
	// because under the runtime's default transport the guest's 127.0.0.1 is the host's.
	// Binding to a real interface address is what makes an allow rule meaningful here.
	addr := hostAddress(t)
	listener, err := net.Listen("tcp", net.JoinHostPort(addr, "0"))
	if err != nil {
		t.Fatalf("listening on %s: %v", addr, err)
	}
	origin.Listener = listener
	origin.Start()
	defer origin.Close()
	originHost := strings.TrimPrefix(origin.URL, "http://")

	spec := testSpec(t, network.ModeNAT)
	spec.Allow = []string{originHost}
	spec.Deny = []string{"blocked.example.com"}
	spec.LogPath = filepath.Join(spec.StateDir, "decisions.jsonl")

	session, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	guest, err := vnettest.Attach(spec.Plan.Socket, spec.Plan.GuestAddr.Addr().String(), spec.Plan.MTU)
	if err != nil {
		t.Fatalf("attaching the fake guest: %v", err)
	}
	defer guest.Close()

	client, err := guest.HTTPClient(session.ProxyURL())
	if err != nil {
		t.Fatal(err)
	}

	// Allowed: the request leaves the virtual network, is judged, and comes back.
	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatalf("the guest could not reach an allowed destination through the proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "hello from the origin" {
		t.Errorf("allowed request returned %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Boks-Policy"); got != "allow" {
		t.Errorf("Boks-Policy = %q on an allowed request", got)
	}

	// Denied: refused by the proxy, with a reason a human can act on, and never dialled.
	resp, err = client.Get("http://blocked.example.com/")
	if err != nil {
		t.Fatalf("the denied request produced a transport error rather than a refusal: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("denied request returned %d, want 403", resp.StatusCode)
	}
	if got := resp.Header.Get("Boks-Policy"); got != "deny" {
		t.Errorf("Boks-Policy = %q on a denied request", got)
	}
	if !strings.Contains(string(body), "blocked by network policy") {
		t.Errorf("the refusal does not explain itself: %q", body)
	}

	// Both decisions are on record, under the sandbox they came from, so that
	// `boks policy log` can answer "what did this sandbox try to reach".
	decisions, err := os.Open(spec.LogPath)
	if err != nil {
		t.Fatalf("no decision log was written: %v", err)
	}
	defer decisions.Close()
	logged, err := policy.ReadDecisions(decisions, 0)
	if err != nil {
		t.Fatalf("reading the decision log: %v", err)
	}
	var sawAllow, sawDeny bool
	for _, d := range logged {
		if d.Sandbox != spec.Sandbox {
			t.Errorf("decision %v is not attributed to the sandbox", d)
		}
		if d.Allowed && strings.Contains(originHost, d.Host) {
			sawAllow = true
		}
		if !d.Allowed && d.Host == "blocked.example.com" {
			sawDeny = true
		}
	}
	if !sawAllow || !sawDeny {
		t.Errorf("the decision log is missing the allow (%v) or the deny (%v): %+v", sawAllow, sawDeny, logged)
	}
}

// TestCloseLeavesNothingBehind: a session that has served real traffic must leave no socket,
// no directory, no listener, nothing answering, and none of Boks' own goroutines. A leaked
// socket makes the next run of the same sandbox fail to bind for a reason nobody will guess;
// something still answering means a guest may have a network Boks believes it has taken away.
//
// One thing it deliberately does not assert: that the *process* returns to its original
// goroutine count. gvisor-tap-vsock v0.8.9 exposes no way to shut a VirtualNetwork down —
// there is no Close, and the gvisor stack it wraps is private — so each stack leaves about
// twenty goroutines of its own behind for the life of the process. Measured, not assumed:
// two open/close cycles in one process go 2 → 23 → 44. That is a real limitation, and it is
// the second reason a sandbox's stack lives in a process of its own: a supervisor that exits
// with its sandbox releases everything, where a long-lived CLI or daemon would accumulate a
// stack's worth of goroutines per sandbox it ever served.
func TestCloseLeavesNothingBehind(t *testing.T) {
	before := boksGoroutines(t)

	spec := testSpec(t, network.ModeNAT)
	session, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	guest, err := vnettest.Attach(spec.Plan.Socket, spec.Plan.GuestAddr.Addr().String(), spec.Plan.MTU)
	if err != nil {
		t.Fatalf("attaching the fake guest: %v", err)
	}
	client, err := guest.HTTPClient(session.ProxyURL())
	if err != nil {
		t.Fatal(err)
	}
	// Any request will do: this is about what a served session leaves behind, not about
	// the verdict.
	if resp, err := client.Get("http://denied.example.com/"); err == nil {
		resp.Body.Close()
	}
	guest.Close()

	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Errorf("Close twice returned %v; cleanup sits in a defer and must be idempotent", err)
	}

	if _, err := os.Stat(filepath.Dir(spec.Plan.Socket)); !os.IsNotExist(err) {
		t.Errorf("the link socket directory survived Close: %v", err)
	}

	// Nothing answers where the proxy was. The stack itself is still up — see the comment
	// above — so this is the assertion that matters: the listener is gone and a guest
	// that kept its link finds nobody home.
	dialCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if conn, err := session.Network().Dial(dialCtx, DefaultProxyPort); err == nil {
		conn.Close()
		t.Error("the proxy still answers inside the virtual network after Close")
	}

	// The proxy's port must be free for the next session: it is a fixed port, so a
	// listener that outlived its session would break the next start of this sandbox.
	again, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("a second session could not start after the first was closed: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	// Boks' own goroutines — the proxy's Serve, the link accept loop, the context watcher
	// — must all be gone. They settle asynchronously, so give them a moment.
	deadline := time.Now().Add(3 * time.Second)
	for boksGoroutines(t) > before && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if after := boksGoroutines(t); after > before {
		t.Errorf("goroutines running Boks code went from %d to %d after teardown:\n%s",
			before, after, goroutineDump())
	}
}

// boksGoroutines counts the goroutines currently executing Boks' own network code. It is a
// sharper question than the process's total, which cannot come back down while the netstack
// library has no shutdown API.
func boksGoroutines(t *testing.T) int {
	t.Helper()
	count := 0
	for _, block := range strings.Split(goroutineDump(), "\n\n") {
		if strings.Contains(block, "_test.go") {
			continue // the test's own goroutines, including this one
		}
		if strings.Contains(block, "boks/internal/proxy") || strings.Contains(block, "boks/internal/network.") ||
			strings.Contains(block, "boks/internal/enforce.") {
			count++
		}
	}
	return count
}

func goroutineDump() string {
	buf := make([]byte, 1<<20)
	return string(buf[:runtime.Stack(buf, true)])
}

// hostAddress returns an address on a real interface, for a test origin that must not be on
// loopback. A machine with no such address cannot run this test.
func hostAddress(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("no interface addresses: %v", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	t.Skip("this machine has no non-loopback IPv4 address to host a test origin on")
	return ""
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}
