#!/usr/bin/env python3
"""Assert that a Linux artifact is the ELF it claims to be, and exports what its consumer binds.

WHY THIS IS A SCRIPT AND NOT THREE LINES OF SHELL. Both checks here have a well-known
degenerate form that passes on everything, and this project has shipped both:

  * `head -c 4` finds \x7fELF and stops. It never looks at e_machine, so an aarch64 binary
    published as the amd64 artifact sails through. A user on the wrong architecture gets
    "cannot execute binary file" and no clue why.

  * `strings libkrun.so | grep krun_add_disk` is a SUBSTRING match. `krun_add_disk2` contains
    `krun_add_disk`, so that grep reports the symbol present no matter what the library
    exports. Worse, `.dynsym` also holds the symbols the library IMPORTS: `memcpy` appears in
    every one of these files, and any strings- or `nm -D`-based test calls it an export.

So this reads the actual ELF structures: e_machine from the header, and `.dynsym` filtered to
entries that are genuinely DEFINED (st_shndx != SHN_UNDEF) and GLOBAL or WEAK. Symbol names
are matched WHOLE, never as substrings.

WHAT IT DOES NOT PROVE. That the library works. A symbol can be exported and return -ENOSYS.
This is a load-time contract check: nerdbox's loader (internal/vm/libkrun/instance.go,
openLibkrun in krun.go) reflects over its whole binding table and resolves every `C:"krun_*"`
tag EAGERLY at dlopen. One missing symbol does not fail late at the call site — it fails the
load, so no VM starts at all. That is the failure this catches, and only that.

Usage:
    assert-elf.py <file> [--machine {x86-64,aarch64}] [--type {exec,dyn}]
                         [--require SYM ...] [--forbid SYM ...]

Every assertion is optional and all of them are checked in one pass, so a single invocation
can say "this is an aarch64 shared object exporting these nineteen names".

EXIT STATUS IS PART OF THE CONTRACT, because the negative controls in CI depend on telling
these apart:

    0   every requested assertion held
    1   the file was read and understood, and an assertion FAILED
    2   the file could not be read or the arguments made no sense

A control that accepted any non-zero status would treat a mistyped path (2) as a successful
rejection (1), which is how a negative control quietly stops testing anything. Callers should
demand exactly 1.

`--forbid` asserts a name is NOT reported as an export. It exists so the workflow can prove
the substring and imported-symbol traps described above are actually closed.
"""

import argparse
import struct
import sys

# ELF constants. Named rather than inlined so the checks below read as the spec does.
ELF_MAGIC = b"\x7fELF"
ELFCLASS64 = 2
ELFDATA2LSB = 1
ET_EXEC, ET_DYN = 2, 3
SHT_DYNSYM = 11
SHN_UNDEF = 0
STB_GLOBAL, STB_WEAK = 1, 2

# e_machine values we publish artifacts for, keyed by the name a human would use.
MACHINES = {"x86-64": 62, "aarch64": 183}
TYPES = {"exec": ET_EXEC, "dyn": ET_DYN}


class NotELF(Exception):
    pass


class ELF64:
    """A deliberately minimal little-endian ELF64 reader — header plus .dynsym."""

    def __init__(self, data: bytes):
        if len(data) < 64 or data[:4] != ELF_MAGIC:
            raise NotELF(
                f"not an ELF file: first 4 bytes are {data[:4]!r}, expected {ELF_MAGIC!r}"
            )
        if data[4] != ELFCLASS64:
            raise NotELF(f"not ELF64: EI_CLASS is {data[4]}, expected {ELFCLASS64}")
        if data[5] != ELFDATA2LSB:
            raise NotELF(
                f"not little-endian: EI_DATA is {data[5]}, expected {ELFDATA2LSB}"
            )
        self.data = data
        # e_type at 16, e_machine at 18; both Elf64_Half.
        self.e_type, self.e_machine = struct.unpack_from("<HH", data, 16)
        # e_shoff at 40 (Elf64_Off), e_shentsize at 58, e_shnum at 60.
        self.e_shoff = struct.unpack_from("<Q", data, 40)[0]
        self.e_shentsize, self.e_shnum = struct.unpack_from("<HH", data, 58)

    def _sections(self):
        for i in range(self.e_shnum):
            off = self.e_shoff + i * self.e_shentsize
            if off + 64 > len(self.data):
                raise NotELF("section header table runs past end of file")
            # sh_type at +4, sh_link at +40, sh_offset at +24, sh_size at +32,
            # sh_entsize at +56.
            sh_type = struct.unpack_from("<I", self.data, off + 4)[0]
            sh_link = struct.unpack_from("<I", self.data, off + 40)[0]
            sh_offset, sh_size = struct.unpack_from("<QQ", self.data, off + 24)
            sh_entsize = struct.unpack_from("<Q", self.data, off + 56)[0]
            yield i, sh_type, sh_link, sh_offset, sh_size, sh_entsize

    def _section_at(self, index):
        off = self.e_shoff + index * self.e_shentsize
        sh_offset, sh_size = struct.unpack_from("<QQ", self.data, off + 24)
        return sh_offset, sh_size

    def defined_dynamic_symbols(self) -> set:
        """Names of dynamic symbols this object DEFINES — i.e. genuine exports.

        Two filters do the work that a strings-based check cannot:
        st_shndx != SHN_UNDEF drops everything the object merely imports (memcpy, dlopen,
        pthread_create), and the binding filter drops LOCAL symbols, which are not
        dlsym-visible. Without the first filter every libc function this library calls would
        be reported as an export of it.
        """
        names = set()
        for _i, sh_type, sh_link, sh_offset, sh_size, sh_entsize in self._sections():
            if sh_type != SHT_DYNSYM or sh_entsize == 0:
                continue
            str_off, str_size = self._section_at(sh_link)
            strtab = self.data[str_off : str_off + str_size]
            for k in range(sh_size // sh_entsize):
                e = sh_offset + k * sh_entsize
                st_name = struct.unpack_from("<I", self.data, e)[0]
                st_info = self.data[e + 4]
                st_shndx = struct.unpack_from("<H", self.data, e + 6)[0]
                if st_shndx == SHN_UNDEF:
                    continue
                if (st_info >> 4) not in (STB_GLOBAL, STB_WEAK):
                    continue
                end = strtab.find(b"\0", st_name)
                if end < 0:
                    continue
                name = strtab[st_name:end].decode("utf-8", "replace")
                if name:
                    names.add(name)
        return names


def load(path: str) -> ELF64:
    with open(path, "rb") as fh:
        return ELF64(fh.read())


def check(args, elf: ELF64) -> bool:
    """Run every requested assertion, reporting all failures rather than the first."""
    ok = True

    if args.machine is not None:
        want = MACHINES[args.machine]
        if elf.e_machine != want:
            actual = next(
                (n for n, v in MACHINES.items() if v == elf.e_machine),
                f"unknown e_machine {elf.e_machine}",
            )
            print(
                f"  {args.file}: WRONG ARCHITECTURE — is {actual}, expected {args.machine}",
                file=sys.stderr,
            )
            ok = False

    if args.type is not None and elf.e_type != TYPES[args.type]:
        actual = next(
            (n for n, v in TYPES.items() if v == elf.e_type), f"unknown ({elf.e_type})"
        )
        print(
            f"  {args.file}: WRONG ELF TYPE — is {actual}, expected {args.type}",
            file=sys.stderr,
        )
        ok = False

    if args.require or args.forbid:
        exports = elf.defined_dynamic_symbols()
        missing = [s for s in args.require if s not in exports]
        forbidden = [s for s in args.forbid if s in exports]

        for sym in args.require:
            if sym not in missing:
                print(f"  export present: {sym}")
        # Report EVERY missing symbol, not just the first. nerdbox's loader names only the
        # one symbol it tripped over, so failing fast here would turn a diagnosis into one
        # CI run per missing symbol — the loop this script exists to avoid.
        for sym in missing:
            print(
                f"  EXPORT MISSING: {sym} — nerdbox resolves eagerly, so this fails the dlopen",
                file=sys.stderr,
            )
        for sym in forbidden:
            print(
                f"  UNEXPECTEDLY EXPORTED: {sym} — this assertion was supposed to reject it",
                file=sys.stderr,
            )
        if missing:
            print(
                f"{len(missing)} of {len(args.require)} required symbols are not exported",
                file=sys.stderr,
            )
        if missing or forbidden:
            ok = False
        elif args.require:
            print(
                f"  {args.file}: all {len(args.require)} required symbols exported "
                f"({len(exports)} defined dynamic symbols in total)"
            )

    if ok:
        kind = next((n for n, v in TYPES.items() if v == elf.e_type), elf.e_type)
        machine = next(
            (n for n, v in MACHINES.items() if v == elf.e_machine), elf.e_machine
        )
        print(f"  {args.file}: ELF64 LSB {kind}, {machine} — all assertions hold")
    return ok


def main(argv) -> int:
    p = argparse.ArgumentParser(
        description="Assert a Linux artifact's ELF identity and exported symbols."
    )
    p.add_argument("file")
    p.add_argument("--machine", choices=sorted(MACHINES))
    p.add_argument("--type", choices=sorted(TYPES))
    p.add_argument("--require", nargs="*", default=[], metavar="SYM")
    p.add_argument("--forbid", nargs="*", default=[], metavar="SYM")
    args = p.parse_args(argv[1:])

    # Asserting nothing must not look like success — that is the shape of a check which was
    # accidentally disabled and went green forever.
    if args.machine is None and args.type is None and not args.require and not args.forbid:
        print("nothing asserted: pass at least one of --machine/--type/--require/--forbid",
              file=sys.stderr)
        return 2

    try:
        elf = load(args.file)
    except NotELF as exc:
        print(f"  {args.file}: {exc}", file=sys.stderr)
        return 1
    except OSError as exc:
        print(f"cannot read {args.file}: {exc}", file=sys.stderr)
        return 2

    return 0 if check(args, elf) else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
