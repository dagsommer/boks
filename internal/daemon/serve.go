package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/proclock"
	"github.com/dagsommer/boks/internal/runtimecfg"
)

// stateDirEnv is how the supervisor is told which state directory it serves. It is the same
// variable a user sets, so the child computes exactly the directory the parent did.
const stateDirEnv = policy.StateDirEnv

// Serve runs one managed containerd in the foreground and returns when it exits.
//
// It is a normal command (`boks daemon serve`) rather than a hidden one for the same reason
// `boks net serve` is: a background process nobody can reproduce, watch or debug is a thing
// users are right to distrust. Running it by hand in a terminal is the supported way to watch
// a daemon that will not start.
//
// stdout carries exactly one line — the ready marker — and nothing else, because the parent
// reads it to know containerd is serving. Everything else goes to stderr, which the parent
// points at the log file.
func Serve(ctx context.Context, stateDir string, stdout, stderr io.Writer) error {
	dir := Dir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	address := addressIn(dir)
	if err := checkSocketPath(address); err != nil {
		return err
	}

	release, err := proclock.Acquire(filepath.Join(dir, lockFile))
	if err != nil {
		if errors.Is(err, proclock.ErrHeld) {
			return errors.New("a boks-managed containerd is already running; 'boks daemon status' shows it")
		}
		return err
	}
	defer release()

	binary, err := FindContainerd()
	if err != nil {
		return describeMissingContainerd(err)
	}

	// containerd's own directories are created here rather than by containerd, and on
	// Windows that is load-bearing: MkdirAllWithACL gives a directory it creates a
	// protected DACL naming only Administrators and SYSTEM, and the unelevated daemon then
	// cannot write inside its own root. It returns early for a directory that already
	// exists, so pre-creating them keeps this user's permissions.
	root, state := filepath.Join(dir, "root"), filepath.Join(dir, "state")
	for _, d := range []string{root, state} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", d, err)
		}
	}

	settings := settingsFor(dir, HasEROFS(), HasExt4())
	config, err := render(settings)
	if err != nil {
		return err
	}
	configPath := filepath.Join(dir, configFile)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", configPath, err)
	}

	for _, note := range Preflight(settings) {
		fmt.Fprintf(stderr, "boks: %s: %s\n%s\n", note.Name, note.Detail, indent(note.Remedy))
	}

	// containerd inherits our environment with the bundle directories prepended to PATH.
	// containerd resolves the runtime shim through its own PATH, and the shim then finds
	// libkrun and the guest kernel by scanning that same PATH — so this line is what makes
	// "start containerd with the shim on its PATH" stop being the user's problem.
	env := append(os.Environ(), "PATH="+daemonPath(os.Getenv("PATH")))

	// Everything after this line in the log is containerd's. See logMarker.
	fmt.Fprintf(stderr, "%s %s --config %s\n", logMarker, binary, configPath)

	cmd := exec.Command(binary, "--config", configPath)
	cmd.Env = env
	// containerd logs to stderr. Both streams go to ours, which is the log file, so that
	// anything it writes on its way down is what `boks daemon logs` shows.
	cmd.Stdout, cmd.Stderr = stderr, stderr
	// Nothing here is meant to be seen. On Windows a console program whose parent has no
	// console is given a new one, window and all: this supervisor is started detached, so
	// without this line `boks daemon start` left a terminal window open on the user's screen
	// for as long as the daemon ran, showing nothing — containerd's output is this log file.
	// A no-op on Unix.
	proclock.NoConsole(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", binary, err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// Nothing below may return while containerd is still running.
	//
	// A supervisor that gave up — because containerd never answered, because the state file
	// could not be written, because its context was cancelled — would otherwise release the
	// lock and exit with a live containerd behind it, holding the socket. Nothing would then
	// be tracking that process: `boks daemon status` would report nothing running, and
	// `boks daemon start` would try to bind a socket somebody else already has. An orphan is
	// the one outcome worse than a failure to start, because it cannot be cleaned up by the
	// command that caused it.
	//
	// The flag is set only where containerd is known to have exited.
	containerdGone := false
	defer func() {
		if containerdGone {
			return
		}
		_ = proclock.Terminate(cmd.Process.Pid)
		select {
		case <-exited:
		case <-time.After(stopGrace):
		}
	}()

	st := State{
		Address:       address,
		Binary:        binary,
		ConfigPath:    configPath,
		LogPath:       filepath.Join(dir, logFile),
		SupervisorPID: os.Getpid(),
		ContainerdPID: cmd.Process.Pid,
		Started:       time.Now(),
	}

	version, err := waitReady(ctx, address, exited)
	if err != nil {
		// waitReady reports a containerd that exited on its own as such, and the
		// deferred cleanup must not then wait stopGrace for a process that is gone.
		containerdGone = errors.Is(err, errContainerdExited)
		return err
	}
	st.Version = version
	if err := writeState(dir, st); err != nil {
		return err
	}

	fmt.Fprintln(stdout, readyMarker)
	if f, ok := stdout.(interface{ Sync() error }); ok {
		_ = f.Sync()
	}

	// From here the supervisor's only job is to outlive nothing: it waits for containerd,
	// forwards a shutdown request to it, and removes the state file on the way out.
	// SIGTERM is what Stop sends and SIGINT is what a hand-run `boks daemon serve` gets from
	// Ctrl-C. Both mean the same thing here: pass it on and wait. syscall.SIGTERM is defined
	// on Windows but never delivered there, which costs nothing and keeps this list one list.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	defer func() { _ = os.Remove(filepath.Join(dir, stateFile)) }()

	select {
	case err := <-exited:
		containerdGone = true
		return containerdExit(err, st.LogPath)
	case <-signals:
	case <-ctx.Done():
	}
	// A shutdown request. The deferred cleanup above does the terminating and the waiting,
	// so there is nothing to repeat here — and having one place that ends containerd is what
	// makes "the supervisor never returns while containerd runs" checkable by reading.
	return nil
}

// errContainerdExited marks the case where the wait ended because containerd died, as opposed
// to timing out or being cancelled. The difference decides whether the caller still has a
// process to clean up.
var errContainerdExited = errors.New("containerd exited before it was ready")

// containerdExit turns containerd's exit into an error naming what it said.
//
// A daemon that exits on its own after having served is a real failure and has to be reported
// as one: the supervisor is about to disappear, and without this the user's next command would
// find no daemon and no explanation.
func containerdExit(err error, logPath string) error {
	if err == nil {
		return nil
	}
	if tail := logTail(logPath, 12); tail != "" {
		return fmt.Errorf("containerd exited: %w. It said:\n\n%s", err, indent(tail))
	}
	return fmt.Errorf("containerd exited: %w", err)
}

// waitReady polls the endpoint until containerd answers, containerd exits, or time runs out.
//
// It polls rather than watching for a socket to appear because the socket appearing is not the
// question: containerd binds its listeners early and finishes initialising some forty plugins
// afterwards, and a client that connects in between gets an error from a service that is not
// there yet. A returned version is proof the whole plugin graph came up.
func waitReady(ctx context.Context, address string, exited <-chan error) (string, error) {
	deadline := time.Now().Add(readyTimeout)
	var last error
	for {
		select {
		case err := <-exited:
			if err != nil {
				return "", fmt.Errorf("%w: %v", errContainerdExited, err)
			}
			return "", errContainerdExited
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		probeCtx, cancel := context.WithTimeout(ctx, readyPoll*10)
		version, err := probe(probeCtx, address)
		cancel()
		if err == nil {
			return version, nil
		}
		last = err
		if time.Now().After(deadline) {
			return "", fmt.Errorf("containerd did not answer %s within %s: %w", address, readyTimeout, last)
		}
		time.Sleep(readyPoll)
	}
}

func writeState(dir string, st State) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, stateFile), data, 0o600)
}

// describeMissingContainerd explains the one failure a fresh host is guaranteed to hit.
func describeMissingContainerd(err error) error {
	if !errors.Is(err, ErrNoContainerd) {
		return err
	}
	return fmt.Errorf("%w.\n\n"+
		"Boks does not yet ship containerd, so it has to find one. It looks in the\n"+
		"directories beside the boks binary and then on PATH. containerd %s or later is\n"+
		"required; no distribution packages one that new, so the usual answer is upstream's\n"+
		"static binaries from https://github.com/containerd/containerd/releases.\n\n"+
		"Point Boks at a particular one with %s.", err, minimumContainerd, binaryEnv)
}

// minimumContainerd is the oldest containerd Boks drives, and it is the version docs and
// packaging already name. It is stated here as well because this is where somebody is told to
// go and install one.
const minimumContainerd = "2.2"

// HasEROFS reports whether containerd will be able to run mkfs.erofs, which decides whether
// the erofs differ may be named in the diff order at all. See diffOrder.
//
// The lookup is LookPath, not exec.LookPath, and that distinction is the whole of a bug found
// on Windows on 2026-08-16: the archive puts mkfs.erofs.exe beside boks.exe and nothing puts
// that directory on the user's PATH, so this returned false, the generated config omitted the
// erofs differ, and every image pull died in the Windows differ with "number of mounts should
// always be 1 for Windows layers". containerd would have found the binary — ContainerdPath
// hands it that directory — but the config had already decided it could not.
func HasEROFS() bool {
	_, err := LookPath(erofsTool)
	return err == nil
}

// erofsTool is the binary the erofs differ shells out to. containerd looks it up on its own
// PATH by this name on every platform, including Windows, where the build in
// packaging/mkfs-erofs-windows supplies mkfs.erofs.exe.
const erofsTool = "mkfs.erofs"

// HasExt4 reports whether containerd will be able to run mkfs.ext4, which is what formats the
// writable layer of every sandbox on a platform where the erofs snapshotter runs in block mode.
// See writableLayerNote for what happens when it cannot.
//
// The lookup is LookPath for the same reason HasEROFS's is: the answer that matters is what
// containerd's PATH contains, not what this shell's does.
func HasExt4() bool {
	_, err := LookPath(ext4Tool)
	return err == nil
}

// ext4Tool is the binary containerd's mount manager shells out to in order to format a
// snapshot's writable layer. It is looked up by this bare name on every platform
// (core/mount/manager/mkfs.go:106,143 in containerd v2.3.3), so on Windows PATHEXT resolves it
// to the mkfs.ext4.exe built by packaging/mkfs-ext4-windows.
const ext4Tool = "mkfs.ext4"

// Namespace is the containerd namespace Boks uses, restated for `boks daemon` so that the
// commands which print a `ctr` line print one that works.
const Namespace = runtimecfg.Namespace
