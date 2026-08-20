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
