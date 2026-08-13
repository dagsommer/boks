package sandbox_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/agent"
	"github.com/dagsommer/boks/internal/sandbox"
	"github.com/dagsommer/boks/internal/workspace"
)

// These drive a real containerd, like the rest of the integration suite, and additionally
// need git — on the host to build the fixture repository, and in the guest image to make the
// clone. The default image is therefore the Boks base image rather than the suite's alpine,
// and BOKS_TEST_GIT_IMAGE overrides it.
//
//	BOKS_INTEGRATION=1 go test ./internal/sandbox/ -run IntegrationClone -v
func cloneConfig(t *testing.T) sandbox.Config {
	t.Helper()
	cfg := testConfig(t) // skips unless BOKS_INTEGRATION=1
	cfg.Image = envOr("BOKS_TEST_GIT_IMAGE", agent.Image("base"))
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("clone-mode tests build a fixture repository and need git on the host")
	}
	return cfg
}

// hostRepo builds a small git repository to be the host side of a clone-mode sandbox. It
// deliberately leaves one modified and one untracked file behind: a clone carries committed
// history only, and that is a property worth having a fixture for.
func hostRepo(t *testing.T) workspace.Workspace {
	t.Helper()
	dir := t.TempDir()

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		// A pristine configuration, so the developer's own git settings — hooks
		// directories, signing, default branch — cannot change what the fixture is.
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=boks", "GIT_AUTHOR_EMAIL=boks@test",
			"GIT_COMMITTER_NAME=boks", "GIT_COMMITTER_EMAIL=boks@test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	writeFile(t, filepath.Join(dir, "Makefile"), "all:\n\t@echo the host's own Makefile\n")
	writeFile(t, filepath.Join(dir, "README.md"), "committed\n")
	git("init", "-q", "-b", "main")
	git("add", "-A")
	git("commit", "-qm", "initial commit")

	writeFile(t, filepath.Join(dir, "README.md"), "committed, then edited on the host\n")
	writeFile(t, filepath.Join(dir, "untracked.txt"), "never committed\n")

	ws, err := workspace.Parse(dir)
	if err != nil {
		t.Fatalf("workspace.Parse: %v", err)
	}
	return ws
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// treeDigest is a content hash of a directory tree: every path, its mode, and the bytes of
// every regular file, in a fixed order.
//
// It is what "the host tree is unchanged" is asserted against. Times are deliberately not
// hashed — reading a file can move its atime, and that is not a change to anyone's work —
// but names, modes, symlink targets and contents all are, which is the whole of what a guest
// could have done to the tree.
func treeDigest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%o\x00", filepath.ToSlash(rel), info.Mode())
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s\x00", target)
		case info.Mode().IsRegular():
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(h, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// newCloneSandbox creates a clone-mode sandbox for one test and removes it afterwards.
func newCloneSandbox(t *testing.T, ws workspace.Workspace) sandbox.Config {
	t.Helper()
	cfg := cloneConfig(t)
	cfg.Name = testName(t)
	cfg.Clone = true
	cfg.Workspaces = []workspace.Workspace{ws}
	cfg.Command = []string{"/bin/sh"}
	cfg.Stdout, cfg.Stderr = os.Stdout, os.Stderr

	t.Cleanup(func() {
		if err := sandbox.Remove(context.Background(), cfg.Address, cfg.Name, true); err != nil &&
			!strings.Contains(err.Error(), "no sandbox named") {
			t.Errorf("cleanup: %v", err)
		}
	})
	if _, err := sandbox.Up(context.Background(), cfg); err != nil {
		t.Fatalf("bringing up a clone-mode sandbox: %v", err)
	}
	return cfg
}

// The point of clone mode, asserted directly: a guest that rewrites, deletes and adds files,
// and commits all of it, leaves the host tree byte-identical.
func TestIntegrationCloneLeavesTheHostTreeUnchanged(t *testing.T) {
	ws := hostRepo(t)
	before := treeDigest(t, ws.HostPath)

	cfg := newCloneSandbox(t, ws)
	code, _, err := execIn(t, cfg, "/bin/sh", "-c", `set -e
cd `+ws.GuestPath+`
echo "the guest's Makefile" > Makefile
rm -f README.md
echo "written by the guest" > NEW.txt
mkdir -p sub && echo nested > sub/deep.txt
git -c user.email=g@t -c user.name=g commit -qam "the guest's commit" || true
git -c user.email=g@t -c user.name=g add -A
git -c user.email=g@t -c user.name=g commit -qm "everything the guest did"
git log --oneline`)
	if err != nil || code != 0 {
		t.Fatalf("the guest could not modify its clone: code=%d err=%v", code, err)
	}

	if after := treeDigest(t, ws.HostPath); after != before {
		t.Errorf("the host tree changed under a clone-mode sandbox:\nbefore %s\nafter  %s", before, after)
	}
	// Belt and braces, because a digest mismatch says only that something moved.
	for _, name := range []string{"NEW.txt", "sub"} {
		if _, err := os.Stat(filepath.Join(ws.HostPath, name)); err == nil {
			t.Errorf("the guest created %s on the host", name)
		}
	}
	if got := readFile(t, filepath.Join(ws.HostPath, "Makefile")); !strings.Contains(got, "the host's own Makefile") {
		t.Errorf("the host Makefile was rewritten to %q — this is the attack clone mode exists to stop", got)
	}
	if _, err := os.Stat(filepath.Join(ws.HostPath, "README.md")); err != nil {
		t.Errorf("the guest deleted README.md on the host: %v", err)
	}
}

// The host repository is the one thing in a clone-mode sandbox that is still the user's
// disk, so it is shared read-only and the guest must not get through it.
func TestIntegrationCloneSourceIsUnwritable(t *testing.T) {
	ws := hostRepo(t)
	cfg := newCloneSandbox(t, ws)

	_, out, err := execIn(t, cfg, "/bin/sh", "-c",
		"touch "+workspace.SourcePath+"/pwned 2>&1 || echo REFUSED; "+
			"rm -f "+workspace.SourcePath+"/Makefile 2>&1 || echo REFUSED; "+
			"echo x > "+workspace.SourcePath+"/.git/HEAD 2>&1 || echo REFUSED")
	if err != nil {
		t.Fatalf("execIn: %v", err)
	}
	if strings.Count(out, "REFUSED") != 3 {
		t.Errorf("writes to %s were not all refused: %q", workspace.SourcePath, out)
	}
	if _, err := os.Stat(filepath.Join(ws.HostPath, "pwned")); err == nil {
		t.Error("the guest created a file in the host repository through the source share")
	}
	if _, err := os.Stat(filepath.Join(ws.HostPath, "Makefile")); err != nil {
		t.Errorf("the guest deleted the host Makefile through the source share: %v", err)
	}
}

// --no-hardlinks, verified rather than assumed. A local git clone hardlinks object files
// when it can; the clone's objects would then be the *same inodes* as the host repository's,
// and a guest that is root could chmod and overwrite them — a write to the host's disk
// through the mode that promises there are none.
func TestIntegrationCloneObjectsAreNotHardlinkedToTheHost(t *testing.T) {
	ws := hostRepo(t)
	cfg := newCloneSandbox(t, ws)

	_, out, err := execIn(t, cfg, "/bin/sh", "-c",
		"find "+ws.GuestPath+"/.git/objects -type f -exec stat -c '%h' {} + | sort -u")
	if err != nil {
		t.Fatalf("execIn: %v", err)
	}
	for _, line := range strings.Fields(out) {
		if line != "1" {
			t.Errorf("a cloned git object has link count %s, want 1: the clone shares inodes "+
				"with the host repository", line)
		}
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("found no objects in the clone; the clone did not happen")
	}
}

// A clone carries committed history. Whatever is dirty or untracked in the host tree is not
// in it, and the sandbox says so when it is created rather than leaving it to be discovered.
func TestIntegrationCloneCarriesCommittedHistoryOnly(t *testing.T) {
	ws := hostRepo(t)
	cfg := newCloneSandbox(t, ws)

	_, out, err := execIn(t, cfg, "/bin/sh", "-c",
		"cat "+ws.GuestPath+"/README.md; test -e "+ws.GuestPath+"/untracked.txt && echo PRESENT || echo absent")
	if err != nil {
		t.Fatalf("execIn: %v", err)
	}
	if strings.Contains(out, "edited on the host") {
		t.Errorf("the clone carries the host's uncommitted edit: %q", out)
	}
	if !strings.Contains(out, "absent") {
		t.Errorf("the clone carries the host's untracked file: %q", out)
	}
	// The committed content is what should be there.
	if !strings.Contains(out, "committed") {
		t.Errorf("the clone is missing the committed README: %q", out)
	}
}

// The clone's origin is the read-only source, which is what lets a guest pick up commits the
// host makes later — and what stops it pushing anything back.
func TestIntegrationCloneCanFetchButNotPushToTheHost(t *testing.T) {
	ws := hostRepo(t)
	cfg := newCloneSandbox(t, ws)

	_, out, err := execIn(t, cfg, "/bin/sh", "-c", "cd "+ws.GuestPath+"; "+
		"git remote get-url origin; "+
		"git fetch origin >/dev/null 2>&1 && echo FETCH-OK || echo fetch-failed; "+
		"git push origin HEAD:refs/heads/pushed >/dev/null 2>&1 && echo PUSH-OK || echo push-refused")
	if err != nil {
		t.Fatalf("execIn: %v", err)
	}
	if !strings.Contains(out, workspace.SourcePath) {
		t.Errorf("origin = %q, want the read-only source share", out)
	}
	if !strings.Contains(out, "FETCH-OK") {
		t.Errorf("the guest cannot fetch from the host repository: %q", out)
	}
	if !strings.Contains(out, "push-refused") {
		t.Errorf("the guest pushed into the host repository: %q", out)
	}
	if _, err := os.Stat(filepath.Join(ws.HostPath, ".git", "refs", "heads", "pushed")); err == nil {
		t.Error("a guest push created a branch in the host repository")
	}
}

// The mode is fixed when a sandbox is created, because it lives in the OCI mounts. A later
// run that does not ask for clone mode gets it anyway, and a later run that does ask cannot
// turn it off — in both directions the sandbox is what it was made as.
func TestIntegrationCloneModeIsFixedAtCreation(t *testing.T) {
	ws := hostRepo(t)
	before := treeDigest(t, ws.HostPath)
	cfg := newCloneSandbox(t, ws)
	ctx := context.Background()

	info, exists, err := sandbox.Find(ctx, cfg.Address, cfg.Name)
	if err != nil || !exists {
		t.Fatalf("Find: %v (exists=%v)", err, exists)
	}
	if !info.Filesystem.IsClone() {
		t.Fatalf("Filesystem = %+v, want clone mode recorded", info.Filesystem)
	}
	if info.Filesystem.Source != workspace.SourcePath || info.Filesystem.Clone != ws.GuestPath {
		t.Errorf("Filesystem = %+v, want source %q and clone %q",
			info.Filesystem, workspace.SourcePath, ws.GuestPath)
	}

	// A re-attach that says nothing about the mode.
	plain := cfg
	plain.Clone = false
	plain.Command = []string{"/bin/sh", "-c", "echo re-attached > reattach.txt"}
	if code, err := sandbox.Run(ctx, plain); err != nil || code != 0 {
		t.Fatalf("re-attaching: code=%d err=%v", code, err)
	}
	if _, err := os.Stat(filepath.Join(ws.HostPath, "reattach.txt")); err == nil {
		t.Error("a re-attach without --clone wrote to the host: the mode was not fixed")
	}

	// And across a stop and start, which is where a mode read from the wrong place
	// would quietly widen.
	if err := sandbox.Stop(ctx, cfg.Address, cfg.Name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := sandbox.Start(ctx, cfg.Address, cfg.Name, os.Stderr); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, _, err = sandbox.Find(ctx, cfg.Address, cfg.Name)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !info.Filesystem.IsClone() {
		t.Errorf("Filesystem = %+v after a restart, want clone mode", info.Filesystem)
	}
	if after := treeDigest(t, ws.HostPath); after != before {
		t.Errorf("the host tree changed:\nbefore %s\nafter  %s", before, after)
	}
}

// The clone is made once. A second start must not re-clone — that would throw away
// everything the agent had done.
func TestIntegrationCloneSurvivesAStopAndStart(t *testing.T) {
	ws := hostRepo(t)
	cfg := newCloneSandbox(t, ws)
	ctx := context.Background()

	if code, _, err := execIn(t, cfg, "/bin/sh", "-c",
		"echo work-in-progress > "+ws.GuestPath+"/wip.txt"); err != nil || code != 0 {
		t.Fatalf("writing in the clone: code=%d err=%v", code, err)
	}
	if err := sandbox.Stop(ctx, cfg.Address, cfg.Name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := sandbox.Start(ctx, cfg.Address, cfg.Name, os.Stderr); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, out, err := execIn(t, cfg, "/bin/sh", "-c", "cat "+ws.GuestPath+"/wip.txt")
	if err != nil {
		t.Fatalf("execIn: %v", err)
	}
	if !strings.Contains(out, "work-in-progress") {
		t.Errorf("the clone was remade across a restart, losing the guest's work: %q", out)
	}
}

// Clone mode is only usable if work can leave the sandbox, and a sandbox has no inbound
// network to serve a git remote from. This is the whole path, end to end: commit in the
// guest, bundle inside it, copy the bundle out over the same channel `boks cp` uses, and
// fetch it into a host repository that has never seen those commits.
func TestIntegrationCloneWorkLeavesAsABundle(t *testing.T) {
	ws := hostRepo(t)
	before := treeDigest(t, ws.HostPath)
	cfg := newCloneSandbox(t, ws)

	code, _, err := execIn(t, cfg, "/bin/sh", "-c", `set -e
cd `+ws.GuestPath+`
echo "written in the sandbox" > FEATURE.md
git -c user.email=g@t -c user.name=g add -A
git -c user.email=g@t -c user.name=g commit -qm "the guest's work"
git bundle create /tmp/work.bundle --all HEAD`)
	if err != nil || code != 0 {
		t.Fatalf("bundling in the guest: code=%d err=%v", code, err)
	}

	hostBundle := filepath.Join(t.TempDir(), "work.bundle")
	if err := sandbox.Copy(context.Background(), sandbox.CopyConfig{
		Address:   cfg.Address,
		Name:      cfg.Name,
		GuestPath: "/tmp/work.bundle",
		HostPath:  hostBundle,
	}); err != nil {
		t.Fatalf("copying the bundle out: %v", err)
	}

	// The host repository must not have the commit before the fetch, or the test would
	// prove nothing.
	if strings.Contains(hostGit(t, ws.HostPath, "log", "--all", "--oneline"), "the guest's work") {
		t.Fatal("the host already has the guest's commit; the fixture is wrong")
	}
	hostGit(t, ws.HostPath, "fetch", hostBundle, "refs/heads/*:refs/sandboxes/test/*")
	refs := hostGit(t, ws.HostPath, "log", "--oneline", "--all", "--glob=refs/sandboxes/test/*")
	if !strings.Contains(refs, "the guest's work") {
		t.Errorf("the fetched refs do not carry the guest's commit: %q", refs)
	}
	if got := hostGit(t, ws.HostPath, "show", "refs/sandboxes/test/main:FEATURE.md"); !strings.Contains(got, "written in the sandbox") {
		t.Errorf("the fetched commit does not carry the guest's file: %q", got)
	}

	// A fetch writes refs and objects under .git, which is exactly what it is for. The
	// working tree — the user's actual files — must be untouched by any of this.
	for _, name := range []string{"Makefile", "README.md", "untracked.txt"} {
		if _, err := os.Stat(filepath.Join(ws.HostPath, name)); err != nil {
			t.Errorf("the bundle round trip disturbed %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(ws.HostPath, "FEATURE.md")); err == nil {
		t.Error("fetching a bundle checked the guest's file out into the host working tree")
	}
	if before == treeDigest(t, ws.HostPath) {
		t.Log("note: the digest is unchanged, so the fetch wrote nothing under .git either")
	}
}

// hostGit runs git against the fixture repository with a pristine configuration.
func hostGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// Direct mode has to keep doing what it did: the write-back test in integration_test.go
// covers the shared workspace, and this covers the record that now describes it.
func TestIntegrationDirectModeIsRecordedAsDirect(t *testing.T) {
	ws := hostRepo(t)
	cfg := cloneConfig(t)
	cfg.Name = testName(t)
	cfg.Workspaces = []workspace.Workspace{ws}
	cfg.Command = []string{"/bin/sh"}
	cfg.Stdout, cfg.Stderr = os.Stdout, os.Stderr
	t.Cleanup(func() {
		_ = sandbox.Remove(context.Background(), cfg.Address, cfg.Name, true)
	})

	info, err := sandbox.Create(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.Filesystem.IsClone() {
		t.Errorf("Filesystem = %+v, want direct mode", info.Filesystem)
	}
	if info.Filesystem.Mode != sandbox.FilesystemDirect {
		t.Errorf("Mode = %q, want %q", info.Filesystem.Mode, sandbox.FilesystemDirect)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(raw)
}
