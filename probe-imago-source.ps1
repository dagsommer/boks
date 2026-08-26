<#
.SYNOPSIS
    Dump the parts of imago that decide what a VMDK's extents map to.

.DESCRIPTION
    Where the investigation stands. The packed disk's descriptor is correct — every partition
    resolves through the extent list to its own layer's EROFS superblock, sizes match, the GPT
    header is right. The guest still reads non-superblock bytes at partition 4, and gets no I/O
    error, which points at one behaviour in imago's readv:

        if chunk_length == 0 { assert!(mapping.is_eof()); bufv.fill(0); break; }

    Reaching the end of a mapping fills the buffer with ZEROS and returns success. So the
    question is why the mapping ends early.

    The lead: libkrun opens the descriptor with

        Vmdk::builder(file).open_sync(PermissiveImplicitOpenGate::default())

    and every extent file is an implicit dependency opened through that gate. In the failing
    disk the opens are, in order: merged_fs_gpt_header.bin, then the layers behind partitions
    1, 2, 3 — and the layer behind partition 4 is the fifth. Partition 4 is the first that
    fails, in both images tested, at wildly different sizes and offsets. A gate that stops
    opening dependencies after a few would produce exactly this.

    This script does not test that. It collects the source needed to confirm or kill it, since
    crates.io is not reachable from where the analysis is happening.

    Read-only. Touches nothing but the cargo registry, and only to read.

.EXAMPLE
    .\probe-imago-source.ps1

.NOTES
    Output goes to one file. Open it in an editor and paste it back — the terminal will mangle
    Rust source, and the whole point is to read the code exactly as written.
#>
[CmdletBinding()]
param(
    [string] $Crate,
    [string] $OutFile = "$env:TEMP\imago-source.txt"
)

Set-StrictMode -Version Latest

if ([string]::IsNullOrWhiteSpace($Crate)) {
    $registry = Join-Path $env:USERPROFILE ".cargo\registry\src"
    $found = Get-ChildItem -LiteralPath $registry -Directory -Recurse -Depth 2 -ErrorAction SilentlyContinue |
             Where-Object { $_.Name -like "imago-*" } |
             Select-Object -First 1
    if ($null -eq $found) {
        Write-Host "imago is not in the cargo registry under $registry - pass -Crate <path>"
        return
    }
    $Crate = $found.FullName
}
if (-not (Test-Path -LiteralPath $Crate)) {
    Write-Host "No such directory: $Crate"
    return
}

"imago source dump from $Crate" | Set-Content -LiteralPath $OutFile
function Emit {
    param([string] $Text = "")
    Add-Content -LiteralPath $OutFile -Value $Text
}
function Heading {
    param([string] $Title)
    Emit ""
    Emit ("=" * 78)
    Emit $Title
    Emit ("=" * 78)
}

Heading "FILE INVENTORY"
Get-ChildItem -LiteralPath (Join-Path $Crate "src") -Recurse -Filter *.rs |
    ForEach-Object {
        $rel = $_.FullName.Substring($Crate.Length + 1)
        $lines = (Get-Content -LiteralPath $_.FullName | Measure-Object -Line).Lines
        Emit ("{0,-52} {1,6} lines" -f $rel, $lines)
    }

# The VMDK driver in full. It is the thing that turns the descriptor into mappings, and every
# hypothesis left standing lives inside it.
Heading "src/vmdk - COMPLETE"
$vmdk = Join-Path $Crate "src\vmdk"
if (Test-Path -LiteralPath $vmdk) {
    Get-ChildItem -LiteralPath $vmdk -Recurse -Filter *.rs | ForEach-Object {
        Emit ""
        Emit ("----- " + $_.FullName.Substring($Crate.Length + 1) + " " + ("-" * 40))
        Emit ""
        Get-Content -LiteralPath $_.FullName | ForEach-Object { Emit $_ }
    }
} else {
    # Older layouts keep it in one file rather than a directory.
    $single = Join-Path $Crate "src\vmdk.rs"
    if (Test-Path -LiteralPath $single) {
        Emit ""
        Emit "----- src/vmdk.rs -----"
        Emit ""
        Get-Content -LiteralPath $single | ForEach-Object { Emit $_ }
    } else {
        Emit "no src/vmdk found"
    }
}

# The gate: what libkrun passes when it opens the descriptor, and the thing that decides
# whether an extent's file gets opened at all.
Heading "IMPLICIT OPEN GATE - every definition and use"
Get-ChildItem -LiteralPath (Join-Path $Crate "src") -Recurse -Filter *.rs |
    Select-String -Pattern "ImplicitOpenGate|implicit_open|open_implicit" -Context 6,30 |
    ForEach-Object {
        Emit ""
        Emit ("--- {0}:{1}" -f $_.Path.Substring($Crate.Length + 1), $_.LineNumber)
        foreach ($l in $_.Context.PreContext)  { Emit "    $l" }
        Emit ">>> $($_.Line)"
        foreach ($l in $_.Context.PostContext) { Emit "    $l" }
    }

# Anything that could stop a mapping early. is_eof is what readv asserts on before it decides
# to hand the guest zeros.
Heading "EOF AND MAPPING TERMINATION"
Get-ChildItem -LiteralPath (Join-Path $Crate "src") -Recurse -Filter *.rs |
    Select-String -Pattern "is_eof|Mapping::Eof|fn get_mapping" -Context 4,20 |
    ForEach-Object {
        Emit ""
        Emit ("--- {0}:{1}" -f $_.Path.Substring($Crate.Length + 1), $_.LineNumber)
        foreach ($l in $_.Context.PreContext)  { Emit "    $l" }
        Emit ">>> $($_.Line)"
        foreach ($l in $_.Context.PostContext) { Emit "    $l" }
    }

$size = (Get-Item -LiteralPath $OutFile).Length
Write-Host ""
Write-Host "Wrote $OutFile ($size bytes)."
Write-Host "Open it in an editor and paste it back - do not read it through the terminal."
Write-Host ""
Write-Host "If it is very large, the section that matters most is 'src/vmdk - COMPLETE',"
Write-Host "followed by 'IMPLICIT OPEN GATE'."
