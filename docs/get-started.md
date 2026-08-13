# Get started

Boks runs a coding agent inside a microVM of its own, on your machine. This page takes you
from nothing to a shell inside one.

> [!WARNING]
> **Boks is experimental.** The VM boundary and the network policy have both been measured
> against a real guest, and both hold — but this is a young project with one verified
> platform, one verified hypervisor, and plenty still unbuilt. Read
> [Which platforms work](#which-platforms-work) before you spend an hour on this, and
> [what is not built yet](roadmap.md) before you rely on it.

## Which platforms work

This is the first thing to check, because the answer decides whether the rest of the page is
worth your time.

| Platform | State |
|---|---|
| **macOS on Apple silicon** | **Works.** This is the platform the VM boundary and the network policy were measured on. |
| **Linux with `/dev/kvm`** | **Supported but not verified.** The KVM path is designed for and built, and has not been exercised end to end by anyone on this project. Expect to be the first. |
| **macOS on Intel** | No. There is no supported VM backend. |
| **Windows, natively** | **No.** Not "unsupported" — it does not run. libkrun's Windows Hypervisor Platform backend is in progress upstream, and `virtio-net`, the one device Boks' enforcement depends on, is not ported yet. |
| **Windows via WSL2** | **Designed for, never run.** Every ingredient is present in a stock WSL2 and `boks doctor` diagnoses the two things that go wrong, but nobody on this project has executed it. See [Windows](windows.md) and [Troubleshooting](troubleshooting.md#wsl2). |

"Verified" here means a specific thing, and [Verification](verification.md) is where the
evidence is: what was observed, on what hardware, on what date, and what each observation
does and does not establish.

## Prerequisites

- **Hardware virtualisation.** Linux with `/dev/kvm`, and membership of the `kvm` group; or
  macOS on Apple silicon, which uses Hypervisor.framework.
- **[containerd](https://containerd.io/) 2.2 or later**, running.
- **[nerdbox](https://github.com/containerd/nerdbox)** — the VM runtime shim. The binary
  `containerd-shim-nerdbox-v1` must be on *containerd's* `PATH`, which is the daemon's, not
  your shell's.
- **[libkrun](https://github.com/containers/libkrun) 1.18 or later.**
- **`erofs-utils`**, for `mkfs.erofs`.
- **Go 1.26 or later**, to build Boks.

Docker Desktop is not required. Docker Sandboxes is not required. There is no account to
create and nothing to sign in to.

On macOS there are two further steps that are easy to miss and fail opaquely when skipped —
the shim has to be codesigned with the `com.apple.security.hypervisor` entitlement, and
`/var/run/containerd` has to be writable by you. Both are in
[Verification](verification.md#macos-setup-notes), and `boks doctor` checks the first.

## Install

```bash
git clone https://github.com/dagsommer/boks
cd boks
make build          # builds ./bin/boks
```

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

- **[Usage](usage.md)** — the sandbox lifecycle, workspaces, network policy, credentials and
  ports. If you read one more page, read that one.
- **[Agents](agents.md)** — the ten agents, which have prepared images, and what each one is
  allowed to reach.
- **[Security model](security-model.md)** — what the boundary is, and the two things about it
  that will surprise you.
- **[Roadmap](roadmap.md)** — what is not built yet, in the project's own words.
