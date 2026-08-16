package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The version rules are exercised through a probe the test supplies, so none of this depends
// on which erofs-utils the machine running it has — including the interesting cases, which
// are the versions too old to install on purpose.

// staticProbe returns a probe that always prints out and returns err.
func staticProbe(out string, err error) versionProbe {
	return func(context.Context, string, ...string) (string, error) { return out, err }
}

func TestParseToolVersion(t *testing.T) {
	tests := []struct {
		name       string
		out        string
		wantMajor  int
		wantMinor  int
		wantText   string
		wantParsed bool
	}{
		{
			// Observed: Debian/Ubuntu's erofs-utils package, run on the machine
			// these tests were written on. No leading v, and a second line.
			name: "the packaged spelling",
			out: "mkfs.erofs (erofs-utils) 1.9\n" +
				"available compressors: lz4, lz4hc, lzma, deflate, libdeflate, zstd\n",
			wantMajor: 1, wantMinor: 9, wantText: "1.9", wantParsed: true,
		},
		{
			name:      "the tagged spelling, with a leading v and a patch level",
			out:       "mkfs.erofs (erofs-utils) v1.9.1\n",
			wantMajor: 1, wantMinor: 9, wantText: "v1.9.1", wantParsed: true,
		},
		{
			name:      "the version Ubuntu 24.04 LTS ships",
			out:       "mkfs.erofs (erofs-utils) 1.7.1\n",
			wantMajor: 1, wantMinor: 7, wantText: "1.7.1", wantParsed: true,
		},
		{
			name:      "exactly the minimum",
			out:       "mkfs.erofs (erofs-utils) 1.8\n",
			wantMajor: 1, wantMinor: 8, wantText: "1.8", wantParsed: true,
		},
		{
			name:      "a release candidate",
			out:       "mkfs.erofs (erofs-utils) v2.0-rc1\n",
			wantMajor: 2, wantMinor: 0, wantText: "v2.0-rc1", wantParsed: true,
		},
		{
			name:      "a git description",
			out:       "mkfs.erofs (erofs-utils) 1.8.1-g0f2c3d4\n",
			wantMajor: 1, wantMinor: 8, wantText: "1.8.1-g0f2c3d4", wantParsed: true,
		},
		{
			name:      "trailing parenthetical",
			out:       "mkfs.erofs (erofs-utils) 1.9 (compiled with libselinux)\n",
			wantMajor: 1, wantMinor: 9, wantText: "1.9", wantParsed: true,
		},
		{
			name:       "a major version alone is not enough to compare",
			out:        "mkfs.erofs (erofs-utils) 2\n",
			wantParsed: false,
		},
		{name: "no version at all", out: "mkfs.erofs (erofs-utils)\n", wantParsed: false},
		{name: "a scheme with no numbers", out: "mkfs.erofs version unknown\n", wantParsed: false},
		{name: "nothing printed", out: "", wantParsed: false},
		{name: "whitespace only", out: "   \n\n", wantParsed: false},
		{name: "an error message", out: "mkfs.erofs: invalid option -- 'V'\n", wantParsed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			version, text, ok := parseToolVersion(tt.out)
			if ok != tt.wantParsed {
				t.Fatalf("parsed = %v, want %v (version %v, text %q)", ok, tt.wantParsed, version, text)
			}
			if !ok {
				return
			}
			if version.major != tt.wantMajor || version.minor != tt.wantMinor {
				t.Errorf("version = %d.%d, want %d.%d",
					version.major, version.minor, tt.wantMajor, tt.wantMinor)
			}
			if text != tt.wantText {
				t.Errorf("text = %q, want %q", text, tt.wantText)
			}
		})
	}
}

func TestToolVersionOlderThan(t *testing.T) {
	tests := []struct {
		a, b toolVersion
		want bool
	}{
		{toolVersion{1, 7}, toolVersion{1, 8}, true},
		{toolVersion{1, 8}, toolVersion{1, 8}, false},
		{toolVersion{1, 9}, toolVersion{1, 8}, false},
		{toolVersion{0, 9}, toolVersion{1, 8}, true},
		{toolVersion{2, 0}, toolVersion{1, 8}, false},
		{toolVersion{1, 10}, toolVersion{1, 8}, false}, // not a string comparison
	}
	for _, tt := range tests {
		if got := tt.a.olderThan(tt.b); got != tt.want {
			t.Errorf("%s.olderThan(%s) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// A mkfs.erofs older than the snapshotter needs is a failure, not a warning: the tool is
// there, it runs, and an image pull will still break partway through.
func TestErofsVersionResultRejectsTooOld(t *testing.T) {
	for _, out := range []string{
		"mkfs.erofs (erofs-utils) 1.7.1\n",
		"mkfs.erofs (erofs-utils) 1.4\n",
		"mkfs.erofs (erofs-utils) v0.9\n",
	} {
		res := erofsVersionResult(context.Background(), "/usr/bin/mkfs.erofs", staticProbe(out, nil))
		if res.Status != StatusFail {
			t.Fatalf("%q: Status = %v, want fail", strings.TrimSpace(out), res.Status)
		}
		version, text, _ := parseToolVersion(out)
		if !strings.Contains(res.Detail, text) {
			t.Errorf("Detail %q does not name the version found (%s)", res.Detail, version)
		}
		for _, want := range []string{text, erofsMinimum.String(), "/usr/bin/mkfs.erofs"} {
			if !strings.Contains(res.Remedy, want) {
				t.Errorf("remedy does not mention %q:\n%s", want, res.Remedy)
			}
		}
	}
}

func TestErofsVersionResultAcceptsNewEnough(t *testing.T) {
	for _, tt := range []struct{ out, want string }{
		{"mkfs.erofs (erofs-utils) 1.8\n", "1.8"},
		{"mkfs.erofs (erofs-utils) 1.9\navailable compressors: lz4\n", "1.9"},
		{"mkfs.erofs (erofs-utils) v1.9.1\n", "v1.9.1"},
		{"mkfs.erofs (erofs-utils) v2.0\n", "v2.0"},
	} {
		res := erofsVersionResult(context.Background(), "/usr/bin/mkfs.erofs", staticProbe(tt.out, nil))
		if res.Status != StatusOK {
			t.Errorf("%q: Status = %v (%s), want ok", strings.TrimSpace(tt.out), res.Status, res.Detail)
		}
		if res.Detail != tt.want {
			t.Errorf("Detail = %q, want %q", res.Detail, tt.want)
		}
	}
}

// Output this parser does not understand says something about the parser, not about the
// host. Failing there would break hosts that work, so it warns and quotes what it saw.
func TestErofsVersionResultWarnsOnOutputItCannotRead(t *testing.T) {
	res := erofsVersionResult(context.Background(), "/usr/bin/mkfs.erofs",
		staticProbe("mkfs.erofs from a downstream build\n", nil))
	if res.Status != StatusWarn {
		t.Fatalf("Status = %v, want warn", res.Status)
	}
	if !strings.Contains(res.Remedy, "mkfs.erofs from a downstream build") {
		t.Errorf("remedy does not quote what the tool printed:\n%s", res.Remedy)
	}
	if !strings.Contains(res.Remedy, erofsMinimum.String()) {
		t.Errorf("remedy does not state the minimum version:\n%s", res.Remedy)
	}
}

// A tool that cannot be run at all is a warning too, with the error in it.
func TestErofsVersionResultWarnsWhenTheProbeFails(t *testing.T) {
	res := erofsVersionResult(context.Background(), "/usr/bin/mkfs.erofs",
		staticProbe("", errors.New("permission denied")))
	if res.Status != StatusWarn {
		t.Fatalf("Status = %v, want warn", res.Status)
	}
	if !strings.Contains(res.Remedy, "permission denied") {
		t.Errorf("remedy does not include the error:\n%s", res.Remedy)
	}
}

// Some tools print their version and still exit non-zero. The version is what matters.
func TestErofsVersionResultUsesOutputEvenWhenTheToolExitsNonZero(t *testing.T) {
	res := erofsVersionResult(context.Background(), "/usr/bin/mkfs.erofs",
		staticProbe("mkfs.erofs (erofs-utils) 1.7.1\n", errors.New("exit status 1")))
	if res.Status != StatusFail {
		t.Errorf("Status = %v, want fail: the version was printed and it is too old", res.Status)
	}
}

// The check must not panic or hang on any of this; it is a diagnostic.
func TestErofsVersionResultSurvivesOddOutput(t *testing.T) {
	for _, out := range []string{
		"", "\n", "\x00\x01\x02", strings.Repeat("v1.9 ", 10000),
		"v.\n", "1.\n", ".9\n", "v1.9999999999999999999999\n", "1.9\r\n",
	} {
		res := erofsVersionResult(context.Background(), "/usr/bin/mkfs.erofs", staticProbe(out, nil))
		if res.Status == StatusOK && res.Detail == "" {
			t.Errorf("%q: reported ok with no version", out)
		}
		if res.Status != StatusOK && res.Remedy == "" {
			t.Errorf("%q: %v with no remedy", out, res.Status)
		}
	}
}

// The version gate has to be wired into the check, not merely available to it: an old
// mkfs.erofs on PATH must make "snapshotter tools" fail.
func TestSnapshotterToolsCheckAppliesTheVersionGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		// exec.LookPath on Windows only finds names with a PATHEXT extension, and
		// "mkfs.erofs" has none. The rules themselves are covered above, without a
		// filesystem.
		t.Skip("cannot place a mkfs.erofs on PATH for LookPath to find on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "mkfs.erofs")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	env := Env{Snapshotter: "erofs"}

	old := snapshotterToolsCheckWith(staticProbe("mkfs.erofs (erofs-utils) 1.7.1\n", nil))
	res := old.Run(context.Background(), env)
	if res.Status != StatusFail {
		t.Errorf("Status = %v (%s), want fail for erofs-utils 1.7.1", res.Status, res.Detail)
	}

	current := snapshotterToolsCheckWith(staticProbe("mkfs.erofs (erofs-utils) 1.9\n", nil))
	res = current.Run(context.Background(), env)
	if res.Status != StatusOK {
		t.Fatalf("Status = %v (%s), want ok for erofs-utils 1.9", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, path) || !strings.Contains(res.Detail, "1.9") {
		t.Errorf("Detail = %q, want the path and the version", res.Detail)
	}
}

// A missing mkfs.erofs is still reported as missing, and the version probe never runs.
func TestSnapshotterToolsCheckStillReportsAMissingTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	probed := false
	check := snapshotterToolsCheckWith(func(context.Context, string, ...string) (string, error) {
		probed = true
		return "", nil
	})
	res := check.Run(context.Background(), Env{Snapshotter: "erofs"})
	if res.Status != StatusFail || !strings.Contains(res.Detail, "mkfs.erofs") {
		t.Errorf("Status = %v, Detail = %q; want a failure naming mkfs.erofs", res.Status, res.Detail)
	}
	if probed {
		t.Error("the version probe ran for a tool that is not installed")
	}
}

// The real probe has to run a program and hand back what it printed, since every rule above
// is applied to that string.
func TestRunVersionProbe(t *testing.T) {
	if _, err := runVersionProbe(context.Background(),
		filepath.Join(t.TempDir(), "definitely-not-installed"), "-V"); err == nil {
		t.Error("probing a binary that is not there returned no error")
	}

	if runtime.GOOS == "windows" {
		t.Skip("no portable way to write a runnable stub here; the rules above need none")
	}
	path := filepath.Join(t.TempDir(), "mkfs.erofs")
	script := "#!/bin/sh\necho 'mkfs.erofs (erofs-utils) 1.9'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runVersionProbe(context.Background(), path, "-V")
	if err != nil {
		t.Fatalf("running %s: %v", path, err)
	}
	if version, text, ok := parseToolVersion(out); !ok || text != "1.9" || version.olderThan(erofsMinimum) {
		t.Errorf("parsed %q as %v (%s, ok=%v), want 1.9", out, version, text, ok)
	}
}

// A snapshotter with no host tools needs no version either.
func TestSnapshotterToolsCheckSkipsSnapshottersWithoutTools(t *testing.T) {
	res := snapshotterToolsCheckWith(staticProbe("", nil)).
		Run(context.Background(), Env{Snapshotter: "overlayfs"})
	if res.Status != StatusSkip {
		t.Errorf("Status = %v, want skip for a snapshotter with no host tools", res.Status)
	}
}

// mkfs.ext4 is required exactly where containerd formats a writable layer, which is not
// everywhere. `blockMode = config.defaultSize > 0` (erofs.go:187) against a platform default of
// 64 MiB off Linux and 0 on it, so macOS and Windows format one ext4 image per active snapshot
// before any task starts, and Linux formats none ever.
//
// Both halves matter. Without the requirement, `boks doctor` is green on a Mac that cannot
// start a sandbox — which is what it was, and is why every macOS run in docs/verification.md
// happened to be on a host with e2fsprogs already installed. With the requirement applied
// everywhere, every Linux host fails for a binary containerd will never invoke there.
func TestSnapshotterToolsAreRequiredWhereTheWritableLayerIsFormatted(t *testing.T) {
	for _, goos := range []string{"darwin", "windows"} {
		var names []string
		for _, tool := range snapshotterTools("erofs", goos) {
			names = append(names, tool.Binary)
		}
		if !slices.Contains(names, "mkfs.ext4") {
			t.Errorf("snapshotterTools(erofs, %s) = %v, missing mkfs.ext4; doctor would pass a "+
				"host whose every 'boks run' dies at task start formatting rwlayer.img", goos, names)
		}
		if !slices.Contains(names, "mkfs.erofs") {
			t.Errorf("snapshotterTools(erofs, %s) = %v, missing mkfs.erofs", goos, names)
		}
	}

	for _, tool := range snapshotterTools("erofs", "linux") {
		if tool.Binary == "mkfs.ext4" {
			t.Error("snapshotterTools(erofs, linux) requires mkfs.ext4; Linux runs the erofs " +
				"snapshotter in ovlfs mode and never formats a writable layer, so this would " +
				"fail every Linux host over a binary containerd does not run")
		}
	}
}

// The two tools come from different packages, so one shared remedy cannot be right for both.
// The failure this guards against is concrete: telling somebody whose mkfs.ext4 is missing to
// install erofs-utils, which they already have, because that is what the erofs snapshotter's
// remedy said.
func TestSnapshotterToolRemediesNameTheRightPackage(t *testing.T) {
	for _, goos := range []string{"darwin", "windows"} {
		for _, tool := range snapshotterTools("erofs", goos) {
			if tool.Binary != "mkfs.ext4" {
				continue
			}
			if strings.Contains(tool.Install, "erofs-utils") {
				t.Errorf("the mkfs.ext4 remedy on %s sends the reader to erofs-utils:\n%s",
					goos, tool.Install)
			}
			if goos == "darwin" && !strings.Contains(tool.Install, "keg-only") {
				t.Errorf("the macOS remedy does not mention that Homebrew keeps e2fsprogs "+
					"keg-only, which is the step that is not obvious:\n%s", tool.Install)
			}
			if goos == "windows" && !strings.Contains(tool.Install, "mkfs.ext4.exe") {
				t.Errorf("the Windows remedy does not name the shipped binary:\n%s", tool.Install)
			}
		}
	}
}
