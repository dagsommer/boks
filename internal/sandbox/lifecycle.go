package sandbox

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/runtimecfg"
)

// Status values reported for a sandbox. These are containerd's process statuses with one
// addition: a sandbox with no task at all is stopped, not missing.
const (
	StatusRunning = "running"
	StatusStopped = "stopped"
	StatusCreated = "created"
	StatusPaused  = "paused"
)

// Info is everything Boks knows about one sandbox, all of it read back from containerd.
type Info struct {
	Name        string         `json:"name"`
	Agent       string         `json:"agent,omitempty"`
	Status      string         `json:"status"`
	Image       string         `json:"image"`
	Runtime     string         `json:"runtime"`
	Snapshotter string         `json:"snapshotter"`
	Created     time.Time      `json:"created"`
	Ephemeral   bool           `json:"ephemeral"`
	Workspaces  []WorkspaceRef `json:"workspaces"`
	// Filesystem says how the workspace reaches the guest — whether guest writes land
	// on the host's disk or on a clone inside the VM. It is fixed at creation and it is
	// the most consequential fact about a sandbox for the files on your machine, so it
	// is always present rather than omitted when it is the default.
	Filesystem Filesystem `json:"filesystem"`
	Command    []string   `json:"command,omitempty"`
	Env        []string   `json:"env,omitempty"`
	// Annotations are the OCI annotations the sandbox was created with. They are how the
	// runtime is configured — resources and networking — so they are the record of what a
	// sandbox is wired to, and a later command that has to bring it back up reads its
	// network mode from here rather than guessing.
	Annotations map[string]string `json:"annotations,omitempty"`
	// Policy is how the sandbox's network policy was chosen, as recorded when it was
	// created. Nil for a sandbox that named no policy, or one made before Boks recorded it.
	Policy *policy.SandboxPolicy `json:"policy,omitempty"`
	// Ports are the publish specifications the sandbox was created with. They are the
	// request rather than the result: what is actually bound right now lives with the
	// running network stack, and `boks ports` is what shows it.
	Ports    []string `json:"ports,omitempty"`
	Cwd      string   `json:"cwd,omitempty"`
	PID      uint32   `json:"pid,omitempty"`
	ExitCode *uint32  `json:"exit_code,omitempty"`
}

// Workspace returns the sandbox's primary workspace host path, or "" if it has none.
func (i Info) Workspace() string {
	if len(i.Workspaces) == 0 {
		return ""
	}
	return i.Workspaces[0].HostPath
}

// CPUs and MemoryMiB report the resources a sandbox was built with, and whether it recorded
// them at all.
//
// They are read back from the runtime's own annotations rather than from a label of our own,
// because those annotations *are* the request: the VM is sized from them when it is built.
// Reading the same strings the runtime reads is what keeps "what the sandbox has" from
// drifting away from what it was given. A sandbox created before Boks set them — or one
// whose annotation is not a number — reports false, and a caller must then say nothing
// rather than guess.
func (i Info) CPUs() (int, bool) { return intAnnotation(i.Annotations, annotationCPU) }

func (i Info) MemoryMiB() (int, bool) { return intAnnotation(i.Annotations, annotationMemory) }

func intAnnotation(annotations map[string]string, key string) (int, bool) {
	value, ok := annotations[key]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// Create registers a sandbox without starting it, so that a slow image pull can happen
// ahead of the first command.
func Create(ctx context.Context, cfg Config) (Info, error) {
	ctx = namespaces.WithNamespace(ctx, runtimecfg.Namespace)
	c, err := connect(ctx, cfg.Address)
	if err != nil {
		return Info{}, err
	}
	defer c.Close()

	container, err := create(ctx, c, cfg)
	if err != nil {
		return Info{}, err
	}
	return describe(ctx, container)
}

// List returns every sandbox Boks manages, oldest first.
//
// The listing is derived entirely from containerd: containers carry the identity and
// configuration, and the presence and state of a task give the status. Nothing is read from
// a host-side file, so a sandbox removed with `ctr` simply stops being listed.
func List(ctx context.Context, address string) ([]Info, error) {
	ctx = namespaces.WithNamespace(ctx, runtimecfg.Namespace)
	c, err := connect(ctx, address)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	containers, err := c.Containers(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing sandboxes: %w", err)
	}

	infos := make([]Info, 0, len(containers))
	for _, container := range containers {
		labels, err := container.Labels(ctx)
		if err != nil {
			// A container deleted while we were listing is not an error worth
			// failing the whole listing for.
			continue
		}
		if labels[LabelManaged] != "1" {
			continue
		}
		info, err := describe(ctx, container)
		if err != nil {
			continue
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Created.Before(infos[j].Created) })
	return infos, nil
}

// Find returns one sandbox by name, reporting absence as a false rather than an error, for
// callers deciding whether to create it.
func Find(ctx context.Context, address, name string) (Info, bool, error) {
	ctx = namespaces.WithNamespace(ctx, runtimecfg.Namespace)
	c, err := connect(ctx, address)
	if err != nil {
		return Info{}, false, err
	}
	defer c.Close()

	container, err := c.LoadContainer(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return Info{}, false, nil
		}
		return Info{}, false, fmt.Errorf("looking up sandbox %q: %w", name, err)
	}
	info, err := describe(ctx, container)
	if err != nil {
		return Info{}, false, err
	}
	return info, true, nil
}

// Choose decides which sandbox an agent and workspace map to, against the sandboxes that
// exist on this host. It is ChooseName with containerd as the lookup.
func Choose(ctx context.Context, address, agent, hostPath string) (Choice, error) {
	infos, err := List(ctx, address)
	if err != nil {
		return Choice{}, err
	}
	byName := make(map[string]Info, len(infos))
	for _, info := range infos {
		byName[info.Name] = info
	}
	return ChooseName(agent, hostPath, func(name string) (Info, bool) {
		info, ok := byName[name]
		return info, ok
	})
}

// Inspect returns the full detail of one sandbox.
func Inspect(ctx context.Context, address, name string) (Info, error) {
	ctx = namespaces.WithNamespace(ctx, runtimecfg.Namespace)
	c, err := connect(ctx, address)
	if err != nil {
		return Info{}, err
	}
	defer c.Close()

	container, err := loadContainer(ctx, c, name)
	if err != nil {
		return Info{}, err
	}
	return describe(ctx, container)
}

// Start brings a stopped sandbox up. Starting a running sandbox is not an error: callers
// mostly want "make sure it is up".
//
// out receives anything the guest says while the sandbox comes up — today, a clone-mode
// sandbox reporting its first clone and what the host's dirty working tree did not bring
// with it. A nil out discards it.
func Start(ctx context.Context, address, name string, out io.Writer) error {
	ctx = namespaces.WithNamespace(ctx, runtimecfg.Namespace)
	c, err := connect(ctx, address)
	if err != nil {
		return err
	}
	defer c.Close()

	container, err := loadContainer(ctx, c, name)
	if err != nil {
		return err
	}
	_, err = ensureRunning(ctx, container, out)
	return err
}

// Stop shuts a sandbox down without destroying it. The task goes away; the container record
// and its writable snapshot stay, so files written inside the sandbox are still there when
// it starts again.
func Stop(ctx context.Context, address, name string) error {
	ctx = namespaces.WithNamespace(ctx, runtimecfg.Namespace)
	c, err := connect(ctx, address)
	if err != nil {
		return err
	}
	defer c.Close()

	container, err := loadContainer(ctx, c, name)
	if err != nil {
		return err
	}
	return stopContainer(ctx, container)
}

func stopContainer(ctx context.Context, container client.Container) error {
	task, err := container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil // already stopped
		}
		return fmt.Errorf("reading sandbox %q: %w", container.ID(), err)
	}

	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()

	// SIGTERM first so the guest can exit cleanly; SIGKILL is the backstop, because a
	// task left behind holds a VM open.
	if !killAndWait(stopCtx, task, syscall.SIGTERM) {
		killAndWait(stopCtx, task, syscall.SIGKILL)
	}
	if _, err := task.Delete(stopCtx); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("stopping sandbox %q: %w", container.ID(), err)
	}
	return nil
}

// WaitUntilStopped blocks until the sandbox's task is gone — exited, deleted, or the whole
// container removed — and returns nil when it is.
//
// It exists for the network supervisor, whose life is exactly the life of the VM it serves.
// Two things make it more than a status poll. It tolerates the task not existing *yet*,
// because the caller starts before the task does: the VM connects to the link socket while
// it boots, so the network cannot be brought up afterwards. And it gives up if no task
// appears within appear, so that a run which failed between "start the network" and "start
// the sandbox" does not leave a stack holding a socket for nobody.
//
// Polling rather than watching containerd's event stream: the interval is a local gRPC call
// and the cost of missing an event is a stack that lingers for one interval, while the cost
// of a dropped stream is a stack that lingers forever.
func WaitUntilStopped(ctx context.Context, address, name string, appear, interval time.Duration) error {
	return WatchTask(ctx, address, name, appear, interval, nil)
}

// WatchTask is WaitUntilStopped with a signal for the moment the task is first seen running.
//
// The signal costs this loop nothing — it already distinguishes "not yet" from "it ran" — and
// it is the only cheap answer to a question the supervisor cannot otherwise ask: *by when
// should the VM have connected to the link socket?* Measuring that from the moment the socket
// was bound would be measuring an image pull, because the network is started before the
// container is created. started is called at most once, and a nil callback is the ordinary
// case.
func WatchTask(ctx context.Context, address, name string, appear, interval time.Duration, started func()) error {
	ctx = namespaces.WithNamespace(ctx, runtimecfg.Namespace)
	c, err := connect(ctx, address)
	if err != nil {
		return err
	}
	defer c.Close()

	if interval <= 0 {
		interval = time.Second
	}
	appearBy := time.Now().Add(appear)
	seen := false

	for {
		running, err := taskRunning(ctx, c, name)
		if err != nil {
			return err
		}
		switch {
		case running:
			if !seen && started != nil {
				started()
			}
			seen = true
		case seen:
			return nil // it ran, and now it does not
		case time.Now().After(appearBy):
			return fmt.Errorf("sandbox %q never started within %s", name, appear)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// taskRunning reports whether the sandbox currently has a live task. A missing container or
// a missing task both mean "not running" rather than an error: the caller is watching for
// exactly that transition, and a sandbox removed out from under it is one way it happens.
func taskRunning(ctx context.Context, c *client.Client, name string) (bool, error) {
	container, err := c.LoadContainer(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("looking up sandbox %q: %w", name, err)
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading sandbox %q: %w", name, err)
	}
	status, err := task.Status(ctx)
	if err != nil {
		return false, nil
	}
	switch status.Status {
	case client.Running, client.Paused, client.Created:
		return true, nil
	}
	return false, nil
}

// Remove deletes a sandbox and its snapshot. A running sandbox is refused unless force is
// set, so that `boks rm` cannot silently kill work in progress.
func Remove(ctx context.Context, address, name string, force bool) error {
	ctx = namespaces.WithNamespace(ctx, runtimecfg.Namespace)
	c, err := connect(ctx, address)
	if err != nil {
		return err
	}
	defer c.Close()

	container, err := loadContainer(ctx, c, name)
	if err != nil {
		return err
	}

	if task, err := container.Task(ctx, nil); err == nil {
		status, statusErr := task.Status(ctx)
		running := statusErr == nil && (status.Status == client.Running || status.Status == client.Paused)
		if running && !force {
			return fmt.Errorf("sandbox %q is running; stop it with 'boks stop %s' or remove it with 'boks rm -f %s'", name, name, name)
		}
		if err := stopContainer(ctx, container); err != nil {
			return err
		}
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("reading sandbox %q: %w", name, err)
	}

	// Deleting the snapshot is part of deletion: without it every removed sandbox
	// leaves its writable layer on disk.
	deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()
	if err := container.Delete(deleteCtx, client.WithSnapshotCleanup); err != nil {
		return fmt.Errorf("removing sandbox %q: %w", name, err)
	}
	return nil
}

// loadContainer looks up a sandbox by name, distinguishing "no such sandbox" from a
// containerd failure because only one of them is the user's to fix.
func loadContainer(ctx context.Context, c *client.Client, name string) (client.Container, error) {
	container, err := c.LoadContainer(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, fmt.Errorf("no sandbox named %q; run 'boks ls' to see what exists", name)
		}
		return nil, fmt.Errorf("looking up sandbox %q: %w", name, err)
	}
	return container, nil
}

// ensureRunning returns the sandbox's running task, starting one if there is none, and makes
// sure a clone-mode sandbox holds its clone before anything else runs in it.
//
// The clone belongs here rather than in each caller because a sandbox outlives one command:
// `run`, `exec` and `start` all reach a running task through this function, and any of them
// may be the first. See ensureClone, which is idempotent for that reason.
func ensureRunning(ctx context.Context, container client.Container, out io.Writer) (client.Task, error) {
	task, err := startTask(ctx, container)
	if err != nil {
		return nil, err
	}
	if err := ensureClone(ctx, container, task, out); err != nil {
		return nil, err
	}
	return task, nil
}

// startTask returns the sandbox's running task, starting one if there is none.
//
// A task whose process has already exited is deleted first: containerd keeps the record
// until it is reaped, and creating a new task on top of it fails with "already exists".
func startTask(ctx context.Context, container client.Container) (client.Task, error) {
	task, err := container.Task(ctx, nil)
	if err == nil {
		status, statusErr := task.Status(ctx)
		if statusErr == nil {
			switch status.Status {
			case client.Running, client.Paused:
				return task, nil
			case client.Created:
				// Created but never started: a previous start failed after
				// containerd had made the task.
				if err := task.Start(ctx); err != nil {
					return nil, fmt.Errorf("starting sandbox %q: %w", container.ID(), err)
				}
				return task, nil
			}
		}
		if _, err := task.Delete(ctx); err != nil && !errdefs.IsNotFound(err) {
			return nil, fmt.Errorf("clearing the previous process of sandbox %q: %w", container.ID(), err)
		}
	} else if !errdefs.IsNotFound(err) {
		return nil, fmt.Errorf("reading sandbox %q: %w", container.ID(), err)
	}

	// The sandbox's own process is the idle keeper, so its output is of no interest and
	// nothing must stay attached to it: this call outlives the CLI invocation.
	task, err = container.NewTask(ctx, cio.NullIO)
	if err != nil {
		return nil, describeStartError(ctx, container, err)
	}
	if err := task.Start(ctx); err != nil {
		cleanupTask(ctx, task)
		return nil, fmt.Errorf("starting sandbox %q: %w", container.ID(), err)
	}
	return task, nil
}

func describeStartError(ctx context.Context, container client.Container, err error) error {
	info, infoErr := container.Info(ctx)
	if infoErr != nil {
		return fmt.Errorf("starting sandbox %q: %w", container.ID(), err)
	}
	return describeTaskError(Config{
		Name:    container.ID(),
		Image:   info.Image,
		Runtime: info.Runtime.Name,
		Command: keeperCommand,
	}, err)
}

// describe reads one container back into an Info.
func describe(ctx context.Context, container client.Container) (Info, error) {
	info, err := container.Info(ctx)
	if err != nil {
		return Info{}, fmt.Errorf("reading sandbox %q: %w", container.ID(), err)
	}

	out := Info{
		Name:        container.ID(),
		Agent:       info.Labels[LabelAgent],
		Status:      StatusStopped,
		Image:       info.Image,
		Runtime:     info.Runtime.Name,
		Snapshotter: info.Snapshotter,
		Created:     info.CreatedAt,
		Ephemeral:   info.Labels[LabelEphemeral] == "1",
		Workspaces:  decodeWorkspaces(info.Labels),
		Filesystem:  decodeFilesystem(info.Labels),
		Command:     decodeCommand(info.Labels),
		Policy:      decodePolicy(info.Labels),
		Ports:       decodePorts(info.Labels),
	}

	if spec, err := container.Spec(ctx); err == nil {
		out.Annotations = spec.Annotations
		if spec.Process != nil {
			out.Env = spec.Process.Env
			out.Cwd = spec.Process.Cwd
		}
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		// No task means stopped, which is the common case and not a failure.
		return out, nil
	}
	status, err := task.Status(ctx)
	if err != nil {
		return out, nil
	}
	out.Status = string(status.Status)
	if status.Status == client.Running || status.Status == client.Paused {
		out.PID = task.Pid()
	}
	if status.Status == client.Stopped {
		code := status.ExitStatus
		out.ExitCode = &code
	}
	return out, nil
}
