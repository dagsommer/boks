package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVersionAcceptsEverySpellingInPlay(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"v2.3.3", "2.3", true},           // a module graph
		{"2.2.6", "2.2", true},            // containerd's own API
		{"2.2.6+boks-erofs", "2.2", true}, // packaging/containerd-windows stamps this
		{"v2.3.0-rc.1", "2.3", true},
		{" 2.2.6 ", "2.2", true},
		{"(devel)", "", false},
		{"2", "", false},
		{"", "", false},
	} {
		got, ok := parseVersion(tc.in)
		if ok != tc.ok {
			t.Errorf("parseVersion(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if ok && got.String() != tc.want {
			t.Errorf("parseVersion(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// The measured failure: a shim linking containerd 2.3.3 against a daemon running 2.2.2 fails
// at task start with `unsupported protocol: Yunix`, which names nothing.
func TestCheckSkewCatchesTheMeasuredFailure(t *testing.T) {
	skew := CheckSkew("2.2.2", "v2.3.3")
	if skew == nil {
		t.Fatal("a 2.2.2 daemon under a 2.3.3 shim is the failure this check exists for")
	}
	if !strings.Contains(skew.Remedy, "Yunix") {
		t.Error("the remedy does not quote the error the user will actually see")
	}
}

// The rule is directional: a reader understands every encoding up to its own, so a newer
// daemon under an older shim is fine and must not be reported.
func TestCheckSkewIsDirectional(t *testing.T) {
	for _, tc := range []struct{ daemon, shim string }{
		{"2.3.3", "v2.2.2"}, // daemon newer: fine
		{"2.2.6", "v2.2.6"}, // equal
		{"2.2.6", "v2.2.9"}, // same minor, different patch: not a claim anyone has made
	} {
		if skew := CheckSkew(tc.daemon, tc.shim); skew != nil {
			t.Errorf("CheckSkew(%q, %q) reported %q, want no skew", tc.daemon, tc.shim, skew.Detail)
		}
	}
}

// A version that cannot be read is not evidence of a problem. A check that guessed would warn
// on hosts that are fine, and a warning people learn to ignore is worse than no warning.
func TestCheckSkewStaysQuietWhenItCannotTell(t *testing.T) {
	for _, tc := range []struct{ daemon, shim string }{
		{"", "v2.3.3"},
		{"2.2.2", ""},
		{"(devel)", "v2.3.3"},
		{"2.2.2", "(devel)"},
	} {
		if skew := CheckSkew(tc.daemon, tc.shim); skew != nil {
			t.Errorf("CheckSkew(%q, %q) reported %q on an unreadable version", tc.daemon, tc.shim, skew.Detail)
		}
	}
}

// ShimContainerd is read out of a real linked binary, so the test builds one.
//
// The obvious shortcut — reading this test binary, which also links containerd — does not
// work, and the reason is worth recording because it is not obvious and it cost a debugging
// round: the Go toolchain omits the `dep` lines from a test binary's build information. Only
// `mod` and the `build` settings survive, so buildinfo finds no containerd at all. Any check
// that reads a *shim* is reading an ordinary executable, and this test has to use one too.
//
// cmd/boks is built rather than a synthetic program because it is already in this module and
// already links containerd, so there is no temporary module to resolve. It takes well under a
// second from a warm cache.
func TestShimContainerdReadsARealBinary(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build a binary with")
	}
	probe := filepath.Join(t.TempDir(), "probe")
	build := exec.Command("go", "build", "-o", probe, "github.com/dagsommer/boks/cmd/boks")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("could not build a probe binary: %v\n%s", err, out)
	}

	version := ShimContainerd(probe)
	if version == "" {
		t.Fatal("no containerd version read from a binary that links containerd")
	}
	if _, ok := parseVersion(version); !ok {
		t.Fatalf("read %q, which does not parse as a version", version)
	}
	if !strings.HasPrefix(version, "v2.") {
		t.Errorf("read %q; this module requires containerd v2", version)
	}

	// And the whole point: the version read out of a binary is directly comparable with
	// the version a daemon reports over its API, which is spelled without the "v".
	if skew := CheckSkew(strings.TrimPrefix(version, "v"), version); skew != nil {
		t.Errorf("a binary compared against its own containerd reported skew: %s", skew.Detail)
	}
}

// A file that is not a Go binary is not a problem to report, it is a question this technique
// cannot answer — and answering it wrongly would produce a warning about a healthy host.
func TestShimContainerdIsSilentOnANonGoFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "containerd-shim-nerdbox-v1")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ShimContainerd(path); got != "" {
		t.Errorf("ShimContainerd(a shell script) = %q, want \"\"", got)
	}
	if got := ShimContainerd(filepath.Join(dir, "absent")); got != "" {
		t.Errorf("ShimContainerd(a missing file) = %q, want \"\"", got)
	}
}

// ShimResolvesUsernames decides whether Boks may hand an image's `USER node` to the guest
// instead of resolving it on the host. Answering true when the guest cannot in fact resolve
// it produces a container running as root with nothing logged, so every case this cannot
// answer must come back false.
//
// The empty allowlist is asserted directly. It is the whole safety property: while no
// nerdbox revision resolves Process.User.Username, there is no binary anywhere that should
// make this function true, and a revision added to the map without a guest to back it would
// be the way the regression ships.
func TestShimResolvesUsernamesIsFalseWithoutEvidence(t *testing.T) {
	if len(nerdboxRevisionsResolvingUsernames) != 0 {
		t.Errorf("nerdboxRevisionsResolvingUsernames has %d entries; no released nerdbox "+
			"resolves Process.User.Username, so anything here lets Boks skip host-side "+
			"resolution and run `USER node` images as root",
			len(nerdboxRevisionsResolvingUsernames))
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "containerd-shim-nerdbox-v1")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		path string
	}{
		{"no shim found at all", ""},
		{"a missing file", filepath.Join(dir, "absent")},
		{"a file that is not a Go binary", script},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ShimResolvesUsernames(tc.path) {
				t.Error("ShimResolvesUsernames = true; an unknown shim must never " +
					"be treated as one whose guest resolves USER names")
			}
		})
	}
}

// A Go binary that is not nerdbox must not be mistaken for one. Boks' own binary is the
// convenient example: it is a real, VCS-stamped Go program of the right shape whose main
// module is something else entirely.
func TestShimNerdboxRejectsAnotherModulesBinary(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build a binary with")
	}
	probe := filepath.Join(t.TempDir(), "probe")
	build := exec.Command("go", "build", "-o", probe, "github.com/dagsommer/boks/cmd/boks")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("could not build a probe binary: %v\n%s", err, out)
	}

	if got := ShimNerdbox(probe); got != "" {
		t.Errorf("ShimNerdbox(the boks binary) = %q, want \"\" — its main module is not nerdbox", got)
	}
	if ShimResolvesUsernames(probe) {
		t.Error("ShimResolvesUsernames(the boks binary) = true")
	}
}
