package daemon

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/dagsommer/boks/internal/runtimecfg"
)

// The containerd configuration Boks writes for the daemon it runs.
//
// Every setting below exists because a real run fell over without it, and each is recorded in
// the generated file next to the failure it prevents — the file is what somebody reads when
// they are debugging, so the reasons belong there and not only here.
//
// This is generated rather than shipped as a static file, and that is a decision rather than a
// convenience. Three of the settings cannot be written down ahead of time:
//
//   - the uid and gid are the *running user's*, and a static file would have to guess;
//   - the paths are under the user's state directory, which varies by platform and by
//     BOKS_STATE_DIR;
//   - whether the erofs differ may be named at all depends on whether mkfs.erofs is on PATH,
//     and naming it when it is absent takes the whole daemon down (see diffOrder).
//
// packaging/containerd-windows/config.toml is the hand-written ancestor of this and remains
// the reference for the Windows half. It is not superseded: it is what a person runs
// containerd with by hand when debugging with ctr, and its comments are longer than these.

// Settings is everything that varies between one host's containerd configuration and
// another's. It is a struct rather than a set of arguments so that the tests can construct a
// Windows configuration on Linux, and a host without mkfs.erofs on one that has it.
type Settings struct {
	// GOOS is the platform the configuration is for.
	GOOS string
	// GOARCH is the architecture, which appears in the unpack platform.
	GOARCH string
	// Root and State are containerd's two top-level directories.
	Root, State string
	// Address is the gRPC endpoint: a socket path on Unix, a named pipe on Windows.
	Address string
	// TTRPCAddress is the ttrpc endpoint. Empty means "let containerd derive it", which is
	// only safe when UID and GID do not need setting — see the comment in render.
	TTRPCAddress string
	// UID and GID are the ownership containerd will chmod and chown both listeners to.
	// They are pointers because 0 is a real answer (a daemon running as root) and has to
	// be distinguishable from "this platform has no such concept".
	UID, GID *int
	// EROFS reports whether mkfs.erofs was found. When it was not, the erofs differ must
	// not be named in the diff order.
	EROFS bool
}

// diffOrder is the diff-service order for a platform, and it is the setting the whole file
// exists for.
//
// Boks pulls with client.Pull, whose unpacker takes its Applier from the *daemon's* diff
// service (containerd client/pull.go: `Applier: c.DiffService()`). So the order below is not a
// detail of some other code path — it is the code path. containerd v2.2.6's default order is:
//
//	linux    ['walking']                 plugins/services/diff/service_unix.go
//	darwin   ['erofs', 'walking']        plugins/services/diff/service_darwin.go
//	windows  ['windows', 'windows-lcow'] plugins/services/diff/service_windows.go
//
// Linux and Windows therefore never ask the erofs differ anything, however healthy it looks
// in `ctr plugins ls`. The walk reaches a differ that cannot read a stacked EROFS layer and
// the unpack fails naming that differ, not the order.
//
// erofs goes FIRST because it is the only differ here that declines politely: shown a mount
// that is not an EROFS layer it returns ErrNotImplemented and the walk continues. The walking,
// windows and windows-lcow differs fail hard instead, which ends the walk — so erofs anywhere
// but first is erofs never.
//
// And erofs is omitted entirely when mkfs.erofs is absent. This is not tidiness, and the
// consequence is worse than a crash would be.
//
// Measured on 2026-08-15, containerd v2.2.6 on linux/arm64, with this order and a PATH with no
// mkfs.erofs on it. The erofs differ skips itself, by its own account:
//
//	skip loading plugin  error="failed to check mkfs.erofs availability: failed to run
//	  mkfs.erofs --help: exec: \"mkfs.erofs\": executable file not found in $PATH: skip plugin"
//	  id=io.containerd.differ.v1.erofs
//	failed to load plugin  error="needed differ not loaded: erofs"
//	  id=io.containerd.service.v1.diff-service
//
// Seven plugins then fail: the diff service, io.containerd.grpc.v1.diff, cri.v1.images,
// grpc.v1.cri, grpc.v1.sandbox-controllers, the restart monitor and the podsandbox controller.
// Note what does *not* happen — io.containerd.metadata.v1.bolt survives, unlike the Windows
// cimfs cascade that takes about forty plugins with it, and **the daemon stays running**. It
// answers its socket, reports its version, and lists the erofs snapshotter as available.
//
// That is why this matters more than a startup failure would. `boks doctor` would report
// containerd ok and snapshotter ok, both truthfully, and the first `boks run` would fail
// inside an image unpack — because client.Pull applies layers through the diff service that
// is no longer there. Writing that configuration onto a host whose only sin is not having
// erofs-utils installed would be Boks quietly breaking the daemon it just offered to manage.
//
// The patched containerd in packaging/containerd-windows drops such a differ with a warning
// instead (patch 0002), but that build is Windows-only and this file must be correct against
// the stock daemon a user already has.
func diffOrder(goos string, erofs bool) []string {
	var rest []string
	switch goos {
	case "windows":
		rest = []string{"windows", "windows-lcow"}
	default:
		rest = []string{"walking"}
	}
	if !erofs {
		return rest
	}
	return append([]string{runtimecfg.Snapshotter}, rest...)
}

// disabledPlugins is what must not load for the daemon to come up at all.
func disabledPlugins(goos string) []string {
	if goos != "windows" {
		return nil
	}
	// Unelevated, the cimfs snapshotter dies at init on "A required privilege is not held
	// by the client". That would be survivable alone, except the bolt metadata plugin
	// requires *every* snapshotter, so one failing snapshotter fails bolt and about forty
	// plugins with it — including the erofs differ, which then reads as an erofs problem.
	// Both entries are needed: the differ is a separate plugin and is useless without its
	// snapshotter.
	return []string{
		"io.containerd.snapshotter.v1.cimfs",
		"io.containerd.differ.v1.cimfs",
	}
}

// render writes the configuration file.
//
// The output is deterministic for a given Settings — no map iteration, no timestamps — so that
// `boks daemon config` can be diffed against what is on disk, and so a test can assert the
// whole file rather than grep it.
func render(s Settings) (string, error) {
	for _, p := range []string{s.Root, s.State, s.Address, s.TTRPCAddress} {
		if strings.Contains(p, "'") {
			// TOML literal strings cannot contain a single quote, and the basic
			// strings that could would need Windows backslashes escaped. A path with
			// a quote in it is pathological; refusing it is better than emitting a
			// file containerd will reject with a parse error nobody can act on.
			return "", fmt.Errorf("cannot write a containerd configuration for a path containing a single quote: %s", p)
		}
	}

	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("# containerd configuration generated by boks. Do not edit: `boks daemon start`\n")
	w("# rewrites this file every time, so a change here is lost at the next start.\n")
	w("#\n")
	w("# This daemon is Boks' own. It has its own root, its own state and its own endpoint,\n")
	w("# so it cannot disturb — or be disturbed by — a containerd that Docker, Kubernetes or\n")
	w("# the distribution's own package is running.\n")
	w("#\n")
	w("# To drive it by hand:\n")
	w("#\n")
	w("#     ctr --address %s --namespace %s plugins ls\n", s.Address, runtimecfg.Namespace)
	w("\n")
	w("version = 3\n")
	w("root = '%s'\n", s.Root)
	w("state = '%s'\n", s.State)
	w("\n")

	if plugins := disabledPlugins(s.GOOS); len(plugins) > 0 {
		w("# Unelevated, the cimfs snapshotter fails at init, and the bolt metadata plugin\n")
		w("# requires every snapshotter — so one failing snapshotter takes about forty plugins\n")
		w("# with it, including the erofs differ. Turning cimfs off is the whole fix.\n")
		w("disabled_plugins = [\n")
		for _, p := range plugins {
			w("  '%s',\n", p)
		}
		w("]\n\n")
	}

	w("# The endpoint. A socket of Boks' own, so nothing else answers on it.\n")
	w("[grpc]\n")
	w("  address = '%s'\n", s.Address)
	if s.UID != nil && s.GID != nil {
		w("  uid = %d\n", *s.UID)
		w("  gid = %d\n", *s.GID)
	}
	w("\n")

	if s.TTRPCAddress != "" {
		w("# The ttrpc endpoint, spelled out rather than left to be derived.\n")
		w("#\n")
		w("# containerd derives it as <grpc address>.ttrpc and, crucially, copies [grpc]'s uid\n")
		w("# and gid with it ONLY when this section is absent (cmd/containerd/command/main.go).\n")
		w("# Both listeners are then created through sys.GetLocalListener, which ends in an\n")
		w("# unconditional os.Chown(path, uid, gid) — so a rootless daemon with the default uid\n")
		w("# of 0 dies before it serves anything:\n")
		w("#\n")
		w("#     chown /…/containerd.sock.ttrpc: operation not permitted\n")
		w("#\n")
		w("# The ttrpc listener is created first, which is why the error names that socket and\n")
		w("# not the gRPC one. Both need the uid and gid below.\n")
		w("[ttrpc]\n")
		w("  address = '%s'\n", s.TTRPCAddress)
		if s.UID != nil && s.GID != nil {
			w("  uid = %d\n", *s.UID)
			w("  gid = %d\n", *s.GID)
		}
		w("\n")
	}

	order := diffOrder(s.GOOS, s.EROFS)
	w("# The diff-service order, which is the setting this file exists for.\n")
	w("#\n")
	w("# Boks unpacks through client.Pull, whose unpacker applies layers with the daemon's own\n")
	w("# diff service. containerd's default order here is ['walking'] on Linux and\n")
	w("# ['windows', 'windows-lcow'] on Windows — the erofs differ is in neither, so it is never\n")
	w("# asked anything and the walk reaches a differ that cannot read a stacked EROFS layer.\n")
	w("#\n")
	w("# erofs goes first because it is the only differ here that declines politely: shown a\n")
	w("# mount that is not an EROFS layer it returns ErrNotImplemented and the walk continues.\n")
	w("# The others fail hard, ending the walk, so erofs anywhere but first is erofs never.\n")
	if !s.EROFS {
		w("#\n")
		w("# mkfs.erofs was NOT found on this host when this file was written, so erofs is\n")
		w("# deliberately absent from the order below. The erofs differ skips itself without\n")
		w("# mkfs.erofs, and naming a skipped differ fails the diff service outright:\n")
		w("#\n")
		w("#     needed differ not loaded: erofs\n")
		w("#\n")
		w("# The daemon then keeps running and answering — it just has no diff service, so an\n")
		w("# image unpack fails and nothing before it says why. Install erofs-utils 1.8 or\n")
		w("# later and run 'boks daemon start' again; this file is rewritten each time.\n")
	}
	w("[plugins.'io.containerd.service.v1.diff-service']\n")
	w("  default = [%s]\n", quoteList(order))
	w("\n")

	// The transfer service's unpack_config is deliberately NOT set.
	//
	// It is what `ctr` uses, and on Windows it had to be corrected — see
	// packaging/containerd-windows/config.toml. Boks does not go through it: client.Pull
	// builds its own unpack.Platform from the snapshotter the caller asked for. Setting it
	// here would buy nothing for Boks and cost something real, because setting it in TOML
	// *replaces* the built-in list rather than adding to it, and the `optional` key that
	// makes an erofs entry survive a host without mkfs.erofs exists only in this project's
	// patched containerd (patch 0003), not in the stock daemon this file must work with.
	w("# Note: [plugins.'io.containerd.transfer.v1.local'].unpack_config is deliberately not\n")
	w("# set. That is the `ctr` path, not Boks' — client.Pull builds its own unpacker — and\n")
	w("# setting it replaces containerd's built-in list rather than extending it.\n")
	return b.String(), nil
}

// quoteList renders a TOML array of literal strings.
func quoteList(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "'" + v + "'"
	}
	return strings.Join(quoted, ", ")
}

// Config returns the containerd configuration Boks would write for this host right now.
//
// It is exported for `boks daemon config`, which exists because the most useful thing to hand
// somebody debugging a daemon is the file it is running with and the reasons for each line.
func Config(stateDir string) (string, error) {
	return render(settingsFor(Dir(stateDir), HasEROFS()))
}

// settingsFor builds the Settings for a host whose state lives under dir.
//
// uid and gid are read from the running process on Unix and left unset on Windows, which has
// neither and whose named pipe is not chowned at all.
func settingsFor(dir string, erofs bool) Settings {
	s := Settings{
		GOOS:   runtime.GOOS,
		GOARCH: runtime.GOARCH,
		Root:   filepath.Join(dir, "root"),
		State:  filepath.Join(dir, "state"),
		EROFS:  erofs,
	}
	s.Address = addressIn(dir)
	if runtime.GOOS != "windows" {
		s.TTRPCAddress = s.Address + ttrpcSuffix
		uid, gid := currentIDs()
		s.UID, s.GID = &uid, &gid
	}
	return s
}
