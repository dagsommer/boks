package daemon

import (
	"strings"
	"testing"
)

// The order is the whole point of the file, so it is asserted per platform rather than
// through whichever one the tests happen to run on.
func TestDiffOrderPutsEROFSFirst(t *testing.T) {
	for _, tc := range []struct {
		goos  string
		erofs bool
		want  []string
	}{
		{"linux", true, []string{"erofs", "walking"}},
		{"darwin", true, []string{"erofs", "walking"}},
		{"windows", true, []string{"erofs", "windows", "windows-lcow"}},
		{"linux", false, []string{"walking"}},
		{"windows", false, []string{"windows", "windows-lcow"}},
	} {
		got := diffOrder(tc.goos, tc.erofs)
		if len(got) != len(tc.want) {
			t.Fatalf("diffOrder(%q, %v) = %v, want %v", tc.goos, tc.erofs, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("diffOrder(%q, %v) = %v, want %v", tc.goos, tc.erofs, got, tc.want)
			}
		}
	}
}

// A differ that is not first is a differ that is never reached, because the ones after it fail
// hard rather than declining. This is the assertion that would catch someone "tidying" the
// order into something alphabetical.
func TestEROFSIsNeverAnywhereButFirst(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		order := diffOrder(goos, true)
		if order[0] != "erofs" {
			t.Errorf("diffOrder(%q, true) = %v; erofs must be first or it is never asked", goos, order)
		}
	}
}

// Measured 2026-08-15 on containerd v2.2.6/linux: naming erofs when mkfs.erofs is absent fails
// the diff service and six other plugins, and the daemon keeps running with no way to unpack an
// image. Omitting it is the only safe answer, so nothing may put it back.
func TestEROFSIsOmittedWithoutMkfs(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, name := range diffOrder(goos, false) {
			if name == "erofs" {
				t.Errorf("diffOrder(%q, false) names erofs; without mkfs.erofs that fails the diff service", goos)
			}
		}
	}
}

func unixSettings() Settings {
	uid, gid := 501, 20
	return Settings{
		GOOS: "linux", GOARCH: "amd64",
		Root:         "/home/u/.local/state/boks/containerd/root",
		State:        "/home/u/.local/state/boks/containerd/state",
		Address:      "/home/u/.local/state/boks/containerd/containerd.sock",
		TTRPCAddress: "/home/u/.local/state/boks/containerd/containerd.sock.ttrpc",
		UID:          &uid, GID: &gid, EROFS: true,
	}
}

// The ttrpc uid and gid are the setting a rootless daemon dies without, and containerd only
// copies [grpc]'s across when the [ttrpc] section is absent entirely. So both must be written,
// on both sections.
func TestRenderWritesOwnershipOnBothListeners(t *testing.T) {
	out, err := render(unixSettings())
	if err != nil {
		t.Fatal(err)
	}
	grpc, ttrpc, found := strings.Cut(out, "[ttrpc]")
	if !found {
		t.Fatal("no [ttrpc] section; a rootless daemon dies on chown of the ttrpc socket")
	}
	for name, section := range map[string]string{"grpc": grpc, "ttrpc": ttrpc} {
		if !strings.Contains(section, "uid = 501") || !strings.Contains(section, "gid = 20") {
			t.Errorf("[%s] section has no uid/gid:\n%s", name, section)
		}
	}
}

func TestRenderWritesTheDiffOrder(t *testing.T) {
	out, err := render(unixSettings())
	if err != nil {
		t.Fatal(err)
	}
	want := "default = ['erofs', 'walking']"
	if !strings.Contains(out, want) {
		t.Errorf("rendered config does not contain %q:\n%s", want, out)
	}
}

// Setting unpack_config replaces containerd's built-in list rather than extending it, and the
// `optional` key that would make an erofs entry survive a host without mkfs.erofs exists only
// in this project's patched containerd. Boks does not use that path at all, so it must stay out
// of the file.
func TestRenderLeavesUnpackConfigAlone(t *testing.T) {
	out, err := render(unixSettings())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "[[plugins.'io.containerd.transfer.v1.local'.unpack_config]]") {
		t.Error("rendered config sets unpack_config, which replaces containerd's default list")
	}
}

// Windows needs cimfs off, or one snapshotter failing at init takes about forty plugins with
// it — and none of the resulting errors mention cimfs.
func TestRenderDisablesCimfsOnWindowsOnly(t *testing.T) {
	win := unixSettings()
	win.GOOS = "windows"
	win.UID, win.GID, win.TTRPCAddress = nil, nil, ""
	win.Address = `\\.\pipe\boks-containerd`
	out, err := render(win)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"io.containerd.snapshotter.v1.cimfs",
		"io.containerd.differ.v1.cimfs",
		"default = ['erofs', 'windows', 'windows-lcow']",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Windows config does not contain %q", want)
		}
	}
	// A named pipe is not chowned and Windows has no uid to chown it to.
	if strings.Contains(out, "uid =") {
		t.Error("Windows config sets a uid, which means nothing there")
	}

	unix, err := render(unixSettings())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(unix, "cimfs") {
		t.Error("the Unix config disables cimfs, a plugin that does not exist there")
	}
}

// A single quote cannot appear in a TOML literal string, so a path containing one has to be
// refused here rather than producing a file containerd rejects with a parse error.
func TestRenderRefusesAQuotedPath(t *testing.T) {
	s := unixSettings()
	s.Root = "/home/o'brien/.local/state/boks/containerd/root"
	if _, err := render(s); err == nil {
		t.Fatal("render accepted a path containing a single quote")
	}
}

// `boks daemon config` is meant to be diffed against the file a daemon is running with, which
// only works if the same input renders the same bytes.
func TestRenderIsDeterministic(t *testing.T) {
	first, err := render(unixSettings())
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		again, err := render(unixSettings())
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatal("render is not deterministic")
		}
	}
}

// The generated file is what somebody reads when they are debugging, so the reason a setting
// exists has to be in it — not only in the source that produced it.
func TestRenderExplainsWhatItWroteAndWhy(t *testing.T) {
	out, err := render(unixSettings())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"operation not permitted", // the ttrpc chown failure
		"ErrNotImplemented",       // why erofs is first
		"generated by boks",       // that editing it is pointless
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered config never explains %q", want)
		}
	}
}

// Without mkfs.erofs the file must say so, because the daemon it configures will start
// cleanly and then be unable to unpack anything.
func TestRenderSaysWhyEROFSIsMissing(t *testing.T) {
	s := unixSettings()
	s.EROFS = false
	out, err := render(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "mkfs.erofs was NOT found") {
		t.Error("a config written without erofs does not say why")
	}
	if strings.Contains(out, "'erofs'") {
		t.Error("a config written without mkfs.erofs still names the erofs differ")
	}
}

// The writable layer must not be 64 MiB.
//
// Off Linux every active snapshot gets its own ext4 image, sized from containerd's
// defaultWritableSize, which is 64 MiB — a floor, not a working size. A sandbox that installs
// anything fills it, and the failure surfaces inside the guest as ENOSPC from whatever was
// writing, naming a path in the container and nothing about Boks. Reported that way: an agent
// unpacking its own runtime, out of space partway through a tar.
func TestWritableLayerSizeIsUsable(t *testing.T) {
	for _, goos := range []string{"darwin", "windows"} {
		out, err := render(Settings{GOOS: goos, EROFS: true, Ext4: true})
		if err != nil {
			t.Fatalf("render(%s): %v", goos, err)
		}
		if !strings.Contains(out, "io.containerd.snapshotter.v1.erofs") {
			t.Errorf("%s: no erofs snapshotter section, so the layer keeps the 64 MiB default:\n%s", goos, out)
		}
		if !strings.Contains(out, `default_size = "16GiB"`) {
			t.Errorf("%s: default_size is not set to the usable value:\n%s", goos, out)
		}
	}
}

// On Linux the writable layer is a plain directory bounded by the real filesystem, because
// defaultWritableSize is 0 there. Writing default_size would switch it into block mode — a
// behaviour change nobody asked for, and one that would put every Linux sandbox behind an ext4
// image it does not need.
func TestWritableLayerSizeIsNotSetOnLinux(t *testing.T) {
	out, err := render(Settings{GOOS: "linux", EROFS: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "default_size") {
		t.Errorf("linux got a default_size, which turns on block mode:\n%s", out)
	}
}

// The override, because 16 GiB is a guess about someone else's workload and the sandbox that
// needs 200 GiB should not need a new Boks.
func TestWritableLayerSizeCanBeOverridden(t *testing.T) {
	t.Setenv(WritableLayerSizeEnv, "200GiB")
	out, err := render(Settings{GOOS: "darwin", EROFS: true, Ext4: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `default_size = "200GiB"`) {
		t.Errorf("%s was ignored:\n%s", WritableLayerSizeEnv, out)
	}
}
