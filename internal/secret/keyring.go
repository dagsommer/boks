package secret

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// A Keyring is the OS's own secret store: macOS Keychain, the Linux Secret Service, Windows
// Credential Manager.
//
// # Why the interface is three methods and not the store's whole surface
//
// A secret store has to do more than hold values — it lists names, holds structured OAuth
// records, and reports where it lives. Implementing all of that three times, once per OS,
// against three APIs that disagree about everything including whether enumeration exists,
// would be three times the code that cannot be run on this project's machines.
//
// So the OS is asked for the one thing only it can do: keep a named string where the operating
// system decides who may read it. Everything else — the list of names, the structure of an
// OAuth record, the JSON — is built on top, once, in Go that runs everywhere and is tested
// like any other code. See KeyringStore.
//
// # Enumeration in particular
//
// Deliberately absent. Listing secrets is the operation the three platforms differ on most:
// macOS's `security dump-keychain` prompts for permission per item and returns a format meant
// for humans, libsecret searches by attribute, and Windows enumerates by filter. Every one of
// those is a different failure mode for a feature — `boks secret ls` — that only needs to know
// which NAMES exist, and a name is not a secret. KeyringStore keeps the names in a plain index
// file beside the state directory and asks the OS only for values.
type Keyring interface {
	// Get returns the value stored under name, or ErrNotFound.
	Get(ctx context.Context, name string) (string, error)
	// Set stores value under name, replacing any existing one.
	Set(ctx context.Context, name, value string) error
	// Delete removes name. Removing something absent is not an error, so that a delete
	// after a partial write leaves nothing behind.
	Delete(ctx context.Context, name string) error
	// Describe names this keyring the way its own platform does — "macOS Keychain",
	// "Secret Service", "Windows Credential Manager" — so that a user told where their
	// credential went can go and look at it.
	Describe() string
}

// ErrNoKeyring reports that this host has no usable OS secret store.
//
// It is a distinct error because the caller's response is specific: fall back to the
// passphrase-encrypted file and say why, rather than fail. A Linux box with no D-Bus session —
// a container, an SSH session with no login keyring — is the common case and is not broken.
var ErrNoKeyring = errors.New("no OS keyring available")

// keyringService is the service name every entry is filed under, so that Boks' secrets are
// identifiable in Keychain Access, seahorse or the Credential Manager UI, and so that
// uninstalling can find them.
const keyringService = "boks"

// OpenKeyring returns this host's OS keyring, or ErrNoKeyring.
//
// The probe is a real operation rather than a check for an executable: a `security` binary
// that exists but cannot reach a keychain, or a secret-tool with no session bus behind it,
// both look installed and both fail at the first Set. Finding that out at store-open time
// gives a clear message; finding it out later gives a failed `boks run`.
func OpenKeyring(ctx context.Context) (Keyring, error) {
	// An explicit opt-out, for two callers who need the same thing for different reasons.
	//
	// A user may prefer the encrypted file — a shared account, a machine whose keychain
	// prompts more than they want, a setup where the credentials must travel with the
	// state directory. Without a way to say so, the keyring being preferred would be a
	// decision Boks makes and they cannot unmake.
	//
	// And a test needs the answer to be the same on every platform. "No passphrase means
	// no store" is true on a host without a keyring and false on one with it, so a test
	// asserting either would pass on this project's Linux machines and fail on the Mac and
	// the Windows runner — which is precisely what happened.
	if os.Getenv(DisableKeyringEnv) != "" {
		return nil, keyringUnavailable(DisableKeyringEnv+" is set", nil)
	}
	return openKeyring(ctx)
}

// DisableKeyringEnv turns the OS keyring off, leaving the passphrase-encrypted file.
const DisableKeyringEnv = "BOKS_NO_KEYRING"

// keyringUnavailable builds the ErrNoKeyring an implementation returns, keeping the reason.
func keyringUnavailable(reason string, err error) error {
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNoKeyring, reason, err)
	}
	return fmt.Errorf("%w: %s", ErrNoKeyring, reason)
}

// validKeyringName rejects a name that cannot survive a round trip through the OS stores.
//
// The three platforms have different limits and Boks applies the intersection, because a name
// that works on one host and not another turns a portable state directory into a
// platform-specific one. Empty names, control characters and newlines are refused everywhere;
// a newline in particular is how a value gets mistaken for the end of a `security` CLI
// response.
func validKeyringName(name string) error {
	if name == "" {
		return errors.New("a secret name cannot be empty")
	}
	if len(name) > 255 {
		return fmt.Errorf("secret name %q is longer than 255 characters", name)
	}
	if strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("secret name %q contains a control character", name)
	}
	return nil
}

// keyringExitCode reports a finished command's exit status.
//
// It lives here rather than beside the backend that uses it because the test that exercises it
// carries no build constraint — it has to run on this project's Linux machines, which is where
// the macOS and Windows backends can never be tested — and an unconstrained test can only see
// symbols the BUILT platform defines. Three copies, one per OS file, left the Windows build
// (which uses syscalls and needs no such helper) failing to vet.
func keyringExitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return 0, false
	}
	// ExitCode is -1 for a process killed by a signal: the portable spelling of "there is no
	// status here to read".
	if code := exit.ExitCode(); code >= 0 {
		return code, true
	}
	return 0, false
}
