# The imago VMDK extent-lookup bug

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

`reproduce/main.rs` builds the two disks' real extent tables and runs both comparators through
the real `slice::binary_search_by`. No VM, no Windows, no imago:

```
rustc -O -o repro reproduce/main.rs && ./repro
```

It is the evidence for everything above, and it is the thing to re-run against any proposed
fix.

## The fix, and where it has to go

`patches/0001-…` changes `<` to `<=`. It is one character; the comment explaining why is
longer than the change, deliberately.

imago is a crates.io dependency of libkrun, not a vendored source, so the patch cannot simply
be applied to a checkout the way `packaging/libkrun-windows/patches/` are. Options, in the
order they should be preferred:

1. **Upstream.** This is a plain bug in a released crate and affects every VMDK with more than
   a couple of extents. It should be reported and fixed there, and libkrun should take the
   bumped version. Everything needed for a report is in this directory.
2. **A `[patch.crates-io]` override in libkrun's build**, pointing at a copy of the crate with
   this patch applied. That is what a Boks build would carry until the above lands.

**Not yet done, and not claimed:** no Boks build applies this today. The analysis is confirmed
by execution, and the fix follows from it directly, but a `krun.dll` built with the patch has
not been produced or booted.
