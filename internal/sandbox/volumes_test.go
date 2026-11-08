package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestVolumesBecomeMountsInTheSpec(t *testing.T) {
	state := t.TempDir()
	t.Setenv("BOKS_STATE_DIR", state)
	cfg := Config{Name: "dev", Volumes: []Volume{
		{Path: "/var/cache/tool"},
		{Path: "/tmp/scratch", Tmpfs: true, Size: 64 << 20, Mode: 0o1777},
	}}
	s := generatedSpec(t, cfg, nil, nil)

	var block, tmp *specs.Mount
	for i := range s.Mounts {
		switch s.Mounts[i].Destination {
		case "/var/cache/tool":
			block = &s.Mounts[i]
		case "/tmp/scratch":
			tmp = &s.Mounts[i]
		}
	}
	if block == nil || block.Type != "bind" || block.Source != VolumeDir(state, "dev", "/var/cache/tool") ||
		!slices.Contains(block.Options, "rw") {
		t.Errorf("block volume mount = %+v", block)
	}
	if tmp == nil || tmp.Type != "tmpfs" ||
		!slices.Contains(tmp.Options, "size=67108864") || !slices.Contains(tmp.Options, "mode=1777") {
		t.Errorf("tmpfs mount = %+v", tmp)
	}
}

func TestBlockVolumesPersistUntilRemoved(t *testing.T) {
	state := t.TempDir()
	t.Setenv("BOKS_STATE_DIR", state)
	cfg := Config{Name: "dev", Volumes: []Volume{{Path: "/data", Mode: 0o700}, {Path: "/ram", Tmpfs: true}}}
	if err := prepareVolumes(cfg); err != nil {
		t.Fatal(err)
	}
	dir := VolumeDir(state, "dev", "/data")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700) {
		t.Fatalf("volume dir: %v %v", fi, err)
	}
	// A second creation attempt (or restart) keeps what is there.
	marker := filepath.Join(dir, "keep")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareVolumes(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("data lost: %v", err)
	}
	if err := removeVolumes(state, "dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "volumes", "dev")); !os.IsNotExist(err) {
		t.Errorf("volumes survived removal: %v", err)
	}
	if err := removeVolumes(state, "../x"); err != nil {
		t.Errorf("hostile name: %v", err)
	}
}
