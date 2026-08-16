# `packaging/windows/`

Assertions about Windows artifacts that more than one workflow needs, so they live here
rather than inside any single recipe. The Linux counterpart is
[`packaging/linux/assert-elf.py`](../linux/assert-elf.py), which is shared the same way
between `linux-runtime.yml` and `packaging/containerd-linux/build.sh`.

| File | What it does |
|---|---|
| `assert-pe.py` | reads a PE image's COFF `Machine` field, its `IMAGE_FILE_DLL` bit and its export directory, and asserts what the caller names |

## Who calls it

| Caller | Artifact | Assertion |
|---|---|---|
| `.github/workflows/libkrun-windows.yml` | `krun.dll` | x86-64, a DLL, and the twenty `krun_*` symbols nerdbox binds |
| `.github/workflows/nerdbox-windows.yml` | `containerd-shim-nerdbox-v1-{amd64,arm64}.exe` | x86-64 / arm64, and an executable rather than a DLL |

Both callers follow the assertion with a **negative-control step** that requires the checker
to reject things it must reject — a strict prefix of a real export, a name that is imported
rather than exported, the wrong architecture, and a file that is not a PE image at all.
`linux-runtime.yml` does the same for `assert-elf.py`, for the same reason: an assertion with
no demonstrated failure mode is indistinguishable from no assertion.

## Why it is a script and not three lines of shell

Until 2026-08-16 `libkrun-windows.yml` checked `krun.dll`'s exports like this:

```sh
tr -c '[:print:]' '\n' <"$dll" | sort -u >/tmp/krun-dll-strings.txt
grep -qxF krun_create_ctx /tmp/krun-dll-strings.txt
```

That is every run of printable bytes anywhere in the file. It cannot tell an export from an
import, from an `.rdata` literal, or from a debug string, and it does not require the file to
be a PE image at all. **A 369-byte file containing nothing but the twenty names as
NUL-terminated ASCII passed all twenty assertions and exited 0** — measured, not supposed, and
that same fabrication is now rebuilt inside the workflow as a control.

`krun.dll` was also the only shipped binary with no format or architecture assertion of any
kind: `mkfs.erofs.exe` had `objdump -f | grep pei-x86-64`, `libkrun.so` had `assert-elf.py`,
and the shim had a two-byte `MZ` check — which is the *same* two bytes for amd64 and arm64,
so a build loop with a hardcoded `GOARCH` shipped a mislabelled arm64 shim green.

`objdump` is deliberately not used. It needs binutils built with PE support, which the Linux
runners do not reliably have (`objdump` there reports `file format not recognized` for a PE),
and it has no name for `windows/arm64` in the form these workflows would grep for.

## What a pass does not mean

That the DLL loads, or that anything in it works. A forwarder export resolves to a DLL that
may not be installed; an exported function can return `-ENOSYS`. This is the *load-time*
contract: nerdbox's loader (`internal/vm/libkrun/krun.go`) reflects over its whole binding
table and resolves every `C:"krun_*"` tag eagerly, so one missing name fails the load and no
VM starts at all. That is what this catches, and only that.

## Exit status is part of the contract

```
0   every requested assertion held
1   the file was read and understood, and an assertion FAILED
2   the file could not be read, or the arguments made no sense
```

Controls must demand **exactly 1**. A control that accepts any non-zero status treats a
mistyped path (2) as a successful rejection (1), which is how a negative control quietly
stops testing anything.
