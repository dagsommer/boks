package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/containerd/containerd/v2/client"

	"github.com/dagsommer/boks/internal/workspace"
)

// Setup is what a kit asks to have done inside the guest: commands to install things, files to
// write, and commands to start at every boot. It is the runtime half of a kit's `setup:` block,
// carried as plain data so this package does not depend on the kit parser.
//
// # Order
//
// Docker's reference puts the steps in this order, and so does Plan: install commands, then
// files, then startup commands. Network rules and environment variables come before all of
// them — the rules are live before the sandbox has a task at all, and the variables are in the
// container spec — so an install command can reach the network it was granted and read what the
// kit set.
type Setup struct {
	Install []InstallStep
	Files   []FileStep
	Startup []StartupStep
	// Home and Workspace are a kit's static files: files/home/ is written under the agent's
	// home, files/workspace/ into the primary workspace.
	Home      []StaticFile
	Workspace []StaticFile
}

// StaticFile is one file from a kit's files/ tree.
type StaticFile struct {
	Path string // relative, slash-separated
	Mode uint32
	Data []byte
}

// homeDir is where files/home/ lands. The agent's home is /home/agent in every image Boks ships
// and in the images kits name, which is what the kit specification says too.
const homeDir = "/home/agent"

// Empty reports whether there is nothing to do.
func (s *Setup) Empty() bool {
	return s == nil || len(s.Install)+len(s.Files)+len(s.Startup)+len(s.Home)+len(s.Workspace) == 0
}

// InstallStep is one `setup.install` entry: a shell string, run as root unless it says otherwise.
type InstallStep struct {
	Command     string
	User        string
	Description string
	// Kit and N name the kit that declared the step and its place in that kit's list (from 1).
	// When Kit is set the step's output is also kept in the guest at
	// /var/log/sbx-kits/<kit>/install-<N>.log, where sbx keeps it.
	//
	// Neither is part of the setup's fingerprint (json:"-"): adding the log must not make an
	// existing sandbox run its installs again.
	Kit string `json:"-"`
	N   int    `json:"-"`
}

// FileStep is one `setup.files` entry.
type FileStep struct {
	Path          string
	Content       string
	Mode          string
	OnlyIfMissing bool
}

// StartupStep is one `setup.startup` entry: an argv, run without a shell, as the agent unless it
// says otherwise.
type StartupStep struct {
	Command     []string
	User        string
	Background  bool
	Description string
}

// setupOp is one thing the guest is asked to do. Plan returns them in execution order, which is
// what makes the order testable without a guest.
type setupOp struct {
	label string
	argv  []string
	user  string // "", "root", "agent", or a uid[:gid]
	stdin string
	// perBoot ops run once per boot of the guest; the rest once per sandbox.
	perBoot bool
	// owned ops get the agent's "uid:gid" appended as their last argument.
	owned bool
}

// writeFileScript writes stdin to $1, creating its directory, unless $2 is "1" and $1 exists,
// and then sets mode $3. Everything arrives as an argument so a path with a quote in it is a
// path and not shell syntax.
const writeFileScript = `
if [ "$2" = 1 ] && [ -e "$1" ]; then exit 0; fi
mkdir -p "$(dirname "$1")" && cat > "$1" && chmod "$3" "$1"
`

// staticFileScript writes stdin to $1/$2 for a kit's files/ tree: it creates the directories
// between, hands each one it created and the file to $5 (the agent), and — when $4 is 1 — leaves
// a file that already exists alone.
const staticFileScript = `
base=$1; rel=$2; mode=$3; keep=$4; owner=$5
dst=$base/$rel
if [ "$keep" = 1 ] && [ -e "$dst" ]; then exit 0; fi
[ -d "$base" ] || { mkdir -p "$base" && chown "$owner" "$base"; } || exit 1
dir=$(dirname "$rel")
if [ "$dir" != . ]; then
	d=$base; IFS=/; set -f
	for part in $dir; do
		d="$d/$part"
		[ -d "$d" ] || { mkdir "$d" && chown "$owner" "$d"; } || exit 1
	done
	unset IFS
fi
cat > "$dst" && chmod "$mode" "$dst" && chown "$owner" "$dst"
`

// backgroundScript starts "$@" detached from the exec that dispatches it, so it keeps running
// after that exec returns and does not hold the CLI open.
const backgroundScript = `nohup "$@" >/dev/null 2>&1 </dev/null &`

// installLogScript runs "$@" with its output kept in the log file $1 as well as passed on, and
// exits with the command's status.
const installLogScript = `log=$1; shift
mkdir -p "$(dirname "$log")" && : > "$log" || exec "$@"
"$@" >"$log" 2>&1; rc=$?
cat "$log"; exit $rc`

// safeLogName keeps a kit name to characters that are safe in a directory name.
func safeLogName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, s)
}

// Plan turns a Setup into the ordered operations that carry it out. workdir is what ${WORKDIR}
// expands to in file content; cloneTarget, when not empty, is the workspace path of a clone-mode
// sandbox, which setup.files may not write at or under.
func (s *Setup) Plan(workdir, cloneTarget string) ([]setupOp, error) {
	if s.Empty() {
		return nil, nil
	}
	var ops []setupOp
	for _, f := range s.Home {
		ops = append(ops, staticOp("home", homeDir, f, false))
	}
	for i, step := range s.Install {
		if strings.TrimSpace(step.Command) == "" {
			return nil, fmt.Errorf("setup.install[%d] has no command", i)
		}
		argv := []string{"/bin/sh", "-c", step.Command}
		if step.Kit != "" && step.N > 0 {
			argv = []string{"/bin/sh", "-c", installLogScript, "sh",
				fmt.Sprintf("/var/log/sbx-kits/%s/install-%d.log", safeLogName(step.Kit), step.N),
				"/bin/sh", "-c", step.Command}
		}
		ops = append(ops, setupOp{
			label: labelOf("install", i, step.Description, step.Command),
			argv:  argv,
			user:  orDefault(step.User, "root"),
		})
	}
	for i, f := range s.Files {
		if !path.IsAbs(f.Path) {
			return nil, fmt.Errorf("setup.files[%d]: path %q is not absolute", i, f.Path)
		}
		clean := path.Clean(f.Path)
		if cloneTarget != "" && (clean == cloneTarget || strings.HasPrefix(clean, cloneTarget+"/")) {
			// The clone is made into that directory and refuses a non-empty one, so a
			// file written first would make the clone fail — or, written after, would
			// be a change the user did not make to their repository.
			return nil, fmt.Errorf("setup.files[%d]: %s is inside the workspace clone at %s; "+
				"put it in files/workspace/ instead", i, f.Path, cloneTarget)
		}
		mode := orDefault(f.Mode, "0644")
		if !validMode(mode) {
			return nil, fmt.Errorf("setup.files[%d]: mode %q is not octal", i, mode)
		}
		only := "0"
		if f.OnlyIfMissing {
			only = "1"
		}
		ops = append(ops, setupOp{
			label: "write " + clean,
			argv:  []string{"/bin/sh", "-c", writeFileScript, "sh", clean, only, mode},
			user:  "root",
			stdin: strings.ReplaceAll(f.Content, "${WORKDIR}", workdir),
		})
	}
	for i, st := range s.Startup {
		if len(st.Command) == 0 {
			return nil, fmt.Errorf("setup.startup[%d] has no command", i)
		}
		argv := st.Command
		if st.Background {
			argv = append([]string{"/bin/sh", "-c", backgroundScript, "sh"}, st.Command...)
		}
		ops = append(ops, setupOp{
			label:   labelOf("startup", i, st.Description, strings.Join(st.Command, " ")),
			argv:    argv,
			user:    orDefault(st.User, "agent"),
			perBoot: true,
		})
	}
	// Last, after the workspace is ready (and, in clone mode, cloned). Never overwriting: in
	// a direct mount these land on the user's own disk, and a kit replacing their .editorconfig
	// on every run would be a kit editing their repository.
	for _, f := range s.Workspace {
		if workdir == "" {
			return nil, fmt.Errorf("the kit has files/workspace/ but the sandbox has no workspace")
		}
		ops = append(ops, staticOp("workspace", workdir, f, true))
	}
	return ops, nil
}

func staticOp(kind, base string, f StaticFile, keep bool) setupOp {
	k := "0"
	if keep {
		k = "1"
	}
	return setupOp{
		label: fmt.Sprintf("files/%s/%s", kind, f.Path),
		argv:  []string{"/bin/sh", "-c", staticFileScript, "sh", base, f.Path, fmt.Sprintf("%04o", f.Mode&0o777), k},
		user:  "root",
		stdin: string(f.Data),
		owned: true,
	}
}

func labelOf(kind string, i int, description, command string) string {
	if description != "" {
		return fmt.Sprintf("%s: %s", kind, description)
	}
	command = strings.Join(strings.Fields(command), " ")
	if len(command) > 60 {
		command = command[:57] + "..."
	}
	return fmt.Sprintf("%s %d: %s", kind, i+1, command)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func validMode(m string) bool {
	if len(m) < 3 || len(m) > 4 {
		return false
	}
	for _, r := range m {
		if r < '0' || r > '7' {
			return false
		}
	}
	return true
}

// fingerprint identifies a setup, so a changed kit is not mistaken for the one already applied.
func (s *Setup) fingerprint() string {
	data, _ := json.Marshal(s)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// Markers. The once-per-sandbox marker lives on the guest's persistent filesystem and is written
// only after every install step and file has succeeded, so a failed install is retried rather
// than remembered as done. The per-boot marker lives under /run, which is memory: it vanishes
// with the guest, which is exactly when startup commands have to run again.
func (s *Setup) installMarker() string { return "/var/lib/boks/kit-setup-" + s.fingerprint() }
func (s *Setup) bootMarker() string    { return "/run/boks-kit-startup-" + s.fingerprint() }

// ensureSetup applies a kit's setup to a running sandbox: install steps and files once, startup
// steps once per boot. It is called with the task the caller has just made sure is running, for
// the same reason ensureClone is, and runs after it: a kit's files must not land in a workspace
// that has not been cloned yet.
func ensureSetup(ctx context.Context, container client.Container, task client.Task, cfg Config) error {
	if cfg.Setup.Empty() {
		return nil
	}
	workdir := ""
	cloneTarget := ""
	if len(cfg.Workspaces) > 0 {
		workdir = cfg.Workspaces[0].Root()
		if cfg.Clone {
			cloneTarget = workdir
		}
	}
	ops, err := cfg.Setup.Plan(workdir, cloneTarget)
	if err != nil {
		return err
	}

	owner, err := sandboxOwner(ctx, container)
	if err != nil {
		return err
	}

	once, boot := cfg.Setup.installMarker(), cfg.Setup.bootMarker()
	haveOnce, err := guestHas(ctx, container, task, once)
	if err != nil {
		return err
	}
	haveBoot, err := guestHas(ctx, container, task, boot)
	if err != nil {
		return err
	}

	for _, op := range ops {
		if (op.perBoot && haveBoot) || (!op.perBoot && haveOnce) {
			continue
		}
		cfg.say("kit: %s", op.label)
		if err := runSetupOp(ctx, container, task, op, owner); err != nil {
			return err
		}
	}

	if !haveOnce {
		if err := touchGuest(ctx, container, task, once); err != nil {
			return err
		}
	}
	if !haveBoot {
		if err := touchGuest(ctx, container, task, boot); err != nil {
			return err
		}
	}
	return nil
}

// setupConfig rebuilds the part of a Config that ensureSetup reads — where the workspace is and
// whether it is a clone — from what the sandbox recorded, for a command that did not create it
// and so has no flags to build one from.
func setupConfig(labels map[string]string, name string, setup *Setup, progress io.Writer) Config {
	cfg := Config{Name: name, Setup: setup, Progress: progress}
	for _, ref := range decodeWorkspaces(labels) {
		guest := ref.GuestPath
		if guest == "" {
			guest = ref.HostPath
		}
		cfg.Workspaces = append(cfg.Workspaces, workspace.Workspace{
			HostPath: ref.HostPath, GuestPath: guest, Mode: workspace.Mode(ref.Mode),
		})
	}
	cfg.Clone = decodeFilesystem(labels).IsClone()
	return cfg
}

// ensureRecordedSetup applies a kit's setup to a sandbox a command with no --kit has just
// brought up. It is ensureSetup for a caller that reloaded the kit from the sandbox's record:
// the markers make it what ensureSetup always is — install and files once, startup commands once
// per boot.
func ensureRecordedSetup(ctx context.Context, container client.Container, task client.Task, setup *Setup, progress io.Writer) error {
	if setup.Empty() {
		return nil
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return fmt.Errorf("reading sandbox %q: %w", container.ID(), err)
	}
	return ensureSetup(ctx, container, task, setupConfig(labels, container.ID(), setup, progress))
}

// resolveUser maps the names kits use to something exec accepts. Kits write "0", "1000", "root"
// and "agent" for the same two users (see kit.InstallCommand.User); only numbers reach exec, and
// "agent" is whoever the sandbox's own processes run as, which is the host's uid when Boks has
// matched it to the workspace's owner.
func resolveUser(user, owner string) string {
	switch user {
	case "root", "0":
		return "0:0"
	case "agent", "1000", "":
		return owner
	}
	return user
}

func runSetupOp(ctx context.Context, container client.Container, task client.Task, op setupOp, owner string) error {
	var out bytes.Buffer
	argv := op.argv
	if op.owned {
		argv = append(append([]string{}, argv...), owner)
	}
	ec := ExecConfig{
		Name:    container.ID(),
		Command: argv,
		User:    resolveUser(op.user, owner),
		Stdout:  &out,
		Stderr:  &out,
	}
	if op.stdin != "" {
		ec.Stdin = strings.NewReader(op.stdin)
	}
	code, err := execProcess(ctx, container, task, ec)
	if err != nil {
		return fmt.Errorf("kit setup, %s: %w", op.label, err)
	}
	if code != 0 {
		return fmt.Errorf("kit setup, %s, failed (exit %d):\n%s", op.label, code, tail(out.String(), 40))
	}
	return nil
}

// guestHas reports whether a path exists in the guest.
func guestHas(ctx context.Context, container client.Container, task client.Task, p string) (bool, error) {
	code, err := execProcess(ctx, container, task, ExecConfig{
		Name:    container.ID(),
		Command: []string{"/bin/sh", "-c", `test -e "$1"`, "sh", p},
		User:    "0:0",
		Stdout:  io.Discard,
		Stderr:  io.Discard,
	})
	if err != nil {
		return false, fmt.Errorf("checking the guest for kit setup: %w", err)
	}
	return code == 0, nil
}

func touchGuest(ctx context.Context, container client.Container, task client.Task, p string) error {
	var out bytes.Buffer
	code, err := execProcess(ctx, container, task, ExecConfig{
		Name:    container.ID(),
		Command: []string{"/bin/sh", "-c", `mkdir -p "$(dirname "$1")" && : > "$1"`, "sh", p},
		User:    "0:0",
		Stdout:  &out,
		Stderr:  &out,
	})
	if err != nil {
		return fmt.Errorf("recording kit setup in the guest: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("recording kit setup in the guest failed (exit %d): %s", code, strings.TrimSpace(out.String()))
	}
	return nil
}

// tail returns the last n lines of s, so a long install log shows its ending, where the error is.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append([]string{fmt.Sprintf("... (%d earlier lines)", len(lines)-n)}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}
