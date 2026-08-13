# `mkfs.erofs.exe` — the EROFS formatter, cross-compiled for Windows

containerd's erofs differ does not write EROFS images itself. It shells out to **`mkfs.erofs`**,
once per image layer, in tar mode:

```
mkfs.erofs --tar=f <layer.erofs> < layer.tar
```

`internal/erofsutils/mount.go` in containerd invokes it by bare name through `exec.Command`, so
on Windows it is found via `PATH` and `PATHEXT` — `mkfs.erofs.exe` anywhere on `PATH` resolves.

There is no Windows package of erofs-utils anywhere. Upstream erofs-utils has zero Windows
support: no MSYS2 package, no vcpkg port, no release binary. Docker ships an `mkfs.erofs.exe`
inside the sbx MSI (PE32+ x86-64, GNU binutils 2.38, importing only `KERNEL32.dll` and
`msvcrt.dll`), which is evidence that a mingw-w64 cross-build works, but not a recipe anyone
can run.

`erofs/go-erofs` builds one in the open, in CI, and that is the recipe this directory reuses.

## Why the port is cheap

erofs-utils on Windows is mostly hopeless — it wants `mmap`, `ioctl`, xattrs, `statx`, POSIX
file modes, and a filesystem walker that reads Unix ownership off real inodes.

**Tar mode does not touch any of that.** `--tar=f` reads a tar stream and writes an image; the
Unix metadata comes out of the tar headers, not out of the host filesystem. containerd's differ
only ever uses tar mode. So the port only has to make the tar path compile and run, and the
patches below are correspondingly small (~9.3 KB in total).

## The pin, and why there is only one

`GO_EROFS_REV` is the only pin here. The erofs-utils version, the lz4 version, the five patches
and the seven MinGW compatibility headers all live in the `erofs/go-erofs` tree at that SHA, and
the workflow reads them from there rather than copying them into this repo.

That is a deliberate departure from `packaging/libkrun-windows/` and `packaging/nerdbox-windows/`,
which vendor their patches. Those two directories carry **our** patches, written here, against
upstreams that have not taken them. These patches are **not ours** — go-erofs maintains them,
against a version of erofs-utils it also chooses. Copying them in would create a second source
of truth that goes stale the first time upstream fixes one, and we would not notice.

At `52cc42c` the recipe is erofs-utils **v1.9.1** against **lz4 1.10.0**.

## The recipe

Straight out of `.github/workflows/ci.yml` in `erofs/go-erofs`, job
**`cross-compile-mkfs-windows`**. Read that job, not this summary, if the two ever disagree.

1. `apt-get install autoconf automake libtool pkg-config mingw-w64`
2. Cross-compile **lz4** static (`TARGET_OS=Windows BUILD_STATIC=yes BUILD_SHARED=no`) and
   install it into `/usr/x86_64-w64-mingw32`.
3. Copy `.github/workflows/mingw-compat-headers/` into `/usr/x86_64-w64-mingw32/include/`.
   Seven headers: `posix_compat.h`, `regex.h`, and stubs for `sys/{ioctl,mman,syscall,uio,xattr}.h`.
4. Apply `.github/workflows/patches/erofs-utils/*.patch` **in order** — they are sequential, and
   `003` does not apply to a pristine tree:

   | Patch | What it does |
   | --- | --- |
   | `001-windows.patch` | the bulk of it — build-system and POSIX-shim wiring |
   | `002-windows-stdin-blocking.patch` | stdin blocking behaviour |
   | `003-windows-pipe-lseek.patch` | `lseek` on a pipe, which Windows does not allow |
   | `004-windows-gzran-zlib-guard.patch` | guards gzran behind zlib, which we build without |
   | `005-windows-tar-uid-gid.patch` | uid/gid handling on the tar path |

5. `./autogen.sh`, then `./configure --host=x86_64-w64-mingw32 --disable-shared --enable-lz4`
   plus `--without-{zlib,libzstd,selinux,uuid,openssl}` and `--disable-{lzma,fuse,debug}`.
6. `make -C lib` and `make -C mkfs`, both with `CPPFLAGS="-include posix_compat.h"`.
7. `x86_64-w64-mingw32-strip mkfs/mkfs.erofs.exe`.

### The runner is pinned to `ubuntu-24.04`, and that is load-bearing

Not `ubuntu-latest`. Built on Debian trixie's newer mingw-w64 the recipe **fails**:

```
rebuild.c:54:13: error: implicit declaration of function 'asprintf'
make: *** [Makefile:885: liberofs_la-rebuild.lo] Error 1
```

Newer mingw-w64 headers no longer declare `asprintf` under the feature-test macros this build
sets. go-erofs's CI runs on `ubuntu-latest` and does not hit it because that is still Ubuntu 24.04
today — which means their recipe will break for them, silently and on someone else's schedule,
when GitHub moves the label. We pin the concrete image so our build breaks when *we* change it.

## What is built, and what is not

**x86-64 only.** go-erofs's recipe cross-compiles for `x86_64-w64-mingw32` and nothing else, so
there is no `mkfs.erofs.exe` for Windows on ARM. On an arm64 Windows host the erofs differ will
skip itself — see `packaging/containerd-windows/README.md`, which is where that matters.

## Verified here, on Linux, 2026-08-13

Built with the recipe above in a container; the resulting binary was inspected, **not run** —
this is a Linux machine.

| Check | Result |
| --- | --- |
| `head -c2` | `MZ` |
| `objdump -f` | `file format pei-x86-64`, `architecture: i386:x86-64` — PE32+ |
| `objdump -p` imports | `KERNEL32.dll`, `msvcrt.dll` — and nothing else |
| size | 550,912 bytes, stripped |
| `strings` | contains `--tar=X   generate a full or index-only image from a tarball(-ish) source` |

That last row is the one that matters for containerd. The differ's plugin init runs
`mkfs.erofs --help` and greps the output for the literal `--tar=`; if the grep fails the plugin
returns `ErrSkipPlugin` and vanishes. The string is present in the binary's help text, so the
grep should succeed — **should**, because nobody has executed this binary. It has never run on
Windows, or anywhere.
