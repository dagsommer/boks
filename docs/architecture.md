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
boks CLI  (cmd/boks, internal/cli)
    |
    v
agent registry (internal/agent)        -- name -> image, startup command, args mode
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
| Sandbox naming, state, lifecycle | **Boks** | the derived name is the identity; see *Persistence and sandbox state* |
| Agent definitions | **Boks** — `internal/agent` | data, not code: name, image, command, args mode. `Registry.Add` is where user-defined agents will arrive |
| Network policy engine | **Boks** — `internal/policy` | applied to every sandbox `run` creates; see *Networking* |
| Host network stack config + supervision | **Boks** — `internal/network`, `internal/enforce` | gvisor-tap-vsock, embedded, one stack per running sandbox |
| Host proxy + credential injection | **Boks** — `internal/proxy`, `internal/secret` | listens *inside* the sandbox's virtual network |
| Port forwarding | **Boks** *(unverified)* | |
| Kits / declarative config | **Boks** *(not started)* | the loader that will feed `Registry.Add`; no schema yet, by choice |
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

0. `boks run [agent] [workspace...]` resolves the agent — a name, an image, a startup
   command and an args mode, from `internal/agent` — and derives the sandbox name from that
   agent and the primary workspace.
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
   deletes the task, and `boks rm` deletes the container and its snapshot. With `--rm` all of
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

The one host where "the host path verbatim" is not a path at all is Windows, since
`C:\Users\dag\src\foo` has no Linux reading. There the workspace is mounted at a reversible
translation of its host path instead — `/c/Users/dag/src/foo`, the convention Docker Sandboxes
and the rest of the Docker family use. `internal/workspace/guestpath.go` is the only code that
knows this, and `docs/windows.md` section 4 is the reasoning. Nothing about it has run on
Windows: a microVM boots there now, but only under `ctr`, and only `boks run` asks for a
workspace to be mapped.

*(verified 2026-08-11: `boks run shell /private/tmp/boksprobe/deep/a/b/c/project -- pwd` printed
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
stack that can drop them. The shape:

```
guest --virtio-net--> unix socket --> gvisor netstack (host) --> policy engine --> upstream
                                              |
                                              +--> boks proxy (HTTP/HTTPS) --> credential injection
```

Raw TCP to unapproved destinations is refused by the netstack, UDP and ICMP are dropped
outright; HTTP and HTTPS are steered to a host-side forward proxy that filters on hostname.
For HTTPS the proxy reads the `CONNECT` target and the TLS SNI without decrypting anything —
**except** for hosts that have a credential injection rule, which are terminated and
re-originated so a header can be attached. That is the only interception Boks performs, and
every logged flow says which it was: `forward` (read by Boks), `forward-bypass` (tunnelled,
ciphertext only) or `transparent` (judged in the stack, by address, with the proxy not
involved).

**The netstack has to be assembled by hand, and this is the reason.** gvisor-tap-vsock's
`virtualnetwork.New` installs its own TCP forwarder, whose handler is a bare
`net.Dial` to whatever address the guest put in the SYN — no policy, and no hook in the
public API to add one. A stack built that way terminates the guest's NIC and then forwards
everything it is handed, which is not a boundary; it was measured not being one, on a real
VM (see [verification.md](verification.md)). Boks therefore does the same assembly itself
from the library's exported pieces — `tap.NewLinkEndpoint`, `tap.NewSwitch`, gvisor's
`stack.New` — and installs its own forwarder via `SetTransportProtocolHandler`, which
consults the policy engine before dialling and records both outcomes. No fork, and no
vendored patch: the link layer and the netstack are still the library's, and only the
decision is ours.

Three consequences of that decision, each of them deliberate:

- **UDP has no forwarder at all**, and datagrams are dropped at the link unless they are DNS
  to the sandbox's own gateway (or DHCP, which is served inside the stack). Forwarding UDP
  would hand the guest a data path with no connection to judge, and DNS to a server of its
  choosing is a channel around every hostname rule. The reference product blocks both and
  does not let a policy re-enable them; this matches.
- **ICMP is dropped**, rather than answered by the stack on behalf of addresses it is not.
- **A denial is a RST, not silence.** A denied destination fails the way a closed port fails,
  immediately, instead of hanging until something times out and leaving the user to guess
  whether it was the policy or the network.

**Configuration (`internal/network`).** Two annotations are required, and each does half the
job. Both were confirmed against nerdbox's source (`internal/shim/task/networking.go` and
`ctrnetworking.go`), not only its documentation, because the documentation is wrong in at
least one place: it shows `addr=192.168.127.2`, while the parser calls `netip.ParsePrefix`
and rejects anything that is not CIDR.

| Annotation | Effect | Required fields |
|---|---|---|
| `io.containerd.nerdbox.network.N` | attaches a NIC to the **VM** | `socket`, `mode` (`unixstream` — see below), `mac` (unicast) |
| `io.containerd.nerdbox.ctr.network.N` | wires the **container** to that NIC | `vmmac`; plus `addr` (CIDR), `gw`, `ifname` |
| `io.containerd.nerdbox.ctr.dns` | writes the container's `/etc/resolv.conf` | `key=value` pairs, one line each |

No OCI spec change is needed, the network namespace is kept, and `CAP_NET_ADMIN` is **not**
required despite what nerdbox's README example implies. The shim deletes these annotations
after parsing, so they never reach the guest.

Consequences Boks' design has to carry:

- **The link is a stream, and Boks does the framing checks.** `mode=unixstream` makes libkrun
  connect to an `AF_UNIX` `SOCK_STREAM` socket Boks listens on and write each Ethernet frame
  behind a 4-byte big-endian length — gvisor-tap-vsock's `qemu` protocol. The datagram mode
  it replaced kept frame boundaries in the kernel; a stream keeps them in a number the peer
  writes, so `internal/network/link.go` bounds that number before the switch allocates on it
  and refuses one too small to be a frame, which would otherwise index past the buffer the
  switch just allocated. It is also what removes the last platform dependency from the host
  stack: Windows' AF_UNIX has no datagram socket, and a stream one it has.
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
  addresses, or proxy the EC2 metadata service. Boks sets all four explicitly closed — and,
  since it assembles the stack itself, implements none of the four at all.
- **Host loopback is closed at the IP layer, not by a rule.** A packet addressed to
  `127.0.0.0/8` arriving on a NIC that is not the loopback interface is a martian and is
  discarded by gvisor before the forwarder sees it. A test gives a simulated guest a policy
  that *permits* the host's loopback and a real listener to reach; it still cannot get there.
- **DNS is mediated.** The container's resolver is set to the gateway rather than inherited
  from a copy of the host's `resolv.conf`, so name resolution is answered by a stack Boks
  controls — and since UDP to anything else is dropped, it is the only resolver the guest can
  reach. That is the hook a policy on names attaches to; it does not by itself close DNS as an
  exfiltration channel, because the gateway still resolves whatever it is asked.
- **IPv6 is live surface now.** TSI had none; a guest with a real NIC brings up link-local
  v6 by itself (the spike saw MLD reports). Boks assigns no routable v6 address and no v6
  gateway, and the policy language covers v6 from the start.
- **A network-less mode exists today, for free.** Emitting only the VM-level annotation
  attaches the NIC — which is what turns TSI off — while never wiring the container to it.
  The container then has `lo` and nothing else, and host loopback is refused. That is
  `--net none`, and it is the strongest containment Boks can currently offer, as well as the
  only one confirmed against a real guest.

### How it is wired into a run (`internal/enforce`)

`boks run` decides, describes and starts the network *before* the sandbox, because the order
is forced by the runtime: the annotations have to be on the container when it is created,
and the host-side stack has to be holding the link socket before the VM boots, since the VM
connects to it during boot.

```
boks run ─┬─ Plan            socket path, addressing, MACs (deterministic per sandbox)
          ├─ annotations  ─> the container, so the VM gets a NIC wired to that socket
          ├─ guest env    ─> HTTP_PROXY/HTTPS_PROXY/NO_PROXY, the CA, credential placeholders
          └─ boks net serve   one process per running sandbox:
                                gvisor netstack on the link, judging every TCP connection
                                + proxy listening at 192.168.127.1:3128 *inside* it
```

The policy engine is built **before** either of them and handed to both, so one set of rules
and one decision log serve the stack and the proxy. A flow the stack allowed and the proxy
then refused appears twice in one log rather than in two logs that disagree.

**The proxy listens inside the virtual network, not on the host.** The listener comes from
the sandbox's own stack, so it exists only in one sandbox's network and nothing is bound on
the host: no other process, no other sandbox and nothing on the LAN can reach it, and two
sandboxes cannot collide on a port. A host port would have failed all three.

**The guest environment is a convenience, not the control.** `HTTP_PROXY`, `HTTPS_PROXY`,
`NO_PROXY`, the CA (as `BOKS_CA_CERT_B64`, as a read-only mount at `/etc/boks`, and through
`NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE` and `CURL_CA_BUNDLE` for the
runtimes that ignore the system trust store) let a cooperating client get hostname rules,
credential injection and readable refusals. A guest that ignores all of it loses those and
keeps the restrictions, because the stack is what enforces — it is judged on addresses and
ports instead of names, which is stricter, not looser: a policy written entirely in hostnames
denies its raw flows. Two details
follow from that distinction and are worth stating: the replacing variables are set only when
Boks could bundle the CA with a public root store, since pointing `SSL_CERT_FILE` at a
Boks-only file would break every host Boks does *not* intercept; and the placeholder a guest
holds for a credential is set from the credential's own configuration, never the secret.

**Where those public roots come from is per-host, and Windows is the awkward one**
(`internal/enforce/roots.go`). On Unix the root store is a PEM file at one of four well-known
paths — the same list Go's own `crypto/x509` walks — and Boks passes it to the guest verbatim,
because the distribution's curated answer is the host's own answer and re-deriving it could
only produce a guest that trusts a different set from its host. Windows has no such file, and
Go cannot supply one either: `x509.SystemCertPool` there returns an opaque pool that defers to
`CertGetCertificateChain` and cannot be enumerated. So the `ROOT` store is read through
CryptoAPI and encoded as PEM, with the `Disallowed` store subtracted from it (a PEM file
cannot express distrust, so a certificate the host has stopped trusting has to be left out
rather than marked) and the `CA` intermediate store deliberately left alone (every certificate
in a PEM bundle is loaded as a *trust anchor*, so copying cached intermediates in would
promote them to roots inside the guest).

**A Windows host that cannot produce that bundle does not get a sandbox.** This is the one
place the guest-environment machinery fails closed rather than degrading, and the reason is
what "no bundle" actually means: not "no trust", but *partial* trust. `NODE_EXTRA_CA_CERTS` is
additive and is set regardless, so Node would trust the Boks CA while curl, python and git —
all on OpenSSL — would not. Half the tools in the sandbox work, half fail with certificate
errors, and the universally documented cure for a certificate error is to stop verifying:
`curl -k`, `NODE_TLS_REJECT_UNAUTHORIZED=0`, `verify=False`. Those switches do not disable
verification only for the intercepted hosts; they disable it for the real origins too. A
sandbox that refuses to start, and says why, is the cheaper failure. The refusal is narrow —
it is only reachable when the sandbox intercepts something in the first place. Unix keeps its
existing behaviour of leaving the three replacing variables unset, since a Unix host with none
of the four paths is a host on which Go itself has no roots.

### The stack has to outlive the command, so it lives in its own process

A persistent sandbox outlives the command that created it — that is the point of it. But
whoever holds the link socket *is* the sandbox's network, so a stack living in the CLI
invocation would mean Ctrl-C silently disconnecting a background build, and `boks run -d`
producing a sandbox with no network at all. The stack's lifetime has to be the *VM's*
lifetime, and no CLI invocation has that lifetime.

Boks therefore runs **one supervisor process per running sandbox** (`boks net serve`), not a
daemon:

- started on demand by whichever command starts the VM (`run`, `exec`, `start`), never at
  boot and never by an installer. A host with no running sandbox runs no Boks process.
- no privilege beyond the CLI's own: it binds a UNIX socket under the user's state directory
  and talks to the same containerd.
- its life is bounded by the sandbox's task, which it watches through containerd. A VM that
  exits, is stopped or is removed takes its supervisor with it — there is nothing to reap and
  no `boks daemon stop` to remember.
- liveness is a held `flock`, not a recorded PID, so a crashed supervisor is detected without
  the risk of signalling a process that inherited its number.
- it is handed the credential *values* it needs on a pipe, and never the passphrase to the
  store, so it can attach the credentials one sandbox was configured with and obtain no
  others.

A **global daemon** was the alternative — it is what Docker Sandboxes runs as `sandboxd`, and
it would be the natural home for port forwarding and live statistics. It was rejected for
this: an always-on service the user did not ask for, holding every sandbox's credentials and
CA, whose crash takes out every sandbox at once, with its own start/stop/status surface and
its own state store. containerd already supervises the VMs; the only thing left needing a
process is the socket at the end of one VM's NIC, and that is exactly what this is. An
**attach-only** stack was also considered and rejected: it is simpler, but it ties the
network to the wrong object, and it stakes everything on an assumption nobody here can test
— that a running VM re-attaches to a fresh socket at the same path once the previous holder
is gone.

A second, unplanned argument arrived from measurement: gvisor-tap-vsock v0.8.9 exposes no way
to shut a `VirtualNetwork` down, so each stack leaves about twenty goroutines behind for the
life of its process (2 → 23 → 44 over two open/close cycles). A supervisor that exits with
its sandbox releases everything; a long-lived process would accumulate a stack's worth per
sandbox it ever served.

*(implemented in `internal/network` and `internal/enforce`, and exercised end to end against
a **simulated** guest — a second stack on the far end of the real link socket, speaking real
Ethernet, ARP, TCP and HTTP through the proxy, with an allowed host fetched and a denied one
refused. **No policy has been enforced against a real guest**, because that needs a
hypervisor and the machine this was built on has none.)*

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
`--inject service@host[,host…]=bearer|basic[:user]|header[:format]`, with host patterns from
the same matcher the policy uses, and a catch-all is rejected: sending a token "wherever this
request is going" is the failure this exists to prevent, and under the interception design it
would also decide to decrypt everything.

The placeholder a guest holds belongs to the credential
(`--guest-credential service=[ENV=]placeholder`) rather than being a constant, because clients
validate credential format locally: a marker like `boks-managed` makes `gh` and friends fail
before a request ever reaches the proxy. Values are wrapped in a type whose `String`,
`GoString` and JSON forms are redacted, and a test asserts a secret cannot be printed.

Almost nobody should have to write either flag. A **service registry** in the same package
maps a name — `anthropic`, `github`, `openai`, and the rest of Docker Sandboxes' list — to
that vendor's hosts, header, guest variable and key shape, so `boks secret set anthropic` is
the whole configuration and a stored credential applies to every sandbox. It is data in the
shape `internal/agent` uses, with `Add` as the seam a user-defined service arrives through,
and each row *renders itself into the two flag spellings above* rather than building rules
directly — so one parser and one validator govern a built-in row and a hand-typed rule alike,
and the process that runs a sandbox's proxy needs no knowledge of the registry at all. Two of
the eleven names carry no rule, because their vendors document a header and not the host their
CLI sends it to; asking for one says so rather than guessing.

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

*(implemented, unit-tested, and applied by `boks run`: the credential values are resolved
from the store by the foreground command and handed to the sandbox's network process on a
pipe, so the passphrase never reaches a long-lived process. The guest is given the
placeholder, in the environment variable the credential names.)*

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
idle keeper — a shell that sleeps, and on SIGTERM sends SIGTERM to every process in the
guest before exiting, so an exec'd build or server is asked to stop rather than killed. User
commands are containerd *execs* inside it. `boks run` therefore means: create if absent,
start if stopped, then exec, and `boks exec` starts a stopped sandbox for the same reason —
containerd can only exec into a running task, so there is nothing to ask the user.

**There is no host-side state store, deliberately.** containerd's container record already
holds the name, image, runtime, snapshotter, creation time and full OCI spec; container
labels carry the three things it cannot express — the workspaces the sandbox was created
for (`dev.boks.workspaces`), the default command (`dev.boks.command`) and the agent it runs
(`dev.boks.agent`) — plus a marker (`dev.boks.managed`) so Boks ignores containers it did
not create. `ls` and `inspect` are
derived views over containerd, which means there is no file to fall out of sync with
reality, nothing orphaned when a sandbox is removed by other means, and no per-user state
directory to place correctly on each platform. If something ever genuinely cannot live in
containerd, it belongs under the platform's state directory (`~/.local/state/boks` on Linux,
`~/Library/Application Support/boks` on macOS) — not a hardcoded Linux path.

Identity: the sandbox name is derived from the agent and the workspace's directory name
(`<agent>-<dir>`, as sbx does) unless `--name` says otherwise, which is what makes a second
`boks run` with the same agent in the same directory re-attach instead of duplicating. The
name *is* the identity — there is no separate key — so the derivation has to answer for
characters containerd rejects, two directories sharing a basename, filesystem roots and
containerd's length limit. Each of those decisions, and their consequences, is in section 2a
of [docker-sandbox-parity.md](docker-sandbox-parity.md).

`boks run --rm` keeps the original ephemeral behaviour: the command is the container process
and the container, task and snapshot are gone when it exits — and so are its network stack,
its link socket and the copy of the CA that was shared into it.

The one piece of host-side state a sandbox now does have is its network: a directory under
the state directory holding the link socket, the supervisor's lock and its record
(`<state>/net/<sandbox>/`), plus the public certificate shared into it
(`<state>/certs/<sandbox>/`). Both are derived from the sandbox's name and are removed with
it. That is not a state store creeping in through the back door — nothing in them is
authoritative, and deleting them while the sandbox is stopped costs the sandbox nothing.

## Platform direction

Linux first (KVM). macOS second — libkrun and nerdbox both support it via
Hypervisor.framework, and containerd 2.3+ runs natively there, so nothing in the design is
Linux-only by construction.

**Windows does not wait for nerdbox**, which is what this document used to say, and it is not
blocked by anything about Windows either. A spike ([windows.md](windows.md)) found that the
reference product runs this exact architecture there — containerd, a nerdbox shim, a Linux
microVM, virtio-net terminated by a userspace stack — on top of the **Windows Hypervisor
Platform**, a user-mode hypervisor API. That architecture now runs here too: with this
repository's patch series for libkrun, containerd and the nerdbox shim, `ctr tasks start` ran a
Linux container in a microVM on Windows 11 on 2026-08-14, booting in 1.9 s
([verification.md](verification.md)). **The one device that has never carried a frame there is
virtio-net** — precisely the one Boks' enforcement depends on. That is the whole gap, and it is
narrow.

One thing is true there regardless of how it is closed: the exact-path workspace property is
impossible, because `C:\Users\dag\src\foo` is not a Linux path (both Boks and the reference
product would map it to `/c/Users/dag/src/foo`). The link socket is no longer on that list —
it used to be, because Windows' AF_UNIX is stream-only and the link was a datagram socket, and
the link is a stream now on every platform.

Until that frame crosses, the Windows answer is **WSL2 with nested virtualisation**, where Boks is
just a Linux program: `/dev/kvm` and EROFS are both in the inbox kernel, the link is a normal
AF_UNIX stream socket inside the distro, and workspace paths are Linux paths so exact-path
mounting holds unchanged. Untested, and not a port.

Code that touches platform specifics is kept behind build tags and interfaces, and `doctor`
is structured as a list of checks each of which knows whether it applies to the current
platform, rather than as a Linux script.

## What a real Docker Sandboxes guest looks like

Observed directly from inside a live sandbox on 2026-08-11, rather than inferred. These are
facts about the reference implementation, and several of them validate or correct choices
made here.

| Observation | What it tells us |
|---|---|
| `/etc/ssl/certs` contains a self-signed **"Docker Sandboxes Proxy CA"** (`CA:TRUE, pathlen:2`, ten-year validity), also exposed as `PROXY_CA_CERT_B64` | TLS interception is **confirmed, not inferred**. The CA is installed in the guest trust store and additionally handed to the guest as an environment variable so runtimes with their own trust stores can pick it up |
| The guest has **`eth0`**, plus 20 virtio devices | Docker does **not** use TSI. It attaches a real virtio-net interface, which is exactly the direction the networking spike pushed Boks toward |
| `/etc/resolv.conf` — "Generated by docker-next", nameserver on the host-side gateway, search domain `<sandbox-name>.docker.internal` | DNS is mediated host-side, and sandboxes are addressable by name. `gateway.docker.internal` resolves to an IPv6 ULA, so the guest↔host path is v6 |
| `/etc/resolv.conf` and `/etc/hosts` are **read-only virtiofs bind mounts** | Host-controlled guest configuration is delivered as mounts rather than written into the image |
| Workspace mounted as `bind-<16 hex> … virtiofs` at its exact host path | The tag format matches nerdbox's `bind-%x` convention exactly — strong evidence of shared lineage, and reassurance that Boks' runtime choice is the same family as the reference product |
| `/home/agent/.claude/skills` bind-mounted **read-write** | The shared skills store is a real rw crossing back to the host. Boks declines to copy it; Docker's own security docs call it a cross-sandbox trust issue |
| `/dev/vdd` → `/var/lib/docker`, **ext4 on its own block device** | Nested Docker gets a separate, independently sized data disk rather than living in the root overlay |
| Root filesystem is **overlay** | Image plus a writable layer, as expected |
| `dockerd` running inside the guest, socket at `/var/run/docker.sock` | Nested Docker confirmed, entirely inside the VM |
| No `/run/sandbox/source` | This sandbox is in direct mode; that path exists only under `--clone` |
| PID 1 is `tini -- sh -c "trap 'kill -TERM -- -1; wait' TERM; sleep infinity & wait"` | The same idle-keeper design Boks arrived at independently — the sandbox's life does not depend on the first command. Two refinements worth taking: the trap signals the **whole process group** so work inside the sandbox shuts down gracefully, and `tini` as PID 1 reaps zombies in a long-lived sandbox |

The overall picture is that Docker Sandboxes and Boks are converging on the same
architecture from different starting points: a containerd-family shim, a microVM per sandbox,
virtiofs for exact-path workspaces, a real NIC into host-controlled networking, and a keeper
process so the sandbox outlives any single command. The substantive differences left are that
Docker runs a single always-on daemon where Boks runs one short-lived supervisor per running
sandbox (see *Networking*), and that Docker ships the agent images and kit tooling that Boks
has yet to build.

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
  — guest `nproc` and `MemTotal` track `--cpus`/`--memory`.
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
  Boks certificate while an unconfigured one keeps the origin's own chain.
- the datapath from a guest to the proxy: **tested against a simulated guest** — a second
  gvisor stack on the far end of the real link socket, with real Ethernet frames, ARP, a TCP
  handshake and HTTP through the proxy at the gateway address. An allowed destination was
  fetched, a denied one was refused with a reason, and both were recorded in the decision log
  under the sandbox's name.
- the wiring into `boks run`: **exercised against a real containerd on the non-VM dev
  runtime**. The annotations and the guest environment were read back from the container
  spec, `--net none` produced the VM NIC annotation and nothing else, the CA reached the guest
  as a read-only mount, and stopping or removing a sandbox left no socket, no supervisor
  process and no state behind. The runc runtime ignores the nerdbox annotations, so this says
  the spec is correct — **not** that a NIC was ever attached.
- **any of it enforcing against a real guest: not observed.** No VM has been refused a
  destination by Boks. That needs a hypervisor; this machine has none.
- whether a running VM re-attaches to a link socket that was restarted under it: **unknown**,
  and it decides how gracefully a crashed supervisor recovers.

See [docs/verification.md](verification.md) for the procedure that will confirm the VM
boundary on capable hardware, and for what evidence counts.
