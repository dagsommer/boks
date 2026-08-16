# Prebuilt Linux runtime: the nerdbox shim and libkrun

This directory produces the three pieces of the Boks stack that a Linux user has otherwise had
to build from source or hunt for. The workflow is
[`.github/workflows/linux-runtime.yml`](../../.github/workflows/linux-runtime.yml); this file
is also shipped inside the artifact it builds, which is why it reads as instructions rather
than as notes.

## Why this exists

Boks orchestrates containerd, the nerdbox shim and libkrun. On Linux — the platform Boks is
designed for — none of the three arrives usable:

- **nerdbox ships no binaries at all.** Its release workflow has failed on every tag since
  v0.2.0, so all ten of its GitHub releases carry zero assets, and it is packaged nowhere:
  not apt, not the AUR, not nixpkgs. Repology tracks it in none of the ~400 repositories it
  knows about.
- **libkrun is not packaged** for the distributions Boks targets.
- **containerd is packaged everywhere and never new enough.** The shim needs 2.3; Ubuntu
  24.04 LTS ships 1.7.x and 26.04 ships 2.2.2.

So the first step of trying Boks on Linux was "clone and build two projects, one of them
Rust", and then find a containerd no distribution offers. This closes that gap.

## What you get

| File | Built from | Patches |
|---|---|---|
| `containerd` | `containerd/containerd` @ the version [`../containerd-linux/build.sh`](../containerd-linux/build.sh) pins | **none** |
| `containerd-shim-nerdbox-v1` | `containerd/nerdbox` @ [`../nerdbox/NERDBOX_REV`](../nerdbox/NERDBOX_REV) | **none** |
| `libkrun.so` | `containers/libkrun` @ [`LIBKRUN_REV`](LIBKRUN_REV), `--features blk,net` | **none** |

All three are stock upstream. The workflow asserts the checkouts are pristine before building,
so a stray patch step added later fails the job rather than quietly changing what "unpatched"
means in the run summary.

### This is not the whole stack

The bundle is three binaries — `containerd`, `containerd-shim-nerdbox-v1` and `libkrun.so` —
plus this README and a `SHA256SUMS` over the three. A sandbox also needs:

- **the guest kernel and rootfs** — `nerdbox-kernel-<arch>` and `nerdbox-rootfs.erofs`, from
  the [`guest-image`](../../.github/workflows/guest-image.yml) workflow. Note the rootfs is
  **not** architecture-suffixed as it comes out of that workflow; the shim accepts either
  spelling, and the `.deb`/`.rpm` rename it to `nerdbox-rootfs-<arch>.erofs`;
- **`mkfs.erofs`** from erofs-utils ≥ 1.8;
- **`/dev/kvm`**, and membership of the `kvm` group.

containerd itself is no longer on that list: the bundle carries one, because **containerd 2.3
is the floor** and no distribution packages a version that high. Ubuntu 24.04 LTS ships 1.7.x
and 26.04 ships 2.2.2; measured on WSL2 on 2026-08-15, a shim linking 2.3.3 against a 2.2.2
daemon dies at task start with `unsupported protocol: Yunix`, which is protobuf framing
rendered as letters and names no version (`internal/daemon/compat.go`).

`boks doctor` names each piece as it finds it missing.

## Installing it

The three files are found in three different ways, and only one of them is on `PATH`.

```sh
# The shim: containerd execs it by name, so anywhere on containerd's PATH.
sudo install -m0755 containerd-shim-nerdbox-v1 /usr/local/bin/

# The library: the shim stats for it directly, and /usr/local/lib is on its default list.
sudo install -m0644 libkrun.so /usr/local/lib/

# containerd: `boks daemon start` runs it, and finds it beside the boks binary or in
# ../libexec/boks. Anywhere BOKS_RUNTIME_DIR names works too.
sudo install -m0755 containerd /usr/local/libexec/boks/
```

The `.deb` and `.rpm` do all three for you, into `/usr/libexec/boks`; this is the route for a
tarball or a source build.

### The filename of `libkrun.so` is load-bearing

nerdbox does not ask the dynamic linker to find libkrun. It stats for two exact filenames, in
order, and `dlopen`s the first that exists (`internal/vm/libkrun/instance.go`):

1. `libkrun-<arch>.so`, where `<arch>` is Go's `GOARCH` mapped through `kernelArch()` — that
   is **`x86_64`** on amd64, but plain `GOARCH` otherwise, so **`arm64`** and *not* `aarch64`
   on 64-bit ARM;
2. `libkrun.so`.

It searches every entry of `PATH`, then every entry of `LIBKRUN_PATH`, and — only if
`LIBKRUN_PATH` is unset or empty — falls back to `/usr/local/lib`, `/usr/local/lib64`,
`/usr/lib`, `/lib`. Note the "only if": exporting a `LIBKRUN_PATH` that does not contain
libkrun turns the default list off rather than adding to it.

The artifact is named `libkrun.so`, which is the name that works on both architectures, so
there is nothing to rename. The library's embedded `DT_SONAME` is `libkrun.so.1` and does not
matter: `dlopen` on a path containing a slash loads that file directly without consulting the
soname.

## Why Linux pins a different libkrun than Windows

[`LIBKRUN_REV`](LIBKRUN_REV) is **not** `../libkrun-windows/UPSTREAM_REV`, and the two are on
opposite sides of an ABI break.

nerdbox's loader reflects over its whole binding table and resolves every symbol **eagerly**
at `dlopen`. A missing symbol does not fail later at the call site — it fails the load, so no
VM starts at all. nerdbox v0.2.3 binds 19 symbols, and they are the libkrun **1.x** ABI.

The Windows pin is libkrun 2.0.0-dev (`ABI_VERSION=2`), which dropped four of them. Measured
by building both revisions for `aarch64-unknown-linux-gnu` with `--features blk,net` and
reading `.dynsym`:

| Symbol | libkrun 2.0 (Windows pin) | libkrun v1.19.4 (Linux pin) |
|---|---|---|
| `krun_set_log_level` | absent | present |
| `krun_set_console_output` | absent | present |
| `krun_set_gvproxy_path` | absent | present |
| `krun_set_net_mac` | absent | present |

The other 15 are present at both. A Linux `libkrun.so` built from the Windows pin would
therefore fail to load on every machine. The Windows series does not hit this because it
patches around it — patch `0014` re-exports the 1.x symbols under `#[cfg(windows)]`, and
`0026` restores `krun_set_console_output` outside the `aws-nitro` feature. Linux gets them by
staying on the 1.x line, which is also what [`docs/install.md`](../../docs/install.md) has
always said it needs (`libkrun ≥ 1.18`).

This is exactly what the workflow's export assertion is for. It is not defensive
programming — it is a check that catches a real, already-made mistake.

## libkrunfw is not needed, and is not built

The obvious worry is that libkrun wants libkrunfw, which bundles a guest kernel, and that
building it would mean compiling a kernel in CI. It does not, for two independent reasons,
both read out of the pinned source:

1. **It is not a build dependency.** No `build.rs` links it and there is no `-lkrunfw`.
   `src/libkrun/src/lib.rs` loads it at *runtime* with
   `libloading::Library::new("libkrunfw.so.5")` inside a `LazyLock`, and stores the failure as
   a value rather than panicking.
2. **The code that uses it is unreachable for Boks.** The only consumer is guarded by
   `external_kernel.is_none() && kernel_bundle.is_none() && firmware_config.is_none()`.
   nerdbox always calls `krun_set_kernel` with the external `nerdbox-kernel-<arch>`, which
   sets `external_kernel`, so the branch is never taken and the `LazyLock` is never forced.

The bundled-kernel `dlopen` therefore never has to succeed. If a future pin makes libkrunfw a
link-time dependency, the build breaks loudly instead of quietly compiling a kernel.

## The patch series in this repository are not applied here

Neither omission was taken on trust; both series were read.

**`../nerdbox-windows/patches/`** — the claim that these are Windows-only is **not true**.
Three of the five modify files with no build constraint and do change Linux behaviour:

- `0003` adds a `krun_add_vsock` binding to the shared `krun.go`;
- `0004` shortens the shim's abandoned-IO teardown from 30 s to 1 s in the shared
  `service.go`;
- `0005`'s hunks in `io.go` join and wrap the shutdown errors on every platform.

They are still left out, because `0003` is a matched set with the *Windows libkrun pin*: it
exists because libkrun `de84d01` ("lib: remove `krun_disable_implicit_vsock` and implicit
vsock creation") made the vsock device explicit. That commit is in the 2.x pin and **not** in
v1.19.4, so on the Linux pin the device is still implicit, the ports resolve without it, and
applying `0003` would add a twentieth eagerly-resolved symbol for nothing. `0004` and `0005`
are genuine cross-platform improvements that upstream should have; carrying them here would
mean shipping a shim that is not the pinned upstream, which is a bigger claim than this
directory wants to make. They are worth upstreaming rather than vendoring.

**`../libkrun-windows/patches/`** — likewise not all Windows-only. Five of the 37 touch code
compiled on Linux (`0025`, `0026`, `0035`, `0036`, `0037`; `0036` gives virtio 24 IOAPIC pins
instead of 16, `0037` publishes a real CMOS wall clock, `0035` restores `krun_set_exec`). The
question is moot here regardless: they are generated against the 2.0 tree and do not apply to
the 1.x pin. Linux uses stock upstream, and gives up those five fixes as a result.

## What CI proves, and what it does not

The workflow asserts, with [`assert-elf.py`](assert-elf.py):

- each binary is an ELF64 object of the architecture it is named for, read from `e_machine`
  rather than from a four-byte magic that every ELF passes;
- `libkrun.so` exports all 19 symbols nerdbox binds, matched **whole** and filtered to symbols
  the object genuinely **defines** (`st_shndx != SHN_UNDEF`).

Both filters exist because the naive versions are hollow. `strings | grep krun_add_disk` is
satisfied by `krun_add_disk2`; `nm -D` lists `memcpy`, which libkrun *imports*, as though it
were an export. Each assertion is accompanied in CI by a negative control that feeds the
checker something it must reject — a cross-architecture binary, a Windows PE, a strict prefix
of a real symbol, an imported symbol — and fails the job if any is accepted.

The guest half of the runtime is asserted by
[`assert-guest-image.py`](assert-guest-image.py), which `guest-image.yml` runs over what
`docker buildx bake` produced. It exists for the same reason: until 2026-08-16 that workflow
asserted *nothing whatsoever* about its outputs — the only check was `sha256sum` of the two
files against themselves, which is true of any two files, and the architecture guard beside it
globs the checked-out repository's kernel configs rather than the artifact. Hardcoding
`KERNEL_ARCH: x86_64` produced a fully green `arm64` run carrying an x86_64 kernel. It now
requires the filenames the shim looks these up by, refuses a kernel named for another
architecture, reads the architecture out of the kernel image itself, and requires the rootfs
to carry the EROFS superblock magic. Note that the kernel is an ELF `vmlinux` on x86_64 and an
arm64 *Image* on arm64 — the measured fact in `docs/install.md` — so the format is identified
before an architecture is read out of it, rather than ELF being assumed.

**Nothing here has been run.** No VM is started by the workflow: GitHub's runners are
virtualised and `/dev/kvm` is absent, so nerdbox's own `kvm.CheckKVM()` would refuse before
libkrun was reached. A symbol can be exported and still return `-ENOSYS`. What is proven is a
load-time contract — that the shim can open the library and bind every function it needs —
not that a sandbox works.

**A sandbox does work on Linux, and that was measured elsewhere.** On 2026-08-15, in WSL2 on
Ubuntu 26.04, `boks run` booted a guest with its own `boot_id`, `nproc` tracked `--cpus`, and
an allowed address completed TLS while a denied one was refused in the same sandbox — 25 of 26
checks (`../../docs/verification.md`). Two limits from that run are real and are not softened
here: it was **inside WSL2, not on bare metal** — nothing has yet booted a sandbox on a Linux
host with a directly attached `/dev/kvm` — and **sandbox creation still needs more privilege
than an ordinary user has**, because `boks` host-mounts the image overlay to read the image
config and that mount fails with `operation not permitted` for a non-root client. Windows no
longer needs elevation and Linux does, which is the wrong way round. Making that run possible
without a two-project build is the entire point of this directory.

## Building it yourself instead

Nothing here is magic, and the recipe is short. Downloading the artifact is the fast path, not
the only one.

```sh
# The shim.
git clone https://github.com/containerd/nerdbox && cd nerdbox
git checkout "$(grep -v '^[[:space:]]*#' /path/to/boks/packaging/nerdbox/NERDBOX_REV)"
GOOS=linux go build -mod=vendor -o containerd-shim-nerdbox-v1 ./cmd/containerd-shim-nerdbox-v1

# libkrun.
git clone https://github.com/containers/libkrun && cd libkrun
git checkout "$(grep -v '^[[:space:]]*#' /path/to/boks/packaging/linux/LIBKRUN_REV)"
cargo build --release -p libkrun --features blk,net
# -> target/release/libkrun.so, already named what the shim looks for.
```

Check what you built with the same script CI uses:

```sh
python3 packaging/linux/assert-elf.py libkrun.so --machine x86-64 --type dyn
python3 packaging/linux/assert-elf.py libkrun.so --require \
  krun_set_log_level krun_init_log krun_create_ctx krun_free_ctx krun_set_vm_config \
  krun_set_kernel krun_set_exec krun_set_console_output krun_start_enter \
  krun_add_vsock_port2 krun_add_virtiofs3 krun_get_shutdown_eventfd krun_set_gpu_options \
  krun_set_gvproxy_path krun_set_net_mac krun_add_disk krun_add_disk2 \
  krun_add_net_unixstream krun_add_net_unixgram
```

If you build libkrun from a different revision than the pin, that second command is the one
that tells you whether the result is usable at all.

Those nineteen names are also kept, one per line, in [`NERDBOX_SYMBOLS`](NERDBOX_SYMBOLS),
which is the canonical list: `scripts/package-linux.sh` reads it and refuses to put a
`libkrun.so` into a `.deb` or `.rpm` that does not export all of them. The command above and
the workflow both still spell the list out inline; both should read the file instead, so that
moving `../nerdbox/NERDBOX_REV` has one place to update rather than three.

## Moving the pins forward

`LIBKRUN_REV` is a full SHA, not a tag, so a repointed tag cannot change what gets built.
Before moving it, build the new revision and run the export assertion above: the 1.x → 2.x
break is not the only way upstream can remove a symbol the shim binds, and that check is the
only thing standing between a pin bump and a library nobody can load.
