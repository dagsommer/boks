package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// readSecretValue obtains a credential from the user, from a pipe when there is one and from
// the terminal when there is not.
//
// # Why a prompt and not just stdin
//
// `boks secret set NAME` read all of stdin unconditionally. Piped, that is exactly right and
// is the form to prefer, because an argument is visible in `ps` to every user on the machine.
// TYPED, it was awful: the command printed nothing and sat there, because io.ReadAll on a
// terminal waits for EOF. The user is looking at a cursor with no prompt, and the thing that
// ends it is Ctrl-D — which nobody guesses, and which after a newline stores a credential
// with a trailing character or nothing at all.
//
// So a terminal gets a prompt and one line, and a pipe gets what it always got.
//
// # Echo
//
// Off while typing, because the alternative is a credential in the terminal's scrollback and
// in any session recording. term.ReadPassword restores the terminal state itself, including
// when the read fails, which is the part worth not writing by hand: a program that exits with
// echo off leaves the user typing blind into their shell.
func readSecretValue(stdin io.Reader, stdout io.Writer, prompt string) (string, error) {
	f, isFile := stdin.(*os.File)
	if !isFile || !term.IsTerminal(int(f.Fd())) {
		// A pipe, a file, or a test's strings.Reader. Read it whole and trim only the
		// line ending, because `printf 'tok' | boks secret set x` and `echo tok | …`
		// must store the same thing.
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("reading the credential from stdin: %w", err)
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	}

	fmt.Fprint(stdout, prompt)
	line, err := term.ReadPassword(int(f.Fd()))
	// The newline the user's Return did not echo, so that whatever prints next starts on
	// its own line rather than after the prompt.
	fmt.Fprintln(stdout)
	if err != nil {
		return "", fmt.Errorf("reading the credential: %w", err)
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

// confirmSecretValue reads a credential twice and refuses to store one that does not match.
//
// Only when typing. A typo in a pasted token is silent until an agent fails to authenticate
// hours later, and the error it produces then names the origin rather than the credential. A
// piped value is not re-read, because there is nothing to compare it against and asking would
// consume input that is not there.
func confirmSecretValue(stdin io.Reader, stdout io.Writer, name string) (string, error) {
	first, err := readSecretValue(stdin, stdout, fmt.Sprintf("Credential for %q: ", name))
	if err != nil {
		return "", err
	}
	if first == "" {
		return "", errors.New("the credential is empty")
	}
	f, isFile := stdin.(*os.File)
	if !isFile || !term.IsTerminal(int(f.Fd())) {
		return first, nil
	}
	second, err := readSecretValue(stdin, stdout, "Repeat: ")
	if err != nil {
		return "", err
	}
	if first != second {
		// Neither value is printed or hinted at, not even its length.
		return "", errors.New("the two entries do not match; nothing was stored")
	}
	return first, nil
}
