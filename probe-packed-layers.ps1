<#
.SYNOPSIS
    Two measurements for the packed-layer mount failure on Windows.

.DESCRIPTION
    Background. When an image has more layers than the shim's gptLayerThreshold, nerdbox stops
    giving each EROFS layer its own virtio-blk device and packs them all into one
    GPT-partitioned VMDK, one partition per layer. That packed disk fails in the guest, always
    at the FOURTH partition, with:

        mount source: "/dev/vdc4", target: ".../mounts/4", fstype: erofs, flags: 1,
        data: "", err: invalid argument

    Already ruled out by measurement: the layout, the GPT header, and libkrun's VMDK reader
    (imago reads all 17 superblocks correctly on Windows). Also ruled out: offset magnitude —
    it fails at partition 4 whether that partition sits at 4 MiB or at 2.3 GB.

    This script collects the two cheapest things not yet looked at.

    PART 1 (read-only, no daemon, no shim) asks what imago's readv promises. libkrun's
    DiskProperties::read_vectored_at_volatile discards readv's result and reports the full
    length unconditionally, so if readv can return short, the guest is handed a half-filled
    buffer WITH a success status.

    PART 2 (needs -Reproduce) captures the guest kernel's own reason for EINVAL. EROFS says why
    it rejected a mount, and containerd's log carries the guest's kmsg — nobody has read it yet.
    Three answers point three different ways:

        "cannot find valid erofs superblock"  -> the guest read the wrong bytes (data path)
        "unsupported blksize" / "incompatible features"
                                              -> right bytes, filesystem rejected (NOT the data path)
        "Buffer I/O error on dev vdc4"        -> the read failed rather than returned garbage

.PARAMETER Reproduce
    Run part 2. This stops the boks daemon, swaps in the shim given by -Shim, runs one sandbox,
    reads the log, and puts the original shim back. The original is restored in a finally block
    and its SHA256 is verified afterwards.

.PARAMETER Shim
    The threshold-2 (or threshold-8) shim to install for the reproduction. Required with
    -Reproduce. Without it the script lists -BackupDir and stops.

.EXAMPLE
    .\probe-packed-layers.ps1
    .\probe-packed-layers.ps1 -Reproduce -Shim C:\scratch-gpt\backup\shim-threshold2.exe

.NOTES
    Everything printed is also appended to -OutFile, so the whole run can be pasted back in one
    piece. Nothing under %LOCALAPPDATA%\boks is deleted.
#>
[CmdletBinding()]
param(
    [switch] $Reproduce,
    [string] $Shim,
    [string] $BackupDir = "C:\scratch-gpt\backup",
    [string] $Workspace = "C:\scratch-gpt\probe",
    [string] $StateDir  = "$env:LOCALAPPDATA\boks",
    [string] $CaptureDir = "C:\scratch-gpt\captured",
    [string] $OutFile   = "$env:TEMP\boks-packed-probe.txt"
)

Set-StrictMode -Version Latest

# Everything goes to the console and to one file, so the result is copy-pasteable in one piece.
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

Section "PART 0 - what this machine has"

$boks = Get-Command boks -ErrorAction SilentlyContinue
if ($null -eq $boks) {
    Say "boks is not on PATH. Nothing below will work; stopping."
    return
}
$boksDir  = Split-Path $boks.Source
$shimPath = Join-Path $boksDir "containerd-shim-nerdbox-v1.exe"
$logPath  = Join-Path $StateDir "containerd\containerd.log"

Say "boks.exe   : $($boks.Source)"
Say "boks version: $((& boks --version 2>&1 | Out-String).Trim())"
Say "shim       : $shimPath"
if (Test-Path -LiteralPath $shimPath) {
    Say "shim sha256: $((Get-FileHash -LiteralPath $shimPath -Algorithm SHA256).Hash)"
} else {
    Say "shim       : NOT FOUND at that path"
}
Say "containerd log: $logPath (exists: $(Test-Path -LiteralPath $logPath))"

Section "PART 1 - imago readv: read-exact, or can it return short?"

$registry = Join-Path $env:USERPROFILE ".cargo\registry\src"
if (-not (Test-Path -LiteralPath $registry)) {
    Say "No cargo registry at $registry - is Rust installed under a different profile?"
} else {
    $crate = Get-ChildItem -LiteralPath $registry -Directory -Recurse -Depth 2 -ErrorAction SilentlyContinue |
             Where-Object { $_.Name -like "imago-*" } |
             Select-Object -First 1
    if ($null -eq $crate) {
        Say "imago is not in the cargo registry. If your harness vendored it, point me at that copy."
    } else {
        Say "crate: $($crate.FullName)"
        Say ""
        # Every definition and every call, with context. The signature is what matters: a
        # Result<()> is read-exact and clears libkrun; a byte count can be short and does not.
        $hits = Get-ChildItem -LiteralPath $crate.FullName -Recurse -Filter *.rs -ErrorAction SilentlyContinue |
                Select-String -Pattern "fn readv" -Context 4,16
        if ($null -eq $hits) {
            Say "No 'fn readv' found. Widening to any readv mention:"
            Get-ChildItem -LiteralPath $crate.FullName -Recurse -Filter *.rs -ErrorAction SilentlyContinue |
                Select-String -Pattern "readv" |
                ForEach-Object { Say ("{0}:{1}: {2}" -f $_.Filename, $_.LineNumber, $_.Line.Trim()) }
        } else {
            foreach ($h in $hits) {
                Say ("--- {0}:{1}" -f $h.Path, $h.LineNumber)
                foreach ($l in $h.Context.PreContext)  { Say "    $l" }
                Say ">>> $($h.Line)"
                foreach ($l in $h.Context.PostContext) { Say "    $l" }
                Say ""
            }
        }
    }
}

if (-not $Reproduce) {
    Section "PART 2 - skipped"
    Say "Re-run with -Reproduce and -Shim <path> to capture the guest kernel's reason."
    if (Test-Path -LiteralPath $BackupDir) {
        Say ""
        Say "Shims available in ${BackupDir}:"
        Get-ChildItem -LiteralPath $BackupDir -File | ForEach-Object { Say "  $($_.Name)  ($($_.Length) bytes)" }
    }
    Say ""
    Say "Output saved to $OutFile"
    return
}

Section "PART 2 - the guest kernel's reason for EINVAL"

if ([string]::IsNullOrWhiteSpace($Shim)) {
    Say "-Reproduce needs -Shim <path to the threshold-2 shim>."
    if (Test-Path -LiteralPath $BackupDir) {
        Say "Candidates in ${BackupDir}:"
        Get-ChildItem -LiteralPath $BackupDir -File | ForEach-Object { Say "  $($_.FullName)" }
    }
    return
}
if (-not (Test-Path -LiteralPath $Shim)) {
    Say "No such shim: $Shim"
    return
}

# The safety net: the working shim is copied aside and put back in a finally block, so an
# interrupt or an exception cannot leave this machine with a threshold-2 shim installed.
$saved = Join-Path $env:TEMP "shim-working-backup.exe"
Copy-Item -LiteralPath $shimPath -Destination $saved -Force
$savedHash = (Get-FileHash -LiteralPath $saved -Algorithm SHA256).Hash
Say "working shim saved to $saved"
Say "  sha256: $savedHash"

$capture = $null
try {
    Say ""
    Say "--- stopping the daemon and installing $Shim"
    Say ((& boks daemon stop 2>&1 | Out-String).Trim())
    Copy-Item -LiteralPath $Shim -Destination $shimPath -Force
    Say "installed sha256: $((Get-FileHash -LiteralPath $shimPath -Algorithm SHA256).Hash)"

    # daemon start truncates containerd.log, so what follows is this run and nothing else.
    Say ((& boks daemon start 2>&1 | Out-String).Trim())

    New-Item -ItemType Directory -Force -Path $Workspace | Out-Null

    # The descriptor exists for about a second. The shim writes merged_fs_gpt.vmdk into the
    # runtime bundle before the VM starts, and its own cleanup removes the bundle when task
    # creation fails -- which is every run that reproduces this. Keeping the sandbox does not
    # help: the bundle belongs to containerd, not to the sandbox. So it is copied out while it
    # is there, by a job that polls faster than the window is short.
    New-Item -ItemType Directory -Force -Path $CaptureDir | Out-Null
    Get-ChildItem -LiteralPath $CaptureDir -File -ErrorAction SilentlyContinue | Remove-Item -Force
    $bundlesDir = Join-Path $StateDir "containerd\state\io.containerd.runtime.v2.task"
    $capture = Start-Job -ArgumentList $bundlesDir, $CaptureDir -ScriptBlock {
        param($bundles, $dest)
        $deadline = (Get-Date).AddMinutes(5)
        while ((Get-Date) -lt $deadline) {
            $found = Get-ChildItem -LiteralPath $bundles -Recurse -ErrorAction SilentlyContinue |
                     Where-Object { $_.Name -like "merged_fs_gpt*" }
            foreach ($f in $found) {
                # Copied on every tick rather than once: an early tick can catch the file
                # half-written, and the last complete copy is the one that survives.
                Copy-Item -LiteralPath $f.FullName -Destination (Join-Path $dest $f.Name) `
                          -Force -ErrorAction SilentlyContinue
            }
            Start-Sleep -Milliseconds 100
        }
    }
    Say "capturing the descriptor into $CaptureDir (job $($capture.Id))"

    Say ""
    Say "--- boks run (the failure is expected)"
    # Deliberately NOT --rm. The packed disk's descriptor lives in the runtime bundle, and
    # removing the sandbox removes the bundle with it -- which is how the first run of this
    # script destroyed the evidence probe-gpt-descriptor.ps1 needs. A bundle whose task never
    # started is left behind, so the descriptor survives for inspection.
    $runOut = (& boks run shell $Workspace -- uname -a 2>&1 | Out-String)
    Say $runOut.Trim()

    Start-Sleep -Seconds 2

    Section "GUEST KERNEL OUTPUT - erofs, block layer, the failing device"
    if (-not (Test-Path -LiteralPath $logPath)) {
        Say "No log at $logPath"
    } else {
        $interesting = Select-String -LiteralPath $logPath -Pattern "erofs|blksize|Buffer I/O|EXT4-fs|vdc|vdb|I/O error"
        if ($null -eq $interesting) {
            Say "(nothing matched - the tail of the guest's kmsg follows instead)"
        } else {
            foreach ($m in $interesting) { Say $m.Line }
        }

        Section "LAST 60 GUEST KMSG LINES"
        $kmsg = Select-String -LiteralPath $logPath -Pattern "component=kmsg"
        if ($null -eq $kmsg) {
            Say "(no kmsg lines - the guest may not have reached userspace)"
        } else {
            $kmsg | Select-Object -Last 60 | ForEach-Object { Say $_.Line }
        }

        Section "LAST 30 LOG LINES OF ANY KIND"
        Get-Content -LiteralPath $logPath -Tail 30 | ForEach-Object { Say $_ }
    }
}
finally {
    Section "RESTORING THE WORKING SHIM"
    & boks daemon stop 2>&1 | Out-Null
    Copy-Item -LiteralPath $saved -Destination $shimPath -Force
    $nowHash = (Get-FileHash -LiteralPath $shimPath -Algorithm SHA256).Hash
    Say "restored sha256: $nowHash"
    if ($nowHash -eq $savedHash) {
        Say "matches the shim that was there before this script ran."
    } else {
        Say "MISMATCH - restore this by hand from $saved before using boks."
    }
    Say ((& boks daemon start 2>&1 | Out-String).Trim())

    if ($null -ne $capture) {
        Stop-Job   -Job $capture -ErrorAction SilentlyContinue
        Remove-Job -Job $capture -Force -ErrorAction SilentlyContinue
    }
    Say ""
    $caught = Get-ChildItem -LiteralPath $CaptureDir -File -ErrorAction SilentlyContinue
    if ($null -eq $caught) {
        Say "Nothing was captured. Either the run failed before the shim wrote the descriptor"
        Say "(check above for 'ttrpc: closed' with no /dev/vdc4 - that is the virtio-net flake,"
        Say "just run this again), or the window was shorter than the poll."
    } else {
        foreach ($c in $caught) { Say "captured: $($c.FullName)  ($($c.Length) bytes)" }
        $desc = Join-Path $CaptureDir "merged_fs_gpt.vmdk"
        $analyzer = Join-Path $PSScriptRoot "probe-gpt-descriptor.ps1"
        if ((Test-Path -LiteralPath $desc) -and (Test-Path -LiteralPath $analyzer)) {
            Say ""
            Say "--- analysing the captured descriptor"
            # The descriptor names its header blob by a bundle path that no longer exists, so
            # the analyser is told where the copies went.
            & $analyzer -Descriptor $desc -FallbackDir $CaptureDir
        } else {
            Say ""
            Say "Now run:  .\probe-gpt-descriptor.ps1 -Descriptor `"$desc`" -FallbackDir `"$CaptureDir`""
        }
    }
    Say ""
    Say "The sandbox 'shell-probe' was left in place on purpose. Remove it when done:"
    Say "  boks rm shell-probe"
    Say ""
    Say "Output saved to $OutFile"
}
