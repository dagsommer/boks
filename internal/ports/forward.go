package ports

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"time"
)

// DialGuest opens a connection to a port inside the sandbox, through its virtual network.
//
// It is a function rather than an interface, and the Forwarder takes it rather than a
// *network.Network, for two reasons. It keeps this package free of the netstack — so the
// forwarding logic can be tested against an ordinary TCP server without standing a stack up —
// and it makes the direction of the dependency obvious: publishing knows how to accept on the
// host and nothing about how a guest is reached.
type DialGuest func(ctx context.Context, port int) (net.Conn, error)

// dialTimeout bounds a connection attempt into the guest. Something on the host is waiting
// for this, so a guest that never answers must fail rather than hang.
const dialTimeout = 10 * time.Second

// Forwarder owns every host listener published for one sandbox.
//
// One Forwarder per sandbox, living in the process that holds that sandbox's link socket —
// the network supervisor. It has to be there rather than in the CLI: the only way into the
// virtual network is the stack that terminates it, and that stack is in the supervisor.
type Forwarder struct {
	dial DialGuest
	log  io.Writer
	// hasIPv6 says whether the sandbox's virtual network carries IPv6, which decides how a
	// specification naming no host address expands. It is false for every sandbox Boks
	// builds today; the field exists so that the expansion rule is written once, in Binds,
	// rather than assumed here.
	hasIPv6 bool

	mu      sync.Mutex
	closed  bool
	entries map[key]*entry
	// conns is every connection currently being spliced, so that Close ends them instead
	// of leaving a host process talking to a sandbox Boks has finished with.
	conns map[io.Closer]struct{}
	// onChange is called with the full list after every change, so the supervisor can
	// write it where `boks ls` will find it.
	onChange func([]Published)
}

// key identifies one binding. The address family suffix is deliberately not part of it:
// `tcp` and `tcp4` bound on 127.0.0.1:8080 are the same socket, and treating them as two
// would let one port be published twice.
type key struct {
	host    string
	port    int
	sandbox int
	udp     bool
}

type entry struct {
	listener net.Listener
	pub      Published
	done     chan struct{}
}

// New builds a forwarder. Nothing is bound until Publish is called.
func New(dial DialGuest, log io.Writer, hasIPv6 bool) *Forwarder {
	return &Forwarder{
		dial:    dial,
		log:     log,
		hasIPv6: hasIPv6,
		entries: map[key]*entry{},
		conns:   map[io.Closer]struct{}{},
	}
}

// OnChange registers a callback invoked with the full published list after every successful
// change. It must be set before the forwarder is used.
func (f *Forwarder) OnChange(fn func([]Published)) { f.onChange = fn }

// ErrUDPNotCarried is returned for a udp/udp4/udp6 specification.
//
// The grammar accepts them because sbx's does, and a user who types one deserves the real
// reason rather than "unknown protocol". The reason is that Boks' host-side stack drops UDP
// at the link — see internal/network/stack_unix.go, where the whitelist carries TCP, ARP and
// DNS to the gateway and nothing else. A published UDP port would need the guest's replies to
// come back through that filter, which means widening it, which is a change to the stack's
// closed posture and not one to make as a side effect of a port flag.
var ErrUDPNotCarried = errors.New(
	"boks does not publish UDP ports: its network stack drops UDP at the link, carrying only " +
		"TCP and DNS to the sandbox's own resolver, so there is no return path for a datagram. " +
		"Publish the TCP port instead, or say why UDP is needed")

// Publish binds the host listeners one specification asks for and starts forwarding.
//
// It is all-or-nothing. A `tcp` specification on a dual-stack sandbox binds two addresses,
// and a half-published port — reachable on one loopback and not the other — is a worse
// outcome than a failure, because nothing in a listing would say which half worked.
func (f *Forwarder) Publish(spec Spec) ([]Published, error) {
	if spec.Protocol.IsUDP() {
		return nil, ErrUDPNotCarried
	}
	if spec.SandboxPort == 0 {
		return nil, errors.New("ports: no sandbox port to forward to")
	}

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, errors.New("ports: this sandbox's network has been shut down")
	}
	f.mu.Unlock()

	addrs := spec.Binds(f.hasIPv6)
	listeners := make([]net.Listener, 0, len(addrs))
	published := make([]Published, 0, len(addrs))
	port := spec.HostPort

	fail := func(err error) ([]Published, error) {
		for _, l := range listeners {
			_ = l.Close()
		}
		return nil, err
	}

	for _, addr := range addrs {
		l, err := net.Listen("tcp", net.JoinHostPort(addr.String(), strconv.Itoa(port)))
		if err != nil {
			return fail(bindError(spec, addr, port, err))
		}
		// An ephemeral request allocates once and reuses the number for the remaining
		// addresses, so that a port published on both loopbacks is the *same* port on
		// both. Two different numbers would be indistinguishable from two publishes.
		if port == 0 {
			port = l.Addr().(*net.TCPAddr).Port
		}
		listeners = append(listeners, l)
		published = append(published, Published{
			HostIP:      addr.String(),
			HostPort:    port,
			SandboxPort: spec.SandboxPort,
			Protocol:    string(transportOf(spec.Protocol)),
		})
	}

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return fail(errors.New("ports: this sandbox's network has been shut down"))
	}
	for _, p := range published {
		if _, exists := f.entries[keyOf(p)]; exists {
			f.mu.Unlock()
			return fail(fmt.Errorf("ports: %s is already published for this sandbox; "+
				"unpublish it first", p))
		}
	}
	for i, p := range published {
		e := &entry{listener: listeners[i], pub: p, done: make(chan struct{})}
		f.entries[keyOf(p)] = e
		go f.serve(e)
	}
	list := f.listLocked()
	f.mu.Unlock()

	f.notify(list)
	return published, nil
}

// bindError explains a refusal in the terms the user is in a position to act on. "address
// already in use" on a port they did not name is confusing; on one they did, it is the whole
// answer.
func bindError(spec Spec, addr netip.Addr, port int, err error) error {
	if spec.HostPort == 0 {
		return fmt.Errorf("ports: binding %s for sandbox port %d: %w", addr, spec.SandboxPort, err)
	}
	return fmt.Errorf("ports: binding %s: %w\n"+
		"Something else on this host is using that port. Omit the host port to let boks pick a "+
		"free one: --publish %d", net.JoinHostPort(addr.String(), strconv.Itoa(port)), err, spec.SandboxPort)
}

// Unpublish removes the bindings a specification names and returns them.
func (f *Forwarder) Unpublish(spec Spec) ([]Published, error) {
	f.mu.Lock()
	var removed []Published
	var closing []*entry
	for k, e := range f.entries {
		if !e.pub.matches(spec) {
			continue
		}
		delete(f.entries, k)
		closing = append(closing, e)
		removed = append(removed, e.pub)
	}
	list := f.listLocked()
	f.mu.Unlock()

	if len(removed) == 0 {
		return nil, fmt.Errorf("ports: %s is not published for this sandbox; "+
			"run 'boks ports' to see what is", spec)
	}
	// Closing outside the lock, and waiting: an unpublish that returns before the host
	// port is actually free would make `--unpublish 8080:3000 --publish 8080:4000` a race
	// against itself.
	for _, e := range closing {
		_ = e.listener.Close()
		<-e.done
	}
	sortPublished(removed)
	f.notify(list)
	return removed, nil
}

// List returns every binding, in a stable order.
func (f *Forwarder) List() []Published {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listLocked()
}

func (f *Forwarder) listLocked() []Published {
	out := make([]Published, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e.pub)
	}
	sortPublished(out)
	return out
}

func sortPublished(p []Published) {
	sort.Slice(p, func(i, j int) bool {
		if p[i].HostPort != p[j].HostPort {
			return p[i].HostPort < p[j].HostPort
		}
		if p[i].HostIP != p[j].HostIP {
			return p[i].HostIP < p[j].HostIP
		}
		return p[i].Protocol < p[j].Protocol
	})
}

func (f *Forwarder) notify(list []Published) {
	if f.onChange != nil {
		f.onChange(list)
	}
}

// serve accepts on one host listener and forwards each connection into the sandbox.
func (f *Forwarder) serve(e *entry) {
	defer close(e.done)
	for {
		conn, err := e.listener.Accept()
		if err != nil {
			// Close is the ordinary way out of this loop, on unpublish and on
			// teardown, so a closed listener is not worth a word.
			if !errors.Is(err, net.ErrClosed) {
				f.logf("ports: %s stopped accepting: %v", e.pub, err)
			}
			return
		}
		go f.forward(e, conn)
	}
}

// guestBindingAdvice is the message for the failure this feature produces most often, and it
// is not a boks failure at all: the service inside the sandbox is bound to the *guest's* own
// 127.0.0.1, where nothing outside the guest can reach it. sbx documents the same constraint.
const guestBindingAdvice = "the service in the sandbox must listen on the VM's external " +
	"interface (bind 0.0.0.0 or ::, not only 127.0.0.1)"

func (f *Forwarder) forward(e *entry, host net.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	guest, err := f.dial(ctx, e.pub.SandboxPort)
	if err != nil {
		reason := fmt.Sprintf("nothing answered on port %d inside the sandbox: %v — %s",
			e.pub.SandboxPort, err, guestBindingAdvice)
		f.noteError(e, reason)
		f.logf("ports: %s: %s", e.pub, reason)
		_ = host.Close()
		return
	}
	f.noteError(e, "")

	if !f.track(host, guest) {
		_ = host.Close()
		_ = guest.Close()
		return
	}
	defer f.untrack(host, guest)

	var wg sync.WaitGroup
	wg.Add(2)
	go copyHalf(host, guest, &wg)
	go copyHalf(guest, host, &wg)
	wg.Wait()

	_ = host.Close()
	_ = guest.Close()
}

// copyHalf carries one direction and then shuts that direction of dst down. Half-close is
// honoured for the same reason internal/network honours it: a client that finishes sending
// and keeps reading is ordinary, and tearing the whole connection down on the first EOF
// breaks it in a way that looks like a network fault.
func copyHalf(dst, src net.Conn, wg *sync.WaitGroup) {
	defer wg.Done()
	_, _ = io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = dst.Close()
}

// noteError records, or clears, the reason connections to a port are not reaching the guest.
func (f *Forwarder) noteError(e *entry, reason string) {
	f.mu.Lock()
	if f.entries[keyOf(e.pub)] != e || e.pub.LastError == reason {
		f.mu.Unlock()
		return
	}
	e.pub.LastError = reason
	list := f.listLocked()
	f.mu.Unlock()
	f.notify(list)
}

func (f *Forwarder) track(cs ...io.Closer) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	for _, c := range cs {
		f.conns[c] = struct{}{}
	}
	return true
}

func (f *Forwarder) untrack(cs ...io.Closer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range cs {
		delete(f.conns, c)
	}
}

// Close releases every host listener and ends every connection in flight.
//
// Both halves matter, and the first one is the one a leak check looks for: a host port still
// bound after the sandbox is gone is a socket accepting connections for a VM that no longer
// exists. It is idempotent, so it can sit in a defer beside the rest of the teardown.
func (f *Forwarder) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	entries := make([]*entry, 0, len(f.entries))
	for _, e := range f.entries {
		entries = append(entries, e)
	}
	f.entries = map[key]*entry{}
	conns := make([]io.Closer, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.conns = map[io.Closer]struct{}{}
	f.mu.Unlock()

	var errs []error
	for _, e := range entries {
		if err := e.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
		<-e.done
	}
	for _, c := range conns {
		_ = c.Close()
	}
	return errors.Join(errs...)
}

func (f *Forwarder) logf(format string, args ...any) {
	if f.log == nil {
		return
	}
	fmt.Fprintf(f.log, format+"\n", args...)
}

func keyOf(p Published) key {
	return key{host: p.HostIP, port: p.HostPort, sandbox: p.SandboxPort, udp: Protocol(p.Protocol).IsUDP()}
}

// transportOf strips the address-family suffix, so a published binding records the transport
// it carries and lets its own address say which family it is on.
func transportOf(p Protocol) Protocol {
	if p.IsUDP() {
		return UDP
	}
	return TCP
}
