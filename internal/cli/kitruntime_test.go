package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/kit"
	"github.com/dagsommer/boks/internal/sandbox"
)

func TestKitPortsBecomeEphemeralPublishSpecs(t *testing.T) {
	spec := &kit.Spec{Name: "k", Ports: []kit.Port{
		{Container: 8080}, {Container: 8080, Protocol: "tcp"}, {Container: 53, Protocol: "udp"}, {Container: 3000},
	}}
	var errOut bytes.Buffer
	got := kitPublish(spec, &errOut)
	if strings.Join(got, ",") != "8080,3000" {
		t.Errorf("specs = %v", got)
	}
	if err := checkPublishSpecs(got); err != nil {
		t.Errorf("not valid --publish specs: %v", err)
	}
	if !strings.Contains(errOut.String(), "53/udp") || !strings.Contains(errOut.String(), "publishing sandbox port 8080") {
		t.Errorf("stderr:\n%s", errOut.String())
	}
}

func TestKitVolumesTranslate(t *testing.T) {
	spec := &kit.Spec{Name: "k", Volumes: []kit.Volume{
		{Path: "/data", Size: "1g"},
		{Path: "/ram", Type: kit.MountTypeTmpfs, Size: "64m", Mode: "1777"},
	}}
	vols, notes, err := kitVolumes(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []sandbox.Volume{{Path: "/data", Size: 1 << 30}, {Path: "/ram", Tmpfs: true, Size: 64 << 20, Mode: 0o1777}}
	if len(vols) != 2 || vols[0] != want[0] || vols[1] != want[1] {
		t.Errorf("volumes = %+v", vols)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "/data") {
		t.Errorf("notes = %v", notes)
	}
}

func TestKitVolumesOnlyAtCreation(t *testing.T) {
	spec := &kit.Spec{Name: "k", Volumes: []kit.Volume{{Path: "/data"}}}

	var errOut bytes.Buffer
	cfg := sandbox.Config{Name: "dev"}
	if err := applyKitVolumes(spec, true, &cfg, &errOut); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Volumes) != 0 || !strings.Contains(errOut.String(), "only when a sandbox is created") {
		t.Errorf("existing sandbox: volumes=%v stderr=%q", cfg.Volumes, errOut.String())
	}

	cfg = sandbox.Config{Name: "dev"}
	if err := applyKitVolumes(spec, false, &cfg, &errOut); err != nil || len(cfg.Volumes) != 1 {
		t.Errorf("new sandbox: volumes=%v err=%v", cfg.Volumes, err)
	}
}
