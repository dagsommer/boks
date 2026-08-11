package sandbox

import (
	"context"
	"fmt"
	"sort"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"

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
	Status      string         `json:"status"`
	Image       string         `json:"image"`
	Runtime     string         `json:"runtime"`
	Snapshotter string         `json:"snapshotter"`
	Created     time.Time      `json:"created"`
	Ephemeral   bool           `json:"ephemeral"`
	Workspaces  []WorkspaceRef `json:"workspaces"`
	Command     []string       `json:"command,omitempty"`
	Env         []string       `json:"env,omitempty"`
	Cwd         string         `json:"cwd,omitempty"`
	PID         uint32         `json:"pid,omitempty"`
	ExitCode    *uint32        `json:"exit_code,omitempty"`
}

// Workspace returns the sandbox's primary workspace host path, or "" if it has none.
func (i Info) Workspace() string {
	if len(i.Workspaces) == 0 {
		return ""
	}
	return i.Workspaces[0].HostPath
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
func Start(ctx context.Context, address, name string) error {
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
	_, err = ensureRunning(ctx, container)
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

// ensureRunning returns the sandbox's running task, starting one if there is none.
//
// A task whose process has already exited is deleted first: containerd keeps the record
// until it is reaped, and creating a new task on top of it fails with "already exists".
func ensureRunning(ctx context.Context, container client.Container) (client.Task, error) {
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
		Status:      StatusStopped,
		Image:       info.Image,
		Runtime:     info.Runtime.Name,
		Snapshotter: info.Snapshotter,
		Created:     info.CreatedAt,
		Ephemeral:   info.Labels[LabelEphemeral] == "1",
		Workspaces:  decodeWorkspaces(info.Labels),
		Command:     decodeCommand(info.Labels),
	}

	if spec, err := container.Spec(ctx); err == nil && spec.Process != nil {
		out.Env = spec.Process.Env
		out.Cwd = spec.Process.Cwd
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
