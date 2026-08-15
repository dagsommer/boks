#!/usr/bin/env python3
"""Assert that named Go functions are compiled into, and reachable in, a Windows PE.

WHY THIS EXISTS. patches/0006 replaces one `os.Symlink` call with a junction on
Windows. There is no configuration value to read back out of the binary the way
assert-diff-order.py reads the diff order, and no static table the way
assert-mkfs-magic.py reads the superblock probes: the patch is code, and the only
thing a Linux runner can check about code it cannot run is that it is *there*.

`strings | grep` cannot answer that, and answering it badly is worse than not
asking. Go concatenates every string literal into one enormous run of printable
bytes, so a literal is almost never a line of `strings` output and `grep -Fx`
finds nothing; which pushes you to a substring match, which is how you write a
check that passes on a binary without the patch. Measured, on an unpatched
containerd v2.3.3 built from the same tree:

    strings -a containerd.exe | grep -c Reparse   ->  22
    strings -a containerd.exe | grep -c WorkDir   ->   4   (cleanupWorkDirs, ...)

Neither number is zero, and a `grep -q` on either would have been decoration.

So this reads the pclntab function-name table instead. Two properties make it
the right table:

  * It survives `-ldflags "-s -w"`, which is what we ship. `-s` drops the symbol
    table and `-w` the DWARF, but the runtime needs function names for
    tracebacks and profiles, so they stay -- as fully qualified,
    NUL-terminated strings.
  * The linker's dead-code pass removes unreachable functions *and their names*.
    So a name being present means the function is reachable from main, not
    merely that the source file was patched in. Verified by building a Windows
    PE with one called and one uncalled function: the called one's name is in
    the binary and the uncalled one's is absent entirely.

Matching is on b"\\0" + name + b"\\0", not on the name alone, and that is not
pedantry. In the patched binary, a bare substring search for
`...core/runtime/v2.createWorkDirJunction` matches three times -- the function
and its two closures, `.func1` and `.func2` -- while the NUL-delimited form
matches exactly once. Same distinction, one layer down, as the one that makes
the `grep` above useless.

WHAT THIS DOES NOT PROVE. That the code runs; that Windows accepts the reparse
point; that `os.Readlink` reads it back. It proves the daemon carries a junction
path and can reach it. `go test ./core/runtime/v2/ -run WorkDirJunction` checks
the bytes that path writes, on Linux, against the documented layout. Only a
Windows machine proves the rest.

Usage: assert-func-linked.py <containerd.exe> <fully.qualified.Func> [...]
       assert-func-linked.py --absent <containerd.exe> <fully.qualified.Func> [...]

`--absent` inverts it: every name must be missing. That is the negative control,
and it is meant to be run against a build of the same tree without the patches.
A check that cannot fail is not a check, and this is how we know this one can.

Exit 0 if every name is present (or, with --absent, every name is missing).
"""

import struct
import sys


def sections(data):
    pe = struct.unpack_from("<I", data, 0x3C)[0]
    if data[pe : pe + 4] != b"PE\0\0":
        raise SystemExit("not a PE image")
    nsec = struct.unpack_from("<H", data, pe + 6)[0]
    optsz = struct.unpack_from("<H", data, pe + 20)[0]
    opt = pe + 24
    if struct.unpack_from("<H", data, opt)[0] != 0x20B:
        raise SystemExit("not a PE32+ image")
    secs, off = [], opt + optsz
    for _ in range(nsec):
        name = data[off : off + 8].rstrip(b"\0").decode()
        vsize, va, rawsize, rawptr = struct.unpack_from("<IIII", data, off + 8)
        secs.append((name, va, vsize, rawptr, rawsize))
        off += 40
    return secs


def section_of(secs, offset):
    for name, _va, _vsize, rawptr, rawsize in secs:
        if rawsize and rawptr <= offset < rawptr + rawsize:
            return name
    return "?"


def main(argv):
    argv = argv[1:]
    absent = False
    if argv and argv[0] == "--absent":
        absent, argv = True, argv[1:]
    if len(argv) < 2:
        raise SystemExit(__doc__)
    path, names = argv[0], argv[1:]

    data = open(path, "rb").read()
    secs = sections(data)

    failed = False
    for name in names:
        needle = b"\0" + name.encode() + b"\0"
        found = data.find(needle)
        if absent:
            if found >= 0:
                print(
                    f"  {path}: {name} IS PRESENT, and the negative control "
                    f"requires it not to be (0x{found + 1:x})",
                    file=sys.stderr,
                )
                failed = True
            else:
                print(f"  {path}: absent, as the control requires: {name}")
            continue
        if found < 0:
            bare = data.count(name.encode())
            hint = "" if bare == 0 else f" (it appears {bare}x as a substring -- suffixed closures, most likely)"
            print(
                f"  {path}: NOT FOUND in the function-name table: {name}{hint}",
                file=sys.stderr,
            )
            failed = True
        else:
            print(
                f"  {path}: {name} at 0x{found + 1:x} in {section_of(secs, found + 1)}"
            )
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
