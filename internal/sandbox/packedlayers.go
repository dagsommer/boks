package sandbox

import (
	"context"
	"fmt"
	"os"
	"regexp"

	"github.com/containerd/containerd/v2/client"
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
		"The device in that message (%s) is a PARTITION, not a disk. Past a threshold the shim\n"+
		"stops giving each layer its own virtio-blk device and packs them all into one\n"+
		"GPT-partitioned VMDK. Reading that disk needs a libkrun carrying the fix in\n"+
		"packaging/imago/: without it, every partition past the third reads as zeros and is\n"+
		"reported as a successful read, which is what an unmountable EROFS looks like.\n\n"+
		"The threshold is deliberately different per platform — on x86-64 the guest has only\n"+
		"nineteen interrupt lines for virtio devices, so a large image has to be packed rather\n"+
		"than given a device per layer — which is why an image can pack here and not elsewhere.\n\n"+
		"What to do:\n"+
		"  - Update Boks and its runtime together. A libkrun without the imago fix, or a shim\n"+
		"    from a different release, is the common cause. 'boks doctor' reports which shim\n"+
		"    and libkrun were found and where.\n"+
		"  - If they are already current, this is a bug worth reporting, with the layer count\n"+
		"    of %s and the device name above.",
		cfg.Image, err, m[1], cfg.Image)
}

// staleBundleDir matches containerd failing to create a task's bundle directory because one is
// already there.
//
// Both spellings, because the message is the operating system's: Windows says "Cannot create a
// file when that file already exists" and Unix says "file exists".
var staleBundleDir = regexp.MustCompile(`mkdir (\S*io\.containerd\.runtime\.v2\.task\S*):.*(?i:already exists|file exists)`)

// describeStaleBundle explains a task that cannot be created because the last attempt left its
// directory behind.
//
// # How a sandbox gets into this state
//
// containerd's shim makes a bundle directory per task and removes it when the task is reaped.
// Interrupt a `boks run` between those two — Ctrl-C during the first start, which is exactly
// when someone reaches for it, because the first start is the one that pulls an image — and the
// shim dies with the directory already made. The next run then fails before it does anything,
// on a path the user has never seen and about a file they did not create.
//
// # Why Boks does not just delete it
//
// It is containerd's state, not Boks'. A directory that looks stale may belong to a task that
// is still running: the same name, from another terminal, or a shim this process cannot see.
// Removing it under a live task would take its mounts out from under it, which is a worse
// failure than the one being fixed and a much harder one to explain. So this says what is
// there and what removes it safely, and leaves the choice with the person who knows whether
// anything else is running.
func describeStaleBundle(cfg Config, msg string, err error) error {
	m := staleBundleDir.FindStringSubmatch(msg)
	if m == nil {
		return nil
	}
	return fmt.Errorf("sandbox %q could not be created because the runtime's state from a "+
		"previous attempt is still there.\n\n%w\n\n"+
		"That directory belongs to containerd and is normally removed when a task ends. An "+
		"interrupted start — Ctrl-C during the first run, which is the one that pulls the "+
		"image — leaves it behind.\n\n"+
		"Boks clears this by itself when nothing owns it, so seeing it here means the\n"+
		"directory could not be removed or is owned by a task containerd still knows about.\n"+
		"`boks rm` does NOT clear it — that removes the sandbox, and this belongs to\n"+
		"containerd.\n\n"+
		"Try 'boks daemon stop' then 'boks daemon start', which ends any shim still holding\n"+
		"it. If it survives that, remove the directory by hand once you are sure no other\n"+
		"terminal is running this sandbox.",
		cfg.Name, err)
}

// clearStaleBundle removes a leftover bundle directory when it is provably safe, so that an
// interrupted start does not make a sandbox permanently unstartable.
//
// Safe means: containerd has no task for this container. A bundle directory holds the mounts of
// a LIVE task when there is one, and removing it under a running sandbox would be a far worse
// failure than the one being repaired — so the check is "does containerd know of a task", not
// "does the directory look old".
//
// Reported rather than silent. Deleting runtime state on someone's behalf is worth one line of
// output: a user who sees a sandbox recover with no explanation has learnt nothing, and a user
// who sees this once a week has a different problem worth noticing.
//
// Returns (true, nil) when the caller should retry, (false, nil) when this is not that failure,
// and (false, err) when the directory is there and cannot be removed — which on Windows is what
// a still-running shim looks like, and is exactly the case that must not be retried.
func clearStaleBundle(ctx context.Context, container client.Container, cause error) (bool, error) {
	m := staleBundleDir.FindStringSubmatch(cause.Error())
	if m == nil {
		return false, nil
	}
	dir := m[1]

	// A task that containerd knows about owns this directory. Not ours to remove, and the
	// caller's own handling for a live task is the right answer.
	if _, taskErr := container.Task(ctx, nil); taskErr == nil {
		return false, nil
	}

	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("sandbox %q cannot start: the runtime's state from a previous "+
			"attempt is at %s and could not be removed (%w).\n\n"+
			"If a shim from an earlier run is still alive it holds that directory. Check for a\n"+
			"containerd-shim process for this sandbox, or restart the daemon with 'boks daemon\n"+
			"stop' and 'boks daemon start', then try again.",
			container.ID(), dir, err)
	}
	return true, nil
}

// krunStartEINVAL matches libkrun refusing to start the VM at all.
//
// -22 is EINVAL, which libkrun returns for anything it will not accept, so this match alone
// proves nothing about the cause. What narrows it is the layer count, which is why the
// explanation below is only offered when there are enough layers for the interrupt budget to
// be the plausible reason.
var krunStartEINVAL = regexp.MustCompile(`krun_start_enter failed: -22\b`)

// x86DeviceBudget is how many virtio-MMIO devices a guest can have on x86_64.
//
// The VMM raises interrupts through the IOAPIC, which has 24 pins. Pins 0-4 are taken by real
// hardware the guest expects to find — the PIT, the i8042, the 8259 cascade and the 16550s —
// so virtio gets 5 through 23. That is not a tunable: it is how many inputs the interrupt
// controller has. aarch64 is not affected, which is why the same image runs on an Apple Silicon
// Mac and stops here.
//
// packaging/libkrun-windows/patches/0036 raised this from 11 by declaring MP-table sources for
// pins 16-23, and its closing paragraph predicted this exact failure: "It does not make the
// per-layer device count scale — a 20-layer image would still exhaust 19 lines."
const x86DeviceBudget = 19

// x86NonLayerDevices is what a sandbox spends before its first image layer: the console, the
// RNG, the balloon, the NIC and virtiofs, plus two block devices — the runtime's config disk
// and the sandbox's writable ext4 layer. Counted from a boot log rather than from the source,
// where they appear as virtio0 through virtio6.
const x86NonLayerDevices = 7

// describeDeviceBudgetFailure explains a VM that will not start because the image has more
// layers than the guest has interrupt lines.
//
// It is deliberately quiet when the layer count is unknown or small. EINVAL from a VMM has
// many causes, and attaching a confident story about interrupt controllers to an unrelated
// configuration error would send someone rebuilding an image for no reason.
// goarch is taken as an argument rather than read, so that the x86 explanation can be
// rendered and asserted on the arm64 machines this project is developed on. A test that skips
// where the bug lives is not a test of it.
func describeDeviceBudgetFailure(cfg Config, layers int, goarch, msg string, err error) error {
	if !krunStartEINVAL.MatchString(msg) || layers <= 0 {
		return nil
	}
	if goarch != "amd64" || layers+x86NonLayerDevices <= x86DeviceBudget {
		return nil
	}
	max := x86DeviceBudget - x86NonLayerDevices
	return fmt.Errorf("the VM for %s could not be started.\n\n%w\n\n"+
		"Image %s has %d layers, and the runtime gives each one its own virtio device. With\n"+
		"the %d a sandbox always needs, that is %d devices against the %d this architecture has\n"+
		"interrupt lines for — an x86 IOAPIC has 24 pins and the first five belong to hardware\n"+
		"the guest expects to find. The practical ceiling is about %d layers.\n\n"+
		"This is not a setting. What can change:\n"+
		"  - Fewer layers in the image. At %d or fewer this starts.\n"+
		"  - Run it on arm64 (an Apple Silicon Mac), whose interrupt controller has no such\n"+
		"    limit. The same image can run there and not here.\n\n"+
		"Packing the layers onto one disk is what the runtime does past its threshold, and that\n"+
		"path does not mount under libkrun today — so it is not an escape from this.",
		cfg.Name, err, cfg.Image, layers, x86NonLayerDevices,
		layers+x86NonLayerDevices, x86DeviceBudget, max, max)
}

// imageLayerCount reports how many layers a container's image has, or 0 when that cannot be
// answered. It is asked only on a failure path, so the cost does not matter and the answer
// never being available must not turn into an error of its own.
func imageLayerCount(ctx context.Context, container client.Container) int {
	image, err := container.Image(ctx)
	if err != nil {
		return 0
	}
	diffs, err := image.RootFS(ctx)
	if err != nil {
		return 0
	}
	return len(diffs)
}
