//go:build darwin

package secret

// The macOS Keychain, driven through the `security(1)` command line tool.
//
// **This backend has never been executed.** Boks is built and tested on Linux; there is no
// Keychain on that machine, and not one line below has ever run. What is tested is the part
// that is platform-neutral — the exit-status reading, in keyring_ostest_test.go — and every
// decision that could not be tested is instead justified from Apple's published source for
// the tool, quoted at the point it decides something. The first run on a real Mac is a
// verification step, not a formality; see docs/security-model.md.
//
// The CLI rather than a cgo binding, for the reason import.go already gives: cgo would make
// every build of Boks platform-specific for the sake of three operations, and `security` is
// present on every macOS install.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// keychainKeyring is the Keyring backed by the login keychain.
//
// No state: `security` resolves the default keychain per invocation, which is also what
// makes the three operations independent of each other and of any session this process holds.
type keychainKeyring struct{}

// errSecItemNotFoundStatus is the exit status `security` gives when nothing matched.
//
// It is 44 for an arithmetic reason worth writing down, because "44" on its own looks like
// folklore: the tool returns the OSStatus, and a process exit status is truncated to its low
// byte. errSecItemNotFound is -25300; as a 32-bit two's complement value that is 0xFFFF9D2C,
// whose low byte is 0x2C = 44. Any other non-zero status is some other OSStatus and is not a
// statement about whether the item exists.
const errSecItemNotFoundStatus = 44

// keychainProbeAccount is the account openKeyring looks for. It is not created, and it is
// long and specific enough that finding it would mean something stranger than a name
// collision.
const keychainProbeAccount = "boks-keyring-availability-probe-do-not-create"

// securityMaxLine is the longest command line `security -i` will read.
//
// SecurityTool/macOS/security.c reads interactive input with readline(buffer, MAX_LINE_LEN)
// into a 4096-byte static buffer, and sharedTool/readline.c stops at buffer_size - 1 without
// reporting anything — the tail of an over-long line stays in the stream and is parsed as the
// *next* command. So an over-long line does not fail, it silently stores a truncated secret
// and then runs a fragment of it as a command name. Boks refuses the write instead. The limit
// here counts the trailing newline, leaving 4094 bytes for the escaped command.
const securityMaxLine = 4095

// openKeyring implements OpenKeyring.
func openKeyring(ctx context.Context) (Keyring, error) {
	k := keychainKeyring{}
	// A find, so nothing is created and nothing is unlocked that a read would not unlock
	// anyway. "Not found" is the expected answer and is the success case: it means the tool
	// ran, reached a keychain and searched it.
	//
	// The one thing this probe cannot avoid: searching a *locked* keychain makes the
	// Security framework ask the user to unlock it, which on a desktop session is a dialog.
	// That is the same dialog the first real Get would raise, so the probe brings it forward
	// rather than adding one — but it is why no deadline is imposed here beyond the
	// caller's. A five-second timeout would read a user typing their keychain password as
	// "this host has no keyring" and quietly fall back to the passphrase file.
	res, err := runSecurity(ctx, "", "find-generic-password", "-s", keyringService, "-a", keychainProbeAccount, "-w")
	if err != nil {
		return nil, keyringUnavailable("could not run the security tool", err)
	}
	switch res.code {
	case 0, errSecItemNotFoundStatus:
		return k, nil
	default:
		return nil, keyringUnavailable(fmt.Sprintf("security exited %d: %s", res.code, res.stderr), nil)
	}
}

// Get implements Keyring.
func (k keychainKeyring) Get(ctx context.Context, name string) (string, error) {
	if err := validKeyringName(name); err != nil {
		return "", err
	}
	res, err := runSecurity(ctx, "", "find-generic-password", "-s", keyringService, "-a", name, "-w")
	if err != nil {
		return "", keyringUnavailable("could not run the security tool", err)
	}
	switch res.code {
	case 0:
		// keychain_find.c's do_password_item_printing writes the password bytes and then a
		// single putchar('\n'). Exactly one newline, always, so exactly one is removed.
		//
		// The same function has a branch this code depends on Set to make unreachable: if
		// any byte of the stored password fails isprint(), it prints the whole password as
		// lowercase hex instead — with nothing to say that it did. Hex output is
		// indistinguishable from a value that happens to be hex digits, so it cannot be
		// detected here; it can only be prevented at write time, which is what
		// securityStorableValue does.
		return strings.TrimSuffix(res.stdout, "\n"), nil
	case errSecItemNotFoundStatus:
		return "", fmt.Errorf("%w: %q", ErrNotFound, name)
	default:
		return "", keyringUnavailable(fmt.Sprintf("security exited %d looking for %q: %s", res.code, name, res.stderr), nil)
	}
}

// Set implements Keyring.
func (k keychainKeyring) Set(ctx context.Context, name, value string) error {
	if err := validKeyringName(name); err != nil {
		return err
	}
	if err := securityStorableValue(value); err != nil {
		return err
	}
	// The value never appears in argv. `security add-generic-password -w <value>` would put
	// the secret in this process's arguments, where it is readable by anything that can see
	// the process — and the point of using the Keychain at all is that the operating system
	// decides who may read the value.
	//
	// The obvious alternative does not work. `-w` with no argument does prompt, but
	// keychain_add.c's promptForPasswordData calls getpass(3) twice and compares, and
	// Libc's getpass is readpassphrase(prompt, buf, _PASSWORD_LEN + 1, RPP_ECHO_OFF), which
	// (a) opens /dev/tty and reads from *that*, ignoring a pipe on stdin whenever this
	// process has a controlling terminal, and (b) keeps at most 128 bytes and discards the
	// rest without a word. Feeding a secret to it would hang on a terminal and truncate off
	// one.
	//
	// So the whole command goes over stdin instead, through `security -i`: it reads command
	// lines from stdin and dispatches them to the same commands, so the value crosses a pipe
	// and lands in the child's own argv, which the kernel never publishes.
	line, err := securityCommandLine("add-generic-password", "-U", "-s", keyringService, "-a", name, "-w", value)
	if err != nil {
		return err
	}
	res, err := runSecurity(ctx, line, "-i")
	if err != nil {
		return keyringUnavailable("could not run the security tool", err)
	}
	if res.code != 0 {
		// Deliberately coarse: anything other than success means Boks cannot rely on this
		// keychain for this write, and the caller's answer to that is the same whatever the
		// OSStatus was. res.stderr is diagnostics from the tool — it never carries the value.
		return keyringUnavailable(fmt.Sprintf("security exited %d storing %q: %s", res.code, name, res.stderr), nil)
	}
	return nil
}

// Delete implements Keyring.
func (k keychainKeyring) Delete(ctx context.Context, name string) error {
	if err := validKeyringName(name); err != nil {
		return err
	}
	res, err := runSecurity(ctx, "", "delete-generic-password", "-s", keyringService, "-a", name)
	if err != nil {
		return keyringUnavailable("could not run the security tool", err)
	}
	switch res.code {
	case 0, errSecItemNotFoundStatus:
		// Already gone is the outcome Delete asked for.
		return nil
	default:
		return keyringUnavailable(fmt.Sprintf("security exited %d deleting %q: %s", res.code, name, res.stderr), nil)
	}
}

// securityResult is one finished run of the tool.
type securityResult struct {
	stdout string
	stderr string
	code   int
}

// runSecurity runs `security`, feeding it stdin when there is any.
//
// A non-zero exit is a result and not an error, because 44 is how the Keychain says "no such
// item" and turning that into an error here is what would make the caller unable to tell
// "absent" from "broken". The error return is for the run not happening at all.
func runSecurity(ctx context.Context, stdin string, args ...string) (securityResult, error) {
	// CommandContext, not Command: a Keychain that has stopped answering must fail `boks
	// run` on the caller's deadline rather than hang it forever.
	cmd := exec.CommandContext(ctx, "security", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	// Leaving Stdin nil otherwise is deliberate: os/exec gives the child /dev/null, so a
	// `security` that decided to read something can never take it from whatever Boks was
	// invoked with.
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	// Only stderr is quoted back, capped, for the same reason import.go caps it: a message
	// the user cannot see is a support ticket, and an unbounded one is a wall of text. The
	// value is never on stderr — `security` writes a found password to stdout under -w.
	detail := strings.TrimSpace(errOut.String())
	if len(detail) > 200 {
		detail = detail[:200]
	}
	res := securityResult{stdout: out.String(), stderr: detail}
	code, ok := keyringExitCode(err)
	if !ok {
		return res, err
	}
	res.code = code
	return res, nil
}

// securityStorableValue refuses a value the Keychain round trip would not return unchanged.
//
// Two limits, both from the tool rather than from the Keychain:
//
//   - keychain_find.c prints a password as hex, unannounced, if any byte of it fails
//     isprint(). `security` never calls setlocale, so that is the C locale: 0x20 to 0x7e and
//     nothing else. A value outside that range would be written correctly and read back as
//     hex digits, which is silent corruption rather than a failure — so it is refused here,
//     loudly, at the only moment a caller can still do something about it.
//   - a newline or a NUL cannot survive the interactive command line at all: readline() ends
//     a line at '\n', and the argument vector is C strings.
//
// The second is implied by the first; both are checked because the first is a property of the
// *reading* tool and could be relaxed one day by storing an encoding of the value (base64
// keeps everything inside printable ASCII), while the second is a property of how it is
// written and would still hold.
//
// The error names no part of the value.
func securityStorableValue(value string) error {
	for i := 0; i < len(value); i++ {
		if c := value[i]; c < 0x20 || c > 0x7e {
			return fmt.Errorf("this credential contains a byte (%#x) the macOS Keychain "+
				"backend cannot store: `security` prints any value with a non-printable "+
				"byte back as hexadecimal, with nothing to mark that it did, so storing it "+
				"would mean handing that hex to an origin as if it were the secret.\n"+
				"API tokens and OAuth records are unaffected — they are ASCII. If this one "+
				"genuinely is not, set BOKS_SECRETS_PASSPHRASE to use the encrypted file "+
				"store instead, which has no such limit", c)
		}
	}
	return nil
}

// securityCommandLine renders one line for `security -i`.
//
// The escaping is total — a backslash before every single byte — rather than shell-style
// quoting, and that is what makes it safe to hand it an arbitrary value. security.c's
// split_line is a five-state machine in which READ_ARG_ESCAPED copies the next byte verbatim
// and returns, with no exceptions for quotes, spaces or backslashes; so escaping everything
// cannot produce a token boundary, cannot open a quote, and needs no knowledge of which
// bytes are special. An empty argument has no bytes to escape and so is written as a pair of
// quotes, which split_line turns into an empty token.
//
// Argument count is not checked: security.c stops at MAX_ARGS 32 and the longest line built
// here has eight.
func securityCommandLine(args ...string) (string, error) {
	var b strings.Builder
	for i, arg := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		if arg == "" {
			b.WriteString("\"\"")
			continue
		}
		for j := 0; j < len(arg); j++ {
			c := arg[j]
			if c == '\n' || c == 0 {
				return "", errors.New("a keychain value cannot contain a newline or a NUL byte")
			}
			b.WriteByte('\\')
			b.WriteByte(c)
		}
	}
	b.WriteByte('\n')
	if b.Len() > securityMaxLine {
		return "", fmt.Errorf("this secret is too long for the security tool: the escaped command is %d bytes and the limit is %d",
			b.Len(), securityMaxLine)
	}
	return b.String(), nil
}
