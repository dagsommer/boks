package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// The real message, from the report on 2026-08-20.
const packedLayerMsg = `failed to create shim task: mount source: "/dev/vdc4", ` +
	`target: "/run/bundles/udi-copilot-default-boksTEST/mounts/4", fstype: erofs, ` +
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
