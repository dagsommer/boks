package network

import (
	"context"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestStartAsksNoPlatformPermission is the Unix half of the change that let Windows try.
//
// There used to be a vmmSupported() gate at the top of Start, and on Windows it refused before
// anything was bound. Removing it must not have moved anything on the platforms where a guest
// has actually been watched across this link, so this pins the two properties that would
// change if it had: the platform reports nothing to warn about, and Start binds a working
// socket in both modes.
//
// The Unexercised expectation is guarded on runtime.GOOS rather than skipped, because it is
// the one assertion whose *answer* differs by platform while the behaviour does not: Start
// binds everywhere, and only the warning attached to it is Windows-specific.
func TestStartAsksNoPlatformPermission(t *testing.T) {
	switch err := Unexercised(); runtime.GOOS {
	case "windows":
		if err == nil {
			t.Error("Unexercised() on windows returned nil; no frame has crossed this " +
				"device there, and something has to say so")
		}
	default:
		if err != nil {
			t.Errorf("Unexercised() on %s = %v; a platform where a guest has been watched "+
				"crossing this link must report nothing to warn about", runtime.GOOS, err)
		}
	}
	for _, mode := range Modes() {
		t.Run(string(mode), func(t *testing.T) {
			n, err := New(testConfig(t, mode))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer n.Stop()
			if err := n.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if _, err := os.Stat(n.Plan().Socket); err != nil {
				t.Fatalf("the link socket does not exist after Start: %v", err)
			}
		})
	}
}

// TestTheWindowsNoteNamesWhatIsUnproven reads the text Unexercised returns on Windows.
//
// The build it belongs to is compiled in CI and executed nowhere, so this is the only place
// the claim is checked at all. What matters is that it says the frame path is unexercised
// rather than impossible, and that it names the consequence of nothing connecting — a guest on
// TSI, which is the failure that looks like success.
func TestTheWindowsNoteNamesWhatIsUnproven(t *testing.T) {
	err := unexercisedOnWindows()
	if err == nil {
		t.Fatal("unexercisedOnWindows() returned nil")
	}
	for _, want := range []string{"no Ethernet frame has ever", "Windows", "TSI", "docs/windows.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the Windows note does not mention %q:\n%v", want, err)
		}
	}
	// It must not read as a refusal: nothing consults it before binding any more, and a
	// sentence that says "not available" would be describing the code it replaced.
	for _, unwanted := range []string{"not available", "is not supported"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("the Windows note still reads as a refusal (%q):\n%v", unwanted, err)
		}
	}
}

// TestTheLinkReportsWhetherAPeerEverAttached covers the signal the supervisor's bounded wait
// is built on: nothing else in Boks can tell "the VM has not booted yet" from "nothing will
// ever connect to this socket".
//
// Both modes carry it, because -net none also binds a link socket — the VM's NIC has to have
// somewhere to write — and a sandbox with no network is exactly as able to be silently left on
// the runtime's own transport as one with a stack.
func TestTheLinkReportsWhetherAPeerEverAttached(t *testing.T) {
	for _, mode := range Modes() {
		t.Run(string(mode), func(t *testing.T) {
			n, err := New(testConfig(t, mode))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer n.Stop()
			if err := n.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}

			select {
			case <-n.Connected():
				t.Fatal("the link reported a peer before anything connected")
			default:
			}

			conn, err := net.Dial("unix", n.Plan().Socket)
			if err != nil {
				t.Fatalf("dialling the link socket: %v", err)
			}
			select {
			case <-n.Connected():
			case <-time.After(5 * time.Second):
				t.Fatal("the link never reported the peer that connected to it")
			}

			// Latched, not live: a VMM that restarts disconnects and reconnects, and a
			// signal that flapped with it would turn every restart into an alarm.
			conn.Close()
			time.Sleep(50 * time.Millisecond)
			select {
			case <-n.Connected():
			default:
				t.Error("the link forgot that a peer had attached once it disconnected")
			}
		})
	}
}
