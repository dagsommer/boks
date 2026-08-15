# Get started

Boks runs a coding agent inside a microVM of its own, on your machine. This page takes you
from nothing to a shell inside one.

> [!WARNING]
> **Boks is experimental.** The VM boundary and the network policy have each been measured
> against a real guest on macOS, Windows and Linux, and both hold on all three — but the
> Linux run was inside WSL2 rather than on bare metal, a single hypervisor sits behind all of
> it, and plenty is still unbuilt. Read
> [Which platforms work](#which-platforms-work) before you spend an hour on this, and
> [what is not built yet](roadmap.md) before you rely on it.

## Which platforms work

This is the first thing to check, because the answer decides whether the rest of the page is
worth your time.

| Platform | State |
|---|---|
| **macOS on Apple silicon** | **Verified.** The most thoroughly measured platform, and where the VM boundary and the network policy were first established. |
| **Windows 11 on x64** | **Verified**, natively, through the Windows Hypervisor Platform and from an ordinary unelevated terminal. One machine so far; there is no Windows arm64 build. See [Windows](windows.md). |
| **Linux with `/dev/kvm`** | **Verified, with two caveats.** The 2026-08-15 run was in WSL2 rather than on bare metal, and creating a sandbox still needs more privilege than an ordinary user has — expect to run as root for now. |
| **Windows via WSL2** | **Verified** — this is where the Linux run above happened. See [Windows](windows.md) and [Troubleshooting](troubleshooting.md#wsl2). |
| **macOS on Intel** | No. There is no VM backend for it, and none is planned. |

No release has been cut on any platform, so every route means building from source today.

"Verified" here means a specific thing, and [Verification](verification.md) is where the
evidence is: what was observed, on what hardware, on what date, and what each observation
does and does not establish.

## Prerequisites

- **Hardware virtualisation.** Linux with `/dev/kvm`, and membership of the `kvm` group; or
  macOS on Apple silicon, which uses Hypervisor.framework.
- **[containerd](https://containerd.io/) 2.3 or later**, running. Not 2.2: the shim emits
  version-3 bootstrap parameters a 2.2 daemon cannot decode, and task start fails with
  `unsupported protocol: Yunix`.
- **[nerdbox](https://github.com/containerd/nerdbox)** — the VM runtime shim. The binary
  `containerd-shim-nerdbox-v1` must be on *containerd's* `PATH`, which is the daemon's, not
  your shell's.
- **[libkrun](https://github.com/containers/libkrun) 1.18 or later.**
- **`erofs-utils`**, for `mkfs.erofs`.
- **Go 1.26 or later**, to build Boks.

Docker Desktop is not required. There is no account to create and nothing to sign in to.

On macOS there are two further steps that are easy to miss and fail opaquely when skipped —
the shim has to be codesigned with the `com.apple.security.hypervisor` entitlement, and
`/var/run/containerd` has to be writable by you. Both are in
[Verification](verification.md#macos-setup-notes), and `boks doctor` checks the first.

## Install

[Installing](install.md) covers every route — Homebrew, `.deb`/`.rpm`, winget, and building
from source — with the honest status of each. The short version: nothing has been released
yet, so today the working route is building from source:

```bash
git clone https://github.com/dagsommer/boks
cd boks
make build          # builds ./bin/boks
```

The commands below use `./bin/boks`; once an installed `boks` is on your `PATH`, drop the
prefix.

## Check the host

```bash
./bin/boks doctor
```

`doctor` checks every prerequisite above and, for each gap, prints what to do about it rather
than what is wrong. Nothing should be `fail`.

One `warn` is expected forever on a perfectly healthy Mac: `virtualization` cannot be probed
without booting a VM, so on Apple silicon it reports architecture support and says so. On
Linux the same check opens `/dev/kvm` and is either `ok` or `fail`.

If something fails, [Troubleshooting](troubleshooting.md) walks through each check.

## The first run

```bash
./bin/boks run
```

That is a shell, in the current directory, inside a microVM. Nothing else is exposed to it.

```bash
./bin/boks run shell . -- uname -a
```

The guest reports a Linux kernel and its own uptime, because it *is* a different kernel — the
thing that separates this from a container. If you want to establish that for yourself rather
than take it on trust, [Verification](verification.md) has the procedure and the evidence.

Two things are happening in that command, and both are worth knowing before the second one:

- **`shell` is an agent.** The agent comes first and decides what the sandbox contains.
  `boks run claude .` runs Claude Code instead. See [Agents](agents.md).
- **`.` is a workspace.** It is shared into the guest at the same absolute path it has on the
  host, and writes reach your real files. Directories above it are not exposed.

## What to read next

- **[Walkthrough](walkthrough.md)** — a longer first session: prove the sandbox is a VM,
  keep it, give it a policy and a credential, publish a port, clean up.
- **[Usage](usage.md)** — the sandbox lifecycle, workspaces, network policy, credentials and
  ports. If you read one more page, read that one.
- **[Agents](agents.md)** — the ten agents, which have prepared images, and what each one is
  allowed to reach.
- **[Security model](security-model.md)** — what the boundary is, and the two things about it
  that will surprise you.
- **[Roadmap](roadmap.md)** — what is not built yet, in the project's own words.
