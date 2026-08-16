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

// TestStartAsksNoPlatformPermission is what is left of the change that let Windows try.
//
// There used to be a vmmSupported() gate at the top of Start, and on Windows it refused before
// anything was bound. Removing it must not have moved anything on the platforms where a guest
// had already been watched across this link, so this pins the two properties that would change
// if it had: the platform reports nothing to warn about, and Start binds a working socket in
// both modes.
//
// The Unexercised expectation is no longer guarded on runtime.GOOS. It was, while Windows was
// the one platform whose answer differed; a guest has since been watched across this link
// there too, with policy allowing one destination and refusing another (docs/verification.md,
// 2026-08-14 and 2026-08-15), so every platform now answers the same way and the test says so
// without a branch. A branch here would be the last place the retired claim could hide.
func TestStartAsksNoPlatformPermission(t *testing.T) {
	if err := Unexercised(); err != nil {
		t.Errorf("Unexercised() on %s = %v; a guest has been watched crossing this link "+
			"on every platform Boks runs on, so nothing should be reported here", runtime.GOOS, err)
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

// TestAnUnexercisedNoteNamesALimitAndItsConsequence constrains the *shape* of whatever this
// package says when a platform's link has not been shown to carry frames — not the sentence.
//
// This replaces a test that asserted the literal string "no Ethernet frame has ever", which is
// how a retired claim survived being retired: the sentence was false from 2026-08-14, and both
// ways of correcting the code — returning nil, or rewording — failed the build. A test that
// pins prose makes the prose unfixable, so this pins the two properties that actually matter
// and nothing else.
//
// It is vacuous today, deliberately. No platform returns non-nil, so the loop body does not
// run. It exists for the moment one does again — a new host platform, or a regression — and it
// is the assertion that would have survived the correction it used to block.
func TestAnUnexercisedNoteNamesALimitAndItsConsequence(t *testing.T) {
	err := Unexercised()
	if err == nil {
		return
	}
	text := err.Error()

	// A limit: the note has to say what has not been shown, on this platform.
	if !strings.Contains(strings.ToLower(text), runtime.GOOS) {
		t.Errorf("the note does not say which platform it is about (%s):\n%s", runtime.GOOS, text)
	}
	// And its consequence, which is the half a reader can act on. The failure that looks
	// like success is a guest left on the runtime's own transport, so the note is useless
	// unless it names that outcome and points somewhere it is written up.
	if !strings.Contains(text, "TSI") {
		t.Errorf("the note names no consequence: a guest left on the runtime's own "+
			"transport is the failure that looks like success, and it has to be said:\n%s", text)
	}
	if !strings.Contains(text, "docs/") {
		t.Errorf("the note points at no document, so a reader cannot check it:\n%s", text)
	}
	// It must not read as a refusal: nothing consults it before binding, and a sentence
	// that says "not available" would be describing code that was deleted in 2026-08.
	for _, unwanted := range []string{"not available", "is not supported"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("the note reads as a refusal (%q):\n%s", unwanted, text)
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
