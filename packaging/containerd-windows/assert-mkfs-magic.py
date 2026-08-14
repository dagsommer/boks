#!/usr/bin/env python3
"""Assert that a Windows containerd.exe carries the mkfs superblock probe table.

WHY THIS EXISTS. `core/mount/manager/mkfs.go` used to treat "the image file
exists" as "the image file is formatted", with a bare `// Check magic number`
comment where the check should have been. The format step creates the file and
truncates it to size *before* running mkfs, so a failed mkfs -- guaranteed on
Windows, where no mkfs.ext4 exists -- left behind a file of exactly the right
size, full of zeroes, that the next attempt accepted on sight and handed to the
guest as ext4. patches/0005 replaces the comment with a real superblock read.

Whether that check is present is not something `strings` can answer. The bytes
that matter are a two-byte magic and a numeric offset, both of which occur by
coincidence all over a 37 MB binary, and the error strings alone would only
prove some text was linked in, not that the table says the right thing.

So this reads the table itself. `superblockProbes` is a package-level

    var superblockProbes = []superblockProbe{
        {fs: "ext4", offset: 1080, magic: "\\x53\\xef"},
        ...
    }

whose elements are all constants, so Go lays the backing array down statically
at link time: consecutive 40-byte records of (string header, int64, string
header) -- 16 + 8 + 16, in declaration order, since Go does not reorder struct
fields. Scanning .data/.rdata for N consecutive records that dereference to
exactly the wanted (fs, offset, magic) triples finds it in a stripped binary
with no symbol table, which is what we ship. Same trick, and same limits, as
assert-diff-order.py next door.

This is still only static evidence. It proves the daemon carries a table
saying ext4's magic is 53 ef at offset 1080; it does not prove the check runs,
still less that a real image is ever rejected. `go test ./core/mount/manager/`
is what proves that, and it runs on Linux against the same platform-neutral
code Windows runs.

Usage: assert-mkfs-magic.py <containerd.exe> ext4:1080:53ef xfs:0:58465342
       (each probe is fs:offset:hex-bytes, in on-disk byte order, in the
        order they are declared)
Exit 0 if found, 1 if not.
"""

import struct
import sys

# (string header, int64, string header) -- see the docstring.
PROBE_SIZE = 40


def sections(data):
    pe = struct.unpack_from("<I", data, 0x3C)[0]
    if data[pe : pe + 4] != b"PE\0\0":
        raise SystemExit("not a PE image")
    nsec = struct.unpack_from("<H", data, pe + 6)[0]
    optsz = struct.unpack_from("<H", data, pe + 20)[0]
    opt = pe + 24
    if struct.unpack_from("<H", data, opt)[0] != 0x20B:
        raise SystemExit("not a PE32+ image")
    imagebase = struct.unpack_from("<Q", data, opt + 24)[0]
    secs, off = [], opt + optsz
    for _ in range(nsec):
        name = data[off : off + 8].rstrip(b"\0").decode()
        vsize, va, rawsize, rawptr = struct.unpack_from("<IIII", data, off + 8)
        secs.append((name, va, vsize, rawptr, rawsize))
        off += 40
    return imagebase, secs


def parse_probe(spec):
    try:
        fs, offset, magic = spec.split(":")
        return fs.encode(), int(offset), bytes.fromhex(magic)
    except ValueError:
        raise SystemExit(f"bad probe {spec!r}, want fs:offset:hexbytes")


def main(argv):
    if len(argv) < 3:
        raise SystemExit(__doc__)
    path = argv[1]
    probes = [parse_probe(a) for a in argv[2:]]
    data = open(path, "rb").read()
    imagebase, secs = sections(data)

    def read(va, n):
        rva = va - imagebase
        for _, sva, vsize, rawptr, rawsize in secs:
            if rawsize and sva <= rva < sva + min(vsize or rawsize, rawsize):
                return data[rawptr + (rva - sva) :][:n]
        return None

    def matches(off, want):
        fs, offset, magic = want
        fsptr, fslen = struct.unpack_from("<QQ", data, off)
        if fslen != len(fs) or fsptr < imagebase or read(fsptr, fslen) != fs:
            return False
        if struct.unpack_from("<q", data, off + 16)[0] != offset:
            return False
        mptr, mlen = struct.unpack_from("<QQ", data, off + 24)
        return mlen == len(magic) and mptr >= imagebase and read(mptr, mlen) == magic

    for name, sva, _vsize, rawptr, rawsize in secs:
        if name not in (".data", ".rdata"):
            continue
        # 8-byte stride: Go aligns static data to the word.
        for off in range(rawptr, rawptr + rawsize - PROBE_SIZE * len(probes), 8):
            if all(matches(off + i * PROBE_SIZE, p) for i, p in enumerate(probes)):
                va = imagebase + sva + (off - rawptr)
                pretty = ", ".join(
                    f"{fs.decode()}@{o}={m.hex()}" for fs, o, m in probes
                )
                print(f"  {path}: superblock probes [{pretty}] at 0x{va:x} in {name}")
                return 0

    pretty = ", ".join(f"{fs.decode()}@{o}={m.hex()}" for fs, o, m in probes)
    print(
        f"  {path}: NOT FOUND as a contiguous []superblockProbe: [{pretty}]",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
