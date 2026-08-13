#!/usr/bin/env python3
"""Assert that a Windows containerd.exe carries a given diff-service default order.

WHY THIS EXISTS. `ctr plugins ls` showing the erofs differ as `ok` says only that
the plugin initialised. It says nothing about whether the diff service will ever
ask it anything -- that is decided by
`[plugins.'io.containerd.service.v1.diff-service'].default`, whose Windows value
upstream is ['windows', 'windows-lcow']. The erofs differ was `ok` and unreachable
for exactly that reason, and nothing in a `go build` or a `strings` grep can tell
the two apart: 'erofs', 'windows' and 'windows-lcow' are all in an unpatched
binary anyway, as unrelated strings.

So this reads the value out of the PE instead. A Go []string is a pointer to N
consecutive 16-byte (data pointer, length) headers, and a composite literal like

    var defaultDifferConfig = &config{Order: []string{"erofs", "windows", ...}}

is laid down statically at link time. Scanning .data/.rdata for N such headers
that dereference to exactly the wanted words, in order, finds it in a stripped
binary with no symbol table -- which is what we ship.

This is still only static evidence. It proves the daemon will start with that
order; it does not prove any differ then does anything useful.

Usage: assert-diff-order.py <containerd.exe> erofs windows windows-lcow
Exit 0 if found, 1 if not.
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
    imagebase = struct.unpack_from("<Q", data, opt + 24)[0]
    secs, off = [], opt + optsz
    for _ in range(nsec):
        name = data[off : off + 8].rstrip(b"\0").decode()
        vsize, va, rawsize, rawptr = struct.unpack_from("<IIII", data, off + 8)
        secs.append((name, va, vsize, rawptr, rawsize))
        off += 40
    return imagebase, secs


def main(argv):
    if len(argv) < 3:
        raise SystemExit(__doc__)
    path, words = argv[1], [w.encode() for w in argv[2:]]
    data = open(path, "rb").read()
    imagebase, secs = sections(data)

    def read(va, n):
        rva = va - imagebase
        for _, sva, vsize, rawptr, rawsize in secs:
            if rawsize and sva <= rva < sva + min(vsize or rawsize, rawsize):
                return data[rawptr + (rva - sva) :][:n]
        return None

    for name, sva, _vsize, rawptr, rawsize in secs:
        if name not in (".data", ".rdata"):
            continue
        # 8-byte stride: Go aligns static string headers to the word.
        for off in range(rawptr, rawptr + rawsize - 16 * len(words), 8):
            for i, want in enumerate(words):
                ptr, ln = struct.unpack_from("<QQ", data, off + i * 16)
                if ln != len(want) or ptr < imagebase or read(ptr, ln) != want:
                    break
            else:
                va = imagebase + sva + (off - rawptr)
                print(f"  {path}: {[w.decode() for w in words]} at 0x{va:x} in {name}")
                return 0

    print(f"  {path}: NOT FOUND as a contiguous []string: {[w.decode() for w in words]}", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
