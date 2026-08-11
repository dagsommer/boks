//go:build linux || darwin

package sandbox

import (
	"context"
	"os"
	"os/signal"

	"golang.org/x/sys/unix"
)

// makeRaw puts the terminal into raw mode and returns a function restoring the previous
// settings. The flag values are the conventional cfmakeraw(3) set: no input translation, no
// echo, no line buffering, and no signal generation, so every keystroke reaches the guest.
func makeRaw(fd uintptr) (func(), error) {
	previous, err := unix.IoctlGetTermios(int(fd), ioctlReadTermios)
	if err != nil {
		return nil, err
	}

	raw := *previous
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(int(fd), ioctlWriteTermios, &raw); err != nil {
		return nil, err
	}

	return func() { _ = unix.IoctlSetTermios(int(fd), ioctlWriteTermios, previous) }, nil
}

func terminalSize(fd uintptr) (width, height uint16, err error) {
	ws, err := unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	return ws.Col, ws.Row, nil
}

// watchResize calls onResize whenever the host terminal changes size.
func watchResize(ctx context.Context, onResize func()) func() {
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, unix.SIGWINCH)
	done := make(chan struct{})
	go func() {
		defer signal.Stop(sigC)
		for {
			select {
			case <-sigC:
				onResize()
			case <-ctx.Done():
				return
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}
