package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/dagsommer/boks/internal/runtimecfg"
)

// ExecConfig describes an additional process to run inside a running sandbox.
type ExecConfig struct {
	Address string
	Name    string
	Command []string
	// Env are extra environment variables, in KEY=VALUE form, added to the sandbox's.
	Env []string
	// Cwd overrides the working directory; empty keeps the sandbox's own.
	Cwd string
	// User overrides the user the process runs as, as UID or UID:GID. Empty keeps the
	// sandbox's own.
	User string
	// TTY allocates a pseudo-terminal for the process.
	TTY bool

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// client, when set, is an already-connected containerd client in the Boks
	// namespace. It lets Run exec without opening a second connection, and its owner
	// closes it.
	client *client.Client
}

// Exec runs a command inside an existing, running sandbox and returns its exit code.
func Exec(ctx context.Context, cfg ExecConfig) (int, error) {
	if len(cfg.Command) == 0 {
		return 1, fmt.Errorf("a command is required; for example 'boks exec %s sh'", cfg.Name)
	}

	c := cfg.client
	if c == nil {
		ctx = namespaces.WithNamespace(ctx, runtimecfg.Namespace)
		var err error
		if c, err = connect(ctx, cfg.Address); err != nil {
			return 1, err
		}
		defer c.Close()
	}

	container, err := loadContainer(ctx, c, cfg.Name)
	if err != nil {
		return 1, err
	}

	// A stopped sandbox is started rather than refused. Requiring `boks start` first was
	// a step that existed only because Boks made the user aware of it: the sandbox has
	// to be running for the command to run, and there is no other answer the user could
	// give. sbx starts it too. Only a sandbox that cannot be started is an error.
	task, err := ensureRunning(ctx, container)
	if err != nil {
		return 1, err
	}

	return execProcess(ctx, container, task, cfg)
}

// execProcess builds the process spec from the sandbox's own, so an exec'd command sees the
// same environment, working directory and user as the sandbox itself.
func execProcess(ctx context.Context, container client.Container, task client.Task, cfg ExecConfig) (int, error) {
	spec, err := container.Spec(ctx)
	if err != nil {
		return 1, fmt.Errorf("reading the spec of sandbox %q: %w", cfg.Name, err)
	}
	if spec.Process == nil {
		return 1, fmt.Errorf("sandbox %q has no process spec", cfg.Name)
	}

	process := *spec.Process
	process.Args = cfg.Command
	process.Terminal = cfg.TTY
	process.Env = append(slices.Clone(spec.Process.Env), cfg.Env...)
	if cfg.Cwd != "" {
		process.Cwd = cfg.Cwd
	}
	if cfg.User != "" {
		user, err := parseUser(cfg.User)
		if err != nil {
			return 1, err
		}
		process.User = user
	}

	id, err := execID()
	if err != nil {
		return 1, err
	}

	stdin := watchStdin(cfg.Stdin)
	proc, err := task.Exec(ctx, id, &process, ioCreator(cfg.TTY, stdin.input(), cfg.Stdout, cfg.Stderr))
	if err != nil {
		return 1, describeExecError(cfg, err)
	}
	defer func() {
		// The exec record lives in the shim until it is deleted; leaving it behind
		// leaks a process entry for the lifetime of the sandbox.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
		defer cancel()
		_, _ = proc.Delete(cleanupCtx)
	}()

	// Establish the exit channel before starting, so a fast-exiting process cannot
	// finish before anyone is listening.
	statusC, err := proc.Wait(ctx)
	if err != nil {
		return 1, fmt.Errorf("waiting on the command in sandbox %q: %w", cfg.Name, err)
	}
	// Runtimes differ on whether a missing command fails at exec or at start, so both
	// paths get the same explanation.
	if err := proc.Start(ctx); err != nil {
		return 1, describeExecError(cfg, err)
	}
	defer stdin.closeGuestStdin(ctx, proc)()

	restore := attachTerminal(ctx, proc, cfg.TTY, cfg.Stdin)
	forwardSignals(ctx, proc)

	status := <-statusC
	restore()
	code, _, statusErr := status.Result()
	if statusErr != nil {
		return 1, fmt.Errorf("the command in sandbox %q failed: %w", cfg.Name, statusErr)
	}
	return int(code), nil
}

// parseUser turns a -u argument into an OCI user.
//
// Only numeric ids are accepted. Resolving a name would mean reading the guest's
// /etc/passwd through the sandbox's own filesystem, and a name silently resolved against the
// *host's* users would run the process as the wrong one — so the limitation is stated rather
// than approximated.
func parseUser(spec string) (specs.User, error) {
	uidText, gidText, hasGID := strings.Cut(spec, ":")
	uid, err := strconv.ParseUint(uidText, 10, 32)
	if err != nil {
		return specs.User{}, fmt.Errorf("user %q is not a numeric id; Boks cannot resolve names "+
			"inside the guest, so pass UID or UID:GID", spec)
	}
	user := specs.User{UID: uint32(uid), GID: uint32(uid)}
	if hasGID {
		gid, err := strconv.ParseUint(gidText, 10, 32)
		if err != nil {
			return specs.User{}, fmt.Errorf("group %q in user %q is not a numeric id", gidText, spec)
		}
		user.GID = uint32(gid)
	}
	return user, nil
}

// describeExecError names the likely cause: an exec that fails immediately almost always
// means the command is not in the guest image.
func describeExecError(cfg ExecConfig, err error) error {
	return fmt.Errorf("running %q in sandbox %q: %w\n"+
		"If the command is missing from the guest image, install it or run a different one.",
		cfg.Command[0], cfg.Name, err)
}

// execID names the exec'd process within the container. containerd requires it to be
// unique for the lifetime of the task, and several commands may run at once.
func execID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating a process id: %w", err)
	}
	return "boks-exec-" + hex.EncodeToString(buf), nil
}
