package sandbox

import (
	"encoding/json"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/dagsommer/boks/internal/runtimecfg"
)

// The pull must name the guest's platform. Without it containerd resolves against the host's,
// which on Windows is windows/<arch> and matches no Linux manifest — the pull then fails with
// "no match for platform in manifest" for an image that exists everywhere.
//
// The options are applied to a bare RemoteContext, which is exactly what containerd does with
// them; none of the three looks at the client, so this reads the real values rather than a
// restatement of them.
func TestPullOptionsAskForTheGuestPlatform(t *testing.T) {
	var rc client.RemoteContext
	for _, opt := range pullOptions(Config{Snapshotter: "erofs"}) {
		if err := opt(nil, &rc); err != nil {
			t.Fatalf("applying a pull option: %v", err)
		}
	}

	want := "linux/" + runtime.GOARCH
	if !slices.Contains(rc.Platforms, want) {
		t.Errorf("Platforms = %v, want it to contain %q", rc.Platforms, want)
	}
	for _, p := range rc.Platforms {
		if p != want {
			t.Errorf("Platforms = %v, want the guest's platform alone; %q would let a "+
				"foreign manifest be chosen", rc.Platforms, p)
		}
	}
	if rc.Platforms[0] != runtimecfg.GuestPlatform() {
		t.Errorf("the pull asks for %q but runtimecfg.GuestPlatform() is %q; the client "+
			"default and the pull would disagree", rc.Platforms[0], runtimecfg.GuestPlatform())
	}

	// The other two are what make the pulled image usable at all, and a platform fix that
	// dropped either would be a worse bug than the one it fixed.
	if !rc.Unpack {
		t.Error("Unpack = false: the image would be pulled but never unpacked for the snapshotter")
	}
	if rc.Snapshotter != "erofs" {
		t.Errorf("Snapshotter = %q, want the configured one", rc.Snapshotter)
	}
}

// On a Windows host containerd's own oci.WithImageConfig cannot be used: it finishes by
// mounting the image's snapshot on the host to read /etc/group, and a Windows host cannot
// mount a Linux EROFS filesystem. Which option is chosen is decided by the host, so this is
// the one assertion that has to know what host it is on.
func TestImageConfigOptFollowsTheHost(t *testing.T) {
	if got, want := containerdReadsGuestRootfs(), runtime.GOOS != "windows"; got != want {
		t.Errorf("containerdReadsGuestRootfs() = %v on %s, want %v", got, runtime.GOOS, want)
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		// Unix must keep containerd's option. The reimplementation exists for the host
		// that cannot use it and must not quietly take over the hosts that can.
		if !containerdReadsGuestRootfs() {
			t.Error("a Unix host would use the Windows stand-in for the image config")
		}
	}
}

// The Windows stand-in has to produce the spec containerd's Linux branch produces, minus the
// step that reads the guest's filesystem. These are the fields it sets.
func TestApplyImageConfig(t *testing.T) {
	s := &specs.Spec{Process: &specs.Process{}}
	err := applyImageConfig(s, ocispec.ImageConfig{
		Env:        []string{"PATH=/usr/bin", "NODE_VERSION=22"},
		Entrypoint: []string{"/usr/bin/tini", "--"},
		Cmd:        []string{"sh"},
		WorkingDir: "/srv",
	})
	if err != nil {
		t.Fatalf("applyImageConfig: %v", err)
	}

	if !slices.Equal(s.Process.Env, []string{"PATH=/usr/bin", "NODE_VERSION=22"}) {
		t.Errorf("Env = %v, want the image's own", s.Process.Env)
	}
	if !slices.Equal(s.Process.Args, []string{"/usr/bin/tini", "--", "sh"}) {
		t.Errorf("Args = %v, want entrypoint then cmd", s.Process.Args)
	}
	if s.Process.Cwd != "/srv" {
		t.Errorf("Cwd = %q, want the image's working directory", s.Process.Cwd)
	}
}

// An image that declares no environment must still get a PATH, or nothing in the guest
// resolves and every command fails as "not found".
func TestApplyImageConfigSuppliesADefaultPath(t *testing.T) {
	s := &specs.Spec{}
	if err := applyImageConfig(s, ocispec.ImageConfig{}); err != nil {
		t.Fatalf("applyImageConfig: %v", err)
	}
	if !slices.Equal(s.Process.Env, defaultGuestEnv) {
		t.Errorf("Env = %v, want the default %v", s.Process.Env, defaultGuestEnv)
	}
	if s.Process.Cwd != "/" {
		t.Errorf("Cwd = %q, want \"/\" when the image names none", s.Process.Cwd)
	}
}

// The image's user is handed to the guest verbatim rather than resolved on the host, which is
// what containerd does on macOS for the same reason. Guessing a uid here is the failure that
// must never happen quietly: an image that says "USER node" running as root instead.
func TestApplyImageConfigDoesNotGuessAUser(t *testing.T) {
	s := &specs.Spec{}
	if err := applyImageConfig(s, ocispec.ImageConfig{User: "node"}); err != nil {
		t.Fatalf("applyImageConfig: %v", err)
	}
	if s.Process.User.Username != "node" {
		t.Errorf("User.Username = %q, want the image's user string for the guest to resolve",
			s.Process.User.Username)
	}
	if s.Process.User.UID != 0 || s.Process.User.GID != 0 {
		t.Errorf("User = %d:%d, want the uid left for the guest rather than invented here",
			s.Process.User.UID, s.Process.User.GID)
	}
	if !slices.Contains(s.Process.User.AdditionalGids, s.Process.User.GID) {
		t.Errorf("AdditionalGids = %v, want the primary group among them",
			s.Process.User.AdditionalGids)
	}

	// An image with no user must not acquire one.
	bare := &specs.Spec{}
	if err := applyImageConfig(bare, ocispec.ImageConfig{}); err != nil {
		t.Fatalf("applyImageConfig: %v", err)
	}
	if bare.Process.User.Username != "" {
		t.Errorf("User.Username = %q for an image that names no user", bare.Process.User.Username)
	}
}

func TestReplaceOrAppendEnv(t *testing.T) {
	tests := []struct {
		name               string
		defaults, override []string
		want               []string
	}{
		{
			name:     "an override replaces in place, keeping the order",
			defaults: []string{"A=1", "B=2", "C=3"},
			override: []string{"B=two"},
			want:     []string{"A=1", "B=two", "C=3"},
		},
		{
			name:     "a new key is appended",
			defaults: []string{"A=1"},
			override: []string{"B=2"},
			want:     []string{"A=1", "B=2"},
		},
		{
			// The OCI convention containerd implements: a bare key unsets.
			name:     "a bare key removes the entry",
			defaults: []string{"A=1", "B=2"},
			override: []string{"B"},
			want:     []string{"A=1"},
		},
		{
			name:     "unsetting something absent adds nothing",
			defaults: []string{"A=1"},
			override: []string{"B"},
			want:     []string{"A=1"},
		},
		{
			name:     "an empty value is an assignment, not a removal",
			defaults: []string{"A=1"},
			override: []string{"A="},
			want:     []string{"A="},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := replaceOrAppendEnv(tt.defaults, tt.override); !slices.Equal(got, tt.want) {
				t.Errorf("replaceOrAppendEnv(%v, %v) = %v, want %v",
					tt.defaults, tt.override, got, tt.want)
			}
		})
	}
}

// containerd builds the cgroups path with filepath.Join, which follows the *host's*
// separator. The path is read inside a Linux guest, so on a Windows host the spec would carry
// a name full of backslashes. The Windows spelling is constructed here rather than waited
// for, which is the only way to test it from Linux.
func TestPOSIXCgroupsPath(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"what a Windows host generates", `\boks\claude-myrepo`, "/boks/claude-myrepo"},
		{"what a POSIX host generates, untouched", "/boks/claude-myrepo", "/boks/claude-myrepo"},
		{"an empty path stays empty rather than becoming the root", "", ""},
		// systemd notation is not a filesystem path and has no separator to fix.
		{"systemd notation is left alone", "system.slice:boks:myrepo", "system.slice:boks:myrepo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := posixCgroupsPath(tt.in); got != tt.want {
				t.Errorf("posixCgroupsPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// linuxSpecFromAWindowsHost is what oci.WithDefaultSpecForPlatform(GuestPlatform()) returns
// when runtime.GOOS == "windows": containerd's Linux spec, plus the empty `windows` section
// its generateDefaultSpecWithPlatform adds for LCOW. The condition is the *host's* OS, which
// a test cannot choose, so the spec is built rather than generated — the same way
// TestPOSIXCgroupsPath constructs the Windows spelling of a cgroups path instead of waiting
// for a Windows machine to produce one.
func linuxSpecFromAWindowsHost() *specs.Spec {
	return &specs.Spec{
		Version: specs.Version,
		Root:    &specs.Root{Path: "rootfs"},
		Process: &specs.Process{Cwd: "/"},
		Linux:   &specs.Linux{CgroupsPath: `\boks\claude-myrepo`},
		Windows: &specs.Windows{},
	}
}

// Why `omitempty` did not save us, stated as an assertion rather than as a claim in a
// comment. specs.Spec.Windows is tagged `json:"windows,omitempty"`, but omitempty on a
// pointer means nil, and containerd stores a non-nil pointer to a zero struct. LayerFolders
// has no omitempty at all, so what nerdbox forwards into the guest is the exact text crun
// refused on 2026-08-14 — and libocispec requires layerFolders whenever `windows` is present.
func TestAnEmptyWindowsSectionMarshalsAsTheFieldTheGuestRejects(t *testing.T) {
	encoded, err := json.Marshal(linuxSpecFromAWindowsHost())
	if err != nil {
		t.Fatalf("marshalling a spec: %v", err)
	}
	if !strings.Contains(string(encoded), `"windows":{"layerFolders":null}`) {
		t.Fatalf("the spec marshals as %s; the point of the repair is that it would "+
			"otherwise carry `\"windows\":{\"layerFolders\":null}`, which crun rejects "+
			"with `Required field 'layerFolders' not present`", encoded)
	}
}

// The repair itself: the section is gone, and so is every trace of it in the JSON the guest
// reads. Nothing else in the spec moves.
func TestWithoutWindowsSectionRemovesTheSectionAWindowsHostAdds(t *testing.T) {
	s := linuxSpecFromAWindowsHost()
	if err := withoutWindowsSection()(t.Context(), nil, nil, s); err != nil {
		t.Fatalf("withoutWindowsSection: %v", err)
	}
	if s.Windows != nil {
		t.Errorf("Windows = %+v, want it removed: a Linux guest has no layer folders", s.Windows)
	}

	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshalling a spec: %v", err)
	}
	if strings.Contains(string(encoded), `"windows"`) {
		t.Errorf("the spec still names a windows section: %s", encoded)
	}

	if s.Linux == nil || s.Linux.CgroupsPath != `\boks\claude-myrepo` {
		t.Errorf("Linux = %+v, want it untouched; this option owns one field", s.Linux)
	}
	if s.Root == nil || s.Root.Path != "rootfs" {
		t.Errorf("Root = %+v, want it untouched", s.Root)
	}
	if s.Process == nil || s.Process.Cwd != "/" {
		t.Errorf("Process = %+v, want it untouched", s.Process)
	}
}

// A spec that is genuinely Windows' keeps its section. Boks generates no such spec, but the
// option runs unconditionally on every host, and stripping the only platform section from a
// spec would be a worse bug than the one being fixed.
func TestWithoutWindowsSectionLeavesARealWindowsSpecAlone(t *testing.T) {
	s := &specs.Spec{Version: specs.Version, Windows: &specs.Windows{LayerFolders: []string{`C:\layer`}}}
	if err := withoutWindowsSection()(t.Context(), nil, nil, s); err != nil {
		t.Fatalf("withoutWindowsSection: %v", err)
	}
	if s.Windows == nil {
		t.Fatal("the Windows section was removed from a spec that has no Linux section")
	}
	if !slices.Equal(s.Windows.LayerFolders, []string{`C:\layer`}) {
		t.Errorf("LayerFolders = %v, want them untouched", s.Windows.LayerFolders)
	}
}

// And the host Boks is developed on must be provably unaffected. containerd's generator adds
// the section only when runtime.GOOS == "windows", so on Unix the spec never has one and the
// repair is a no-op on a real, generated spec rather than on a constructed one.
func TestTheGeneratedSpecHasNoWindowsSectionOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the assertion is about the hosts that were already working")
	}
	ctx := namespaces.WithNamespace(t.Context(), "boks")
	var s specs.Spec
	opt := oci.WithDefaultSpecForPlatform(runtimecfg.GuestPlatform())
	if err := opt(ctx, nil, &containers.Container{ID: "claude-myrepo"}, &s); err != nil {
		t.Fatalf("generating the default spec: %v", err)
	}
	if s.Windows != nil {
		t.Fatalf("containerd generated a Windows section on %s: %+v", runtime.GOOS, s.Windows)
	}

	before, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshalling a spec: %v", err)
	}
	if err := withoutWindowsSection()(ctx, nil, nil, &s); err != nil {
		t.Fatalf("withoutWindowsSection: %v", err)
	}
	after, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshalling a spec: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("the repair changed a spec generated on %s:\n before %s\n after  %s",
			runtime.GOOS, before, after)
	}
}

// The repair must survive a spec that has no Linux section at all rather than panicking on
// one — the option runs unconditionally, and a spec is a spec.
func TestPOSIXCgroupsPathOptSkipsANonLinuxSpec(t *testing.T) {
	s := &specs.Spec{}
	if err := withPOSIXCgroupsPath()(t.Context(), nil, nil, s); err != nil {
		t.Fatalf("withPOSIXCgroupsPath on a spec with no Linux section: %v", err)
	}

	s = &specs.Spec{Linux: &specs.Linux{CgroupsPath: `\boks\name`}}
	if err := withPOSIXCgroupsPath()(t.Context(), nil, nil, s); err != nil {
		t.Fatalf("withPOSIXCgroupsPath: %v", err)
	}
	if s.Linux.CgroupsPath != "/boks/name" {
		t.Errorf("CgroupsPath = %q, want the POSIX spelling the guest reads", s.Linux.CgroupsPath)
	}
}
