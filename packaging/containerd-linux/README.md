# The containerd the Linux packages vendor

Two things live here. [`config.toml`](config.toml) is the hand-written configuration for
driving a containerd by hand while debugging, and predates `boks daemon`; it is unchanged and
still the reference for that. [`build.sh`](build.sh) is new and builds the containerd that
goes *into* the `.deb` and the `.rpm`.

```sh
packaging/containerd-linux/build.sh amd64 dist/runtime-amd64
packaging/containerd-linux/build.sh arm64 dist/runtime-arm64
```

Nothing it produces is committed. A vendored binary in this repository would be the one thing
this directory must never leave behind, and `build.sh` writes only to the output directory
you name.

## Why the packages carry a containerd at all

`Depends: containerd` produces a machine that looks provisioned and cannot start a sandbox.
That is not a prediction:

| Distribution | containerd | Result |
|---|---|---|
| Ubuntu 24.04 LTS | 1.7.x | far below anything Boks can use |
| Ubuntu 26.04 | 2.2.2 | **measured to fail**, see below |
| Debian, Fedora, others | 1.7.x–2.2.x | same shape |

Measured on WSL2/Ubuntu 26.04, 2026-08-15 (`docs/verification.md`, "Boks runs and enforces
policy on Linux"): the nerdbox shim emits **version-3** bootstrap parameters, a 2.2 daemon
cannot decode them, falls back to treating the entire protobuf reply as an address, and dies
at task start with

```
unsupported protocol: Yunix
```

`Yunix` is three bytes of protobuf framing rendered as letters. It names no version, no shim
and no protocol. `boks doctor` reported `containerd ok` and `vm runtime ok` — both truthfully.
That failure is the whole argument for vendoring: it is not that the distribution's containerd
is old, it is that being old is invisible until the moment a sandbox will not start.

Vendoring collapses the axis: one package ships one containerd and one shim, and they are
built from pins that agree by construction.

## The pins

| File | What it pins |
|---|---|
| [`CONTAINERD_VERSION`](CONTAINERD_VERSION) | `v2.3.3`, the containerd `packaging/nerdbox/NERDBOX_REV` vendors |
| [`BUILDTAGS`](BUILDTAGS) | the tags it is compiled with, and the measured cost of each |

`CONTAINERD_VERSION` is deliberately a separate file from
`packaging/containerd-windows/CONTAINERD_VERSION`, even though both currently read `v2.3.3`.
That one is pinned to the tree its six patches apply to; this one moves with the nerdbox pin.
The two libkrun pins were once assumed to be the same thing and turned out to sit on opposite
sides of an ABI break — see [`../linux/LIBKRUN_REV`](../linux/LIBKRUN_REV) — which is the
cost of collapsing two pins that only look alike.

## Size, measured

The whole cost of vendoring is bytes, so the numbers are measured rather than estimated.
containerd v2.3.3, `CGO_ENABLED=0 -trimpath -ldflags "-s -w"`, on 2026-08-15:

| Build tags | linux/arm64 | Δ |
|---|---|---|
| `urfave_cli_no_docs` (upstream's own set) | 35,717,282 | — |
| `+ no_cri` | 29,950,114 | −5.8 MB |
| **`+ no_cri no_devmapper no_zfs`** (what `build.sh` uses) | **29,753,506** | **−6.0 MB** |
| `+ no_cri no_devmapper no_zfs no_tracing` | 28,901,538 | −6.8 MB |

and for the chosen set, per architecture:

| Architecture | Bytes |
|---|---|
| linux/arm64 | 29,753,506 |
| linux/amd64 | 31,924,386 |

For comparison, Ubuntu's own `/usr/bin/containerd` is 43.3 MB stripped (`docs/distribution.md`,
Part 3). The tags are worth roughly six megabytes and `BUILDTAGS` explains, tag by tag, why
each removal is safe — and why `no_tracing` is left on the table despite being the largest
remaining saving.

### The tags are asserted, not trusted

`go build -tags` is **silent** about a tag that matches no build constraint anywhere: no
warning, no error, exit 0, and the plugin stays in. Until 2026-08-16 nothing here noticed —
the only post-build assertion was the ELF machine, which is identical either way. Measured on
2026-08-16, containerd v2.3.3, linux/arm64: changing `no_cri` to `no_cri_`, one character,
produced a **35,586,210-byte** daemon with CRI and CNI compiled in — 5.8 MB above the table
above — and `build.sh` reported success.

`build.sh` now does two things instead. Before building, it refuses a tag it has no entry for,
which catches the typo. After building, it checks the binary for the Go package path each tag
is supposed to remove — `plugins/cri` and `go-cni` for `no_cri`, `plugins/snapshots/devmapper`
for `no_devmapper`, `zfs/v2` for `no_zfs`, `go-md2man` for `urfave_cli_no_docs` — which
catches the other half: a tag spelled correctly for a constraint upstream has since renamed.
Package paths survive `-trimpath` and `-ldflags "-s -w"`, so this is read from the artifact
rather than inferred from the flags that produced it.

The removal checks carry a **positive control**: the erofs snapshotter's package path must be
*present*. Without it, a future toolchain change that stripped package paths would make every
"is absent" assertion pass while testing nothing — and erofs is the one snapshotter Boks pins,
so its presence is worth asserting in its own right.

## What is NOT patched here, and how that was decided

`packaging/containerd-windows/patches/` carries six patches. None is applied here, and that
was checked patch by patch rather than assumed from the directory's name.

| Patch | Files it touches | Applies to Linux? |
|---|---|---|
| `0001` register the erofs snapshotter and differ | `builtins_windows.go` | **No.** `cmd/containerd/builtins/builtins_linux.go` in stock v2.3.3 already imports `plugins/snapshots/erofs/plugin`, `plugins/diff/erofs/plugin` and `plugins/mount/erofs`. Read, not assumed |
| `0002` erofs in the Windows diff order | `service_windows.go`, and **`local.go`, which is shared** | The Windows half, no. The shared half is real: it drops a differ that returned `ErrSkipPlugin` from the order with a warning instead of failing the diff service. Boks does not need it, because `boks daemon`'s generated configuration omits `erofs` from the order when `mkfs.erofs` is absent — the cascade never starts (`internal/daemon/config.go`) |
| `0003` optional Linux erofs unpack config | `plugin_defaults_windows.go` | **No**, and it would be inert: it configures the transfer service, which is `ctr`'s path. Boks pulls with `client.Pull`, which builds its own unpacker |
| `0004` `ctr` platform selection | `cmd/ctr/…` | **No.** The packages ship no `ctr` |
| `0005` verify the mkfs superblock | **`core/mount/manager/mkfs.go`, shared, no build constraint** | **Yes, in principle.** See below |
| `0006` link a bundle's work directory with a junction | `worklink_windows.go`, `bundle.go` | **No.** `worklink_other.go` makes it a plain rename off Windows |

### The one candidate: patch 0005

`mkfs.Transform` decides an image is already formatted by calling `Stat` on it. The format
path creates the file, truncates it to size, and only then runs mkfs — so any way mkfs can
fail leaves a file of exactly the expected size full of zeroes, which the next call accepts as
a formatted filesystem. The erofs snapshotter asks for an `mkfs/ext4` mount for every active
snapshot, so this is reachable on Linux.

It is not applied, for two reasons that are about evidence rather than taste. The trigger on
Windows is that `mkfs.ext4` does not exist there **at all**; on Linux it is in e2fsprogs,
which is `Priority: required` on Debian and in Fedora's base, so the precondition is not
normally met. And nobody has produced the failure on Linux. Shipping a containerd that is not
the upstream release is a larger claim than this directory currently wants to make, and
`build.sh` refuses to build from a dirty tree precisely so that "unpatched" cannot quietly
stop being true.

If that changes, the shape is fixed: add `patches/`, apply them in `build.sh` deliberately,
relax the pristine check, and change this table in the same commit. The right destination for
0005 is upstream regardless — it is a bug in containerd, not a Boks-specific need.

## What building this proves, and what it does not

`build.sh` asserts the result is an ELF64 object of the architecture it was asked for, using
[`../linux/assert-elf.py`](../linux/assert-elf.py) — `e_machine`, not a four-byte magic that
every ELF passes. That is the mistake a user experiences as `cannot execute binary file`.

It does not prove the daemon works. What **has** been shown, on linux/arm64 on 2026-08-15,
running the `boks` and the `containerd` out of an unpacked `.deb` rather than from a build
tree:

```
$ boks daemon status
binary     …/usr/libexec/boks/containerd
serving    containerd v2.3.3
```

`boks daemon start` located the vendored containerd through
`internal/daemon/locate.go`'s `<exe dir>/../libexec/boks`, started it, and waited until it
answered its API. Its log shows `io.containerd.snapshotter.v1.erofs`,
`io.containerd.differ.v1.erofs` and `io.containerd.mount-handler.v1.erofs` all loaded, no
plugin failing to load, and `containerd successfully booted in 0.014525s`. `boks doctor`
against it read `containerd ok`, `snapshotter ok (erofs available)` and `runtime skew ok
(containerd v2.3.3, shim built against v2.3.3)` — the skew line being the one this pin exists
to keep true. `boks daemon stop` ended it, leaving the machine's other two containerds
running and untouched.

Two things that were *not* shown, and are not claimed. A sandbox booting: that needs a real
`libkrun.so` (the run above used a stub exporting the nineteen symbols) and the guest images,
and this host has no `/dev/kvm`. And a clean `boks doctor`: three of its checks look on the
shell's `PATH` rather than in the bundle and report the vendored files missing — see the note
in [`../apt/README.md`](../apt/README.md). `docs/verification.md` remains the record of what
has actually run.
