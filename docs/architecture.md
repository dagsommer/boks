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
| Network policy engine | **Boks** — `internal/policy` *(built, not enforcing)* | see *Networking* |
| Host network stack config + supervision | **Boks** — `internal/network` *(built, not wired)* | gvisor-tap-vsock, embedded |
| Host proxy + credential injection | **Boks** — `internal/proxy`, `internal/secret` *(built, cooperating-client only)* | |
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
6. Boks waits on the task, streams stdio, and propagates the guest exit code.
7. On exit — or on signal — Boks kills the task, deletes it, deletes the container and
   removes the snapshot. Ephemeral sandboxes leave nothing behind.

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

*(verified 2026-08-11, macOS host with a working hypervisor: the provider **does** displace
TSI. Same binary, same image, only annotations differ. With the defaults the guest has `lo`
only and a host service on `127.0.0.1:9999` answers it; with the provider attached the guest
has `eth0` and the same probe returns **connection refused** — the call is now handled by
the guest's own loopback stack instead of being impersonated on the host. Corroborated three
ways: a ninth virtio device (`VIRTIO_ID_NET`) appears, the host stack logs frames carrying
the VM's MAC, and the guest's `resolv.conf` changes from a copy of the host's file to the
gateway address Boks configures.)*

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
`CONNECT` target and the TLS SNI without decrypting anything — **except** for hosts that
have a credential injection rule, which are terminated and re-originated so a header can be
attached. That is the only interception Boks performs, and every logged flow says which it
was: `forward` (read by Boks) or `forward-bypass` (tunnelled, ciphertext only).

**Configuration (`internal/network`).** Two annotations are required, and each does half the
job. Both were confirmed against nerdbox's source (`internal/shim/task/networking.go` and
`ctrnetworking.go`), not only its documentation, because the documentation is wrong in at
least one place: it shows `addr=192.168.127.2`, while the parser calls `netip.ParsePrefix`
and rejects anything that is not CIDR.

| Annotation | Effect | Required fields |
|---|---|---|
| `io.containerd.nerdbox.network.N` | attaches a NIC to the **VM** | `socket`, `mode` (`unixgram` for gvisor-tap-vsock), `mac` (unicast) |
| `io.containerd.nerdbox.ctr.network.N` | wires the **container** to that NIC | `vmmac`; plus `addr` (CIDR), `gw`, `ifname` |
| `io.containerd.nerdbox.ctr.dns` | writes the container's `/etc/resolv.conf` | `key=value` pairs, one line each |

No OCI spec change is needed, the network namespace is kept, and `CAP_NET_ADMIN` is **not**
required despite what nerdbox's README example implies. The shim deletes these annotations
after parsing, so they never reach the guest.

Consequences Boks' design has to carry:

- **One stack per sandbox.** A second VM on the same socket gets a duplicate address; a
  third fails to attach. `internal/network` therefore creates a unique socket directory per
  sandbox and ties the stack's lifetime to the sandbox's.
- **The stack is embedded, not spawned.** gvisor-tap-vsock is used as a Go library rather
  than by exec'ing `gvproxy`: a goroutine cannot outlive a crashed parent, there is no PID
  to track or orphan to reap, and the closed posture becomes a typed configuration a test
  can assert rather than the absence of command-line flags. The cost is gvisor's netstack in
  the binary; it cross-compiles for darwin/arm64 and windows/amd64.
- **Deny-by-default is asserted, not assumed.** The observed unreachability of the host was a
  property of one configuration, not a guarantee: gvisor-tap-vsock can be told to translate
  an address onto the host's loopback, forward host ports inward, answer on extra gateway
  addresses, or proxy the EC2 metadata service. Boks sets all four explicitly closed.
- **DNS is mediated.** The container's resolver is set to the gateway rather than inherited
  from a copy of the host's `resolv.conf`, so name resolution is answered by a stack Boks
  controls. That is the hook a policy on names attaches to; it does not by itself close DNS
  as an exfiltration channel.
- **IPv6 is live surface now.** TSI had none; a guest with a real NIC brings up link-local
  v6 by itself (the spike saw MLD reports). Boks assigns no routable v6 address and no v6
  gateway, and the policy language covers v6 from the start.
- **A network-less mode exists today, for free.** Emitting only the VM-level annotation
  attaches the NIC — which is what turns TSI off — while never wiring the container to it.
  The container then has `lo` and nothing else, and host loopback is refused. That is
  `-net none`, and it is the strongest containment Boks can currently offer.

*(implemented in `internal/network`, unit-tested, **not yet wired into `boks run`'s
datapath**. The transport change is verified; no policy has yet been enforced against a real
guest.)*

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

**Implemented in `internal/secret`.** The model has two levels, following the credential
grammar Docker Sandboxes' kits use: a credential names a service and owns a set of injection
rules, each naming a domain, a header and a value format with one `%s` (`bearer` and
`basic[:user]` are shorthands for the two common shapes). Several hosts can therefore share
one stored secret — an enterprise Git host and its API endpoint, a feed and its mirror —
without repeating the scheme, which is how a rotation ends up applied to three places out of
four. On the command line that is
`-inject service@host[,host…]=bearer|basic[:user]|header[:format]`, with host patterns from
the same matcher the policy uses, and a catch-all is rejected: sending a token "wherever this
request is going" is the failure this exists to prevent, and under the interception design it
would also decide to decrypt everything.

The placeholder a guest holds belongs to the credential
(`-guest-credential service=[ENV=]placeholder`) rather than being a constant, because clients
validate credential format locally: a marker like `boks-managed` makes `gh` and friends fail
before a request ever reaches the proxy. Values are wrapped in a type whose `String`,
`GoString` and JSON forms are redacted, and a test asserts a secret cannot be printed.

Storage is an AES-256-GCM file keyed by PBKDF2-HMAC-SHA256 over a passphrase from
`BOKS_SECRETS_PASSPHRASE`. Secret *names* are inside the ciphertext too. A key file next to
the encrypted file is deliberately not offered: it encrypts nothing against anyone who can
read the directory, while looking like it does.

**HTTPS costs an interception (`internal/ca`).** Injection needs to see the request, and an
HTTPS request is visible only to something that terminates the session. Boks terminates TLS
for hosts an injection rule names, and for no others: it mints a leaf from a locally
generated CA whose private key never leaves the host, verifies the origin's real certificate
itself, and streams bodies through without retaining anything. `Injector.Handles` is the
single predicate deciding both injection and interception, so the two sets cannot drift
apart. `boks ca show|export|env|regenerate` inspects, distributes and retires the authority;
the certificate goes to a guest as a file and as `BOKS_CA_CERT_B64`, because Node and Python
carry their own trust stores and ignore the system one.

*(implemented and unit-tested; not wired into `boks run`.)*

## Nested Docker

The target is a Docker daemon **inside** the guest, so `docker build` works without any path
to the host daemon. The host Docker socket is never forwarded — that would defeat the
isolation boundary entirely. This requires a guest image carrying dockerd plus a writable
data volume; it is not part of the first milestone.

## Persistence

Docker Sandboxes keeps a sandbox alive until explicitly removed: packages, images and
shell history survive stop/start. Boks starts ephemeral (`run` creates and destroys) and
will grow named, persistent sandboxes backed by a writable snapshot plus a small host-side
state store. State belongs under XDG paths on Linux (`~/.local/state/boks`).

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
- exact-path workspace mount construction: **unit-tested**, and **observed inside a booted
  microVM** — including auto-creation of the intermediate guest directories.
- VM boot, guest kernel identity, virtiofs sharing under nerdbox: **observed**
  (2026-08-11, macOS/Apple silicon). Guest ran Linux 6.12.44 on a Darwin host, with its own
  boot_id, uptime and vCPU/memory topology.
- resource annotations (`io.containerd.nerdbox.resources.*`) reaching the VMM: **observed**
  — guest `nproc` and `MemTotal` track `-cpus`/`-memory`.
- the Linux/KVM path: **not observed**. Verification so far is macOS-only.
- network isolation: **observed absent in the default configuration**. With nerdbox's
  defaults the guest reaches the host's loopback services via TSI; see
  [security-model.md](security-model.md).
- the external network provider displacing TSI: **observed** (2026-08-11, macOS). With the
  provider attached the guest gains `eth0` and the same host-loopback probe is refused. What
  has **not** been observed is any policy being enforced against a guest — the transport is
  verified, the enforcement built on it is not.
- policy engine, host proxy (HTTP + CONNECT + SNI filtering, and TLS termination for
  credential-bearing hosts only), credential injection, network annotation generation and
  host-stack supervision: **unit-tested on the host**, and the proxy exercised end to end
  against real TLS origins — including a demonstration that an intercepted host presents a
  Boks certificate while an unconfigured one keeps the origin's own chain. None of it is
  connected to a sandbox.

See [docs/verification.md](verification.md) for the procedure that will confirm the VM
boundary on capable hardware, and for what evidence counts.
