# Verifying a Boks release on Windows

This is the script to hand a Windows agent, or to follow by hand, the first time a release
archive is tested on a real machine. It is deliberately short: everything Boks can check about
itself is already in `boks doctor`, so what a human is needed for is the part `doctor` cannot
see — whether the thing actually boots, and what Windows says about an unsigned binary.

Every previous Windows result in `docs/verification.md` was obtained by driving containerd by
hand, from CI artifacts, in a tree somebody had built. **None of it used a release archive.**
That is the gap this closes.

> [!IMPORTANT]
> Do not skip a step because an earlier round passed. This tests the *archive*, not the code:
> the same binaries laid out differently is exactly the class of thing that has broken here
> before — a shim whose filename must match a runtime id, a containerd resolved from the wrong
> directory.

## What to report back

For each numbered step: the command, its output, and whether it did what the step says. If a
step fails, **stop and report** rather than working around it — a workaround hides the defect
this is trying to find. Quote errors verbatim; a paraphrased Windows error has lost the part
that identifies it.

---

## 1. Download and verify

From an **ordinary** PowerShell — not elevated. If any step below turns out to need elevation,
that is a finding and it should be reported, because Boks is supposed to need none.

```powershell
$v = "0.1.0"    # the release version being tested
cd $env:USERPROFILE\Downloads
gh release download "v$v" --repo dagsommer/boks -p "boks_${v}_windows_amd64.zip" -p "SHA256SUMS"
```

Check the archive against the published checksum. Do not skip this: it is the only thing
standing between a corrupted download and an afternoon spent debugging a hypervisor.

```powershell
$want = (Select-String -Path SHA256SUMS -Pattern "boks_${v}_windows_amd64.zip").Line.Split()[0]
$got  = (Get-FileHash "boks_${v}_windows_amd64.zip" -Algorithm SHA256).Hash.ToLower()
"want $want"; "got  $got"; if ($want -eq $got) { "MATCH" } else { "MISMATCH — stop here" }
```

## 2. Note what Windows says before you unblock it

**Report exactly what appears, including nothing.** The archive is unsigned and this is the
question the docs currently answer with an assumption.

```powershell
Get-Item "boks_${v}_windows_amd64.zip" -Stream * | Select-Object Stream
```

A `Zone.Identifier` stream means Windows tagged it as internet-sourced (Mark of the Web).
Then extract and run `boks.exe` **once without unblocking anything**, and record whether a
SmartScreen dialogue appears, what it says, and whether it offers "Run anyway":

```powershell
Expand-Archive "boks_${v}_windows_amd64.zip" -DestinationPath .
cd "boks_${v}_windows_amd64"
.\boks.exe version
```

## 3. `boks doctor`

```powershell
.\boks.exe doctor
```

Report the whole table. Expected: everything `ok` except possibly `containerd`, which has
nothing to talk to until the next step.

The two lines that matter most, because they test that the archive's layout is one Boks can
navigate — both files sit beside `boks.exe` rather than on any `PATH`:

- `vm runtime` should name `containerd-shim-nerdbox-v1.exe` **in the archive directory**
- `hypervisor library` should name `krun.dll` in the same place

## 4. Start the daemon

```powershell
.\boks.exe daemon start
.\boks.exe daemon status
```

`status` must report `binary` pointing **inside the archive directory**, and a containerd
version of **2.3 or newer**. If it names a containerd somewhere else — Docker Desktop's, or
one on your `PATH` — that is a defect; report the path and version. (The Linux equivalent of
this bug was found on 2026-08-15 and fixed; whether the fix holds on Windows is untested.)

Then `.\boks.exe doctor` again. `runtime skew` should now read `ok`.

## 5. Run a sandbox — the actual test

```powershell
.\boks.exe run shell . -- cmd /c ver
```

This is the step everything else exists to reach. Report the full output including timings.

Then confirm it is a real VM and not something sharing your kernel:

```powershell
.\boks.exe run shell . -- uname -a
.\boks.exe run shell . -- cat /proc/sys/kernel/random/boot_id
```

`uname` reporting **Linux** on a Windows host is the isolation boundary in one line. The
`boot_id` should differ from any previous sandbox's.

## 6. Network policy

```powershell
.\boks.exe run --allow github.com shell . -- curl -sS -o /dev/null -w "%{http_code}" https://github.com
.\boks.exe run --allow github.com shell . -- curl -sS -o /dev/null -w "%{http_code}" https://example.com
```

The first should print `200`. The second **must fail** — that is the policy being enforced
outside the guest rather than suggested to it. Report both, and if the second succeeds say so
loudly, because it would mean the policy is not a boundary.

## 7. Clean up, and report

```powershell
.\boks.exe ls                      # name each sandbox this run created
.\boks.exe rm -f <name>            # once per name; there is no --all
.\boks.exe daemon stop
```

Report, in this order:

1. whether any step needed an elevated prompt (expected: none);
2. exactly what SmartScreen did in step 2, quoted;
3. the `boks daemon status` binary path and containerd version from step 4;
4. the `uname -a` and `boot_id` output from step 5;
5. the two HTTP results from step 6;
6. anything that failed, verbatim, with the step number.

---

## What this does not test

Worth stating so the result is not read as more than it is. This exercises the release archive
on one machine: it says nothing about winget delivery (a separate path, with its own symlink
indirection), nothing about a machine that has Docker Desktop or Hyper-V already configured,
and nothing about AMD hardware unless the machine happens to be AMD — every Windows
measurement so far has been on Intel. Say which CPU the run was on.
