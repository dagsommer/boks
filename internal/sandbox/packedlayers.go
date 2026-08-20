package sandbox

import (
	"fmt"
	"regexp"
)

// packedLayerMount matches the guest's refusal to mount an image layer out of a PARTITION —
// /dev/vdc4 rather than /dev/vdc — which is the shape of the runtime's packed-layer path.
var packedLayerMount = regexp.MustCompile(`mount source: "(/dev/vd[a-z][0-9]+)".*fstype: erofs`)

// describePackedLayerFailure explains a failure that has nothing to do with anything the user
// typed, and whose message names a device node they have never heard of:
//
//	mount source: "/dev/vdc4", target: "…/mounts/4", fstype: erofs, flags: 1, err: invalid argument
//
// # What is actually happening
//
// The shim gives each of an image's layers its own virtio-block device — until there are more
// than eight of them. Past that it packs every layer into ONE disk as a GPT-partitioned VMDK
// (nerdbox internal/shim/task/mount.go, `gptLayerThreshold = 8`), and the layers become
// partitions: /dev/vdc1, /dev/vdc2, and so on. That path needs the VMDK support libkrun
// documents as "FLAT/ZERO formats without delta links", and when the guest cannot read a layer
// where the partition table says it is, the mount fails with EINVAL.
//
// A digit on the end of the device node is what separates the two paths, and it is the only
// part of the message that says which one was taken.
//
// # Why this check is worth its weight
//
// Nothing in the error mentions layers, a threshold, or a disk format, so every natural reading
// of it is wrong: a corrupt image, a bad pull, a broken erofs. It is none of those — an image
// with eight layers works and the same image with nine does not, which is not a distinction
// anyone would arrive at from the text. Boks cannot fix it, since both the packing and the disk
// format belong to the runtime underneath, but it can say what happened.
//
// Measured against a real failure: a nine-plus-layer image on macOS/arm64, reported 2026-08-20.
// The eight-layer case is not theoretical either — the `claude` image mounts vdc through vdj,
// exactly eight erofs layers, and works.
func describePackedLayerFailure(cfg Config, msg string, err error) error {
	m := packedLayerMount.FindStringSubmatch(msg)
	if m == nil {
		return nil
	}
	return fmt.Errorf("the guest could not mount a layer of image %s.\n\n%w\n\n"+
		"The device in that message (%s) is a PARTITION, which means the image has more than\n"+
		"eight layers: past that the runtime stops giving each layer its own disk and packs them\n"+
		"all into one, as a GPT-partitioned VMDK. That is a different code path from the one an\n"+
		"eight-layer image takes, which is why a smaller image can work and this one not.\n\n"+
		"What helps:\n"+
		"  - Fewer layers. Squashing the image to eight or fewer avoids the packing entirely and\n"+
		"    uses the path every working sandbox takes.\n"+
		"  - A newer libkrun, if yours predates its VMDK support: brew upgrade libkrun.\n"+
		"  - 'boks doctor' reports which libkrun was found and where.",
		cfg.Image, err, m[1])
}
