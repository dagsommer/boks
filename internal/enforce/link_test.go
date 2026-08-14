package enforce

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unexercised stands in for network.Unexercised()'s Windows answer.
//
// The whole point of watchLink taking that error as a parameter is that the armed behaviour
// can be built here, on Linux, rather than only existing on a platform the tests never run on.
var unexercised = errors.New("no Ethernet frame has ever crossed this device on this platform")

// TestTheLinkWatchdogIsInertWhereTheLinkIsExercised is the proof that this change does nothing
// on Unix.
//
// Everything the watchdog can do is reached through two channels, and where the platform has
// been exercised both are nil: a receive on a nil channel blocks forever, so the supervisor's
// select can never take that arm, and no goroutine, timer or wait exists at all. If arming
// ever became unconditional — the one mistake that would change Unix behaviour — this test
// fails, because failure() would produce an error for a sandbox whose VM simply had not
// dialled yet.
func TestTheLinkWatchdogIsInertWhereTheLinkIsExercised(t *testing.T) {
	never := make(chan struct{}) // nothing ever connects
	w := watchLink(context.Background(), nil, "box", "/run/boks/net.sock", never, time.Millisecond, io.Discard)

	if w.failure() != nil {
		t.Fatal("the watchdog is armed on a platform where a guest has been watched on this link")
	}
	w.taskStarted() // must be safe, and must arm nothing
	w.taskStarted()

	select {
	case err := <-w.failure():
		t.Fatalf("an inert watchdog reported %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestTheLinkWatchdogFailsWhenNothingDials is the Windows case, constructed rather than
// depended on: the sandbox's task is running and nothing ever connects to the link socket.
func TestTheLinkWatchdogFailsWhenNothingDials(t *testing.T) {
	never := make(chan struct{})
	w := watchLink(context.Background(), unexercised, "agentbox", "/run/boks/net.sock",
		never, 10*time.Millisecond, io.Discard)
	w.taskStarted()

	select {
	case err := <-w.failure():
		text := err.Error()
		// Everything a person needs in order to act, named in the message rather than
		// left in a file: what did not happen, and the three things to check.
		for _, want := range []string{
			"agentbox",
			"/run/boks/net.sock",
			"nothing connected",
			"not being enforced",
			unexercised.Error(),
			"containerd-shim-nerdbox-v1",
			"TSI",
			"--features blk,net",
			"krun_add_net_unixstream",
			"boks doctor",
			"boks stop agentbox",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("the failure does not mention %q:\n%s", want, text)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog never reported that nothing had connected")
	}
}

// TestTheLinkWatchdogIsSilentWhenTheGuestAttaches: a VM that dials is the whole point, and it
// must not be accused afterwards.
func TestTheLinkWatchdogIsSilentWhenTheGuestAttaches(t *testing.T) {
	connected := make(chan struct{})
	var log strings.Builder
	w := watchLink(context.Background(), unexercised, "box", "/run/boks/net.sock",
		connected, 10*time.Millisecond, &log)
	w.taskStarted()
	close(connected)

	select {
	case err := <-w.failure():
		t.Fatalf("a link with a peer on it reported %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if !strings.Contains(log.String(), "attached to the link socket") {
		t.Errorf("the attach was not recorded in the operational log: %q", log.String())
	}
}

// TestTheLinkWatchdogWaitsForTheTaskBeforeItTimesAnything pins the reason the clock starts
// where it does.
//
// The network is started *before* the container is created — the VM connects while it boots —
// so the gap between "the socket is bound" and "a VM could possibly dial" contains an image
// pull of unknown length. A watchdog timing that gap would accuse every slow pull.
func TestTheLinkWatchdogWaitsForTheTaskBeforeItTimesAnything(t *testing.T) {
	never := make(chan struct{})
	w := watchLink(context.Background(), unexercised, "box", "/run/boks/net.sock",
		never, time.Millisecond, io.Discard)

	select {
	case err := <-w.failure():
		t.Fatalf("the watchdog timed the wait before the task was running: %v", err)
	case <-time.After(100 * time.Millisecond): // a hundred grace periods
	}
}

// TestTheLinkWatchdogStopsWithTheSupervisor: a cancelled context ends the wait, so a
// supervisor being torn down does not accuse a sandbox on the way out.
func TestTheLinkWatchdogStopsWithTheSupervisor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	never := make(chan struct{})
	w := watchLink(ctx, unexercised, "box", "/run/boks/net.sock", never, 20*time.Millisecond, io.Discard)
	w.taskStarted()
	cancel()

	select {
	case err := <-w.failure():
		t.Fatalf("a cancelled watchdog reported %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestLogTailQuotesTheEndOfTheLog covers the other half of "the failure must be legible": the
// spawn error used to say "see stack.log" and leave the reason in a file.
func TestLogTailQuotesTheEndOfTheLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, logFile)
	var content strings.Builder
	for i := 0; i < 200; i++ {
		content.WriteString("line ")
		content.WriteString(strings.Repeat("x", 60))
		content.WriteString("\n")
	}
	content.WriteString("network: binding the link socket: permission denied\n")
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tail := logTail(path)
	if !strings.Contains(tail, "permission denied") {
		t.Errorf("the tail does not carry the last line:\n%s", tail)
	}
	if !strings.Contains(tail, path) {
		t.Errorf("the tail does not name the log it came from:\n%s", tail)
	}
	// Bounded: a supervisor that failed in an unknown way must not be able to make an
	// error message unreadable.
	if lines := strings.Count(tail, "\n"); lines > 9 {
		t.Errorf("the tail quoted %d lines; it is supposed to be bounded:\n%s", lines, tail)
	}
}

func TestLogTailSaysSoWhenThereIsNothingToQuote(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, logFile)
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if tail := logTail(empty); !strings.Contains(tail, "wrote nothing") || !strings.Contains(tail, empty) {
		t.Errorf("an empty log produced %q", tail)
	}
	missing := filepath.Join(dir, "absent.log")
	if tail := logTail(missing); !strings.Contains(tail, missing) {
		t.Errorf("a missing log produced %q, which does not name it", tail)
	}
}
