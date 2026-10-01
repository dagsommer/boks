package sandbox

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Resolving an image's USER *name* on a host that cannot mount the image.
//
// # Why this exists
//
// applyImageUser can only honour the numbers a USER string states outright. An image saying
// `USER agent` — which is most images built for agents, including most kit-defined ones — reached the
// spec as uid 0 with the name left in Process.User.Username, which no runtime reads. Under the
// host-uid override that was invisible; under an idmapped workspace it is the whole question,
// because the mapping has to name the container's uid *before* the container exists, so it
// cannot be left for the guest to work out (see idmapWorkspaceMounts).
//
// containerd does this on Linux by mounting the image's snapshot on the host. That mount is
// what macOS and Windows cannot do — but the layers it would have been assembled from are
// already sitting in the content store as ordinary tar streams, and two small text files are
// all that is needed from them. So this reads /etc/passwd and /etc/group out of the layers
// directly, top layer first, honouring whiteouts the way the overlay would have, and resolves
// the name exactly as containerd's WithUser and WithAdditionalGIDs would on Linux.
//
// # What it costs
//
// Scanning a layer means decompressing it, and a layer has to be read to its end to know
// it does not contain a file. The scan stops at the first layer, from the top, that decides
// both files; for an image that adds its user in a late, small layer — the usual shape — that
// is a few megabytes. It happens once, at container creation, never on re-attach.
//
// # How it fails
//
// Open. Any error — a layer missing from the content store, an unreadable tar, a name absent
// from the image's passwd — leaves the spec exactly as applyImageUser left it, which is the
// behaviour before this file existed. What it never does is guess a uid.

// etcPasswd and etcGroup are the two paths read, in the layer's own (relative) spelling.
const (
	etcPasswd = "etc/passwd"
	etcGroup  = "etc/group"
)

// maxEtcFileSize bounds what is buffered for either file. A passwd file of this size is
// already absurd; one larger is treated as absent rather than read into memory.
const maxEtcFileSize = 4 << 20

// needsImageUserFiles reports whether resolving userstr needs the image's passwd or group:
// any half that is a name, or a bare uid, whose primary group lives in passwd.
func needsImageUserFiles(userstr string) bool {
	if userstr == "" {
		return false
	}
	_, haveUID, _, haveGID := numericIDs(userstr)
	userPart, groupPart, hasGroup := strings.Cut(userstr, ":")
	if !haveUID && userPart != "" {
		return true
	}
	if hasGroup {
		return !haveGID && groupPart != ""
	}
	return true
}

// resolveImageUserFromLayers fills in the uid, gid and supplementary groups for an image's
// USER from the image's own /etc/passwd and /etc/group. It changes s only on success.
func resolveImageUserFromLayers(ctx context.Context, image client.Image, s *specs.Spec, userstr string) error {
	manifest, err := images.Manifest(ctx, image.ContentStore(), image.Target(), image.Platform())
	if err != nil {
		return fmt.Errorf("reading the manifest of %s: %w", image.Name(), err)
	}
	cs := image.ContentStore()
	layers := manifest.Layers
	open := func(i int) (io.ReadCloser, error) {
		ra, err := cs.ReaderAt(ctx, layers[i])
		if err != nil {
			return nil, err
		}
		return readerAtCloser{Reader: content.NewReader(ra), Closer: ra}, nil
	}
	files, err := findLayerFiles(len(layers), open, etcPasswd, etcGroup)
	if err != nil {
		return fmt.Errorf("reading /etc/passwd and /etc/group from %s: %w", image.Name(), err)
	}
	user, err := resolveUserString(userstr, files[etcPasswd], files[etcGroup])
	if err != nil {
		return err
	}
	s.Process.User.UID = user.uid
	s.Process.User.GID = user.gid
	s.Process.User.AdditionalGids = user.additionalGids
	ensureAdditionalGIDs(s)
	return nil
}

type readerAtCloser struct {
	io.Reader
	io.Closer
}

// findLayerFiles returns the content of each of names as the merged filesystem would show it,
// given n layers where open(0) is the bottom one. A name whited out, or absent everywhere,
// maps to nil. Layers are scanned from the top and the scan stops once every name is decided.
func findLayerFiles(n int, open func(i int) (io.ReadCloser, error), names ...string) (map[string][]byte, error) {
	found := make(map[string][]byte, len(names))
	decided := make(map[string]bool, len(names))
	for i := n - 1; i >= 0 && len(decided) < len(names); i-- {
		layer, err := scanLayer(open, i, names)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if decided[name] {
				continue
			}
			if data, ok := layer.files[name]; ok {
				found[name], decided[name] = data, true
				continue
			}
			if layer.hides(name) {
				decided[name] = true
			}
		}
	}
	return found, nil
}

// layerScan is what one layer says about the names asked for: the files it carries, and which
// paths it whites out or makes opaque, so that lower layers are hidden behind it.
type layerScan struct {
	files     map[string][]byte // a regular file's content, or nil for any other type
	whiteouts map[string]bool   // ".wh.<name>": the path is deleted
	opaque    map[string]bool   // ".wh..wh..opq": the directory's lower contents are hidden
}

// hides reports whether this layer hides name, or any directory above it, from the layers
// below.
func (l layerScan) hides(name string) bool {
	if l.whiteouts[name] {
		return true
	}
	for dir := path.Dir(name); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if l.whiteouts[dir] || l.opaque[dir] {
			return true
		}
	}
	return false
}

func scanLayer(open func(i int) (io.ReadCloser, error), i int, names []string) (layerScan, error) {
	want := make(map[string]bool, len(names))
	for _, name := range names {
		want[name] = true
	}
	scan := layerScan{files: map[string][]byte{}, whiteouts: map[string]bool{}, opaque: map[string]bool{}}

	raw, err := open(i)
	if err != nil {
		return scan, fmt.Errorf("opening layer %d: %w", i, err)
	}
	defer raw.Close()
	stream, err := compression.DecompressStream(raw)
	if err != nil {
		return scan, fmt.Errorf("decompressing layer %d: %w", i, err)
	}
	defer stream.Close()

	tr := tar.NewReader(stream)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return scan, nil
		}
		if err != nil {
			return scan, fmt.Errorf("reading layer %d: %w", i, err)
		}
		name := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
		dir, base := path.Dir(name), path.Base(name)
		switch {
		case base == ".wh..wh..opq":
			scan.opaque[dir] = true
		case strings.HasPrefix(base, ".wh."):
			scan.whiteouts[path.Join(dir, strings.TrimPrefix(base, ".wh."))] = true
		case want[name]:
			// Anything but a regular file — a symlink, most often — is recorded as present
			// and unreadable rather than followed: it still hides the layers below.
			if hdr.Typeflag != tar.TypeReg || hdr.Size > maxEtcFileSize {
				scan.files[name] = nil
				continue
			}
			data, err := io.ReadAll(io.LimitReader(tr, maxEtcFileSize))
			if err != nil {
				return scan, fmt.Errorf("reading %s from layer %d: %w", name, i, err)
			}
			scan.files[name] = data
		}
	}
}

// resolvedUser is an image USER turned into the ids crun reads.
type resolvedUser struct {
	uid, gid       uint32
	additionalGids []uint32
}

// passwdEntry and groupEntry are the fields of /etc/passwd and /etc/group this reads.
type passwdEntry struct {
	name     string
	uid, gid uint32
}

type groupEntry struct {
	name    string
	gid     uint32
	members []string
}

// resolveUserString resolves an image's USER against its passwd and group files, following
// containerd's WithUser on Linux: every form the image spec allows, each half decided on its
// own, a bare user taking its primary group from passwd and a bare uid absent from passwd
// taking gid 0. Supplementary groups are the groups naming the user as a member, as in
// WithAdditionalGIDs. A name that cannot be found is an error, never uid 0.
func resolveUserString(userstr string, passwd, group []byte) (resolvedUser, error) {
	users, groups := parsePasswd(passwd), parseGroup(group)
	userPart, groupPart, hasGroup := strings.Cut(userstr, ":")

	var u resolvedUser
	var username string
	if id, ok := parseID(userPart); ok {
		u.uid = id
		for _, e := range users {
			if e.uid == id {
				u.gid, username = e.gid, e.name
				break
			}
		}
	} else {
		var found bool
		for _, e := range users {
			if e.name == userPart {
				u.uid, u.gid, username, found = e.uid, e.gid, e.name, true
				break
			}
		}
		if !found {
			return u, fmt.Errorf("user %q is not in the image's /etc/passwd", userPart)
		}
	}

	if hasGroup {
		if id, ok := parseID(groupPart); ok {
			u.gid = id
		} else {
			var found bool
			for _, g := range groups {
				if g.name == groupPart {
					u.gid, found = g.gid, true
					break
				}
			}
			if !found {
				return u, fmt.Errorf("group %q is not in the image's /etc/group", groupPart)
			}
		}
	}

	if username != "" {
		for _, g := range groups {
			for _, m := range g.members {
				if m == username && g.gid != u.gid {
					u.additionalGids = append(u.additionalGids, g.gid)
					break
				}
			}
		}
	}
	return u, nil
}

func parseID(s string) (uint32, bool) {
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 || v > 1<<31-1 {
		return 0, false
	}
	return uint32(v), true
}

// parsePasswd reads name:password:uid:gid:... lines, skipping any it cannot read.
func parsePasswd(data []byte) []passwdEntry {
	var out []passwdEntry
	eachLine(data, func(fields []string) {
		if len(fields) < 4 {
			return
		}
		uid, ok1 := parseID(fields[2])
		gid, ok2 := parseID(fields[3])
		if ok1 && ok2 {
			out = append(out, passwdEntry{name: fields[0], uid: uid, gid: gid})
		}
	})
	return out
}

// parseGroup reads name:password:gid:member,member lines, skipping any it cannot read.
func parseGroup(data []byte) []groupEntry {
	var out []groupEntry
	eachLine(data, func(fields []string) {
		if len(fields) < 3 {
			return
		}
		gid, ok := parseID(fields[2])
		if !ok {
			return
		}
		g := groupEntry{name: fields[0], gid: gid}
		if len(fields) > 3 && fields[3] != "" {
			g.members = strings.Split(fields[3], ",")
		}
		out = append(out, g)
	})
	return out
}

func eachLine(data []byte, fn func(fields []string)) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fn(strings.Split(line, ":"))
	}
}
