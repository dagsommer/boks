//go:build !darwin && !linux && !windows

package secret

// Every operating system that is not macOS, Linux or Windows: no OS secret store Boks knows
// how to drive.
//
// This is a refusal and not a stub, and the difference matters to the caller. ErrNoKeyring is
// the one error that means "use the passphrase-encrypted file and say why" — a store that
// pretended to work here, or a nil Keyring returned with a nil error, would fail later and
// somewhere less obvious. FreeBSD, OpenBSD and the rest reach this file; there is no reason
// they could not have a backend, and adding one is adding a file next to this one rather than
// changing anything that calls it.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
)

// openKeyring implements OpenKeyring.
func openKeyring(context.Context) (Keyring, error) {
	return nil, keyringUnavailable(fmt.Sprintf("Boks has no OS secret store backend for %s", runtime.GOOS), nil)
}
