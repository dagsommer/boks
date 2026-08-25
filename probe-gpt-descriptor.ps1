<#
.SYNOPSIS
    Follow a failing packed disk's VMDK descriptor by hand and see what each partition's
    superblock actually resolves to.

.DESCRIPTION
    The guest said what it thinks:

        erofs: (device vdc4): erofs_read_superblock: cannot find valid erofs superblock

    Partitions 1-3 mount; the 4th reads bytes that are not a superblock, and the guest gets no
    I/O error at all. That matches one specific behaviour in imago's readv: reaching a mapping's
    end fills the rest of the buffer with ZEROS and returns success. So the question is where the
    disk's mapping stops being what the shim intended.

    This reads the descriptor the FAILING RUN actually used -- not a regenerated one -- and
    resolves every partition through the extent list with no imago, no Rust, and no VM. It
    answers one question: is the descriptor for this disk semantically correct?

        correct  -> the descriptor is exonerated and the fault is in the VMM at runtime
        wrong    -> we can see exactly which partition drifts and by how much

    It also prints the GPT header fields the guest's own partition scanner reads, because if
    PartitionEntryLBA or SizeOfPartitionEntry disagree with where the entries really are, Linux
    computes different partition offsets than the shim intended -- which would look exactly like
    this.

    Nothing is written or deleted anywhere. Read-only.

.EXAMPLE
    .\probe-gpt-descriptor.ps1

.NOTES
    The descriptor exists for about a second. The shim writes it into the runtime bundle before
    the VM starts, and its own cleanup removes the bundle when task creation fails -- which is
    every run that reproduces this. Keeping the sandbox does not help: the bundle belongs to
    containerd, not to the sandbox. probe-packed-layers.ps1 -Reproduce copies it out while it
    is there and then calls this script with -Descriptor and -FallbackDir, so running that is
    the way to get here.
#>
[CmdletBinding()]
param(
    [string] $StateDir = "$env:LOCALAPPDATA\boks",
    [string] $Descriptor,
    [string] $FallbackDir,
    [string] $OutFile = "$env:TEMP\boks-gpt-descriptor.txt"
)

Set-StrictMode -Version Latest

"" | Set-Content -LiteralPath $OutFile
function Say {
    param([string] $Text = "")
    Write-Host $Text
    Add-Content -LiteralPath $OutFile -Value $Text
}
function Section {
    param([string] $Title)
    Say ""
    Say ("=" * 78)
    Say $Title
    Say ("=" * 78)
}

# A captured descriptor names its header blob by a bundle path that the shim's cleanup has
# already removed. The copy sits in the capture directory under the same name, so a missing
# file is looked for there before being called missing. Layer files are unaffected: they live
# in the snapshotter's root and outlive the bundle.
function Resolve-File {
    param([string] $Path)
    if ([string]::IsNullOrWhiteSpace($Path)) { return $Path }
    if (Test-Path -LiteralPath $Path) { return $Path }
    if (-not [string]::IsNullOrWhiteSpace($FallbackDir)) {
        $alt = Join-Path $FallbackDir (Split-Path $Path -Leaf)
        if (Test-Path -LiteralPath $alt) { return $alt }
    }
    return $Path
}

Section "FINDING THE DESCRIPTOR"

if ([string]::IsNullOrWhiteSpace($Descriptor)) {
    $bundles = Join-Path $StateDir "containerd\state\io.containerd.runtime.v2.task"
    Say "searching $bundles"
    $found = Get-ChildItem -LiteralPath $bundles -Recurse -Filter "merged_fs_gpt.vmdk" -ErrorAction SilentlyContinue |
             Sort-Object LastWriteTime -Descending
    if ($null -eq $found -or $found.Count -eq 0) {
        Say ""
        Say "No merged_fs_gpt.vmdk anywhere under the runtime state."
        Say "The bundle was cleaned up. Re-run the failing case WITHOUT --rm, then run this again."
        return
    }
    foreach ($f in $found) { Say "  $($f.LastWriteTime)  $($f.FullName)" }
    $Descriptor = $found[0].FullName
    Say ""
    Say "using the newest: $Descriptor"
}
if (-not (Test-Path -LiteralPath $Descriptor)) {
    Say "No such file: $Descriptor"
    return
}

Section "DESCRIPTOR, VERBATIM"
Get-Content -LiteralPath $Descriptor | ForEach-Object { Say $_ }

Section "EXTENT MAP"

# Two line shapes are produced by nerdbox: a FLAT extent naming a backing file and a byte
# offset, and a ZERO extent that occupies sectors while reading as zeros.
#   RW <sectors> FLAT "<path>" <offset-in-sectors>
#   RW <sectors> ZERO
$extents = @()
$cursor  = [uint64]0
foreach ($line in (Get-Content -LiteralPath $Descriptor)) {
    $m = [regex]::Match($line, '^RW\s+(\d+)\s+(FLAT|ZERO)(?:\s+"(.+)"\s+(\d+))?\s*$')
    if (-not $m.Success) { continue }
    $count = [uint64]$m.Groups[1].Value
    $kind  = $m.Groups[2].Value
    $file  = if ($m.Groups[3].Success) { $m.Groups[3].Value } else { "" }
    $off   = if ($m.Groups[4].Success) { [uint64]$m.Groups[4].Value } else { [uint64]0 }
    $extents += [pscustomobject]@{
        Index      = $extents.Count
        StartLBA   = $cursor
        Sectors    = $count
        Kind       = $kind
        File       = (Resolve-File $file)
        FileOffLBA = $off
    }
    $cursor += $count
}

Say "extents: $($extents.Count)   total sectors: $cursor   ($($cursor * 512) bytes)"
Say ""
Say ("{0,-5} {1,-12} {2,-10} {3,-5} {4}" -f "idx", "startLBA", "sectors", "kind", "file")
foreach ($e in $extents) {
    $name = if ($e.File -eq "") { "-" } else { Split-Path $e.File -Leaf }
    Say ("{0,-5} {1,-12} {2,-10} {3,-5} {4}" -f $e.Index, $e.StartLBA, $e.Sectors, $e.Kind, $name)
}

# Resolve a virtual disk sector to a real file and byte offset, following the extent list the
# way anything reading this disk has to.
function Resolve-Sector {
    param([uint64] $Lba)
    foreach ($e in $script:extents) {
        if ($Lba -ge $e.StartLBA -and $Lba -lt ($e.StartLBA + $e.Sectors)) {
            $within = $Lba - $e.StartLBA
            return [pscustomobject]@{
                Extent = $e.Index
                Kind   = $e.Kind
                File   = $e.File
                Byte   = ($e.FileOffLBA + $within) * 512
            }
        }
    }
    return $null
}

Section "GPT HEADER, AS THE GUEST'S PARTITION SCANNER READS IT"

# The header blob is the first extent: LBA0 protective MBR, LBA1 the GPT header, LBA2..33 the
# entry array.
$headerExtent = $extents | Where-Object { $_.Kind -eq "FLAT" } | Select-Object -First 1
if ($null -eq $headerExtent) { Say "no FLAT extent found - descriptor is not what this expects"; return }
$headerFile = $headerExtent.File
Say "header blob: $headerFile"
if (-not (Test-Path -LiteralPath $headerFile)) { Say "  MISSING - this alone would explain everything"; return }

$hdr = [System.IO.File]::ReadAllBytes($headerFile)
Say "  size: $($hdr.Length) bytes (expected 17408 = 34 sectors)"

$sig = [System.Text.Encoding]::ASCII.GetString($hdr, 512, 8)
Say "  signature at LBA1: '$sig' (expected 'EFI PART')"
Say "  MyLBA              : $([BitConverter]::ToUInt64($hdr, 512 + 24))"
Say "  FirstUsableLBA     : $([BitConverter]::ToUInt64($hdr, 512 + 40))"
Say "  LastUsableLBA      : $([BitConverter]::ToUInt64($hdr, 512 + 48))"
Say "  PartitionEntryLBA  : $([BitConverter]::ToUInt64($hdr, 512 + 72))   (expected 2)"
Say "  NumberOfEntries    : $([BitConverter]::ToUInt32($hdr, 512 + 80))   (expected 128)"
Say "  SizeOfEntry        : $([BitConverter]::ToUInt32($hdr, 512 + 84))   (expected 128)"

Section "PARTITIONS - what each superblock resolves to"

$entrySize  = [BitConverter]::ToUInt32($hdr, 512 + 84)
$entryCount = [BitConverter]::ToUInt32($hdr, 512 + 80)
if ($entrySize -eq 0) { $entrySize = 128 }
if ($entryCount -eq 0 -or $entryCount -gt 128) { $entryCount = 128 }

Say ("{0,-5} {1,-12} {2,-12} {3,-10} {4,-9} {5,-12} {6}" -f "part", "firstLBA", "lastLBA", "sectors", "extent", "magic", "verdict")

for ($i = 0; $i -lt $entryCount; $i++) {
    $base = 1024 + ($i * $entrySize)
    if (($base + $entrySize) -gt $hdr.Length) { break }

    # An unused entry is an all-zero type GUID.
    $used = $false
    for ($b = 0; $b -lt 16; $b++) { if ($hdr[$base + $b] -ne 0) { $used = $true; break } }
    if (-not $used) { continue }

    $first = [BitConverter]::ToUInt64($hdr, $base + 32)
    $last  = [BitConverter]::ToUInt64($hdr, $base + 40)
    $res   = Resolve-Sector -Lba $first

    if ($null -eq $res) {
        Say ("{0,-5} {1,-12} {2,-12} {3,-10} {4}" -f ($i + 1), $first, $last, ($last - $first + 1), "UNMAPPED - past the end of the extent list")
        continue
    }
    if ($res.Kind -ne "FLAT") {
        Say ("{0,-5} {1,-12} {2,-12} {3,-10} {4,-9} {5}" -f ($i + 1), $first, $last, ($last - $first + 1), $res.Extent, "lands in a ZERO extent")
        continue
    }

    # The EROFS superblock lives 1024 bytes into the partition; its magic is 0xE0F5E1E2 LE.
    $magic   = "??"
    $verdict = "unreadable"
    if (Test-Path -LiteralPath $res.File) {
        $fs = [System.IO.File]::OpenRead($res.File)
        try {
            $want = $res.Byte + 1024
            if ($want + 4 -le $fs.Length) {
                $null = $fs.Seek($want, [System.IO.SeekOrigin]::Begin)
                $buf = New-Object byte[] 4
                $null = $fs.Read($buf, 0, 4)
                $magic = ($buf | ForEach-Object { $_.ToString("x2") }) -join ""
                if ($magic -eq "e2e1f5e0") { $verdict = "OK" } else { $verdict = "NOT EROFS" }
            } else {
                $verdict = "past end of $(Split-Path $res.File -Leaf) ($($fs.Length) bytes)"
            }
        } finally { $fs.Dispose() }
    } else {
        $verdict = "backing file missing"
    }

    Say ("{0,-5} {1,-12} {2,-12} {3,-10} {4,-9} {5,-12} {6}  {7}" -f `
        ($i + 1), $first, $last, ($last - $first + 1), $res.Extent, $magic, $verdict, (Split-Path $res.File -Leaf))
}

Section "BACKING FILES - declared extent size vs actual file size"

foreach ($e in ($extents | Where-Object { $_.Kind -eq "FLAT" })) {
    $len = if (Test-Path -LiteralPath $e.File) { (Get-Item -LiteralPath $e.File).Length } else { -1 }
    $need = ($e.FileOffLBA + $e.Sectors) * 512
    $note = if ($len -lt 0) { "MISSING" } elseif ($len -lt $need) { "SHORT by $($need - $len) bytes - reads here return zeros" } else { "ok" }
    Say ("extent {0,-4} needs {1,-14} file has {2,-14} {3}  {4}" -f $e.Index, $need, $len, $note, (Split-Path $e.File -Leaf))
}

Say ""
Say "Output saved to $OutFile"
