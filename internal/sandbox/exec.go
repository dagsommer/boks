package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"slices"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"

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

	task, err := container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return 1, stoppedError(cfg.Name)
		}
		return 1, fmt.Errorf("reading sandbox %q: %w", cfg.Name, err)
	}
	status, err := task.Status(ctx)
	if err != nil {
		return 1, fmt.Errorf("reading the state of sandbox %q: %w", cfg.Name, err)
	}
	if status.Status != client.Running {
		return 1, stoppedError(cfg.Name)
	}

	return execProcess(ctx, container, task, cfg)
}

func stoppedError(name string) error {
	return fmt.Errorf("sandbox %q is not running; start it with 'boks start %s'", name, name)
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
