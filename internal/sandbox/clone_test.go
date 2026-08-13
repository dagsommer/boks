package sandbox

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/workspace"
)

func cloneConfig() Config {
	return Config{
		Name:  "s",
		Clone: true,
		Workspaces: []workspace.Workspace{
			{HostPath: "/home/alice/src/foo", GuestPath: "/home/alice/src/foo", Mode: workspace.ModeReadWrite},
		},
	}
}

// The property the whole mode exists for, expressed as mounts: the workspace's host path is
// not shared at all, and the one share of the user's disk is read-only.
func TestCloneSharesTheHostRepositoryReadOnlyAndNowhereElse(t *testing.T) {
	mounts := workspaceMounts(guestShares(cloneConfig()))
	if len(mounts) != 1 {
		t.Fatalf("got %d mounts, want 1: %+v", len(mounts), mounts)
	}
	m := mounts[0]
	if m.Source != "/home/alice/src/foo" {
		t.Errorf("Source = %q, want the host repository", m.Source)
	}
	if m.Destination != workspace.SourcePath {
		t.Errorf("Destination = %q, want %q", m.Destination, workspace.SourcePath)
	}
	if !slices.Contains(m.Options, "ro") || slices.Contains(m.Options, "rw") {
		t.Errorf("Options = %v, want read-only", m.Options)
	}
	// The host path itself must not be a mount destination: a bind there would put the
	// guest's writes straight back on the user's disk, which is the thing being removed.
	for _, m := range mounts {
		if m.Destination == "/home/alice/src/foo" {
			t.Errorf("the workspace host path is still mounted at %q", m.Destination)
		}
	}
}

// Direct mode must be untouched by any of this.
func TestDirectModeStillSharesTheWorkspaceInPlace(t *testing.T) {
	cfg := cloneConfig()
	cfg.Clone = false

	mounts := workspaceMounts(guestShares(cfg))
	if len(mounts) != 1 || mounts[0].Destination != "/home/alice/src/foo" {
		t.Fatalf("mounts = %+v, want the workspace at its host path", mounts)
	}
	if !slices.Contains(mounts[0].Options, "rw") {
		t.Errorf("Options = %v, want read-write", mounts[0].Options)
	}
}

// Plumbing mounts — the CA certificate today — travel alongside the source share rather than
// displacing it.
func TestCloneKeepsExtraMounts(t *testing.T) {
	cfg := cloneConfig()
	cfg.Mounts = []workspace.Workspace{{HostPath: "/state/ca", GuestPath: "/etc/boks-ca", Mode: workspace.ModeReadOnly}}
	cfg.Workspaces = append(cfg.Workspaces,
		workspace.Workspace{HostPath: "/lib", GuestPath: "/lib", Mode: workspace.ModeReadOnly})

	var destinations []string
	for _, m := range workspaceMounts(guestShares(cfg)) {
		destinations = append(destinations, m.Destination)
	}
	want := []string{workspace.SourcePath, "/lib", "/etc/boks-ca"}
	if !slices.Equal(destinations, want) {
		t.Errorf("destinations = %v, want %v", destinations, want)
	}
}

// guestShares must not write through to the caller's slice: Config is reused by the caller
// after create, and a workspace quietly turned into the source share would make `boks ls`
// report the wrong host path.
func TestGuestSharesDoesNotMutateTheConfig(t *testing.T) {
	cfg := cloneConfig()
	_ = guestShares(cfg)
	if cfg.Workspaces[0].GuestPath != "/home/alice/src/foo" {
		t.Errorf("the config's workspace was rewritten to %q", cfg.Workspaces[0].GuestPath)
	}
}

// The record is what makes the mode durable, and it is read back by `ls`, `inspect`,
// `bundle` and the re-attach check. It carries a version because a reader that cannot tell
// which shape it holds would be guessing about a security property.
func TestFilesystemRecordRoundTrips(t *testing.T) {
	raw, err := encodeFilesystemLabel(configFilesystem(cloneConfig()))
	if err != nil {
		t.Fatalf("encodeFilesystemLabel: %v", err)
	}

	got := decodeFilesystem(map[string]string{LabelFilesystem: raw})
	if !got.IsClone() {
		t.Fatalf("decoded %+v, want clone mode", got)
	}
	if got.Version != filesystemRecordVersion {
		t.Errorf("Version = %d, want %d", got.Version, filesystemRecordVersion)
	}
	if got.Source != workspace.SourcePath {
		t.Errorf("Source = %q, want %q", got.Source, workspace.SourcePath)
	}
	if got.Clone != "/home/alice/src/foo" {
		t.Errorf("Clone = %q, want the workspace path", got.Clone)
	}
}

// Direct mode writes no label. A sandbox created before clone mode existed and one created
// in direct mode today are the same thing, and must read alike.
func TestDirectModeWritesNoLabel(t *testing.T) {
	cfg := cloneConfig()
	cfg.Clone = false

	raw, err := encodeFilesystemLabel(configFilesystem(cfg))
	if err != nil {
		t.Fatalf("encodeFilesystemLabel: %v", err)
	}
	if raw != "" {
		t.Errorf("label = %q, want none for direct mode", raw)
	}
}

// An absent, unreadable or unrecognised record must read as direct mode, which is the answer
// that claims the least. Reading a broken label as "clone" would tell a user their files are
// safe on the strength of a parse failure.
func TestUnreadableRecordIsDirectMode(t *testing.T) {
	tests := map[string]map[string]string{
		"no label":       {},
		"empty":          {LabelFilesystem: ""},
		"not json":       {LabelFilesystem: "clone"},
		"unknown mode":   {LabelFilesystem: `{"version":1,"mode":"magic"}`},
		"a future shape": {LabelFilesystem: `{"version":99,"mode":"something-else"}`},
	}
	for name, labels := range tests {
		t.Run(name, func(t *testing.T) {
			got := decodeFilesystem(labels)
			if got.IsClone() {
				t.Errorf("decoded %+v, want direct mode", got)
			}
			if got.Mode != FilesystemDirect {
				t.Errorf("Mode = %q, want %q", got.Mode, FilesystemDirect)
			}
		})
	}
}

// A record written by a future Boks that still says "clone" is honoured, version and all, so
// that a newer sandbox met by an older binary is not silently downgraded to direct.
func TestAFutureCloneRecordIsStillClone(t *testing.T) {
	got := decodeFilesystem(map[string]string{
		LabelFilesystem: `{"version":2,"mode":"clone","source":"/run/sandbox/source","clone":"/w","extra":"x"}`,
	})
	if !got.IsClone() || got.Version != 2 || got.Clone != "/w" {
		t.Errorf("decoded %+v, want the record as written", got)
	}
}

// The label is JSON in a container label, so it has to survive the round trip through
// containerd as a string rather than as a struct.
func TestFilesystemRecordIsPlainJSON(t *testing.T) {
	raw, err := encodeFilesystemLabel(configFilesystem(cloneConfig()))
	if err != nil {
		t.Fatalf("encodeFilesystemLabel: %v", err)
	}
	var any map[string]any
	if err := json.Unmarshal([]byte(raw), &any); err != nil {
		t.Fatalf("the label is not JSON: %v", err)
	}
	if any["mode"] != FilesystemClone {
		t.Errorf("mode = %v, want %q", any["mode"], FilesystemClone)
	}
}

// --no-hardlinks is a security control. A local git clone hardlinks object files when source
// and destination share a filesystem — same inode — and the guest is root and can chmod, so
// without it a hostile guest could overwrite the host repository's object store through the
// mode that promises it cannot write to the host at all.
func TestCloneRefusesHardlinks(t *testing.T) {
	script := strings.Join(cloneCommand("/w", "1000:1000"), " ")
	if !strings.Contains(script, "--no-hardlinks") {
		t.Error("the clone does not pass --no-hardlinks")
	}
}

// The destination and the owner travel as arguments, never interpolated, so a host path
// containing shell syntax is a path.
func TestCloneCommandPassesPathsAsArguments(t *testing.T) {
	argv := cloneCommand(`/home/a b/"; rm -rf /`, "1000:1000")
	if len(argv) != 5 {
		t.Fatalf("argv = %q, want sh -c SCRIPT DST OWNER", argv)
	}
	if argv[3] != `/home/a b/"; rm -rf /` {
		t.Errorf("argv[3] = %q, want the destination verbatim", argv[3])
	}
	if argv[4] != "1000:1000" {
		t.Errorf("argv[4] = %q, want the owner", argv[4])
	}
	if strings.Contains(argv[2], "rm -rf") {
		t.Error("the destination was interpolated into the script")
	}
}

// An ephemeral clone-mode sandbox runs the keeper and execs its command, because the clone
// has to be made by something before the command can run in it and only a running task can
// be exec'd into.
func TestCloneModeAlwaysRunsTheKeeper(t *testing.T) {
	tests := []struct {
		ephemeral, clone, want bool
	}{
		{ephemeral: false, clone: false, want: true},
		{ephemeral: false, clone: true, want: true},
		{ephemeral: true, clone: false, want: false},
		{ephemeral: true, clone: true, want: true},
	}
	for _, tc := range tests {
		got := runsKeeper(Config{Ephemeral: tc.ephemeral, Clone: tc.clone})
		if got != tc.want {
			t.Errorf("runsKeeper(ephemeral=%v, clone=%v) = %v, want %v",
				tc.ephemeral, tc.clone, got, tc.want)
		}
	}
}
