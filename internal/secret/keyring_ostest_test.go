package secret

// What can be tested about the OS keyring backends on a machine that has none.
//
// No build constraint, on purpose: everything here is either platform-neutral or is a copy
// that exists on every platform, so this file is the one part of the keyring work that is
// actually exercised on macOS, Linux and Windows alike. The backends themselves are not
// touched — running them would need a Keychain, a Secret Service or a Credential Manager, and
// a test that quietly skipped itself on the machines Boks is built on would be a test that
// never runs.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestValidKeyringName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"a plain name", "github", false},
		{"empty", "", true},
		{"a space, which is not a control character", "my token", false},
		{"the longest accepted name", strings.Repeat("a", 255), false},
		{"one byte too long", strings.Repeat("a", 256), true},
		// Length is counted in bytes, and the limit is a byte limit on every one of the
		// three stores, so a name of 128 two-byte runes must be refused even though it is
		// 128 characters.
		{"256 bytes of two-byte runes", strings.Repeat("é", 128), true},
		{"non-ASCII inside the limit", "café", false},
		{"a newline", "two\nlines", true},
		{"a carriage return", "two\rlines", true},
		{"a tab", "two\tcolumns", true},
		{"a NUL", "nul\x00byte", true},
		{"DEL", "del\x7fbyte", true},
		{"the last control character below space", "unit\x1fseparator", true},
		// Accepted, and both backends have to cope with it: a name that begins with a dash
		// is an option to getopt and to GOption. keyring_linux.go passes `--` before the
		// attributes for this reason, and keyring_darwin.go relies on getopt taking the
		// argument of -a whatever it looks like.
		{"a leading dash", "-w", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validKeyringName(c.input)
			if c.wantErr && err == nil {
				t.Fatalf("validKeyringName(%q) = nil, want an error", c.input)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("validKeyringName(%q) = %v, want nil", c.input, err)
			}
			// The name is not a secret, but the error is what a user reads, so it has to
			// name the thing that was wrong — quoted, since the names being refused here are
			// exactly the ones that would otherwise put a control character in a message.
			if c.wantErr && c.input != "" && !strings.Contains(err.Error(), strconv.Quote(c.input)) {
				t.Errorf("error %q does not quote the offending name %q", err, c.input)
			}
		})
	}
}

// TestKeyringUnavailableIsNotNotFound pins the invariant every backend's classification rests
// on: the error that means "this host has no usable keyring" must not be mistakable for the
// error that means "this host has one and the secret is not in it". Conflating them is what
// would make Boks ask again for a credential the user already stored.
func TestKeyringUnavailableIsNotNotFound(t *testing.T) {
	err := keyringUnavailable("no session bus", errors.New("connection refused"))
	if !errors.Is(err, ErrNoKeyring) {
		t.Errorf("keyringUnavailable() = %v, want it to wrap ErrNoKeyring", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("keyringUnavailable() = %v, must not also be ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "no session bus") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("keyringUnavailable() = %q, want it to keep both the reason and the cause", err)
	}
}

func TestKeyringExitCode(t *testing.T) {
	t.Run("a command that succeeded", func(t *testing.T) {
		code, ok := keyringExitCode(nil)
		if !ok || code != 0 {
			t.Errorf("keyringExitCode(nil) = %d, %v; want 0, true", code, ok)
		}
	})

	// The statuses that decide something. 44 is errSecItemNotFound truncated to a byte, and
	// 1 is how secret-tool says both "not found" and "the bus is gone"; 127 is what a shell
	// returns for a missing command, and is here to prove that keyringExitCode reports it
	// rather than rounding it to one of the two that mean something.
	for _, want := range []int{0, 1, 44, 127} {
		t.Run("a command that exited "+strconv.Itoa(want), func(t *testing.T) {
			err := exitHelper(t, strconv.Itoa(want)).Run()
			code, ok := keyringExitCode(err)
			if !ok {
				t.Fatalf("keyringExitCode(%v) reported no usable status, want %d", err, want)
			}
			if code != want {
				t.Errorf("keyringExitCode(%v) = %d, want %d", err, code, want)
			}
		})
	}

	t.Run("a command that was never run", func(t *testing.T) {
		// The shape of "secret-tool is not installed": exec reports its own error, not an
		// ExitError, and there is no status to read.
		err := exec.Command("boks-no-such-keyring-tool-9f3c").Run()
		if err == nil {
			t.Fatal("running a command that does not exist somehow succeeded")
		}
		if code, ok := keyringExitCode(err); ok {
			t.Errorf("keyringExitCode(%v) = %d, true; want a refusal to read a status", err, code)
		}
	})

	t.Run("a context that was already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := exec.CommandContext(ctx, exitHelperPath(t), "-test.run=TestKeyringExitHelper").Run()
		if err == nil {
			t.Fatal("a cancelled context did not stop the command")
		}
		if code, ok := keyringExitCode(err); ok {
			t.Errorf("keyringExitCode(%v) = %d, true; want a refusal to read a status", err, code)
		}
	})

	t.Run("a command killed by a signal", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			// Kill on Windows terminates with status 1, which is a status and is read as
			// one. There is nothing to assert here that would not be asserting Windows.
			t.Skip("no signals on Windows")
		}
		cmd := exitHelper(t, "hang")
		if err := cmd.Start(); err != nil {
			t.Skipf("cannot start a child process on %s: %v", runtime.GOOS, err)
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("killing the child: %v", err)
		}
		err := cmd.Wait()
		if err == nil {
			t.Fatal("a killed child reported success")
		}
		// The case that would be silently wrong if keyringExitCode used ExitCode() alone:
		// a signalled process has ExitCode -1, and anything that clamped that to 0 would
		// report a killed `security` as a Keychain that answered "yes".
		if code, ok := keyringExitCode(err); ok {
			t.Errorf("keyringExitCode(%v) = %d, true; a signalled process has no status", err, code)
		}
	})
}

// exitHelperEnv carries the status the child process should exit with, or "hang".
const exitHelperEnv = "BOKS_SECRET_TEST_EXIT"

func exitHelperPath(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	return self
}

// exitHelper builds a command that exits with a chosen status.
//
// It re-executes the test binary rather than shelling out, following
// internal/proclock/process_test.go: there is no command spelled the same way on every
// platform, and this file has to run on all of them.
func exitHelper(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(exitHelperPath(t), "-test.run=TestKeyringExitHelper")
	cmd.Env = append(os.Environ(), exitHelperEnv+"="+mode)
	return cmd
}

// TestKeyringExitHelper is the child half of exitHelper. Under an ordinary test run the
// variable is unset and it does nothing.
func TestKeyringExitHelper(t *testing.T) {
	mode := os.Getenv(exitHelperEnv)
	switch {
	case mode == "":
		t.Skip("not the child process")
	case mode == "hang":
		// Sleeping rather than blocking forever: a child with nothing runnable is a
		// deadlock the Go runtime detects and exits 2 for, which is a status, which is the
		// opposite of what the caller is trying to produce.
		time.Sleep(time.Hour)
		os.Exit(3)
	default:
		code, err := strconv.Atoi(mode)
		if err != nil {
			os.Exit(2)
		}
		os.Exit(code)
	}
}
