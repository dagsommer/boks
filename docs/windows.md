# Boks on Windows

Status: **feasibility spike, no implementation.** Nothing in this document has been executed.
No machine on this project has Windows or a hypervisor for it, and none of the findings below
were obtained by running anything on Windows. They are read from the source of
`microsoft/hcsshim`, `containerd` and `gvisor-tap-vsock`, from Microsoft's documentation, from
the CI configuration those projects ship, and — for the section that matters most — from the
shipped Windows binaries of the reference product. Every claim is labelled **verified** (traced
to a primary source), **inferred** (reasoned from one), or **unknown**.

The one thing that *was* demonstrated is that `GOOS=windows GOARCH=amd64 go build ./...`
succeeds. That demonstrates nothing about whether a sandbox would run, and it is not offered
as if it did.

> **Reading order.** This document was written in the order the spike happened, and the spike
> changed its mind. If you are reading it to make a decision rather than to follow the
> reasoning:
>
> - **Where a Windows user should start today** (immediately below) — the WSL2 route, which
>   needs no new code and is the only one that works now.
> - **Section 7** — what the reference product actually does on Windows. This is the finding
>   that matters, and it overturned the rest of the document.
> - **Section 8** — the VMM candidates, the only remaining question for a native port.
> - **Section 4** — the workspace-path decision, which holds regardless of backend.
>
> Sections 1–3, 5 and 6 investigate **LCOW** — Linux containers in a Hyper-V utility VM — which
> was the assumed path at the start and turned out to be the wrong target. That analysis is
> kept because it is correct, because it disposes of an option someone will otherwise propose
> again, and because the supervisor findings in section 6 hold whatever the backend is.

## The verdict

**Windows can host exactly the sandbox Boks builds, including the network enforcement. Boks
cannot build one there because it has no VMM that speaks the platform's hypervisor API — and
that is the whole gap.**

This reverses the conclusion this document reached on its first pass, which was that Windows
structurally could not support host-terminated guest networking. That conclusion was drawn
from the Host Compute Service, and it is true *of HCS*. It is not true of Windows, because the
interesting path does not go through HCS at all.

The reference product settles it. Docker Sandboxes runs Linux microVMs on Windows 11 and
offers the same network policy presets there as on Linux and macOS. It does that with:

- the **Windows Hypervisor Platform** (`winhvplatform.dll`, `winhvemulation.dll`) — a
  **user-mode** hypervisor API, not the Hyper-V management stack;
- its own VMM, which maps guest memory into its own process and **emulates virtio-net in user
  space**;
- a **userspace network stack** that terminates the guest's TCP/IP, applies policy, and
  re-originates flows as ordinary host sockets;
- **no kernel driver at all** — the installer ships no `.sys`, registers no service, and
  installs per-user into `%LOCALAPPDATA%` without administrator rights.

That is Boks' architecture, on Windows, in a shipping product. So the question "can a host
userspace process terminate and judge a guest's every packet on Windows" is answered *yes*, by
demonstration.

| Half | Status |
|---|---|
| A Linux microVM per sandbox on Windows, driven through containerd | **Available in principle.** The reference product ships `containerd-shim-nerdbox-v1.exe` — the same shim family Boks targets — so the orchestration layer is known to port. |
| Terminating the guest's network in a host userspace process | **Architecturally available.** A user-mode VMM owns the virtio-net backend; nothing in the platform stands in the way. |
| **A VMM Boks can use to do it** | **Missing.** libkrun targets KVM and Hypervisor.framework. Docker substituted a VMM of their own, which is not open source. |

The recommendation therefore changes shape. It is **not** "do not build a Windows backend
because policy could not be enforced" — that was wrong. It is:

**Do not build a native Windows backend until there is a WHP-capable VMM to build it on, and
treat finding or building one as the entire question.** Everything else — the shim, the
snapshotter, the workspace mapping, the netstack, the proxy, the policy engine — is either
already portable or a known, bounded piece of work. Section 8 assesses the candidates.

### And there is an answer for Windows users today

**Run Boks inside WSL2.** It needs no new code: Boks is then an ordinary Linux program on an
ordinary Linux kernel, using `/dev/kvm` and its existing netstack, with none of the problems
above. Nested virtualisation is on by default on Windows 11 x64, and the inbox WSL kernel
carries both KVM and EROFS.

It also **preserves the exact-path workspace invariant**, which a native port would have to
abandon (section 4) — inside WSL2 a workspace is already a Linux path.

This is not a Windows port and must not be described as one, and nobody here has run it. But it
is a real answer to "can I use Boks on my Windows machine", it is available now, and it should
be offered rather than buried. **the WSL2 section** has the requirements, the costs and the one
experiment most worth running.

What has *not* changed is the honesty requirement. None of this has been run. "Docker does it,
so it is possible" is a strong existence proof and a weak implementation plan.

## Why the earlier answers were wrong

Twice, and both errors are worth keeping visible because they are the same mistake at
different scales:

1. The repository used to say Windows was **"blocked on nerdbox"** — blocked on a VM runtime
   gaining Windows support. Wrong: Docker ships a Windows nerdbox shim, so the shim is the
   least of it.
2. This document's first pass said Windows **structurally could not** terminate guest traffic
   in userspace, because the HCS device schema exposes only an HNS endpoint. That is a correct
   reading of HCS generalised into a false claim about the platform. HCS is one consumer of the
   hypervisor; WHP is another, and it is the one that matters here.

The lesson is narrow and worth stating: *"the API I looked at cannot do X"* is not *"the
platform cannot do X"*, and the difference is only visible if you go and look at what a working
implementation actually links against.

---

## Where a Windows user should start today: Boks inside WSL2

**There is a Windows story Boks can offer right now, and it needs no new code: run Boks inside
WSL2, where it is an ordinary Linux program on an ordinary Linux kernel.**

This is not a Windows port and should never be described as one. But a developer on Windows 11
who wants Boks can plausibly have it today, and that is worth more than a roadmap entry.

### Why it should work

Boks needs four things from a Linux host. All four are present in a stock WSL2:

| Boks needs | In WSL2 |
|---|---|
| `/dev/kvm` | **Nested virtualisation is on by default on Windows 11 x64** — `EnableNestedVirtualization = !Arm64 && IsWindows11OrAbove()` in WSL's source — and it is vendor-agnostic, not the Intel-only feature it was on Hyper-V historically. `CONFIG_KVM=m` in the inbox kernel. |
| `unixgram` (AF_UNIX `SOCK_DGRAM`) for the link | Fine. `CONFIG_UNIX=y`; the datagram type is unconditional upstream. The link is VMM↔netstack, **both inside the distro**, so Windows' stream-only AF_UNIX never enters it. |
| containerd | Runs. Less trodden than the dockerd path — Rancher Desktop has an open containerd-in-WSL startup bug — but demonstrably works. |
| `erofs` + `mkfs.erofs` | `CONFIG_EROFS_FS=m` in the inbox kernel, enabled by Microsoft in 2022. |

cgroups are clean v2 since **WSL 2.5.1**, which also introduced the modules image that makes
any `=m` symbol loadable at all. That makes **WSL ≥ 2.5.1 a hard floor**, not a recommendation.

### The three things that will actually go wrong

Every ingredient being present is not the same as it working out of the box, and it will not.
These are the failures a user hits in order, and `doctor` now names each one specifically
(`internal/doctor/wsl_linux.go`).

**1. The modules are not loaded.** `CONFIG_KVM*` and `CONFIG_EROFS_FS` are `=m`, and WSL's
boot-time module list is three hardcoded entries — `tun`, `ip_tables`, `br_netfilter`. So both
of the things Boks needs are absent until asked for:

```bash
sudo modprobe kvm_amd      # or kvm_intel
sudo modprobe erofs
```

To persist, in `%UserProfile%\.wslconfig` on the Windows side, then `wsl --shutdown`:

```ini
[wsl2]
loadKernelModules=kvm_amd,erofs
```

`loadKernelModules` is present in WSL's source but **undocumented on Learn**, so treat it as
best-effort and keep `modprobe` as the fallback. `nested=1` on the inner KVM module is **not**
needed — that governs a third level of nesting and is cargo-culted widely.

**2. `/dev/kvm` is `root:root 0600`.** The node appears automatically via devtmpfs, but **WSL
runs no udev**, so the rule that would widen it on an ordinary distribution never runs. The fix
is a group, a membership and a boot command in `/etc/wsl.conf`:

```ini
[boot]
command = /bin/bash -c 'chown root:kvm /dev/kvm && chmod 660 /dev/kvm'
```

plus `sudo usermod -aG kvm $USER`. Check `getent group kvm` first — on Debian and Ubuntu that
group arrives with qemu/libvirt rather than the base system. **Do not use `chmod 666`**, which
most guides suggest and which hands VM creation to every local account. With
`[boot] systemd=true` udev runs and the stock rules should give `root:kvm 0660` without the
command, but systemd is off by default in many images.

**3. `erofs-utils` is too old on the obvious distribution.** containerd's EROFS snapshotter
needs **≥ 1.8**; **Ubuntu 24.04 LTS ships 1.7.1**. Boks' `doctor` checks that `mkfs.erofs`
exists but not its version, so this surfaces later as a confusing failure during an image
unpack. Worth fixing in `snapshotterToolsCheck`, and worth knowing meanwhile.

### Diagnosing it, and the trap in the obvious diagnosis

**Nested virtualisation is already on**, so "add `nestedVirtualization=true` to `.wslconfig`" —
the advice every generic guide leads with — is usually **not** the fix and sends the user to
the Windows side for nothing. It genuinely is off only on Windows 10, on ARM64, on a CPU
predating Haswell or Zen, under `safeMode=true`, or under the `AllowNestedVirtualization`
enterprise policy.

The three causes are distinguishable from inside the distribution, because the Windows setting
is literally `ComputeTopology.Processor.ExposeVirtualizationExtensions`:

| Check | nested virt off | on, module unloaded | on, loaded, permissions wrong |
|---|---|---|---|
| `grep -Ec '^flags.*\b(vmx\|svm)\b' /proc/cpuinfo` | **0** | ≥1 | ≥1 |
| `/dev/kvm` exists | no | **no** | yes |
| open `/dev/kvm` read-write | — | — | **EACCES** |

Two things that look like diagnostics and are not: a **malformed `.wslconfig` fails silently**
— WSL launches normally with the settings ignored, so a typo'd stanza is indistinguishable from
no stanza — and the Windows-side error `Nested virtualization is not supported on this machine.`
goes to `wsl.exe`'s stderr **on the Windows side**, so nothing running inside the distribution
can ever see it. Do not grep for it.

Note also that `.wslconfig` is **global only**; `/etc/wsl.conf` has no nested-virtualisation
key. The section name and key are case-sensitive: `[wsl2]`, `nestedVirtualization`.

### Detecting WSL, for anything that has to branch on it

Use **`/bin/wslinfo`**, which WSL's init creates unconditionally in every distribution on every
boot. The familiar test — matching `microsoft-standard-WSL2` in `/proc/sys/kernel/osrelease` —
comes from `CONFIG_LOCALVERSION` and disappears under a custom kernel, which is exactly the
configuration most likely to be missing KVM and therefore most in need of the diagnosis.
`wslinfo --networking-mode` additionally returns the *live* mode (`nat`, `bridged`, `mirrored`,
`consomme`, `none`, or the literal `wsl1`), which is worth having because `.wslconfig` lies when
mirrored mode silently falls back to NAT.

Rejected alternatives, with reasons: `$WSL_DISTRO_NAME` is environment-only and absent under
systemd units and cron; `/run/WSL` is the interop socket directory and disappears with
`[interop] enabled=false`; `/mnt/wsl` moves when `[automount] root=` is set.

### What is preserved that a native port would lose

**The exact-path workspace invariant survives completely.** Inside WSL2 a workspace is a Linux
path — `/home/dag/src/foo` — and Boks mounts it at `/home/dag/src/foo`, exactly as on any Linux
host. None of section 4 applies. That is a real advantage over a native Windows port, not a
consolation.

It holds even for a workspace on the Windows drive: `/mnt/c/Users/dag/src/foo` is itself a valid
Linux path, so exact-path preservation is literally true there too.

### The costs, stated plainly

- **`/mnt/c` is slow.** WSL2 reaches the Windows filesystem over **9p**, which is the same
  mechanism and the same performance profile as section 3 describes for LCOW. A workspace kept
  on `/mnt/c` will make `git status` on a large repository painful, and it is then crossing 9p
  *and* virtiofs to reach the guest. **Keep workspaces in the WSL2 filesystem.** This is the
  single most important piece of advice for anyone running Boks this way.
- **`/mnt/c` is case-insensitive** by default, with the same consequences for Linux toolchains
  described in section 3.
- **It is two nested VM boundaries.** Boks' microVM runs inside the WSL2 utility VM. The
  sandbox boundary is still a hypervisor boundary, but the threat model now includes WSL2's
  own, and nested virtualisation is a less-exercised path than bare-metal KVM.
- **Do not build a custom WSL kernel.** A custom kernel without a matching modules image
  silently loses every `=m` symbol — which includes **both KVM and EROFS**, the two things Boks
  needs most. Use the inbox kernel. This is a live failure mode, not a hypothetical: it is what
  broke Docker Desktop's bootstrap for a user who built their own 6.18 kernel.
- **The reference product does not do this.** Docker explicitly supports the native Windows
  build and calls installing the Linux build inside WSL "best-effort". Boks offering the
  reverse is a divergence, and an honest one only if labelled as such.

### What is unverified

**All of it, in combination.** Each ingredient is traced to WSL's source, its issue tracker or
its kernel config; nobody on this project has run Boks inside WSL2, or run anything on Windows
at all. The `doctor` logic added for this is tested, but only its logic — the values it reads
have never been read on a real WSL system.

The specific thing most worth testing first is **`/dev/kvm` on an AMD machine**. Nested
virtualisation is vendor-agnostic in WSL's source, but the empirical record for AMD bare metal
is thinner than for Intel, and if it fails there this answer covers substantially fewer users
than it appears to.

That test is cheap — `ls -l /dev/kvm`, `modprobe kvm_amd`, `boks doctor` — and it is the
highest-value single experiment named anywhere in this document.

---

## 1. Is LCOW a live path?

**Answer: it is live as Microsoft's internal Azure infrastructure and deprecated as a Windows
product feature — simultaneously, and both statements are from Microsoft.** For Boks'
purposes that combination is worse than either alone.

LCOW — Linux Containers on Windows — means a Linux container running inside a Hyper-V utility
VM ("UVM"), managed by the Host Compute Service through Microsoft's `hcsshim`. Three different
things have been called "LCOW" and conflating them is the main trap in this area:

1. The **old Docker daemon feature** (`dockerd --platform linux` on Windows, the `lcow`
   graphdriver). **Dead.** Docker's deprecation table records it deprecated in v20.10 and
   removed in v23.0, with the reason stated verbatim: *"the feature never reached completeness,
   and development has now stopped in favor of running Docker natively on Linux in WSL2."*
   The code went in [moby/moby#42451](https://github.com/moby/moby/pull/42451) (2021).
2. The **containerd + runhcs + Linux UVM** path. Alive in code, and the one that matters here.
3. **Kata Containers**, which AKS Confidential Containers use. Easy to confuse with (2) — both
   say "UVM", both use Rego policy — but it is a separate codebase running on Linux hosts, and
   it is **not** hcsshim. Evidence about Kata says nothing about LCOW.

Anyone citing the Docker removal as proof that (2) is dead is answering a different question.

### The deprecation that does apply

Microsoft's support documentation, updated **2026-02-12**, states plainly:

> "The Linux Containers on Windows (LCOW) feature on Windows Server has been deprecated."

The same page disclaims containerd on Windows generally: *"ContainerD running on Windows Server
can create, manage, and run **Windows Server Containers** but Microsoft doesn't provide any
support for it"* — note it claims only Windows containers. Microsoft's own "Set up Linux
containers on Windows" page (updated 2025-09-17) tells the reader to install Docker Desktop and
switch to Linux containers: **WSL2 is the recommended answer**, and LCOW is not mentioned.

containerd's position matches. Its CRI plugin has essentially no LCOW wiring, so
Kubernetes-on-Windows does not use it, and
[containerd#6313](https://github.com/containerd/containerd/issues/6313) — "Unable to run linux
container on windows using ctr and lcow" — was **closed as not planned**. containerd's docs do
not mention Windows snapshotters at all; the issue tracking that gap
([#9822](https://github.com/containerd/containerd/issues/9822)) has been open for over two
years.

### The counter-signal, which is real

Against that, hcsshim is **not** a dormant repository, and the 2026 investment is specifically
in LCOW:

- Latest release **v0.14.1 (2026-04-08)**, with commits continuing through August 2026.
- A brand-new **`cmd/containerd-shim-lcow-v2`** shipped in 2026 (umbrella issue
  [#2667](https://github.com/microsoft/hcsshim/issues/2667)), splitting the monolithic V1 shim
  into per-platform shims. **LCOW is the only one implemented** — the WCOW and process-isolated
  shims are both marked "coming soon". Nobody rewrites a shim architecture LCOW-first for a
  dead feature.
- Roughly 25 PRs between April and August 2026 on **LCOW live migration**, plus continuous
  `pkg/securitypolicy` work. Many commit titles are mirrored Azure DevOps PR numbers, i.e. this
  is developed on Microsoft's internal production pipeline.
- It is **in production**: Microsoft Research's Parma paper states that the `pkg/securitypolicy`
  implementation *"is the technology which enables Confidential Azure Container Instances"*, and
  describes exactly this architecture — a custom container shim on the host, a UVM per pod, a
  guest agent spawning runc inside it.

**The honest reading:** Microsoft deprecated LCOW as a customer-facing Windows Server feature
while doubling down on it as internal Azure infrastructure. It is a platform component now, not
a product. A third party building on it gets the engineering activity without the support, the
documentation, or the distribution.

One requirement worth flagging against the stated goal of **Windows 11**: the new LCOW v2 shim
documents its platform requirement as **Windows Server 2025 (build 26100) or later**. Windows 11
24H2 shares that build number, so it may well work, but the support statement is about the
server SKU and nothing verifies the client one. **Unknown.**

### What is verified present in code

*Read from the exact module versions Boks already depends on — see `go.mod`.*

| Fact | Where |
|---|---|
| `hcsshim` **v0.14.1** ships the full LCOW stack: `internal/uvm`, `internal/lcow`, `internal/hcsoci/resources_lcow.go`, and the Linux guest agent itself under `internal/guest` and `cmd/gcs` | module cache, and the tree on GitHub |
| hcsshim's README declares it: the repo contains "code for the guest agent (commonly referred to as the GCS or Guest Compute Service) used to support running Linux Hyper-V containers" | `README.md` |
| containerd **v2.2.6** registers a `windows-lcow` **snapshotter** plugin and a `windows-lcow` **diff** plugin | `plugins/snapshots/lcow/lcow.go`, `plugins/diff/lcow/lcow.go` |
| `io.containerd.runhcs.v1` is containerd's **default runtime on Windows** | `defaults/defaults_windows.go` |
| containerd's own `ctr` has a first-class LCOW path: `--snapshotter windows-lcow` switches the generated spec to `linux/amd64` and clears the rootfs section | `cmd/ctr/commands/run/run_windows.go` |

Both of those modules are already in Boks' graph (`github.com/Microsoft/hcsshim v0.14.1`,
`github.com/containerd/containerd/v2 v2.2.6`), pulled in by containerd. So the dependency cost
of using them is a promotion from indirect to direct, not a new dependency.

### What is verified *absent*

This is the part that changes the risk assessment, and it comes from the projects' own CI
configuration rather than from anybody's opinion:

- **hcsshim's CI explicitly excludes the LCOW functional tests.** `.github/workflows/ci.yml`
  runs the functional suite as
  `./functional.test.exe -exclude=LCOW,LCOWIntegrity -test.timeout=1h …`, with the comment
  *"Don't run Linux uVM (ie, nested virt) or LCOW integrity tests."* The stated reason is that
  GitHub runners cannot do nested virtualisation, which is a fair reason — but the effect is
  that no public signal exists that LCOW works at any given commit.
- **containerd's CI does not mention LCOW at all.** A grep for `lcow` across
  `.github/` in v2.2.6 returns nothing. containerd *does* run a periodic Windows Hyper-V
  integration job — `.github/workflows/windows-hyperv-periodic.yml` — but it configures
  `runhcs-wcow-hypervisor` with `SandboxIsolation = 1`: that is **Windows** containers with
  Hyper-V isolation, not Linux ones.

hcsshim *has* a substantial LCOW functional suite —
`test/functional/lcow_container_test.go`, `lcow_uvm_test.go`, `lcow_networking_test.go`,
`lcow_policy_test.go` — so the tests exist. They are simply never run in public, and the CI
steps that mention LCOW are compile and unit only. **No public CI anywhere boots a Linux
utility VM.** Real testing happens somewhere Microsoft-internal.

**Conclusion (inferred, but firmly):** for anyone outside Microsoft, "LCOW works at version X"
is not a checkable statement. For a project whose credibility rests on distinguishing verified
from assumed, that is a material risk to write down rather than discover later.

### The boot files, which are the actual blocker

**This is the single most decision-relevant finding in section 1, and it is worse than the
deprecation notice.**

A UVM does not boot from the container image. It needs a kernel and a root filesystem of its
own, and hcsshim looks for them (verified, `internal/uvm/create_lcow.go`):

1. `LinuxBootFiles\` next to the running executable, if that directory exists; otherwise
2. `%ProgramFiles%\Linux Containers\`

and expects `kernel` (or `vmlinux` for direct boot) plus `initrd.img` (or `rootfs.vhd`). The
path is overridable per-container with the annotation
`io.microsoft.virtualmachine.lcow.bootfilesrootpath`.

**These files do not ship with Windows, and there is no public source for a current build.**

- hcsshim's `Makefile.bootfiles` builds the *userland* — `initrd.img` from a base plus a delta
  cpio, `rootfs.vhd` via `tar2ext4` — but **`KERNEL_PATH` is empty**. There is no download
  logic, no LinuxKit reference, no kernel package. **The kernel is a user-supplied
  dependency.**
- The historical public source was **`linuxkit/lcow`**. It is **archived and read-only since
  March 2020**, and its last release is **v4.14.35-v0.3.9 from November 2018** — Linux 4.14.
  Its README says LCOW support *"was experimental and is no longer being developed"*.
- Microsoft plainly has a current image: hcsshim's own test script defaults to
  `C:\ContainerPlat\LinuxBootFiles`, a Microsoft "ContainerPlat" bundle, and Microsoft's
  confidential-ACI examples refer to *"the team within Microsoft responsible for producing the
  UVM image"*. **No public, documented 2026 download for it was found.**

So the position is: the code to boot a Linux UVM is current and actively developed, and the
Linux UVM it boots is not distributed. Until someone demonstrates otherwise, **"we can obtain a
supported UVM kernel and rootfs" must be treated as unproven**, and everything downstream of it
is conditional. This is the first thing anyone attempting a Windows port should settle, and it
is step 4 of the checklist below.

There is also no usable public documentation. hcsshim's README explains how to build the GCS
and the shims but never says where to get a kernel; its only `ctr run` example is a Windows
nanoserver container. **No working public step-by-step exists in 2026** — a port would be
assembling one from source.

---

## 2. Can a runhcs LCOW container run an arbitrary Linux image?

**Answer: yes, with a writable snapshot. Verified from source.** This was the question most
likely to kill the idea outright, and it does not.

The mechanism (verified, `plugins/snapshots/lcow/lcow.go` and
`internal/layers/lcow.go`):

- The **`windows-lcow` snapshotter** unpacks OCI layers into **ext4 VHDs** on the Windows host
  — one `layer.vhd` per layer — and creates a writable `sandbox.vhdx` scratch disk per
  container. Layer conversion is `tar2ext4`, which hcsshim ships as `cmd/tar2ext4`.
- At container start, `MountLCOWLayers` attaches each layer VHD to the UVM over SCSI or VPMem
  and the guest combines them with **overlayfs**. The scratch VHD is the upper layer.
- Scratch size is controllable per snapshot with the label
  `containerd.io/snapshot/io.microsoft.container.storage.rootfs.size-gb`.

So the shape Boks already relies on holds: image in, writable layer on top, container process
inside a VM. Boks' persistence model — "a sandbox is a containerd container plus its writable
snapshot" — survives unchanged, because it is expressed in containerd's vocabulary and
containerd is doing the same job with a different snapshotter.

Two substitutions would be needed in `internal/runtimecfg`, and they are exactly the "single
string" the architecture document promised:

| Today | On Windows |
|---|---|
| `Runtime = "io.containerd.nerdbox.v1"` | `io.containerd.runhcs.v1` |
| `Snapshotter = "erofs"` | `windows-lcow` |

`runtimecfg.ShimBinary` already derives the right executable name from the handler without
modification — `io.containerd.runhcs.v1` → `containerd-shim-runhcs-v1.exe`, including the
`.exe` suffix it already appends on Windows. That helper was written for nerdbox and happens
to be correct here, which is a small piece of evidence that the abstraction was drawn in the
right place.

**Not done, deliberately.** Making those two constants platform-specific is a five-line change
and this spike does not make it. The reason is `IsolatedRuntime()`: it reports whether a
runtime handler provides a VM boundary, and Boks uses that to decide whether it may present
something as a sandbox. Adding `io.containerd.runhcs.v1` to it would make Boks assert, on a
platform where nothing has ever been run, that its sandboxes are isolated. That assertion has
to be earned on hardware, not by editing a constant.

---

## 3. How are host directories shared into the guest?

**Answer: Plan 9 (9p) over a Hyper-V socket, and it has sharp edges.** Verified from
`internal/uvm/plan9.go`, `internal/uvm/share.go` and the guest side in
`internal/guest/storage/plan9/plan9.go`.

The path an OCI bind mount takes (verified, `internal/hcsoci/resources_lcow.go`):

```
host C:\Users\dag\src\foo
  -> HCS Plan9Share, AccessName "<counter>", served by vmwp.exe on vsock port 564
  -> guest mount -t 9p -o trans=fd,rfdno=N,wfdno=N,msize=65536,aname=<counter>
  -> container <mount.Destination>      (destination is free-form)
```

This is structurally the same as nerdbox's virtiofs mapping, and — importantly — **the
container-side destination is an ordinary OCI spec field that the shim leaves alone.** It
rewrites `mount.Source` to the UVM path and never touches `mount.Destination`. So requesting
an arbitrary absolute guest path is supported, exactly as on Linux and macOS. `AddPlan9`
validates only that the path is non-empty.

Boks' existing single-file caveat transfers verbatim: hcsshim shares the **parent directory**
when the source is a file, restricting visibility with an `AllowedFiles` list. Boks already
refuses to mount single files, so this costs nothing.

### The sharp edges

| Property | Finding |
|---|---|
| **Mechanism** | 9p only. VSMB is rejected for Linux UVMs (`errNotSupported`). **There is no virtiofs in hcsshim** — a grep of the tree returns nothing, and the HCS device schema has no virtio bus at all. |
| **Performance** | The guest mount passes **no `cache=` option** and `msize=65536`, so 9p runs uncached over an hvsock stream with a user-mode server in `vmwp.exe`. Inferred, but strongly: this is the same architecture as WSL 1/WSL2's `/mnt/c`, which is the canonical example of slow. Microsoft replaced it in WSL2 with virtiofs; **that replacement does not exist for LCOW.** Boks' parity matrix already flags large-repo slowness over passthrough as a known cost; on Windows it would be substantially worse. |
| **Case sensitivity** | **Case-insensitive.** `Plan9ShareFlagsCaseSensitive` exists but hcsshim leaves it commented out, with a TODO saying it only works if the host path supports case sensitivity and that detection is unimplemented. |
| **Cache coherency** | Two 9p shares over the same host directory are not coherent (hcsshim issue #464); the fix was refcounting to avoid creating two, not making them coherent. |
| **Ownership / permissions** | `Plan9ShareFlagsLinuxMetadata` is always set, so Linux mode/uid/gid ride along in NTFS extended attributes. The exact scheme is not documented in open source (the server is in `vmwp.exe`). No uid/gid mapping options are passed. Windows ACLs are not visible to the guest; actual access is gated by the host token of the VM worker process. |
| **Share count limit** | **Unknown.** hcsshim has a monotonic counter and no maximum, unlike SCSI and VPMem which both have explicit caps. Any limit lives inside closed-source HCS. |

**Case-insensitivity outlives this section, so it is worth flagging beyond LCOW.** A Linux
toolchain on a case-insensitive share is a well-known source of quiet breakage — `Makefile`
versus `makefile`, two Go files differing only in case, npm packages, anything that resolves
imports by exact name. Here it is unfixable, because the flag that would fix it is disabled
upstream in hcsshim.

It is not an LCOW problem though; it is an NTFS one. **Any** route from a Linux guest to a
directory on a Windows volume inherits it — a native WHP port sharing a Windows directory over
virtiofs, and `/mnt/c` under WSL2, both included. The only route that avoids it entirely is a
workspace living in a Linux filesystem, which is one more reason the WSL2 section recommends keeping
workspaces inside the WSL2 distro rather than on the Windows drive.

---

## 4. The workspace path problem

This is the deviation, and it needs to be stated plainly because the rest of the project treats
the property as inviolable.

**Boks' defining behaviour is that a workspace appears in the guest at its absolute host
path.** `internal/workspace` says so in its package comment; `docs/architecture.md` and the
parity matrix both list it as a P0 that is done and verified. On Windows the host path is
`C:\Users\dag\src\foo`, and the guest is Linux, where that is not a path at all — a colon is
legal in a Linux filename, a backslash is legal in a Linux filename, and there is no drive
letter concept to preserve. **Exact-path preservation as Boks defines it is impossible on
Windows.** No amount of runtime work changes that; it is a statement about two path grammars.

So something must translate, and the only question is what.

**Scope, and it is narrower than it looks.** This applies to a *native* Windows port, where the
host path is a Windows path. It does **not** apply to Boks running inside WSL2 (the WSL2 section),
where the host is Linux and the workspace path is already a Linux path — `/home/dag/src/foo`
mounts at `/home/dag/src/foo`, and even a Windows drive reached as `/mnt/c/Users/dag/src/foo`
is a valid Linux path preserved verbatim. **The invariant holds completely on that route**,
which is a genuine argument in its favour and not merely a convenience.

### What the runtime permits

Everything. As established in section 3, the guest mount destination is free-form. The
constraint is entirely a design choice, not a platform limit.

### What Docker Sandboxes does — verified

**`sbx` maps `C:\Users\dag\src\foo` to `/c/Users/dag/src/foo`.** Evidence:
[docker/sbx-releases#215](https://github.com/docker/sbx-releases/issues/215) and
[#31](https://github.com/docker/sbx-releases/issues/31).

That is not an arbitrary pick. It is the Docker family's convention and has been for over a
decade, with an unbroken lineage:

| Artifact | Behaviour |
|---|---|
| `docker-machine` VirtualBox driver on Windows | shares `C:\Users` under the share *name* `c/Users` |
| `boot2docker` init | mounts each share at `/$name`, giving `/c/Users/...` |
| `docker-machine` source, verbatim TODO | `// ie, s!^([a-z]+):[/\\]+!\1/!; s!\\!/!g` |
| Docker Desktop today, **documented** CLI input | `docker run -v /c/Users/user/work:/work alpine` |
| Docker Compose | `COMPOSE_CONVERT_WINDOWS_PATHS=1` converts `C:\Users` → `/c/Users` |
| minikube, VirtualBox driver on Windows | host `C://Users` → guest `/c/Users` |
| **Docker Sandboxes** | `C:\Users\dag\src\foo` → `/c/Users/dag/src/foo` |

That decade-old `docker-machine` regex — lowercase drive, drop the colon, backslashes to
forward slashes, prefix `/` — **is exactly the rule `sbx` implements today.**

Worth correcting a natural assumption: Docker has never documented `/mnt/c` as an input
anywhere. `/host_mnt/c/...` and `/run/desktop/mnt/host/c/...` are de-facto internals that
appear in no official page and should be treated as inferred, not verified.

### The conventions in use elsewhere

| Convention | Used by |
|---|---|
| `/c/Users/dag/src/foo` | **Docker family** (above), Git Bash / MSYS2 |
| `/mnt/c/Users/dag/src/foo` | **WSL** `DrvFs` automount, podman machine, Rancher Desktop |
| `/host_mnt/c/...`, `/run/desktop/mnt/host/c/...` | Docker Desktop internals (undocumented) |
| preserve the absolute path | **Lima**, whose `mountPoint` defaults to the host `location` — but this works only because macOS paths are already valid Linux paths. Lima's `wsl2` VM type on a Windows host publishes **no** path convention. The project that thought hardest about exact-path mounting has no Windows answer. |
| no preservation | most VM tools: a fixed `/workspace` |

**One invariant holds across every tool checked** — WSL, podman, docker-machine, MSYS2,
minikube, `sbx`: **the drive letter is lowercased and the rest of the path preserves case.**
That is the closest thing to a standard that exists.

### What Boks should do

**Recommendation: match `sbx` exactly — map `C:\Users\dag\src\foo` to
`/c/Users/dag/src/foo`.** Lowercase the drive letter, drop the colon, replace backslashes with
forward slashes, prefix with `/`.

The reasons, in order of weight:

1. **Boks' stated purpose is behavioural parity with Docker Sandboxes.** This is a case where
   the reference product has already made the decision, the decision is defensible, and
   matching it costs nothing. Diverging would cost user surprise for no benefit — and the
   parity matrix would have to record a difference that exists only because this spike
   preferred a different spelling.
2. **It is reversible.** Exactly one host path maps to a given guest path and vice versa. That
   matters more than familiarity, because Boks needs the inverse in real places: `boks cp`
   translating between the two sides, error messages naming a path the user can open, and
   `inspect` reporting a workspace the user recognises. A lossy mapping — dropping the drive
   letter to give `/Users/dag/src/foo` — is ambiguous the moment a user has both `C:\src` and
   `D:\src`, and would be silently wrong rather than loudly wrong.
3. **It is shorter than `/mnt/c`,** which matters more than it sounds: every parent component
   becomes a directory the guest has to scaffold as a mount point.
4. **It preserves the *structure* of the property even though it cannot preserve the string.**
   The guest path is still derived from, and unique to, the host path; the workspace still
   appears at one predictable location; parents still exist only as empty mount points. What is
   lost is literal string equality, and only that.

Rules that follow, and should be decided up front rather than discovered:

- **Also create `/mnt/c` as a symlink to `/c`** (and the reverse for any other drive). It costs
  nothing and absorbs the muscle memory of every WSL user. This is the accepted community
  workaround for exactly this confusion in Rancher Desktop
  ([#6630](https://github.com/rancher-sandbox/rancher-desktop/issues/6630)), where a
  pre-converted `/c/...` path silently failed to mount.
- **Do not encode the colon.** `/C:/Users/...` round-trips unambiguously — a colon is legal in
  a Linux filename and illegal in a Windows one — and no tool anywhere does it, because it
  breaks `PATH`, `LD_LIBRARY_PATH`, Docker's own `src:dst:opts` syntax, `scp`/`rsync` host
  specs and Makefile rules. Ruled out.
- **Lowercase the drive letter**, so `C:` and `c:` give one guest path rather than two, and
  never uppercase it: `/mnt/C` does not exist in WSL either, because the guest side is a
  case-sensitive filesystem where the directory was created literally as `c`.
- **UNC paths (`\\server\share\...`) are refused**, not mapped. WSL does not automount them,
  there is no established spelling, and a share reached over SMB inside a 9p share inside a VM
  is a failure mode nobody wants to debug.
- The existing symlink resolution stays: Boks resolves the host path first, then maps.
- Sandbox naming is unaffected — it uses the workspace's base name, which survives the mapping.

### The consequences, stated as costs

- **`Workspace.GuestPath != Workspace.HostPath` on Windows.** Every statement in
  `docs/architecture.md`, `docs/docker-sandbox-parity.md` and `internal/workspace`'s package
  comment about exact-path mounting becomes platform-conditional. Those documents would need
  editing, not a footnote.
- **Absolute paths generated inside the sandbox stop being valid on the host.** This is the
  concrete user-visible loss and it is the whole reason the property exists. A binary compiled
  in the sandbox with embedded debug paths, a `compile_commands.json`, a `.pyc`, a generated
  lockfile with absolute paths, an IDE index — all refer to `/c/...`, which no Windows tool can
  open. On Linux and macOS these are portable across the boundary in both directions; on
  Windows they are not. The argument for exact-path mounting is precisely that "build output,
  stack traces and tool config keep working", and on Windows they keep working *inside* the
  sandbox only.
- **Host-side configuration containing `C:\` paths is meaningless in the guest**, which is the
  same problem in the other direction and was always true for any Windows-to-Linux crossing.

The honest summary for a user: on Windows a workspace is *mounted at a predictable, reversible
translation of* its host path, not at its host path. That is a real deviation from what Boks
promises everywhere else, and it should be documented as a platform difference in the parity
matrix rather than quietly implemented.

### One thing this does *not* deviate on

The reference product makes the same compromise. Docker Sandboxes' documentation makes the same
exact-path claim Boks does, and on Windows it translates — to `/c/...`, as established above.
So the deviation here is from *Boks' own stated invariant*, not from parity: on this point Boks
and `sbx` would still agree. That is worth stating because it is the rare case where the honest
answer and the compatible answer are the same one.

---

## 5. Can an *LCOW* guest get a NIC our netstack can terminate?

**Answer: no — and this is the finding that made the first pass of this document reach the
wrong verdict.**

> **Scope.** Everything in this section is about the Host Compute Service, the API that drives
> Hyper-V containers and LCOW. It is accurate. It is **not** a statement about Windows: a
> user-mode VMM on the Windows Hypervisor Platform emulates its own NIC and never asks HCS for
> one, which is what the reference product does (section 7). The section is kept in full
> because `NetworkAdapter{EndpointId, MacAddress, IovSettings}` is exactly the artefact that
> produces a confident wrong answer, and the next person to look will find it too.

Boks enforces by owning the far end of the guest's NIC. The VMM writes the guest's Ethernet
frames to a host socket; a gvisor netstack in a Boks process terminates them and judges every
flow before dialling. That is why a guest which ignores `HTTP_PROXY` is still contained — it is
stated as the central property in `docs/architecture.md` and `docs/security-model.md`.

That shape requires something that can hand a userspace process the guest's frames. **HCS
cannot**, and if HCS is how you create the VM, there is no way to arrange it.

### The evidence

**The HCS device schema has no such device.** `internal/hcs/schema2/devices.go` enumerates
every device a Hyper-V VM can be given: `ComPorts, Scsi, VirtualPMem, NetworkAdapters,
VideoMonitor, Keyboard, Mouse, HvSocket, EnhancedModeVideo, GuestCrashReporting, VirtualSmb,
Plan9, Battery, FlexibleIov, SharedMemory, VirtualPci`. There is no virtio bus, no tap, and no
socket-backed network device.

**The one networking device is a pointer to an HNS endpoint.** `NetworkAdapter` is, in full:

```go
type NetworkAdapter struct {
	EndpointId string       `json:"EndpointId,omitempty"`
	MacAddress string       `json:"MacAddress,omitempty"`
	IovSettings *IovSettings `json:"IovSettings,omitempty"`
}
```

Three fields. No socket path, no file descriptor, no external provider. `internal/uvm/network.go`
attaches a NIC by sending exactly this to HCS with an `EndpointId` obtained from HNS/HCN. The
guest receives configuration only — addresses, routes, DNS — which GCS applies to the synthetic
VMBus NIC (`netvsc`) that HNS plugged into the Windows vSwitch.

**The vSwitch can be extended, but only from kernel mode.** Hyper-V's Extensible Switch does
support third-party capture, filtering and forwarding extensions — as **NDIS filter drivers**
or **WFP callout drivers**. A signed kernel-mode driver is not a host userspace process, and
shipping one is not a direction this project should take.

**`ncproxy` is not the escape hatch its name suggests.** hcsshim's `NetworkConfigProxy` /
`NewExternalNetworkSetup` externalise *who decides the network configuration*, not *who carries
the packets*. The result still funnels into `addNIC(endpoint)` with an HCN `EndpointId`. The
data plane is unchanged.

### The near-miss: an internal HNS network

One arrangement gets closer than any other and is worth writing down, because it is the first
thing a reviewer will propose and it *almost* works.

HNS can create an **`Internal`** network: the guest gets an endpoint, and the **host** gets a
vNIC on the same layer-2 segment. Point the guest's default route at that host vNIC, leave
Windows IP forwarding disabled and configure no NAT, and the guest can reach exactly one thing
— the host. Run the Boks proxy on the host vNIC and the guest's HTTP and HTTPS go through it.

That is containment, and it is real. It is **not** what Boks does, and the difference is
precisely the property the project spent its netstack work on:

| | Boks today | Internal HNS network |
|---|---|---|
| Who terminates the guest's frames | a Boks netstack | the Windows kernel |
| Raw TCP to an arbitrary address | judged per flow against policy, allowed or `RST` with a logged reason | fails, because nothing routes it — no decision, no log entry |
| Allowing a specific IP or port | a policy rule | impossible without host firewall/route surgery Boks does not own |
| `transparent`-mode flows in `boks policy log` | recorded | do not exist |
| What enforcement rests on | a stack Boks assembled and can test | Windows routing and firewall state, which anything with admin can change |

So an internal HNS network would give Boks a **proxy-only** posture: cooperating clients get
hostname rules and credential injection, and everything else gets silence instead of a
judgement. Boks explicitly rejected that model on Linux and macOS — `docs/architecture.md` calls
the guest environment "a convenience, not the control", and the whole point of putting policy
in the stack's own TCP forwarder was that a guest ignoring `HTTP_PROXY` must still be judged.

It is worth knowing this exists, because it is the best available Windows posture and it is
better than nothing. It should not be described as network policy enforcement, because a policy
that cannot say "allow this address" and cannot log a denial is not the same object.

### Why hvsock does not close the gap

The original hypothesis was that gvisor-tap-vsock's hvsock transport could stand in for the
missing `unixgram`. It cannot, and the reason is a difference in kind rather than degree.

- **hvsock is a stream between two processes, not a link.** `AF_HYPERV` supports `SOCK_STREAM`
  only; there are no frames, no MAC, no ARP, no MTU. (Separately: Windows' AF_UNIX also
  implements only `SOCK_STREAM`, which is the real reason gvisor-tap-vsock's
  `unixgram_windows.go` is a stub — the gap is in the OS, not the library.)
- **Frames cross it only because an agent inside the guest puts them there.**
  gvisor-tap-vsock's guest binary `gvforwarder` (`cmd/vm/main_linux.go`) opens `/dev/net/tun`,
  creates `tap0`, dials the host over vsock, and shuttles frames in both directions. The
  project's own README says so: *"A tap network interface is running in the VM. It's the
  default gateway… Tap device sends these packets to a process on the host using vsock."*
- **That is how podman machine works on Hyper-V** — a Fedora CoreOS guest running
  `gvforwarder -preexisting -iface vsock0 -url vsock://2:<port>/connect` from an ignition unit.
  It is a supported, real deployment. It is just not the same security posture: the datapath
  depends on a cooperating component living *inside* the boundary being enforced.

Compare the macOS path Boks actually verified: `vfkit` is started with
`--device virtio-net,unixSocketPath=…`, so **the hypervisor itself** is the far end of the
socket and no guest cooperation is involved. That device is what Hyper-V does not have.

### The one construction that might work, and why it is not proposed

For completeness, because it should be ruled out explicitly rather than left as an
unexamined hope:

1. Create the UVM with **no HNS endpoint at all**, so the vSwitch path does not exist.
2. Open a channel with the `io.microsoft.virtualmachine.lcow.extra-vsock-ports` annotation and
   run the Boks netstack on that hvsock service, scoped to the UVM's VM ID.
3. Run `gvforwarder` inside the UVM — GCS does support processes outside any container, and
   the shim exposes `DiagExecInHost` over its `shimdiag` ttrpc socket to start them.

Every guest packet would then terminate in a Boks process, and enforcement would hold in the
sense that matters: a hostile guest could kill the forwarder, but that only costs it the
network, and frames it injects directly are still judged by our stack.

It is not proposed, for four reasons:

- **The LCOW kernel probably has no TUN.** This is **unknown** and is the load-bearing
  unknown. hcsshim does not build the kernel; `Makefile.bootfiles` takes it as an external
  input, and the rootfs it builds contains only `init`, `vsockexec`, `gcs`, `gcstools` and
  `wait-paths`. Nothing in the guest tree references `tun` or `/dev/net/tun`, and the kernel is
  deliberately minimised (`pci=off`, virtio blacklisted). Assume absent until checked.
- It means **shipping a modified UVM image**, which means owning a kernel and initrd build —
  far more than "replace the runtime handler string".
- It fights the shim's entire networking path, and containerd/CRI would have to be prevented
  from creating endpoints.
- `hvsock.Listen` as gvisor-tap-vsock calls it uses `VMID: GUIDWildcard`, accepting connections
  from **any** partition on the host. For a per-sandbox stack that is a cross-sandbox concern
  that would have to be closed by scoping to the UVM's VM ID.

### The conclusion this supports, and the one it does not

**Supported:** if Boks were to create its VMs through HCS, it could not enforce network policy
on them, and a port built that way should ship `-net none` only. Every route out of that is
either a kernel-mode driver or a guest-side agent in a UVM image Boks would have to own.

**Not supported, though the first pass of this document claimed it:** that Windows cannot do
this. The premise "if HCS is how you create the VM" is a choice, not a constraint, and it is
one the reference product declines to make. Do not create the VM through HCS.

That is the whole reason this section is now a cul-de-sac rather than a verdict.

---

## 6. What does the supervisor look like?

**Answer: the design survives intact; the primitive differs in one semantically important way;
and there is nothing to supervise until a VMM exists.**

`internal/enforce` proves a supervisor is alive by holding an `flock` for the life of the
process (`lock_unix.go`). The property that makes it correct is that **the kernel releases the
lock when the holder dies, however it dies**, so "can I take this lock" answers "is that
supervisor gone" with no window and no risk of signalling a recycled PID.

The Windows equivalent is `LockFileEx` with
`LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY`, which is what Go's own toolchain uses in
`cmd/go/internal/lockedfile`. Details, all verified:

- **Release on death is guaranteed but not prompt.** Microsoft's documentation states: *"If a
  process terminates with a portion of a file locked … the locks are unlocked by the operating
  system. However, the time it takes for the operating system to unlock these locks depends
  upon available system resources."* Go's `lockedfile` restates this. **This is a genuine
  semantic difference from `flock`**, and a port could not treat a single
  `ERROR_LOCK_VIOLATION` as proof the previous supervisor is alive — it would need a short
  retry, which the Unix version does not.
- Error codes: `ERROR_LOCK_VIOLATION` (33) means held; `gofrs/flock` also treats
  `ERROR_IO_PENDING` (997) as held. Neither maps to `fs.ErrPermission` or `fs.ErrExist`, so
  `errors.Is` against Go's portable sentinels does not work — the raw `syscall.Errno` must be
  compared.
- Alternatives are worse. `CreateFile` with `dwShareMode = 0` releases more promptly but
  produces **false positives** whenever Defender, the search indexer or a backup agent holds a
  transient handle, which would report a live supervisor where there is none. Named mutexes
  lose filesystem addressability and require `runtime.LockOSThread` for the hold; Microsoft's
  own `CreateMutex` documentation recommends a locked file in the user's profile directory for
  exactly this use case.
- State belongs under `os.UserCacheDir()` → `%LOCALAPPDATA%`, **not** `os.UserConfigDir()` →
  `%AppData%`, because the latter is the roaming profile and gets synced to a file server in
  domain environments. Machine-local liveness state must not roam.
- Boks' link socket path would also need attention: Windows supports `AF_UNIX` `SOCK_STREAM`
  from Windows 10 1803, but **not `unixgram`**, and `sun_path` is still ~108 bytes while
  `%LOCALAPPDATA%` paths are long. A named pipe (`\\.\pipe\…`) is the idiomatic Windows answer
  and has no stale-file problem.

**Does the per-sandbox supervisor design survive? Yes, and unusually cleanly.** The supervisor
exists to hold one sandbox's link and run the netstack behind it, for exactly the sandbox's
lifetime. Nothing in that reasoning is Unix-specific: it follows from the stack having to
outlive the CLI invocation and not outlive the VM. A Windows port would keep the shape, swap
`flock` for `LockFileEx` with the retry the promptness caveat demands, and quite possibly swap
the UNIX socket for a named pipe.

One detail would change and is worth flagging now: `detach` on Unix calls `setsid` so the
supervisor survives Ctrl-C and the closing terminal. The Windows equivalent is
`CREATE_NEW_PROCESS_GROUP` (and usually `DETACHED_PROCESS` or `CREATE_NO_WINDOW`) in
`SysProcAttr.CreationFlags`, which is a different mechanism with different console-inheritance
consequences, not a rename.

**This spike does not implement any of it**, because there is no VM under it yet: a correct
lock with no caller would suggest the supervisor is one file away from working, and it is a
whole VMM away. `lock_windows.go` records the `LockFileEx` design and the promptness caveat in
a comment so that whoever does implement it does not rediscover them.

---

## 7. What the reference product actually does

**This is the section that decides the document.** Everything above investigates LCOW; Docker
Sandboxes does not use LCOW.

The evidence is the shipped Windows installer, `DockerSandboxes.msi` v0.38.0, downloaded from
the project's own releases and unpacked. Static analysis of a signed binary is not the same as
watching it run, but for questions of the form "what does this link against" it is close to
conclusive, and it is a great deal better than inference from documentation.

### The installed payload

| File | Note |
|---|---|
| `bin\sbx.exe` | the CLI and daemon, Go |
| `libexec\containerd-shim-nerdbox-v1.exe` | **the nerdbox shim, on Windows** |
| `libexec\mkfs.erofs.exe`, `libexec\mkfs.ext4.exe` | the same filesystem tools Boks' `doctor` checks for |
| `libexec\nerdbox-rootfs-x86_64.erofs` | guest rootfs |
| `libexec\lib\sailor.dll` | the VMM |

Two structural facts about the installer carry as much weight as the file list:

- **There is no `.sys` anywhere in it**, and its MSI tables contain no `ServiceInstall`, no
  `ServiceControl` and no driver-installation tables. It *cannot* install a kernel driver or a
  service.
- **It installs per-user into `%LOCALAPPDATA%`** and needs no administrator rights. A
  kernel-mode component is structurally impossible under that constraint.

So the answer to "does Docker ship a signed kernel driver to police sandbox traffic on Windows"
is **no**, and it is no for a reason that cannot be worked around by a different build: the
product does not install anything privileged.

### What the VMM links against

`sailor.dll`'s import table:

```
winhvplatform.dll  → WHvCreatePartition, WHvSetupPartition, WHvMapGpaRange,
                     WHvCreateVirtualProcessor, WHvRunVirtualProcessor, WHvTranslateGva, …
winhvemulation.dll → WHvEmulatorCreateEmulator, WHvEmulatorTryIoEmulation,
                     WHvEmulatorTryMmioEmulation
ws2_32.dll         → socket, connect, WSARecv, send, getaddrinfo, …
virtdisk.dll       → OpenVirtualDisk, AttachVirtualDisk
```

What is **absent** matters as much: no `vmcompute.dll` (HCS), no `computenetwork.dll` (HNS/HCN),
no `fwpuclnt.dll` (WFP), nothing NDIS. The VMM that owns the VM and its NIC never touches the
Hyper-V management stack or the Windows filtering platform.

`WHvEmulatorTryMmioEmulation` and `WHvEmulatorTryIoEmulation` are the signature of a user-mode
VMM emulating its own devices: the guest's writes to the virtio queues trap out of
`WHvRunVirtualProcessor` and are decoded in Docker's own process.

Strings in the same DLL name a Rust `net-stack` crate —
`crates\net-stack\src\{stack,wire,arp,dhcp,dns,tcp,udp,icmp_proxy,egress,peek,pcap}.rs` — plus
virtio-net worker messages. That is a complete userspace TCP/IP stack sitting behind a
userspace virtio-net backend. It is the same object as Boks' gvisor stack behind libkrun's
virtio-net, built from different parts.

### The data path

```
guest process
  → guest Linux virtio-net driver (inside the microVM)
  → virtio queues in guest physical memory (WHvMapGpaRange'd from the VMM's own heap)
  → MMIO/PIO trap → WHvRunVirtualProcessor exit → WHvEmulatorTryMmioEmulation
  → virtio-net backend, user mode
  → userspace net-stack: wire → arp/dhcp/dns → tcp/udp/icmp → SNI peek → egress
  → policy decision, and MITM for hosts that need it
  → ordinary Windows user-mode sockets
```

Compare Boks on macOS: guest → libkrun's virtio-net → host socket → gvisor netstack → policy →
host sockets. **The same shape, with the VMM's device backend in place of a socket hop.**

### Enforcement is at full parity on Windows

The Windows `sbx.exe` contains `gvisor.dev/gvisor` and `github.com/elazarl/goproxy` at the
**same versions as the macOS build**, and its strings include the three proxy modes Boks
mirrors — `forward`, `forward-bypass`, and a transparent forwarding dialer that peeks SNI on a
connection the userspace stack has already terminated. Docker's documentation states that UDP
and ICMP are blocked at the network layer and cannot be re-enabled by policy, and documents
**no Windows-specific caveat**.

Two conclusions follow, and both matter to Boks:

- **The reference product's guarantee does not weaken on Windows.** It is one implementation on
  three platforms, not a strong Unix story and a degraded Windows one.
- **A `transparent` flow on Windows does not imply kernel interposition.** The "network layer"
  in that phrase is Docker's own stack in ring 3. This is worth stating because it is the
  natural wrong inference, and it is the one that led this document astray in its first pass.

### Requirements, and what they rule out

Docker's documented Windows requirements are: x86-64, Windows 11, and the Hypervisor Platform
feature enabled —

```powershell
Enable-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform -All
```

— and, explicitly, **Docker Desktop is not required**. WSL2 is not listed. The Hyper-V role is
not listed. `HypervisorPlatform` is the WHP feature: the third-party-VMM API, not the Hyper-V
management stack.

That disposes of the hypothesis that sandboxes are nested inside the Docker Desktop WSL2
utility VM. There is no such VM to nest in when Docker Desktop is absent, and a nested Linux
microVM would be created by KVM ioctls from an ELF binary, not by a Windows PE calling
`WHvCreatePartition`. Docker's maintainers say the same directly: installing the Linux build
inside WSL is supported only "best-effort", and the native Windows build is the preferred path.

### What this costs the LCOW analysis

Sections 1–5 remain accurate and are now mostly irrelevant:

- The **LCOW deprecation, the missing UVM boot files and the absent CI** (section 1) no longer
  block anything, because nothing needs LCOW.
- **9p and its case-insensitivity** (section 3) do not apply; a WHP VMM would use virtiofs, as
  Docker's does.
- The **HCS "no socket-backed NIC" finding** (section 5) is true and is not a limit on Windows.
  It is kept because it is exactly the reasoning that produces the wrong answer, and the next
  person to look at this will find `NetworkAdapter{EndpointId, …}` and reach the same false
  conclusion unless the record says why it does not apply.

What survives untouched is **section 4** — the workspace path problem is a property of Windows
path grammar, not of any VM backend, and `/c/...` is what the reference product does regardless
— and **section 6**, since a supervisor's liveness primitive does not care which hypervisor is
underneath.

---

## 8. What a VMM would have to provide, and how little Boks would have to change

Section 7 reduces the native-Windows question to one thing: **a VMM that runs on the Windows
Hypervisor Platform and gives Boks the guest's Ethernet frames.** This section states the
requirement precisely and measures the Boks-side cost, which turns out to be very small.

### The requirement, in order of how hard it is to satisfy

1. **Runs on Windows against WHP**, and boots an arbitrary Linux kernel and rootfs.
2. **Exposes virtio-net with a backend a separate process can own.** A NAT'd or host-bridged
   NIC is useless here — Boks needs the frames, not connectivity. This is the load-bearing
   requirement and the one that eliminates most candidates.
3. **Shares a host directory into the guest**, ideally virtiofs.
4. **Is drivable one-VM-per-container**, ideally by a containerd shim, so Boks' orchestration
   layer is unchanged.

### The Boks side is nearly free, and this is measured

Two findings from reading Boks' own code and gvisor-tap-vsock's, and they are better than
expected.

**The link format Boks needs is already implemented, and it is a stream.** gvisor-tap-vsock's
`tap.Switch.Accept` takes a `net.Conn` and a protocol constant; the `qemu` protocol is a plain
4-byte big-endian length prefix per frame, and declares `Stream() bool { return true }`
(`pkg/tap/protocols.go`). Boks' stack calls:

```go
return h.sw.Accept(ctx, conn, types.VfkitProtocol)   // stack_unix.go
```

**Supporting a QEMU-style link is that constant plus a listener.** Everything above the link —
the forwarder, the policy hook, DHCP, DNS, the proxy — is untouched.

**And the transport can be TCP, which sidesteps the Windows socket problem entirely.**
gvisor-tap-vsock documents QEMU with `-netdev socket,connect=127.0.0.1:1234` against
`gvproxy -listen-qemu tcp://…`, and its generic listener supports the `tcp` scheme on every
platform. So the fact that Windows' AF_UNIX has no `SOCK_DGRAM` — which looked like a blocker
in section 6 — stops mattering: a stream protocol over loopback TCP works on Windows today.

That leaves the VMM as the only genuinely missing piece, which is the point of this section.

**One security consequence to design for, not to discover.** A loopback TCP link is reachable
by *any* local process, unlike a UNIX socket in a mode-0700 directory. Whoever implements this
must close that: bind to `127.0.0.1` on an ephemeral port, and authenticate or otherwise
constrain the peer. Today Boks relies on filesystem permissions for this and it is invisible
because it is free; on TCP it would have to be deliberate. A Windows named pipe with a
restrictive DACL is the idiomatic alternative and keeps the stream shape.

### The candidates

Assessed against requirement 2, which is where they are decided. **This assessment is the least
settled part of the document** — it is the live question, and it should be treated as a
starting point for the Part B checklist rather than a conclusion.

| Candidate | Runs on Windows/WHP | virtio-net backend Boks could own |
|---|---|---|
| **libkrun** — what Boks uses today | **No.** KVM and Hypervisor.framework only | n/a |
| **cloud-hypervisor** | No. Its `mshv` backend is for Linux running on Microsoft Hypervisor, not Windows userland | n/a |
| **Firecracker** | No. Linux/KVM only | n/a |
| **QEMU** with the WHPX accelerator | Yes in principle — `-accel whpx` is upstream | **Yes** — `-netdev socket` / `-netdev stream` with `virtio-net-pci`, which is exactly the integration gvisor-tap-vsock documents |
| **crosvm** | Has a Windows port | unassessed |
| **OpenVMM** (Microsoft, Rust, open source) | Targets Windows via WHP | unassessed |

**QEMU + WHPX is the obvious first thing to try**, because it is the only candidate where
requirement 2 is already known to be satisfied *and* already known to work with the exact
library Boks uses. Its weaknesses are equally obvious and should be checked before anyone
invests: WHPX's maintenance status and performance, whether a full QEMU is an acceptable
dependency for a tool that currently embeds its VMM, and the absence of a containerd shim
driving QEMU in the shape Boks wants — requirement 4, where libkrun-plus-nerdbox is doing a
lot of work for Boks today that QEMU alone would not.

**OpenVMM is the most interesting unknown.** A Microsoft-authored, open-source, Rust VMM
targeting WHP is on paper the closest thing to an open `sailor.dll`. If it satisfies
requirement 2 it is likely the right answer; if it does not, that is worth knowing early.

---

## The architecture as it would be

The shape that follows from section 7, i.e. the one the reference product demonstrates.
Recorded so the plan exists, **not** as a claim that it works.

```
boks CLI
    |
    v
containerd (Windows, named pipe \\.\pipe\containerd-containerd)
    |
    +-- snapshotter producing a guest-mountable rootfs (erofs, as today)
    |
    v
containerd-shim-nerdbox-v1.exe         -- one shim per sandbox
    |
    v
a VMM on the Windows Hypervisor Platform    <-- THE GAP: Boks has none
    +-- guest memory mapped into the VMM's own process (WHvMapGpaRange)
    +-- devices emulated in user mode (WHvEmulatorTry{Mmio,Io}Emulation)
    +-- workspace via virtiofs
    +-- virtio-net, backend owned in user space
              |
              v
    Boks' gvisor netstack + policy engine + proxy   (unchanged from Linux/macOS)
```

Every box except one is either already built or a known port. What changes in Boks, by package:

| Package | Change | Size |
|---|---|---|
| **the VMM** | **does not exist for Windows** — see section 8 | the entire question |
| `internal/network` gateway | a Windows link: the VMM's virtio-net backend, whatever form it takes. Note Windows AF_UNIX is stream-only, so the `unixgram` transport cannot be reused as-is | small once a VMM exists, and shaped by which one |
| `internal/enforce` | `LockFileEx` with a retry, and `CREATE_NEW_PROCESS_GROUP` in place of `setsid` | small (section 6) |
| `internal/workspace` | Windows host path → `/c/…`; refuse UNC | small — the type already separates `HostPath` from `GuestPath`, so nothing downstream changes |
| `internal/runtimecfg` | containerd's Windows named-pipe address is already handled; the runtime handler stays `io.containerd.nerdbox.v1` if the shim is the same family | trivial |
| `internal/doctor` | Windows prerequisite checks: the Hypervisor Platform feature, containerd, the shim, the VMM | small |
| `internal/network` stack, `internal/proxy`, `internal/policy`, `internal/secret`, `internal/ca` | **unchanged** | none |

Two measurements support that last row, and both were taken during this spike:

- **The netstack is already portable.** `internal/network/stack_unix.go` imports no
  platform-specific package and makes no syscall; it carries `!windows` only because the gateway
  that feeds it does. Removing the build tag and compiling the package for `windows/amd64`
  succeeds — gvisor's netstack, the tap switch, and the DHCP and DNS services all build. The
  enforcement engine is not the obstacle. (Demonstrated, then reverted; it says nothing about
  whether a guest could reach it.)
- **The reference product reaches the same conclusion independently.** Its Windows build links
  the same `gvisor` and `goproxy` versions as its macOS build, and carries all three proxy
  modes. The policy layer is genuinely platform-independent in practice, not just in principle.

---

## What was built, and what was not

**Built** — message accuracy only, no new capability:

- `internal/doctor/virt_windows.go`: a Windows-specific `virtualization` check whose failure
  says the platform is not the obstacle and names the missing VMM. It deliberately probes for
  nothing — offering a prerequisite checklist would imply that completing it leads somewhere,
  and today there is no Boks Windows backend to enable.
- `internal/doctor/doctor.go`: the platform check's Windows remedy no longer says "blocked on
  runtime support".
- `internal/network/gateway_windows.go`, `internal/enforce/lock_windows.go`: the errors and
  comments now name WHP and the missing VMM, record the HCS and AF_UNIX findings as *true but
  not load-bearing* so they are not rediscovered as blockers, and carry the `LockFileEx` design
  for whoever implements it.

**Built** — one real capability, on the Linux path, because the WSL2 route is the one users can
actually take:

- `internal/doctor/wsl_linux.go` and the WSL branches in `virt_linux.go`: `doctor` detects WSL
  via `/bin/wslinfo` and splits a KVM failure into the three causes that are distinguishable
  from inside a distribution — nested virtualisation genuinely off, the module merely not
  loaded, or the device node left `root:root 0600` because WSL runs no udev. Each gets its own
  remedy. This replaces a generic message whose first suggestion — enable nested
  virtualisation — is, on Windows 11, usually the *wrong* one.
- Tested: the CPU-flag discriminator against synthetic `/proc/cpuinfo`, and assertions that the
  remedies keep the two warnings most easily lost in an edit (that `chmod 666` is dangerous, and
  that nested virtualisation is already on by default). **The logic is tested; the values it
  reads have never been read on a real WSL system.**

Each of these was written once against the LCOW conclusion and rewritten when section 7
overturned it. That is worth noting because the first version would have been a confidently
worded, well-sourced, wrong error message — the exact failure mode this project's documentation
standard exists to prevent.

**Not built, on purpose:**

- **No Windows link transport.** Its shape depends on which VMM, and there is no VMM.
- **No `LockFileEx` supervisor liveness.** A correct primitive with no caller (section 6).
- **No VMM.** Section 8 is an assessment, not a plan of record.
- **No new dependencies.** `hcsshim` and `linuxkit/virtsock` remain indirect. Nothing here
  needed to import either; the findings came from reading them.

Linux and macOS behaviour is untouched. `gofmt`, `go vet`, `go test ./... -count=1` are clean,
and `linux/amd64`, `darwin/arm64` and `windows/amd64` all build.

---

## What is unknown

Ranked by how much it would change the conclusion.

1. **Whether `/dev/kvm` is actually usable in WSL2 on an AMD machine.** Cheapest to answer,
   and it decides whether the route this document now leads with covers most Windows
   developers or only those on Intel. Nothing else here is both this consequential and this
   easy to settle.
2. **Whether an open-source WHP-capable VMM exists that can do virtio-net with a backend Boks
   can own.** The entire native-port question; section 8 assesses it and does not close it.
3. **Whether the nerdbox shim's Windows support is upstream or Docker's own.** Docker ships
   `containerd-shim-nerdbox-v1.exe`; whether that comes from `containerd/nerdbox` or a private
   fork decides whether Boks could use it or would have to port it.
4. **What form a WHP VMM's virtio-net backend would take** — a host socket, a callback, a
   named pipe — and therefore what the Windows gateway looks like.
5. Whether that VMM would be a separate process or linked in, which decides whether the
   supervisor's lifetime argument survives unchanged.
6. **Whether the whole WSL2 route works end to end.** Every ingredient is sourced; the
   combination has never been run.

The LCOW-specific unknowns are retired rather than answered: where to obtain UVM boot files,
whether the LCOW kernel has TUN, whether LCOW works at current versions, whether its Windows
Server 2025 requirement covers Windows 11, and the 9p share limit. None of them matters unless
someone deliberately chooses the LCOW path, and section 7 is the argument for not doing that.

Two questions open when this document was first written have been answered and moved into the
sections above: the workspace path inside a Windows-hosted `sbx` sandbox (section 4 — `/c/...`),
and how Docker Sandboxes enforces policy on Windows (section 7).

---

## Verification checklist

For someone with a Windows 11 machine and hardware virtualisation. Written in the spirit of
[verification.md](verification.md): each step says what would count as evidence, and none of it
has been run.

Four parts, in descending order of value:

- **Part D (steps 11–13) — Boks inside WSL2.** The only steps here that could produce a
  *working* Boks sandbox on a Windows machine, and the cheapest to run. Start here.
- **Part A (steps 1–4) — corroborate section 7.** That section reverses this document's
  original verdict on the strength of an import table; an afternoon would make it an
  observation.
- **Part B (steps 5–7) — evaluate a candidate VMM.** The native-port question.
- **Part C (steps 8–10) — LCOW.** Kept only for anyone who wants to confirm the cul-de-sac is
  one.

Parts A, B and C **will not produce a working Boks sandbox**; Boks has no native Windows
backend, and none of these steps give it one. They establish whether one is worth building.

### Part A — corroborate what the reference product does

1. **Confirm the platform requirement is WHP, not Hyper-V.** On a clean Windows 11 with the
   Hyper-V *role* disabled and only the Hypervisor Platform feature enabled:

   ```powershell
   Get-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform, Microsoft-Hyper-V-All |
       Select FeatureName, State
   Get-Service vmms -ErrorAction SilentlyContinue
   ```

   Evidence: `sbx` runs a sandbox with `HypervisorPlatform` enabled and
   `Microsoft-Hyper-V-All` disabled, and with the `vmms` service absent or stopped. **That is
   the single cleanest confirmation** that the Hyper-V management stack — and therefore HCS,
   HNS and the virtual switch — is not in the path.

2. **Confirm no kernel driver and no privileged service.**

   ```powershell
   Get-ChildItem -Recurse "$env:LOCALAPPDATA\DockerSandboxes" -Include *.sys
   Get-Service | Where-Object { $_.DisplayName -match 'Docker|Sandbox' }
   fltmc filters
   ```

   Evidence: no `.sys`, no service, no filter driver attributable to the product.

3. **Watch a sandbox's traffic actually being judged.** Run a workload under a restrictive
   policy and inspect the decision log for a `transparent` flow — a raw TCP connection judged
   without the proxy.

   Evidence: a denial recorded for a direct-to-IP connection. That proves userspace termination
   end to end, since nothing in the kernel is positioned to do it.

4. **Confirm the workspace path convention.**

   ```
   sbx run --workspace C:\Users\<you>\src\foo <agent> -- sh -c "pwd; mount; ls /sys/bus"
   ```

   Evidence: `pwd` prints `/c/Users/<you>/src/foo`; `mount` shows **virtiofs**, not 9p; and
   `/sys/bus` contains `virtio`. A 9p mount or a `vmbus` topology would contradict section 7
   and should be reported.

### Part B — can a candidate VMM give Boks what it needs?

For whichever VMM section 8 identifies, the questions are the same three, in order. Stop at the
first failure; each is a hard requirement.

5. **Does it run on Windows via WHP, and boot an arbitrary Linux kernel and rootfs?**
   Evidence: a Linux guest reaching userspace on a machine with the Hyper-V role disabled.

6. **Does it expose virtio-net with a backend Boks can own?** This is the load-bearing
   question and the reason for the whole exercise. A NAT'd or host-bridged NIC is **not**
   sufficient — Boks needs the guest's Ethernet frames delivered to a Boks process, as a
   socket, a pipe or an in-process callback.
   Evidence: frames from the guest arriving in a userspace program that is not the VMM, or a
   documented API that would allow it.

7. **Can it be driven by a containerd shim, one VM per container?**
   Evidence: either an existing shim, or an embeddable API rather than a CLI-only VMM.

If step 6 fails for every candidate, the honest conclusion is that Boks' Windows story depends
on writing a VMM or on an upstream project adding a backend — and that should be stated as
plainly as the rest of this document.

### Part C — the LCOW path (for completeness only)

Sections 1–5 argue this is the wrong target. These steps confirm that rather than pursue it,
and the most likely outcome is stopping at step 9.

8. **Enable Hyper-V and install containerd, then confirm the LCOW plugins initialised.**

   ```powershell
   Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V-All
   ctr plugins ls | Select-String "lcow"
   Get-Command containerd-shim-runhcs-v1.exe
   ```

   Evidence: `io.containerd.snapshotter.v1  windows-lcow` with an empty error column, and a
   path for the shim. A plugin listed with an init error is the interesting failure and should
   be recorded verbatim. Note that containerd's PATH is the service's, not the shell's — the
   same trap `doctor` already warns about on Unix.

9. **Find the boot files.** This is the step most likely to stop everything.

   ```powershell
   Get-ChildItem "$env:ProgramFiles\Linux Containers"
   ```

   Evidence: `kernel` or `vmlinux`, plus `initrd.img` or `rootfs.vhd`. **If this directory does
   not exist, record where you obtained the files, or that you could not.** That answer is the
   single most valuable output of this checklist.

### Does the runtime half work?

5. **Boot an arbitrary Linux image in a utility VM.**

   ```powershell
   ctr run --rm --snapshotter windows-lcow --runtime io.containerd.runhcs.v1 `
       docker.io/library/alpine:latest lcowtest sh -c "uname -a; cat /proc/version"
   ```

   Evidence: Linux output on a Windows host. As on macOS, host and guest kernels differ in
   *kind*, so a shared-kernel container cannot explain it. Record the kernel version — it
   identifies which UVM image is in use.

6. **Confirm the VM boundary with the criteria verification.md already defines.**

   ```powershell
   ctr run --rm --snapshotter windows-lcow --runtime io.containerd.runhcs.v1 `
       docker.io/library/alpine:latest lcowid sh -c `
       "cat /proc/sys/kernel/random/boot_id; cat /proc/uptime; nproc; ls /sys/bus/vmbus/devices"
   ```

   Evidence: a `boot_id` unique per run and an uptime in seconds. Note the device path is
   `/sys/bus/vmbus/devices`, **not** `/sys/bus/virtio/devices` — a Hyper-V guest has VMBus
   synthetic devices, and expecting virtio here would produce a false negative.

7. **Share a host directory and find out where it lands.**

   ```powershell
   mkdir C:\boksprobe\deep\a\b\c\project
   echo hello > C:\boksprobe\deep\a\b\c\project\marker.txt
   ctr run --rm --snapshotter windows-lcow --runtime io.containerd.runhcs.v1 `
       --mount type=bind,src=C:\boksprobe\deep\a\b\c\project,dst=/c/boksprobe/deep/a/b/c/project,options=rbind:rw `
       docker.io/library/alpine:latest lcowmnt sh -c "pwd; ls -la /c/boksprobe/deep/a/b/c/project; mount | grep 9p"
   ```

   The destination spelling here is the `/c/...` convention section 4 recommends; the point of
   the step is partly to confirm the runtime accepts an arbitrary guest path at all.

   Evidence: `marker.txt` visible, and a `9p` line in `mount` confirming the transport. Then
   the three questions that decide sections 3 and 4:

   - **Are intermediate directories created automatically?** `ls /c/boksprobe/deep` should
     contain only the next component, as on Linux and macOS.
   - **Is the share case-insensitive?** `ls /c/BOKSPROBE/...` succeeding is the answer, and
     it is expected to succeed. Then, inside the workspace:
     `touch Makefile makefile; ls` — one file rather than two confirms the problem is real for
     a Linux toolchain.
   - **Does a write reach the host promptly?** `touch` in the guest, `Get-ChildItem` on the
     host.

   Also worth timing here: `git status` on a large repository, host versus guest, to quantify
   the 9p cost.

10. **Confirm the LCOW network dead end** — the step that makes this path a cul-de-sac rather
    than an option.

    ```powershell
    ctr run --rm --snapshotter windows-lcow --runtime io.containerd.runhcs.v1 `
        docker.io/library/alpine:latest lcownet sh -c "ip addr; ip route; ls -l /dev/net/tun"
    ```

    Expected: a NIC backed by an HNS endpoint with a route out, and **no** `/dev/net/tun`.
    That confirms the frames are on the Windows vSwitch with no userspace process in the path,
    and that the guest-agent construction in section 5 is not available either.

    A Boks port built on this could offer `-net none` and nothing else. That is why section 7
    exists.

### Part D — Boks inside WSL2

The cheapest and highest-value experiment in this document, and the only one that could produce
a working Boks sandbox on a Windows machine.

11. **Confirm `/dev/kvm`.** In a WSL2 distro on Windows 11:

    ```bash
    cat /proc/sys/kernel/osrelease      # identifies the WSL kernel
    ls -l /dev/kvm                      # exists? owner, group, mode?
    lsmod | grep kvm
    ```

    **Run this on an AMD machine specifically.** Nested virtualisation is documented as
    vendor-agnostic on Windows 11, but the empirical record is thinner for AMD than for Intel,
    and it decides how many users this answer actually covers. Record the CPU vendor either way.

12. **Run `boks doctor`, then a sandbox.** Every check should pass exactly as on any Linux host.
    Then boot a sandbox and apply the evidence criteria in [verification.md](verification.md) —
    `boot_id`, uptime, `nproc` — which are unchanged, because this *is* Linux.

    Evidence: a sandbox whose `boot_id` differs from the WSL2 distro's. Note that there are now
    two nested boundaries, so record the WSL2 distro's own `boot_id` as well as the host's.

13. **Confirm the workspace invariant survives.** With a workspace in the WSL2 filesystem:

    ```bash
    boks run shell ~/src/foo -- pwd
    ```

    Expected: `/home/<you>/src/foo`, exactly. **Section 4 does not apply here**, and confirming
    that is the point — it is the concrete advantage of this route over a native port.

    Then repeat with a workspace under `/mnt/c` and time `git status` on a large repository
    both ways, to quantify the 9p cost that makes "keep workspaces in the WSL2 filesystem" the
    standing advice.

### Corroborate the Docker Sandboxes findings

14. If Docker Sandboxes is installed, one command corroborates section 4 and section 7 at once:

    ```
    sbx run --workspace C:\Users\<you>\src\foo <agent> -- sh -c "pwd; mount; ls /sys/bus; ip -o addr"
    ```

    Expected, from the evidence in those sections: `pwd` prints `/c/Users/<you>/src/foo`, and
    the workspace arrives over **virtiofs**, not 9p.

    Note what `/proc/version` does *and does not* tell you here. Inside the sandbox it reports
    the **microVM's own kernel**, so it identifies neither the host nor the substrate — a
    nested-in-WSL2 design and a native Hyper-V design both show a kernel Docker chose. The
    discriminator is the **device topology**: `ls /sys/bus` showing a `virtio` bus means a
    Linux-hosted microVM, while `vmbus` synthetic devices and a 9p mount would mean an LCOW
    utility VM.

    **A 9p mount here would contradict section 7 and should be reported**, since it would mean
    `sbx` is on LCOW after all and is solving the network problem some way this spike missed.

    To identify the substrate rather than the guest, look from the Windows side instead:
    `wsl -l -v` for the distributions in play, and Task Manager / `Get-VM` for whether a
    separate Hyper-V VM exists per sandbox.

### Recording results

As in verification.md: record the host build (`winver`), **the CPU vendor**, the WSL version
and kernel, the containerd version, and the verbatim output of each step.

**A step that fails is a result, not a blocked task.** The three most useful outcomes this
checklist can produce, in order:

1. **Step 11 on an AMD machine** — either result. If `/dev/kvm` is there, the WSL2 section covers
   most Windows developers and Boks has a Windows answer today. If it is not, the WSL2 section covers
   Intel only and should say so.
2. **Step 1** — `sbx` running with the Hyper-V role disabled. That single observation converts
   section 7 from static analysis into fact and retires sections 1–5 for good.
3. **Step 6** — a candidate VMM either does or does not expose a virtio-net backend Boks can
   own. That is the whole native-port question, and a negative is as valuable as a positive
   because it redirects the effort to upstream work.

---

## Sources

### Source trees read directly

These are the strongest evidence in the document: the exact module versions Boks already
depends on, read out of the local module cache rather than quoted from a summary.

- `github.com/Microsoft/hcsshim` **v0.14.1** — `internal/uvm/{plan9,share,network,create_lcow,hvsocket}.go`,
  `internal/hcs/schema2/{devices,network_adapter}.go`, `internal/hcsoci/resources_lcow.go`,
  `internal/layers/lcow.go`, `internal/guest/`, `pkg/annotations/`, `internal/annotations/`,
  `Makefile.bootfiles`, `README.md`, `.github/workflows/ci.yml`
- `github.com/containerd/containerd/v2` **v2.2.6** — `plugins/snapshots/lcow/`,
  `plugins/diff/lcow/`, `defaults/defaults_windows.go`, `plugins/types.go`,
  `cmd/ctr/commands/run/run_windows.go`, `.github/workflows/windows-hyperv-periodic.yml`,
  and the absence of any `lcow` reference under `.github/`
- `github.com/containers/gvisor-tap-vsock` **v0.8.9** — `pkg/transport/{listen,listen_windows,unixgram_windows}.go`,
  `cmd/vm/main_linux.go`, `README.md`
- `github.com/linuxkit/virtsock` — `pkg/hvsock/{hvsock.go,hvsock_windows.go}`

### LCOW status (section 1)

- [LCOW deprecated on Windows Server](https://learn.microsoft.com/en-us/virtualization/windowscontainers/deploy-containers/linux-containers)
  (Microsoft Learn, updated 2026-02-12)
- [Set up Linux containers on Windows](https://learn.microsoft.com/en-us/virtualization/windowscontainers/quick-start/quick-start-windows-10-linux) —
  Microsoft's recommended path is Docker Desktop on WSL2
- [Docker deprecated features](https://docs.docker.com/engine/deprecated/) — LCOW deprecated
  v20.10, removed v23.0; [moby/moby#42451](https://github.com/moby/moby/pull/42451) removed it
- [containerd#6313](https://github.com/containerd/containerd/issues/6313) — "Unable to run linux
  container on windows using ctr and lcow", closed as **not planned**
- [containerd#9822](https://github.com/containerd/containerd/issues/9822) — Windows snapshotters
  undocumented, open for years
- [hcsshim#2667](https://github.com/microsoft/hcsshim/issues/2667) — the 2026 shim split, LCOW
  implemented first
- [Parma: confidential containers via attested execution policies](https://arxiv.org/abs/2302.03976) —
  Microsoft Research; `pkg/securitypolicy` is what powers Confidential Azure Container Instances
- [linuxkit/lcow](https://github.com/linuxkit/lcow) — archived March 2020, last release
  v4.14.35-v0.3.9 (November 2018)

### Storage and networking mechanics (sections 3 and 5)

- [Hyper-V Extensible Switch](https://learn.microsoft.com/en-us/windows-hardware/drivers/network/overview-of-the-hyper-v-extensible-switch)
  and [Filtering Extensions](https://learn.microsoft.com/en-us/windows-hardware/drivers/network/filtering-extensions) —
  interposition on the vSwitch is kernel-mode only
- [AF_UNIX comes to Windows](https://devblogs.microsoft.com/commandline/af_unix-comes-to-windows/) —
  `SOCK_STREAM` only, no `SOCK_DGRAM`
- [Make an integration service](https://learn.microsoft.com/en-us/virtualization/hyper-v-on-windows/user-guide/make-integration-service) —
  hvsock service registration
- [hcsshim#464](https://github.com/microsoft/hcsshim/issues/464) — 9p cache coherency between
  two shares of one directory
- [containers/podman](https://github.com/containers/podman) `pkg/machine/hyperv/` — the working
  `gvforwarder`-in-guest deployment on Hyper-V

### The workspace path convention (section 4)

- [docker/sbx-releases#215](https://github.com/docker/sbx-releases/issues/215) and
  [#31](https://github.com/docker/sbx-releases/issues/31) — `sbx` maps `C:\…` to `/c/…`
- [Docker Desktop volume documentation](https://docs.docker.com/desktop/troubleshoot-and-support/troubleshoot/topics/) —
  `-v /c/Users/user/work:/work` as documented CLI input
- [Compose `COMPOSE_CONVERT_WINDOWS_PATHS`](https://docs.docker.com/compose/how-tos/environment-variables/envvars/)
- `docker/machine` `drivers/virtualbox` — the `s!^([a-z]+):[/\\]+!\1/!` conversion, and
  boot2docker mounting each share at `/$name`
- [rancher-desktop#6630](https://github.com/rancher-sandbox/rancher-desktop/issues/6630) — the
  `/c/` versus `/mnt/c/` confusion and the symlink workaround
- [Lima](https://github.com/lima-vm/lima) `mountPoint` defaulting to the host `location` — the
  exact-path approach, and its absence of a Windows convention

### What the reference product does (section 7)

The strongest evidence in this document, because it is what a working implementation actually
links against rather than what anyone says about it.

- `DockerSandboxes.msi` **v0.38.0**, from
  [docker/sbx-releases releases](https://github.com/docker/sbx-releases/releases) — unpacked;
  MSI `File` table, absence of `ServiceInstall`/`ServiceControl`/driver tables, per-user
  `%LOCALAPPDATA%` install root, and the import tables and strings of `sailor.dll` and
  `sbx.exe`
- [Windows SBOM](https://github.com/docker/sbx-releases/releases/download/v0.38.0/DockerSandboxes-windows-amd64.sbom.json)
  and [macOS SBOM](https://github.com/docker/sbx-releases/releases/download/v0.38.0/DockerSandboxes-darwin-arm64.sbom.json) —
  identical `gvisor` and `goproxy` versions across the two platforms
- [Docker Sandboxes prerequisites](https://docs.docker.com/ai/sandboxes/get-started/) —
  Windows 11 + `HypervisorPlatform`, Docker Desktop **not** required, WSL2 not listed
- [Docker Sandboxes architecture](https://docs.docker.com/ai/sandboxes/architecture/) and
  [local policy](https://docs.docker.com/ai/sandboxes/security/policy/) — enforcement model,
  UDP/ICMP blocked at the network layer, no Windows caveat
- [Why MicroVMs: the architecture behind Docker Sandboxes](https://www.docker.com/blog/why-microvms-the-architecture-behind-docker-sandboxes/) —
  "each OS's native hypervisor: Apple's Hypervisor.framework, Windows Hypervisor Platform, and
  Linux KVM… a single codebase for three platforms"
- [sbx-releases#397](https://github.com/docker/sbx-releases/issues/397) — Docker's maintainer:
  the native Windows build is preferred; the Linux build in WSL is "best-effort"
- [Windows Hypervisor Platform API](https://learn.microsoft.com/en-us/virtualization/api/hypervisor-platform/hypervisor-platform) —
  a user-mode API for third-party virtualization stacks; guest physical memory is "populated
  using memory allocated in the user-mode process of the virtualization stack"; it exposes no
  networking device of any kind

### Boks inside WSL2

- [microsoft/WSL releases](https://github.com/microsoft/WSL/releases) — kernel versions;
  note that Learn's [kernel release notes](https://learn.microsoft.com/en-us/windows/wsl/kernel-release-notes)
  are **stale** (newest entry 5.15.57.1, 2022) and should not be cited
- WSL kernel config: `arch/x86/configs/config-wsl` on the `linux-msft-wsl-6.18.y` branch of
  [microsoft/WSL2-Linux-Kernel](https://github.com/microsoft/WSL2-Linux-Kernel) — `CONFIG_KVM=m`,
  `CONFIG_EROFS_FS=m`, `CONFIG_UNIX=y`. Note `master` is still the legacy 4.19 branch, and on
  6.x the old `Microsoft/config-wsl` path is a symlink that naive raw fetches return literally
- [microsoft/WSL#7257](https://github.com/microsoft/WSL/issues/7257) — the request that got
  EROFS enabled (Microsoft, 2022)
- [microsoft/wsl#40573](https://github.com/microsoft/wsl/issues/40573) — a custom kernel
  without a matching modules VHD loses every `=m` symbol
- [microsoft/WSL#5272](https://github.com/microsoft/WSL/issues/5272) — Windows' native AF_UNIX
  has no `SOCK_DGRAM`, open since 2020
- [microsoft/WSL#7149](https://github.com/microsoft/WSL/issues/7149) — a Microsoft engineer
  confirming WSL runs no udev, which is why `/dev/kvm` stays `root:root 0600`
- WSL source, `WslCoreConfig.h` — `"wsl2.nestedVirtualization"` and
  `"wsl2.loadKernelModules"` as config keys, and
  `EnableNestedVirtualization = !shared::Arm64 && IsWindows11OrAbove()`, i.e. **on by default**
- [WSL configuration settings](https://learn.microsoft.com/en-us/windows/wsl/wsl-config) —
  `.wslconfig` is global-only; `loadKernelModules` is **not** documented there
- [rancher-desktop#9708](https://github.com/rancher-sandbox/rancher-desktop/issues/9708) —
  containerd-in-WSL startup failure while dockerd works; the containerd path there is less
  trodden

### The supervisor (section 6)

- [LockFileEx](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-lockfileex) —
  the release-on-death promptness caveat, quoted in section 6
- [CreateMutexW](https://learn.microsoft.com/en-us/windows/win32/api/synchapi/nf-synchapi-createmutexw) —
  recommends a locked file in the user profile instead of a named mutex
- Go `cmd/go/internal/lockedfile/internal/filelock` — the `LockFileEx` reference
  implementation, and `github.com/gofrs/flock` for the error-code handling
