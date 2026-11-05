package sandbox

import (
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/workspace"
)

func TestKitRecordRoundTripsThroughTheLabel(t *testing.T) {
	rec := &KitRecord{
		V:      KitRecordVersion,
		Refs:   []string{"./team-kit", "git+https://host/org/kits.git#ref=v1&dir=a"},
		Pins:   map[string]string{"./team-kit": "/work/team-kit", "git+https://host/org/kits.git#ref=v1&dir=a": "git+https://host/org/kits.git#dir=a&ref=0123456789abcdef0123456789abcdef01234567"},
		Digest: "abc123",
	}
	labels, err := containerLabels(Config{Kits: rec, Workspaces: []workspace.Workspace{{HostPath: "/w", GuestPath: "/w"}}}, []string{"sh"})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeKits(labels)
	if got == nil || len(got.Refs) != 2 || got.Digest != "abc123" {
		t.Fatalf("decoded %+v", got)
	}
	if got.Target("./team-kit") != "/work/team-kit" {
		t.Errorf("Target of a pinned ref = %q", got.Target("./team-kit"))
	}
	if got.Target("./other") != "./other" {
		t.Errorf("an unpinned ref must be read as it stands")
	}
}

func TestNoKitsWritesNoLabel(t *testing.T) {
	labels, err := containerLabels(Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := labels[LabelKits]; ok {
		t.Errorf("a sandbox with no kit carries a %s label", LabelKits)
	}
	if decodeKits(labels) != nil || decodeKits(map[string]string{LabelKits: "{not json"}) != nil {
		t.Errorf("an absent or unreadable record must decode to nothing")
	}
}

func TestKitsLabelTooLargeIsAnErrorNotATruncation(t *testing.T) {
	rec := &KitRecord{V: 1, Refs: []string{strings.Repeat("x", maxLabelBytes)}}
	if _, err := encodeKitsLabel(rec); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("err = %v, want a refusal rather than a label containerd would reject", err)
	}
}

func TestSetupConfigIsRebuiltFromTheRecordedWorkspace(t *testing.T) {
	fs, err := encodeLabel(Filesystem{Mode: "clone", Clone: "/guest/p"})
	if err != nil {
		t.Fatal(err)
	}
	labels, err := containerLabels(Config{
		Workspaces: []workspace.Workspace{{HostPath: "/host/p", GuestPath: "/guest/p"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := setupConfig(labels, "box", &Setup{}, nil)
	if len(cfg.Workspaces) != 1 || cfg.Workspaces[0].Root() != "/guest/p" || cfg.Clone {
		t.Errorf("direct: %+v", cfg)
	}
	labels[LabelFilesystem] = fs
	if cfg := setupConfig(labels, "box", &Setup{}, nil); !cfg.Clone {
		t.Errorf("a clone-mode sandbox must keep setup.files out of its clone: %+v", cfg)
	}
}
