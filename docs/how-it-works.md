# How it works

A short tour of what happens when you type `boks run`, and why the parts are arranged the
way they are. It is the middle level of detail: more than the front page, less than
[Architecture](architecture.md), which is the reference page for the same material.

## A sandbox is a virtual machine

```
boks CLI → containerd → containerd-shim-nerdbox-v1 → libkrun → microVM
                                                                 ├── workspace via virtiofs
                                                                 └── your command
```

Boks is the orchestration layer, not a hypervisor. It uses containerd's Go API to create a
container whose runtime handler is `io.containerd.nerdbox.v1`; that handler boots a microVM
and runs the container inside it. Everything below the CLI in that chain is existing
open-source software — containerd, the nerdbox shim, and libkrun, which drives KVM on Linux,
Hypervisor.framework on macOS and the Windows Hypervisor Platform on Windows.

The practical consequence is that the guest is not sharing your kernel. It reports its own
kernel version, its own `boot_id`, its own uptime, and vCPU and memory counts that follow the
flags you passed rather than the machine you are sitting at. A shared-kernel container can
produce none of that, which is what makes the boundary checkable rather than merely claimed —
see [Verification](verification.md).

**Assume code in the sandbox is hostile.** The hypervisor is the boundary; permissions
*inside* the guest are not — the guest is disposable, and its whole filesystem is the
agent's to ruin. The agent runs as uid 1000 rather than root, which is hygiene rather
than containment.

## The workspace is the only thing it can see

You name directories on the command line, and those directories — and nothing above them —
are shared into the guest over virtiofs, at the same absolute path they have on the host:

```bash
boks run claude ~/src/foo ~/src/lib:ro
```

The exact-path arrangement exists so that a tool which prints, logs or hard-codes a path
produces a path that means the same thing on both sides. A `:ro` suffix shares a directory
read-only, and a symlink inside the workspace that points at `/etc` or `~/.ssh` resolves
inside the guest rather than reaching your host's copy.

Writes go straight through to your real files. That is the point — it is how you get work
out — but it also means a sandbox can change a `Makefile`, a `package.json` script, a Git
hook or a CI config that you then run on your own machine. Review diffs from a workspace a
sandbox has touched. More in [Workspaces](usage.md#workspaces).

## The network is judged on the host

A sandbox gets a virtual NIC whose far end is a userspace network stack running in a Boks
process on the host. Every TCP connection the guest opens is evaluated there, by address and
port, *before* anything is dialled.

That placement is the whole design. A guest that ignores the proxy environment variables, or
opens a raw socket, is judged rather than unfiltered — the decision does not depend on the
guest cooperating. UDP and ICMP are dropped at the link, except DNS to the sandbox's own
resolver. `--net none` gives the sandbox no network at all.

Policy is state rather than an argument: `boks policy allow github.com:443` outlives the
command that wrote it, and a sandbox remembers what it was created with, so a later
`boks start` serves the same containment. Every decision, allowed or denied, is recorded in
`boks policy log`. More in [The network a sandbox gets](usage.md#the-network-a-sandbox-gets).

## Credentials stay on the host

The guest is given a placeholder in whatever environment variable its tooling reads. The
real secret never enters the sandbox: it lives on the host and is attached to outbound
requests to the hosts that credential is for.

```bash
boks secret set anthropic
```

The name is the whole configuration. Boks already knows each supported vendor's hosts, header
and key shape, so there is nothing to look up. The hosts a credential is attached to — and
only those — have their TLS terminated so the header can be added, and `boks run` says which
ones the first time a sandbox meets them. Everything else is tunnelled with the origin's own
certificate chain. More in
[Credentials](usage.md#credentials-the-name-is-the-configuration).

## Sandboxes persist, and the name is the identity

A sandbox is named after the agent and the workspace directory — `shell-boks` for the `shell`
agent in `~/git_repos/boks` — and that derived name is what a second run looks up. So running
the same agent in the same directory puts you back where you left off, with packages, caches
and shell state intact, until `boks rm` removes it.

`boks stop` keeps everything inside; a later `boks start` boots a *new* VM over the same
writable snapshot, which is why the `boot_id` changes while your files do not. More in
[Sandboxes persist](usage.md#sandboxes-persist-and-the-name-is-the-identity).

## Nothing leaves your machine

There is no account, no cloud service and no telemetry. Boks runs no background process on a
host where you have not started one, and it will never mount the host's Docker or containerd
socket into a guest.

## Where to go next

- [Security model](security-model.md) — trust boundaries, and the escape surfaces ranked
- [Architecture](architecture.md) — the same material as reference, layer by layer
- [Verification](verification.md) — what has actually been observed, on which hardware
