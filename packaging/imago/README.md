# The imago VMDK extent-lookup bug

**Status: fixed upstream, and Boks takes the released fix.** imago
[`f7e1406`](https://gitlab.com/hreitz/imago) — "vmdk: fix get_extent_at binary search at
extent boundaries", Derek McGowan, 2026-06-06 — landed the same one-character change with the
same reasoning, and it shipped in **0.2.4** on 2026-07-17. libkrun's manifest asks for
`imago = "0.2.3"`, which semver-permits 0.2.4; it is the `Cargo.lock` at the revision Boks
pins that holds it back, so `.github/workflows/libkrun-windows.yml` moves the lock and nothing
else. Boks carries no patch.

This file is kept because the analysis below was done independently, from the guest inwards,
and the trail is worth having: it says what was ruled out, in what order, and which of the
obvious explanations were wrong. If a packed disk ever misbehaves again, start here.

---


Boks does not depend on [imago](https://crates.io/crates/imago) directly. libkrun does, for
every disk format that is not raw, and on Windows that is how a Boks sandbox reads a
GPT-partitioned VMDK — the single disk nerdbox packs an image's EROFS layers into when there
are more of them than it will give virtio-block devices.

That path did not work, and this is why.

## The defect

`imago-0.2.3/src/vmdk/mod.rs`, `get_extent_at`:

```rust
self.extents
    .binary_search_by(|extent| {
        if extent.disk_range.contains(&offset) {
            cmp::Ordering::Equal
        } else if extent.disk_range.end < offset {   // <-- must be <=
            cmp::Ordering::Less
        } else {
            cmp::Ordering::Greater
        }
    })
```

`disk_range` is a half-open `Range`: the extent covers `[start, end)`. When
`offset == extent.disk_range.end` the extent does not contain the offset and lies entirely
**below** it, so the comparator must say `Less`. It says `Greater` — "look further left" —
which violates `binary_search_by`'s contract. The search can then walk away from the extent
that does cover the offset and return `Err`.

Nothing reports an error. `get_extent_at` maps `Err` to `None`; `get_mapping` turns `None`
into `ShallowMapping::Eof`; and `FormatAccess::readv` does this with an EOF mapping:

```rust
if chunk_length == 0 { assert!(mapping.is_eof()); bufv.fill(0); break; }
```

**Zeros, returned as a successful read.** A guest asking for a filesystem superblock is handed
4 KiB of nothing and told it worked.

## Why it looks arbitrary

Every offset that coincides with an extent boundary is exposed, and on a partitioned flat VMDK
that is every partition start. Whether a particular one survives depends on which extents the
binary search happens to probe first, which depends on the extent count. So the failure lands
on some partitions and not others, with no relation to their size or position:

```
shell (8 layers): 18 extents
  partition 1   offset       1048576  buggy=2    fixed=2    mounts
  partition 2   offset       2097152  buggy=4    fixed=4    mounts
  partition 3   offset       3145728  buggy=6    fixed=6    mounts
  partition 4   offset       4194304  buggy=Err  fixed=8    ZEROS
  …
copilot (17 layers): 36 extents
  partition 3   offset    2138046464  buggy=6    fixed=6    mounts
  partition 4   offset    2315255808  buggy=Err  fixed=8    ZEROS
  …
  partition 9   offset    3894411264  buggy=18   fixed=18   mounts   <-- works again
  …
  partition 13  offset    3898605568  buggy=Err  fixed=26   ZEROS
```

Two unrelated images, one failing at 4 MiB and one at 2.3 GB, both first failing at partition
**4**. That coincidence is what made this hard to find: it looks like an ordinal limit, and it
is not — it is where the search path diverges.

## What it looked like from outside

On Windows, with nerdbox's threshold lowered so an 8-layer image packs:

```
erofs: (device vdc1): mounted with root inode @ nid 36.
erofs: (device vdc2): mounted with root inode @ nid 36.
erofs: (device vdc3): mounted with root inode @ nid 36.
erofs: (device vdc4): erofs_read_superblock: cannot find valid erofs superblock
```

and, four layers up, `failed to create shim task: … fstype: erofs … err: invalid argument`.
No I/O error at any level, because from the VMM's point of view the read succeeded.

Ruled out before finding it, each by measurement rather than reading: nerdbox's layout and
`ComputeLayout`, the GPT header, the descriptor's extent arithmetic, backing-file sizes, 64-bit
offset handling on Windows, and imago's own `read()` on the same descriptor. See
`packaging/nerdbox/README.md` for that trail.

## Reproducing it

Two programs, because they prove different things.

`reproduce/main.rs` builds the two disks' real extent tables and runs both comparators through
the real `slice::binary_search_by`. No VM, no Windows, no imago, no network:

```
rustc -O -o repro reproduce/main.rs && ./repro
```

It shows *why* the failure lands where it does, including the part no count-based theory
predicts: in the 17-layer disk, partitions 9 through 12 work again.

`verify/` proves the same thing end to end through imago itself. It writes a real packed VMDK
of the shape nerdbox produces and reads the first block of every partition, with each layer
carrying its own index beside the magic so that landing on the *wrong* layer is as detectable
as landing on zeros:

```
cd verify && cargo run --release -- /tmp/disk
```

Against imago 0.2.3: `3 of 8 partitions read their own superblock`, with 4 through 8 reporting
ZEROS — the reported failure, reproduced without a hypervisor. Against 0.2.4: `8 of 8`. It
exits non-zero unless every partition reads correctly, and it is a gate in the Windows libkrun
workflow, run before anything is built against the resolved crate.

One thing that version of the test got wrong first, kept here because it is the trap: reading
at `partition_start + 1024` passes against the *unpatched* crate. That offset is inside the
extent, where `contains()` matches and the comparator is never consulted about a boundary. The
guest reads a block starting at the partition's first byte, and only that reproduces it.

## The fix, and how Boks gets it

The change is one character — `<` becomes `<=` — and it is already released, so Boks does not
carry it as a patch. The Windows libkrun workflow runs `cargo update -p imago --precise 0.2.4`
against the pinned libkrun revision, asserts the resolved version is **at least** 0.2.4 rather
than exactly it, and skips entirely when the pin already resolves to something newer, so a
later libkrun revision cannot be silently downgraded by this step.

Verified on Linux against the real crates and the real pin:

- imago 0.2.4 from crates.io: `verify/` reports **8 of 8** partitions, and 0.2.3 reports 3 of 8
  and exits 1.
- `cargo update -p imago` on libkrun at `07fd40d` moves 0.2.3 → 0.2.4 with no manifest change,
  since `"0.2.3"` is a caret requirement.
- 0.2.4 keeps the `sync-wrappers` and `vm-memory` features libkrun asks for, despite the
  `maybe-async` refactor between the two releases, and `krun-devices` type-checks against it
  for `x86_64-pc-windows-msvc` with the Windows patch series applied.

**What is not claimed:** no `krun.dll` has been built against 0.2.4, and no guest has booted
from a packed disk.
