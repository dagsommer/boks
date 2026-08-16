// Package daemon runs the containerd that Boks drives.
//
// # Why Boks starts a daemon at all
//
// Boks orchestrates a stack it does not contain, and every tester so far has lost time to the
// same step: getting containerd running, configured correctly, and pointed at the shim. The
// failures are not Boks' code and they do not look like anyone's code. On Linux the last two
// were containerd chowning its ttrpc socket to uid 0, and its diff-service default order being
// ['walking'], which cannot unpack a stacked EROFS layer. On Windows they were a protected
// DACL on a directory containerd created for itself, a cimfs snapshotter failing at init and
// taking forty plugins with it, and the erofs differ being registered but never asked. Five
// blockers, none of them Boks, all of them reported as Boks.
//
// A daemon Boks starts is a daemon whose configuration Boks writes, and each of those five is
// a setting or a directory. See config.go, which carries the reasons in the generated file.
//
// # What this is not
//
// It is not always-on. Nothing is installed as a service, nothing runs at boot, and a host
// that has not run `boks daemon start` runs no Boks process at all. It is not a replacement
// for a containerd the user already has either: `boks daemon` uses its own root, its own
// state and its own endpoint, so a machine running Docker's containerd keeps doing so and the
// two never see each other. And it is not required — Boks still talks to whatever
// --containerd-address names, and a user who has containerd set up the way they want should
// keep using it.
//
// # The supervisor
//
// containerd runs as the child of a small `boks daemon serve` process rather than being
// spawned and forgotten. That process costs almost nothing and buys three things:
//
//   - Liveness is a held lock rather than a recorded PID. See internal/proclock for why that
//     distinction is worth a process.
//   - containerd's own stderr is captured to a file, so when the daemon refuses to start,
//     `boks daemon start` can print the reason containerd gave instead of "it did not come
//     up". That is the whole complaint this package exists to answer.
//   - Stopping is exact. Stop signals containerd — whose PID cannot have been reused, because
//     its parent is alive and has not reaped it — and the supervisor exits behind it.
//
// It is the same shape as the per-sandbox network supervisor in internal/enforce, deliberately.
// That package's design note argues against "a global daemon, as Docker Sandboxes runs with
// sandboxd", and this is not one: it supervises containerd, holds no credentials, serves no
// API of its own, and its crash costs the user a `boks daemon start` rather than every
// sandbox's network.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/dagsommer/boks/internal/proclock"
	"github.com/dagsommer/boks/internal/runtimecfg"
)

const (
	stateFile  = "daemon.json"
	lockFile   = "daemon.lock"
	logFile    = "containerd.log"
	configFile = "config.toml"

	// socketName is the gRPC endpoint's file name on Unix. Windows uses pipeName instead.
	socketName = "containerd.sock"

	// ttrpcSuffix is what containerd appends to the gRPC address when deriving the ttrpc
	// one. Boks writes the derived name explicitly so that it can attach a uid and gid,
	// which containerd only copies across when the section is absent.
	ttrpcSuffix = ".ttrpc"

	// pipeName is the Windows endpoint. It is a fixed name rather than one derived from
	// the state directory because a named pipe is not a path on disk and has no directory
	// to be derived from.
	pipeName = `\\.\pipe\boks-containerd`

	// readyTimeout bounds the wait for containerd to answer its own socket. Everything
	// before that point is local — no image pull, no network — but bolt recovery on a
	// large content store is not instant, so this is generous rather than tight.
	readyTimeout = 30 * time.Second

	// readyPoll is how often the socket is tried while waiting.
	readyPoll = 100 * time.Millisecond

	// stopGrace is how long Stop waits for a signalled containerd to exit before it
	// terminates the supervisor as well.
	stopGrace = 15 * time.Second

	readyMarker = "ready"

	// unixSocketMax is containerd's own limit, from pkg/sys/socket_unix.go: "BSDs have a
	// 104 limit". Exceeding it fails at listen time with a message about the socket rather
	// than about the directory that made it long, so Boks checks first.
	unixSocketMax = 104
)

// State is the record a running supervisor leaves, so other commands can report and stop it.
type State struct {
	// Address is the endpoint containerd serves on.
	Address string `json:"address"`
	// Binary is the containerd that was started, and Version what it reported.
	Binary  string `json:"binary"`
	Version string `json:"version,omitempty"`
	// ConfigPath and LogPath are for the user, who will want both when something is wrong.
	ConfigPath string `json:"config_path"`
	LogPath    string `json:"log_path"`
	// SupervisorPID is the `boks daemon serve` process; ContainerdPID its child.
	SupervisorPID int       `json:"supervisor_pid"`
	ContainerdPID int       `json:"containerd_pid"`
	Started       time.Time `json:"started"`
}

// Dir is where the managed daemon's files live: its configuration, its log, its lock, and
// containerd's own root and state.
//
// containerd's root holds unpacked image layers and can reach gigabytes, which is why it is
// under the state directory rather than a cache: a cache is something a machine may clear at
// any moment, and clearing this one under a running sandbox would remove the filesystem the
// sandbox is executing from.
func Dir(stateDir string) string { return filepath.Join(stateDir, "containerd") }

// addressIn returns the endpoint for a daemon whose files live in dir.
func addressIn(dir string) string {
	if runtime.GOOS == "windows" {
		return pipeName
	}
	return filepath.Join(dir, socketName)
}

// Address is the endpoint a Boks-managed containerd serves on.
func Address(stateDir string) string { return addressIn(Dir(stateDir)) }

// LogPath is where the managed daemon's output is written.
func LogPath(stateDir string) string { return filepath.Join(Dir(stateDir), logFile) }

// ConfigPath is where the generated containerd configuration is written.
func ConfigPath(stateDir string) string { return filepath.Join(Dir(stateDir), configFile) }

// currentIDs returns the uid and gid containerd's listeners must be owned by.
func currentIDs() (int, int) { return os.Getuid(), os.Getgid() }

// Lookup returns the running managed daemon, if there is one.
//
// Liveness is the supervisor's lock, not the recorded PID: a PID can be reused between a crash
// and this call. The returned State is still filled in for a daemon that is not running, so
// that callers can show where its log was.
func Lookup(stateDir string) (State, bool) {
	dir := Dir(stateDir)
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return State{}, false
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, false
	}
	return st, proclock.Locked(filepath.Join(dir, lockFile))
}

// Running reports whether Boks is managing a containerd right now.
func Running(stateDir string) bool {
	_, ok := Lookup(stateDir)
	return ok
}

// DefaultAddress is the containerd endpoint Boks talks to when the user has not named one.
//
// The managed daemon wins over the platform default, because a user who has started one has
// said which containerd they mean, and having `boks daemon start` succeed while `boks run`
// carried on talking to a socket in /run would be the most confusing outcome available.
//
// An explicit BOKS_CONTAINERD_ADDRESS still wins over both: it is the more specific
// instruction, and someone who set it is pointing Boks at a particular daemon on purpose.
// --containerd-address wins over everything, being handled by the flag itself.
func DefaultAddress(stateDir string) string {
	if addr, ok := runtimecfg.AddressOverride(); ok {
		return addr
	}
	if Running(stateDir) {
		return Address(stateDir)
	}
	return runtimecfg.DefaultAddress()
}

// Status is what `boks daemon status` reports, and it deliberately asks two different
// questions rather than one.
//
// A held lock says a supervisor is alive. A version returned over the socket says containerd
// is actually serving. They can disagree — during startup, and on Windows for an unspecified
// interval after a crash — and a status command that collapsed them would report a daemon
// that answers nothing as running.
type Status struct {
	// Managed reports whether a Boks-managed supervisor holds the lock.
	Managed bool
	// State is the record it left, empty if there is none.
	State State
	// Version is what containerd answered over its API, empty if it answered nothing.
	Version string
	// Err is why it answered nothing, when it did not.
	Err error
}

// Query reports on the managed daemon.
func Query(ctx context.Context, stateDir string) Status {
	st, managed := Lookup(stateDir)
	status := Status{Managed: managed, State: st}
	if !managed {
		return status
	}
	version, err := probe(ctx, st.Address)
	status.Version, status.Err = version, err
	return status
}

// probe asks containerd for its version, which is the only answer that proves it is serving.
func probe(ctx context.Context, address string) (string, error) {
	client, err := runtimecfg.Connect(ctx, address)
	if err != nil {
		return "", err
	}
	defer client.Close()
	version, err := client.Version(ctx)
	if err != nil {
		return "", err
	}
	return version.Version, nil
}

// Start makes sure a managed containerd is running and returns its state.
//
// A daemon that is already running is returned as it stands rather than restarted: `boks
// daemon start` is "make sure it is up", and restarting one that is serving sandboxes would
// take them down.
func Start(ctx context.Context, stateDir string, progress io.Writer) (State, error) {
	if st, ok := Lookup(stateDir); ok {
		return st, nil
	}
	dir := Dir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return State{}, fmt.Errorf("creating %s: %w", dir, err)
	}
	// A dead supervisor leaves a state file and possibly a socket. Both are removed;
	// containerd's root and state are not, because they hold the image content store and
	// removing them would turn a crash into a re-download of every image.
	_ = os.Remove(filepath.Join(dir, stateFile))
	if runtime.GOOS != "windows" {
		_ = os.Remove(addressIn(dir))
		_ = os.Remove(addressIn(dir) + ttrpcSuffix)
	}
	return spawn(ctx, stateDir, progress)
}

// spawn starts the supervisor and waits for it to report containerd serving.
func spawn(ctx context.Context, stateDir string, progress io.Writer) (State, error) {
	self, err := os.Executable()
	if err != nil {
		return State{}, fmt.Errorf("locating the boks binary: %w", err)
	}
	dir := Dir(stateDir)
	logDest, err := os.OpenFile(filepath.Join(dir, logFile), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return State{}, fmt.Errorf("opening the daemon log: %w", err)
	}
	defer logDest.Close()

	cmd := exec.Command(self, "daemon", "serve")
	cmd.Env = append(os.Environ(), stateDirEnv+"="+stateDir)
	cmd.Stderr = logDest
	proclock.Detach(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return State{}, err
	}
	if err := cmd.Start(); err != nil {
		return State{}, fmt.Errorf("starting the containerd supervisor: %w", err)
	}
	// Nothing waits on this process: it outlives us by design.
	defer func() { _ = cmd.Process.Release() }()

	logPath := filepath.Join(dir, logFile)
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		switch {
		case err != nil:
			ready <- startFailure(logPath)
		case strings.TrimSpace(line) != readyMarker:
			ready <- fmt.Errorf("the containerd supervisor said %q instead of %q",
				strings.TrimSpace(line), readyMarker)
		default:
			ready <- nil
		}
	}()

	select {
	case err := <-ready:
		if err != nil {
			_ = proclock.Terminate(cmd.Process.Pid)
			return State{}, err
		}
	case <-time.After(readyTimeout + 5*time.Second):
		_ = proclock.Terminate(cmd.Process.Pid)
		return State{}, fmt.Errorf("containerd did not answer within %s.\nIts log is %s",
			readyTimeout, logPath)
	case <-ctx.Done():
		_ = proclock.Terminate(cmd.Process.Pid)
		return State{}, ctx.Err()
	}

	st, ok := Lookup(stateDir)
	if !ok {
		return State{}, errors.New("the containerd supervisor reported ready but left no state")
	}
	if progress != nil {
		fmt.Fprintf(progress, "containerd %s listening on %s\n", st.Version, st.Address)
		report(progress, Preflight(settingsFor(dir, HasEROFS(), HasExt4())))
		// The skew check runs here rather than before the start, because the version
		// that matters is the one the daemon reported over its own API — not what its
		// binary claims on a --version line, and not what its file name suggests.
		if skew := CheckSkew(st.Version, ShimContainerd(FindShim(runtimecfg.Runtime, daemonPath(os.Getenv("PATH"))))); skew != nil {
			report(progress, []Note{{Name: "runtime skew", Detail: skew.Detail, Remedy: skew.Remedy}})
		}
	}
	return st, nil
}

// report prints notes the way doctor's remedies are printed: the statement, then the
// explanation indented under it.
func report(w io.Writer, notes []Note) {
	for _, note := range notes {
		fmt.Fprintf(w, "\n%s: %s\n%s\n", note.Name, note.Detail, indent(note.Remedy))
	}
}

// startFailure turns a supervisor that exited before reporting ready into the reason it gave.
//
// This function is the reason the supervisor exists. containerd's failures are specific and
// well-written — "needed differ not loaded: erofs", "chown …containerd.sock.ttrpc: operation
// not permitted" — and they all used to be lost, because the daemon was something the user
// started in another terminal and Boks only ever saw a socket that was not there. Reading the
// log here costs nothing and turns "look in this file" into the answer the file contains.
func startFailure(logPath string) error {
	tail := logTail(logPath, 12)
	if tail == "" {
		return fmt.Errorf("containerd exited before it was ready, and wrote nothing to %s", logPath)
	}
	return fmt.Errorf("containerd exited before it was ready. It said:\n\n%s\n\n(the full log is %s)",
		indent(tail), logPath)
}

// logMarker separates what the supervisor said from what containerd said.
//
// It earns its place. The supervisor writes its preflight notes to the same file, and those
// notes are long — the /run/containerd one is a dozen lines — so a plain tail of the log
// reports Boks' own advice as though it were containerd's dying words, burying the one line
// that says what went wrong. Everything after this marker came from containerd.
const logMarker = "--- containerd ---"

// logTail returns the last n non-empty lines containerd wrote, ignoring anything the
// supervisor said before starting it.
func logTail(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := string(data)
	if i := strings.LastIndex(text, logMarker); i >= 0 {
		text = text[i+len(logMarker):]
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimRight(line, "\r"))
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func indent(text string) string {
	return "  " + strings.ReplaceAll(text, "\n", "\n  ")
}

// Stop ends the managed daemon. Stopping one that is not running is not an error: callers
// mostly want "make sure it is gone".
//
// containerd is signalled first, not the supervisor. Its PID is safe to signal for a reason
// worth stating: it is the supervisor's child, the supervisor is alive (the lock said so) and
// has not reaped it, so the number cannot have been recycled — on Unix an unreaped child holds
// its PID as a zombie, and on Windows the parent's open handle reserves it. Signalling the
// supervisor first would work on Unix, where it forwards SIGTERM, and would orphan containerd
// on Windows, where proclock.Terminate is TerminateProcess and nothing is forwarded.
func Stop(stateDir string) error {
	dir := Dir(stateDir)
	st, running := Lookup(stateDir)
	if !running {
		_ = os.Remove(filepath.Join(dir, stateFile))
		return nil
	}
	if st.ContainerdPID > 0 {
		if err := proclock.Terminate(st.ContainerdPID); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("stopping containerd (pid %d): %w", st.ContainerdPID, err)
		}
	}
	deadline := time.Now().Add(stopGrace)
	for time.Now().Before(deadline) {
		if !Running(stateDir) {
			_ = os.Remove(filepath.Join(dir, stateFile))
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	// containerd did not go. Take the supervisor with it rather than reporting a stop that
	// did not happen.
	if st.SupervisorPID > 0 {
		if err := proclock.Terminate(st.SupervisorPID); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("containerd (pid %d) did not exit within %s, and the supervisor could not be stopped: %w",
				st.ContainerdPID, stopGrace, err)
		}
	}
	if Running(stateDir) {
		return fmt.Errorf("containerd (pid %d) did not exit within %s; its log is %s",
			st.ContainerdPID, stopGrace, st.LogPath)
	}
	_ = os.Remove(filepath.Join(dir, stateFile))
	return nil
}

// checkSocketPath refuses a Unix socket path containerd would reject at listen time.
//
// The failure without this is "failed to create unix socket on …: unix socket path too long
// (> 104)", which names the socket and not the state directory that made it long — and the
// state directory is the thing the user can change.
func checkSocketPath(address string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	longest := address + ttrpcSuffix
	if len(longest) <= unixSocketMax {
		return nil
	}
	return fmt.Errorf("the containerd socket path would be %d bytes, and containerd's limit is %d:\n"+
		"  %s\n"+
		"Set BOKS_STATE_DIR to somewhere shorter and run this again.",
		len(longest), unixSocketMax, longest)
}
