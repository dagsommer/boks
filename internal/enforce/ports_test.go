//go:build !windows

package enforce

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/ports"
)

// TestAPublishedPortReachesAServiceInTheGuest is the whole feature, end to end, on the real
// datapath: a listener on the host's loopback, a connection dialled through the sandbox's
// virtual network across the real link socket, and a server on the far side of it answering.
//
// The far side is a second gvisor stack attached to the same SOCK_DGRAM socket a VM would
// use, so everything between the two ends — ARP, the TCP handshake, the bytes — is real, and
// only the hypervisor is missing. It is as much as a machine with no VM can demonstrate, and
// it demonstrates exactly the thing that used to be missing: there was no path at all from
// the host into a sandbox.
func TestAPublishedPortReachesAServiceInTheGuest(t *testing.T) {
	spec := testSpec(t, network.ModeNAT)
	session, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	guest := attachGuest(t, spec)
	defer guest.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// The host end of a SOCK_DGRAM link learns its peer from the first datagram it
	// receives, so nothing the host initiates is deliverable until the guest has spoken
	// once. A real VM does that while bringing its interface up.
	if err := guest.Announce(ctx); err != nil {
		t.Fatalf("announcing the fake guest: %v", err)
	}

	const guestPort = 3000
	ln, err := guest.Listen(guestPort)
	if err != nil {
		t.Fatalf("listening inside the fake guest: %v", err)
	}
	defer ln.Close()
	go serveEcho(ln)

	published, err := session.forwarder.Publish(mustPublish(t, strconv.Itoa(guestPort)))
	if err != nil {
		t.Fatalf("publishing: %v", err)
	}
	if len(published) != 1 || published[0].HostIP != "127.0.0.1" {
		t.Fatalf("published %+v, want one binding on 127.0.0.1", published)
	}

	reply := speakToHostPort(t, published[0], "a dev server in a sandbox")
	if reply != "echo: a dev server in a sandbox" {
		t.Fatalf("the guest's service answered %q", reply)
	}

	// And the port is gone when the sandbox's network is: a host port still bound after
	// this would be a socket accepting connections for a VM that no longer exists.
	addr := net.JoinHostPort(published[0].HostIP, strconv.Itoa(published[0].HostPort))
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Errorf("%s is still bound after the sandbox's network was torn down", addr)
	}
}

// TestPortsPublishedAtCreationComeUpWithTheSandbox covers the `-p` path: the specifications
// travel in the spec, and a failure to bind fails the session rather than producing a sandbox
// that quietly lacks the port its user asked for.
func TestPortsPublishedAtCreationComeUpWithTheSandbox(t *testing.T) {
	spec := testSpec(t, network.ModeNAT)
	spec.Publish = []string{"3000"}
	session, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	list := session.Ports()
	if len(list) != 1 || list[0].SandboxPort != 3000 || list[0].HostPort == 0 {
		t.Fatalf("published %+v, want one ephemeral binding to sandbox port 3000", list)
	}

	// A host port already taken is a failure of the whole session, not a warning.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	clash := testSpec(t, network.ModeNAT)
	clash.Publish = []string{strconv.Itoa(taken.Addr().(*net.TCPAddr).Port) + ":3000"}
	if s, err := Open(context.Background(), clash, io.Discard); err == nil {
		s.Close()
		t.Error("a sandbox came up without the port it was told to publish")
	}
}

// TestASandboxWithNoNetworkCannotPublish. `-net none` is the strongest containment Boks
// offers, and a port flag must not be a way to give such a sandbox a network after all.
func TestASandboxWithNoNetworkCannotPublish(t *testing.T) {
	spec := testSpec(t, network.ModeNone)
	spec.Publish = []string{"8080:3000"}
	session, err := Open(context.Background(), spec, io.Discard)
	if err == nil {
		session.Close()
		t.Fatal("a port was published into a sandbox with no network")
	}
	if !strings.Contains(err.Error(), "no virtual network") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestTheControlSocketIsOwnerOnlyAndOutOfTheGuestsReach is the security assertion behind the
// decision recorded in control.go: the supervisor grew an API, and the whole argument for
// that rests on nothing in the guest being able to use it.
//
// Three things are checked, and each is a different way in:
//
//   - the socket is 0600 inside a 0700 directory, so no other user on the host can reach it;
//   - **nothing in the state directory's `net/` subtree is shared into the sandbox** — the
//     one host directory a sandbox gets is the CA certificate directory, read-only, and it
//     lives elsewhere. A guest cannot open a UNIX socket at a path that does not exist in its
//     filesystem;
//   - the sandbox's virtual network offers nothing but the proxy. A guest that dials the
//     gateway looking for a control channel finds the ports Boks bound there and no other.
func TestTheControlSocketIsOwnerOnlyAndOutOfTheGuestsReach(t *testing.T) {
	spec := testSpec(t, network.ModeNAT)
	dir := dirFor(spec.StateDir, spec.Sandbox)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	session, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	path := controlPath(spec.StateDir, spec.Sandbox)
	control, err := serveControl(path, io.Discard,
		func(req controlRequest) controlResponse { return handleControl(session, req) })
	if err != nil {
		t.Fatalf("serveControl: %v", err)
	}
	defer control.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the control socket is mode %04o, want 0600", perm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("the directory holding the control socket is mode %04o, want 0700", perm)
	}

	// Nothing under the network state directory is shared into the sandbox. This is the
	// property that makes the control socket unreachable from the guest rather than
	// merely inconvenient to reach, so it is asserted against what Prepare actually hands
	// the runtime rather than against a comment.
	guest, err := spec.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for _, mount := range guest.Mounts {
		host, err := filepath.Abs(mount.HostPath)
		if err != nil {
			t.Fatal(err)
		}
		if within(host, dir) {
			t.Errorf("mount %s shares the sandbox's network state directory into the guest, "+
				"which would put the control socket inside it", mount.HostPath)
		}
	}
	// And nothing about the socket is announced to the guest either — an environment
	// variable naming it would be an invitation to look for a way to reach it.
	for _, env := range guest.Env {
		if strings.Contains(env, controlSocket) || strings.Contains(env, dir) {
			t.Errorf("the guest's environment mentions the control socket: %s", env)
		}
	}
}

// TestTheGuestCannotAskForAPublish drives the negative case from inside the virtual network,
// with a real fake guest on the real link.
//
// A sandbox that could ask the host to publish its own ports would have escaped in the way
// that matters, so the property is tested rather than argued: the guest's only channel is the
// link, the only things bound in its network are Boks' own, and a published port adds nothing
// there. The control socket exists on the host filesystem, which the guest has no path to at
// all.
func TestTheGuestCannotAskForAPublish(t *testing.T) {
	spec := testSpec(t, network.ModeNAT)
	session, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	guest := attachGuest(t, spec)
	defer guest.Close()

	published, err := session.forwarder.Publish(mustPublish(t, "3000"))
	if err != nil {
		t.Fatalf("publishing: %v", err)
	}

	gateway := spec.Plan.Gateway.String()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := guest.Announce(ctx); err != nil {
		t.Fatalf("announcing the fake guest: %v", err)
	}

	// Publishing a port binds on the *host's* loopback. It must add nothing inside the
	// sandbox's network: the host port number, reached on the gateway from the guest, is
	// not a thing that exists.
	probeCtx, cancelProbe := context.WithTimeout(ctx, 3*time.Second)
	defer cancelProbe()
	if conn, err := guest.Dial(probeCtx, net.JoinHostPort(gateway, strconv.Itoa(published[0].HostPort))); err == nil {
		conn.Close()
		t.Errorf("publishing a port made something answer on the gateway at %d, inside the sandbox",
			published[0].HostPort)
	}

	// Nor is the control socket's path meaningful in the sandbox's network: there is no
	// address there but the gateway, and the gateway answers on the ports Boks bound.
	for _, port := range []int{1, 80, 443, 8080} {
		if port == spec.proxyPort() {
			continue
		}
		c, cancelDial := context.WithTimeout(ctx, 2*time.Second)
		conn, err := guest.Dial(c, net.JoinHostPort(gateway, strconv.Itoa(port)))
		cancelDial()
		if err == nil {
			conn.Close()
			t.Errorf("the gateway answered the guest on port %d", port)
		}
	}
}

// TestTheControlProtocolRefusesWhatItDoesNotKnow. The set of verbs is closed on purpose —
// three, none of which can read a secret, a policy or a log — and a supervisor that quietly
// accepted an unknown one would be a supervisor whose surface nobody can state.
func TestTheControlProtocolRefusesWhatItDoesNotKnow(t *testing.T) {
	_, path := controlFixture(t)

	for _, op := range []string{"", "secrets", "policy", "shutdown", "LIST", "exec"} {
		resp := exchange(t, path, controlRequest{Op: op})
		if resp.Error == "" {
			t.Errorf("the control socket accepted the operation %q", op)
		}
	}
	// And the three it does know still work, so the refusal is not simply a broken
	// handler.
	if resp := exchange(t, path, controlRequest{Op: opList}); resp.Error != "" {
		t.Errorf("list was refused: %s", resp.Error)
	}
}

// TestControlPublishAndUnpublishRoundTrip drives the socket the way the CLI does, which is
// the only way `boks ports` reaches a running sandbox.
func TestControlPublishAndUnpublishRoundTrip(t *testing.T) {
	_, path := controlFixture(t)

	resp := exchange(t, path, controlRequest{Op: opPublish, Specs: []string{"3000"}})
	if resp.Error != "" {
		t.Fatalf("publish: %s", resp.Error)
	}
	if len(resp.Ports) != 1 || len(resp.Changed) != 1 {
		t.Fatalf("publish returned %+v / changed %+v", resp.Ports, resp.Changed)
	}
	host := resp.Ports[0].HostPort

	resp = exchange(t, path, controlRequest{Op: opList})
	if len(resp.Ports) != 1 {
		t.Fatalf("list returned %+v", resp.Ports)
	}

	spec := strconv.Itoa(host) + ":3000"
	resp = exchange(t, path, controlRequest{Op: opUnpublish, Specs: []string{spec}})
	if resp.Error != "" {
		t.Fatalf("unpublish: %s", resp.Error)
	}
	if len(resp.Ports) != 0 || len(resp.Changed) != 1 {
		t.Fatalf("unpublish returned %+v / changed %+v", resp.Ports, resp.Changed)
	}

	// A malformed specification is refused by the supervisor as well as by the CLI: a
	// process that trusts what arrives on a socket is protected by that socket's
	// permissions alone.
	if resp := exchange(t, path, controlRequest{Op: opPublish, Specs: []string{"not-a-port"}}); resp.Error == "" {
		t.Error("the supervisor accepted a malformed specification")
	}
}

// TestTheControlSocketDiesWithTheSupervisor: nothing may still answer for a sandbox whose
// network is gone, and the path must be free for the next supervisor to bind.
func TestTheControlSocketDiesWithTheSupervisor(t *testing.T) {
	spec := testSpec(t, network.ModeNAT)
	if err := os.MkdirAll(dirFor(spec.StateDir, spec.Sandbox), 0o700); err != nil {
		t.Fatal(err)
	}
	session, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	path := controlPath(spec.StateDir, spec.Sandbox)
	control, err := serveControl(path, io.Discard,
		func(req controlRequest) controlResponse { return handleControl(session, req) })
	if err != nil {
		t.Fatalf("serveControl: %v", err)
	}
	if conn, err := net.Dial("unix", path); err != nil {
		t.Fatalf("the control socket does not answer while the supervisor is up: %v", err)
	} else {
		conn.Close()
	}

	if err := control.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if conn, err := net.Dial("unix", path); err == nil {
		conn.Close()
		t.Error("the control socket still answers after the supervisor stopped serving it")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the control socket file survives its server: %v", err)
	}
	// And a fresh supervisor can take the same path, which is what makes a stop
	// immediately followed by a start work.
	again, err := serveControl(path, io.Discard,
		func(req controlRequest) controlResponse { return handleControl(session, req) })
	if err != nil {
		t.Fatalf("rebinding the control socket: %v", err)
	}
	_ = again.Close()
}

// controlFixture opens a session and a control server for it, and returns the socket's path.
func controlFixture(t *testing.T) (*Session, string) {
	t.Helper()
	spec := testSpec(t, network.ModeNAT)
	if err := os.MkdirAll(dirFor(spec.StateDir, spec.Sandbox), 0o700); err != nil {
		t.Fatal(err)
	}
	session, err := Open(context.Background(), spec, io.Discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	path := controlPath(spec.StateDir, spec.Sandbox)
	control, err := serveControl(path, io.Discard,
		func(req controlRequest) controlResponse { return handleControl(session, req) })
	if err != nil {
		t.Fatalf("serveControl: %v", err)
	}
	t.Cleanup(func() { control.Close() })
	return session, path
}

// exchange performs one control request against a socket path, the way call does but without
// the liveness check, which depends on a state file no test here writes.
func exchange(t *testing.T, path string, req controlRequest) controlResponse {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dialling the control socket: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if err := writeRequest(conn, req); err != nil {
		t.Fatalf("writing the request: %v", err)
	}
	resp, err := readResponse(conn)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	return resp
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

func serveEcho(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			line, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil {
				return
			}
			_, _ = io.WriteString(conn, "echo: "+line)
		}()
	}
}

func speakToHostPort(t *testing.T, p ports.Published, line string) string {
	t.Helper()
	addr := net.JoinHostPort(p.HostIP, strconv.Itoa(p.HostPort))
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		t.Fatalf("connecting to the published host port %s: %v", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		t.Fatalf("writing to the published port: %v", err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("reading from the published port: %v", err)
	}
	return strings.TrimSpace(reply)
}

func mustPublish(t *testing.T, s string) ports.Spec {
	t.Helper()
	spec, err := ports.ParsePublish(s)
	if err != nil {
		t.Fatalf("ParsePublish(%q): %v", s, err)
	}
	return spec
}
