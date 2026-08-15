# libkrun, patched for Windows

Boks' VMM is [libkrun](https://github.com/containers/libkrun). libkrun does not build for
Windows yet. `patches/` holds the changes that move that wall forward, and
`.github/workflows/libkrun-windows.yml` compiles the result on `windows-latest` so the series
cannot rot unnoticed.

**These patches are a staging area, not a fork.** The goal is to delete this directory. Every
patch here is meant to go upstream to `containers/libkrun`, where the Windows port is already
in progress and where this work belongs; they live in the Boks repository only for as long as
it takes to get them there. Nothing in Boks links against a patched libkrun — the patches are
compiled, never shipped.

## Why this exists

`docs/windows.md` has the long version. The short one:

Boks wants native Windows support, not WSL2. Everything above the VMM is already in place —
containerd's nerdbox shim builds for `windows/amd64` and `windows/arm64` and loads the VMM as
`krun.dll` — and the Windows Hypervisor Platform backend (`krun-whp`) compiles upstream today.
What is missing is the rest of the workspace: the crates between `krun-whp` and the `krun.dll`
cdylib still assume a Unix host. These patches are the beginning of removing that assumption.

## What is in `patches/`

Three patches, generated with `git format-patch` from the `boks-windows` working branch,
against the upstream revision pinned in `UPSTREAM_REV`:

| Patch | What it does |
| --- | --- |
| `0001-windows-unblock-the-build-wall-*` | Turns off `vm-memory`'s `rawfd` feature on Windows (it refuses to compile there by design), fixes an `unsafe extern` block, and adds the `Wdk` feature to `windows-sys` so the driver-kit types resolve. |
| `0002-windows-gate-TSI-define-the-FUSE-stat-types-*` | Gates the transparent-socket-impersonation device, which is Linux-only, defines the FUSE `stat` structures Windows has no `libc` definition for, and gates eleven `match` arms on errno values that do not exist on Windows. |
| `0003-windows-implement-scatter-gather-file-I-O-*` | Implements virtio-fs' vectored file I/O over `ReadFile`/`WriteFile`. There is no `preadv`/`pwritev` on Windows, so the scatter-gather traits are reimplemented against overlapped handles. |

They touch six files across `src/devices` and `src/utils`, roughly 260 lines added.

## How far they get

Checked with `cargo check --target x86_64-pc-windows-msvc` at the pinned revision with the
series applied. **This was measured by cross-compiling from Linux, not on Windows** — see the
caveat below.

Compiles:

- `krun-utils`, `krun-polly`, `krun-arch-gen`, `krun-smbios`, `krun-kernel`, `krun-arch`
- `krun-whp` — the Windows Hypervisor Platform backend, which needed no patch from us

Does not compile yet, and why:

| Crate | Blocker |
| --- | --- |
| `krun-cpuid` | Imports `kvm_bindings` unconditionally. The x86 CPUID templates need separating from the KVM structures that carry them. |
| `krun-devices` | Four errors. Two in `legacy/x86_64/serial.rs`, which calls `as_raw_fd()` on a `Box<dyn ReadableFd>` — the serial device needs a handle abstraction instead of a raw fd. Two in `virtio/fs/device.rs`, which references `PermissionSemantics` and `Config.semantics`; the Windows `passthrough.rs` defines neither, because `LinuxSimplified` semantics keep mode bits in the host file system and NTFS has nowhere to put them. |
| `krun-vmm` | The two above, plus `vm-memory` built with `rawfd` (which refuses on Windows) and `std::os::fd` imports that do not exist there. |
| `libkrun` | The cdylib. Blocked on all of the above; when it goes green, `krun.dll` exists. |

Also missing entirely, and not addressed by any patch here: **virtio-net**. It is the one
device upstream did not port to WHP, and it is precisely the one Boks' network enforcement
depends on. Nothing in this directory changes that — see `docs/windows.md` section 8 and
`internal/network/gateway_windows.go`.

### A caveat about the measurement

The table above came from a Linux host cross-compiling to `x86_64-pc-windows-msvc`. That is
good enough for portability errors — a missing `std::os::fd` is missing either way — but it is
not the same as a native build, and it produces at least one false failure of its own: crates
whose build scripts invoke a C compiler (`bzip2-sys`, `zstd-sys`, reached through the `libkrun`
crate) fail for want of `cl.exe`, which says nothing about Windows.

That is why the workflow runs on `windows-latest` rather than cross-compiling. **The first CI
run is the first native measurement**, and it may well disagree with this table. If it does,
correct the table rather than the workflow.

## Working on these patches

Reproduce what CI does:

```sh
git clone https://github.com/containers/libkrun
cd libkrun
git checkout "$(grep -v '^[[:space:]]*#' ../boks/packaging/libkrun-windows/UPSTREAM_REV | tr -d '[:space:]')"
git apply ../boks/packaging/libkrun-windows/patches/*.patch
cargo check --target x86_64-pc-windows-msvc -p krun-whp
```

CI uses `git apply` rather than `git am` on purpose: `am` writes commits, and a commit needs a
committer identity that a fresh runner does not have — it fails with `empty ident name` before
it ever reads the patch.

### Adding or amending a patch

Work on a branch off the pinned revision, then regenerate the whole series so the files stay a
faithful `format-patch` of real commits:

```sh
rm -f packaging/libkrun-windows/patches/*.patch
git -C /path/to/libkrun format-patch --no-signature \
    -o /path/to/boks/packaging/libkrun-windows/patches "$UPSTREAM_REV..HEAD"
```

Do not hand-edit a `.patch` file. The series is regenerable by construction, and that is what
makes it possible to check that it still reproduces the branch it came from.

When a crate starts compiling, move it from the expected-to-fail step to the required step in
`.github/workflows/libkrun-windows.yml`. That ratchet is the point of the two-step split:
the failing list only ever shrinks.

### Moving the pin

`UPSTREAM_REV` is a full SHA so that upstream landing something cannot break a Boks pull
request that touched none of this. To move it forward: bump the file, rebase the branch onto
the new revision, regenerate the series, and let the workflow tell you what changed. Expect
patches to drop out over time — upstream is working on the same port, and a patch that no
longer applies because upstream fixed it is the outcome this directory is aiming for.

### This pin is Windows-only, and Linux has its own

`UPSTREAM_REV` here is libkrun **2.0.0-dev**. [`packaging/linux/LIBKRUN_REV`](../linux/LIBKRUN_REV)
is **v1.19.4**, and the divergence is deliberate rather than neglect.

libkrun 2.0 removed four of the nineteen symbols nerdbox v0.2.3 binds — `krun_set_log_level`,
`krun_set_console_output`, `krun_set_gvproxy_path` and `krun_set_net_mac` — and nerdbox
resolves its whole binding table eagerly at `dlopen`, so a missing symbol fails the load
rather than the call. This series does not notice because patch 0014 re-exports the 1.x names
under `#[cfg(windows)]` and patch 0026 restores `krun_set_console_output` outside the
`aws-nitro` feature. An *unpatched* build of this revision, which is what Linux ships, would
not load at all — measured by building both revisions for `aarch64-unknown-linux-gnu` with
`--features blk,net` and reading `.dynsym`.

So do not "unify" the two pins without checking the exports first, and do not assume this
series is safe to skip on Linux merely because these files sit in a directory named
`libkrun-windows`: five of the 37 patches (0025, 0026, 0035, 0036, 0037) touch code that is
compiled on Linux too. `packaging/linux/README.md` records what Linux gives up by omitting
them.

## Upstreaming

The tracking issue is [containers/libkrun#798](https://github.com/containers/libkrun/issues/798),
where the maintainer's stated plan is Windows support as part of libkrun 2.0. Patches 0001–0003
are all mechanical portability work with no design content, which is the easiest kind to land.
`docs/windows.md` lists the merged Windows pull requests this work sits on top of.
