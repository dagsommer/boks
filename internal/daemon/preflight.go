package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/containerd/containerd/v2/defaults"
)

// The things Boks cannot fix by writing a configuration file, checked before they fail later.
//
// Everything in config.go is a setting, and a setting can simply be written correctly. What is
// left over is the residue: one directory whose location is compiled into containerd and
// cannot be moved, and one host tool whose absence quietly changes what the configuration is
// allowed to say. Both fail long after `boks daemon start` — at task start and at image unpack
// respectively — so both are worth a sentence up front.

// Note is something the user should know about the daemon that is starting. It is never fatal:
// a daemon that comes up and can pull images is useful even if it cannot yet start a task, and
// refusing to start it would remove the machine somebody is trying to debug with.
type Note struct {
	Name   string
	Detail string
	Remedy string
}

// Preflight returns what is wrong with the host that the configuration cannot express.
func Preflight(settings Settings) []Note {
	var notes []Note
	if n := shimSocketRootNote(); n != nil {
		notes = append(notes, *n)
	}
	if !settings.EROFS {
		notes = append(notes, Note{
			Name:   "mkfs.erofs",
			Detail: "not on PATH, so the erofs differ is not in the diff order",
			Remedy: "containerd's erofs differ shells out to mkfs.erofs and skips itself without it,\n" +
				"and naming a skipped differ in the diff order fails the diff service — measured\n" +
				"2026-08-15 on containerd v2.2.6, seven plugins including the diff gRPC service.\n" +
				"The daemon would keep running and answering, with no way to unpack an image.\n" +
				"So this one is configured without erofs instead, and cannot unpack a sandbox's\n" +
				"root filesystem either way. Install erofs-utils 1.8 or later (apt: erofs-utils,\n" +
				"brew: erofs-utils) and run 'boks daemon start' again.",
		})
	}
	return notes
}

// shimSocketRootNote reports whether containerd will be able to create the shim's socket.
//
// containerd derives that path from a compile-time constant — pkg/shim/util_unix.go's
// `const socketRoot = defaults.DefaultStateDir` — so no configuration setting moves it
// (containerd#12444), and this is the one part of the daemon's layout Boks cannot choose. On
// Linux it is /run/containerd and on macOS /var/run/containerd, both of which a normal user
// cannot create.
//
// The failure without this note arrives much later and reads as a Boks failure:
//
//	creating sandbox process: mkdir /var/run/containerd: permission denied
//
// docs/install.md calls the fix "the only step that needs root", and it still is — Boks does
// not elevate to do it. What Boks can do is try the harmless half first: if the directory can
// simply be created, it is created and there is nothing to report.
func shimSocketRootNote() *Note {
	root := defaults.DefaultStateDir
	if writableDir(root) {
		return nil
	}
	if err := os.MkdirAll(root, 0o700); err == nil && writableDir(root) {
		return nil
	}
	return &Note{
		Name:   "shim socket directory",
		Detail: root + " is not writable by you",
		Remedy: shimSocketRemedy(root),
	}
}

func shimSocketRemedy(root string) string {
	base := fmt.Sprintf(
		"containerd puts each shim's socket in %s, and that path is a compile-time\n"+
			"constant (containerd#12444) — no configuration moves it, so Boks cannot put it\n"+
			"under your state directory with everything else. The daemon will start and can\n"+
			"pull images; starting a sandbox will fail with\n\n"+
			"    creating sandbox process: mkdir %s: permission denied\n\n", root, root)
	if runtime.GOOS == "windows" {
		return base + "Give your account write access to that directory, or run the daemon elevated."
	}
	return base + fmt.Sprintf(
		"This is the one step that needs root, and it is needed once:\n\n"+
			"    sudo mkdir -p %s\n"+
			"    sudo chown \"$(id -u):$(id -g)\" %s", root, root)
}

// writableDir reports whether path is a directory this process can create a file in.
//
// Permission bits are not consulted, and that is deliberate: they answer the wrong question
// under a group membership, an ACL, or a read-only mount. Creating a file and removing it
// answers the question that will actually be asked later.
func writableDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	probe, err := os.CreateTemp(path, ".boks-writable-")
	if err != nil {
		return false
	}
	name := probe.Name()
	probe.Close()
	_ = os.Remove(name)
	return true
}

// ShimSocketRoot is where containerd will put shim sockets, exported so that `boks doctor` can
// name the same directory rather than repeating the constant.
func ShimSocketRoot() string { return filepath.Clean(defaults.DefaultStateDir) }
