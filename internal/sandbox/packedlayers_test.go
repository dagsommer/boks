package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// The real message, from the report on 2026-08-20.
const packedLayerMsg = `failed to create shim task: mount source: "/dev/vdc4", ` +
	`target: "/run/bundles/team-copilot-default-boksTEST/mounts/4", fstype: erofs, ` +
	`flags: 1, data: "", err: invalid argument`

// The distinction the whole check rests on: a PARTITION means the packed path, a whole device
// means the ordinary one. Getting this backwards would attach a confident explanation about
// layer counts to a failure that has nothing to do with them.
func TestPackedLayerFailureIsRecognised(t *testing.T) {
	cfg := Config{Image: "example/big:1"}
	err := describePackedLayerFailure(cfg, packedLayerMsg, errors.New(packedLayerMsg))
	if err == nil {
		t.Fatal("the packed-layer failure was not recognised")
	}
	for _, want := range []string{"/dev/vdc4", "eight layers", "example/big:1", "libkrun"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the explanation is missing %q:\n%v", want, err)
		}
	}
	// The original message survives, because it is what a bug report needs.
	if !strings.Contains(err.Error(), "invalid argument") {
		t.Errorf("the underlying error was dropped:\n%v", err)
	}
}

// Everything that is NOT this failure must be left alone. A wrong match here would explain a
// layer-count problem to someone whose image is fine.
func TestPackedLayerFailureIgnoresEverythingElse(t *testing.T) {
	cfg := Config{Image: "example/small:1"}
	for _, msg := range []string{
		// A whole device: the ordinary one-disk-per-layer path, which is not this bug.
		`mount source: "/dev/vdc", target: "/run/x", fstype: erofs, flags: 1, err: invalid argument`,
		// An erofs failure with no device at all.
		"failed to create shim task: erofs is not supported",
		// A partition, but not erofs — a data volume, say.
		`mount source: "/dev/vdc4", target: "/run/x", fstype: ext4, flags: 0, err: invalid argument`,
		"executable file not found in $PATH",
		"",
	} {
		if err := describePackedLayerFailure(cfg, msg, errors.New(msg)); err != nil {
			t.Errorf("describePackedLayerFailure claimed %q:\n%v", msg, err)
		}
	}
}

// The hook must fire only when the guest is genuinely running, because `boks run` clears the
// terminal from it. Firing early — which two earlier placements did — wipes the evidence of
// whatever then goes wrong: a pull that is still running, a task that cannot start.
//
// This asserts the contract at the level a unit test can reach: that Config carries the hook
// and that it is not called by anything on the failure paths. A real end-to-end proof needs a
// hypervisor, which this machine does not have.
func TestOnGuestReadyIsNotCalledWhenTheTaskFails(t *testing.T) {
	called := false
	cfg := Config{
		Name:         "example",
		Image:        "example/image:1",
		OnGuestReady: func() { called = true },
	}

	// describeTaskError is on the path a failed task takes. Reaching it must not have
	// invoked the hook: the run never got a guest, so the screen must not be cleared.
	err := describeTaskError(cfg, errors.New(packedLayerMsg))
	if err == nil {
		t.Fatal("the packed-layer failure was not described")
	}
	if called {
		t.Error("OnGuestReady fired on a failure path; the terminal would be cleared over " +
			"the error the user needs to read")
	}
}

// Both spellings of the operating system's complaint, because the message is not containerd's.
// A regex that matched only Unix would leave Windows — where this was reported — unrepaired.
func TestStaleBundleIsRecognisedOnBothPlatforms(t *testing.T) {
	for _, msg := range []string{
		`mkdir C:\Users\E194604\AppData\Local\boks\containerd\state\io.containerd.runtime.v2.task\boks\x: Cannot create a file when that file already exists.`,
		`mkdir /home/u/.local/state/boks/containerd/state/io.containerd.runtime.v2.task/boks/x: file exists`,
	} {
		m := staleBundleDir.FindStringSubmatch(msg)
		if m == nil {
			t.Errorf("not recognised: %s", msg)
			continue
		}
		if !strings.Contains(m[1], "io.containerd.runtime.v2.task") {
			t.Errorf("captured %q, which is not the bundle directory", m[1])
		}
	}
}

// And nothing else. This function removes a directory, so a loose match would delete state
// belonging to a failure it does not understand.
func TestStaleBundleIgnoresOtherFailures(t *testing.T) {
	for _, msg := range []string{
		`mkdir /tmp/other: file exists`,
		"failed to create shim task: executable file not found",
		`mkdir /var/lib/io.containerd.runtime.v2.task/x: permission denied`,
		"",
	} {
		if m := staleBundleDir.FindStringSubmatch(msg); m != nil {
			t.Errorf("claimed %q, capturing %q", msg, m[1])
		}
	}
}
