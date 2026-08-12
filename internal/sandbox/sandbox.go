// Package sandbox drives containerd to create, run and keep sandboxes.
//
// The isolation boundary comes from the containerd runtime handler: with the nerdbox
// runtime, containerd launches a shim that boots a microVM and runs the process inside it.
// This package owns the lifecycle around that — image, spec, task, IO and cleanup — and
// deliberately contains no VM-specific logic of its own.
//
// A sandbox outlives a single command. `boks run` creates one if the agent and workspace do
// not have one yet, then executes the command as an additional process inside it; the
// sandbox stays until `boks rm`. Which sandbox that is comes from its name, derived in
// identity.go — the name is the identity, not a label on top of one. The container's own process is an idle keeper (see keeperCommand),
// so the sandbox's lifetime does not depend on whatever the user happened to run first.
// Ephemeral sandboxes, which are created and destroyed around one command, remain available
// through Config.Ephemeral.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
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

// keeperCommand is the process a persistent sandbox runs as its own.
//
// containerd can only exec into a container that has a running task, so a sandbox that is
// "up with nothing running" still needs one process. An idle shell is the cheapest thing
// every usable image has, and making it — rather than the user's first command — the
// container process means the sandbox does not disappear when that command exits.
//
// The trap is what makes `boks stop` graceful. `kill -TERM -- -1` signals every process in
// the guest, so a build or a server started with `boks exec` is asked to stop before the
// sandbox goes away; without it those processes learn nothing until containerd's SIGKILL
// arrives. The blast radius is the guest's PID namespace and nothing else, which is the
// only place this command ever runs. Exiting from the trap keeps the stop prompt rather
// than waiting out the ten-second SIGKILL timer.
//
// Sleeping in the background with `wait` is what lets the shell see a signal at all — a
// foreground `sleep` would swallow it. The loop rather than `sleep infinity` is for the
// implementations that do not accept it; busybox does, GNU coreutils does, some do not, and
// an image whose sleep refuses would leave a sandbox that dies the moment it starts.
//
// Docker Sandboxes wraps the same shape in `tini`, so that PID 1 reaps orphans and forwards
// signals. That is worth having in a long-lived sandbox, but it cannot go here: an arbitrary
// image has no init to wrap it with. It belongs to a Boks agent image, whenever there is one.
var keeperCommand = []string{"/bin/sh", "-c",
	`trap 'kill -TERM -- -1 2>/dev/null; wait; exit 0' TERM INT; while :; do sleep 86400 & wait $!; done`}

// Config describes a sandbox to create, and the command to run in it.
type Config struct {
	// Name identifies the sandbox. Must be unique within the namespace.
	Name string
	// Agent is the name of the agent the sandbox runs, recorded so that `boks ls` can
	// show it and `boks run -name x` can find out what it is without being told.
	Agent string
	// Image is the OCI reference providing the guest root filesystem.
	Image string
	// Command is the argv to execute inside the guest. Empty uses the sandbox's
	// recorded default command, which falls back to the image's.
	Command []string
	// Workspaces are host directories shared into the guest.
	Workspaces []workspace.Workspace
	// Mounts are further host directories shared into the guest that are not the user's
	// workspaces — the public half of the interception CA, today. They are kept apart
	// because a workspace is part of the sandbox's identity, recorded in its labels and
	// shown by `boks ls`, and a directory Boks put there for its own plumbing is not.
	Mounts []workspace.Workspace
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
	// Annotations are extra OCI annotations passed to the runtime. The VM runtime is
	// configured this way — networking, for instance — so exposing them lets a
	// capability be tried before Boks grows a first-class flag for it.
	Annotations map[string]string
	// TTY allocates a pseudo-terminal for the guest process.
	TTY bool
	// Ephemeral removes the sandbox, its task and its snapshot when the command exits.
	Ephemeral bool

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Run executes the configured command in the sandbox and returns its exit code.
//
// For a persistent sandbox (the default) it creates the sandbox if it does not exist,
// starts it if it is stopped, and then runs the command as an additional process inside it.
// Running again for the same workspace therefore re-attaches rather than duplicating.
//
// For an ephemeral sandbox the command *is* the container process, and the container, its
// task and its snapshot are removed before Run returns — whether the command succeeded,
// failed, or was interrupted.
func Run(ctx context.Context, cfg Config) (int, error) {
	if cfg.Ephemeral {
		return runEphemeral(ctx, cfg)
	}

	ctx = namespaces.WithNamespace(ctx, runtimecfg.Namespace)
	c, err := connect(ctx, cfg.Address)
	if err != nil {
		return 1, err
	}
	defer c.Close()

	container, err := ensureContainer(ctx, c, cfg)
	if err != nil {
		return 1, err
	}
	if _, err := ensureRunning(ctx, container); err != nil {
		return 1, err
	}

	command := cfg.Command
	if len(command) == 0 {
		labels, err := container.Labels(ctx)
		if err != nil {
			return 1, fmt.Errorf("reading sandbox %q: %w", cfg.Name, err)
		}
		command = decodeCommand(labels)
	}
	if len(command) == 0 {
		return 1, fmt.Errorf("sandbox %q has no default command; pass one after '--'", cfg.Name)
	}

	return Exec(ctx, ExecConfig{
		Address: cfg.Address,
		Name:    cfg.Name,
		Command: command,
		Env:     cfg.Env,
		TTY:     cfg.TTY,
		Stdin:   cfg.Stdin,
		Stdout:  cfg.Stdout,
		Stderr:  cfg.Stderr,
		client:  c,
	})
}

// Up creates the sandbox if it does not exist and makes sure it is running, without
// attaching to anything. It is what `boks run -d` does, and what `boks exec` needs before it
// can run a command.
func Up(ctx context.Context, cfg Config) (Info, error) {
	ctx = namespaces.WithNamespace(ctx, runtimecfg.Namespace)
	c, err := connect(ctx, cfg.Address)
	if err != nil {
		return Info{}, err
	}
	defer c.Close()

	container, err := ensureContainer(ctx, c, cfg)
	if err != nil {
		return Info{}, err
	}
	if _, err := ensureRunning(ctx, container); err != nil {
		return Info{}, err
	}
	return describe(ctx, container)
}

// ensureContainer returns the sandbox's container record, creating it if this is the first
// time the sandbox has been asked for.
func ensureContainer(ctx context.Context, c *client.Client, cfg Config) (client.Container, error) {
	container, err := c.LoadContainer(ctx, cfg.Name)
	if err != nil {
		if !errdefs.IsNotFound(err) {
			return nil, fmt.Errorf("looking up sandbox %q: %w", cfg.Name, err)
		}
		return create(ctx, c, cfg)
	}
	if err := warnWorkspaceMismatch(ctx, container, cfg); err != nil {
		return nil, err
	}
	return container, nil
}

// runEphemeral is the create-run-destroy path: the command is the container process and
// nothing survives it.
func runEphemeral(ctx context.Context, cfg Config) (exitCode int, err error) {
	ctx = namespaces.WithNamespace(ctx, runtimecfg.Namespace)

	c, err := connect(ctx, cfg.Address)
	if err != nil {
		return 1, err
	}
	defer c.Close()

	container, err := create(ctx, c, cfg)
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

// connect opens a containerd client, reporting a missing daemon plainly rather than as a
// dial timeout.
func connect(ctx context.Context, address string) (*client.Client, error) {
	c, err := runtimecfg.Connect(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("connecting to containerd at %s: %w\n"+
			"Run 'boks doctor' to check the host.", address, err)
	}
	return c, nil
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

// create pulls the image if needed and registers the container with containerd.
func create(ctx context.Context, c *client.Client, cfg Config) (client.Container, error) {
	image, err := ensureImage(ctx, c, cfg)
	if err != nil {
		return nil, err
	}

	// The container process differs by lifetime: an ephemeral sandbox exists to run one
	// command, a persistent one has to stay up between commands.
	processArgs := cfg.Command
	if !cfg.Ephemeral {
		processArgs = keeperCommand
	}

	specOpts := []oci.SpecOpts{
		// Must come first: it resets the spec to the platform default, discarding
		// anything applied before it.
		oci.WithDefaultSpecForPlatform(guestPlatform()),
		oci.WithImageConfig(image),
		oci.WithAnnotations(resourceAnnotations(cfg)),
	}
	if len(processArgs) > 0 {
		specOpts = append(specOpts, oci.WithProcessArgs(processArgs...))
	}
	if len(cfg.Env) > 0 {
		specOpts = append(specOpts, oci.WithEnv(cfg.Env))
	}
	if cfg.TTY && cfg.Ephemeral {
		specOpts = append(specOpts, oci.WithTTY)
	}
	if mounts := workspaceMounts(append(slices.Clone(cfg.Workspaces), cfg.Mounts...)); len(mounts) > 0 {
		specOpts = append(specOpts, oci.WithMounts(mounts))
	}
	if len(cfg.Workspaces) > 0 {
		// Start in the primary workspace so relative paths behave as they would on
		// the host. Only a workspace can be that: the plumbing mounts are not places a
		// user asked to be.
		specOpts = append(specOpts, oci.WithProcessCwd(cfg.Workspaces[0].Root()))
	}

	labels, err := containerLabels(ctx, image, cfg)
	if err != nil {
		return nil, err
	}

	container, err := c.NewContainer(ctx, cfg.Name,
		client.WithImage(image),
		client.WithSnapshotter(cfg.Snapshotter),
		client.WithNewSnapshot(cfg.Name, image),
		client.WithRuntime(cfg.Runtime, nil),
		client.WithContainerLabels(labels),
		client.WithNewSpec(specOpts...),
	)
	if err != nil {
		return nil, describeCreateError(cfg, err)
	}
	return container, nil
}

// containerLabels records what containerd's own container record cannot express.
func containerLabels(ctx context.Context, image client.Image, cfg Config) (map[string]string, error) {
	workspacesJSON, err := encodeLabel(workspaceRefs(cfg.Workspaces))
	if err != nil {
		return nil, err
	}

	// The default command is resolved once, at creation, because that is the only point
	// where the image config is at hand. Later `boks run` invocations with no command of
	// their own read it back from here.
	command := cfg.Command
	if len(command) == 0 {
		command = imageCommand(ctx, image)
	}
	commandJSON, err := encodeLabel(command)
	if err != nil {
		return nil, err
	}

	labels := map[string]string{
		LabelManaged:    "1",
		LabelWorkspaces: workspacesJSON,
		LabelCommand:    commandJSON,
	}
	if cfg.Agent != "" {
		labels[LabelAgent] = cfg.Agent
	}
	if cfg.Ephemeral {
		labels[LabelEphemeral] = "1"
	}
	return labels, nil
}

// imageCommand returns the image's own default argv, used when neither `create` nor `run`
// was given a command. An unreadable image config is not fatal: the caller reports the
// missing command instead, which is the more actionable message.
func imageCommand(ctx context.Context, image client.Image) []string {
	spec, err := image.Spec(ctx)
	if err != nil {
		return nil
	}
	return append(append([]string{}, spec.Config.Entrypoint...), spec.Config.Cmd...)
}

// warnWorkspaceMismatch tells the user when they re-attached to a sandbox that does not
// share the directory they are standing in. Mounts are fixed when a sandbox is created, so
// this is a real surprise and not a warning worth suppressing.
func warnWorkspaceMismatch(ctx context.Context, container client.Container, cfg Config) error {
	if len(cfg.Workspaces) == 0 || cfg.Stderr == nil {
		return nil
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return fmt.Errorf("reading sandbox %q: %w", cfg.Name, err)
	}
	existing := decodeWorkspaces(labels)
	for _, ws := range existing {
		if ws.HostPath == cfg.Workspaces[0].HostPath {
			return nil
		}
	}
	if len(existing) == 0 {
		return nil
	}
	fmt.Fprintf(cfg.Stderr,
		"warning: sandbox %q shares %s, not %s. Workspaces are fixed when a sandbox is\n"+
			"         created; remove it or use a different -name to change them.\n",
		cfg.Name, existing[0].HostPath, cfg.Workspaces[0].HostPath)
	return nil
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

// resourceAnnotations builds the annotations handed to the runtime: the resource requests
// derived from flags, plus anything the caller passed through explicitly.
//
// Caller-supplied entries win, so an explicit -annotation can override a computed one.
func resourceAnnotations(cfg Config) map[string]string {
	annotations := map[string]string{}
	if cfg.CPUs > 0 {
		annotations[annotationCPU] = strconv.Itoa(cfg.CPUs)
	}
	if cfg.MemoryMiB > 0 {
		annotations[annotationMemory] = strconv.Itoa(cfg.MemoryMiB)
	}
	maps.Copy(annotations, cfg.Annotations)
	return annotations
}

// runTask starts the guest process, streams its IO, and returns its exit code.
func runTask(ctx context.Context, container client.Container, cfg Config) (int, error) {
	stdin := watchStdin(cfg.Stdin)
	creator := ioCreator(cfg.TTY, stdin.input(), cfg.Stdout, cfg.Stderr)

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
	defer stdin.closeGuestStdin(ctx, task)()

	restore := attachTerminal(ctx, task, cfg.TTY, cfg.Stdin)
	interrupted := forwardSignals(ctx, task)

	status := <-statusC
	restore()
	code, _, statusErr := status.Result()

	cleanupTask(ctx, task)

	// See interruptedExit: Ctrl-C is not a failure, and the status error here would be
	// the cancelled RPC rather than anything the user did.
	if exit := interruptedExit(interrupted); exit != 0 {
		return exit, nil
	}
	if statusErr != nil {
		return 1, fmt.Errorf("sandbox process failed: %w", statusErr)
	}
	return int(code), nil
}

func ioCreator(tty bool, stdin io.Reader, stdout, stderr io.Writer) cio.Creator {
	streams := []cio.Opt{cio.WithStreams(stdin, stdout, stderr)}
	if tty {
		streams = append(streams, cio.WithTerminal)
	}
	return cio.NewCreator(streams...)
}

// forwardSignals relays interrupt and termination to the guest process so that Ctrl-C
// behaves as it would for a local command, and so cleanup still runs.
//
// The returned value holds the signal that was delivered, or zero if the run was never
// interrupted, so the caller can report the conventional 128+signal exit status instead of
// surfacing the transport-level cancellation that tearing the process down produces.
func forwardSignals(ctx context.Context, p client.Process) *atomic.Int32 {
	var received atomic.Int32

	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		defer signal.Stop(sigC)
		select {
		case sig := <-sigC:
			if s, ok := sig.(syscall.Signal); ok {
				received.Store(int32(s))
				// Kill on a context detached from cancellation: the process-wide
				// handler has very likely cancelled ctx already, which would make
				// this a no-op and leave the guest running until cleanup forces it.
				killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
				defer cancel()
				_ = p.Kill(killCtx, s)
			}
		case <-ctx.Done():
		}
	}()

	return &received
}

// interruptedExit reports the exit code for a run cut short by a signal, following the
// shell convention of 128 plus the signal number. It returns 0 if no signal arrived.
//
// An interrupted run is not a failure, so the caller reports this code and says nothing —
// rather than printing the "context canceled" error that tearing the process down produces.
func interruptedExit(received *atomic.Int32) int {
	if sig := received.Load(); sig != 0 {
		return 128 + int(sig)
	}
	return 0
}

// cleanupTask removes the task, killing it first if it is still running. Errors are
// intentionally swallowed: this runs on the error path too, where the original failure is
// the more useful thing to report.
func cleanupTask(ctx context.Context, task client.Task) {
	// Use a context detached from cancellation so cleanup still runs after Ctrl-C.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()

	killAndWait(cleanupCtx, task, syscall.SIGKILL)
	_, _ = task.Delete(cleanupCtx)
}

// killAndWait signals a running task and waits for it to exit, returning whether it did.
func killAndWait(ctx context.Context, task client.Task, sig syscall.Signal) bool {
	status, err := task.Status(ctx)
	if err != nil {
		return false
	}
	if status.Status != client.Running && status.Status != client.Paused {
		return true
	}
	statusC, err := task.Wait(ctx)
	if err != nil {
		return false
	}
	if err := task.Kill(ctx, sig, client.WithKillAll); err != nil {
		return false
	}
	select {
	case <-statusC:
		return true
	case <-ctx.Done():
		return false
	}
}

// describeCreateError turns containerd's low-level errors into guidance, since the common
// causes are all host configuration rather than user mistakes.
func describeCreateError(cfg Config, err error) error {
	if errdefs.IsAlreadyExists(err) {
		return fmt.Errorf("a sandbox named %q already exists; remove it with 'boks rm %s' or choose another name", cfg.Name, cfg.Name)
	}
	return fmt.Errorf("creating sandbox %q: %w", cfg.Name, err)
}

// describeTaskError explains task-creation failures, whose two common causes look alike in
// containerd's output: the runtime shim is missing from the host, or the requested command
// is missing from the image.
//
// Both are reported as "executable file not found", so the guidance is chosen by which name
// the error carries — and offered only when the error actually says an executable was
// missing. An earlier version decided this by looking the shim up on Boks' own PATH, which
// is not the PATH containerd uses: every unrelated failure, a malformed annotation among
// them, was then reported as a missing shim, sending the user after a problem that did not
// exist while the real cause sat in the line above.
func describeTaskError(cfg Config, err error) error {
	msg := err.Error()
	if !mentionsMissingExecutable(msg) {
		return fmt.Errorf("creating sandbox process: %w", err)
	}

	shim := runtimecfg.ShimBinary(cfg.Runtime)
	if shim != "" && strings.Contains(msg, shim) {
		return missingShimError(cfg, shim, err)
	}
	if len(cfg.Command) > 0 && mentionsCommand(msg, cfg.Command[0]) {
		return fmt.Errorf("the command %q was not found inside the guest image %s.\n"+
			"Check the command exists in that image, or pass a different -template.\n\n"+
			"underlying error: %w", cfg.Command[0], cfg.Image, err)
	}
	if shim != "" {
		return missingShimError(cfg, shim, err)
	}
	return fmt.Errorf("creating sandbox process: %w", err)
}

// missingShimError explains that containerd could not run the runtime's shim.
//
// It says where the executable is looked for rather than asserting it is absent: containerd
// searches its daemon's PATH, which Boks cannot see. Whether the shim is on *our* PATH is
// worth mentioning as corroboration, and nothing more.
func missingShimError(cfg Config, shim string, err error) error {
	corroboration := ""
	if _, lookErr := exec.LookPath(shim); lookErr != nil {
		corroboration = "\nIt is not on this command's PATH either, which makes that the likely cause."
	}
	return fmt.Errorf("starting the %s runtime failed: %w\n\n"+
		"Boks asked containerd for runtime %q, which containerd resolves to the executable\n"+
		"%q, looked up on the containerd daemon's PATH.%s\n"+
		"Run 'boks doctor' for details.",
		cfg.Runtime, err, cfg.Runtime, shim, corroboration)
}

// mentionsMissingExecutable reports whether an error is about a program that could not be
// run. Runtimes word this differently, so several phrasings are matched.
func mentionsMissingExecutable(msg string) bool {
	for _, phrase := range []string{
		"executable file not found",
		"no such file or directory",
		"not found in $PATH",
	} {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}

// mentionsCommand reports whether an error names a specific command.
//
// Plain substring matching is not safe here: the common command "sh" occurs inside "shim"
// and inside every shim binary name, so a missing shim would be misread as a missing guest
// command. Runtimes quote the program they could not run, and absolute paths appear
// verbatim; both are specific enough to key on.
func mentionsCommand(msg, command string) bool {
	if command == "" {
		return false
	}
	if strings.Contains(msg, `"`+command+`"`) || strings.Contains(msg, "'"+command+"'") {
		return true
	}
	return strings.HasPrefix(command, "/") && strings.Contains(msg, command)
}
