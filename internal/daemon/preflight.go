package daemon

// The things Boks cannot fix by writing a configuration file, checked before they fail later.
//
// Everything in config.go is a setting, and a setting can simply be written correctly. What is
// left over is the residue: host tools whose absence is invisible until much later — one of
// which quietly changes what the configuration is allowed to say. They fail at task start or at
// image unpack, and not at `boks daemon start`, which is when somebody is looking. So each is
// worth a sentence up front.
//
// # The shim socket directory is no longer one of them
//
// Until 2026-10-02 this also warned that /var/run/containerd (/run/containerd on Linux) was not
// writable, and told the user to create it with sudo — "the one step that needs root". It was
// right for containerd 2.2, which compiled the shim socket directory in (containerd#12444). It
// is wrong for 2.3 and later, which is every daemon Boks can run with: the shim links 2.3.3 and
// CheckSkew refuses an older daemon. From 2.3 an unprivileged containerd picks a directory it
// can use itself ($XDG_RUNTIME_DIR/containerd/s, /run/<uid>/containerd/s, then
// /tmp/containerd-s-<uid>, created 0700 and checked to be the caller's —
// core/runtime/v2/shim_unix.go, defaultSocketDir) and hands it to the shim in the bootstrap
// parameters, which nerdbox honours (pkg/shim/manager, bparams.GetSocketDir). On macOS the old
// advice was worse than unnecessary: /var/run is emptied at every boot, so the sudo had to be
// repeated after each restart.

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
	if !settings.EROFS {
		notes = append(notes, Note{
			Name:   "mkfs.erofs",
			Detail: "not on PATH, so the erofs differ is not in the diff order",
			Remedy: "containerd's erofs differ shells out to mkfs.erofs and skips itself without it,\n" +
				"and naming a skipped differ in the diff order takes the diff service down with\n" +
				"it — so this daemon is configured without erofs instead. It will start and\n" +
				"answer, but it cannot unpack a sandbox's root filesystem either way. Install\n" +
				"erofs-utils 1.8 or later (apt: erofs-utils, brew: erofs-utils) and run\n" +
				"'boks daemon start' again.",
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
