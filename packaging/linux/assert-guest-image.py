#!/usr/bin/env python3
"""Assert that a built nerdbox guest image is for the architecture it is about to be labelled.

    assert-guest-image.py <dir> --arch {x86_64,arm64}

WHY THIS EXISTS. .github/workflows/guest-image.yml built a kernel and a rootfs and then
asserted nothing whatsoever about either: the only check was `sha256sum` of the outputs
against themselves, which is true of any two files. The architecture guard added before it
globs the CHECKED-OUT REPOSITORY's kernel configs — it never opens the artifact. Hardcoding
`KERNEL_ARCH: x86_64` therefore produced an entirely green `arm64` run whose artifact held an
x86_64 kernel, which release.yml would lay into `boks-guest_<v>_arm64.tar.gz` beside a
SOURCE.txt saying arm64. Reproduced on 2026-08-16 against a staged `_output/`.

WHAT IT ASSERTS, in one pass, reporting every failure rather than the first:

  1. The two files nerdbox's bake produces are present under the names the shim looks them up
     by — `nerdbox-kernel-<arch>` and `nerdbox-rootfs.erofs` (internal/doctor/checks.go, and
     scripts/build-nerdbox-guest.sh, which copies exactly those two names).
  2. No kernel for any OTHER architecture is present. A directory holding
     `nerdbox-kernel-x86_64` in an arm64 build is the failure above, and it is caught by the
     name before anything is parsed.
  3. The kernel really is a kernel for that architecture, read out of the image itself.
  4. The rootfs really carries the EROFS superblock magic.
  5. The rootfs's /sbin/vminitd really is an ELF for that architecture.

WHY 5 EXISTS, AND WHY IT IS THE IMPORTANT ONE. Checks 1-4 all failed to notice the defect
that shipped in v0.1.0 and v0.1.1: both guest archives carried an ARM64 kernel or an x86_64
one as their name promised, a valid EROFS rootfs — and the SAME x86_64 `/sbin/vminitd` inside
it, byte for byte. nerdbox's bake takes the rootfs's architecture from the target platform,
which its bake file never sets, so on an x86_64 runner `KERNEL_ARCH=arm64` cross-compiles the
kernel and builds the rootfs natively. Nothing anywhere caught it: the kernel is right, the
magic is right, and unlike the kernel the rootfs's FILENAME carries no architecture, so it is
identical in both archives. A Mac booting the result panicked with

    Kernel panic - not syncing: Requested init /sbin/vminitd failed (error -8).

-8 is ENOEXEC, and neither the panic nor `boks doctor` names an architecture, a file or a
release. Reading the init binary the kernel will actually exec is the assertion that would
have turned that into a red build.

HOW IT READS THE ROOTFS. `fsck.erofs --extract=<file> --path=/sbin/vminitd`, from erofs-utils,
because the image is lz4-compressed and a byte scan for `\x7fELF` finds only compressed noise
— tried first, and it reported four "ELF headers" with impossible e_machine values. When
fsck.erofs is missing this script exits 2, "the check could not be made", which is
deliberately distinct from a pass: a CI runner that stops shipping erofs-utils must go red,
not quietly drop the only assertion that catches this.

WHY THE KERNEL IS NOT SIMPLY AN ELF CHECK. It is one on x86_64 — nerdbox's recipe produces an
ELF `vmlinux` there, deliberately not a bzImage, because `krun_set_kernel` takes ELF. It is
NOT one on arm64: the arm64 build measured on 2026-08-13 (docs/install.md) produced a
15,835,648-byte `nerdbox-kernel-arm64` carrying the arm64 *Image* magic `ARM\\x64`, which is
not an ELF at all. Asserting ELF unconditionally would have turned a real arm64 build red.
So the format is IDENTIFIED first and then required to agree with the requested architecture;
a format this script does not recognise is a failure, never a shrug.

The ELF path reuses assert-elf.py's reader rather than re-deriving e_machine, so there is one
ELF parser in this repository and it is the one with the negative controls in CI.

EXIT STATUS IS PART OF THE CONTRACT, because the controls in CI depend on telling these apart:

    0   every assertion held
    1   the directory was read and understood, and an assertion FAILED
    2   the directory could not be read, or the arguments made no sense
"""

import argparse
import importlib.util
import pathlib
import shutil
import struct
import subprocess
import sys
import tempfile

HERE = pathlib.Path(__file__).resolve().parent

# nerdbox spells 64-bit ARM `arm64`, not `aarch64`, and the shim looks the kernel up by Go's
# GOARCH with only amd64 rewritten to x86_64. These are the two spellings that may appear in
# a filename; the values are what each format's own header must say.
ARCHES = {
    "x86_64": {"elf_machine": "x86-64"},
    "arm64": {"elf_machine": "aarch64"},
}

ROOTFS_NAME = "nerdbox-rootfs.erofs"

# The path inside the rootfs that the guest kernel execs as PID 1 — nerdbox's Dockerfile
# installs the binary there, and a boot log says so in as many words: `Run /sbin/vminitd as
# init process`. It is the one file whose architecture decides whether the VM comes up at all.
INIT_PATH = "/sbin/vminitd"


class CannotCheck(Exception):
    """The machine cannot make an assertion — as opposed to the assertion failing.

    Raised only for a missing tool. Everything that can be read and is wrong is a problem in
    check() and exits 1; this exits 2, so that "erofs-utils is not installed" can never be
    mistaken for "the rootfs is the right architecture".
    """


class BadRootfs(Exception):
    """The rootfs was opened and something about it is wrong. One of check()'s problems."""

# EROFS superblock: magic 0xE0F5E1E2, little-endian, at offset 1024 (EROFS_SUPER_OFFSET).
EROFS_SUPER_OFFSET = 1024
EROFS_MAGIC = 0xE0F5E1E2

# arm64 Linux Image header: the 4-byte magic "ARM\x64" at offset 56 (arch/arm64/kernel/head.S,
# and Documentation/arch/arm64/booting.rst).
ARM64_IMAGE_MAGIC_OFFSET = 56
ARM64_IMAGE_MAGIC = b"ARM\x64"

# x86 bzImage: the 4-byte "HdrS" signature at offset 0x202 (Documentation/arch/x86/boot.rst).
BZIMAGE_MAGIC_OFFSET = 0x202
BZIMAGE_MAGIC = b"HdrS"

ELF_MAGIC = b"\x7fELF"


def load_elf_reader():
    """Import assert-elf.py's ELF64 class by path — the filename has a hyphen in it."""
    spec = importlib.util.spec_from_file_location("assert_elf", HERE / "assert-elf.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def kernel_format(data: bytes):
    """Identify the kernel image, returning (format-name, architecture-it-declares).

    The architecture is None when the format itself does not carry one that this script can
    read; an unrecognised format returns (None, None) and is a failure at the call site.
    """
    if data[:4] == ELF_MAGIC:
        elf_module = load_elf_reader()
        try:
            elf = elf_module.ELF64(data)
        except elf_module.NotELF as exc:
            return (f"a broken ELF ({exc})", None)
        name = next(
            (n for n, v in elf_module.MACHINES.items() if v == elf.e_machine),
            f"unknown e_machine {elf.e_machine}",
        )
        arch = next((a for a, spec in ARCHES.items() if spec["elf_machine"] == name), None)
        return (f"ELF64 vmlinux, {name}", arch)
    if data[ARM64_IMAGE_MAGIC_OFFSET : ARM64_IMAGE_MAGIC_OFFSET + 4] == ARM64_IMAGE_MAGIC:
        return ("arm64 Linux Image", "arm64")
    if data[BZIMAGE_MAGIC_OFFSET : BZIMAGE_MAGIC_OFFSET + 4] == BZIMAGE_MAGIC:
        # Not what nerdbox's recipe produces — krun_set_kernel takes ELF — so this is
        # identified in order to say so precisely rather than to be accepted.
        return ("x86 bzImage", "x86_64")
    return (None, None)


def init_binary_arch(rootfs: pathlib.Path):
    """Return (architecture, description) for the rootfs's /sbin/vminitd.

    The architecture is None when the ELF names a machine Boks does not publish for, so the
    caller can report what it actually found rather than "not arm64".

    Raises CannotCheck when there is no fsck.erofs to open the image with — that is exit 2 —
    and BadRootfs when the image itself is wrong, which is one of check()'s problems.
    """
    fsck = shutil.which("fsck.erofs")
    if fsck is None:
        raise CannotCheck(
            f"fsck.erofs is not on PATH, so {INIT_PATH} cannot be read out of {rootfs.name}. "
            f"Install erofs-utils (apt: erofs-utils, brew: erofs-utils). This is exit 2, not "
            f"a pass: the check was not made."
        )

    with tempfile.TemporaryDirectory() as tmp:
        # --path extracts that one inode, and writes it AT the --extract path rather than
        # under it, so this names a file that does not exist yet rather than a directory.
        target = pathlib.Path(tmp) / "vminitd"
        proc = subprocess.run(
            [fsck, f"--extract={target}", f"--path={INIT_PATH}", str(rootfs)],
            capture_output=True,
            text=True,
        )
        if proc.returncode != 0 or not target.is_file():
            said = (proc.stderr or proc.stdout or "").strip().splitlines()
            raise BadRootfs(
                f"{INIT_PATH} could not be extracted from {rootfs.name} "
                f"(fsck.erofs exit {proc.returncode}"
                + (f": {said[-1]}" if said else "")
                + ")"
            )
        data = target.read_bytes()

    elf_module = load_elf_reader()
    try:
        elf = elf_module.ELF64(data)
    except elf_module.NotELF as exc:
        raise BadRootfs(f"{INIT_PATH} is not an ELF64 executable ({exc})") from exc

    name = next(
        (n for n, v in elf_module.MACHINES.items() if v == elf.e_machine),
        f"unknown e_machine {elf.e_machine}",
    )
    found = next((a for a, spec in ARCHES.items() if spec["elf_machine"] == name), None)
    return found, f"ELF64 {name}, {len(data)} bytes"


def check(directory: pathlib.Path, arch: str) -> bool:
    problems = []
    kernel_name = f"nerdbox-kernel-{arch}"

    present = sorted(p.name for p in directory.iterdir() if p.is_file())
    print(f"  {directory}: {', '.join(present) or '(empty)'}")

    # 2 before 1: a kernel for the wrong architecture is the failure this exists for, and its
    # name says so before a byte is parsed.
    strays = [
        f"nerdbox-kernel-{other}" for other in ARCHES if other != arch
    ]
    for stray in strays:
        if stray in present:
            problems.append(
                f"{stray} is present in an artifact labelled {arch} — the build used a "
                f"different KERNEL_ARCH from the one the artifact will be named for"
            )

    kernel = directory / kernel_name
    if not kernel.is_file():
        problems.append(
            f"{kernel_name} is missing; the shim looks the kernel up by that exact name"
        )
    else:
        data = kernel.read_bytes()
        fmt, declared = kernel_format(data)
        if fmt is None:
            problems.append(
                f"{kernel_name} is not a kernel image this script recognises: it is neither "
                f"an ELF, an arm64 Image, nor a bzImage (first bytes {data[:8]!r})"
            )
        elif declared is None:
            problems.append(f"{kernel_name} is {fmt}, which names no architecture Boks ships")
        elif declared != arch:
            problems.append(
                f"{kernel_name} is {fmt} — that is {declared}, and this build is {arch}"
            )
        else:
            print(f"  {kernel_name}: {fmt}, {len(data)} bytes")

    rootfs = directory / ROOTFS_NAME
    if not rootfs.is_file():
        problems.append(f"{ROOTFS_NAME} is missing")
    else:
        with rootfs.open("rb") as fh:
            fh.seek(EROFS_SUPER_OFFSET)
            head = fh.read(4)
        if len(head) < 4 or struct.unpack("<I", head)[0] != EROFS_MAGIC:
            problems.append(
                f"{ROOTFS_NAME} has no EROFS superblock: bytes at offset "
                f"{EROFS_SUPER_OFFSET} are {head!r}, expected magic "
                f"0x{EROFS_MAGIC:08x} little-endian"
            )
        else:
            print(f"  {ROOTFS_NAME}: EROFS superblock, {rootfs.stat().st_size} bytes")
            try:
                found, detail = init_binary_arch(rootfs)
            except BadRootfs as exc:
                problems.append(str(exc))
            else:
                if found is None:
                    problems.append(
                        f"{ROOTFS_NAME} holds a {INIT_PATH} that is {detail} — that names no "
                        f"architecture Boks ships, and this build is {arch}"
                    )
                elif found != arch:
                    problems.append(
                        f"{ROOTFS_NAME} holds a {found} {INIT_PATH} ({detail}) and this "
                        f"build is {arch}. The kernel would exec it and panic with ENOEXEC: "
                        f"'Requested init {INIT_PATH} failed (error -8)'. The rootfs target "
                        f"needs its platform set — KERNEL_ARCH does not reach it"
                    )
                else:
                    print(f"  {ROOTFS_NAME} {INIT_PATH}: {detail}")

    for problem in problems:
        print(f"  GUEST IMAGE WRONG: {problem}", file=sys.stderr)
    return not problems


def main(argv) -> int:
    p = argparse.ArgumentParser(
        description="Assert a built nerdbox guest image matches the architecture it claims."
    )
    p.add_argument("directory")
    p.add_argument("--arch", required=True, choices=sorted(ARCHES))
    args = p.parse_args(argv[1:])

    directory = pathlib.Path(args.directory)
    if not directory.is_dir():
        print(f"cannot read {directory}: not a directory", file=sys.stderr)
        return 2

    try:
        held = check(directory, args.arch)
    except CannotCheck as exc:
        print(f"cannot check {directory}: {exc}", file=sys.stderr)
        return 2
    if not held:
        return 1
    print(f"  {directory}: a complete {args.arch} guest image — all assertions hold")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
