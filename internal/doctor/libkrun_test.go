package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The hypervisor library check is exercised by passing goos, goarch, an environment and a
// filesystem explicitly, so every case here is ordinary Go on whatever machine runs it.
// Nothing reads the real filesystem: the point of these tests is a host that has libkrun in
// one specific place under one specific name, and no test host has that on demand. The macOS
// cases matter most, since no machine on this project runs macOS.

// fakeFS builds a libraryFS from a list of full paths that exist.
func fakeFS(paths ...string) libraryFS {
	files := map[string]bool{}
	dirs := map[string][]string{}
	for _, p := range paths {
		p = filepath.Clean(p)
		files[p] = true
		dir, name := filepath.Split(p)
		dir = filepath.Clean(dir)
		dirs[dir] = append(dirs[dir], name)
	}
	return libraryFS{
		exists:  func(path string) bool { return files[filepath.Clean(path)] },
		entries: func(dir string) []string { return dirs[filepath.Clean(dir)] },
	}
}

// envOf builds a getenv function over a fixed map, joining directory lists the way the
// running platform does — which is how the shim reads them too.
func envOf(vars map[string][]string) func(string) string {
	return func(key string) string {
		return strings.Join(vars[key], string(os.PathListSeparator))
	}
}

func TestNerdboxLibraryNamesMirrorTheShim(t *testing.T) {
	tests := []struct {
		goos, goarch string
		want         []string
	}{
		// amd64 is spelled x86_64, as nerdbox's kernelArch does.
		{"linux", "amd64", []string{"libkrun-x86_64.so", "libkrun.so"}},
		{"linux", "arm64", []string{"libkrun-arm64.so", "libkrun.so"}},
		{"darwin", "arm64", []string{
			"libkrun-arm64.dylib", "libkrun.dylib",
			"libkrun-efi-arm64.dylib", "libkrun-efi.dylib",
		}},
		{"darwin", "amd64", []string{
			"libkrun-x86_64.dylib", "libkrun.dylib",
			"libkrun-efi-x86_64.dylib", "libkrun-efi.dylib",
		}},
		{"windows", "amd64", []string{"krun.dll"}},
	}
	for _, tt := range tests {
		t.Run(tt.goos+"/"+tt.goarch, func(t *testing.T) {
			got := nerdboxLibraryNames(tt.goos, tt.goarch)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("nerdboxLibraryNames(%q, %q) = %v, want %v", tt.goos, tt.goarch, got, tt.want)
			}
		})
	}
}

func TestNerdboxSearchPathsMirrorTheShim(t *testing.T) {
	defaults := []string{"/usr/local/lib", "/usr/local/lib64", "/usr/lib", "/lib"}

	t.Run("PATH first, then the defaults", func(t *testing.T) {
		got := nerdboxSearchPaths("linux", envOf(map[string][]string{"PATH": {"/opt/bin", "/usr/bin"}}))
		want := append([]string{"/opt/bin", "/usr/bin"}, defaults...)
		assertPaths(t, got, want)
	})

	t.Run("LIBKRUN_PATH replaces the defaults", func(t *testing.T) {
		got := nerdboxSearchPaths("linux", envOf(map[string][]string{
			"PATH":         {"/usr/bin"},
			"LIBKRUN_PATH": {"/opt/krun"},
		}))
		assertPaths(t, got, []string{"/usr/bin", "/opt/krun"})
	})

	t.Run("an empty list element means the working directory", func(t *testing.T) {
		got := nerdboxSearchPaths("linux", func(key string) string {
			if key == "PATH" {
				return string(os.PathListSeparator) + "/usr/bin"
			}
			return ""
		})
		if len(got) == 0 || got[0] != "." {
			t.Errorf("first search path = %v, want \".\" for an empty PATH element", got)
		}
	})

	t.Run("darwin appends the Homebrew prefix to the defaults", func(t *testing.T) {
		got := nerdboxSearchPaths("darwin", envOf(map[string][]string{"PATH": {"/usr/bin"}}))
		want := append([]string{"/usr/bin"}, defaults...)
		want = append(want, "/opt/homebrew/lib")
		assertPaths(t, got, want)
	})

	t.Run("darwin appends the Homebrew prefix to LIBKRUN_PATH too", func(t *testing.T) {
		got := nerdboxSearchPaths("darwin", envOf(map[string][]string{
			"PATH":         {"/usr/bin"},
			"LIBKRUN_PATH": {"/opt/krun"},
		}))
		assertPaths(t, got, []string{"/usr/bin", "/opt/krun", "/opt/homebrew/lib"})
	})

	// The list separator is the running platform's, since the shim runs on the host it is
	// reading the variable from; goos selects the naming and default-directory rules.
	// The directory below therefore carries no drive letter: a colon would be a separator
	// when this test runs on Linux, which is where it usually runs.
	t.Run("windows has no default directories", func(t *testing.T) {
		got := nerdboxSearchPaths("windows", envOf(map[string][]string{"PATH": {`\krun\bin`}}))
		assertPaths(t, got, []string{`\krun\bin`})
	})
}

func assertPaths(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("search paths =\n  %v\nwant\n  %v", got, want)
	}
}

// A libkrun the shim would load must be reported ok, wherever the shim would find it. The
// old check looked only at a hardcoded list of library directories, so a libkrun on PATH —
// which is the first thing the shim tries — was reported missing on a host that works.
func TestHypervisorLibraryAcceptsWhatTheShimLoads(t *testing.T) {
	tests := []struct {
		name         string
		goos, goarch string
		env          map[string][]string
		files        []string
		want         string
	}{
		{
			name: "on PATH, next to the shim",
			goos: "linux", goarch: "amd64",
			env:   map[string][]string{"PATH": {"/opt/nerdbox/bin"}},
			files: []string{"/opt/nerdbox/bin/libkrun.so"},
			want:  "/opt/nerdbox/bin/libkrun.so",
		},
		{
			name: "in LIBKRUN_PATH",
			goos: "linux", goarch: "arm64",
			env:   map[string][]string{"PATH": {"/usr/bin"}, "LIBKRUN_PATH": {"/opt/krun"}},
			files: []string{"/opt/krun/libkrun.so"},
			want:  "/opt/krun/libkrun.so",
		},
		{
			name: "arch-qualified name",
			goos: "linux", goarch: "amd64",
			env:   map[string][]string{"PATH": {"/usr/bin"}},
			files: []string{"/usr/bin/libkrun-x86_64.so"},
			want:  "/usr/bin/libkrun-x86_64.so",
		},
		{
			name: "in a default directory",
			goos: "linux", goarch: "amd64",
			env:   map[string][]string{"PATH": {"/usr/bin"}},
			files: []string{"/usr/lib/libkrun.so"},
			want:  "/usr/lib/libkrun.so",
		},
		{
			name: "the Homebrew prefix on macOS",
			goos: "darwin", goarch: "arm64",
			env:   map[string][]string{"PATH": {"/usr/bin"}},
			files: []string{"/opt/homebrew/lib/libkrun.dylib"},
			want:  "/opt/homebrew/lib/libkrun.dylib",
		},
		{
			name: "the EFI variant on macOS, which the shim also accepts",
			goos: "darwin", goarch: "arm64",
			env:   map[string][]string{"PATH": {"/usr/bin"}},
			files: []string{"/opt/homebrew/lib/libkrun-efi.dylib"},
			want:  "/opt/homebrew/lib/libkrun-efi.dylib",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := hypervisorLibraryResult(tt.goos, tt.goarch, envOf(tt.env), fakeFS(tt.files...))
			if res.Status != StatusOK {
				t.Fatalf("Status = %v (%s), want ok — the shim would load %s",
					res.Status, res.Detail, tt.want)
			}
			if res.Detail != filepath.Clean(tt.want) {
				t.Errorf("Detail = %q, want %q", res.Detail, filepath.Clean(tt.want))
			}
		})
	}
}

// The shim scans directories in order and, within a directory, names in order. A libkrun
// earlier in the directory list wins even when a later directory holds a name the shim
// prefers, because the shim stops at the first directory that has any of them.
func TestHypervisorLibrarySearchOrderIsTheShimsOrder(t *testing.T) {
	env := envOf(map[string][]string{"PATH": {"/opt/nerdbox/bin"}})
	res := hypervisorLibraryResult("linux", "amd64", env, fakeFS(
		"/opt/nerdbox/bin/libkrun.so",
		"/usr/lib/libkrun-x86_64.so",
	))
	if res.Detail != filepath.Clean("/opt/nerdbox/bin/libkrun.so") {
		t.Errorf("Detail = %q, want the PATH entry: the shim searches PATH before /usr/lib", res.Detail)
	}

	res = hypervisorLibraryResult("linux", "amd64", env, fakeFS(
		"/opt/nerdbox/bin/libkrun.so",
		"/opt/nerdbox/bin/libkrun-x86_64.so",
	))
	if res.Detail != filepath.Clean("/opt/nerdbox/bin/libkrun-x86_64.so") {
		t.Errorf("Detail = %q, want the arch-qualified name: the shim stats it first", res.Detail)
	}
}

// A soname-versioned libkrun is not a libkrun the shim can load. It stats libkrun.so and
// dlopens that exact path; it never asks the dynamic loader to resolve a soname, so
// libkrun.so.1 on its own boots nothing.
//
// The old check accepted the name outright and reported ok, which is the worst outcome this
// command has: a clean bill of health for a host that fails at VM boot. It must not be ok —
// and it must not be a bare "not found" either, since the fix is a symlink away and saying
// "not found" sends someone to reinstall a package they already have.
func TestHypervisorLibraryRejectsSonameOnlyInstall(t *testing.T) {
	tests := []struct {
		name         string
		goos, goarch string
		file         string
	}{
		{"linux, versioned soname", "linux", "amd64", "/usr/lib/libkrun.so.1"},
		{"linux, fully versioned", "linux", "amd64", "/usr/lib/libkrun.so.1.18.0"},
		{"macOS, versioned dylib", "darwin", "arm64", "/opt/homebrew/lib/libkrun.1.dylib"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := hypervisorLibraryResult(tt.goos, tt.goarch,
				envOf(map[string][]string{"PATH": {"/usr/bin"}}), fakeFS(tt.file))
			if res.Status == StatusOK {
				t.Fatalf("Status = ok for %s, which the shim never stats", tt.file)
			}
			if res.Status != StatusWarn {
				t.Fatalf("Status = %v, want warn", res.Status)
			}
			if !strings.Contains(res.Detail, filepath.Clean(tt.file)) {
				t.Errorf("Detail = %q, does not name the file that was found", res.Detail)
			}
			// The way out is a symlink beside the file, under a name the shim
			// stats — the directory it is in is already one the shim searches.
			symlink := "ln -s " + filepath.Clean(tt.file) + " " +
				filepath.Join(filepath.Dir(filepath.Clean(tt.file)), canonicalLibraryName(tt.goos))
			for _, want := range []string{filepath.Clean(tt.file), symlink, "LIBKRUN_PATH"} {
				if !strings.Contains(res.Remedy, want) {
					t.Errorf("remedy does not mention %q:\n%s", want, res.Remedy)
				}
			}
		})
	}
}

// A libkrun in a directory the shim does not scan is the same kind of near miss: present,
// correctly named, and never loaded. The multiarch directory is where Debian and Ubuntu put
// shared libraries, so this is the likely shape of it.
func TestHypervisorLibraryReportsADirectoryTheShimDoesNotSearch(t *testing.T) {
	res := hypervisorLibraryResult("linux", "amd64",
		envOf(map[string][]string{"PATH": {"/usr/bin"}}),
		fakeFS("/usr/lib/x86_64-linux-gnu/libkrun.so"))

	if res.Status != StatusWarn {
		t.Fatalf("Status = %v (%s), want warn", res.Status, res.Detail)
	}
	if !strings.Contains(res.Remedy, filepath.Clean("/usr/lib/x86_64-linux-gnu")) {
		t.Errorf("remedy does not name the directory the library is in:\n%s", res.Remedy)
	}
	if !strings.Contains(res.Remedy, "not a directory the shim searches") {
		t.Errorf("remedy does not say why the shim will not load it:\n%s", res.Remedy)
	}
	// A symlink out of the multiarch directory into one the shim does search is the
	// shortest way out, and the command has to be right.
	want := "ln -s " + filepath.Clean("/usr/lib/x86_64-linux-gnu/libkrun.so") + " " +
		filepath.Clean("/usr/lib/libkrun.so")
	if !strings.Contains(res.Remedy, want) {
		t.Errorf("remedy does not suggest %q:\n%s", want, res.Remedy)
	}
}

// Setting LIBKRUN_PATH replaces the shim's default directories rather than adding to them,
// which is a way to break a host that was working. A libkrun left behind in one of those
// defaults must be reported, not silently dropped.
func TestHypervisorLibraryReportsDefaultsDiscardedByLibkrunPath(t *testing.T) {
	res := hypervisorLibraryResult("linux", "amd64",
		envOf(map[string][]string{"PATH": {"/usr/bin"}, "LIBKRUN_PATH": {"/opt/krun"}}),
		fakeFS("/usr/lib/libkrun.so"))

	if res.Status != StatusWarn {
		t.Fatalf("Status = %v (%s), want warn", res.Status, res.Detail)
	}
	if !strings.Contains(res.Remedy, filepath.Clean("/usr/lib/libkrun.so")) {
		t.Errorf("remedy does not name the library LIBKRUN_PATH excluded:\n%s", res.Remedy)
	}
	if !strings.Contains(res.Remedy, "replaces the defaults") {
		t.Errorf("remedy does not explain that LIBKRUN_PATH replaces the defaults:\n%s", res.Remedy)
	}
	// The library is already at the one path a symlink could sensibly occupy. Telling
	// someone to link a file to itself is worse than telling them nothing; LIBKRUN_PATH
	// is the whole of the fix here.
	if strings.Contains(res.Remedy, "ln -s") {
		t.Errorf("remedy suggests a symlink from the library to itself:\n%s", res.Remedy)
	}
	if !strings.Contains(res.Remedy, "LIBKRUN_PATH at "+filepath.Clean("/usr/lib")) {
		t.Errorf("remedy does not say where to point LIBKRUN_PATH:\n%s", res.Remedy)
	}
}

// With no libkrun anywhere, the check warns rather than fails: the search depends on
// containerd's PATH and LIBKRUN_PATH, which doctor cannot read, so a miss is not proof.
func TestHypervisorLibraryMissNamesWhatItLookedFor(t *testing.T) {
	res := hypervisorLibraryResult("linux", "amd64",
		envOf(map[string][]string{"PATH": {"/usr/bin"}}), fakeFS())

	if res.Status != StatusWarn {
		t.Fatalf("Status = %v, want warn: doctor cannot read containerd's environment", res.Status)
	}
	for _, want := range []string{"libkrun.so", "/usr/lib", "PATH", "LIBKRUN_PATH", "containerd's environment"} {
		if !strings.Contains(res.Remedy, want) {
			t.Errorf("remedy does not mention %q:\n%s", want, res.Remedy)
		}
	}
}

// Every non-ok verdict has to tell the user what to do; a bare warning about a library they
// cannot see is not actionable.
func TestHypervisorLibraryAlwaysExplainsItself(t *testing.T) {
	for _, files := range [][]string{{}, {"/usr/lib/libkrun.so.1"}, {"/usr/lib64/libkrun.so"}} {
		res := hypervisorLibraryResult("linux", "amd64",
			envOf(map[string][]string{"PATH": {"/usr/bin"}}), fakeFS(files...))
		if res.Status != StatusOK && res.Remedy == "" {
			t.Errorf("files %v: status %v with no remedy", files, res.Status)
		}
	}
}

// Windows gets no verdict on libkrun. The platform check already reports that Boks has no
// Windows backend, and a second line about a file it would not use is noise.
func TestHypervisorLibraryIsSkippedWhereBoksHasNoBackend(t *testing.T) {
	for _, goos := range []string{"windows", "plan9"} {
		res := hypervisorLibraryResult(goos, "amd64",
			envOf(map[string][]string{"PATH": {"/usr/bin"}}), fakeFS("/usr/lib/libkrun.so"))
		if res.Status != StatusSkip {
			t.Errorf("goos %q: Status = %v, want skip", goos, res.Status)
		}
	}
}

func TestLooksLikeHypervisorLibrary(t *testing.T) {
	tests := []struct {
		goos, name string
		want       bool
	}{
		{"linux", "libkrun.so", true},
		{"linux", "libkrun.so.1", true},
		{"linux", "libkrun-x86_64.so", true},
		{"linux", "libkrun.so.1.18.0", true},
		// libkrunfw ships beside libkrun and is a different library. Offering to
		// symlink libkrun.so at it would be worse advice than saying nothing.
		{"linux", "libkrunfw.so.4", false},
		{"darwin", "libkrunfw.dylib", false},
		{"linux", "libc.so.6", false},
		{"linux", "krun.dll", false},
		{"darwin", "libkrun.dylib", true},
		{"darwin", "libkrun.1.dylib", true},
		{"darwin", "libkrun.so", false},
		{"windows", "krun.dll", true},
		{"windows", "KRUN.DLL", true},
		{"windows", "kernel32.dll", false},
	}
	for _, tt := range tests {
		if got := looksLikeHypervisorLibrary(tt.goos, tt.name); got != tt.want {
			t.Errorf("looksLikeHypervisorLibrary(%q, %q) = %v, want %v",
				tt.goos, tt.name, got, tt.want)
		}
	}
}

// A host with a shelf of libkrun files must not produce a remedy the size of a screen.
func TestHypervisorLibraryCapsTheNearMissList(t *testing.T) {
	var files []string
	for _, n := range []string{"1", "2", "3", "4", "5", "6", "7"} {
		files = append(files, "/usr/lib/libkrun.so."+n)
	}
	res := hypervisorLibraryResult("linux", "amd64",
		envOf(map[string][]string{"PATH": {"/usr/bin"}}), fakeFS(files...))
	if res.Status != StatusWarn {
		t.Fatalf("Status = %v, want warn", res.Status)
	}
	if !strings.Contains(res.Remedy, "and 3 more") {
		t.Errorf("remedy does not say how many were left out:\n%s", res.Remedy)
	}
	if strings.Contains(res.Remedy, "libkrun.so.7") {
		t.Errorf("remedy lists every file it found:\n%s", res.Remedy)
	}
}

// An unreadable directory is not an error to report. doctor sweeps directories it does not
// own, and a mode it does not like must not turn into a verdict about libkrun.
func TestHypervisorLibrarySurvivesUnreadableDirectories(t *testing.T) {
	fsys := libraryFS{
		exists:  func(string) bool { return false },
		entries: func(string) []string { return nil },
	}
	res := hypervisorLibraryResult("linux", "amd64",
		envOf(map[string][]string{"PATH": {"/usr/bin"}}), fsys)
	if res.Status != StatusWarn || res.Remedy == "" {
		t.Errorf("Status = %v, Remedy = %q; want a warning that explains itself", res.Status, res.Remedy)
	}
}

// The real filesystem adapter has to behave like the fake one it is tested through.
func TestHostLibraryFSReadsTheFilesystem(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "libkrun.so")
	if err := os.WriteFile(path, []byte("not really a library"), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys := hostLibraryFS()
	if !fsys.exists(path) {
		t.Errorf("exists(%q) = false for a file that is there", path)
	}
	if fsys.exists(filepath.Join(dir, "libkrun.dylib")) {
		t.Error("exists reported a file that is not there")
	}
	if names := fsys.entries(dir); len(names) != 1 || names[0] != "libkrun.so" {
		t.Errorf("entries(%q) = %v, want [libkrun.so]", dir, names)
	}
	if names := fsys.entries(filepath.Join(dir, "nope")); names != nil {
		t.Errorf("entries of a missing directory = %v, want nothing", names)
	}
}

// End to end on the running host: whatever the answer is, the check must produce one, and
// must not claim ok without naming a file that is really there.
func TestHypervisorLibraryCheckOnThisHost(t *testing.T) {
	res := hypervisorLibraryCheck().Run(context.Background(), Env{})
	switch res.Status {
	case StatusOK:
		if _, err := os.Stat(res.Detail); err != nil {
			t.Errorf("reported ok with %q, which cannot be stat-ed: %v", res.Detail, err)
		}
	case StatusWarn:
		if res.Remedy == "" {
			t.Error("warned without a remedy")
		}
	case StatusSkip:
	default:
		t.Errorf("Status = %v; this check never fails, since it cannot read containerd's environment", res.Status)
	}
}
