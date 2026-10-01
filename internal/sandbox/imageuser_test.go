package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"reflect"
	"testing"
)

const testPasswd = `root:x:0:0:root:/root:/bin/bash
nginx:x:101:101:nginx:/nonexistent:/bin/false
agent:x:1000:1000::/home/agent:/bin/bash
`

const testGroup = `root:x:0:
sudo:x:27:agent
docker:x:999:someone,agent
agent:x:1000:agent
nginx:x:101:
`

// A typical kit image's shape — `USER agent` — is the case this exists for, and every other form
// the image spec allows has to agree with what containerd's WithUser would produce on Linux.
func TestResolveUserString(t *testing.T) {
	for _, tc := range []struct {
		user string
		want resolvedUser
	}{
		{"agent", resolvedUser{1000, 1000, []uint32{27, 999}}},
		{"1000", resolvedUser{1000, 1000, []uint32{27, 999}}},
		{"101", resolvedUser{101, 101, nil}},
		{"agent:27", resolvedUser{1000, 27, []uint32{999, 1000}}}, // agent is listed in its own group
		{"agent:sudo", resolvedUser{1000, 27, []uint32{999, 1000}}},
		{"1000:nginx", resolvedUser{1000, 101, []uint32{27, 999, 1000}}},
		{"4242", resolvedUser{4242, 0, nil}}, // a uid absent from passwd takes gid 0
	} {
		t.Run(tc.user, func(t *testing.T) {
			got, err := resolveUserString(tc.user, []byte(testPasswd), []byte(testGroup))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("resolveUserString(%q) = %+v, want %+v", tc.user, got, tc.want)
			}
		})
	}
}

// A name that cannot be found must be an error, never uid 0 — that silent root is the
// failure the whole file is replacing.
func TestResolveUserStringRefusesToGuess(t *testing.T) {
	for _, user := range []string{"ghost", "agent:ghosts", "1000:ghosts"} {
		if got, err := resolveUserString(user, []byte(testPasswd), []byte(testGroup)); err == nil {
			t.Errorf("resolveUserString(%q) = %+v, want an error", user, got)
		}
	}
	if got, err := resolveUserString("agent", nil, nil); err == nil {
		t.Errorf("resolveUserString with no passwd = %+v, want an error", got)
	}
}

func TestNeedsImageUserFiles(t *testing.T) {
	for user, want := range map[string]bool{
		"":           false,
		"1000:1000":  false,
		"agent":      true,
		"1000":       true, // the primary group is in passwd
		"agent:1000": true,
		"1000:staff": true,
	} {
		if got := needsImageUserFiles(user); got != want {
			t.Errorf("needsImageUserFiles(%q) = %v, want %v", user, got, want)
		}
	}
}

type tarEntry struct {
	name    string
	content string
	typ     byte
}

func layerBytes(t *testing.T, gz bool, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.Writer = &buf
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(&buf)
		w = zw
	}
	tw := tar.NewWriter(w)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: typ, Size: int64(len(e.content))}
		if typ == tar.TypeSymlink {
			hdr.Size, hdr.Linkname = 0, "/elsewhere"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// opener serves layers bottom-first and records which were opened.
func opener(layers [][]byte, opened *[]int) func(int) (io.ReadCloser, error) {
	return func(i int) (io.ReadCloser, error) {
		*opened = append(*opened, i)
		return io.NopCloser(bytes.NewReader(layers[i])), nil
	}
}

func TestFindLayerFilesTakesTheTopmostCopy(t *testing.T) {
	layers := [][]byte{
		layerBytes(t, true, tarEntry{name: "etc/passwd", content: "old"}, tarEntry{name: "etc/group", content: "base-group"}),
		layerBytes(t, false, tarEntry{name: "./usr/bin/x", content: "bin"}),
		layerBytes(t, true, tarEntry{name: "./etc/passwd", content: "new"}),
	}
	var opened []int
	got, err := findLayerFiles(len(layers), opener(layers, &opened), etcPasswd, etcGroup)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[etcPasswd]) != "new" || string(got[etcGroup]) != "base-group" {
		t.Errorf("passwd=%q group=%q, want the top layer's passwd and the base's group", got[etcPasswd], got[etcGroup])
	}
}

// The scan stops as soon as both files are decided: a base layer of hundreds of megabytes
// is never decompressed when the image's user was added on top of it.
func TestFindLayerFilesStopsOnceDecided(t *testing.T) {
	layers := [][]byte{
		layerBytes(t, true, tarEntry{name: "etc/passwd", content: "base"}),
		layerBytes(t, true, tarEntry{name: "etc/passwd", content: "top"}, tarEntry{name: "etc/group", content: "top"}),
	}
	var opened []int
	if _, err := findLayerFiles(len(layers), opener(layers, &opened), etcPasswd, etcGroup); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(opened, []int{1}) {
		t.Errorf("opened layers %v, want only the top one", opened)
	}
}

func TestFindLayerFilesHonoursWhiteouts(t *testing.T) {
	base := layerBytes(t, true, tarEntry{name: "etc/passwd", content: "base"}, tarEntry{name: "etc/group", content: "base"})
	for _, tc := range []struct {
		name string
		top  []byte
	}{
		{"file whiteout", layerBytes(t, true, tarEntry{name: "etc/.wh.passwd"}, tarEntry{name: "etc/.wh.group"})},
		{"opaque directory", layerBytes(t, true, tarEntry{name: "etc/.wh..wh..opq"})},
		{"directory whiteout", layerBytes(t, true, tarEntry{name: ".wh.etc"})},
		{"symlinks are not followed", layerBytes(t, true, tarEntry{name: "etc/passwd", typ: tar.TypeSymlink}, tarEntry{name: "etc/group", typ: tar.TypeSymlink})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opened []int
			got, err := findLayerFiles(2, opener([][]byte{base, tc.top}, &opened), etcPasswd, etcGroup)
			if err != nil {
				t.Fatal(err)
			}
			if got[etcPasswd] != nil || got[etcGroup] != nil {
				t.Errorf("got passwd=%q group=%q, want both hidden from the base layer", got[etcPasswd], got[etcGroup])
			}
			if !reflect.DeepEqual(opened, []int{1}) {
				t.Errorf("opened layers %v, want only the top one", opened)
			}
		})
	}

	// An opaque directory hides what is below it, not what sits beside it in the same layer.
	top := layerBytes(t, true, tarEntry{name: "etc/.wh..wh..opq"}, tarEntry{name: "etc/passwd", content: "top"})
	var opened []int
	got, err := findLayerFiles(2, opener([][]byte{base, top}, &opened), etcPasswd, etcGroup)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[etcPasswd]) != "top" || got[etcGroup] != nil {
		t.Errorf("got passwd=%q group=%q, want the opaque layer's own passwd and no group", got[etcPasswd], got[etcGroup])
	}
}
