package sandbox

import (
	"context"
	"io"
	"os"

	"github.com/containerd/containerd/v2/client"
)

// terminalEnv returns the host's terminal environment variables in KEY=VALUE form, for
// forwarding into the guest when a TTY is in use. Only variables that are set on the host
// are included. Callers use replaceOrAppendEnv to layer these as overrides, so explicit
// --env flags passed by the user take precedence.
//
// The five vars included:
//
//   - TERM: the terminfo key — colors, cursor movement, and everything terminfo provides.
//   - COLORTERM: signals true-color support to clients that check it ("truecolor" / "24bit").
//   - TERM_PROGRAM: the terminal emulator's identity, used by some apps for keyboard-protocol
//     and feature negotiation (e.g. Kitty keyboard protocol, Shift-Enter handling in Claude Code).
//   - TERM_PROGRAM_VERSION: complements TERM_PROGRAM for version-gated features.
//   - TERMINAL_EMULATOR: the JetBrains terminal emulator identity, set instead of TERM_PROGRAM.
//   - NO_COLOR: user intent to suppress color output. Checked for presence, not value — the
//     convention is "set at all, even to empty".
//
// Deliberately excluded: TERMINFO / TERMINFO_DIRS (host paths, meaningless in the guest);
// TMUX, STY, ITERM_SESSION_ID, KITTY_LISTEN_ON (session handles for sockets not in the guest).
func terminalEnv() []string {
	var env []string
	for _, k := range []string{"TERM", "COLORTERM", "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "TERMINAL_EMULATOR"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	// NO_COLOR: the convention is "variable is set, value is irrelevant" — include it even
	// when empty so the guest respects the user's intent.
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		env = append(env, "NO_COLOR="+os.Getenv("NO_COLOR"))
	}
	return env
}

// attachTerminal prepares the host terminal for a process running with a TTY, and returns a
// function that puts it back.
//
// Two things are needed for an interactive shell in a sandbox to behave like a local one.
// The host terminal must be in raw mode, or the host line discipline echoes keystrokes and
// swallows Ctrl-C instead of passing it to the guest. And the guest's pty must be told the
// window size, or full-screen programs draw into an 80x24 corner and stay there when the
// window is resized.
//
// It is a no-op when there is no TTY, when stdin is not a terminal (a pipe, or a test), or
// on platforms without termios. The returned function is always safe to call.
func attachTerminal(ctx context.Context, p client.Process, tty bool, stdin io.Reader) func() {
	noop := func() {}
	if !tty {
		return noop
	}
	f, ok := stdin.(*os.File)
	if !ok {
		return noop
	}

	restore, err := makeRaw(f.Fd())
	if err != nil {
		// Not a terminal, or the platform has no termios: streaming still works,
		// it is just line-buffered and echoed. Not worth failing the command.
		return noop
	}

	resize := func() {
		width, height, err := terminalSize(f.Fd())
		// A zero size means the host terminal has none to report — under `script`,
		// or in a CI harness. Passing it on would leave the guest believing its
		// terminal is 0x0, which breaks anything that asks.
		if err != nil || width == 0 || height == 0 {
			return
		}
		_ = p.Resize(ctx, uint32(width), uint32(height))
	}
	resize()
	stopResize := watchResize(ctx, resize)

	return func() {
		stopResize()
		restore()
	}
}
