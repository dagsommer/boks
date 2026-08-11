package sandbox

import (
	"context"
	"io"
	"os"

	"github.com/containerd/containerd/v2/client"
)

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
