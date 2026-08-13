//go:build windows

package doctor

import "testing"

// NOT RUN. This project has no Windows machine; these tests are compile-checked with
// `GOOS=windows go vet ./...` and nothing more. They are here so that the Windows wiring is
// stated as an expectation somewhere executable, rather than only in a comment.

// The shim looks for exactly one filename on Windows. Adding a second — libkrun.dll, say —
// would make doctor accept a file the shim will not load.
func TestWindowsHypervisorLibraryIsKrunDLL(t *testing.T) {
	names := hypervisorLibraryNames()
	if len(names) != 1 || names[0] != "krun.dll" {
		t.Errorf("hypervisorLibraryNames() = %v, want exactly [krun.dll]", names)
	}
}

// PATH and LIBKRUN_PATH are the shim's whole search on Windows: it resolves a path from them
// and hands it to LoadLibrary. There are no default prefixes to fall back on, so anything
// else this returned would be a place the shim never looks.
func TestWindowsHypervisorLibrarySearchPathsAreThePATHAndLIBKRUNPATH(t *testing.T) {
	t.Setenv("PATH", `C:\bin`)
	t.Setenv("LIBKRUN_PATH", `C:\libkrun`)

	got := hypervisorLibrarySearchPaths()
	want := []string{`C:\bin`, `C:\libkrun`}
	if len(got) != len(want) {
		t.Fatalf("hypervisorLibrarySearchPaths() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hypervisorLibrarySearchPaths() = %v, want %v", got, want)
		}
	}
}
