package enforce

// Why a sandbox's network lives in a process of its own.
//
// The host-side stack terminates the guest's NIC: the VM writes frames to a UNIX socket
// that a Boks process holds open. Whoever holds that socket *is* the sandbox's network. A
// persistent sandbox outlives the command that created it — that is the entire point of it
// — so if the stack lived in the CLI invocation, pressing Ctrl-C would silently disconnect
// a background build running inside the sandbox, and `boks run -d` would produce a sandbox
// with no network at all. The stack's lifetime has to be the *VM's* lifetime, and no CLI
// invocation has that lifetime.
//
// Three shapes were considered:
//
//   - **Attached only.** The stack lives while a CLI process is attached. Simplest, and
//     wrong for the reason above: it ties the network to the wrong object. It also stakes
//     everything on an unverified assumption — that a running VM re-attaches to a fresh
//     socket at the same path after the previous holder dies. The VMM connects to that path
//     once; whether it reconnects when the peer disappears is not something this project
//     has been able to test, and if the answer is no, the first Ctrl-C costs the sandbox
//     its network permanently.
//   - **A per-sandbox supervisor.** One small process per *running* sandbox, spawned on
//     demand by whichever command starts the VM, which exits when the VM's task exits.
//     What is implemented here.
//   - **A global daemon**, as Docker Sandboxes runs with `sandboxd`. One supervision point
//     for every sandbox, and the natural home for port forwarding and live stats. It is
//     also an always-on service the user did not ask for, holding every sandbox's
//     credentials and CA, whose crash takes out every sandbox at once, and which needs its
//     own start/stop/status surface and its own state store. Boks does not have one and does
//     not need one: containerd already supervises the VMs.
//
// The supervisor is deliberately the smallest thing that can own that lifetime:
//
//   - It is started on demand, never at boot and never by an installer. A host with no
//     running sandbox runs no Boks process.
//   - It needs no privilege beyond what the CLI already has — it binds a UNIX socket in the
//     user's state directory and talks to the same containerd.
//   - Its life is bounded by the sandbox's task, which it watches through containerd. A VM
//     that dies, is stopped, or is removed takes its supervisor with it, so there is no
//     orphan to reap in the normal case, and no `boks daemon stop` to remember.
//   - It holds a lock file for as long as it lives, so a crashed supervisor is detected by
//     the next command rather than by a PID that may have been reused, and its leftovers
//     are cleaned up before a new one starts.
//   - It receives the secrets it needs on a pipe and never the passphrase to the store, so
//     it can attach the credentials configured for one sandbox and cannot obtain any other.
//
// What is unverified, and what would settle it: whether a VM whose link socket disappeared
// re-attaches when a new stack binds the same path. If it does, a crashed supervisor is
// recoverable by simply running another command; if it does not, the sandbox needs a
// restart to get its network back. Both are handled the same way here — the next command
// cleans up and starts a fresh supervisor — but the user-visible consequence differs, and
// only a host with a hypervisor can tell us which it is.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/ports"
)

const (
	stateFile = "stack.json"
	lockFile  = "stack.lock"
	logFile   = "stack.log"

	// readyTimeout bounds the wait for a spawned supervisor to report that the link
	// socket is bound. It is short because everything before that point is local: no
	// image pull, no network.
	readyTimeout = 20 * time.Second

	// TaskAppearTimeout bounds how long a supervisor waits for the sandbox's task to
	// exist before concluding that the command that spawned it failed. It has to cover an
	// image pull on a slow link, because the CLI starts the network before it creates the
	// container — the VM connects to the socket while it boots, so the socket cannot
	// appear afterwards.
	TaskAppearTimeout = 15 * time.Minute

	// TaskPollInterval is how often the supervisor asks containerd whether the sandbox is
	// still running. It is a local call; the interval trades a little latency in teardown
	// for not holding a streaming watch open for the life of a sandbox.
	TaskPollInterval = 2 * time.Second

	readyMarker = "ready"
)

// State is the record a running supervisor leaves in the sandbox's state directory, so
// that other commands can find it, report it and stop it.
type State struct {
	Sandbox   string    `json:"sandbox"`
	PID       int       `json:"pid"`
	Mode      string    `json:"mode"`
	Socket    string    `json:"socket"`
	Gateway   string    `json:"gateway"`
	ProxyURL  string    `json:"proxy_url,omitempty"`
	Started   time.Time `json:"started"`
	LogPath   string    `json:"log_path"`
	Intercept bool      `json:"intercept"`
	// Ports is what this sandbox currently publishes, rewritten by the supervisor after
	// every change.
	//
	// It is duplicated here, rather than only being answerable over the control socket,
	// so that `boks ls` can render a PORTS column for every sandbox by reading files. A
	// listing that opened a socket per sandbox would be a listing that one wedged
	// supervisor could hang.
	Ports []ports.Published `json:"ports,omitempty"`
}

// dirFor is where one sandbox's network state lives. It is the directory that holds the
// link socket, so that everything belonging to a sandbox's network is removed together.
func dirFor(stateDir, sandbox string) string {
	return filepath.Join(stateDir, "net", sanitize(sandbox))
}

// StateDir is exported for the CLI, which builds the plan and so must place the socket in
// the same directory the supervisor will look in.
func StateDir(stateDir, sandbox string) string { return dirFor(stateDir, sandbox) }

// Lookup returns the running supervisor for a sandbox, if there is one.
//
// Liveness is decided by the lock file rather than by the recorded PID: a PID can be reused
// between a crash and this call, and signalling a stranger's process is not an acceptable
// failure mode.
func Lookup(stateDir, sandbox string) (State, bool) {
	dir := dirFor(stateDir, sandbox)
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return State{}, false
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, false
	}
	if !locked(filepath.Join(dir, lockFile)) {
		return st, false
	}
	return st, true
}

// List returns every running supervisor, for `boks net ls`.
func List(stateDir string) []State {
	entries, err := os.ReadDir(filepath.Join(stateDir, "net"))
	if err != nil {
		return nil
	}
	var out []State
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stateDir, "net", e.Name(), stateFile))
		if err != nil {
			continue
		}
		var st State
		if err := json.Unmarshal(data, &st); err != nil {
			continue
		}
		if !locked(filepath.Join(stateDir, "net", e.Name(), lockFile)) {
			continue
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sandbox < out[j].Sandbox })
	return out
}

// Ensure makes sure a sandbox has a running network, and returns its state.
//
// If one is already running — another `boks run` attached to the same sandbox, or the
// supervisor started when the sandbox came up — it is reused as it stands. Otherwise the
// leftovers of any dead one are cleared and a new supervisor is spawned.
//
// It must be called before the sandbox's task starts.
func Ensure(ctx context.Context, spec Spec, progress io.Writer) (State, error) {
	if spec.Sandbox == "" {
		return State{}, errors.New("enforce: no sandbox name")
	}
	if st, ok := Lookup(spec.StateDir, spec.Sandbox); ok {
		return st, nil
	}
	// A dead supervisor leaves a socket and a state file behind. Removing them here,
	// rather than leaving them for a human, is what makes a crash recoverable by running
	// the command again.
	dir := dirFor(spec.StateDir, spec.Sandbox)
	if err := os.RemoveAll(dir); err != nil {
		return State{}, fmt.Errorf("enforce: clearing %s: %w", dir, err)
	}
	return spawn(ctx, spec, progress)
}

// Stop ends a sandbox's network and removes its state directory. Stopping one that is not
// running is not an error: callers mostly want "make sure it is gone".
func Stop(stateDir, sandbox string) error {
	dir := dirFor(stateDir, sandbox)
	st, running := Lookup(stateDir, sandbox)
	if running && st.PID > 0 {
		if err := terminate(st.PID); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("enforce: stopping the network of sandbox %q (pid %d): %w", sandbox, st.PID, err)
		}
		// The supervisor removes its own directory on the way out; wait for it rather
		// than racing it, so a stop followed by a start cannot collide on the socket.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, alive := Lookup(stateDir, sandbox); !alive {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("enforce: removing %s: %w", dir, err)
	}
	return nil
}

// Forget removes what a sandbox leaves on the host beyond its stack: the directory holding
// the copy of the CA certificate that was shared into it.
//
// It is separate from Stop because the two have different lifetimes. The stack goes away
// whenever the sandbox stops; the certificate directory is the source of a mount and has to
// survive a stop, so that the sandbox can be started again. Only removal takes it.
func Forget(stateDir, sandbox string) error {
	dir := filepath.Join(stateDir, "certs", sanitize(sandbox))
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("enforce: removing %s: %w", dir, err)
	}
	return nil
}

// supervisorFailure is the reason a supervisor gave for not coming up, taken from its log,
// or "" if it wrote nothing usable.
//
// The supervisor's stderr is the log, and the last thing a failing one writes there is the
// error the CLI prints for it, prefixed with "boks: ". Starting from that prefix keeps a
// multi-line explanation whole, instead of quoting its final fragment; anything else falls
// back to the last line written. Reading it costs nothing and turns "look in this file" into
// the answer the file contains.
func supervisorFailure(logPath string) string {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return ""
	}
	if i := strings.LastIndex(text, cliErrorPrefix); i >= 0 {
		text = text[i+len(cliErrorPrefix):]
	} else if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	return strings.TrimSpace(text)
}

// cliErrorPrefix is what cmd/boks puts in front of an error on its way out; see cli.Main.
const cliErrorPrefix = "boks: "

// spawn starts a supervisor process and waits for it to report the link socket bound.
func spawn(ctx context.Context, spec Spec, progress io.Writer) (State, error) {
	self, err := os.Executable()
	if err != nil {
		return State{}, fmt.Errorf("enforce: locating the boks binary: %w", err)
	}
	dir := dirFor(spec.StateDir, spec.Sandbox)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return State{}, fmt.Errorf("enforce: creating %s: %w", dir, err)
	}
	logDest, err := os.OpenFile(filepath.Join(dir, logFile), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return State{}, fmt.Errorf("enforce: opening the network log: %w", err)
	}
	defer logDest.Close()

	payload, err := json.Marshal(spec)
	if err != nil {
		return State{}, fmt.Errorf("enforce: encoding the network spec: %w", err)
	}

	cmd := exec.Command(self, "net", "serve")
	cmd.Stdin = strings.NewReader(string(payload))
	cmd.Stderr = logDest
	// A new session: the supervisor must not receive the Ctrl-C meant for the command
	// that spawned it, and must not die with the terminal it was started from.
	detach(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return State{}, fmt.Errorf("enforce: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return State{}, fmt.Errorf("enforce: starting the network supervisor: %w", err)
	}
	// Nothing waits on this process: it outlives us by design. Release the child handle
	// so we do not leave a zombie for the lifetime of this command.
	defer func() { _ = cmd.Process.Release() }()

	logPath := filepath.Join(dir, logFile)
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		switch {
		case err != nil:
			// The supervisor's stdout carries nothing but the ready marker, so end
			// of file here means the process is gone. *Why* it is gone is in its
			// log, and the difference between "it crashed" and "it declined to
			// start" is the whole content of the message: a supervisor that refused
			// deliberately is not one that died, and a user told it died goes
			// looking for a crash that never happened.
			if reason := supervisorFailure(logPath); reason != "" {
				ready <- fmt.Errorf("the network supervisor did not start: %s\n(its full log is %s)",
					reason, logPath)
				return
			}
			ready <- fmt.Errorf("the network supervisor exited before it was ready, saying nothing; see %s",
				logPath)
		case strings.TrimSpace(line) != readyMarker:
			ready <- fmt.Errorf("the network supervisor said %q instead of %q", strings.TrimSpace(line), readyMarker)
		default:
			ready <- nil
		}
	}()

	select {
	case err := <-ready:
		if err != nil {
			_ = terminate(cmd.Process.Pid)
			return State{}, fmt.Errorf("enforce: %w", err)
		}
	case <-time.After(readyTimeout):
		_ = terminate(cmd.Process.Pid)
		return State{}, fmt.Errorf("enforce: the network supervisor did not come up within %s; see %s",
			readyTimeout, logPath)
	case <-ctx.Done():
		_ = terminate(cmd.Process.Pid)
		return State{}, ctx.Err()
	}

	st, ok := Lookup(spec.StateDir, spec.Sandbox)
	if !ok {
		return State{}, errors.New("enforce: the network supervisor reported ready but left no state")
	}
	if progress != nil {
		if st.ProxyURL == "" {
			// -net none still runs a process: the VM's NIC has to have somewhere to
			// write or it does not come up. Saying what it writes to is more useful
			// than announcing a stack that is not there.
			fmt.Fprintf(progress, "network: none for %s — the VM's NIC ends in a discard socket (pid %d)\n",
				st.Sandbox, st.PID)
		} else {
			fmt.Fprintf(progress, "network: %s stack for %s, proxy at %s (pid %d)\n",
				st.Mode, st.Sandbox, st.ProxyURL, st.PID)
		}
	}
	return st, nil
}

// Serve is the supervisor process itself: `boks net serve`, reading its spec from stdin.
//
// It is not meant to be typed by a human, but it is a plain command rather than a hidden
// one: a background process that cannot be inspected, reproduced or run in the foreground
// is a thing users are right to distrust.
//
// The sequence matters. The link socket must be bound before the caller starts the task,
// because the VM connects to it while it boots; so the socket is bound, the state written
// and readiness reported, and only then does the supervisor start watching the task it is
// bound to.
func Serve(ctx context.Context, spec Spec, ready io.Writer, watch func(context.Context) error) error {
	if spec.Sandbox == "" {
		return errors.New("boks net serve: the spec names no sandbox")
	}
	dir := dirFor(spec.StateDir, spec.Sandbox)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("boks net serve: creating %s: %w", dir, err)
	}
	// Held for the life of the process. Everything else — the state file, the socket —
	// can be left behind by a crash; the lock cannot, which is what makes "is this
	// sandbox's network alive" answerable without trusting a PID.
	release, err := acquire(filepath.Join(dir, lockFile))
	if err != nil {
		if errors.Is(err, errLockHeld) {
			return fmt.Errorf("boks net serve: sandbox %q already has a network supervisor: %w",
				spec.Sandbox, err)
		}
		// The lock was not taken, and nothing here knows why — so nothing here may
		// invent a reason. Anything but a held lock is reported as itself: a platform
		// that refuses to run a supervisor at all says that, and a state directory
		// that cannot be written says that, instead of both being announced as a
		// supervisor that is already running.
		return fmt.Errorf("boks net serve: sandbox %q: %w", spec.Sandbox, err)
	}
	defer release()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// SIGTERM is how `boks stop`, `boks rm` and `boks net stop` end this process. SIGINT
	// is here for a supervisor someone ran in the foreground to watch it.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	session, err := Open(ctx, spec, os.Stderr)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := session.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "closing the network: %v\n", cerr)
		}
		// The stack is gone, so nothing may still advertise it. Removing the whole
		// directory takes the socket, the state file and the log with it.
		_ = os.RemoveAll(dir)
	}()

	st := State{
		Sandbox:   spec.Sandbox,
		PID:       os.Getpid(),
		Mode:      string(spec.Plan.Mode),
		Socket:    spec.Plan.Socket,
		Gateway:   spec.Plan.Gateway.String(),
		ProxyURL:  session.ProxyURL(),
		Started:   time.Now(),
		LogPath:   filepath.Join(dir, logFile),
		Intercept: spec.intercepts(),
		Ports:     session.Ports(),
	}
	if err := writeState(dir, st); err != nil {
		return err
	}

	// The control socket is what makes `boks ports` work on a *running* sandbox. See
	// control.go for why a supervisor that was built with no API now has one, and for the
	// argument that the guest cannot reach it. A sandbox with no network gets none: there
	// is nothing to publish into, so there is nothing to ask for.
	if spec.Plan.Mode != network.ModeNone {
		published := &publishedState{dir: dir, state: st}
		session.forwarder.OnChange(published.write)
		control, err := serveControl(controlPath(spec.StateDir, spec.Sandbox), os.Stderr,
			func(req controlRequest) controlResponse { return handleControl(session, req) })
		if err != nil {
			return err
		}
		defer func() { _ = control.Close() }()
	}

	fmt.Fprintln(ready, readyMarker)

	watched := make(chan error, 1)
	go func() { watched <- watch(ctx) }()

	select {
	case sig := <-signals:
		fmt.Fprintf(os.Stderr, "network: %s, tearing down the stack for %s\n", sig, spec.Sandbox)
	case err := <-watched:
		if err != nil {
			fmt.Fprintf(os.Stderr, "network: watching sandbox %s: %v\n", spec.Sandbox, err)
		} else {
			fmt.Fprintf(os.Stderr, "network: sandbox %s stopped, tearing down the stack\n", spec.Sandbox)
		}
	case <-ctx.Done():
	}
	return nil
}

// publishedState keeps the sandbox's state file in step with what is actually published.
//
// The forwarder calls write from whichever goroutine changed something — a control request,
// or a connection that discovered nothing was listening in the guest — so the mutex is doing
// real work rather than guarding against a theoretical race.
type publishedState struct {
	dir string
	mu  sync.Mutex
	// state is the whole record, kept so that rewriting the ports does not lose the rest
	// of it.
	state State
}

func (p *publishedState) write(list []ports.Published) {
	p.mu.Lock()
	p.state.Ports = list
	st := p.state
	p.mu.Unlock()
	if err := writeState(p.dir, st); err != nil {
		fmt.Fprintf(os.Stderr, "network: recording the published ports: %v\n", err)
	}
}

// handleControl answers one control request. It is the entire API the supervisor exposes.
//
// Every specification is parsed here as well as in the CLI. The CLI parses to fail fast on a
// typo; this parses because a process that trusts what arrives on a socket is a process whose
// protection is the socket's permissions alone, and one control is not enough for something
// that opens a hole into a VM.
func handleControl(session *Session, req controlRequest) controlResponse {
	if session.forwarder == nil {
		return controlResponse{Error: "this sandbox has no virtual network to publish a port into"}
	}
	switch req.Op {
	case opList:
		return controlResponse{Ports: session.Ports()}

	case opPublish:
		var changed []ports.Published
		for _, text := range req.Specs {
			spec, err := ports.ParsePublish(text)
			if err != nil {
				return controlResponse{Error: err.Error(), Ports: session.Ports(), Changed: changed}
			}
			added, err := session.forwarder.Publish(spec)
			if err != nil {
				// Deliberately not unwound. The ports published before the
				// failure are working, and taking them away would surprise a
				// user who asked for three and can have two.
				return controlResponse{Error: err.Error(), Ports: session.Ports(), Changed: changed}
			}
			changed = append(changed, added...)
		}
		return controlResponse{Ports: session.Ports(), Changed: changed}

	case opUnpublish:
		var changed []ports.Published
		for _, text := range req.Specs {
			spec, err := ports.ParseUnpublish(text)
			if err != nil {
				return controlResponse{Error: err.Error(), Ports: session.Ports(), Changed: changed}
			}
			removed, err := session.forwarder.Unpublish(spec)
			if err != nil {
				return controlResponse{Error: err.Error(), Ports: session.Ports(), Changed: changed}
			}
			changed = append(changed, removed...)
		}
		return controlResponse{Ports: session.Ports(), Changed: changed}

	default:
		// Refused rather than ignored. The set of verbs is closed on purpose — nothing
		// here reads a secret, a policy or a log — and a supervisor that quietly
		// accepted an unknown one would be a supervisor whose surface nobody can state.
		return controlResponse{Error: fmt.Sprintf("unknown control operation %q; this socket accepts "+
			"only %s, %s and %s", req.Op, opList, opPublish, opUnpublish)}
	}
}

func writeState(dir string, st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	// 0600: it names the socket and the proxy address of one user's sandbox. It holds no
	// secret, and it is still nobody else's business.
	if err := os.WriteFile(filepath.Join(dir, stateFile), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("boks net serve: writing %s: %w", filepath.Join(dir, stateFile), err)
	}
	return nil
}

// ReadSpec decodes a supervisor's spec from a pipe.
func ReadSpec(r io.Reader) (Spec, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Spec{}, fmt.Errorf("boks net serve: reading the spec: %w", err)
	}
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return Spec{}, fmt.Errorf("boks net serve: decoding the spec: %w", err)
	}
	return spec, nil
}
