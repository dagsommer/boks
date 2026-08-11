# Boks Architecture

Status: **design document, partially implemented.** Sections marked *(unverified)* describe
intent that has not yet been demonstrated on real hardware.

## Goals

Boks runs untrusted developer tooling — coding agents in particular — behind a hypervisor
boundary, on your own machine, with no account, no cloud dependency and no telemetry.

The behavioural reference is Docker Sandboxes (`sbx`). Boks is an independent
implementation built from open-source components; it is not derived from Docker's code.

## Layering

```
boks CLI  (cmd/boks)
    |
    v
orchestration  (internal/sandbox)      -- resolves workspace, image, spec; owns lifecycle
    |
    v
containerd     (client-go API)         -- images, snapshots, content, task lifecycle
    |
    v
containerd-shim-nerdbox-v1             -- one shim process per sandbox
    |
    v
libkrun VMM                            -- KVM on Linux, Hypervisor.framework on macOS
    |
    v
microVM: own kernel, own PID space
    +-- workspace via virtiofs
    +-- guest init (vminitd) -> container process
    +-- nested Docker daemon           (future)
```

Boks talks to containerd over its gRPC socket and asks for a container whose runtime
handler is `io.containerd.nerdbox.v1`. Everything below that line is existing
open-source software. Boks does not implement a VMM, a kernel or a shim.

## Component ownership

| Concern | Provided by | Notes |
|---|---|---|
| Image pull, content store, snapshots | containerd | Boks uses the Go client, not the `ctr` binary |
| Task create/start/wait/kill, IO streaming | containerd | FIFO-based stdio via the task API |
| VM creation and teardown | nerdbox shim + libkrun | one VM per container today |
| Guest kernel | nerdbox (`kernel/`) | libkrunfw-derived, carries TSI patches |
| Guest init / process supervision | nerdbox `vminitd` | shim talks to it over vsock (ttrpc) |
| Host↔guest filesystem sharing | virtiofs, via nerdbox | see *Workspace sharing* |
| Guest root filesystem format | EROFS | required on macOS, optional on Linux |
| Workspace resolution, exact-path mounting | **Boks** | OCI spec construction |
| Host prerequisite diagnosis (`doctor`) | **Boks** | |
| Sandbox naming, state, lifecycle | **Boks** | |
| Network policy engine | **Boks** *(unverified)* | see *Networking* |
| Host proxy + credential injection | **Boks** *(unverified)* | |
| Port forwarding | **Boks** *(unverified)* | |
| Kits / declarative config | **Boks** *(not started)* | |
| Nested Docker daemon | guest image + **Boks** *(not started)* | |

## Why nerdbox

The alternatives considered:

- **Kata Containers** — mature and OCI-native, but a heavier stack (agent, runtime,
  virtiofsd, hypervisor config) oriented towards Kubernetes, and no macOS story.
- **Firecracker / cloud-hypervisor directly** — means writing our own shim, guest agent,
  image handling and vsock protocol. That is the bulk of nerdbox.
- **libkrun directly** — same problem one layer down; we would reimplement the shim.
- **nerdbox** — a containerd sub-project that already provides the shim, the guest init,
  virtiofs bind-mount plumbing and a Linux+macOS VMM. It is explicitly experimental, which
  is an accepted risk: it is the only component that matches both the container-native
  interface and the cross-platform requirement.

Choosing nerdbox means Boks' orchestration layer speaks plain containerd. If nerdbox proves
unsuitable, the runtime handler is a single string; another VM-backed shim can replace it
without touching the CLI or the workspace logic.

## VM lifecycle

1. `boks run` connects to containerd and selects a namespace (`boks`).
2. Image is pulled if absent, and unpacked with the snapshotter the runtime needs
   (`erofs` for nerdbox).
3. Boks builds an OCI runtime spec: process args, env, cwd, and the workspace mount.
4. A container is created with runtime handler `io.containerd.nerdbox.v1` plus resource
   annotations (`io.containerd.nerdbox.resources.cpu`, `.memory`).
5. Starting the task causes containerd to launch the shim, which boots a microVM, mounts
   virtiofs shares, and starts the process under the guest init.
6. Boks waits on the process, streams stdio, and propagates the guest exit code.
7. A persistent sandbox stays up: only the exec'd process is reaped. `boks stop` kills and
   deletes the task, and `boks rm` deletes the container and its snapshot. With `-rm` all of
   that happens when the command exits — on signal too, so nothing is left behind.

## Workspace sharing

Docker Sandboxes exposes the workspace at **the same absolute path** inside the sandbox as
on the host, so build output, stack traces and tool config keep working. Boks targets the
same behaviour.

nerdbox implements OCI bind mounts by turning each one into a virtiofs share:

```
host /home/alice/src/foo
  -> virtiofs share, tag bind-<hash>
  -> guest /run/mnt/bind-<hash>
  -> container /home/alice/src/foo      (destination is free-form)
```

The mount destination inside the container is an ordinary OCI spec field, so requesting the
host path verbatim is exactly what Boks does. Parent directories (`/home`, `/home/alice`)
exist in the guest only as empty directories created to host the mount point; their host
contents are never shared.

One caveat inherited from nerdbox: bind-mounting a *single file* shares its **parent
directory** with the VM. Boks therefore only mounts directories for workspaces.

*(verified 2026-08-11: `boks run /private/tmp/boksprobe/deep/a/b/c/project -- pwd` printed
that exact path inside the guest; the intermediate directories were created automatically
and each contained only the next component of the path, nothing from the host.)*

## Networking

Two options exist below us, and the choice matters for security.

**TSI (nerdbox default).** No virtual NIC. libkrun's patched guest kernel rewrites
`AF_INET` socket syscalls to `AF_TSI`, and the VMM performs the connection on the host.
Convenient, but: no IPv6, no ICMP, and the policy decision point lives inside libkrun where
Boks cannot express rules.

*(verified 2026-08-11: confirmed in a running guest — `/sys/class/net` contains only `lo`,
yet outbound TCP works and the host's own `127.0.0.1` services answered the guest, because
the host performs the connect. This is why TSI cannot be the long-term answer.)*

**External network provider (chosen direction).** nerdbox can attach a virtio-net interface
backed by a host UNIX socket, via `io.containerd.nerdbox.network.*` annotations. Pointing
that at [gvisor-tap-vsock](https://github.com/containers/gvisor-tap-vsock) gives a userspace
TCP/IP stack **on the host**, which sees every packet the guest emits.

That is the property we need: enforcement does not depend on the guest honouring
`HTTP_PROXY`. A guest that opens a raw socket still has its packets terminated by a host
stack that can drop them. The intended shape:

```
guest --virtio-net--> unix socket --> gvisor netstack (host) --> policy engine --> upstream
                                              |
                                              +--> boks proxy (HTTP/HTTPS) --> credential injection
```

Raw TCP/UDP to unapproved destinations is dropped by the netstack; HTTP and HTTPS are
steered to a host-side forward proxy that filters on hostname. For HTTPS the proxy reads the
`CONNECT` target and, later, the TLS SNI — no interception, no custom CA.

*(unverified: not implemented.)*

## Credential injection

The principle borrowed from Docker Sandboxes: the real secret never enters the guest. The
guest sees a placeholder; the host proxy attaches the real credential to outbound requests
that match an explicitly configured destination.

Design constraints Boks adopts:

- injection is keyed on destination host, never on guest request content;
- the guest cannot enumerate or request secrets — there is no host API for it to call;
- the mechanism is generic (header set/replace, bearer, API-key header), not vendor-specific.

A local encrypted file provider comes first; OS keychains (Keychain, Secret Service,
Credential Manager) later.

*(unverified: not implemented.)*

## Nested Docker

The target is a Docker daemon **inside** the guest, so `docker build` works without any path
to the host daemon. The host Docker socket is never forwarded — that would defeat the
isolation boundary entirely. This requires a guest image carrying dockerd plus a writable
data volume; it is not part of the first milestone.

## Persistence and sandbox state

Docker Sandboxes keeps a sandbox alive until explicitly removed: packages, images and shell
history survive stop/start. Boks does the same. A sandbox is a containerd container plus its
writable snapshot; `stop` kills and deletes the *task*, leaving both, and `start` creates a
new task over the same snapshot. Only `rm` deletes the container and the snapshot.

So that a sandbox outlives whatever command created it, the container's own process is an
idle keeper (`sh` trapping SIGTERM and sleeping), and user commands are containerd *execs*
inside it. `boks run` therefore means: create if absent, start if stopped, then exec. This is
also what makes `boks exec` possible at all — containerd can only exec into a running task.

**There is no host-side state store, deliberately.** containerd's container record already
holds the name, image, runtime, snapshotter, creation time and full OCI spec; container
labels carry the two things it cannot express — the workspaces the sandbox was created for
(`dev.boks.workspaces`) and the default command (`dev.boks.command`) — plus a marker
(`dev.boks.managed`) so Boks ignores containers it did not create. `ls` and `inspect` are
derived views over containerd, which means there is no file to fall out of sync with
reality, nothing orphaned when a sandbox is removed by other means, and no per-user state
directory to place correctly on each platform. If something ever genuinely cannot live in
containerd, it belongs under the platform's state directory (`~/.local/state/boks` on Linux,
`~/Library/Application Support/boks` on macOS) — not a hardcoded Linux path.

Identity: the sandbox name is derived from the workspace's absolute host path
(`boks-<12 hex of sha256>`) unless `-name` says otherwise, which is what makes a second
`boks run` in the same directory re-attach instead of duplicating. See open question 3 in
[docker-sandbox-parity.md](docker-sandbox-parity.md) for the reasoning and its consequences.

`boks run -rm` keeps the original ephemeral behaviour: the command is the container process
and the container, task and snapshot are gone when it exits.

## Platform direction

Linux first (KVM). macOS second — libkrun and nerdbox both support it via
Hypervisor.framework, and containerd 2.2+ runs natively there, so nothing in the design is
Linux-only by construction. Windows waits for nerdbox support.

Code that touches platform specifics is kept behind build tags and interfaces, and `doctor`
is structured as a list of checks each of which knows whether it applies to the current
platform, rather than as a Linux script.

## Verification status

Honest statement of what has actually been observed, as of this commit:

- containerd connection, image pull/unpack, container+task lifecycle, stdio streaming,
  exit-code propagation, cleanup: **tested locally**.
- the persistent lifecycle — `create`, `ls`, `inspect`, `start`, `stop`, `exec`, `rm`, `cp`,
  re-attach by workspace, and files surviving stop/start: **tested against a real containerd
  on the runc runtime only**, on a host with no hypervisor. That exercises the orchestration
  and containerd's snapshot semantics; it says nothing about whether these hold across a VM
  boundary.
- exact-path workspace mount construction: **unit-tested**, and **observed inside a booted
  microVM** — including auto-creation of the intermediate guest directories.
- VM boot, guest kernel identity, virtiofs sharing under nerdbox: **observed**
  (2026-08-11, macOS/Apple silicon). Guest ran Linux 6.12.44 on a Darwin host, with its own
  boot_id, uptime and vCPU/memory topology.
- resource annotations (`io.containerd.nerdbox.resources.*`) reaching the VMM: **observed**
  — guest `nproc` and `MemTotal` track `-cpus`/`-memory`.
- the Linux/KVM path: **not observed**. Verification so far is macOS-only.
- network isolation: **observed absent**. The guest reaches the host's loopback services
  via TSI; see [security-model.md](security-model.md).

See [docs/verification.md](verification.md) for the procedure that will confirm the VM
boundary on capable hardware, and for what evidence counts.
