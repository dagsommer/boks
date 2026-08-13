package enforce

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
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
	spec := Spec{
		Sandbox:  "boks-test",
		Plan:     plan,
		StateDir: dir,
		CADir:    filepath.Join(dir, "ca"),
	}
	setPolicy(t, &spec, policy.PresetOpen, nil, nil)
	return spec
}

// setPolicy gives a spec the policy a run would have resolved for it. Specs carry a resolved
// policy rather than the ingredients, so tests resolve one the same way the CLI does instead
// of constructing rules by hand.
func setPolicy(t *testing.T, spec *Spec, preset string, allow, deny []string) {
	t.Helper()
	res, err := (policy.Request{Preset: preset, Allow: allow, Deny: deny}).Resolve()
	if err != nil {
		t.Fatalf("resolving the test policy: %v", err)
	}
	spec.Resolution = &res
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
	if env["NODE_EXTRA_CA_CERTS"] != path.Join(GuestCADir, certFile) {
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
		if env["SSL_CERT_FILE"] != path.Join(GuestCADir, bundleFile) {
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

// TestProxyAnswersInsideTheVirtualNetwork is the cooperating-guest datapath test.
//
// A fake guest is attached to the real link socket and speaks HTTP through the proxy at the
// gateway address, exactly as a guest honouring HTTP_PROXY would: real Ethernet frames over
// the real socket, a real ARP exchange, a real TCP handshake, a real HTTP request. An
// allowed destination is fetched and a denied one is refused with a reason.
//
// The origin is on the host's loopback, and that is deliberate. This test is about the
// *proxy* path, where the guest never addresses the origin itself: it addresses the proxy
// inside its own virtual network and names the origin in a request line, and the outbound
// connection is made by the proxy on the host, where loopback is an ordinary address. An
// earlier version bound the origin to whatever non-loopback interface address the machine
// happened to have, because the presets deny loopback — and that made the test depend on the
// host being willing to accept a connection to its own LAN address, which a macOS host is
// not. The rule being dodged was a policy rule, so the fix is to choose a policy: `locked`
// carries no deny rules at all, and this test names the one destination it needs. That the
// *presets* refuse loopback is asserted where it belongs, in the policy tests.
//
// What no version of this test shows: any of it happening across a hypervisor boundary. A
// real guest reaches this socket through libkrun's virtio-net device and nerdbox's
// annotations, and neither is exercised here — there is no hypervisor on the machine this
// was written on.
func TestProxyAnswersInsideTheVirtualNetwork(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello from the origin")
	}))
	defer origin.Close()
	originHost := strings.TrimPrefix(origin.URL, "http://")

	spec := testSpec(t, network.ModeNAT)
	setPolicy(t, &spec, policy.PresetLocked, []string{originHost}, []string{"blocked.example.com"})
	spec.LogPath = filepath.Join(spec.StateDir, "decisions.jsonl")

	session, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	guest := attachGuest(t, spec)
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
	// A published port is part of what a session leaves behind, and the worst kind: a host
	// port still bound after teardown accepts connections for a VM that no longer exists.
	spec.Publish = []string{"3000"}
	session, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	publishedAddr := ""
	if list := session.Ports(); len(list) == 1 {
		publishedAddr = net.JoinHostPort(list[0].HostIP, strconv.Itoa(list[0].HostPort))
	} else {
		t.Fatalf("the session published %+v, want one binding", list)
	}
	guest := attachGuest(t, spec)
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
	if conn, err := net.DialTimeout("tcp", publishedAddr, time.Second); err == nil {
		conn.Close()
		t.Errorf("the published host port %s is still bound after Close", publishedAddr)
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
			strings.Contains(block, "boks/internal/enforce.") || strings.Contains(block, "boks/internal/ports.") {
			count++
		}
	}
	return count
}

func goroutineDump() string {
	buf := make([]byte, 1<<20)
	return string(buf[:runtime.Stack(buf, true)])
}

// attachGuest puts a fake guest on the far end of a started sandbox's link, configured the
// way Boks' annotations configure a real one: one address in the subnet, and a default route
// through the gateway.
func attachGuest(t *testing.T, spec Spec) *vnettest.Guest {
	t.Helper()
	guest, err := vnettest.Attach(vnettest.Config{
		Socket:    spec.Plan.Socket,
		GuestIP:   spec.Plan.GuestAddr.Addr().String(),
		GatewayIP: spec.Plan.Gateway.String(),
		Subnet:    spec.Plan.Subnet.String(),
		MTU:       spec.Plan.MTU,
	})
	if err != nil {
		t.Fatalf("attaching the fake guest: %v", err)
	}
	return guest
}

// originTheGuestCouldAddress starts a test origin at an address a guest is able to *put in a
// packet*, and verifies that this machine can actually connect to it.
//
// Loopback will not do here, and the reason is a property worth naming: a packet addressed
// to 127.0.0.0/8 arriving on a NIC that is not the loopback interface is a martian, and the
// host-side stack drops it at the IP layer, before any policy is consulted. The guest cannot
// address the host's own loopback at all — not "is denied by every preset", cannot. So a raw
// flow needs an origin on a real interface address.
//
// Whether such an address is usable is a property of the machine, not of Boks: a host that
// refuses connections to its own interface address (a macOS firewall, a local-network
// privacy prompt nobody can answer in a test run) cannot host an origin for this. That is
// checked here with a real connection rather than assumed, so the test skips with a reason
// instead of failing as though the policy were wrong.
func originTheGuestCouldAddress(t *testing.T, body string) (*httptest.Server, string) {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("this machine's interface addresses could not be read: %v", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() || ipnet.IP.To4() == nil {
			continue
		}
		listener, err := net.Listen("tcp", net.JoinHostPort(ipnet.IP.To4().String(), "0"))
		if err != nil {
			continue
		}
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, body)
		}))
		srv.Listener = listener
		srv.Start()

		// The host-side stack will reach this origin with an ordinary net.Dial from this
		// process. If that cannot work, nothing downstream can, and the failure has
		// nothing to do with the policy under test.
		conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), 2*time.Second)
		if err == nil {
			conn.Close()
			return srv, strings.TrimPrefix(srv.URL, "http://")
		}
		srv.Close()
	}
	t.Skip("this machine cannot host a test origin the sandbox's stack could dial: " +
		"no non-loopback IPv4 address accepts a connection from this process")
	return nil, ""
}

// deniedDestination is in TEST-NET-3 (RFC 5737), which is reserved for documentation and is
// not routable anywhere. If a deny ever failed open, this address would still not connect —
// which is exactly why the test that uses it asserts the *decision*, not just the failure.
const deniedDestination = "203.0.113.7:443"

// TestRawFlowToADeniedAddressIsRefusedAndLogged is the finding this whole datapath exists to
// fix, turned into a test.
//
// A guest with no proxy configured at all — no HTTP_PROXY, no cooperation, a socket and an
// address — opens a TCP connection straight at a destination the policy denies. Before the
// stack judged flows, that connection was dialled by gvisor-tap-vsock's own forwarder with a
// bare net.Dial and never reached the policy engine: on a real VM it completed a TLS
// handshake to a denied address and `boks policy log` showed nothing at all.
//
// Two things are asserted, and the second is the one that matters. The connection is refused
// — but a refusal on its own proves little, since an unroutable address would also fail to
// connect. So the decision log must show that Boks *decided* this: a denial, at the network
// stage, in transparent mode, naming the rule that decided it. A deny that failed open would
// leave an allow in the log, or nothing.
//
// This is the stack refusing a flow from a simulated guest on a real link socket. It is not
// a real VM being refused; nobody has seen that. See docs/verification.md.
func TestRawFlowToADeniedAddressIsRefusedAndLogged(t *testing.T) {
	spec := testSpec(t, network.ModeNAT)
	setPolicy(t, &spec, policy.PresetLocked, nil, []string{"203.0.113.9:443"}) // deny by default
	spec.LogPath = filepath.Join(spec.StateDir, "decisions.jsonl")

	session, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	guest := attachGuest(t, spec)
	defer guest.Close()

	for _, tc := range []struct {
		name, addr, wantRule string
	}{
		{"denied by default", deniedDestination, "no applicable policies"},
		{"denied by rule", "203.0.113.9:443", "203.0.113.9:443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			conn, err := guest.Dial(ctx, tc.addr)
			if err == nil {
				conn.Close()
				t.Fatalf("a raw connection to %s succeeded; the policy denies it", tc.addr)
			}
			if !vnettest.Refused(err) {
				t.Errorf("the guest saw %v; a denied destination must be refused, not left to hang", err)
			}

			host, _, _ := net.SplitHostPort(tc.addr)
			d, ok := findDecision(t, spec.LogPath, host)
			if !ok {
				t.Fatalf("nothing was logged for %s; a flow that never reaches the log is a flow "+
					"nobody can see was denied", tc.addr)
			}
			if d.Allowed {
				t.Errorf("the decision for %s was an allow: %+v", tc.addr, d)
			}
			if d.Mode != policy.ModeTransparent {
				t.Errorf("mode = %q, want %q: this flow never touched the proxy", d.Mode, policy.ModeTransparent)
			}
			if d.Stage != policy.StageNetwork {
				t.Errorf("stage = %q, want %q", d.Stage, policy.StageNetwork)
			}
			if !strings.Contains(d.Rule, tc.wantRule) {
				t.Errorf("rule = %q, want something containing %q", d.Rule, tc.wantRule)
			}
			if d.Sandbox != spec.Sandbox {
				t.Errorf("the decision is not attributed to the sandbox: %+v", d)
			}
		})
	}
}

// TestRawFlowToAnAllowedAddressIsCarriedAndLogged is the other half: enforcement that only
// ever says no is indistinguishable from a broken network.
//
// The same uncooperating guest — no proxy anywhere in the path — reaches an origin the policy
// permits, and the flow is recorded as a transparent allow. The body coming back is what
// makes it an end-to-end statement rather than a claim about a decision: the stack judged the
// SYN, dialled the destination itself, and spliced the two halves together.
func TestRawFlowToAnAllowedAddressIsCarriedAndLogged(t *testing.T) {
	origin, originHost := originTheGuestCouldAddress(t, "hello from the origin")
	defer origin.Close()

	spec := testSpec(t, network.ModeNAT)
	setPolicy(t, &spec, policy.PresetLocked, []string{originHost}, nil)
	spec.LogPath = filepath.Join(spec.StateDir, "decisions.jsonl")

	session, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	guest := attachGuest(t, spec)
	defer guest.Close()

	resp, err := guest.RawHTTPClient().Get(origin.URL)
	if err != nil {
		t.Fatalf("a raw connection to an allowed destination failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "hello from the origin" {
		t.Fatalf("the allowed raw flow returned %d %q", resp.StatusCode, body)
	}

	host, _, _ := net.SplitHostPort(originHost)
	d, ok := findDecision(t, spec.LogPath, host)
	if !ok {
		t.Fatalf("the allowed raw flow was carried but not recorded; the log must show what a " +
			"sandbox reached, not only what it was refused")
	}
	if !d.Allowed {
		t.Errorf("the decision for %s was a denial: %+v", originHost, d)
	}
	if d.Mode != policy.ModeTransparent || d.Stage != policy.StageNetwork {
		t.Errorf("mode/stage = %q/%q, want %q/%q", d.Mode, d.Stage, policy.ModeTransparent, policy.StageNetwork)
	}
	if !strings.Contains(d.Resource, "net:ip:") {
		t.Errorf("resource = %q; a raw flow carries no hostname and must be recorded as an address",
			d.Resource)
	}
}

// TestTheMetadataEndpointIsRefusedEvenWhenAllowed: 169.254.169.254 is the cloud instance
// metadata service, and on a hosted machine it is the most reliable credential source there
// is. gvisor-tap-vsock's forwarder refused link-local unless its EC2 flag was set; Boks
// asserts that flag off and now owns the forwarder, so the refusal has to be reimplemented
// here or the assertion becomes decorative.
//
// The test permits the address explicitly, which is the point: this one is not a policy
// question, and `-allow 169.254.169.254` must not buy it.
func TestTheMetadataEndpointIsRefusedEvenWhenAllowed(t *testing.T) {
	spec := testSpec(t, network.ModeNAT)
	setPolicy(t, &spec, policy.PresetLocked, []string{"169.254.169.254:80"}, nil)
	spec.LogPath = filepath.Join(spec.StateDir, "decisions.jsonl")

	session, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	guest := attachGuest(t, spec)
	defer guest.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if conn, err := guest.Dial(ctx, "169.254.169.254:80"); err == nil {
		conn.Close()
		t.Fatal("the guest reached the instance metadata endpoint")
	}
	if d, ok := findDecision(t, spec.LogPath, "169.254.169.254"); ok && d.Allowed {
		t.Errorf("the metadata endpoint was allowed by the policy engine: %+v", d)
	}
}

// TestCloseTearsDownAFlowInProgress: "this sandbox no longer has a network" has to be true
// of the connection the guest opened a moment ago, not only of the next one it tries.
//
// A spliced flow holds two sockets and three goroutines on the host. If Close left them
// alive, a guest would keep a working connection to a destination Boks believes it has taken
// away, for as long as the far end kept it open — and the goroutines would accumulate one
// set per sandbox the process ever served.
func TestCloseTearsDownAFlowInProgress(t *testing.T) {
	origin, originHost := originTheGuestCouldAddress(t, "hello from the origin")
	defer origin.Close()

	before := boksGoroutines(t)

	spec := testSpec(t, network.ModeNAT)
	setPolicy(t, &spec, policy.PresetLocked, []string{originHost}, nil)

	session, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	guest := attachGuest(t, spec)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := guest.Dial(ctx, originHost)
	if err != nil {
		t.Fatalf("dialling an allowed destination: %v", err)
	}
	defer conn.Close()

	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	guest.Close()

	// The guest's end of the flow is gone: a read returns rather than blocking on a
	// connection Boks no longer carries.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Error("a flow that was open when the network was closed is still carrying data")
	} else if strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "timed out") {
		t.Errorf("the flow was left hanging rather than torn down: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for boksGoroutines(t) > before && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if after := boksGoroutines(t); after > before {
		t.Errorf("goroutines running Boks code went from %d to %d after tearing down a live flow:\n%s",
			before, after, goroutineDump())
	}
}

// TestTheGuestCannotAddressTheHostsLoopback is the property the verification run on a real
// VM saw as `curl rc=7`, pinned here so it cannot regress quietly.
//
// A packet addressed to 127.0.0.0/8 arriving on a NIC that is not the loopback interface is
// a martian: the host-side stack drops it at the IP layer, before the forwarder is reached
// and before any policy is consulted. That is stronger than a deny rule, and this test says
// so by giving the guest a policy that *permits* the address it is trying to reach and a
// real listener to reach. It still cannot get there.
//
// If this ever fails, every unauthenticated dev server, database and debug endpoint on the
// developer's machine is inside the sandbox's reach.
func TestTheGuestCannotAddressTheHostsLoopback(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "this must never reach a sandbox")
	}))
	defer origin.Close()
	originHost := strings.TrimPrefix(origin.URL, "http://")

	spec := testSpec(t, network.ModeNAT)
	setPolicy(t, &spec, policy.PresetLocked, []string{originHost}, nil) // deliberately permitted, and still unreachable
	spec.LogPath = filepath.Join(spec.StateDir, "decisions.jsonl")

	session, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	guest := attachGuest(t, spec)
	defer guest.Close()

	// Long enough for a handshake that was going to work, short enough not to pad the
	// suite: the SYN is dropped, so this is a wait for silence.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := guest.Dial(ctx, originHost)
	if err == nil {
		conn.Close()
		t.Fatalf("the guest reached the host's loopback at %s", originHost)
	}

	// And nothing was decided about it, because the packet never got as far as a decision.
	// This is the one case where an empty log is the correct outcome.
	if d, ok := findDecision(t, spec.LogPath, "127.0.0.1"); ok {
		t.Errorf("a loopback destination reached the policy engine: %+v", d)
	}
}

// findDecision returns the most recent decision recorded for a host.
func findDecision(t *testing.T, path, host string) (policy.Decision, bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("no decision log was written: %v", err)
	}
	defer f.Close()
	decisions, err := policy.ReadDecisions(f, 0)
	if err != nil {
		t.Fatalf("reading the decision log: %v", err)
	}
	var found policy.Decision
	var ok bool
	for _, d := range decisions {
		if d.Host == host {
			found, ok = d, true
		}
	}
	return found, ok
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}
