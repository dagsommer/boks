package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/ca"
	"github.com/dagsommer/boks/internal/daemon"
	"github.com/dagsommer/boks/internal/enforce"
	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/proclock"
	"github.com/dagsommer/boks/internal/secret"
)

// purgeState points the CLI at a state directory of the test's own and returns it, so that
// nothing a purge test runs can reach the developer's real containerd root.
//
// This matters more here than anywhere else in the suite: every one of these tests ends in a
// command whose job is to delete the state directory.
func purgeState(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "boks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Resolved, because purge.Root resolves symlinks before it reports anything (see its
	// doc comment) and these tests compare printed paths against this one. The two forms
	// differ on two of the three platforms: macOS temp dirs live under /var, a symlink to
	// /private/var, and on Windows the runner's TEMP is the 8.3 short name
	// C:\Users\RUNNER~1\... which resolves to C:\Users\runneradmin\....
	//
	// That is what made TestPurgeReclaimsDiskAndKeepsIdentity fail on Windows while the
	// command was doing exactly the right thing: it printed the resolved path, the test
	// wanted the short one, and the log showed two paths a human reads as identical.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(policy.StateDirEnv, resolved)
	return resolved
}

// writeState creates one file's worth of a named piece of state, so a plan has something to
// find without a containerd being involved.
func writeState(t *testing.T, root, name string, size int) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The whole point of the command: the disk comes back, and the four things a user would be
// upset to lose without being asked do not go with it.
func TestPurgeReclaimsDiskAndKeepsIdentity(t *testing.T) {
	root := purgeState(t)
	writeState(t, root, filepath.Join("containerd", "root", "layer"), 4096)
	writeState(t, root, filepath.Join("ca", "ca-key.pem"), 64)
	writeState(t, root, "secrets.json", 32)
	writeState(t, root, filepath.Join("policy", "policy.json"), 32)
	writeState(t, root, "policy-log.jsonl", 32)

	out, _, err := runCLI(t, "y\n", "purge")
	if err != nil {
		t.Fatalf("boks purge: %v\n%s", err, out)
	}
	// The removal line specifically, not just the figure: the plan printed above already
	// carries "4.0 KiB", so an assertion on the number alone would hold even if the
	// command never reported what it actually did.
	if !strings.Contains(out, "removed 4.0 KiB from "+root) {
		t.Errorf("the output never says how much it freed and from where:\n%s", out)
	}
	if _, err := os.Lstat(filepath.Join(root, "containerd")); !os.IsNotExist(err) {
		t.Errorf("containerd's root survived: %v", err)
	}
	for _, kept := range []string{"ca", "secrets.json", "policy", "policy-log.jsonl"} {
		if _, err := os.Lstat(filepath.Join(root, kept)); err != nil {
			t.Errorf("%s was removed by a purge that promised to keep it: %v", kept, err)
		}
	}
}

// --dry-run is the command a cautious person runs first, so it has to be inert.
func TestPurgeDryRunRemovesNothing(t *testing.T) {
	root := purgeState(t)
	writeState(t, root, filepath.Join("containerd", "root", "layer"), 4096)
	writeState(t, root, filepath.Join("ca", "ca-key.pem"), 64)

	// "y" on stdin as well, so that a dry run that fell through to the prompt would be
	// answered rather than saved by an empty reader.
	out, errOut, err := runCLI(t, "y\n", "purge", "--all", "--dry-run")
	if err != nil {
		t.Fatalf("boks purge --dry-run: %v\n%s", err, errOut)
	}
	if !strings.Contains(errOut, "nothing was removed") {
		t.Errorf("a dry run does not say it removed nothing:\n%s", errOut)
	}
	if !strings.Contains(out, "will be removed") {
		t.Errorf("a dry run does not show a plan:\n%s", out)
	}
	for _, name := range []string{"containerd", "ca"} {
		if _, err := os.Lstat(filepath.Join(root, name)); err != nil {
			t.Errorf("--dry-run removed %s: %v", name, err)
		}
	}
}

// Nothing goes without an answer, and the default answer is no.
func TestPurgeRefusesWithoutConfirmation(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
		args  []string
		want  string
	}{
		{"empty answer", "\n", []string{"purge"}, "not purging"},
		{"an answer that is not yes", "n\n", []string{"purge"}, "not purging"},
		{"nothing on stdin at all", "", []string{"purge"}, "not purging"},
		// --all takes the CA's private key and every stored credential. A mis-keyed
		// "y" must not be able to do that, so the answer has to be a word.
		{"y is not enough for --all", "y\n", []string{"purge", "--all"}, "was not the word 'purge'"},
		{"the word itself, mistyped", "purged\n", []string{"purge", "--all"}, "was not the word 'purge'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := purgeState(t)
			writeState(t, root, filepath.Join("containerd", "root", "layer"), 4096)
			writeState(t, root, filepath.Join("ca", "ca-key.pem"), 64)

			_, _, err := runCLI(t, tt.stdin, tt.args...)
			if err == nil {
				t.Fatal("the purge went ahead without confirmation")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
			for _, name := range []string{"containerd", "ca"} {
				if _, err := os.Lstat(filepath.Join(root, name)); err != nil {
					t.Errorf("%s was removed despite the refusal: %v", name, err)
				}
			}
		})
	}

	// The control: the same word, spelled right, does go ahead. Without this the
	// assertions above would still pass if the command refused everything.
	root := purgeState(t)
	writeState(t, root, filepath.Join("ca", "ca-key.pem"), 64)
	if _, errOut, err := runCLI(t, "purge\n", "purge", "--all"); err != nil {
		t.Fatalf("the correct confirmation was refused: %v\n%s", err, errOut)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Errorf("the state directory survived a confirmed --all: %v", err)
	}
}

// Deleting containerd's root out from under a running containerd would turn a disk-space
// problem into a wedged daemon, so the purge refuses while one is up and names how to stop it.
func TestPurgeRefusesWhileTheManagedDaemonIsRunning(t *testing.T) {
	root := purgeState(t)
	writeState(t, root, filepath.Join("containerd", "root", "layer"), 4096)
	holdDaemonLock(t, root)

	if _, running := daemon.Lookup(root); !running {
		t.Fatal("the fixture does not look like a running daemon; the test would prove nothing")
	}
	_, _, err := runCLI(t, "y\n", "purge")
	if err == nil {
		t.Fatal("the purge went ahead with a daemon running")
	}
	for _, want := range []string{"not purging while something is using this state",
		"boks daemon stop", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "containerd")); err != nil {
		t.Errorf("containerd's root was removed despite the refusal: %v", err)
	}

	// --force is the documented override, and it has to actually override.
	if _, errOut, err := runCLI(t, "y\n", "purge", "--force"); err != nil {
		t.Fatalf("--force did not override the refusal: %v\n%s", err, errOut)
	}
	if _, err := os.Lstat(filepath.Join(root, "containerd")); !os.IsNotExist(err) {
		t.Errorf("--force did not remove containerd's root: %v", err)
	}
}

// A sandbox's network supervisor can be up when the managed daemon is not — anyone using
// their own containerd through --containerd-address has exactly that — so it is a separate
// question with a separate refusal.
func TestPurgeRefusesWhileASandboxNetworkIsUp(t *testing.T) {
	root := purgeState(t)
	writeState(t, root, filepath.Join("containerd", "root", "layer"), 4096)
	holdStackLock(t, root, "web")

	if live := enforce.List(root); len(live) != 1 {
		t.Fatalf("the fixture reports %d live networks, want 1; the test would prove nothing", len(live))
	}
	_, _, err := runCLI(t, "y\n", "purge")
	if err == nil {
		t.Fatal("the purge went ahead with a sandbox network up")
	}
	for _, want := range []string{"a sandbox network is up for web", "boks stop web"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "containerd")); err != nil {
		t.Errorf("containerd's root was removed despite the refusal: %v", err)
	}
}

// A dry run has to warn about the refusal too. Otherwise the careful route — look first, then
// act — shows a plan and then fails on the very next command with no warning in between.
func TestPurgeDryRunReportsWhatWouldRefuse(t *testing.T) {
	root := purgeState(t)
	writeState(t, root, filepath.Join("containerd", "root", "layer"), 4096)
	holdDaemonLock(t, root)

	_, errOut, err := runCLI(t, "", "purge", "--dry-run")
	if err != nil {
		t.Fatalf("boks purge --dry-run: %v", err)
	}
	if !strings.Contains(errOut, "this would be refused while") {
		t.Errorf("a dry run under a running daemon says nothing about the refusal:\n%s", errOut)
	}
}

// --dry-run says not to remove anything and --yes says to remove it without asking. A caller
// that passed both has a bug, and either answer would be somebody's surprise.
func TestPurgeRefusesDryRunWithYes(t *testing.T) {
	root := purgeState(t)
	writeState(t, root, filepath.Join("containerd", "root", "layer"), 4096)

	_, errOut, code := mainExitCode(t, "purge", "--dry-run", "--yes")
	if code != 2 {
		t.Errorf("exit = %d, want 2 — contradictory flags are a usage error", code)
	}
	if !strings.Contains(errOut, "cannot be combined") {
		t.Errorf("stderr does not name the conflict:\n%s", errOut)
	}
	if _, err := os.Lstat(filepath.Join(root, "containerd")); err != nil {
		t.Errorf("the refused invocation removed something: %v", err)
	}
}

// A machine that has never run Boks, and a machine that was purged a minute ago, both have to
// exit 0 rather than report a missing directory as a failure.
func TestPurgeOnAMachineWithNoState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never-used")
	t.Setenv(policy.StateDirEnv, dir)

	out, _, code := mainExitCode(t, "purge", "--all")
	if code != 0 {
		t.Errorf("exit = %d, want 0 when there is no state directory", code)
	}
	if !strings.Contains(out, "nothing to purge") {
		t.Errorf("output does not say there is nothing to do:\n%s", out)
	}
}

// The catalogue is a fixed list of names, which is what makes the command safe — and what
// makes it possible for it to go quietly out of date. This asserts the other direction: every
// path Boks actually builds under the state directory resolves to a name the catalogue knows.
//
// It is written against the real constructors rather than against string literals, so renaming
// daemon.Dir's "containerd" or secret.DefaultPath's "secrets.json" fails here instead of
// shipping a purge that silently stops reclaiming the largest thing on the disk.
func TestPurgeCoversEveryPathBoksWritesUnderTheStateDirectory(t *testing.T) {
	root := purgeState(t)

	paths := []struct {
		what string
		path string
		dir  bool
	}{
		{"the containerd root and state", daemon.Dir(root), true},
		{"the containerd log", daemon.LogPath(root), false},
		{"the containerd configuration", daemon.ConfigPath(root), false},
		{"the containerd endpoint", daemon.Address(root), false},
		{"the certificate authority", ca.DefaultDir(root), true},
		{"the credential store", secret.DefaultPath(root), false},
		{"the decision log", policy.DefaultLogPath(), false},
		{"the policy store", policy.DefaultStorePath(), false},
		{"a sandbox's network state", enforce.StateDir(root, "web"), true},
		{"a sandbox's certificate directory", enforce.CertDir(root, "web"), true},
		{"a sandbox's notices", noticePath(root, "web"), false},
	}
	for _, tt := range paths {
		// On Windows the containerd endpoint is a named pipe rather than a path, so
		// it is nothing under the state directory and there is nothing to cover.
		if strings.HasPrefix(tt.path, `\\`) || strings.HasPrefix(tt.path, "npipe:") {
			continue
		}
		rel, err := filepath.Rel(root, tt.path)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			t.Errorf("%s (%s) is not under the state directory: %v", tt.what, tt.path, err)
			continue
		}
		if tt.dir {
			writeState(t, root, filepath.Join(rel, "probe"), 8)
		} else {
			writeState(t, root, rel, 8)
		}
		name := strings.Split(filepath.ToSlash(rel), "/")[0]
		if !purgeKnows(t, root, name) {
			t.Errorf("%s lives in %q, which 'boks purge' does not recognise; it would be "+
				"reported as a file boks did not write and never removed", tt.what, name)
		}
	}

	// The control: a name that genuinely is not Boks' must come back unrecognised, or
	// every assertion above passes by the check being unable to say no.
	writeState(t, root, "someone-elses-file", 8)
	if purgeKnows(t, root, "someone-elses-file") {
		t.Error("purgeKnows claims a file boks never writes; the assertions above prove nothing")
	}
}

// purgeKnows asks the command itself whether a name is one it manages, by reading the plan it
// prints. Going through the CLI rather than the catalogue is the point: it is the answer a
// user gets.
func purgeKnows(t *testing.T, root, name string) bool {
	t.Helper()
	out, _, err := runCLI(t, "", "purge", "--all", "--dry-run")
	if err != nil {
		t.Fatalf("boks purge --dry-run: %v", err)
	}
	_, unknown, found := strings.Cut(out, "left alone")
	if !found {
		return true
	}
	for _, line := range strings.Split(unknown, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), name+" ") ||
			strings.TrimSpace(line) == name {
			return false
		}
	}
	return true
}

// holdDaemonLock makes daemon.Lookup report a running managed containerd, by leaving the
// record a supervisor leaves and holding the lock its liveness is decided by.
func holdDaemonLock(t *testing.T, root string) {
	t.Helper()
	dir := daemon.Dir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(daemon.State{Address: filepath.Join(dir, "containerd.sock")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "daemon.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := proclock.Acquire(filepath.Join(dir, "daemon.lock"))
	if err != nil {
		t.Fatalf("taking the daemon lock: %v", err)
	}
	t.Cleanup(release)
}

// holdStackLock makes enforce.List report a live network for one sandbox, the same way.
func holdStackLock(t *testing.T, root, sandbox string) {
	t.Helper()
	dir := enforce.StateDir(root, sandbox)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(enforce.State{Sandbox: sandbox, PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stack.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := proclock.Acquire(filepath.Join(dir, "stack.lock"))
	if err != nil {
		t.Fatalf("taking the stack lock: %v", err)
	}
	t.Cleanup(release)
}
