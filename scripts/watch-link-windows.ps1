# Watch a sandbox's link socket while it starts, on Windows.
#
# The guest's virtio-net backend connects to an AF_UNIX socket that the network supervisor
# binds. When that connect is refused, libkrun panics rather than returning an error, the shim
# aborts, and containerd reports `ttrpc: closed` — a failure that names nothing involved. Worse,
# the evidence deletes itself: the supervisor removes its own directory on the way out, and the
# failing run removes it again on the way out.
#
# So this watches from outside, and keeps a copy of the supervisor's log the moment one exists.
#
#   .\scripts\watch-link-windows.ps1 -Sandbox team-copilot-default-boks_experiment
#
# Run it in a second terminal BEFORE `boks run`, and stop it with Ctrl-C afterwards. The two
# things worth reading in the output are whether net.sock was present at the instant the guest
# failed to attach, and whether a boks process was still alive holding it.
param(
  [Parameter(Mandatory=$true)][string]$Sandbox,
  [string]$StateDir = "$env:LOCALAPPDATA\boks",
  [int]$IntervalMs = 250
)

$dir = Join-Path (Join-Path $StateDir "net") $Sandbox
$out = Join-Path $env:TEMP "boks-linkwatch-$Sandbox.txt"
$logCopy = Join-Path $env:TEMP "boks-stack-$Sandbox.log"

"watching $dir" | Tee-Object -FilePath $out
while ($true) {
  $t = Get-Date -Format "HH:mm:ss.fff"
  # Get-ChildItem rather than Test-Path: an AF_UNIX socket is a reparse point, and listing the
  # directory reports what is actually there without depending on how Test-Path treats one.
  $names = (Get-ChildItem -Force -LiteralPath $dir -ErrorAction SilentlyContinue |
            ForEach-Object Name) -join '|'
  if (-not $names) { $names = "<no directory>" }
  $procs = (Get-Process boks -ErrorAction SilentlyContinue | ForEach-Object Id) -join ','
  "$t [$names] boks=[$procs]" | Tee-Object -FilePath $out -Append
  # Copied every tick: the original is deleted by the cleanup that follows the failure, and it
  # is the only place the supervisor says why it left.
  $log = Join-Path $dir "stack.log"
  if (Test-Path -LiteralPath $log) {
    Copy-Item -LiteralPath $log -Destination $logCopy -Force -ErrorAction SilentlyContinue
  }
  Start-Sleep -Milliseconds $IntervalMs
}
