package cli

import (
	"fmt"
	"io"

	"github.com/dagsommer/boks/internal/kit"
	"github.com/dagsommer/boks/internal/sandbox"
)

// kitVolumes translates a kit's volumes into what a sandbox is created with, plus notes for
// the user about what will not be honoured exactly.
//
// A block-backed volume is a directory under Boks' state directory, so it has no size limit:
// a kit that asks for one is told, rather than left believing it has a quota.
func kitVolumes(spec *kit.Spec) ([]sandbox.Volume, []string, error) {
	if spec == nil {
		return nil, nil, nil
	}
	var (
		out   []sandbox.Volume
		notes []string
	)
	for _, v := range spec.Volumes {
		size, err := v.Bytes()
		if err != nil {
			return nil, nil, fmt.Errorf("kit volume %s: size: %w", v.Path, err)
		}
		mode, err := v.Perm()
		if err != nil {
			return nil, nil, fmt.Errorf("kit volume %s: %w", v.Path, err)
		}
		tmpfs := v.Type == kit.MountTypeTmpfs
		if !tmpfs && size > 0 {
			notes = append(notes, fmt.Sprintf("volume %s: size %s is not enforced for a block-backed volume", v.Path, v.Size))
		}
		out = append(out, sandbox.Volume{Path: v.Path, Tmpfs: tmpfs, Size: size, Mode: mode})
	}
	return out, notes, nil
}

// applyKitVolumes puts the kit's volumes on a sandbox that is about to be created, or says
// why they are not. Volumes are part of the container's spec, so a sandbox that exists keeps
// the ones it was created with; this does not refuse, because re-running the command that
// created it carries the same kit.
func applyKitVolumes(spec *kit.Spec, exists bool, cfg *sandbox.Config, stderr io.Writer) error {
	if spec == nil || len(spec.Volumes) == 0 {
		return nil
	}
	if exists {
		fmt.Fprintf(stderr,
			"note: kit %s declares %d volume(s), which are attached only when a sandbox is created.\n"+
				"      Sandbox %q keeps the mounts it has; to get them, 'boks rm %s' and run again.\n",
			spec.Name, len(spec.Volumes), cfg.Name, cfg.Name)
		return nil
	}
	vols, notes, err := kitVolumes(spec)
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Fprintf(stderr, "kit %s: %s\n", spec.Name, n)
	}
	cfg.Volumes = vols
	return nil
}

// kitPublish is the kit's ports as --publish specifications, announced on stderr: a fetched
// file opening a port on the host is something the user should see happen.
func kitPublish(spec *kit.Spec, stderr io.Writer) []string {
	if spec == nil || len(spec.Ports) == 0 {
		return nil
	}
	specs, notes := kit.PublishSpecs(spec.Ports)
	for _, n := range notes {
		fmt.Fprintf(stderr, "kit %s: %s\n", spec.Name, n)
	}
	for _, s := range specs {
		fmt.Fprintf(stderr, "kit %s: publishing sandbox port %s on an ephemeral 127.0.0.1 port (see 'boks ports')\n", spec.Name, s)
	}
	return specs
}
