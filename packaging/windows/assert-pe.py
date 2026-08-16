#!/usr/bin/env python3
"""Assert that a Windows artifact is the PE it claims to be, and EXPORTS what its consumer binds.

This is packaging/linux/assert-elf.py's counterpart, written for the same reason and after
the same bug. Read that file first; the argument is identical and the failures it describes
had all shipped here too.

WHY THIS IS A SCRIPT AND NOT THREE LINES OF SHELL. Until 2026-08-16 the Windows workflow
tested `krun.dll` like this:

    tr -c '[:print:]' '\\n' <"$dll" | sort -u >/tmp/strings.txt
    grep -qxF krun_create_ctx /tmp/strings.txt

That is every run of printable bytes anywhere in the file. It cannot tell an export from an
import, from a `.rdata` literal, from a debug string, or from a file that is not a PE image
at all: a 369-byte file containing nothing but the twenty names as NUL-terminated ASCII
passed all twenty assertions and exited 0. `krun.dll` was also the only shipped binary with
no format or architecture assertion of any kind, while `mkfs.erofs.exe` had
`objdump -f | grep pei-x86-64` and `libkrun.so` had assert-elf.py.

So this walks the actual structures: the DOS stub's e_lfanew, the PE signature, the COFF
Machine field, the optional header's data directory 0, and IMAGE_EXPORT_DIRECTORY's
AddressOfNames — the only table in the file that says what a caller may resolve. RVAs are
translated to file offsets through the section table. Names are matched WHOLE, never as
substrings, so `krun_add_vsock` is not satisfied by the bytes of `krun_add_vsock_port2` —
the exact bug this repository shipped on the Windows side.

IMPORTS ARE NOT EXPORTS, and nothing here reads the import directory. A PE's imports
(`memcpy` from VCRUNTIME140.dll, `CreateFileW` from KERNEL32.dll) are as present in the
file's bytes as its exports; the string-based test called them exports of the library.
Asserting an imported name is NOT found is what proves the export directory is really the
thing being read, and the CI controls do exactly that.

WHAT IT DOES NOT PROVE. That the DLL works, or even that it loads: a forwarder export
resolves to a DLL that may not be installed, and an exported function can return -ENOSYS.
This is the load-time contract nerdbox's loader depends on — internal/vm/libkrun/krun.go
reflects over its whole binding table and resolves every `C:"krun_*"` tag EAGERLY, so one
missing name fails LoadLibrary/GetProcAddress and no VM starts. That is what this catches,
and only that.

Usage:
    assert-pe.py <file> [--machine {x86-64,arm64,i386,arm}] [--kind {dll,exe}]
                        [--require SYM ...] [--forbid SYM ...] [--list]

Every assertion is optional and all of them are checked in one pass.

EXIT STATUS IS PART OF THE CONTRACT, because the negative controls in CI depend on telling
these apart:

    0   every requested assertion held
    1   the file was read and understood, and an assertion FAILED
    2   the file could not be read, or the arguments made no sense

A control that accepted any non-zero status would treat a mistyped path (2) as a successful
rejection (1), which is how a negative control quietly stops testing anything. Callers should
demand exactly 1.
"""

import argparse
import struct
import sys

# PE/COFF constants, named rather than inlined so the checks read as the spec does.
# Reference: Microsoft PE Format, "MS-DOS Stub", "COFF File Header", "Optional Header",
# ".edata Section".
DOS_MAGIC = b"MZ"
PE_SIGNATURE = b"PE\0\0"
E_LFANEW_OFFSET = 0x3C
COFF_HEADER_SIZE = 20
PE32_MAGIC = 0x10B
PE32PLUS_MAGIC = 0x20B
IMAGE_FILE_DLL = 0x2000
SECTION_HEADER_SIZE = 40
EXPORT_DIRECTORY_INDEX = 0

# IMAGE_FILE_MACHINE_*, keyed by the name a human would use. These are the four a Boks
# artifact could plausibly be; anything else is reported by number rather than guessed at.
MACHINES = {
    "i386": 0x014C,
    "arm": 0x01C0,
    "x86-64": 0x8664,
    "arm64": 0xAA64,
}

# The number of data directories that must be present before index 0 can be read. A COFF
# object file (no optional header at all) has none, and reading its "export directory" would
# be reading whatever follows in the file.
MIN_DATA_DIRECTORIES = 1


class NotPE(Exception):
    pass


class PEFile:
    """A deliberately minimal PE reader — headers, the section table, and .edata's names."""

    def __init__(self, data: bytes):
        self.data = data
        if len(data) < E_LFANEW_OFFSET + 4:
            raise NotPE(f"too short to be a PE image: {len(data)} bytes")
        if data[:2] != DOS_MAGIC:
            raise NotPE(
                f"not a PE image: first 2 bytes are {data[:2]!r}, expected {DOS_MAGIC!r}"
            )
        self.pe_offset = struct.unpack_from("<I", data, E_LFANEW_OFFSET)[0]
        if self.pe_offset + COFF_HEADER_SIZE + 4 > len(data):
            raise NotPE(
                f"e_lfanew points to 0x{self.pe_offset:x}, past the end of a "
                f"{len(data)}-byte file"
            )
        if data[self.pe_offset : self.pe_offset + 4] != PE_SIGNATURE:
            raise NotPE(
                f"no PE signature at e_lfanew (0x{self.pe_offset:x}): found "
                f"{data[self.pe_offset:self.pe_offset + 4]!r}"
            )

        coff = self.pe_offset + 4
        (
            self.machine,
            self.n_sections,
            _timestamp,
            _sym_ptr,
            _n_syms,
            self.opt_header_size,
            self.characteristics,
        ) = struct.unpack_from("<HHIIIHH", data, coff)

        self.opt_offset = coff + COFF_HEADER_SIZE
        self.magic = None
        self.data_directories = []
        if self.opt_header_size:
            self._read_optional_header()

        self.sections = self._read_sections()

    def _read_optional_header(self):
        data, off = self.data, self.opt_offset
        if off + 2 > len(data):
            raise NotPE("optional header runs past the end of the file")
        self.magic = struct.unpack_from("<H", data, off)[0]
        if self.magic == PE32PLUS_MAGIC:
            # PE32+ drops BaseOfData, so NumberOfRvaAndSizes sits 4 bytes earlier than PE32
            # and the 8-byte image-base fields shift everything before it.
            n_rva_off, dir_off = off + 108, off + 112
        elif self.magic == PE32_MAGIC:
            n_rva_off, dir_off = off + 92, off + 96
        else:
            raise NotPE(f"unknown optional header magic 0x{self.magic:04x}")
        if n_rva_off + 4 > len(data):
            raise NotPE("optional header is truncated before NumberOfRvaAndSizes")
        n_rva = struct.unpack_from("<I", data, n_rva_off)[0]
        # A declared count is not evidence the entries are there. Read only as many as the
        # file can actually hold, so a truncated image raises rather than reporting the
        # bytes that happen to follow as a data directory.
        available = max(0, (len(data) - dir_off) // 8)
        for i in range(min(n_rva, available)):
            rva, size = struct.unpack_from("<II", data, dir_off + i * 8)
            self.data_directories.append((rva, size))

    def _read_sections(self):
        first = self.opt_offset + self.opt_header_size
        sections = []
        for i in range(self.n_sections):
            off = first + i * SECTION_HEADER_SIZE
            if off + SECTION_HEADER_SIZE > len(self.data):
                raise NotPE("section header table runs past the end of the file")
            name = self.data[off : off + 8].rstrip(b"\0").decode("latin-1")
            virtual_size, virtual_address, raw_size, raw_pointer = struct.unpack_from(
                "<IIII", self.data, off + 8
            )
            sections.append((name, virtual_address, virtual_size, raw_pointer, raw_size))
        return sections

    def offset_of(self, rva: int):
        """File offset for a relative virtual address, or None if no section covers it."""
        for _name, va, vsize, raw_ptr, raw_size in self.sections:
            # Use the raw size as the bound: the tail of a section that exists only in
            # memory (bss-like) has no bytes in the file to read.
            span = min(vsize, raw_size) if vsize else raw_size
            if va <= rva < va + span:
                return raw_ptr + (rva - va)
        return None

    def _string_at(self, rva: int):
        off = self.offset_of(rva)
        if off is None:
            return None
        end = self.data.find(b"\0", off)
        if end < 0:
            return None
        return self.data[off:end].decode("utf-8", "replace")

    def exported_names(self) -> set:
        """Names in IMAGE_EXPORT_DIRECTORY.AddressOfNames — what GetProcAddress can resolve.

        Ordinal-only exports are deliberately not counted: nerdbox resolves every symbol by
        NAME, so an export reachable only by ordinal does not satisfy its loader and must not
        satisfy this check either.
        """
        if len(self.data_directories) <= EXPORT_DIRECTORY_INDEX:
            return set()
        rva, size = self.data_directories[EXPORT_DIRECTORY_INDEX]
        if not rva or not size:
            return set()
        table = self.offset_of(rva)
        if table is None:
            return set()
        # IMAGE_EXPORT_DIRECTORY: NumberOfNames at +24, AddressOfNames at +32.
        if table + 40 > len(self.data):
            raise NotPE("export directory runs past the end of the file")
        n_names = struct.unpack_from("<I", self.data, table + 24)[0]
        names_rva = struct.unpack_from("<I", self.data, table + 32)[0]
        names_off = self.offset_of(names_rva)
        if names_off is None:
            return set()
        names = set()
        for i in range(n_names):
            entry = names_off + i * 4
            if entry + 4 > len(self.data):
                raise NotPE("export name table runs past the end of the file")
            name = self._string_at(struct.unpack_from("<I", self.data, entry)[0])
            if name:
                names.add(name)
        return names

    @property
    def is_dll(self) -> bool:
        return bool(self.characteristics & IMAGE_FILE_DLL)

    def describe(self) -> str:
        machine = next(
            (n for n, v in MACHINES.items() if v == self.machine),
            f"unknown machine 0x{self.machine:04x}",
        )
        bits = {PE32PLUS_MAGIC: "PE32+", PE32_MAGIC: "PE32"}.get(
            self.magic, f"magic 0x{self.magic:04x}" if self.magic else "no optional header"
        )
        return f"{bits} {machine} {'DLL' if self.is_dll else 'executable'}"


def check(args, pe: PEFile) -> bool:
    """Run every requested assertion, reporting all failures rather than the first."""
    ok = True

    if args.machine is not None and pe.machine != MACHINES[args.machine]:
        actual = next(
            (n for n, v in MACHINES.items() if v == pe.machine),
            f"unknown machine 0x{pe.machine:04x}",
        )
        print(
            f"  {args.file}: WRONG ARCHITECTURE — is {actual}, expected {args.machine}",
            file=sys.stderr,
        )
        ok = False

    if args.kind is not None:
        want_dll = args.kind == "dll"
        if pe.is_dll != want_dll:
            print(
                f"  {args.file}: WRONG IMAGE KIND — IMAGE_FILE_DLL is "
                f"{'set' if pe.is_dll else 'clear'}, expected {args.kind}",
                file=sys.stderr,
            )
            ok = False

    if args.require or args.forbid or args.list:
        exports = pe.exported_names()

        if args.list:
            for name in sorted(exports):
                print(f"  export: {name}")

        missing = [s for s in args.require if s not in exports]
        forbidden = [s for s in args.forbid if s in exports]

        for sym in args.require:
            if sym not in missing:
                print(f"  export present: {sym}")
        # Report EVERY missing symbol. Windows names only the one symbol GetProcAddress
        # tripped over, so failing fast here would turn a diagnosis into one CI run per
        # missing symbol — the loop this script exists to avoid.
        for sym in missing:
            print(
                f"  EXPORT MISSING: {sym} — nerdbox resolves eagerly, so this fails the load",
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
                f"({len(exports)} named exports in total)"
            )

    if ok:
        print(f"  {args.file}: {pe.describe()} — all assertions hold")
    return ok


def main(argv) -> int:
    p = argparse.ArgumentParser(
        description="Assert a Windows artifact's PE identity and exported symbols."
    )
    p.add_argument("file")
    p.add_argument("--machine", choices=sorted(MACHINES))
    p.add_argument("--kind", choices=["dll", "exe"])
    p.add_argument("--require", nargs="*", default=[], metavar="SYM")
    p.add_argument("--forbid", nargs="*", default=[], metavar="SYM")
    p.add_argument("--list", action="store_true", help="print every named export")
    args = p.parse_args(argv[1:])

    # Asserting nothing must not look like success — that is the shape of a check which was
    # accidentally disabled and went green forever.
    if (
        args.machine is None
        and args.kind is None
        and not args.require
        and not args.forbid
        and not args.list
    ):
        print(
            "nothing asserted: pass at least one of --machine/--kind/--require/--forbid/--list",
            file=sys.stderr,
        )
        return 2

    try:
        with open(args.file, "rb") as fh:
            pe = PEFile(fh.read())
    except NotPE as exc:
        print(f"  {args.file}: {exc}", file=sys.stderr)
        return 1
    except OSError as exc:
        print(f"cannot read {args.file}: {exc}", file=sys.stderr)
        return 2

    return 0 if check(args, pe) else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
