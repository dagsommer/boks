//go:build !linux && !darwin

package sandbox

import (
	"context"
	"errors"
)

// Windows consoles need a different mechanism entirely (SetConsoleMode), and Boks does not
// run sandboxes there yet. Interactive output still works; it is merely cooked.
var errNoTerminalControl = errors.New("terminal control is not implemented on this platform")

func makeRaw(fd uintptr) (func(), error) { return nil, errNoTerminalControl }

func terminalSize(fd uintptr) (width, height uint16, err error) { return 0, 0, errNoTerminalControl }

func watchResize(ctx context.Context, onResize func()) func() { return func() {} }
