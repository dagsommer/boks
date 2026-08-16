# `mkfs.ext4.exe` — the ext4 formatter, cross-compiled for Windows

Every sandbox Boks starts off Linux needs an ext4 image, and containerd makes it by running
`mkfs.ext4`. This directory is the recipe for a `mkfs.ext4.exe` that Windows can run.

## What needs it, and when

containerd's erofs snapshotter has two modes, and which one it uses is decided by a single
number: `blockMode = config.defaultSize > 0` (`plugins/snapshots/erofs/erofs.go:187`). That
default is **64 MiB off Linux** (`erofs_other.go:27-30`, the `!linux` file) and **0 on Linux**
(`erofs_linux.go`). So macOS and Windows get block mode and Linux does not.

In block mode an active snapshot's mount list begins with (`erofs.go:395-411`):

```go
{ Source: s.writablePath(snap.ID), Type: "mkfs/ext4",
  Options: []string{"X-containerd.mkfs.fs=ext4", "X-containerd.mkfs.size=67108864", "rw", "loop"} }
```

`writablePath(id)` is `<erofs root>\snapshots\<id>\rwlayer.img` (`erofs.go:206-208`).
`mkfs/ext4` is not in nerdbox's `mkdir/*,format/*,erofs` allow list, so containerd's own mount
manager handles it: it creates that file, truncates it to the requested size, and runs
`mkfs.ext4` (`core/mount/manager/mkfs.go:106,143`). That happens at **task start**, once per
sandbox, after the image has been pulled and unpacked — which is why it was the last thing to
fail on Windows and the first thing nobody had a plan for.

Measured on Windows 11 on 2026-08-16, from the v0.1.0 archive, on an image that pulled and
unpacked completely:

```
boks: starting the io.containerd.nerdbox.v1 runtime failed: failed format
"…\io.containerd.snapshotter.v1.erofs\snapshots\11\rwlayer.img":
mkfs.ext4 failed: : exec: "mkfs.ext4": executable file not found in %PATH%
```

That is not a regression. **Nothing in Boks has ever put a writable layer in place.** The only
thing that ever did was a human running the `Copy-Item` in `docs/windows-e2e.md` step 5.

## Why a real formatter and not the pre-formatted image

The bundle also ships `rwlayer-64m.img`: 64 MiB, formatted by `mkfs.ext4` on the Linux CI
runner. containerd skips the format step when the target image is already formatted — with
`patches/0005` applied it checks the superblock magic rather than merely `Stat`ing the file —
so an image placed in advance is accepted. That is a supported route and it is why the file
exists.

It is not the route this directory chooses, for three reasons.

**The size is fixed and nothing checks it.** `patches/0005` verifies the magic, not the length.
A 64 MiB template dropped in where containerd asked for 256 MiB is accepted in silence, and the
sandbox gets a quarter of the writable space its configuration says it has. The requested size
is `default_size` from `[plugins.'io.containerd.snapshotter.v1.erofs']`, so it is a knob a user
or a future Boks release can turn — and the template would then be quietly wrong rather than
loudly absent. A formatter has no such failure: whatever size containerd truncates the file to
is the size of the filesystem in it.

**Somebody has to place it, for every snapshot, at the right moment.** The path contains the
snapshotter's internal numeric id, which no Boks code sees, and the file is needed at task
start for every active snapshot — including `View` snapshots (`erofs.go:355`), which Boks does
not create and would not know to seed. Whatever placed it would be reimplementing containerd's
own convention from outside, and would be wrong the first time containerd changed it.

**It fails safe.** If this binary turns out not to work on Windows, `boks run` stops with the
same `failed format` error it stops with today, and `patches/0005` deletes the zero-filled file
so the next attempt retries rather than mounting 64 MiB of zeroes. The downside of trying a
real formatter is bounded by the behaviour we already have.

`rwlayer-64m.img` no longer ships in `boks_<v>_windows_amd64.zip`, the archive a user installs.
`mkfs.ext4.exe` ran there for the first time on 2026-08-16, unattended, and produced a real
superblock (`53 ef` at offset 1080), so nothing in that archive reads the pre-formatted image
and it was 64 MiB of the download for everyone. It still ships in
`boks-runtime_<v>_windows_amd64.zip`, which is where `docs/windows-e2e.md` collects the files
for its by-hand procedure, and it remains that document's fallback for a machine where the
formatter does not run.

## Why the port is free

erofs-utils needed five upstream patches and seven compatibility headers before it would
cross-compile (`packaging/mkfs-erofs-windows/README.md`). e2fsprogs needs none. mingw is a
supported host in its own `configure.ac`:

| `configure.ac` | What it does |
| --- | --- |
| `case "$host_os" in mingw*)` at `:863` | disables tdb by default |
| at `:1042` | defines the headers `include/mingw/` supplies but `AC_CHECK_HEADERS` cannot see |
| at `:1837` | adds `-I$(top_srcdir)/include/mingw` to `INCLUDES` |
| at `:1973` | `OS_IO_FILE=windows_io` |

`include/mingw/` ships `unistd.h`, `pwd.h`, `grp.h`, `arpa/`, `linux/` and `sys/`.
`lib/ext2fs/windows_io.c` is 1028 lines of Win32 I/O manager, with a bounce-buffer alignment
path mirroring `unix_io.c`'s. All of that is upstream, and none of it is ours.

**A plain file is exactly what it is built for.** `windows_open_device` passes any name that
does not start with `\` straight to `CreateFile` with `OPEN_EXISTING`
(`lib/ext2fs/windows_io.c:562-580`), and `OPEN_EXISTING` is not a problem here: containerd
creates and truncates `rwlayer.img` before it runs the formatter, so the file is already there
at the size the filesystem should be.

## The recipe

Run on Linux with `mingw-w64` installed. Every step below was executed while writing this;
see "Status".

1. `apt-get install mingw-w64`
2. Fetch and verify the tarball named in `E2FSPROGS_VERSION`.
3. Configure for the mingw host, **statically**:

   ```
   LDFLAGS="-static" ./configure --host=x86_64-w64-mingw32 \
     --disable-nls --disable-uuidd --disable-tls --disable-fuse2fs \
     --disable-e2initrd-helper --disable-testio-debug --disable-defrag \
     --disable-backtrace --without-libarchive
   ```

   `LDFLAGS="-static"` is load-bearing, not tidiness. Without it the binary imports
   `libwinpthread-1.dll`, which is not on a stock Windows machine — a `mkfs.ext4.exe` that
   cannot start is indistinguishable from one that is missing, and produces the same
   `failed format` error with a much worse explanation. With it the import table is
   `KERNEL32.dll` and `msvcrt.dll` and nothing else, which is the same profile as the
   `mkfs.erofs.exe` Docker ships.

4. Build the libraries **in this order**:

   ```
   make -C lib/et        # com_err
   make -C lib/ext2fs    # generates ext2_err.h, which lib/e2p includes
   make -C lib/e2p
   make -C lib/uuid
   make -C lib/blkid
   make -C lib/support
   ```

   The order is not cosmetic. `make -C lib/e2p` first fails with
   `No rule to make target '../../lib/ext2fs/ext2_err.h', needed by 'feature.o'` — the header
   is generated by `lib/ext2fs`.

   **`lib/ss` is skipped on purpose, and `make libs` is not used.** `lib/ss` is the interactive
   subsystem library for `debugfs`, it does not compile against mingw (`sigset_t` undeclared,
   `SIGCONT` undeclared in `listen.c`), and `mke2fs` does not link it. `make libs` walks into it
   and stops, which is what makes the top-level target look like the port has failed when it
   has not.

5. `make -C misc mke2fs` — the target is `mke2fs`, not `mke2fs.exe`.
6. `x86_64-w64-mingw32-strip misc/mke2fs.exe`
7. Copy it to **`mkfs.ext4.exe`**. See below — the name is the configuration.

### The name is the configuration

There is no `--fs-type` in the invocation containerd makes. It runs
`exec.Command("mkfs.ext4", "-q", <path>)`, and `mke2fs` decides which filesystem to create
**from `argv[0]`**: `parse_fs_type` takes `argv[0]` (`misc/mke2fs.c:2158`), strips a leading
`mkfs.`, and uses the rest as the type (`:1408-1421`). A copy named `mkfs.ext4.exe` therefore
makes ext4, and one named anything else falls back to the profile default, which is **ext2**.

Two things make this safe rather than fragile, and both are worth knowing because ext2's
superblock magic is the same `0xEF53` — so `patches/0005` would accept a wrongly-named build's
output without complaint:

- Go passes the bare name. `exec.Command("mkfs.ext4", …)` sets `Args[0]` to the string it was
  given, not to the resolved path, and Windows builds the child's command line from `Args`. So
  `argv[0]` is `mkfs.ext4`, with no directory and no `.exe`.
- `get_progname` splits on `/` only (`misc/util.c:78-87`). A Windows path in `argv[0]` would
  therefore *not* be split, which is exactly why the point above matters.

A hardlink or a symlink would do on a filesystem that has them. A copy is used because the
archive is a zip and neither survives it.

### No `mke2fs.conf` is required

`mke2fs` reads `ROOT_SYSCONFDIR "/mke2fs.conf"` — which mingw's CRT turns into `C:\etc\mke2fs.conf`,
a path no Windows machine has. That is not a failure: on `ENOENT` it falls back to a profile
compiled into the binary (`misc/mke2fs.c:1704-1710`, `misc/default_profile.c`), which carries
the ext4 feature set —
`has_journal,extent,huge_file,flex_bg,metadata_csum,metadata_csum_seed,64bit,dir_nlink,extra_isize,orphan_file`
— a 4096-byte block size and a 256-byte inode. The CI job asserts those strings are in the
shipped binary, because "works without a config file" is the difference between shipping one
file and shipping two.

`MKE2FS_CONFIG` still overrides it if anyone needs to.

## Status

**This binary has never been executed.** It has been cross-compiled and inspected, on Linux, on
an arm64 machine that cannot run a Windows PE at all. That is the same status
`packaging/mkfs-erofs-windows/README.md` records for its patched build, and it means the same
thing: nothing here tells you whether it works.

What *was* executed, on 2026-08-16, on a Linux/arm64 host with `mingw-w64` 13-win32:

| Step | Result |
| --- | --- |
| `./configure --host=x86_64-w64-mingw32` on pristine 1.47.2 | exit 0, `OS_IO_FILE=windows_io` |
| the six library builds | all exit 0, no patches, no compatibility headers of ours |
| `make -C misc mke2fs` | exit 0 |
| the artifact | `PE32+ executable for MS Windows 5.02 (console), x86-64`, 525,312 bytes stripped |
| import table | `KERNEL32.dll`, `msvcrt.dll` — no `libwinpthread-1.dll` after `LDFLAGS="-static"` |
| I/O manager | `lib/ext2fs/windows_io.o` built, `lib/ext2fs/unix_io.o` not; `windows_io_manager` in the symbol table, `unix_io_manager` absent |
| compiled-in profile | present in the stripped binary |

No size or checksum is quoted as a thing to compare against, for the reason
`packaging/mkfs-erofs-windows/README.md` gives: it depends on the runner's mingw-w64 and changes
without anything here changing. The workflow prints the real `sha256sum` into its job summary.

### What the workflow checks, and what it cannot

| Check | Why it is there |
| --- | --- |
| `head -c2` is `MZ`, `x86_64-w64-mingw32-objdump -f` says `pei-x86-64` | a cross-build that fell back to the host compiler would produce a working-looking ELF named `.exe` |
| import table is exactly `KERNEL32.dll` and `msvcrt.dll` | a dynamic `libwinpthread-1.dll` makes the binary refuse to start on a stock machine, and the resulting error is the same "failed format" as having no binary at all |
| `windows_io.o` built and `unix_io.o` not | the one platform-specific decision in the whole build. A binary carrying `unix_io` would compile, link, ship, and then fail to open any path on Windows |
| imports `CreateFileA`, `SetFilePointerEx`, `GetFileSizeEx`, `ReadFile`, `WriteFile` | the same fact, asserted against the shipped artifact rather than an intermediate object |
| `strings` contains the compiled-in profile's ext4 feature list | proves it needs no `mke2fs.conf`, so shipping one file is enough |
| the artifact is named `mkfs.ext4.exe` | the name selects the filesystem. A build shipped as `mke2fs.exe` would not be found by containerd; one shipped as anything else beginning `mkfs.` would make the wrong filesystem, and `patches/0005` cannot tell ext2 from ext4 because they share a magic |

**x86-64 only.** Same as `mkfs.erofs.exe`, and for a weaker reason — nothing about e2fsprogs is
x86-specific, `--host=aarch64-w64-mingw32` is simply not something this recipe has tried. On an
arm64 Windows host there is no `mkfs.erofs.exe` either, so the bundle is unusable there for
reasons that arrive earlier.

### To learn whether it works

On a Windows machine, with the bundle unpacked:

```powershell
mkfs.ext4.exe -V                      # the exe starts at all
fsutil file createnew rw.img 67108864
mkfs.ext4.exe -q rw.img               # format a file the size containerd asks for
# byte 1080 and 1081 must read 53 ef
Get-Content rw.img -AsByteStream -TotalCount 1082 | Select-Object -Last 2
```

Then the real test, which is `boks run` with no `Copy-Item` beforehand and no `rwlayer.img`
anywhere under `…\io.containerd.snapshotter.v1.erofs\snapshots\`.
