package ports

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// echoServer stands in for a service inside the sandbox. The forwarder is given a dial
// function rather than a network, so the datapath through a real gvisor stack is tested where
// that stack lives (internal/enforce) and the accept/splice/teardown logic is tested here,
// against something that starts in a millisecond.
func echoServer(t *testing.T) (dial DialGuest, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
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
	}()
	target := ln.Addr().(*net.TCPAddr).Port
	return func(ctx context.Context, p int) (net.Conn, error) {
		if p != target {
			return nil, errors.New("connection refused")
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	}, target
}

func speak(t *testing.T, addr, line string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("connecting to the published port %s: %v", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		t.Fatalf("writing: %v", err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	return strings.TrimSpace(reply)
}

// TestAPublishedPortCarriesTraffic is the feature in one test: connect to the host port, be
// answered by the service on the far side.
func TestAPublishedPortCarriesTraffic(t *testing.T) {
	dial, target := echoServer(t)
	f := New(dial, io.Discard, false)
	t.Cleanup(func() { _ = f.Close() })

	published, err := f.Publish(mustPublish(t, strconv.Itoa(target)))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(published) != 1 {
		t.Fatalf("published %d bindings, want 1 on an IPv4-only sandbox: %v", len(published), published)
	}
	if got := speak(t, hostAddr(published[0]), "hello"); got != "echo: hello" {
		t.Errorf("the published port answered %q", got)
	}
}

// TestAnEphemeralHostPortIsAllocated covers the form with no host port at all, which is what
// a user types when they do not care which port they get — and what `boks ports` has to
// report back, since nothing else can tell them.
func TestAnEphemeralHostPortIsAllocated(t *testing.T) {
	dial, target := echoServer(t)
	f := New(dial, io.Discard, false)
	t.Cleanup(func() { _ = f.Close() })

	published, err := f.Publish(mustPublish(t, strconv.Itoa(target)))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if published[0].HostPort == 0 {
		t.Fatal("no host port was allocated")
	}
	if published[0].HostIP != "127.0.0.1" {
		t.Errorf("bound %s, want loopback", published[0].HostIP)
	}
	if got := speak(t, hostAddr(published[0]), "ephemeral"); got != "echo: ephemeral" {
		t.Errorf("the ephemeral port answered %q", got)
	}
}

// TestUnpublishReleasesTheListener: the host port has to be free when Unpublish returns, not
// merely scheduled to be. A user moving a port — unpublish then publish the same number —
// would otherwise race their own command.
func TestUnpublishReleasesTheListener(t *testing.T) {
	dial, target := echoServer(t)
	f := New(dial, io.Discard, false)
	t.Cleanup(func() { _ = f.Close() })

	published, err := f.Publish(mustPublish(t, strconv.Itoa(target)))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	addr := hostAddr(published[0])
	spec := strconv.Itoa(published[0].HostPort) + ":" + strconv.Itoa(target)

	removed, err := f.Unpublish(mustUnpublish(t, spec))
	if err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed %d bindings, want 1", len(removed))
	}
	if len(f.List()) != 0 {
		t.Errorf("still listed as published: %v", f.List())
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Fatalf("%s still accepts connections after being unpublished", addr)
	}
	// And the number is immediately reusable, which is the point of waiting.
	if _, err := f.Publish(mustPublish(t, spec)); err != nil {
		t.Errorf("republishing the same host port straight away: %v", err)
	}
}

// TestUnpublishingSomethingThatIsNotPublished says so rather than succeeding quietly. A user
// who mistyped the port they meant to close should not be told it is closed.
func TestUnpublishingSomethingThatIsNotPublished(t *testing.T) {
	f := New(func(context.Context, int) (net.Conn, error) { return nil, errors.New("no") }, io.Discard, false)
	t.Cleanup(func() { _ = f.Close() })
	if _, err := f.Unpublish(mustUnpublish(t, "8080:3000")); err == nil {
		t.Error("unpublishing a port that was never published succeeded")
	}
}

// TestCloseLeavesNoListenerBehind is the leak check in test form. A host port still bound
// after the sandbox's network is gone is a socket accepting connections for a VM that no
// longer exists, and the next run of the same sandbox would fail to bind it.
func TestCloseLeavesNoListenerBehind(t *testing.T) {
	dial, target := echoServer(t)
	f := New(dial, io.Discard, false)

	published, err := f.Publish(mustPublish(t, strconv.Itoa(target)))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	addr := hostAddr(published[0])
	if got := speak(t, addr, "before"); got != "echo: before" {
		t.Fatalf("the port did not work before teardown: %q", got)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Fatalf("%s is still bound after the forwarder was closed", addr)
	}
	// Idempotent, so it can sit in a defer beside every other piece of teardown.
	if err := f.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// And nothing can be published into a closed forwarder afterwards.
	if _, err := f.Publish(mustPublish(t, strconv.Itoa(target))); err == nil {
		t.Error("a port was published into a network that has been shut down")
	}
}

// TestPublishingTheSamePortTwiceIsRefused. The second bind would fail anyway, but with the
// operating system's message about an address in use rather than one naming the sandbox that
// already has it.
func TestPublishingTheSamePortTwiceIsRefused(t *testing.T) {
	dial, target := echoServer(t)
	f := New(dial, io.Discard, false)
	t.Cleanup(func() { _ = f.Close() })

	if _, err := f.Publish(mustPublish(t, strconv.Itoa(target))); err != nil {
		t.Fatal(err)
	}
	published := f.List()
	spec := strconv.Itoa(published[0].HostPort) + ":" + strconv.Itoa(target)
	_, err := f.Publish(mustPublish(t, spec))
	if err == nil {
		t.Fatal("the same host port was published twice")
	}
	if !strings.Contains(err.Error(), "already published") {
		t.Errorf("the error does not say why: %v", err)
	}
}

// TestNothingListeningInTheGuestSaysWhy is the failure this feature produces most often, and
// it is almost never boks' fault: the dev server is bound to the guest's own 127.0.0.1, where
// nothing outside the VM can reach it. sbx documents the same constraint, and a message that
// only said "connection refused" would send the user looking in the wrong place.
func TestNothingListeningInTheGuestSaysWhy(t *testing.T) {
	var log strings.Builder
	dial := DialGuest(func(context.Context, int) (net.Conn, error) {
		return nil, errors.New("connection refused")
	})
	f := New(dial, &log, false)
	t.Cleanup(func() { _ = f.Close() })

	published, err := f.Publish(mustPublish(t, "3000"))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Publishing succeeds even though nothing answers: at the moment a port is published,
	// the service inside the sandbox has usually not been started yet.
	conn, err := net.DialTimeout("tcp", hostAddr(published[0]), 5*time.Second)
	if err != nil {
		t.Fatalf("connecting to the published port: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(conn); err != nil && !errors.Is(err, io.EOF) {
		// A reset rather than a clean close is fine; the connection ending is the point.
		t.Logf("read after the failed forward: %v", err)
	}
	conn.Close()

	if !strings.Contains(log.String(), "external interface") {
		t.Errorf("the log does not mention the guest's binding: %q", log.String())
	}
	// The same advice reaches `boks ports`, where the user is more likely to look.
	deadline := time.Now().Add(5 * time.Second)
	for {
		list := f.List()
		if len(list) == 1 && strings.Contains(list[0].LastError, "external interface") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the published port records no reason for the failure: %+v", list)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestUDPIsRefusedWithTheReason. The grammar accepts udp because sbx's does; the forwarder
// declines it because Boks' network stack drops UDP at the link and a published UDP port
// would need a hole in that filter. Saying so is better than "unknown protocol".
func TestUDPIsRefusedWithTheReason(t *testing.T) {
	f := New(func(context.Context, int) (net.Conn, error) { return nil, errors.New("no") }, io.Discard, false)
	t.Cleanup(func() { _ = f.Close() })

	for _, proto := range []string{"udp", "udp4", "udp6"} {
		_, err := f.Publish(mustPublish(t, "8080:3000/"+proto))
		if !errors.Is(err, ErrUDPNotCarried) {
			t.Errorf("publishing %s returned %v, want the UDP explanation", proto, err)
		}
	}
	if len(f.List()) != 0 {
		t.Errorf("a refused UDP specification left something bound: %v", f.List())
	}
}

// TestOnChangeReportsEveryChange: the supervisor writes this list into the sandbox's state
// file, which is what `boks ls` reads. A change that did not fire would leave the PORTS
// column describing a sandbox that no longer looks like that.
func TestOnChangeReportsEveryChange(t *testing.T) {
	dial, target := echoServer(t)
	f := New(dial, io.Discard, false)
	t.Cleanup(func() { _ = f.Close() })

	var seen [][]Published
	f.OnChange(func(list []Published) { seen = append(seen, list) })

	if _, err := f.Publish(mustPublish(t, strconv.Itoa(target))); err != nil {
		t.Fatal(err)
	}
	spec := strconv.Itoa(f.List()[0].HostPort) + ":" + strconv.Itoa(target)
	if _, err := f.Unpublish(mustUnpublish(t, spec)); err != nil {
		t.Fatal(err)
	}
	if len(seen) < 2 {
		t.Fatalf("%d changes reported, want at least one per operation", len(seen))
	}
	if len(seen[0]) != 1 || len(seen[len(seen)-1]) != 0 {
		t.Errorf("the reported lists do not match the operations: %v", seen)
	}
}

func hostAddr(p Published) string {
	return net.JoinHostPort(p.HostIP, strconv.Itoa(p.HostPort))
}

func mustPublish(t *testing.T, s string) Spec {
	t.Helper()
	spec, err := ParsePublish(s)
	if err != nil {
		t.Fatalf("ParsePublish(%q): %v", s, err)
	}
	return spec
}

func mustUnpublish(t *testing.T, s string) Spec {
	t.Helper()
	spec, err := ParseUnpublish(s)
	if err != nil {
		t.Fatalf("ParseUnpublish(%q): %v", s, err)
	}
	return spec
}
