//go:build linux

package secret

// The Linux Secret Service — GNOME Keyring, KWallet's Secret Service front end, KeePassXC's,
// anything else that owns org.freedesktop.secrets — driven through libsecret's
// `secret-tool(1)`.
//
// **This backend has never been executed.** The machine Boks is built on is a Linux container
// with no secret-tool and no session bus, which is precisely the host this file is supposed
// to *decline* to run on, and even that has not been observed. What is tested is the
// platform-neutral part — the exit-status reading, in keyring_ostest_test.go — and the rest is
// justified from libsecret's own source for the tool, cited where it decides something. The
// first run on a real desktop session is a verification step; see docs/security-model.md.
//
// The CLI rather than a cgo binding against libsecret, for the reason import.go already
// gives for the Keychain: cgo would make every build of Boks platform-specific, and it would
// add a build-time dependency on libsecret headers to a project that otherwise builds with
// nothing but Go.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"unicode/utf8"
)

// secretServiceKeyring is the Keyring backed by whatever owns the Secret Service on this bus.
type secretServiceKeyring struct{}

// secretToolProbeAccount is the account openKeyring looks for. Nothing is created by looking.
const secretToolProbeAccount = "boks-keyring-availability-probe-do-not-create"

// secretToolMaxValue is the longest value `secret-tool store` will take.
//
// tool/secret-tool.c's read_password_stdin allocates 8192 bytes once and reads into what is
// left of it; when the buffer fills it prints "password is too long" and stores the truncated
// prefix anyway. Refusing at 8191 keeps Boks clear of both the truncation and the warning
// that a single read returning exactly the remaining space would produce.
const secretToolMaxValue = 8191

// openKeyring implements OpenKeyring.
//
// # Why this probes rather than reads DBUS_SESSION_BUS_ADDRESS
//
// The question is "will a Secret Service answer", and the environment variable answers a
// different one. It is wrong in both directions: unset does not mean absent, because libsecret
// goes through GDBus, which falls back to the well-known socket at $XDG_RUNTIME_DIR/bus and
// can autolaunch a bus besides; and set does not mean present, because a variable inherited
// from a session that has since died points at a socket nothing is listening on, and a stale
// value looks exactly like a good one. Neither is a rare arrangement — a systemd user session
// gives the first, `sudo -E` and long-lived tmux sessions give the second.
//
// So the decision is made by running the lookup and reading what comes back, and the variable
// is used for the one thing it is good for: when it is unset and the probe has failed, saying
// so turns "no OS keyring available" into a sentence the user can act on.
func openKeyring(ctx context.Context) (Keyring, error) {
	k := secretServiceKeyring{}
	res, err := runSecretTool(ctx, nil, "lookup", "--", "service", keyringService, "account", secretToolProbeAccount)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, keyringUnavailable("secret-tool is not installed (it ships in libsecret-tools or libsecret)", nil)
		}
		return nil, keyringUnavailable("could not run secret-tool", err)
	}
	// A lookup that found nothing is the expected answer and the success case: it means the
	// tool reached a Secret Service and searched it. See Get for why an empty stderr is what
	// separates that from a failure.
	if res.code == 0 || (res.code == 1 && res.stderr == "") {
		return k, nil
	}
	return nil, keyringUnavailable(secretToolFailure(res), nil)
}

// secretToolFailure describes a run that failed, adding the session-bus hint when the most
// common cause of it is also true.
func secretToolFailure(res secretToolResult) string {
	reason := fmt.Sprintf("secret-tool exited %d: %s", res.code, res.stderr)
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		reason += "; DBUS_SESSION_BUS_ADDRESS is not set, so this is probably a session with no D-Bus (a container, or ssh without a login keyring)"
	}
	return reason
}

// Get implements Keyring.
func (k secretServiceKeyring) Get(ctx context.Context, name string) (string, error) {
	if err := validKeyringName(name); err != nil {
		return "", err
	}
	res, err := runSecretTool(ctx, nil, "lookup", "--", "service", keyringService, "account", name)
	if err != nil {
		return "", keyringUnavailable("could not run secret-tool", err)
	}
	switch {
	case res.code == 0:
		// Verbatim, with nothing trimmed. secret-tool.c's write_password_stdout writes the
		// secret's bytes and appends a newline only `if (isatty (1))` — stdout here is a
		// pipe, so what arrives is the value and nothing else. Trimming a newline "just in
		// case" would corrupt any secret that ends in one.
		return res.stdout, nil
	case res.code == 1 && res.stderr == "":
		// The distinction this whole function exists to make. secret_tool_action_lookup
		// returns 1 twice over: once for an error, having written it to stderr with
		// g_printerr, and once for `value == NULL` — nothing matched — having written
		// nothing anywhere. The exit status alone cannot tell those apart, and treating a
		// broken Secret Service as "no such secret" is what would make Boks quietly ask the
		// user to paste a credential they already stored.
		return "", fmt.Errorf("%w: %q", ErrNotFound, name)
	default:
		return "", keyringUnavailable(secretToolFailure(res), nil)
	}
}

// Set implements Keyring.
func (k secretServiceKeyring) Set(ctx context.Context, name, value string) error {
	if err := validKeyringName(name); err != nil {
		return err
	}
	if len(value) > secretToolMaxValue {
		return fmt.Errorf("this secret is %d bytes and secret-tool truncates anything over %d", len(value), secretToolMaxValue)
	}
	if !utf8.ValidString(value) {
		// read_password_stdin calls g_utf8_validate and exit(1)s on failure, so this would
		// fail anyway — it is checked here so the message says what is wrong rather than
		// quoting a tool the user did not know was involved.
		return errors.New("the Secret Service backend can only store text: this value is not valid UTF-8")
	}
	// The value goes on stdin, which is what `secret-tool store` wants and also the only
	// place it may go: an argument would put the secret in this process's argv, where
	// anything that can see the process can read it. secret_tool_action_store branches on
	// isatty(0) — a terminal gets a getpass prompt, anything else is read to EOF — and a
	// pipe is what os/exec builds for a Reader, so this takes the second branch.
	//
	// Written with no trailing newline, on purpose. read_password_stdin read(2)s until EOF
	// and stores every byte it got, so a newline added for tidiness would be stored as part
	// of the secret and would come back from Get.
	label := "boks: " + name
	// `--` before the attributes because validKeyringName permits a leading dash and GOption
	// would otherwise read the name as an option. GLib's g_option_context_parse treats a bare
	// "--" as the end of options and then removes it from argv, so the attribute pairs stay
	// paired.
	res, err := runSecretTool(ctx, strings.NewReader(value), "store", "--label="+label, "--",
		"service", keyringService, "account", name)
	if err != nil {
		return keyringUnavailable("could not run secret-tool", err)
	}
	if res.code != 0 {
		return keyringUnavailable(secretToolFailure(res), nil)
	}
	return nil
}

// Delete implements Keyring.
func (k secretServiceKeyring) Delete(ctx context.Context, name string) error {
	if err := validKeyringName(name); err != nil {
		return err
	}
	res, err := runSecretTool(ctx, nil, "clear", "--", "service", keyringService, "account", name)
	if err != nil {
		return keyringUnavailable("could not run secret-tool", err)
	}
	switch {
	case res.code == 0:
		return nil
	case res.code == 1 && res.stderr == "":
		// secret_tool_action_clear returns 1 when secret_password_clearv_sync reports that
		// it removed nothing, printing only when there was also an error. Nothing removed is
		// the outcome Delete asked for.
		return nil
	default:
		return keyringUnavailable(secretToolFailure(res), nil)
	}
}

// secretToolResult is one finished run of the tool.
type secretToolResult struct {
	stdout string
	stderr string
	code   int
}

// runSecretTool runs `secret-tool`, feeding it stdin when there is any.
//
// A non-zero exit is a result and not an error: exit 1 is how the tool says both "no such
// item" and "the bus is gone", and only the caller — with the stderr this returns — can tell
// which.
func runSecretTool(ctx context.Context, stdin io.Reader, args ...string) (secretToolResult, error) {
	// CommandContext, not Command: a keyring daemon that has stopped answering — waiting on
	// a prompt nobody will see, most often — must fail on the caller's deadline rather than
	// hang `boks run` forever.
	cmd := exec.CommandContext(ctx, "secret-tool", args...)
	cmd.Stdin = stdin
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	// Only stderr is quoted back, capped, for the same reason import.go caps it: a message
	// the user cannot see is a support ticket, and an unbounded one is a wall of text. The
	// secret is never on stderr — a found value goes to stdout — so this is safe to repeat.
	// The trim also matters to Get and Delete, which read "stderr said nothing at all" as
	// "the item is simply not there".
	detail := strings.TrimSpace(errOut.String())
	if len(detail) > 200 {
		detail = detail[:200]
	}
	res := secretToolResult{stdout: out.String(), stderr: detail}
	code, ok := keyringExitCode(err)
	if !ok {
		return res, err
	}
	res.code = code
	return res, nil
}

// Describe names this platform's store, so that a user told where a credential went can
// open the right application and look at it.
func (k secretServiceKeyring) Describe() string { return "Secret Service keyring" }
