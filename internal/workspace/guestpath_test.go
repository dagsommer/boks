package workspace

import (
	"runtime"
	"strings"
	"testing"
)

// The Windows mapping is exercised by passing styleWindows explicitly, so these tests are
// ordinary Go on whatever machine runs them. Nothing here touches the filesystem: the inputs
// are Windows paths as strings, because no machine on this project runs Windows and a test
// that needed one would never run.

func TestGuestPathWindows(t *testing.T) {
	tests := []struct {
		name string
		host string
		want string
	}{
		{"the ordinary case", `C:\Users\x\repo`, "/c/Users/x/repo"},
		// The one case observed rather than reasoned: this host path and this guest path
		// were read off a Windows 11 machine running Docker Sandboxes, from the guest's
		// own /proc/mounts. See docs/windows.md section 7a.
		{"what sbx was observed to do", `C:\Users\E194604\source\repos\DigitalPostNy`,
			"/c/Users/E194604/source/repos/DigitalPostNy"},
		{"lowercase drive", `c:\Users\x\repo`, "/c/Users/x/repo"},
		{"another drive", `D:\data\repo`, "/d/data/repo"},
		{"forward slashes", `C:/Users/x`, "/c/Users/x"},
		{"mixed separators", `C:\Users/x\repo`, "/c/Users/x/repo"},
		{"trailing backslash", `C:\Users\x\`, "/c/Users/x"},
		{"trailing forward slash", `C:/Users/x/`, "/c/Users/x"},
		{"bare drive root", `C:\`, "/c"},
		{"bare drive root, forward slash", `C:/`, "/c"},
		{"repeated separators", `C:\\Users\\x`, "/c/Users/x"},
		{"spaces", `C:\Users\Dag Sommer\My Repo`, "/c/Users/Dag Sommer/My Repo"},
		{"non-ASCII", `C:\Users\Åse\prosjekt-æøå\日本`, "/c/Users/Åse/prosjekt-æøå/日本"},
		{"dot segment", `C:\Users\x\.\repo`, "/c/Users/x/repo"},
		{"parent segment", `C:\Users\x\..\y\repo`, "/c/Users/y/repo"},
		{"extended-length prefix", `\\?\C:\Users\x\repo`, "/c/Users/x/repo"},
		{"extended-length prefix, drive root", `\\?\C:\`, "/c"},
		// Case is preserved below the drive letter. The guest filesystem is
		// case-sensitive and the directory really is called Users, not users.
		{"case preserved below the drive", `C:\PROGRAM FILES\Foo`, "/c/PROGRAM FILES/Foo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := guestPath(tt.host, styleWindows)
			if err != nil {
				t.Fatalf("guestPath(%q): %v", tt.host, err)
			}
			if got != tt.want {
				t.Errorf("guestPath(%q) = %q, want %q", tt.host, got, tt.want)
			}
		})
	}
}

// A path Boks cannot map must be refused. The alternative is producing some other path and
// sharing a directory the user did not name, which is a silent wrong answer where this is a
// loud one.
func TestGuestPathWindowsRejects(t *testing.T) {
	tests := []struct {
		name string
		host string
	}{
		{"UNC path", `\\server\share\project`},
		{"UNC path, forward slashes", `//server/share/project`},
		{"UNC path behind the extended-length prefix", `\\?\UNC\server\share\project`},
		{"UNC path behind a lowercase extended-length prefix", `\\?\unc\server\share`},
		{"WSL distribution share", `\\wsl$\Ubuntu\home\dag\src`},
		{"device namespace", `\\.\C:\Users\x`},
		{"volume GUID", `\\?\Volume{f366aeed-8faf-cfc3-0000-000000000000}\repo`},
		{"drive-relative", `C:repo`},
		{"root-relative with no drive", `\Users\x`},
		{"relative", `Users\x`},
		{"bare drive letter", `C:`},
		{"empty", ``},
		{"not a drive letter", `1:\repo`},
		{"non-ASCII drive letter", `Å:\repo`},
		{"NUL byte", "C:\\Users\\x\x00\\repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := guestPath(tt.host, styleWindows)
			if err == nil {
				t.Fatalf("guestPath(%q) = %q, want an error", tt.host, got)
			}
		})
	}
}

// Windows filesystems are case-insensitive and the guest's is not, so two spellings of one
// host directory must not become two shares. Drive-letter folding is this package's half of
// that; the components below it are canonicalised by filepath.EvalSymlinks before guestPath
// ever sees them (see Parse).
func TestGuestPathWindowsFoldsDriveCase(t *testing.T) {
	for _, pair := range [][2]string{
		{`C:\Users\x\repo`, `c:\Users\x\repo`},
		{`C:/Users/x`, `c:\Users\x`},
		{`D:\`, `d:/`},
	} {
		upper, err := guestPath(pair[0], styleWindows)
		if err != nil {
			t.Fatalf("guestPath(%q): %v", pair[0], err)
		}
		lower, err := guestPath(pair[1], styleWindows)
		if err != nil {
			t.Fatalf("guestPath(%q): %v", pair[1], err)
		}
		if upper != lower {
			t.Errorf("guestPath(%q) = %q but guestPath(%q) = %q; one directory must give one share",
				pair[0], upper, pair[1], lower)
		}
	}
}

// Two host directories sharing a guest path would mean one sandbox mount shadowing the other,
// so the mapping has to be injective. It is, because the only lossy step is folding the drive
// letter, and a drive letter is already case-insensitive on the host.
func TestGuestPathWindowsIsInjective(t *testing.T) {
	hosts := []string{
		`C:\Users\x\repo`, `C:\Users\x\Repo`, `C:\users\x\repo`,
		`D:\Users\x\repo`, `C:\Users\y\repo`, `C:\Users\x`, `C:\`, `D:\`,
		`C:\Users\x\repo two`, `C:\Users\x\repo-two`,
	}
	seen := make(map[string]string, len(hosts))
	for _, host := range hosts {
		guest, err := guestPath(host, styleWindows)
		if err != nil {
			t.Fatalf("guestPath(%q): %v", host, err)
		}
		// c:\ and C:\ are the same directory, so they are allowed to agree.
		if prev, ok := seen[guest]; ok && !strings.EqualFold(prev, host) {
			t.Errorf("guestPath(%q) and guestPath(%q) both give %q", prev, host, guest)
		}
		seen[guest] = host
	}
}

// Every mapped workspace lives under a single-letter directory, so no host path can be shared
// over /etc, /run, /usr or anything else the guest image already has. That is a property of
// the convention rather than a check, and it is worth a test because losing it would let a
// workspace mount shadow the guest's own filesystem.
func TestGuestPathWindowsCannotShadowGuestDirectories(t *testing.T) {
	for _, host := range []string{
		`C:\etc\passwd`, `E:\tc`, `R:\un\boks`, `U:\sr\bin`, `C:\`,
	} {
		guest, err := guestPath(host, styleWindows)
		if err != nil {
			t.Fatalf("guestPath(%q): %v", host, err)
		}
		first, _, _ := strings.Cut(strings.TrimPrefix(guest, "/"), "/")
		if len(first) != 1 || !isDriveLetter(first[0]) {
			t.Errorf("guestPath(%q) = %q, whose first component %q is not a bare drive letter",
				host, guest, first)
		}
	}
}

// The Unix behaviour is the identity and must stay byte-for-byte identical, including for
// paths that look like Windows ones: a Linux directory may really be called `C:\Users`, and
// rewriting it would share the wrong thing on the platform Boks actually runs on.
func TestGuestPathPOSIXIsIdentity(t *testing.T) {
	for _, host := range []string{
		"/home/alice/src/foo",
		"/",
		`/home/alice/C:\Users\x`,
		`/home/alice/odd:name`,
		"/home/alice/prosjekt-æøå",
		`/home/alice/back\slash`,
		"/private/tmp/probe/deep/a/b/c/project",
	} {
		got, err := guestPath(host, stylePOSIX)
		if err != nil {
			t.Fatalf("guestPath(%q): %v", host, err)
		}
		if got != host {
			t.Errorf("guestPath(%q) = %q, want it unchanged", host, got)
		}
	}
}

func TestHostStyleMatchesTheRunningOS(t *testing.T) {
	want := stylePOSIX
	if runtime.GOOS == "windows" {
		want = styleWindows
	}
	if got := hostStyle(); got != want {
		t.Errorf("hostStyle() = %v on %s, want %v", got, runtime.GOOS, want)
	}
}

// The ":ro" suffix and the Windows drive separator are both colons, and only one of them is a
// mode.
func TestSplitMode(t *testing.T) {
	tests := []struct {
		name     string
		arg      string
		style    pathStyle
		wantPath string
		wantMode Mode
	}{
		{"posix, no suffix", "/src/foo", stylePOSIX, "/src/foo", ModeReadWrite},
		{"posix, read-only", "/src/foo:ro", stylePOSIX, "/src/foo", ModeReadOnly},
		{"posix, read-write", "/src/foo:rw", stylePOSIX, "/src/foo", ModeReadWrite},
		{"posix, colon in the path", "/src/odd:name", stylePOSIX, "/src/odd:name", ModeReadWrite},
		{"posix, relative read-only", "x:ro", stylePOSIX, "x", ModeReadOnly},
		{"windows, no suffix", `C:\src\foo`, styleWindows, `C:\src\foo`, ModeReadWrite},
		{"windows, read-only", `C:\src\foo:ro`, styleWindows, `C:\src\foo`, ModeReadOnly},
		{"windows, read-write", `C:\src\foo:rw`, styleWindows, `C:\src\foo`, ModeReadWrite},
		// The drive separator is not a mode: this asks for a directory called "ro" on
		// drive C, not for drive C read-only.
		{"windows, drive-relative ro", `C:ro`, styleWindows, `C:ro`, ModeReadWrite},
		{"windows, drive root", `C:\`, styleWindows, `C:\`, ModeReadWrite},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPath, gotMode := splitMode(tt.arg, tt.style)
			if gotPath != tt.wantPath || gotMode != tt.wantMode {
				t.Errorf("splitMode(%q) = (%q, %q), want (%q, %q)",
					tt.arg, gotPath, gotMode, tt.wantPath, tt.wantMode)
			}
		})
	}
}
