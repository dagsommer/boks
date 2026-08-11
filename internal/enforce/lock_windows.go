package enforce

import (
	"errors"
	"os/exec"
)

// Windows has no sandbox networking to supervise: the VM runtime does not support Windows,
// and the link is a SOCK_DGRAM UNIX socket that gvisor-tap-vsock does not implement there.
// These exist so the package compiles, and say so rather than pretending.

var errUnsupported = errors.New("enforce: sandbox networking is not available on Windows")

func acquire(string) (func(), error) { return nil, errUnsupported }

func locked(string) bool { return false }

func terminate(int) error { return errUnsupported }

func detach(*exec.Cmd) {}
