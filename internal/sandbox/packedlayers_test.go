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
	// No layer count is asserted, because the message no longer states one: the threshold is
	// 20 where the interrupt map is not the constraint and 10 on x86-64 where it is, so any
	// single number in this text would be wrong on some platform. The advice used to say
	// "eight", which told a user with a 17-layer image to squash it when the real answer was
	// that their shim predated the threshold entirely.
	for _, want := range []string{"/dev/vdc4", "interrupt lines", "example/big:1", "boks doctor"} {
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
	err := describeTaskError(cfg, 0, errors.New(packedLayerMsg))
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

// The real message, from Windows on 2026-08-25, on a 17-layer image with a shim that had just
// stopped packing it.
const krunEINVALMsg = "failed to create shim task: failure running vm: krun_start_enter failed: -22"

// The explanation has to carry the arithmetic, because the arithmetic is the only part a user
// can act on: their layer count against this platform's line count.
func TestDeviceBudgetFailureCountsTheLayers(t *testing.T) {
	cfg := Config{Name: "s", Image: "example/deep:1"}
	err := describeDeviceBudgetFailure(cfg, 17, "amd64", krunEINVALMsg, errors.New(krunEINVALMsg))
	if err == nil {
		t.Fatal("a 17-layer image over a 19-device budget was not recognised")
	}
	for _, want := range []string{"17 layers", "24 devices", "19", "12 layers", "example/deep:1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the explanation is missing %q:\n%v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "-22") {
		t.Errorf("the underlying error was dropped:\n%v", err)
	}
}

// Quiet unless the arithmetic actually supports the story. EINVAL from a VMM has many causes,
// and an image that fits the budget failing to start is one of them — explaining that one with
// interrupt lines would send someone rebuilding an image for no reason.
func TestDeviceBudgetFailureStaysQuietWhenItCannotKnow(t *testing.T) {
	cfg := Config{Name: "s", Image: "example/small:1"}
	cases := []struct {
		why    string
		layers int
		goarch string
		msg    string
	}{
		{"the image fits the budget", 6, "amd64", krunEINVALMsg},
		{"the layer count is unknown", 0, "amd64", krunEINVALMsg},
		{"a different failure entirely", 17, "amd64", "failed to create shim task: ttrpc: closed"},
		{"a different krun error", 17, "amd64", "failure running vm: krun_start_enter failed: -1"},
		// The same 17-layer image on arm64, where the interrupt controller has no such
		// ceiling. This is the case that must stay silent for a reason other than
		// arithmetic, and it is the one the reporter can reach by switching machines.
		{"arm64, where this ceiling does not exist", 17, "arm64", krunEINVALMsg},
	}
	for _, c := range cases {
		if err := describeDeviceBudgetFailure(cfg, c.layers, c.goarch, c.msg, errors.New(c.msg)); err != nil {
			t.Errorf("explained a failure it should have left alone (%s):\n%v", c.why, err)
		}
	}
}
