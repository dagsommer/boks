# The nerdbox pin, and the patches that are not about any one platform

`NERDBOX_REV` is the containerd/nerdbox commit three things are built from: the guest kernel,
the guest rootfs (which contains `vminitd`), and the `containerd-shim-nerdbox-v1` that boots
them. It lives here, in a directory named after neither Linux nor Windows, because all three
have to come from the same commit and a SHA that lives in two files eventually disagrees with
itself.

`patches/` is new and holds patches with the same property: they are not about a host platform,
so they do not belong in [`../nerdbox-windows/patches/`](../nerdbox-windows/patches/).

## Why this directory exists rather than reusing the Windows one

`packaging/nerdbox-windows/patches/` already carries five nerdbox patches, and three of them
are not Windows-specific either. So the obvious move was to add a sixth there. It is the wrong
move, for a reason that is structural rather than tidy-mindedness:

**nothing applies the Windows series to the artifacts Linux ships.**
[`.github/workflows/linux-runtime.yml`](../../.github/workflows/linux-runtime.yml) checks the
pinned revision out and then *asserts the tree is pristine* before building — "this workflow
ships UNPATCHED binaries" — and [`guest-image.yml`](../../.github/workflows/guest-image.yml)
bakes the kernel and rootfs from an equally untouched checkout. A patch dropped into the
Windows directory would compile in CI and reach no guest on any platform.

That is correct for the Windows series, whose three portable members fix things Linux does not
hit at the revisions Linux pins. It is wrong for a patch whose entire point is that all three
hosts need it.

## `0002-raise-the-layer-count-at-which-the-shim-packs-layers.patch`

The one that decides whether a sandbox starts.

The shim gives each of an image's erofs layers its own virtio-block device, up to eight
(`internal/shim/task/mount.go`, `gptLayerThreshold`). Past that it packs them all into one
GPT-partitioned VMDK, and on libkrun that mount fails:

```
mount source: "/dev/vdc4", target: "…/mounts/4", fstype: erofs, flags: 1, err: invalid argument
```

Eight is far below the budget. `vda`–`vdz` is 26 letters, the libkrun manager reserves one
(`ReservedDisks() == 1`), and a container spends one more on its writable ext4 layer — leaving
24 for layers. Eight left sixteen unused while sending every larger image down a path that does
not work here. A .NET SDK image is commonly 10–15 layers, so "larger" means ordinary.

Raised to 20, which keeps four spare for volumes and puts ordinary images on the flat path that
every working sandbox already uses. **It does not fix the packed path**: an image with more than
20 layers still takes it and still fails. That is a separate defect, and this is deliberately
the smaller change of using the code path that works.

Verified against a real checkout of the pinned tag: the patch applies, nerdbox's own
`internal/shim/task` tests pass with it, and the shim builds. Not verified: that a 9-layer image
then boots, which needs a hypervisor this project's machines do not have.

### What is known about the packed path it avoids

Investigated on Windows on 2026-08-25, on the 17-layer image that found this. Recorded here so
the ruled-out ground is not walked again — every item below is a measurement, not a reading of
the source:

**Ruled out.** The synthetic disk is laid out correctly and libkrun's VMDK reader parses it
correctly. `ComputeLayout` rejects nothing (all 17 layers are 512-multiples); the descriptor's
36 extents (18 FLAT, 18 ZERO) sum exactly to `TotalSectors`; no extent approaches the 2 GiB
split limit and no partition spans more than one. Resolving every partition's `FirstLBA`
through the extent map lands on its layer's EROFS superblock — magic `e0f5e1e2` on all 17. A
harness linking imago 0.2.3, the crate libkrun delegates all VMDK parsing to, reproduced that
on Windows through the same entry point `block/device.rs` uses: 17 of 17 correct.

**Where the evidence points instead.** The mount that fails is partition 4, and partition 4 is
the first whose superblock lives above 2^31 bytes:

| partition | superblock offset | |
| --- | --- | --- |
| 1 | 1,049,600 | mounts |
| 2 | 106,955,776 | mounts |
| 3 | 2,138,047,488 | mounts — 9 MB *under* the boundary |
| 4 | 2,315,256,832 | **EINVAL** — first one over |

2^31 is 2,147,483,648. Partition 2 *extends* past it and still mounts, because mounting reads
the superblock at the partition's start and nothing further. So every read known to have
succeeded is below the boundary and the first known to have failed is above it. That is the
signature of a signed-32-bit truncation, and one layer per device — where every offset is
small — is exactly the configuration that does not hit it.

**The 2^31 reading was wrong**, and the experiment that killed it is worth keeping: with the
threshold lowered to 2, Boks' own 8-layer `shell` image fails identically — `/dev/vdc4`, at
4 MiB rather than 2.3 GB. The failure is positional. It is always the **fourth partition**,
whatever the sizes or offsets.

**What the guest says**, which nobody had read until 2026-08-25 — containerd's log carries the
guest's kmsg:

```
erofs: (device vdc1): mounted with root inode @ nid 36.
erofs: (device vdc2): mounted with root inode @ nid 36.
erofs: (device vdc3): mounted with root inode @ nid 36.
erofs: (device vdc4): erofs_read_superblock: cannot find valid erofs superblock
```

Not a rejected filesystem, not a failed read: **wrong bytes reported as good**. The guest gets
no I/O error at all. That matches one behaviour in imago's `readv` — reaching the end of a
mapping fills the rest of the buffer with zeros and returns success:

```rust
if chunk_length == 0 { assert!(mapping.is_eof()); bufv.fill(0); break; }
```

**The descriptor from the failing run is correct.** Captured in flight (the shim's cleanup
deletes the bundle on the failure, so it exists for about a second) and resolved by hand: 18
extents summing exactly to the disk's 1,345,536 sectors, every FLAT extent's declared size
matching its backing file byte for byte, the GPT header's `PartitionEntryLBA`, entry count and
entry size all correct, and all 8 partitions landing on their own layer's `e2e1f5e0`.

**The open lead** is how libkrun opens it (`block/device.rs:312`):

```rust
Vmdk::builder(file).open_sync(PermissiveImplicitOpenGate::default())
```

Every extent file is an implicit dependency opened through that gate. In the failing disk the
opens are, in order: the header blob, then partitions 1, 2 and 3 — and partition 4's layer is
the **fifth**. A gate that stops opening dependencies after a few would explain the ordinal
exactly, in both images, and would surface as zeros rather than as an error.

**Not yet examined**, and the reason this is a lead rather than a cause: the guest
does not reach imago the way that harness did. Its path is
`block/worker.rs` (`request_header.sector * 512`) → `Writer::write_from_at`
(`descriptor_utils.rs`) → `DiskProperties::read_vectored_at_volatile` (`file_traits.rs`) →
imago's **`readv`** with a vector of guest buffers. Every offset in libkrun's own code along
that path is `u64` — read at the pinned revision, 07fd40d. The untested step is imago's
vectored read on Windows, which the harness's plain `read()` did not exercise.

One latent defect found on the way, not this bug: imago `src/vmdk/mod.rs:641` adds the
descriptor's fourth field — defined by the VMDK spec in *sectors* — to a byte offset. It is
inert only because nerdbox writes 0 for every FLAT offset.

## A patch that was tried and withdrawn: mounting virtiofs shares with `default_permissions`

Boks wants to idmap a shared workspace's bind mount (see `internal/sandbox/hostuser.go`), and
the guest kernel only allows an idmapped mount on top of one made with `default_permissions`
(`fs/fuse/inode.c:fuse_fill_super_common`). A patch was written adding
`Options: []string{"default_permissions"}` to `bindMounter.VmMounts()`'s
`mount.Mount{Type: "virtiofs", ...}` literal (`internal/shim/task/mount.go`) on the reasoning
that nothing currently sets it.

**Booted and found wrong, 2026-10-01.** The mount itself failed:

```
failed to mount bind-eb5a2bb732f4b795 at /run/mnt/bind-eb5a2bb732f4b795: mount source:
"bind-eb5a2bb732f4b795", target: "/run/mnt/bind-eb5a2bb732f4b795", fstype: virtiofs, flags: 0,
data: "default_permissions", err: invalid argument
```

Reading `fs/fuse/virtio_fs.c` at the pinned guest kernel (6.12.44) explains why:
`default_permissions` is a generic-FUSE mount option, and `virtiofs` is a *different*,
narrower filesystem type with its own parameter table —

```c
static const struct fs_parameter_spec virtio_fs_parameters[] = {
	fsparam_flag("dax", OPT_DAX),
	fsparam_enum("dax", OPT_DAX_ENUM, dax_param_enums),
	{}
};
```

— which only knows `dax`. Passing anything else makes `fs_parse()` reject the mount outright,
which is the EINVAL above. But `virtio_fs_fill_super()` calls `virtio_fs_ctx_set_defaults()`
unconditionally before that parsing even matters:

```c
static inline void virtio_fs_ctx_set_defaults(struct fuse_fs_context *ctx)
{
	ctx->rootmode = S_IFDIR;
	ctx->default_permissions = 1;
	...
}
```

Every virtiofs mount already has `default_permissions` on, for every Boks sandbox, today, with
no option to turn it off and nothing elsewhere in the file that does. The patch was not just
unneeded, it broke the one thing it touched. It has been reverted. The actual blocker, found by
continuing to boot against a plain unpatched shim once this one was pulled, is `0003` below.

## `0003-report-idmap-mount-support-in-shim-info.patch`

The one that actually gates whether containerd will accept an idmapped mount at all.

With the withdrawn patch above reverted, booting again against a plain pinned shim reached
container creation and failed differently:

```
failed to create shim task: failed to validate OCI runtime features: unmarshal runtime
features: type with url : not found
```

This is containerd's own safety net, not nerdbox's or Boks' — `core/runtime/v2/task_manager.go`
(containerd 2.2.6), `validateRuntimeFeatures`: whenever a spec's mounts carry `UIDMappings` or
`GIDMappings` (`usesIDMapMounts`), containerd asks the runtime, via the shim's `Info` RPC,
whether it actually supports idmapped mounts before creating the task — because a runc-family
low-level runtime silently ignores spec fields it does not recognise, and a silently-dropped
idmap is a real permissions regression, not a no-op. `manager.Info()`
(`pkg/shim/manager/manager.go`) never sets the `Features` field at all, so containerd cannot
even unmarshal it — not "unsupported," just empty — which fails every task creation that asks
for an idmapped mount, including ones the runtime would gladly have honoured.

`containerd-shim-runc-v2` answers this by shelling out to `runc features` and forwarding
whatever it prints. That path does not exist here: crun runs inside the guest, and the guest is
not started yet at the point containerd asks `Info`. What this project already knows at build
time is which crun it pins — 1.24, this project's own Dockerfile — and that it supports
idmapped mounts: reading `src/libcrun/linux.c` at that tag shows
`maybe_create_userns_for_idmapped_mount` creating a throwaway user namespace per mount, with no
dependence on the container having one of its own. The patch states that fact rather than
discovering it at runtime.

**Verified:** applies cleanly together with `0001` and `0002` against the pinned commit, and
the resulting shim builds clean for `darwin/arm64` (`go build -tags no_grpc`) with `go vet
./...` passing across the whole tree.

### What happened once containerd actually accepted the task, 2026-10-01

It did — `mount_setattr(MOUNT_ATTR_IDMAP)` succeeded, `mount` inside the guest reported the
workspace share as `idmapped`, and the container's process ran as the image's own uid (`101`,
not root, not the host's uid). The Go-side mechanism this patch exists to unblock
(`internal/sandbox/hostuser.go`'s `idmapWorkspaceMounts`) is doing exactly what it was
designed to do.

What doesn't work yet: every uid/gid read through the idmapped mount — `ls`, `stat`, a fresh
`touch` — returns the kernel's overflow sentinel (`65534`), and every write fails with
`EOVERFLOW`, as if the mapping table were empty. It is not empty. The bundle's `config.json`,
read directly off disk, carries exactly the right values:

```json
"uidMappings":[{"containerID":101,"hostID":502,"size":1}],
"gidMappings":[{"containerID":0,"hostID":20,"size":1}]
```

— matching `id -u`/`id -g` (502/20) and the real on-disk owner of the shared directory
(`stat -f '%u %g'` on the host, also 502/20), confirmed independently on the macOS side. So
this is not a Boks bug, not a value lost somewhere in the containerd/shim pipeline — the
correct mapping reaches crun and crun's own `mount_setattr` call reports success.

**Ruled out by direct testing** (not by reading source and reasoning it away):

| Hypothesis | Test | Result |
| --- | --- | --- |
| Stale cwd reference, since source and destination are the same path | Fresh absolute-path access from a different cwd | Same failure |
| Missing the literal `"idmap"`/`"ridmap"` mount-option string (crun's own passing test, `test_idmapped_mounts_without_userns`, always sets it alongside `UIDMappings`) | Added `"ridmap"` to `Options` | Same failure |
| `rw` specifically clobbers the mapping via crun's second, option-driven `mount_setattr` call (crun's own passing test uses `"ro"`) | Idmapped a read-only mount instead | Same failure |
| General crun+idmap+no-container-userns mechanism is broken | Same crun, same kernel, idmapped a plain ext4 write (`/tmp` inside the guest, no mount involved) | **Works** — correct uid, no error |

That last row is the one worth sitting with: the exact same crun code path, same kernel, same
"no container user namespace" shape, works correctly against ext4 and fails identically against
virtiofs regardless of direction (read or write) or mode (ro or rw). crun's own test coverage
for "idmap without a container userns" exercises a plain directory, never FUSE — this
combination may simply never have been exercised before.

Traced through `fs/fuse/dir.c`'s `fuse_fillattr`/`fuse_getattr` and the kernel's
`make_vfsuid`/`i_uid_into_vfsuid` machinery at the pinned kernel tag (6.12.44): structurally
correct, properly idmap-aware, nothing found. That doesn't mean nothing is there — it means
finding it needs live syscall tracing (`strace`/`bpftrace` against the actual `mount_setattr`
call and the FUSE requests that follow it), which is a different kind of tool than source
reading, and nobody has run it yet.

### Found, 2026-10-01: the mapping was written backwards

Nothing was wrong in crun, the kernel or the pipeline. The **mapping Boks wrote was inverted**,
and the decisive evidence was in the first reproduction all along. The test file's gid on disk
was `0` (macOS `wheel`, under `/private/tmp`), and the guest listed it as **`20`**:

```
uid=101(nginx) gid=0(root) groups=0(root)
-rw-r--r-- 1 65534 20 3 Oct  1 15:31 f
touch: cannot touch 'g': Value too large for defined data type
```

The gid entry was `{containerID: 0, hostID: 20}`, and it had turned on-disk `0` into a
visible `20`. So `containerID` is the id **on disk** and `hostID` is the id the **caller
sees**. crun writes each entry into the throwaway namespace's `uid_map` as `inside outside
size`, and an idmapped mount maps an on-disk id as an *inside* id of that namespace
(`make_vfsuid` → `make_kuid(idmap_userns, ...)`). The uid entry `{101 → 502}` therefore had
`101` on the disk side, so the real on-disk `502` was unmapped, which gives `65534` on read.
On write, the caller's `101` had no on-disk counterpart, so FUSE's request setup failed with
`EOVERFLOW`. The field names only read naturally when the container has a user namespace of
its own, where a mount's mapping is normally a copy of the container's.

That also explains the ext4 row in the table above: nothing on that path compared against a
host-owned file, so it never put a real on-disk id through the reversed entry.

**Fixed in `idmapWorkspaceMounts`** (`internal/sandbox/hostuser.go`), which now writes
`{containerID: hostUID, hostID: containerUID}` (and the same for gids). The unit test asserts
that direction. Booted against the patched shim, same image:

| Inside the guest (uid 101) | On the host |
| --- | --- |
| existing `f` lists as `101` | `502 20` |
| `echo >> f` succeeds | `502 20`, 12 bytes |
| `touch g`, `mkdir d`, `touch d/h` succeed, all list as `101` | all `502 20` |

**Not caused by idmap:** files created through the share land on the host as `0600`/`0700`
even though the guest reports `0644`/`0755`. The host-uid override path does the same
(checked on the same day, same shim), so it is libkrun's passthrough behaviour and predates
this feature.

**Where this leaves things:** the mechanism works end to end against a shim built with
`0001`+`0002`+`0003`. Boks decides whether to use it by running the shim with `-info` and
reading the same `linux.mountExtensions.idmap.enabled` that containerd requires
(`ShimSupportsIdmappedMounts`, `internal/daemon/compat.go`). It does not use a revision
allowlist, because the Homebrew-built shim carries no vcs stamp, and a patch does not change
the revision it applies on top of anyway. So a Mac picks it up as soon as `brew upgrade`
installs the `revision 2` formula. Until then, the shim prints no features and Boks keeps the
host-uid override.

### Named `USER`: resolved on the host, 2026-10-01

The idmap needs the container's uid when the spec is built, so `USER agent` (most agent
images, including most kit-defined ones) had to be resolved before the guest exists. On macOS it used
to reach the spec as uid 0 and take the override. `internal/sandbox/imageuser.go` now reads
`/etc/passwd` and `/etc/group` out of the image's layers in the content store, top layer
first and honouring whiteouts, and resolves the name the way containerd does on Linux.
Verified on 2026-10-01 against `registry.example.test/copilot-sandbox:latest`
through the `team-copilot-default` kit: the agent ran as `uid=1000(agent)` with groups `sudo`
and `docker`, and `HOME=/home/agent` was writable. Its workspace writes landed as `502:20` on
the host.

## `0001-fix-vminitd-resolve-Process.User.Username-against-th.patch`

### The field nothing reads

An OCI image may name its user rather than number it — `USER node` rather than `USER 1000`.
Resolving the name means reading `/etc/passwd` out of the image's root filesystem. The OCI
runtime spec has a field for handing an unresolved name to whoever can do that:
`Process.User.Username`.

No Linux runtime reads it. Verified by reading both sides at the revisions this project pins:

| Component | What it does with `Process.User.Username` |
| --- | --- |
| `vminitd` (nerdbox `cd2c23f`) | Nothing. `grep -rn Username --include=*.go .` outside `vendor/` returns **zero hits** in nerdbox's own code. Its only read of the spec is `ShouldKillAllOnExit` (`internal/vminit/runc/util.go:33`), which looks at `Linux.Namespaces` and nothing else. |
| `crun` (`3425c83`) | Nothing. Every read of the user struct in `src/libcrun/` is one of `user->uid` (6), `user->gid` (4), `user->umask{,_present}` (4), `user->additional_gids{,_len}` (2). The only `username` hits in the tree are `libcrun_set_usernamespace` — user *namespace*, unrelated. |
| runtime-spec schema | `username` **is** a valid property of `process.user` (`schema/config-schema.json`), and is tagged `platform:"windows"` in the Go bindings. |

The last row is what makes this dangerous rather than merely broken. An *unknown* field would be
rejected by libocispec and the container would fail to start loudly — which is exactly what
happened with `layerFolders` (see [`../nerdbox-windows/README.md`](../nerdbox-windows/README.md)).
A *known-but-ignored* field parses fine and is then dropped. `uid` keeps its zero value, and a
container that asked to drop to `node` runs as **root** with nothing in any log saying so.

### Who ends up in that state

containerd's `oci.WithUser` resolves the name host-side by mounting the image's snapshot. That
needs `CAP_SYS_ADMIN`, and on a macOS or Windows host holding a *Linux* guest filesystem it is
not merely privileged but impossible. containerd knows, and gives up explicitly:

```go
if (s.Windows != nil && s.Linux != nil) || runtime.GOOS == "darwin" {
	s.Process.User.Username = userstr
	return nil
}
```

Its own comment calls the field "a temporary holding spot until the guest can use the string to
perform these same operations to grab the uid:gid inside". Nothing in the guest ever did. This
patch is that missing half.

### What it does

Resolves the name in `NewContainer` (`internal/vminit/runc/container.go`), after the rootfs
components are mounted and before crun is handed the spec — the only moment at which the
image's `/etc/passwd` exists and the container does not — then rewrites the bundle's
`config.json` with the numeric uid/gid crun actually reads.

Every form the image spec allows is accepted (`user`, `uid`, `user:group`, `uid:gid`,
`uid:group`, `user:gid`), following containerd's reading of them so a spec resolved in the guest
and one resolved by `oci.WithUser` on a Linux host agree. Supplementary groups come from
`/etc/group` as in `oci.WithAdditionalGIDs`. No new dependency: the passwd/group parsing is
about sixty lines in the patch rather than a vendored user library, so `go mod vendor` is
untouched and the diff stays reviewable.

**It fails open.** A rootfs with no `/etc/passwd`, a name absent from the one that is there, an
unreadable `config.json` — each leaves the spec exactly as it arrived and lets crun proceed.
This code only ever runs on a spec a host already gave up on, so "leave it alone" is the
behaviour that was already in place; turning it into a hard error would break containers that
run today. What it will not do is guess: an unresolvable name is logged at WARN rather than
silently becoming uid 0, which is the whole failure being fixed.

### What has actually been run

`go test ./internal/vminit/runc/` passes against a pristine checkout of the pinned revision with
the patch applied, on `linux/arm64`, on 2026-08-16. The tests use `testing/fstest` and a
temp-dir bundle, so they need no VM, no root and no image.

They were **mutation-checked**, because this project has shipped assertions that could not fail:

| Mutation | Test that fails |
| --- | --- |
| The resolver returns immediately — i.e. the pre-patch behaviour | `TestResolveSpecUserRewritesTheBundle` |
| The user's own group is not skipped when collecting supplementary groups | `TestSupplementalGroupsSkipsTheUsersOwnGroup` |
| An unresolvable name falls through to uid 0 instead of erroring | `TestResolveUserStringRefusesToGuess`, `TestResolveSpecUserLeavesAnUnresolvableNameAlone` |

The whole of `./internal/... ./pkg/...` still passes, and the tree builds for `linux/amd64`,
`linux/arm64` and — for the shim — `windows/amd64`.

**No microVM has booted with this patch.** This repository's machines have no `/dev/kvm`. The
claim proven above is that the resolver produces the right spec; the claim *not* proven is that
a guest carrying it starts a container as the resolved uid.

## What applies this series

**Changing anything in this directory means bumping `revision` in
`packaging/homebrew/tap/Formula/nerdbox.rb.in`.** A formula's version comes from its `url` —
a nerdbox tag this project pins deliberately — so adding or editing a patch changes what the
formula builds while leaving what it claims to be identical. `brew upgrade` then sees nothing
outdated and rebuilds nothing, and the shim on disk stays the one from before the patch: a new
Boks, an unchanged shim, and a bug that was supposed to be fixed. `revision` exists for exactly
this and costs one line.

**`packaging/homebrew/render.sh` embeds every patch here into the rendered `nerdbox.rb`**, after
an `__END__`, and the formula applies them with `patch :DATA` before it builds. That is the
shim macOS installs, so a Mac runs a patched shim.

Nothing else does. `linux-runtime.yml` still asserts a pristine tree and ships an unpatched
shim, and `guest-image.yml` bakes an unpatched kernel and rootfs — so a Linux install and every
guest image are unpatched. That split is not tidy and it is not permanent; it is where things
stand, and it means a patch's effect depends on which platform you are on.

**The formula builds only the shim** (`go build ./cmd/containerd-shim-nerdbox-v1`), so a patch
touching `internal/vminit/` — 0001 does — compiles nothing there and changes no binary anyone
runs. It is applied because applying the series as a series is simpler to reason about than
maintaining a list of which patches count, and because it cannot do anything: the guest rootfs
is built by a different workflow entirely. In particular it cannot make
`ShimResolvesUsernames` answer differently, since that reads the nerdbox REVISION out of the
shim and matches it against a list, not the source it was built from.

## Why 0001 stays inert, and that is deliberate

Neither `guest-image.yml` nor `linux-runtime.yml` has been changed to apply `patches/`. So as of
this commit the capability exists in this directory and in **no artifact Boks builds, ships or
downloads**.

That is the safe order, not an oversight. The guest rootfs carries no version, no manifest and
no embedded revision — [`../../internal/daemon/compat.go`](../../internal/daemon/compat.go)
lists this as a known gap: *"Neither file carries a version, so there is nothing to compare
without publishing a manifest alongside them."* Applying the patch before that is fixed would
make the capability real but **unobservable**: Boks would have a guest that resolves names and
no way to know it, so it would still have to assume the worst, and anyone reasoning from "the
patch is applied" would be reasoning from something no running process can check.

The order that works is:

1. This patch lands upstream in nerdbox, or is applied here **and** the guest image gains
   something that identifies it (a manifest naming the nerdbox revision, or a version `vminitd`
   reports over ttrpc).
2. Boks learns to read that identification — see `ShimResolvesUsernames` in
   `internal/daemon/compat.go`, which is the single place the answer is decided and today
   answers `false` for every input.
3. Only then does Linux switch to the metadata-only spec path.

Doing 3 before 2 is what ships the silent-root regression, which is why
`internal/sandbox/imageconfig.go` refuses the metadata path unless step 2 says yes rather than
documenting the hazard and trusting whoever reads it.

## Working on these patches

Same as the Windows series, against the same pin:

```sh
git clone https://github.com/containerd/nerdbox
cd nerdbox
git checkout "$(grep -v '^[[:space:]]*#' ../boks/packaging/nerdbox/NERDBOX_REV | tr -d '[:space:]')"
git apply ../boks/packaging/nerdbox/patches/*.patch
go test ./internal/vminit/runc/
```

Regenerate rather than hand-edit, so the series stays a faithful `format-patch` of real commits:

```sh
rm -f packaging/nerdbox/patches/*.patch
git -C /path/to/nerdbox format-patch --no-signature \
    -o /path/to/boks/packaging/nerdbox/patches "$NERDBOX_REV..HEAD"
```

If both series are ever applied to one tree, apply `packaging/nerdbox/patches/` first: it is the
platform-independent one, and the Windows series is generated against the same base.

## Upstreaming

`0001` is an independent bug report against `containerd/nerdbox` and does not depend on anything
in `../nerdbox-windows/`. It is the strongest case of any patch this project carries: a
correctness defect on the platform nerdbox already supports, with a reproduction that needs no
Windows and no unusual hardware, and tests that run in nerdbox's existing unit-test job. Send it
with the crun field-read table above, since the fix only makes sense once it is clear that the
field it fills in is one every Linux runtime ignores.
