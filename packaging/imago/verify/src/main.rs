//! Read every partition of a nerdbox-shaped packed disk through imago, the way libkrun does.
//!
//! Builds a real GPT-style flat VMDK -- a header extent, then one ZERO pad and one FLAT extent
//! per layer, aligned to 1 MiB, exactly as `nerdbox internal/erofs` writes it -- and asks imago
//! for the first block of each partition. Each layer file carries the EROFS superblock magic at
//! offset 1024 and its own index beside it, so a read landing on the wrong layer is as
//! detectable as one landing on zeros.
//!
//! Against a stock imago 0.2.3 this reports partitions 1-3 as ok and the rest as ZEROS, which is
//! the failure as it was reported from Windows. Against a patched one, all of them read their
//! own superblock. Exit status is 0 only if every partition does.
//!
//!     cargo run --release -- /tmp/somewhere
//!
//! Eight 4 KiB layers rather than the reported disk's 660 MB: the defect depends on the extent
//! LAYOUT, and this is the same 18-extent shape at 1/80000th the bytes.

use imago::file::File as ImagoFile;
use imago::vmdk::Vmdk;
use imago::{DynStorage, FormatDriverBuilder, PermissiveImplicitOpenGate, Storage, StorageOpenOptions, SyncFormatAccess};
use std::fs;
use std::io::Write;
use std::sync::Arc;

const SEC: u64 = 512;
const ALIGN: u64 = 2048;
const RESERVED: u64 = 34;
const MAGIC: [u8; 4] = [0xe2, 0xe1, 0xf5, 0xe0]; // EROFS_SUPER_MAGIC_V1, LE

fn align_up(v: u64, a: u64) -> u64 { v.div_ceil(a) * a }

fn main() -> std::io::Result<()> {
    let dir = std::env::args().nth(1).expect("usage: vmdk-e2e <dir>");
    let _ = fs::remove_dir_all(&dir);
    fs::create_dir_all(&dir)?;

    // Eight 4 KiB layers. The defect depends on the extent LAYOUT, not on sizes: this is the
    // same 18-extent shape as the reported disk, at 1/80000th the bytes.
    let layers: [u64; 8] = [8, 8, 8, 8, 8, 8, 8, 8];

    // Header blob: 34 sectors. Content is irrelevant here -- imago never parses the GPT, it
    // only maps extents -- but the extent must exist and be the right length.
    let header = format!("{dir}/header.bin");
    fs::write(&header, vec![0u8; (RESERVED * SEC) as usize])?;

    let mut desc = String::from(
        "# Disk DescriptorFile\nversion=1\nCID=fffffffe\nparentCID=ffffffff\n\
         createType=\"twoGbMaxExtentFlat\"\n\n# Extent description\n",
    );
    desc.push_str(&format!("RW {RESERVED} FLAT \"{header}\" 0\n"));

    let mut cursor = RESERVED;
    let mut next = ALIGN;
    let mut starts = Vec::new();
    for (i, &len) in layers.iter().enumerate() {
        // Each layer file carries the magic at 1024, then its own index everywhere after, so a
        // read landing on the WRONG layer is as detectable as one landing on zeros.
        let path = format!("{dir}/layer{i}.bin");
        let mut buf = vec![(i as u8) + 1; (len * SEC) as usize];
        buf[1024..1028].copy_from_slice(&MAGIC);
        buf[1028] = i as u8;
        fs::File::create(&path)?.write_all(&buf)?;

        if next > cursor { desc.push_str(&format!("RW {} ZERO\n", next - cursor)); }
        desc.push_str(&format!("RW {len} FLAT \"{path}\" 0\n"));
        starts.push(next * SEC);
        cursor = next + len;
        next = align_up(cursor, ALIGN);
    }
    if next > cursor { desc.push_str(&format!("RW {} ZERO\n", next - cursor)); }
    desc.push_str("\n\n# The Disk Data Base\n#DDB\n\nddb.virtualHWVersion = \"4\"\n");

    let vmdk_path = format!("{dir}/merged_fs_gpt.vmdk");
    fs::write(&vmdk_path, desc)?;

    // Opened exactly as libkrun's block/device.rs does it.
    let file = ImagoFile::open_sync(StorageOpenOptions::new().write(false).filename(&vmdk_path))?;
    let vmdk = Vmdk::<Box<dyn DynStorage>, Arc<imago::FormatAccess<_>>>::builder(Box::new(file))
        .open_sync(PermissiveImplicitOpenGate::default())?;
    let disk = SyncFormatAccess::new(vmdk)?;

    let mut bad = 0;
    for (i, start) in starts.iter().enumerate() {
        // Read the way the guest does: one 4 KiB block STARTING at the partition's first
        // byte, with the superblock 1024 in. Reading at start+1024 instead would land inside
        // the extent, where `contains()` matches and the defect never shows -- which is how
        // the first version of this test passed against the unpatched crate.
        let mut buf = vec![0u8; 4096];
        disk.read(&mut buf[..], *start)?;
        let sb = &buf[1024..1029];
        let ok = sb[..4] == MAGIC && sb[4] == i as u8;
        let what = if sb[..4] == [0, 0, 0, 0] {
            "ZEROS (mapping ended early)".to_string()
        } else if sb[..4] != MAGIC {
            format!("garbage {:02x?}", &sb[..4])
        } else if sb[4] != i as u8 {
            format!("WRONG LAYER (read layer {})", sb[4])
        } else {
            "ok".to_string()
        };
        println!("  partition {:<3} offset {:>10}  {}", i + 1, start, what);
        if !ok { bad += 1; }
    }
    println!("\n{} of {} partitions read their own superblock", starts.len() - bad, starts.len());
    std::process::exit(if bad == 0 { 0 } else { 1 });
}
