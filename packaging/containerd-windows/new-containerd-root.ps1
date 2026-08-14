<#
.SYNOPSIS
    Create containerd's --root and --state before containerd does, so that an unelevated
    containerd does not lock itself out of its own state. For an UNELEVATED daemon only --
    see .NOTES before using it with an elevated one.

.DESCRIPTION
    containerd creates its root and state with a hardened ACL and no ACE for whoever is
    running it:

        cmd/containerd/server/server.go:70   sys.MkdirAllWithACL(config.Root,  0o700)
        cmd/containerd/server/server.go:81   sys.MkdirAllWithACL(config.State, 0o711)

        pkg/sys/filesys_windows.go:31
        const SddlAdministratorsLocalSystem = "D:P(A;OICI;GA;;;BA)(A;OICI;GA;;;SY)"

    That SDDL is a PROTECTED DACL (the `P`) granting GENERIC_ALL, inheritable, to
    BUILTIN\Administrators and NT AUTHORITY\SYSTEM, and to nobody else. `P` blocks
    inheritance from the parent, so nothing the parent directory would have granted the
    invoking user survives. It is the right ACL for containerd's normal Windows
    deployment -- a service running as LocalSystem, whose image content, snapshots and
    bolt metadata database an unprivileged user must not be able to edit -- and it is not
    conditioned on the daemon's own identity, so an unelevated containerd writes a DACL
    that excludes itself.

    The failure does not surface at those two calls. Creating a directory is a right on
    the PARENT, so both succeed; the denial lands the first time a plugin writes INSIDE
    one, with plain os.MkdirAll:

        plugins/content/local/store.go:94
        failed to mkdir "C:\cdtest\root\io.containerd.content.v1.content": Access is denied.

    io.containerd.metadata.v1.bolt requires the content store, and roughly forty further
    plugins require bolt, so the whole graph collapses behind an error that names a
    plugin rather than a permission.

    The workaround is exact rather than lucky. mkdirall's fast path is:

        pkg/sys/filesys_windows.go:65
        dir, err := os.Stat(path); if err == nil { if dir.IsDir() { return nil } ... }

    -- an existing directory is returned unchanged, with no ACL applied at all. So a root
    and state that already exist, created by the user with ordinary inherited
    permissions, are simply accepted. Everything containerd then creates underneath them
    is made with plain os.MkdirAll and inherits from them.

    Note that no choice of path avoids this: the fast path skips only directories that
    already EXIST, so pointing --root somewhere inside your own profile does not help if
    the leaf is still missing. Pre-creation is the whole of the fix.

    See packaging/containerd-windows/README.md for why Boks does not patch this out.

.PARAMETER Root
    containerd's --root. Must match `root` in config.toml. Default C:\cdtest\root.

.PARAMETER State
    containerd's --state. Must match `state` in config.toml. Default C:\cdtest\state.

.EXAMPLE
    .\new-containerd-root.ps1

.EXAMPLE
    .\new-containerd-root.ps1 -Root D:\cd\root -State D:\cd\state

.NOTES
    THIS SCRIPT IS FOR AN UNELEVATED containerd, AND ONLY FOR THAT.

    Run it UNELEVATED, as the same user that will run containerd.exe. Running it elevated
    creates directories owned by Administrators and puts you back where you started.

    If you are going to run containerd.exe ELEVATED -- which you must, or turn on
    Developer Mode, if you want to create a task and not merely unpack an image; see
    "Elevation, Developer Mode, and the choice you actually have" in
    packaging/containerd-windows/README.md -- then DO NOT USE THIS SCRIPT, and do not
    point that daemon at a root this script made.

    The reason is the same MkdirAllWithACL fast path this script exploits, read the other
    way round. An existing directory is accepted unchanged, with no ACL applied, so a root
    pre-created by an ordinary user keeps that user's inherited permissions -- and an
    elevated daemon then fills it with the content store, the snapshotters' root
    filesystems and the bolt metadata database, all writable by an unprivileged account.
    That account can put a binary into a layer a later container executes, or repoint an
    image's metadata at content it controls, against a daemon running as an administrator.
    That is the escalation the PROTECTED bit in containerd's SDDL exists to prevent.

    An elevated daemon needs no help here: MkdirAllWithACL succeeds, and the ACL it writes
    names Administrators and SYSTEM, which is what it is running as. Give it a root
    directory of its own and let it create it.

    This script has never been executed. Nobody on this project has Windows.
#>

[CmdletBinding()]
param(
    [string] $Root  = 'C:\cdtest\root',
    [string] $State = 'C:\cdtest\state'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$me    = [System.Security.Principal.WindowsIdentity]::GetCurrent()
$mySid = $me.User.Value

# Ask the filesystem, not the ACL.
#
# containerd's plugins do the equivalent of os.MkdirAll inside these directories, and
# whether that succeeds is the only question worth asking. Parsing a DACL and reasoning
# about which of the current token's group SIDs it matches is a second implementation of
# the Windows access check, and a wrong one. Creating a directory and deleting it again
# is the real check, and it is the same operation containerd will perform.
function Test-BoksDirectoryWritable {
    param([Parameter(Mandatory)] [string] $Path)

    $probe = Join-Path $Path ('.boks-probe-' + [System.Guid]::NewGuid().ToString('N'))
    try {
        New-Item -ItemType Directory -Path $probe -ErrorAction Stop | Out-Null
    } catch {
        return $false
    }
    try {
        Remove-Item -LiteralPath $probe -Force -Recurse -ErrorAction Stop
    } catch {
        Write-Warning "left a probe directory behind: $probe"
    }
    return $true
}

# Grant the current user full control, inheritable.
#
# Only possible because the directory's OWNER is implicitly granted WRITE_DAC by the
# Windows access check even when the DACL names nobody -- which is exactly the situation
# an unelevated containerd leaves behind, since CreateDirectory takes the owner from the
# calling token. If somebody else owns the directory this fails, and it should: the fix
# then is an administrator's, not a script's.
#
# The SID is used rather than the account name because account and group names are
# localised and `icacls` matches them literally.
function Repair-BoksDirectoryAccess {
    param([Parameter(Mandatory)] [string] $Path)

    Write-Host "  repairing DACL on $Path (granting $($me.Name))"

    # icacls writes to stderr on failure, and in PowerShell 7 a native command's stderr
    # under $ErrorActionPreference = 'Stop' raises a terminating NativeCommandError before
    # $LASTEXITCODE can be read. Relax it for this one call so the real message is shown.
    # $code starts at -1 rather than being read straight out of $LASTEXITCODE, because if
    # icacls.exe cannot be launched at all $LASTEXITCODE is never set, and Set-StrictMode
    # then turns "icacls is missing" into "the variable '$LASTEXITCODE' cannot be
    # retrieved" — an error about this script rather than about the machine.
    $out  = $null
    $code = -1
    try {
        $out  = & icacls.exe $Path /grant "*${mySid}:(OI)(CI)F" 2>&1
        $code = $LASTEXITCODE
    } catch {
        $out = $_
    }
    if ($code -ne 0) {
        Write-Host (($out | Out-String).TrimEnd())
        throw ("cannot rewrite the DACL on '$Path'. That works only if you own it. " +
               "Check the owner with: Get-Acl '$Path' | Select-Object -ExpandProperty Owner " +
               "-- if it is not $($me.Name), an administrator has to remove or re-own it.")
    }
}

function New-BoksContainerdDirectory {
    param(
        [Parameter(Mandatory)] [string] $Label,
        [Parameter(Mandatory)] [string] $Path
    )

    Write-Host "$Label = $Path"

    if (Test-Path -LiteralPath $Path -PathType Container) {
        Write-Host '  exists already'
    } else {
        # If an ancestor is itself already locked down -- the usual case is a second run
        # after containerd created C:\cdtest and C:\cdtest\root but nothing under them --
        # creating the leaf fails too. Walk up to the deepest ancestor that exists, make
        # sure we can write into it, and only then create.
        $ancestor = Split-Path -Path $Path -Parent
        while ($ancestor -and -not (Test-Path -LiteralPath $ancestor -PathType Container)) {
            $ancestor = Split-Path -Path $ancestor -Parent
        }
        if ($ancestor -and -not (Test-BoksDirectoryWritable -Path $ancestor)) {
            Write-Host "  cannot write into existing ancestor $ancestor"
            Repair-BoksDirectoryAccess -Path $ancestor
        }

        New-Item -ItemType Directory -Path $Path -Force | Out-Null
        Write-Host '  created'
    }

    if (-not (Test-BoksDirectoryWritable -Path $Path)) {
        Write-Host '  NOT writable -- containerd created this, or something else did'
        Repair-BoksDirectoryAccess -Path $Path
        if (-not (Test-BoksDirectoryWritable -Path $Path)) {
            throw "still cannot create a directory inside '$Path' after repairing its DACL."
        }
        Write-Host '  writable after repair'
    } else {
        Write-Host '  writable'
    }
}

Write-Host "running as $($me.Name)"
Write-Host ''

if ($Root -eq $State) {
    throw 'root and state must be different paths -- containerd refuses otherwise (server.go:68).'
}

New-BoksContainerdDirectory -Label 'root ' -Path $Root
New-BoksContainerdDirectory -Label 'state' -Path $State

Write-Host ''
Write-Host 'Both directories exist and are writable by this user. containerd will now'
Write-Host 'accept them unchanged (MkdirAllWithACL returns early for an existing'
Write-Host 'directory) instead of creating them with an ACL that excludes you.'
Write-Host ''
Write-Host 'These paths must match config.toml. If you edited one, edit the other.'
