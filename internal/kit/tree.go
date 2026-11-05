package kit

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// MaxTreeBytes bounds the static files a kit may carry. They are read into memory and streamed
// into the guest one file at a time, so this is a sanity limit on a fetched kit rather than a
// storage limit: a kit that needs more than this wants an image, not a copy.
const MaxTreeBytes = 16 << 20

// TreeFile is one file from a kit's files/ tree.
type TreeFile struct {
	// Path is slash-separated and relative to the tree's root, never starting with "..".
	Path string
	// Mode is the permission bits the file had in the kit.
	Mode uint32
	Data []byte
}

// Tree is a kit's static files: files/home/ is copied under the agent's home, and
// files/workspace/ into the primary workspace.
type Tree struct {
	Home      []TreeFile
	Workspace []TreeFile
}

// Empty reports whether the kit carries no static files.
func (t *Tree) Empty() bool { return t == nil || len(t.Home)+len(t.Workspace) == 0 }

// ReadTree reads the files/ tree beside a kit's spec.yaml. A kit without one returns nil.
//
// The tree comes from a kit that may have been fetched, so it is read as untrusted: only regular
// files are taken, and a symlink is skipped with a warning rather than followed, because one
// pointing at /etc/shadow on the host would otherwise be copied into the guest as content. Paths
// come from walking the directory, so they cannot contain "..".
func ReadTree(kitDir string) (*Tree, []string, error) {
	root := filepath.Join(kitDir, "files")
	info, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, []string{"files is not a directory and was ignored"}, nil
	}

	var warnings []string
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}
	var t Tree
	total := int64(0)
	for _, e := range entries {
		var dst *[]TreeFile
		switch e.Name() {
		case "home":
			dst = &t.Home
		case "workspace":
			dst = &t.Workspace
		default:
			warnings = append(warnings, fmt.Sprintf("files/%s is not files/home or files/workspace and was ignored", e.Name()))
			continue
		}
		if e.Type()&fs.ModeSymlink != 0 || !e.IsDir() {
			warnings = append(warnings, fmt.Sprintf("files/%s is not a plain directory and was ignored", e.Name()))
			continue
		}
		base := filepath.Join(root, e.Name())
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(base, p)
			if err != nil || rel == "." {
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 {
				warnings = append(warnings, fmt.Sprintf("files/%s/%s is a symlink and was skipped", e.Name(), filepath.ToSlash(rel)))
				return nil
			}
			if d.IsDir() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			if !fi.Mode().IsRegular() {
				return nil
			}
			total += fi.Size()
			if total > MaxTreeBytes {
				return fmt.Errorf("the kit's files/ tree is larger than %d MiB", MaxTreeBytes>>20)
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			*dst = append(*dst, TreeFile{Path: filepath.ToSlash(rel), Mode: uint32(fi.Mode().Perm()), Data: data})
			return nil
		})
		if err != nil {
			return nil, nil, fmt.Errorf("reading kit files/%s: %w", e.Name(), err)
		}
		sort.Slice(*dst, func(i, j int) bool { return (*dst)[i].Path < (*dst)[j].Path })
	}
	if t.Empty() {
		return nil, warnings, nil
	}
	return &t, warnings, nil
}
