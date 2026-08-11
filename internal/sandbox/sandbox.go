// Package sandbox drives containerd to run a command inside an isolated guest.
//
// The isolation boundary comes from the containerd runtime handler: with the nerdbox
// runtime, containerd launches a shim that boots a microVM and runs the process inside it.
// This package owns the lifecycle around that — image, spec, task, IO and cleanup — and
// deliberately contains no VM-specific logic of its own.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/dagsommer/boks/internal/runtimecfg"
	"github.com/dagsommer/boks/internal/workspace"
)

// Resource annotations understood by the nerdbox runtime.
const (
	annotationCPU    = "io.containerd.nerdbox.resources.cpu"
	annotationMemory = "io.containerd.nerdbox.resources.memory"
)

// stopTimeout bounds how long cleanup waits for a killed task to exit before giving up and
// deleting it forcefully. Leaving a task behind leaks a VM, so this never blocks forever.
const stopTimeout = 10 * time.Second

// Config describes one sandbox invocation.
type Config struct {
	// Name identifies the sandbox. Must be unique within the namespace.
	Name string
	// Image is the OCI reference providing the guest root filesystem.
	Image string
	// Command is the argv to execute inside the guest. Empty uses the image default.
	Command []string
	// Workspaces are host directories shared into the guest.
	Workspaces []workspace.Workspace
	// Env are additional environment variables, in KEY=VALUE form.
	Env []string
	// CPUs is the number of vCPUs for the guest.
	CPUs int
	// MemoryMiB is guest memory in MiB.
	MemoryMiB int
	// Runtime is the containerd runtime handler.
	Runtime string
	// Snapshotter is the containerd snapshotter.
	Snapshotter string
	// Address is the containerd socket.
	Address string
	// TTY allocates a pseudo-terminal for the guest process.
	TTY bool

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Run executes the configured command inside a sandbox and returns its exit code.
//
// The sandbox is ephemeral: the container, its task and its snapshot are removed before Run
// returns, whether the command succeeded, failed, or was interrupted.
func Run(ctx context.Context, cfg Config) (exitCode int, err error) {
	ctx = namespaces.WithNamespace(ctx, runtimecfg.Namespace)

	c, err := client.New(cfg.Address)
	if err != nil {
		return 1, fmt.Errorf("connecting to containerd at %s: %w", cfg.Address, err)
	}
	defer c.Close()

	image, err := ensureImage(ctx, c, cfg)
	if err != nil {
		return 1, err
	}

	container, err := createContainer(ctx, c, image, cfg)
	if err != nil {
		return 1, err
	}
	defer func() {
		// Cleanup runs on a context detached from cancellation: on Ctrl-C the caller's
		// context is already done, and deleting through it would leave the container
		// and its snapshot behind. Snapshot cleanup is part of deletion; without it
		// every run leaks a layer.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
		defer cancel()
		if delErr := container.Delete(cleanupCtx, client.WithSnapshotCleanup); delErr != nil && err == nil {
			err = fmt.Errorf("removing sandbox %s: %w", cfg.Name, delErr)
		}
	}()

	return runTask(ctx, container, cfg)
}

// ensureImage returns the image, pulling it if it is not present locally.
func ensureImage(ctx context.Context, c *client.Client, cfg Config) (client.Image, error) {
	image, err := c.GetImage(ctx, cfg.Image)
	if err == nil {
		// A locally present image may still be unpacked for a different snapshotter.
		unpacked, uErr := image.IsUnpacked(ctx, cfg.Snapshotter)
		if uErr != nil {
			return nil, fmt.Errorf("checking image %s: %w", cfg.Image, uErr)
		}
		if unpacked {
			return image, nil
		}
		if err := image.Unpack(ctx, cfg.Snapshotter); err != nil {
			return nil, fmt.Errorf("unpacking image %s for snapshotter %q: %w", cfg.Image, cfg.Snapshotter, err)
		}
		return image, nil
	}
	if !errors.Is(err, errdefs.ErrNotFound) {
		return nil, fmt.Errorf("looking up image %s: %w", cfg.Image, err)
	}

	image, err = c.Pull(ctx, cfg.Image,
		client.WithPullUnpack,
		client.WithPullSnapshotter(cfg.Snapshotter),
	)
	if err != nil {
		return nil, fmt.Errorf("pulling image %s: %w", cfg.Image, err)
	}
	return image, nil
}

// guestPlatform is the platform the OCI spec describes.
//
// The guest is always Linux, even when the host is not: the spec describes the microVM's
// contents, not the machine running Boks. Without this, spec generation on macOS produces a
// Darwin spec with no Linux section, and the image config cannot be applied at all.
func guestPlatform() string {
	return "linux/" + runtime.GOARCH
}

// createContainer builds the OCI spec and registers the container with containerd.
func createContainer(ctx context.Context, c *client.Client, image client.Image, cfg Config) (client.Container, error) {
	specOpts := []oci.SpecOpts{
		// Must come first: it resets the spec to the platform default, discarding
		// anything applied before it.
		oci.WithDefaultSpecForPlatform(guestPlatform()),
		oci.WithImageConfig(image),
		oci.WithAnnotations(resourceAnnotations(cfg)),
	}
	if len(cfg.Command) > 0 {
		specOpts = append(specOpts, oci.WithProcessArgs(cfg.Command...))
	}
	if len(cfg.Env) > 0 {
		specOpts = append(specOpts, oci.WithEnv(cfg.Env))
	}
	if cfg.TTY {
		specOpts = append(specOpts, oci.WithTTY)
	}
	if mounts := workspaceMounts(cfg.Workspaces); len(mounts) > 0 {
		specOpts = append(specOpts, oci.WithMounts(mounts))
		// Start in the primary workspace so relative paths behave as they would on
		// the host.
		specOpts = append(specOpts, oci.WithProcessCwd(cfg.Workspaces[0].Root()))
	}

	container, err := c.NewContainer(ctx, cfg.Name,
		client.WithImage(image),
		client.WithSnapshotter(cfg.Snapshotter),
		client.WithNewSnapshot(cfg.Name, image),
		client.WithRuntime(cfg.Runtime, nil),
		client.WithNewSpec(specOpts...),
	)
	if err != nil {
		return nil, describeCreateError(cfg, err)
	}
	return container, nil
}

// workspaceMounts turns workspaces into OCI bind mounts whose destination is the host path,
// which is what preserves absolute paths inside the guest.
func workspaceMounts(workspaces []workspace.Workspace) []specs.Mount {
	mounts := make([]specs.Mount, 0, len(workspaces))
	for _, ws := range workspaces {
		mounts = append(mounts, specs.Mount{
			Type:        "bind",
			Source:      ws.HostPath,
			Destination: ws.GuestPath,
			Options:     ws.MountOptions(),
		})
	}
	return mounts
}

func resourceAnnotations(cfg Config) map[string]string {
	annotations := map[string]string{}
	if cfg.CPUs > 0 {
		annotations[annotationCPU] = strconv.Itoa(cfg.CPUs)
	}
	if cfg.MemoryMiB > 0 {
		annotations[annotationMemory] = strconv.Itoa(cfg.MemoryMiB)
	}
	return annotations
}

// runTask starts the guest process, streams its IO, and returns its exit code.
func runTask(ctx context.Context, container client.Container, cfg Config) (int, error) {
	creator := ioCreator(cfg)

	task, err := container.NewTask(ctx, creator)
	if err != nil {
		return 1, describeTaskError(cfg, err)
	}

	// Establish the exit channel before starting, so a fast-exiting process cannot
	// finish before anyone is listening.
	statusC, err := task.Wait(ctx)
	if err != nil {
		cleanupTask(ctx, task)
		return 1, fmt.Errorf("waiting on sandbox process: %w", err)
	}

	if err := task.Start(ctx); err != nil {
		cleanupTask(ctx, task)
		return 1, fmt.Errorf("starting sandbox process: %w", err)
	}

	interrupted := forwardSignals(ctx, task)

	status := <-statusC
	code, _, statusErr := status.Result()

	cleanupTask(ctx, task)

	// An interrupted run is not a failure. Report it the way a shell does — 128 plus the
	// signal number — and say nothing, rather than surfacing the transport-level
	// cancellation that tearing the task down produces.
	if sig := interrupted.Load(); sig != 0 {
		return 128 + int(sig), nil
	}

	if statusErr != nil {
		return 1, fmt.Errorf("sandbox process failed: %w", statusErr)
	}
	return int(code), nil
}

func ioCreator(cfg Config) cio.Creator {
	streams := []cio.Opt{cio.WithStreams(cfg.Stdin, cfg.Stdout, cfg.Stderr)}
	if cfg.TTY {
		streams = append(streams, cio.WithTerminal)
	}
	return cio.NewCreator(streams...)
}

// forwardSignals relays interrupt and termination to the guest process so that Ctrl-C
// behaves as it would for a local command, and so cleanup still runs.
//
// The returned value holds the signal number that was delivered, or zero if the run was
// never interrupted, so the caller can report the conventional 128+signal exit status.
func forwardSignals(ctx context.Context, task client.Task) *atomic.Int32 {
	var received atomic.Int32

	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		defer signal.Stop(sigC)
		select {
		case sig := <-sigC:
			if s, ok := sig.(syscall.Signal); ok {
				received.Store(int32(s))
				// Kill on a detached context: the process-wide signal handler has
				// very likely cancelled ctx already, which would make this a no-op
				// and leave the guest running until cleanup forces it.
				killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
				defer cancel()
				_ = task.Kill(killCtx, s)
			}
		case <-ctx.Done():
		}
	}()

	return &received
}

// cleanupTask removes the task, killing it first if it is still running. Errors are
// intentionally swallowed: this runs on the error path too, where the original failure is
// the more useful thing to report.
func cleanupTask(ctx context.Context, task client.Task) {
	// Use a context detached from cancellation so cleanup still runs after Ctrl-C.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()

	if status, err := task.Status(cleanupCtx); err == nil {
		if status.Status == client.Running || status.Status == client.Paused {
			if err := task.Kill(cleanupCtx, syscall.SIGKILL); err == nil {
				if statusC, err := task.Wait(cleanupCtx); err == nil {
					select {
					case <-statusC:
					case <-cleanupCtx.Done():
					}
				}
			}
		}
	}
	_, _ = task.Delete(cleanupCtx)
}

// describeCreateError turns containerd's low-level errors into guidance, since the common
// causes are all host configuration rather than user mistakes.
func describeCreateError(cfg Config, err error) error {
	if errdefs.IsAlreadyExists(err) {
		return fmt.Errorf("a sandbox named %q already exists; remove it or choose another name", cfg.Name)
	}
	return fmt.Errorf("creating sandbox %q: %w", cfg.Name, err)
}

// describeTaskError explains task-creation failures, whose two common causes look alike in
// containerd's output: the runtime shim is missing from the host, or the requested command
// is missing from the image.
//
// These are distinguished by looking for the shim ourselves rather than by matching on
// error text, since "executable file not found" is produced for both.
func describeTaskError(cfg Config, err error) error {
	shim := runtimecfg.ShimBinary(cfg.Runtime)
	if shim != "" {
		if _, lookErr := exec.LookPath(shim); lookErr != nil {
			return fmt.Errorf("starting the %s runtime failed: %w\n\n"+
				"Boks asked containerd for runtime %q, which containerd resolves to the\n"+
				"executable %q on the containerd daemon's PATH. That binary was not found.\n"+
				"Run 'boks doctor' for details.",
				cfg.Runtime, err, cfg.Runtime, shim)
		}
	}
	// Runtimes word this differently ("executable file not found", "stat ...: no such
	// file or directory"), so key on the command name appearing alongside a
	// not-found phrase rather than on any single wording.
	if len(cfg.Command) > 0 && strings.Contains(err.Error(), cfg.Command[0]) {
		msg := err.Error()
		if strings.Contains(msg, "executable file not found") || strings.Contains(msg, "no such file or directory") {
			return fmt.Errorf("the command %q was not found inside the guest image %s.\n"+
				"Check the command exists in that image, or pass a different -image.\n\n"+
				"underlying error: %w", cfg.Command[0], cfg.Image, err)
		}
	}
	return fmt.Errorf("creating sandbox process: %w", err)
}
