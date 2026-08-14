# Running the Linux verification

Boks has one verified platform: macOS on Apple silicon. Linux with KVM — the platform it is
designed for, built for, and shipped for — has never booted a sandbox. This page is for the
person, or the agent, who is about to change that.

It has two parts: the [prompt](#the-prompt) to give an AI agent driving the machine, and the
[things that will mislead it](#the-traps) if nobody warns it first. Both exist because the
run itself is mostly mechanised — [`scripts/verify-linux.sh`](../scripts/verify-linux.sh)
does the work — and what is left over is judgement about what the transcript means.

## Before you start

- **The script never modifies the machine.** It installs nothing and edits no configuration.
  It changes exactly two things and announces both: containerd's content store gains the Boks
  base image, and a temporary directory holds the workspace. Boks' own state goes to a
  throwaway `BOKS_STATE_DIR`, so the operator's real policy log is untouched.
- **It stops at the first failure that makes the rest meaningless.** No `/dev/kvm` means no
  VM, and there is nothing to learn from twenty further checks that all fail for that reason.
- **It cannot report success for a machine it did not test.** Every check is pass, fail,
  indeterminate or skip. `VERIFIED` requires every check to have run *and* passed; anything
  unanswered is `INCOMPLETE`, which is not a pass.

```bash
make build                      # writes ./bin/boks
scripts/verify-linux.sh --list  # what it checks, in order, touching nothing
scripts/verify-linux.sh
```

Exit status is `0` for VERIFIED, `1` for FAILED, `2` for STOPPED or INCOMPLETE.

## The prompt

Give this to the agent verbatim. It is written to be pasted.

---

> You are running the first end-to-end verification of Boks on Linux. Nobody has ever done
> this. The result matters more than the speed, and a wrong "it works" is worse than no
> answer at all.
>
> **Your job is to produce a transcript, not a conclusion.** Run the script, read what it
> says, fix what it tells you to fix, run it again, and report exactly what was observed.
> Do not summarise a failure as a success, and do not describe something as verified when
> the script called it indeterminate.
>
> **Setup.**
>
> 1. Clone the repository and `make build`. Boks needs Go 1.26 or later.
> 2. Read `docs/get-started.md#prerequisites`. Boks needs a running containerd 2.2+, the
>    `containerd-shim-nerdbox-v1` binary on *containerd's* `PATH` (the daemon's, not your
>    shell's), libkrun 1.18+, and `mkfs.erofs` from erofs-utils **1.8 or later**. None of
>    these are installed by the script and it will not install them for you.
> 3. Run `scripts/verify-linux.sh --list` first, so you know what is coming.
> 4. Run `scripts/verify-linux.sh`. Capture the whole transcript.
>
> **If you are inside WSL2** — which is the expected case on a Windows machine, because Boks
> has no native Windows build — read `docs/troubleshooting.md#wsl2` before you start, and:
>
> - Confirm the WSL version on the **Windows** side with `wsl --version`. **2.5.1 is a hard
>   floor**, because it introduced the modules image without which KVM and erofs — both built
>   as modules — cannot be loaded at all. Nothing inside the distribution can report the
>   version, so this is a question you have to ask the Windows side.
> - **Do not start by adding `nestedVirtualization=true` to `.wslconfig`.** It is already on
>   by default on Windows 11 x64, and it is the first thing every generic guide suggests.
>   Discriminate first: `grep -Ec '^flags.*\b(vmx|svm)\b' /proc/cpuinfo`. A non-zero count
>   means nested virtualisation is working and the module is simply not loaded. The script
>   makes the same distinction and prints the correct remedy for whichever case it finds.
> - Keep the workspace in the WSL2 filesystem, under `$HOME`. Not `/mnt/c`: WSL2 reaches the
>   Windows filesystem over 9p, and a workspace there crosses 9p *and* virtiofs to reach the
>   guest.
> - Record the **CPU vendor**. `/dev/kvm` working on an **AMD** machine is the single
>   highest-value unknown in `docs/windows.md`: nested virtualisation is vendor-agnostic in
>   WSL's source, but the empirical record for AMD is thinner, and if it fails there the WSL2
>   route covers far fewer users than it appears to.
>
> **What to do with each outcome.**
>
> - **STOPPED.** The script printed the remedy in full. Apply it, then run the script again
>   from the beginning. Do not skip ahead by hand — the checks after a gate are not
>   meaningful until the gate passes.
> - **A `?????` (indeterminate) result.** Something could not be determined. This is the one
>   outcome you must not round off in either direction. Say what could not be determined and
>   why, and if it is fixable, fix it and re-run.
> - **A `FAIL` in check 7.** Stop and read carefully. This is the network boundary and it is
>   the strongest claim the project makes. A failure here is a real finding and belongs in
>   `docs/verification.md` word for word, with the transcript. It has failed before, on macOS
>   on 2026-08-12, and recording that failure is what led to the fix.
> - **VERIFIED.** Do not stop there. Record the run in `docs/verification.md`: the date, the
>   host OS and kernel, the CPU (vendor and model), whether it was WSL2 and which version,
>   the containerd, nerdbox, libkrun and erofs-utils versions, and the observed boot_ids,
>   uptimes and vCPU counts. An unrecorded pass is an anecdote.
>
> **Then go beyond the script.** It covers a single boot. These are unverified on any Linux
> host and are the natural next steps, in order of value:
>
> 1. `BOKS_INTEGRATION=1 go test ./internal/sandbox/ -run Integration -v` — the suite defaults
>    to the isolating runtime, so a pass means the assertions held behind a VM boundary. A run
>    that logs a warning about a non-isolating runtime does not count.
> 2. `boks run -d`, then `boks exec` from a fresh shell, and compare
>    `/proc/sys/kernel/random/boot_id` between the two: does the exec'd process run inside the
>    *same* VM?
> 3. `boks stop` then `boks start`, and check that a file written to `~` before the stop is
>    still there after — and that the boot_id changed, because `start` boots a new VM rather
>    than resuming one.
> 4. `boks cp` in both directions, which reaches the guest over vsock rather than a local
>    FIFO.
>
> **Reporting.** Give the whole transcript, not a summary of it. Where you inferred something
> rather than observed it, say which. Where a check could not run, say so rather than leaving
> it out.

---

## The traps

These produce a **false pass**: the transcript looks like enforcement working, and it is not.
The script handles all of them; they are written down because an agent that goes off-script —
and it will, when something fails — will walk straight into them.

### The guest's proxy variables are lowercase too

Boks sets **six** variables in the guest: `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` and all
three lowercased. The CLI banner mentions only the uppercase pair.

A probe that unsets `HTTP_PROXY` and `HTTPS_PROXY` and then reaches for a denied host is
**still going through the proxy**, and is correctly refused — which reads exactly like the
network stack enforcing policy. Several runs during the macOS session looked like passes for
this reason. Unset all six, plus `ALL_PROXY`/`all_proxy`, or better, use a raw socket where
there is no configuration to forget.

### A missing tool reads as a block

The base image has **no `wget`, no `nslookup`, no `nc`**, and its `/bin/sh` is dash, so
`/dev/tcp` is unavailable there. A `wget`-based probe fails with `command not found`, and the
exit status is indistinguishable from a refused connection.

It does have `curl`, `python3` and `bash`. Check the tool exists before you conclude anything
from its failure — the script emits `BOKSFACT tooling NO_CURL` and calls the check
indeterminate rather than passing it.

### Blocking everything looks identical to enforcing policy

This is the important one, and it is why check 7 is built the way it is. A network stack that
refused *every* non-proxied flow would produce exactly the transcript of a policy engine that
judges each flow: denied host refused, raw socket refused, everything refused. That is not
enforcement, and it is not what Boks claims.

The only thing that separates them is a **positive control**: a destination the policy
explicitly permits, connecting end to end, in the same sandbox and the same process as the
refusal, presenting **the origin's own certificate**. The script does exactly this — it
resolves the allowed host on the host side, passes those addresses to `--allow`, and requires
the allowed address to complete a TLS handshake while `1.1.1.1` is refused moments later.

If the positive control fails, the script reports the negative control as **indeterminate**,
not as a pass. A run where everything was refused proves nothing.

### `--allow example.com` does not allow `example.com`'s address

`--allow` with a hostname is a hostname rule. A raw socket carries no name, so the flow is
judged on its address, and a hostname-only policy therefore denies direct-by-IP traffic
*including to the allowed host*. That is fail-closed and is the right direction, but it means
"allowed through the proxy" and "allowed on a raw socket" are different questions. The script
allows the resolved addresses explicitly for the raw-socket sandbox, which is why its positive
control connects.

### UDP and ICMP leave no trace

They are dropped silently: nothing appears in `boks policy log` for either. TCP denials are
logged with a reason. A guest quietly probing UDP leaves no record — an observability gap, not
a containment one, but do not read an empty log as "nothing was attempted".

Relatedly, a raw `SOCK_RAW`/`IPPROTO_ICMP` socket *can* be opened inside the guest. It simply
never gets a packet anywhere. Opening it successfully is not evidence of reachability, and a
probe that only checks for `PermissionError` is measuring the wrong thing.

### On Linux, a different kernel version is weak evidence

The macOS run had it easy: the host was Darwin and the guest was Linux, so a shared kernel was
not a possible explanation for anything observed. On Linux both are Linux. The load-bearing
fact is **`boot_id`** — a container shares the host's, a VM has its own — corroborated by an
uptime bounded by the sandbox's age and a vCPU count that tracks `--cpus`. The script boots
twice with different `--cpus` values for exactly this reason: "nproc equals what I asked for"
only means something when two runs asking for different numbers get different answers.

## What the script does not cover

Named here so that a `VERIFIED` verdict is not read as more than it is:

- Whether the guest kernel is the one Boks shipped, rather than one substituted for it. The
  evidence is that the kernel identity is *its own*, not that it is any particular kernel.
- Credential injection and TLS interception. Both are verified elsewhere — against a real
  guest on macOS, and against real origins on a Linux host with no hypervisor — but not here.
- The persistent lifecycle beyond a single boot: stop/start snapshot persistence, `exec` into
  a running sandbox, `cp` over vsock. These are the "then go beyond the script" list above.
- Whether the transport Boks asks for today behaves like the one the macOS run used. That run
  reached the guest over a datagram link; the link is now a stream, and no VM has been booted
  onto it. The script exercises whatever Boks asks for; it cannot tell you that it changed.
