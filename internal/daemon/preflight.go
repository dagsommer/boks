package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/v2/defaults"
)

// The things Boks cannot fix by writing a configuration file, checked before they fail later.
//
// Everything in config.go is a setting, and a setting can simply be written correctly. What is
// left over is the residue: one directory whose location is compiled into containerd and
// cannot be moved, and two host tools whose absence is invisible until much later — one of
// which quietly changes what the configuration is allowed to say. The directory and one of the
// tools fail at task start, the other at image unpack, and none of the three at `boks daemon
// start`, which is when somebody is looking. So each is worth a sentence up front.

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
	if n := shimSocketRootNote(settings.GOOS); n != nil {
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
	if n := writableLayerNote(settings); n != nil {
		notes = append(notes, *n)
	}
	return notes
}

// writableLayerNote reports whether containerd will be able to format a sandbox's writable
// layer, on the platforms where it has to format one at all.
//
// This is the note that was missing for the whole of the Windows bring-up. Off Linux the erofs
// snapshotter runs in block mode (see blockWritableLayer), so before any task starts, the mount
// manager creates `<erofs root>/snapshots/<id>/rwlayer.img`, truncates it to the requested size
// and runs `mkfs.ext4` on it. There is no configuration that turns this off and nothing checks
// the binary is there. Measured on Windows 11 on 2026-08-16, from the v0.1.0 archive, after a
// complete and successful image pull:
//
//	boks: starting the io.containerd.nerdbox.v1 runtime failed: failed format
//	"...\io.containerd.snapshotter.v1.erofs\snapshots\11\rwlayer.img":
//	mkfs.ext4 failed: : exec: "mkfs.ext4": executable file not found in %PATH%
//
// macOS has exactly the same gap and has never reported it. Every macOS run recorded in
// docs/verification.md happened on a host with e2fsprogs 1.47.4 already installed
// (docs/verification.md:39) — Homebrew's erofs-utils does not pull it in, no Boks install path
// installs it, and `boks doctor` was green about a host that would have failed here.
//
// It is a Note and not a hard failure for the reason at the top of this file: a daemon that
// comes up and can pull images is still worth having, and this one may be fixed without
// restarting the daemon, since containerd resolves mkfs.ext4 per invocation.
func writableLayerNote(settings Settings) *Note {
	if !blockWritableLayer(settings.GOOS) || settings.Ext4 {
		return nil
	}
	remedy := "On " + settings.GOOS + " the erofs snapshotter gives every active snapshot its own ext4\n" +
		"image for the writable layer (containerd's defaultWritableSize is 64 MiB off Linux\n" +
		"and 0 on it), and containerd's mount manager formats that image by running\n" +
		"mkfs.ext4. Nothing turns this off. The daemon will start and can pull images;\n" +
		"'boks run' will fail at task start with\n\n" +
		"    failed format \"<snapshots dir>/<id>/rwlayer.img\": mkfs.ext4 failed\n\n"
	switch settings.GOOS {
	case "windows":
		remedy += "The Windows archive ships mkfs.ext4.exe beside boks.exe. If it is missing, take it\n" +
			"from the boks-runtime zip for this release, or set BOKS_RUNTIME_DIR to the\n" +
			"directory holding it."
	case "darwin":
		remedy += "Install e2fsprogs (brew: e2fsprogs). Homebrew keeps it keg-only, so mkfs.ext4\n" +
			"lands in $(brew --prefix e2fsprogs)/sbin and not on any PATH — Boks adds that\n" +
			"directory to the daemon's PATH itself, so installing it is enough."
	default:
		remedy += "Install e2fsprogs and run 'boks daemon start' again."
	}
	return &Note{
		Name:   ext4Tool,
		Detail: "not on containerd's PATH, so no sandbox can start",
		Remedy: remedy,
	}
}

// shimSocketRootNote reports whether containerd will be able to create the shim's socket.
//
// containerd derives that path from a compile-time constant — pkg/shim/util_unix.go's
// `socketRoot`, which is `filepath.Join(defaults.DefaultStateDir, "s")` — so no configuration
// setting moves it (containerd#12444), and this is the one part of the daemon's layout Boks
// cannot choose. On Linux it is /run/containerd and on macOS /var/run/containerd, both of which
// a normal user cannot create.
//
// The failure without this note arrives much later and reads as a Boks failure:
//
//	creating sandbox process: mkdir /var/run/containerd: permission denied
//
// docs/install.md calls the fix "the only step that needs root", and it still is — Boks does
// not elevate to do it. What Boks can do is try the harmless half first: if the directory can
// simply be created, it is created and there is nothing to report.
//
// # Why it says nothing on Windows
//
// It used to, and it was wrong. `boks daemon start` on Windows 11 warned about
// `C:\ProgramData\containerd\state` on 2026-08-16, on a machine where that path did not exist
// and where sandboxes then started, ran, enforced policy and stopped without it. The check was
// asking a question with no meaning on that host: a shim on Windows is reached over a **named
// pipe**, which lives in the kernel's object namespace and not on any filesystem. containerd's
// own pkg/shim/util_windows.go has no socketRoot, no SocketAddress and no writeSocketDir, and
// its RemoveSocket is a no-op — the whole mechanism this note is about is Unix-only. The only
// Windows use of DefaultStateDir is a *default* for `--state`, which Boks overrides anyway
// (see config.go).
//
// It was also the wrong remedy in the wrong direction: it told an unelevated user to give
// themselves write access to a machine directory or "run the daemon elevated", on the one
// platform where Boks has been verified end to end with no elevation at all. A warning that
// fires on a working host teaches its reader to ignore the ones that mean something, which is
// the argument internal/cli/notice.go already makes about volume.
//
// The platform is a parameter rather than a read of runtime.GOOS so that the Windows case can
// be constructed by a test on a machine that is not Windows — which is the only kind of machine
// this repository's tests have ever run on.
func shimSocketRootNote(goos string) *Note {
	if goos == "windows" {
		return nil
	}
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
	return fmt.Sprintf(
		"containerd puts each shim's socket under %s, and that path is a compile-time\n"+
			"constant (containerd#12444) — no configuration moves it, so Boks cannot put it\n"+
			"under your state directory with everything else. The daemon will start and can\n"+
			"pull images; starting a sandbox will fail with\n\n"+
			"    creating sandbox process: mkdir %s: permission denied\n\n"+
			"This is the one step that needs root, and it is needed once:\n\n"+
			"    sudo mkdir -p %s\n"+
			"    sudo chown \"$(id -u):$(id -g)\" %s", root, root, root, root)
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
