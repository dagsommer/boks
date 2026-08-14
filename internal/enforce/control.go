package enforce

// The supervisor's control socket, and why one exists at all.
//
// # The conflict
//
// The netstack that terminates a sandbox's NIC lives in the per-sandbox supervisor, and that
// process was built deliberately with **no API exposed** — see supervisor.go, where it is
// listed among its security properties. It reads a spec on a pipe at startup and then talks
// to nothing.
//
// `boks ports --publish` contradicts that. A host listener forwarding into a sandbox can only
// live in the process holding that sandbox's link socket, because that process *is* the only
// way into the virtual network: one stack per sandbox, and a second one attached to the same
// link would hand out a duplicate address. So publishing a port on an already-running sandbox
// means telling a running supervisor something, and there was no way to tell it anything.
//
// # What was chosen, and what was rejected
//
// Three shapes were considered.
//
//   - **Publish only at creation.** `boks ports --publish` on a running sandbox becomes an
//     error telling the user to restart it. Honest, adds no surface, and rejected: it makes
//     the feature useless for the case that motivates it most. You do not know which port
//     your dev server will use until you have started it, and by then the sandbox is up —
//     "stop your agent and start it again" is not an answer for something you will do several
//     times an hour. sbx says explicitly that `sbx ports` is how a running sandbox is
//     changed, and a restart-only version would also make an OAuth flow with a loopback
//     redirect unservable, if one turns up.
//   - **A file plus a signal.** The CLI writes a desired-state file into the sandbox's state
//     directory and SIGHUPs the supervisor, which reconciles. No socket, no protocol. It was
//     close, and it lost on the return path: an ephemeral publish has to report *which* port
//     was allocated, and a failed bind has to report which port could not be taken and why.
//     Both would have to come back through the same file, polled, with a generation counter
//     to tell a stale answer from a fresh one — a request/response protocol with the
//     request/response part hidden.
//   - **A minimal control socket.** What is implemented here.
//
// # Why the socket does not cost what it looks like it costs
//
// "No API exposed" becomes "an owner-only local socket", and that is a real weakening, so it
// is worth being precise about what it does and does not open.
//
//   - **The guest cannot reach it.** This is the property that matters, and it is structural
//     rather than enforced. A sandbox's only channel to the host is the virtio-net link,
//     which terminates in the netstack; that stack forwards TCP to addresses the policy
//     permits and answers DNS, and it has no path to the filesystem at all. There is no
//     vsock, no shared directory containing this socket — the one host directory shared into
//     a sandbox is the CA certificate directory, read-only, and it lives under `certs/`,
//     nowhere near `net/`. A guest cannot open an AF_UNIX socket on a host path it cannot
//     name and could not reach if it could. A sandbox able to ask the host to publish its own
//     ports would have escaped in the way that matters; this one cannot ask.
//   - **The trust boundary is the same one the credential store already has.** The socket
//     lives in a 0700 directory in the user's state directory and is itself 0600, and every
//     connection's peer credentials are checked against this process's own uid where the
//     platform can report them. A process running as the same user could already read that
//     user's secret store, run `boks run --allow '*'`, or simply run `boks ports` itself, so
//     this grants it nothing it did not have.
//   - **The protocol cannot read anything.** Three verbs — list, publish, unpublish — and no
//     verb returns a secret, a policy, a spec or a log. The supervisor holds this sandbox's
//     credential values; nothing here can ask for them. An unrecognised verb is refused
//     rather than ignored, and a test pins that.
//   - **Publishing is a hole into the sandbox, not out of it.** The capability this socket
//     grants is "let something on this host connect into this VM". It does not widen the
//     policy, does not add a destination the guest may reach, and cannot move data outward
//     that the guest could not already move.
//
// The socket is created by the supervisor and dies with it. Nothing listens when no sandbox
// is running, which is the same property the supervisor itself has.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/ports"
)

// controlSocket is the socket's name inside a sandbox's network state directory.
//
// Eight characters, exactly as long as `net.sock`, and that is deliberate: a UNIX socket path
// is a fixed-size field of 104 bytes on macOS, network.NewPlan already refuses a sandbox whose
// link socket would overflow it, and a control socket with a longer name could fail to bind
// for a sandbox whose link socket fit.
const controlSocket = "ctl.sock"

// Control verbs. The set is closed, and each one is here because `boks ports` needs it.
const (
	opList      = "list"
	opPublish   = "publish"
	opUnpublish = "unpublish"
)

// controlRequest is one command. Specs are the strings the user typed, parsed in the
// supervisor as well as in the CLI: the CLI parses them to reject a mistake before it spawns
// anything, and the supervisor parses them because it must never trust what arrives on a
// socket, however well protected.
type controlRequest struct {
	Op    string   `json:"op"`
	Specs []string `json:"specs,omitempty"`
}

// controlResponse is the answer. Ports is always the full list after the change, so a caller
// never has to reconstruct the state from a delta.
type controlResponse struct {
	Error   string            `json:"error,omitempty"`
	Ports   []ports.Published `json:"ports,omitempty"`
	Changed []ports.Published `json:"changed,omitempty"`
}

const (
	// controlDeadline bounds one exchange. A client that connects and says nothing must
	// not hold a goroutine in the supervisor for the life of the sandbox.
	controlDeadline = 10 * time.Second
	// maxControlRequest caps a request. Nothing legitimate is near it; the cap exists so
	// that a request is a request rather than a way to grow the supervisor's memory.
	maxControlRequest = 64 << 10
)

// controlPath is where a sandbox's control socket lives.
func controlPath(stateDir, sandbox string) string {
	return filepath.Join(dirFor(stateDir, sandbox), controlSocket)
}

// controlSocketRefusal is why this socket is not bound on a platform that can enforce neither
// the mode it is created with nor the identity of whoever connects to it.
//
// The text lives here, in a file with no build constraint, so that a test can read it on the
// machine the tests run on; control_windows.go holds the argument and is the only caller. It
// is deliberately phrased for two audiences at once, because both see it: the supervisor logs
// it once at startup, and `boks ports` prints it to whoever asks a running Windows sandbox to
// change its ports.
func controlSocketRefusal() error {
	return errors.New("the supervisor's control socket is not bound on Windows, so the ports of a " +
		"running sandbox cannot be changed there.\nNeither protection it relies on exists on this " +
		"platform: the 0700 directory and 0600 socket are permission bits Windows ignores, and " +
		"AF_UNIX there carries no peer credentials, so the supervisor cannot check who connected " +
		"(GetNamedPipeClientProcessId answers that only for a named pipe). A socket that can open a " +
		"hole into a running VM is not worth binding on an argument boks cannot assert.\n" +
		"Ports given at creation time are unaffected: start the sandbox with the ports it needs " +
		"(boks run --publish ...)")
}

// controlServer answers control requests for one sandbox.
type controlServer struct {
	listener net.Listener
	done     chan struct{}
	handler  func(controlRequest) controlResponse
	log      io.Writer
}

// serveControl binds the control socket and starts answering.
//
// It is called by the supervisor after the stack is up. A failure to bind is a failure to
// start: a sandbox whose ports cannot be changed later, with no indication of why, is worse
// than one that refused to come up.
func serveControl(path string, log io.Writer, handler func(controlRequest) controlResponse) (*controlServer, error) {
	// A socket left by a crashed supervisor would make bind fail with "address already in
	// use" even though nothing holds it. The lock file, not this path, is what decides
	// whether another supervisor is alive; by the time this runs, that has been settled.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("boks net serve: removing a stale control socket: %w", err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("boks net serve: binding the control socket %s: %w", path, err)
	}
	// The containing directory is already 0700, which is what actually keeps other users
	// out; this is the belt to that pair of braces, and it costs one syscall.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("boks net serve: securing the control socket: %w", err)
	}

	s := &controlServer{listener: l, done: make(chan struct{}), handler: handler, log: log}
	go s.accept()
	return s, nil
}

func (s *controlServer) accept() {
	defer close(s.done)
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

// handle answers one request and closes. One exchange per connection: there is no session to
// keep, and a connection that cannot outlive its answer cannot be left half-open.
func (s *controlServer) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(controlDeadline))

	if uid, ok := peerUID(conn); ok && uid != os.Getuid() {
		// Belt and braces: the directory permissions already exclude other users. If
		// one arrives anyway, the supervisor says so rather than serving them.
		s.logf("network: refusing a control connection from uid %d", uid)
		_ = writeResponse(conn, controlResponse{Error: "this socket serves only the user who owns the sandbox"})
		return
	}

	req, err := readRequest(conn)
	if err != nil {
		_ = writeResponse(conn, controlResponse{Error: fmt.Sprintf("unreadable control request: %v", err)})
		return
	}
	_ = writeResponse(conn, s.handler(req))
}

// The four halves of the wire format. One JSON value in each direction, size-limited, so that
// a client which connects and then sends nothing useful costs the supervisor a bounded amount
// of memory and a bounded amount of time.
func writeRequest(w io.Writer, req controlRequest) error    { return writeJSONLine(w, req) }
func writeResponse(w io.Writer, resp controlResponse) error { return writeJSONLine(w, resp) }

func readRequest(r io.Reader) (controlRequest, error) {
	var req controlRequest
	err := json.NewDecoder(io.LimitReader(r, maxControlRequest)).Decode(&req)
	return req, err
}

func readResponse(r io.Reader) (controlResponse, error) {
	var resp controlResponse
	err := json.NewDecoder(io.LimitReader(r, maxControlRequest)).Decode(&resp)
	return resp, err
}

func writeJSONLine(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

func (s *controlServer) logf(format string, args ...any) {
	if s.log == nil {
		return
	}
	fmt.Fprintf(s.log, format+"\n", args...)
}

// Close stops answering and removes the socket.
func (s *controlServer) Close() error {
	err := s.listener.Close()
	<-s.done
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// ErrNoSupervisor is returned when a sandbox has no running network to talk to.
var ErrNoSupervisor = errors.New("this sandbox has no running network stack")

// Ports lists the ports published for a running sandbox.
func Ports(ctx context.Context, stateDir, sandbox string) ([]ports.Published, error) {
	resp, err := call(ctx, stateDir, sandbox, controlRequest{Op: opList})
	if err != nil {
		return nil, err
	}
	return resp.Ports, nil
}

// Publish asks a running sandbox's supervisor to publish ports, and returns the full list
// afterwards together with the bindings this call created.
func Publish(ctx context.Context, stateDir, sandbox string, specs []string) ([]ports.Published, []ports.Published, error) {
	resp, err := call(ctx, stateDir, sandbox, controlRequest{Op: opPublish, Specs: specs})
	if err != nil {
		return nil, nil, err
	}
	return resp.Ports, resp.Changed, nil
}

// Unpublish asks a running sandbox's supervisor to release ports.
func Unpublish(ctx context.Context, stateDir, sandbox string, specs []string) ([]ports.Published, []ports.Published, error) {
	resp, err := call(ctx, stateDir, sandbox, controlRequest{Op: opUnpublish, Specs: specs})
	if err != nil {
		return nil, nil, err
	}
	return resp.Ports, resp.Changed, nil
}

// call performs one exchange with a sandbox's supervisor.
func call(ctx context.Context, stateDir, sandbox string, req controlRequest) (controlResponse, error) {
	st, alive := Lookup(stateDir, sandbox)
	if !alive {
		return controlResponse{}, fmt.Errorf("%w: %s", ErrNoSupervisor, sandbox)
	}
	// A sandbox with no network runs a supervisor — the VM's NIC has to have somewhere to
	// write — but no stack, no control socket and nothing to publish into. Saying "no
	// running network stack" here would send the user to `boks start` for a sandbox that
	// is already running.
	if st.Mode == string(network.ModeNone) {
		return controlResponse{}, fmt.Errorf("sandbox %q was created with -net none, so it has no "+
			"virtual network to publish a port into.\nRecreate it without -net none: boks rm %s",
			sandbox, sandbox)
	}
	// Asked before dialling, so that the answer names the reason rather than the symptom.
	// Without this the user of a platform where the socket is deliberately not bound gets
	// "this sandbox has no running network stack (connect: no such file or directory)" for a
	// sandbox that is running perfectly well.
	if refusal := controlSocketSecurable(); refusal != nil {
		return controlResponse{}, refusal
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", controlPath(stateDir, sandbox))
	if err != nil {
		return controlResponse{}, fmt.Errorf("%w: %s (%v)", ErrNoSupervisor, sandbox, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(controlDeadline))
	}

	if err := writeRequest(conn, req); err != nil {
		return controlResponse{}, fmt.Errorf("sending a request to the network of %s: %w", sandbox, err)
	}
	resp, err := readResponse(conn)
	if err != nil {
		return controlResponse{}, fmt.Errorf("reading the reply from the network of %s: %w", sandbox, err)
	}
	if resp.Error != "" {
		return controlResponse{}, errors.New(resp.Error)
	}
	return resp, nil
}
