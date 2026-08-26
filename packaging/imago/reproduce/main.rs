//! The imago VMDK extent-lookup defect, in isolation.
//!
//! Builds the extent tables of two real packed disks -- Boks' own `shell` image and the
//! 17-layer image that found this -- and runs imago's comparator and a corrected one through
//! the real `slice::binary_search_by`. No imago, no network, no VM, no Windows:
//!
//!     rustc -O -o repro main.rs && ./repro
//!
//! `../verify` proves the same thing end to end through imago itself, which is stronger
//! evidence but needs the crate. This one runs anywhere, and it is what shows *why* the
//! failure lands where it does: the search path, not the sizes or the offsets.

use std::cmp::Ordering;
use std::ops::Range;

const SEC: u64 = 512;
const ALIGN: u64 = 2048; // gptAlignSectors, 1 MiB
const RESERVED: u64 = 34; // protective MBR + GPT header + entry array

fn align_up(v: u64, a: u64) -> u64 { v.div_ceil(a) * a }

/// Rebuild exactly what nerdbox writes: a header extent, then for each layer a ZERO pad to the
/// next 1 MiB boundary followed by the layer's FLAT extent, then a trailing pad.
fn layout(layer_sectors: &[u64]) -> (Vec<Range<u64>>, Vec<u64>) {
    let mut extents: Vec<Range<u64>> = Vec::new();
    let mut part_starts: Vec<u64> = Vec::new();
    let mut cursor = RESERVED;
    extents.push(0..RESERVED * SEC);
    let mut next = ALIGN;
    for &len in layer_sectors {
        if next > cursor { extents.push(cursor * SEC..next * SEC); }
        extents.push(next * SEC..(next + len) * SEC);
        part_starts.push(next * SEC);
        cursor = next + len;
        next = align_up(cursor, ALIGN);
    }
    if next > cursor { extents.push(cursor * SEC..next * SEC); }
    (extents, part_starts)
}

/// imago 0.2.3, src/vmdk/mod.rs, get_extent_at.
fn find_buggy(extents: &[Range<u64>], offset: u64) -> Option<usize> {
    extents.binary_search_by(|e| {
        if e.contains(&offset) { Ordering::Equal }
        else if e.end < offset { Ordering::Less }
        else { Ordering::Greater }
    }).ok()
}

/// The same, with the comparator telling the truth about an extent that ends exactly at offset.
fn find_fixed(extents: &[Range<u64>], offset: u64) -> Option<usize> {
    extents.binary_search_by(|e| {
        if e.contains(&offset) { Ordering::Equal }
        else if e.end <= offset { Ordering::Less }
        else { Ordering::Greater }
    }).ok()
}

fn report(name: &str, layers: &[u64]) {
    let (extents, starts) = layout(layers);
    println!("\n{name}: {} layers, {} extents, {} sectors total",
             layers.len(), extents.len(), extents.last().unwrap().end / SEC);
    for (i, start) in starts.iter().enumerate() {
        let buggy = find_buggy(&extents, *start);
        let fixed = find_fixed(&extents, *start);
        let verdict = match buggy {
            Some(_) => "mounts",
            None => "ZEROS  <-- cannot find valid erofs superblock",
        };
        println!("  partition {:<3} offset {:>13}  buggy={:<9} fixed={:<9} {}",
                 i + 1, start,
                 buggy.map(|v| v.to_string()).unwrap_or("Err".into()),
                 fixed.map(|v| v.to_string()).unwrap_or("Err".into()),
                 verdict);
    }
}

fn main() {
    // Boks' own shell image, from the captured descriptor.
    report("shell (8 layers)", &[8, 8, 40, 464, 112, 283568, 878464, 167968]);
    report("tiny (8 layers, 4 KiB each)", &[8, 8, 8, 8, 8, 8, 8, 8]);
    // The 17-layer image this investigation started from.
    report("copilot (17 layers)", &[205144, 3966728, 345616, 415528, 1949752, 598968,
                                        12224, 103168, 16, 16, 16, 8, 112, 143904, 8, 16, 219952]);
}
