package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// This file mirrors how the nerdbox shim finds the hypervisor library, because a check that
// looks somewhere else answers a different question than the one the user is asking.
//
// The shim does not dlopen a bare soname and let the dynamic loader resolve it. It stats a
// list of file names in a list of directories and dlopens the first *full path* that exists
// (nerdbox internal/vm/libkrun/instance.go, NewInstance, followed by openLibkrun ->
// purego.Dlopen(path)). So the only libkrun that matters is one sitting at a path the shim
// builds itself: ld.so.conf, the ELF SONAME and the loader cache have no say.
//
// Everything here is transcribed from that function rather than paraphrased, and takes goos
// and goarch as parameters so the macOS rules can be exercised on Linux — no machine on this
// project runs macOS.

// nerdboxLibraryNames returns the file names the shim stats, in the order it tries them.
//
// From NewInstance:
//
//	sharedNames := []string{fmt.Sprintf("libkrun-%s.so", arch), "libkrun.so"}
//	switch runtime.GOOS {
//	case "darwin":
//	        sharedNames = []string{fmt.Sprintf("libkrun-%s.dylib", arch), "libkrun.dylib", fmt.Sprintf("libkrun-efi-%s.dylib", arch), "libkrun-efi.dylib"}
//	case "windows":
//	        sharedNames = []string{"krun.dll"}
//	}
func nerdboxLibraryNames(goos, goarch string) []string {
	arch := nerdboxArch(goarch)
	switch goos {
	case "darwin":
		return []string{
			fmt.Sprintf("libkrun-%s.dylib", arch),
			"libkrun.dylib",
			fmt.Sprintf("libkrun-efi-%s.dylib", arch),
			"libkrun-efi.dylib",
		}
	case "windows":
		return []string{"krun.dll"}
	default:
		return []string{fmt.Sprintf("libkrun-%s.so", arch), "libkrun.so"}
	}
}

// nerdboxArch is nerdbox's kernelArch: the shim spells amd64 the way the kernel does.
func nerdboxArch(goarch string) string {
	if goarch == "amd64" {
		return "x86_64"
	}
	return goarch
}

// nerdboxSearchPaths returns the directories the shim scans, in the order it scans them.
//
// From NewInstance: PATH first, then LIBKRUN_PATH; when LIBKRUN_PATH is empty and the host is
// not Windows, LIBKRUN_PATH's half is replaced by a fixed set of defaults. macOS appends the
// Homebrew prefix in either case. An empty list element means "." — Unix shell semantics,
// which the shim implements explicitly.
//
// Note what this implies and what the remedies below therefore say: setting LIBKRUN_PATH
// *replaces* the defaults rather than adding to them, and the environment that counts is
// containerd's, since containerd spawns the shim.
//
// The list is split with filepath.SplitList, which uses the running platform's separator —
// exactly what the shim does, since it runs on the same host. goos selects the naming and
// default-directory rules, which are the parts that differ.
func nerdboxSearchPaths(goos string, getenv func(string) string) []string {
	p1 := filepath.SplitList(getenv("PATH"))
	p2 := filepath.SplitList(getenv("LIBKRUN_PATH"))
	if goos != "windows" && len(p2) == 0 {
		p2 = []string{"/usr/local/lib", "/usr/local/lib64", "/usr/lib", "/lib"}
	}
	if goos == "darwin" {
		p2 = append(p2, "/opt/homebrew/lib")
	}

	dirs := make([]string, 0, len(p1)+len(p2))
	for _, dir := range append(p1, p2...) {
		if dir == "" {
			dir = "."
		}
		dirs = append(dirs, dir)
	}
	return dirs
}

// unsearchedLibraryDirs returns directories the shim does *not* scan but where a host may
// still have libkrun installed: the multiarch and lib64 layouts distributions use, whatever
// the loader has been pointed at, and — when LIBKRUN_PATH is set — the defaults that setting
// it discarded.
//
// Nothing found here makes a host work. They are swept only so that a miss can say "it is
// installed, here, and the shim does not look there" instead of "not found", which sends
// someone to reinstall a package they already have.
func unsearchedLibraryDirs(goos string, getenv func(string) string) []string {
	var dirs []string
	switch goos {
	case "darwin":
		dirs = append(dirs, "/usr/local/lib", "/opt/local/lib")
		dirs = append(dirs, splitList(getenv("DYLD_LIBRARY_PATH"))...)
	case "windows":
		return nil
	default:
		dirs = append(dirs, "/usr/lib64", "/usr/local/lib64")
		// Multiarch layouts vary; probe the common ones rather than parsing ld.so config.
		for _, triple := range []string{"aarch64-linux-gnu", "x86_64-linux-gnu"} {
			dirs = append(dirs, "/usr/lib/"+triple, "/usr/local/lib/"+triple)
		}
		dirs = append(dirs, splitList(getenv("LD_LIBRARY_PATH"))...)
	}
	if goos != "windows" && len(filepath.SplitList(getenv("LIBKRUN_PATH"))) > 0 {
		// LIBKRUN_PATH replaced these; someone who set it may not know that.
		dirs = append(dirs, "/usr/local/lib", "/usr/local/lib64", "/usr/lib", "/lib")
	}
	return dirs
}

// splitList splits a PATH-style list, dropping empty entries.
func splitList(value string) []string {
	var out []string
	for _, part := range filepath.SplitList(value) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// libraryFS is the sliver of filesystem the sweep needs.
//
// It is injected so the sweep is tested against layouts the test constructs — a host with
// only libkrun.so.1, a macOS Homebrew prefix — rather than against whatever the machine
// running the tests happens to have installed.
type libraryFS struct {
	// exists reports whether a path can be stat-ed, which is the test the shim applies.
	exists func(path string) bool
	// entries lists the file names in a directory. An unreadable directory lists nothing:
	// a diagnostic must not fail because /usr/lib had a mode it did not like.
	entries func(dir string) []string
}

func hostLibraryFS() libraryFS {
	return libraryFS{
		exists: func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		},
		entries: func(dir string) []string {
			ents, err := os.ReadDir(dir)
			if err != nil {
				return nil
			}
			names := make([]string, 0, len(ents))
			for _, e := range ents {
				names = append(names, e.Name())
			}
			return names
		},
	}
}

// libraryNearMiss is a libkrun on the host that the shim will not load, and why not.
type libraryNearMiss struct {
	Path string
	// Reason says what is wrong with the path, in a few words. The check prints the
	// shim's full search right underneath, so this does not restate it.
	Reason string
	// SymlinkTarget is where a symlink to Path would put it under a name and in a
	// directory the shim uses. Empty when a symlink is not the answer — notably when the
	// file is already at the only path a symlink could sensibly occupy.
	SymlinkTarget string
}

// libraryScan is the outcome of sweeping the host for the hypervisor library.
type libraryScan struct {
	// Loadable is the path the shim would dlopen, or empty if it would find nothing.
	Loadable string
	// NearMisses are libkrun files found somewhere the shim will not load them from.
	// Only meaningful when Loadable is empty.
	NearMisses []libraryNearMiss
}

// scanForHypervisorLibrary reproduces the shim's search, and — only if that finds nothing —
// looks around for a libkrun the shim will not load.
func scanForHypervisorLibrary(goos, goarch string, getenv func(string) string, fsys libraryFS) libraryScan {
	names := nerdboxLibraryNames(goos, goarch)
	searched := nerdboxSearchPaths(goos, getenv)

	// The shim's own loop: directories outer, names inner, first hit wins.
	for _, dir := range searched {
		for _, name := range names {
			candidate := filepath.Join(dir, name)
			if fsys.exists(candidate) {
				return libraryScan{Loadable: candidate}
			}
		}
	}

	loadable := make(map[string]bool, len(names))
	for _, name := range names {
		loadable[name] = true
	}

	var scan libraryScan
	seen := map[string]bool{}
	sweep := func(dir string, dirIsSearched bool) {
		if dir == "" || seen[filepath.Clean(dir)] {
			return
		}
		seen[filepath.Clean(dir)] = true
		for _, name := range fsys.entries(dir) {
			if !looksLikeHypervisorLibrary(goos, name) {
				continue
			}
			// Where a symlink would have to go for the shim to find it: beside
			// the file when the directory is one the shim searches, and in a
			// default directory when it is not.
			target := filepath.Join(canonicalLibraryDir(goos), canonicalLibraryName(goos))
			if dirIsSearched {
				target = filepath.Join(dir, canonicalLibraryName(goos))
			}
			var reason string
			switch {
			case dirIsSearched && loadable[name]:
				// The name is one the shim stats, yet the stat above failed:
				// a dangling symlink, or a directory it cannot read.
				reason = "the shim stats this path and could not read it — a dangling symlink?"
			case dirIsSearched:
				reason = "not a name the shim stats"
			case loadable[name]:
				reason = "not a directory the shim searches"
			default:
				reason = "not a name the shim stats, in a directory it does not search"
			}
			path := filepath.Join(dir, name)
			if target == path {
				// Suggesting a symlink from a file to itself is worse than
				// suggesting nothing; LIBKRUN_PATH is the answer here.
				target = ""
			}
			scan.NearMisses = append(scan.NearMisses, libraryNearMiss{
				Path:          path,
				Reason:        reason,
				SymlinkTarget: target,
			})
		}
	}
	for _, dir := range searched {
		sweep(dir, true)
	}
	for _, dir := range unsearchedLibraryDirs(goos, getenv) {
		sweep(dir, false)
	}
	return scan
}

// looksLikeHypervisorLibrary reports whether a file name is a libkrun of some spelling —
// including the soname-versioned ones (libkrun.so.1, libkrun.1.dylib) that the shim never
// stats. Matching loosely is the point: the sweep exists to find what the strict search
// missed.
//
// It is not loose enough to match libkrunfw, the firmware library that ships beside libkrun.
// That one is a different thing, and pointing a symlink named libkrun.so at it would replace
// a missing library with a broken one.
func looksLikeHypervisorLibrary(goos, name string) bool {
	lower := strings.ToLower(name)
	stem, suffixed := "", false
	switch goos {
	case "darwin":
		stem, suffixed = strings.TrimPrefix(lower, "libkrun"), strings.HasSuffix(lower, ".dylib")
		if !strings.HasPrefix(lower, "libkrun") {
			return false
		}
	case "windows":
		stem, suffixed = strings.TrimPrefix(lower, "krun"), strings.HasSuffix(lower, ".dll")
		if !strings.HasPrefix(lower, "krun") {
			return false
		}
	default:
		if !strings.HasPrefix(lower, "libkrun") {
			return false
		}
		stem = strings.TrimPrefix(lower, "libkrun")
		suffixed = strings.HasSuffix(lower, ".so") || strings.Contains(lower, ".so.")
	}
	// What follows the name must be an extension or an arch suffix, never more letters:
	// libkrun-x86_64.so and libkrun.so.1 are libkrun, libkrunfw.so.4 is not.
	return suffixed && (strings.HasPrefix(stem, ".") || strings.HasPrefix(stem, "-"))
}

// canonicalLibraryName and canonicalLibraryDir are the name and directory to suggest when a
// libkrun is on the host under a name or in a directory the shim will not load: the plain
// name from the middle of the shim's list, in a directory the shim searches by default.
func canonicalLibraryName(goos string) string {
	switch goos {
	case "darwin":
		return "libkrun.dylib"
	case "windows":
		return "krun.dll"
	default:
		return "libkrun.so"
	}
}

func canonicalLibraryDir(goos string) string {
	if goos == "darwin" {
		return "/usr/local/lib"
	}
	return "/usr/lib"
}
