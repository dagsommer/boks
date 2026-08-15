package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/update"
)

// Telling a user that a newer Boks exists.
//
// The mechanism, the privacy argument and the guarantee that this never delays or fails a
// command are all in internal/update. What lives here is the wording and where it appears.
//
// It appears on `boks run` and `boks create` — the commands that start something — and
// nowhere else. Not on `ls`, not on `inspect`, not on `doctor`: a notice attached to every
// command is a notice people learn to skip, which is the same argument notice.go makes about
// the security text, and the habit of skipping transfers between them.

const updateDisclosure = `boks checks once a day whether a newer version has been released, by asking GitHub
        which release is newest. It sends nothing about you or what you run — no version,
        no identifier — and the comparison happens on this machine. Turn it off with
        BOKS_NO_UPDATE_CHECK=1 (DO_NOT_TRACK=1 is honoured too, and CI is skipped).
        No check was made on this run.
`

// noticeUpdate prints whatever the update check has to say, and starts the next check.
//
// Errors are impossible by construction: internal/update resolves every failure to silence.
// The returned channel is for tests; callers ignore it, because waiting for the background
// refresh is precisely what this must not do.
func noticeUpdate(stderr io.Writer) <-chan struct{} {
	exe, err := os.Executable()
	if err != nil {
		// Not knowing the path costs the specific upgrade command, not the notice.
		exe = ""
	}
	notice, done := update.Notify(update.Config{
		StateDir: policy.StateDir(),
		Current:  Version,
		ExePath:  exe,
	})
	if notice == nil {
		return done
	}
	if notice.Disclosure {
		fmt.Fprintf(stderr, "note:   %s\n", updateDisclosure)
		return done
	}
	// One line. Someone in the middle of starting a sandbox is not reading a changelog,
	// and the two facts they need are that there is a newer version and what to type.
	fmt.Fprintf(stderr, "update: boks %s is available (you have %s) — %s\n",
		notice.Latest, Version, notice.Upgrade)
	return done
}
