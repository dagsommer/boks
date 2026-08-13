# Boks Security Model

**Read this before trusting Boks with anything.** Boks is experimental. Several boundaries
described here are designed but not implemented, and the VM boundary itself has not yet been
demonstrated by this project on real hardware. Claims are marked accordingly.

## Threat model

**Assume code running inside the sandbox is hostile.** Not "possibly buggy" — actively
trying to reach the host, read credentials, or exfiltrate data. A coding agent executing
code it wrote from text it read on the internet is exactly this.

In scope:
- guest code attempting to escape to the host;
- guest code attempting to read host files outside the workspace;
- guest code attempting to reach the network in ways policy forbids;
- guest code attempting to obtain raw credentials;
- guest code tampering with the workspace to attack the host *later*.

Out of scope, for now:
- a malicious host (nothing protects the guest from you);
- hypervisor 0-days and CPU side channels;
- physical attacks;
- denial of service by resource exhaustion;
- a malicious base image — you choose the image, and it runs as your sandbox.

## Trust boundaries

```
                          ┌──────────────── host (trusted) ────────────────┐
                          │  boks CLI · policy engine · proxy · secrets    │
                          └───────────────────────┬────────────────────────┘
   ══════════════════ hypervisor boundary ════════╪══════════════════════════
                          ┌───────────────────────┴────────────────────────┐
                          │  guest VM (hostile): own kernel, root, agent   │
                          └────────────────────────────────────────────────┘
        crossings:  virtiofs workspace · virtio-net/vsock · stdio
```

The **hypervisor boundary is the primary control**. Everything else is defence in depth.
In-guest permissions are not a boundary: the agent has root in the guest, by design.

### Guest → host

Three channels cross the boundary, and each is a potential escape surface:

1. **virtiofs** — the workspace share. Attack surface is the virtiofs implementation in
   libkrun and the guest kernel driver. Boks limits exposure by sharing directories, never
   single files (nerdbox implements file bind mounts by sharing the *parent directory* —
   surprising and dangerous, so Boks does not use them).
2. **virtio-net / vsock** — device emulation in the VMM, and the shim's ttrpc protocol to
   the guest init. A guest that compromises the shim is on the host.
3. **stdio** — terminal output from the guest is written to your terminal. Untrusted output
   can contain escape sequences; treat sandbox output as untrusted data.

*(unverified: Boks has not demonstrated VM boot, so it has not demonstrated that this
boundary holds in practice for our configuration.)*

### containerd exposure

Boks runs on the host and talks to containerd. **containerd's socket is host-side and is
never exposed to the guest.** Anyone who can reach that socket can run containers as root —
so access to it is equivalent to host root, and Boks does nothing to change that. Boks does
not run a daemon of its own and adds no new privileged socket.

Rootless containerd is the better posture and nerdbox is rootless by default; Boks does not
yet require or verify a rootless setup.

### Filesystem exposure

What the guest can see of your disk:

- **the workspace directory only**, at its host path;
- nothing above it — `/home`, `/home/alice` and similar exist in the guest only as empty
  directories that hold the mount point;
- the guest root filesystem, which comes from the image, not your host.

Two real risks remain, both inherited from the "mount the live workspace" model:

- **Live writes.** In direct mode the guest writes straight to your host files. Anything in
  the workspace that the *host* later executes is a path back to you: Git hooks, `Makefile`,
  `package.json` scripts, CI config, IDE tasks, `.envrc`, editor and agent config. **Review
  diffs before running anything from a workspace a sandbox touched.** Docker documents this
  same risk for direct mode; a clone mode that keeps writes inside the VM is the mitigation,
  and Boks has not implemented it yet.
- **Symlinks.** *(verified 2026-08-11, macOS host.)* A workspace symlink pointing outside
  the shared directory does **not** escape. Symlinks are resolved against the guest's own
  root, not the host's: a link to `/etc` read the guest's `/etc`, and links to
  `~/.ssh` and to a sibling file outside the workspace both failed with
  `No such file or directory`, because those paths do not exist in the guest. A link
  therefore reaches host data only if its target is itself inside a shared workspace.

### Network

The design principle: **an environment variable is not a security boundary.** A guest that
ignores `HTTP_PROXY` and opens a raw socket must still be stopped.

Enforcement is a host-side userspace network stack terminating the guest's virtio-net link.
Every TCP connection the guest opens is judged there — against the address and port in the
packet, before anything is dialled — so a denied destination is refused whether or not the
guest cooperated. UDP and ICMP are dropped at the link, with DNS to the sandbox's own
resolver as the only exception. HTTP/HTTPS is *additionally* steered to a forward proxy
inside the same virtual network, which filters on the `CONNECT` target and TLS SNI. Almost
all traffic is tunnelled without interception, so end-to-end TLS is preserved and the proxy
cannot read it; the exception is credential injection, described under
[TLS interception](#tls-interception-and-the-boks-ca) below.

#### What exists today

| Piece | State |
|---|---|
| Policy engine — exact/wildcard hosts, IP and CIDR rules, ports, deny precedence, presets, decision log (`internal/policy`) | built, unit-tested |
| Durable, scoped policy store — global, per-sandbox and profile scopes (`internal/policy/store.go`) | built, unit-tested |
| A sandbox remembering the policy it was created with (`dev.boks.policy`) | built, integration-tested against containerd |
| Host forward proxy — HTTP, `CONNECT`, SNI cross-check (`internal/proxy`) | built, exercised end to end against real TLS origins |
| TLS interception for credential-bearing hosts only, local CA (`internal/ca`) | built, demonstrated end to end with a real client, two real HTTPS origins and certificate comparison |
| Credential injection, encrypted host-side store (`internal/secret`) | built, unit-tested |
| Network annotations and host-stack supervision (`internal/network`, `internal/enforce`) | built, unit-tested |
| Policy-aware TCP forwarder in the stack; UDP and ICMP dropped (`internal/network`) | built, **verified against a real guest** 2026-08-13 |
| All of it applied to a running sandbox (`boks run`) | **done** |
| Policy enforced against a real guest **that uses the proxy** | **verified** 2026-08-12 |
| Policy enforced against a real guest **that ignores the proxy** | **verified** 2026-08-13 — see below |

> [!IMPORTANT]
> **This was measured broken on 2026-08-12 and measured fixed against a real guest on
> 2026-08-13.** *(macOS host with a hypervisor.)* A guest that unset
> `http_proxy`/`https_proxy` reached any destination it liked: under `--policy locked --allow
> example.com`, a direct `https://www.google.com/` returned **HTTP 200**, and a raw Python
> TLS handshake to `1.1.1.1:443` completed against **the origin's own certificate**
> (`SSL.com SSL Intermediate CA ECC R2`). Neither appeared in `boks policy log`, because
> neither reached the policy engine.
>
> The cause was that the host-side stack was built by gvisor-tap-vsock's
> `virtualnetwork.New`, whose TCP forwarder dials whatever address the guest puts in a SYN.
> Boks now assembles the stack itself and installs a forwarder that consults the policy
> engine first. **Re-run against a real guest, that same handshake is
> `ConnectionRefusedError`**, the denial is logged as `transparent`, and UDP to an external
> resolver times out. A control in the same sandbox — the address permitted explicitly —
> connects end to end and presents the origin's real certificate, so the forwarder decides
> per flow rather than dropping everything that skips the proxy. Full transcript in
> [docs/verification.md](verification.md).
>
> One consequence worth knowing. Enforcement is on the **address**, so a hostname-only
> policy denies raw connections *including to the allowed host* — fail-closed, but not what
> a reader of `--allow example.com` expects. `boks policy ls` now says so when every allow
> rule it resolved names a host, rather than leaving it to be discovered by a refusal.
>
> UDP and ICMP drops are no longer silent: they are recorded as `transparent` refusals, once
> per destination and capped, so a guest probing them appears in `boks policy log` without
> being able to choose how large that log grows.
>
> `--net none` is unaffected and remains a real boundary.

**The proxy is not the enforcement boundary; the stack under it is.** A forward proxy filters
only traffic a client chooses to send it, so if `HTTP_PROXY` were all Boks did, a three-line
program in the guest would walk past it — which is precisely what was measured. What the
proxy provides, and only for a cooperating client, is the set of things that need a
conversation rather than a packet: hostname rules, credential injection, and a refusal that
explains itself. A guest that declines all of that is judged on addresses and ports instead.

The residue that no stack can remove: **a hostname rule cannot be applied to a raw flow**,
because a SYN carries no name. A policy written entirely in hostnames therefore *denies* raw
flows rather than permitting the addresses those names resolve to. That fails in the safe
direction, and it is why an agent that needs a non-HTTP protocol needs an address rule.

Two properties of the listener are worth stating because they were design choices, not
accidents:

- **Nothing is bound on the host.** The proxy's listener comes from the sandbox's own virtual
  network, so it is reachable from that one sandbox and from nowhere else — not the host, not
  another sandbox, not the LAN — and two sandboxes cannot collide on a port.
- **The stack runs in a process whose life is the VM's life**, not the CLI's. See
  [The network supervisor](#the-network-supervisor) below for what that process is trusted
  with.

#### What the enforcement path now rests on

*(verified 2026-08-11, macOS host with a hypervisor.)* An external network provider **does**
displace TSI. With nerdbox's defaults the guest has `lo` only and a host service on
`127.0.0.1` answers it; with a virtio-net link to a host-side stack, the guest has `eth0`
and the same probe is **refused** — the connection is handled by the guest's own loopback
stack instead of being impersonated on the host. So there is now a point at which Boks can
see and drop a flow.

*(re-verified 2026-08-12 through `boks run` itself: a host `python3 -m http.server 9999
--bind 127.0.0.1` was unreachable from the guest — `curl` returned rc=7 and a Python
`connect()` raised `ConnectionRefusedError`. The host-loopback hole TSI left open is
closed.)* That point is now used: the stack drops what the policy denies.

Host loopback is closed twice over, and the stronger of the two is not the policy. A packet
addressed to `127.0.0.0/8` arriving on a NIC that is not the loopback interface is a martian,
and the host-side stack discards it at the IP layer — before the forwarder, before any rule
is consulted. A test gives a simulated guest a policy that *permits* the host's loopback and
a real listener to reach, and it still cannot get there.

Link-local (`169.254.0.0/16`, which contains the `169.254.169.254` instance metadata
endpoint) is refused the same way: in the forwarder, before the policy is asked, so that an
explicit `--allow 169.254.169.254` does not buy it either. Every preset also denies it, but
that is the weaker of the two statements.

Three things that verification changed, all of them reflected in the code:

- **Deny-by-default must be asserted, not inherited.** The host being unreachable was a
  property of one configuration. The same stack can be told to map an address onto the
  host's loopback, forward host ports inward, answer on extra gateway addresses, or proxy
  the EC2 metadata service. Boks sets all four explicitly closed, and a test reads them back.
- **IPv6 became live surface.** TSI had none. A guest with a real NIC brings up link-local
  v6 by itself. Boks hands out no routable v6 address and no v6 gateway, and the policy
  language covers v6 from the start rather than as an addition.
- **A genuinely network-less mode exists now.** Attaching the NIC to the VM without wiring
  the container to it turns TSI off and leaves the container with loopback only. That is
  `--net none`, and it is the strongest containment Boks can currently offer — the only
  posture whose enforcement has been confirmed against a real guest.

A fourth thing the 2026-08-12 run changed: **a stack is not a boundary until something in it
says no.** The library's stack terminated the guest's NIC and then forwarded everything it
was handed. Terminating the link is necessary and nowhere near sufficient, and a design that
stops at "we control the far end of the NIC" has stopped one step early.

#### Where a policy lives, and what a scope can do

Policy is durable state, not an argument to a run. Rules live in `<state>/policy/policy.json`
— versioned JSON, `0600` in a `0700` directory, replaced by an atomic rename — and are read by
every command that brings a sandbox's network up. Three scopes write into it: **global** (every
sandbox on this machine), **per-sandbox**, and **profile** (a named policy a run selects with
`--profile`).

**The scoping rule is one sentence, and it is a security property rather than an ergonomic
one: a deny in any scope beats an allow in any scope.** There is no specificity ordering, no
"closer scope wins", and no way for a narrower scope to unsay a wider one. A sandbox-scoped
rule can add access the machine's policy already tolerates, and can take access away; it can
never widen past a prohibition someone wrote down. Only the base preset — chosen by
`policy init`, by a profile, or by a `--policy` flag — decides what happens to a destination no
rule mentions, so no stored rule can turn a deny-by-default machine into an allow-by-default
one. Every ordered pair of scopes is tested for this.

The per-run `--policy`, `--allow` and `--deny` flags are a **Boks addition**, not something sbx
has, and they are overrides rather than a second policy: `--policy` and `--allow` replace the
posture and the allow list, while `--deny` is *added* to what the sandbox already denies. A run
therefore cannot drop a prohibition by typing a different one.

**The store fails closed.** A store that cannot be read — malformed, unreadable, or written by
a newer Boks whose schema this build does not understand — is an error on every path that
touches it, so no sandbox starts. A single rule that does not parse rejects the whole file
rather than being skipped, because the rule that failed to parse may be a deny. A *missing*
store is not an error: it is the uninitialised state, and it resolves to the built-in
deny-by-default preset.

**A sandbox remembers its own selection.** The profile, preset and per-run rules a sandbox was
created with are recorded in a container label (`dev.boks.policy`) and read back by `boks
start` and `boks exec`, neither of which has policy flags. Before that existed, restarting a
sandbox served it the *default* preset instead of the policy it was created under — a
containment that silently widened on restart, which is exactly the direction a failure must not
go. The label records the selection and not the resolved rules, so a rule added to the store
after a sandbox was created still reaches it, and so the record stays well inside containerd's
label size limit; a record that would exceed it is refused rather than truncated.

Nothing in the store or the label is a secret. Both hold destinations, preset names and
dispositions. Credential *rules* name a service and are not recorded on a sandbox at all;
credential *values* live only in the encrypted store and travel to the supervisor on a pipe.

#### The network supervisor

A sandbox's network is served by one `boks net serve` process per *running* sandbox, started
on demand by whichever command starts the VM and ending when the sandbox's task ends. What it
is trusted with, and what bounds it:

- It holds the **policy** for one sandbox, the **credential values** for that sandbox only,
  and the CA's signing key if that sandbox intercepts anything. The policy arrives already
  resolved: the supervisor never reads the policy store itself, so the rules a sandbox is
  running under are fixed at the moment it starts, and editing the store cannot widen a
  sandbox that is already running.
- It is given those values **on a pipe at startup, never in its command line or environment**,
  and it is never given the passphrase to the encrypted store. A supervisor cannot obtain a
  credential the run was not configured with.
- It runs as the user, with no privilege the CLI does not already have, and it exposes **no
  API**: it listens inside one sandbox's virtual network and on nothing else. There is no
  host socket for a guest, or for another process, to talk to.
- It ends with the sandbox. `boks stop` and `boks rm` take it down explicitly, and it also
  watches the sandbox's containerd task so that a VM dying takes its supervisor with it.
- Liveness is a held file lock rather than a recorded PID, so a stale record can never cause
  a signal to be sent to a process that inherited that number.

The trade-off against a single always-on daemon is deliberate: a daemon would hold *every*
sandbox's credentials and CA for as long as the machine is up, and one compromise or crash
would reach every sandbox at once. One short-lived process per sandbox has a blast radius of
one sandbox and a lifetime of one VM.

#### TLS interception and the Boks CA

**This is a real reduction in the guarantee, and it is the deliberate cost of credential
injection over HTTPS.** Attaching a header to a request means reading the request; reading
an HTTPS request means terminating the TLS session. Every credential worth injecting —
model APIs, Git hosts, package registries — is HTTPS-only, so "we never intercept" and "we
inject credentials" cannot both be true. Boks chooses to intercept, narrowly, and to say so
loudly.

**What is intercepted.** A flow is terminated **only if its destination host has a
credential injection rule configured for it**. Nothing else: no flag, preset or default
turns on wider interception, and the decision is taken from the `CONNECT` target before the
guest has sent a byte. Interception scope and credential scope are the same set by
construction — one predicate (`Injector.Handles`) answers both questions, so a host cannot
be decrypted for no reason. With no CA configured, nothing is ever intercepted and HTTPS
credential rules simply do not fire, which the log records rather than passing over.

**What Boks can read, for those hosts.** Everything: request line, headers, request and
response bodies, in both directions. What it does with that is bounded by what the code
does, not by what it could do:

- nothing derived from traffic is logged. No body, header value or URL reaches the decision
  log, the operational log, or an error message. Parse errors from `net/http` are
  deliberately *not* printed verbatim, because they quote the bytes they choked on and a
  request line can carry a token in a query string. Tests drive canaries through a
  credential, a request body, a response body and a URL query, including the malformed-input
  path, and assert none of them appears anywhere.
- bodies stream through and are never buffered, decoded or examined.
- Boks becomes responsible for verifying the origin, and does: full verification against the
  host's trust store, with the flow refused and a readable `502` returned if it fails.

**What the guest loses.** For intercepted hosts only: end-to-end confidentiality with the
origin, and certificate pinning — a pinned client will fail, visibly, which is the correct
failure. HTTP/2 is not carried inside an intercepted flow; ALPN offers `http/1.1` so clients
negotiate down rather than break. Tunnelled flows are untouched and may use whatever they
like.

**The CA.**

- Generated on this machine, on first use. Key and certificate live under the state
  directory, owner-only (`0600`, in a `0700` directory), and Boks refuses to sign with a key
  other users can read.
- **The private key never leaves the host.** No guest, image, mount or annotation carries
  it, and no code path prints it. `boks ca export` and `boks ca env` hand out the
  certificate only.
- The certificate is public. A guest holding it can *verify* certificates Boks minted; it
  cannot mint any. Exfiltrating it gains an attacker nothing.
- A guest trusting this CA is trusting the host it already runs on — which already owns its
  kernel, disk and clock. That is not a new trust relationship. **Do not install it in your
  host trust store**: in a guest its reach is one sandbox, in your login keychain it is every
  TLS connection you make, and anyone who reads the key file owns them.
- Leaves are minted per host from the *policy target*, never from the guest's ClientHello, so
  a guest cannot choose what gets signed. They are cached in memory and never written to
  disk.
- Revocation is regeneration: no guest checks a revocation list, so `boks ca regenerate`
  replaces the key and everything issued under the old one stops chaining to anything
  trusted. Anything holding the old certificate must be given the new one.

**Transparency.** Every logged flow records how it was carried, using Docker Sandboxes' own
vocabulary: `forward` (Boks handled it at the HTTP level and could read it — plaintext HTTP,
or HTTPS it terminated), `forward-bypass` (tunnelled, ciphertext only), `transparent` (judged
in the network stack by address and port, with the proxy not involved at all — Boks saw a
destination and nothing else). `boks policy ls` and `boks proxy` both state, unprompted,
which hosts will be decrypted.

#### Known limits, stated up front

- **Verified on one host, once.** A real guest was refused on macOS/Apple silicon on
  2026-08-13, through libkrun's virtio-net device and nerdbox's annotations. The Linux/KVM
  path has never been exercised end to end, and one measurement on one platform is evidence
  rather than assurance.
- **A hostname rule does not authorise a raw connection.** Enforcement reads the address in
  the packet, so `--allow example.com` permits the proxied flow and denies a direct
  connection to the address that name resolves to. Fail-closed, but surprising.
- **UDP and ICMP carry no reason a rule could express.** They are refused categorically, so
  the log records the refusal and the category rather than a matching rule — there is no rule
  to name. Recorded once per destination and capped, since the guest chooses the packet rate.
- **A sandbox created before this existed, or by something else, has no wiring** and runs on
  the runtime's default transport, where the guest's `127.0.0.1` is the host's. Boks warns
  loudly when it meets one; the fix is to recreate it, because the mode lives in annotations
  that are fixed when a container is created.
- **A sandbox created before it recorded its policy has no record**, so `boks start` serves it
  the default preset plus whatever the store says about it, exactly as it did before. It is
  not distinguishable from a sandbox that named no policy, and the fix is the same: recreate
  it, or write its rules into the store with `boks policy allow --sandbox NAME`.
- **Credential rules are still per-invocation.** The policy a sandbox remembers covers
  destinations, not credentials, so `boks start` and `boks exec` bring up a stack with no
  injection configured. Deliberate: resolving a credential needs the passphrase to the
  encrypted store, and prompting for one inside `boks start` is a worse trade than a missing
  header. It fails in the safe direction — a request goes out unauthenticated rather than a
  secret going somewhere unexpected — but it is a surprise, and it is unbuilt rather than
  decided against.
- **Two `boks policy` commands running at once can lose a rule.** The store is read, modified
  and written back without a lock; the write itself is atomic, so the file is never
  half-written, but a concurrent pair of writers can leave one of the two rules out. Adding a
  rule is an interactive act, so this has been left rather than solved.
- **If a supervisor is killed, that sandbox's network is gone until the sandbox is
  restarted.** The next `boks run` or `boks exec` starts a fresh stack, but a *running* VM
  does **not** re-attach to it — measured 2026-08-12. A guest whose network vanishes fails
  closed, which is the right direction for the failure to go, but nothing repairs it.
- SNI-based filtering can be evaded by clients that omit or lie about SNI; the netstack's
  destination-IP rules are the backstop. The proxy checks the SNI against policy and drops
  the tunnel on a mismatch, but it can only do so *after* answering `200`, so the client
  sees a broken handshake and the reason lives in the decision log.
- Encrypted Client Hello removes the SNI signal entirely.
- Hostname rules are meaningless for raw sockets — only IP/port rules apply there. The stack
  judges raw flows on addresses, so a hostname-only policy denies them.
- A hostname allow says nothing about the address it resolves to. The proxy re-checks the
  resolved address against the deny rules before dialling, which stops `evil.test A
  127.0.0.1`; it cannot stop a name whose address changes between check and connect.
- **DNS is mediated but not filtered.** The guest's resolver is the sandbox's own gateway,
  and UDP to any other destination is dropped, so a guest cannot query a nameserver of its
  choosing over UDP — which closes the "DNS as a free covert channel" hole. What it does not
  do is judge the *names*: the gateway resolves whatever it is asked through the host's
  resolver, so query names still leak upstream and a low-bandwidth channel remains in the
  names themselves. Rules over names are the natural next thing to attach there, and are
  unbuilt.
- **UDP and ICMP are dropped, and no policy re-enables them.** That matches the reference
  product, and it costs the guest QUIC (clients fall back to TCP), `ping`, and any UDP
  protocol. A workload that genuinely needs one has no way to ask for it today.
- **Every allowed host is an exfiltration destination.** An allowlist bounds *where* data can
  go, not whether it goes. This is why the default preset is short and exact.
- A flow marked for interception that turns out not to be TLS for the host that was judged —
  no SNI, a mismatched SNI, or a protocol that is not TLS at all — is carried blind instead
  and gets no credential. The log records the downgrade rather than leaving a `forward` entry
  standing for a flow Boks never read.
- Host patterns come in one wildcard form (`*.example.com`, matching any depth of subdomain
  but not the apex). Docker Sandboxes' v2 permission grammar distinguishes single-label from
  multi-label wildcards; Boks cannot express "exactly one label". See the parity matrix.

### The measured baseline today

*(verified 2026-08-11, macOS host, nerdbox defaults.)* What a sandbox can actually reach
with no policy in place:

| Probe | Result |
|---|---|
| virtio-net device in guest | **none** — `/sys/class/net` lists only `lo` |
| DNS | resolves (host's nameserver, via a `/etc/resolv.conf` copied from the host) |
| outbound HTTPS / HTTP | succeeds |
| raw TCP to an arbitrary host:port | connects |
| ICMP | fails — `Network unreachable` |
| **host loopback services** | **reachable** — a host listener on `127.0.0.1` answered the guest |

The absent NIC is the important detail: this is libkrun's TSI, which rewrites the guest's
`AF_INET` socket calls and performs the connection **on the host**. So the guest's
`127.0.0.1` is *the host's* `127.0.0.1`. Anything bound to host loopback — a database, a
model server, a debug endpoint, an unauthenticated dev server — is inside the sandbox's
reach, and no `HTTP_PROXY` setting changes that because the guest never has to cooperate.

This is the single largest gap between Boks today and its stated goals, and it is the
argument for the external-network-provider direction above: with TSI there is no point at
which Boks can see or drop a flow.

**This baseline is what a sandbox gets when Boks does *not* wire it** — the runtime's own
default. Every sandbox `boks run` creates now carries the annotations that replace it, and
`--net none` is the same replacement without the container being wired to the NIC at all.

The same probes with an external network provider attached *(verified 2026-08-11, same
host)*:

| Probe | TSI (default) | provider attached |
|---|---|---|
| interfaces in the guest | `lo` only | `lo` + `eth0` |
| host service on `127.0.0.1` | **answers the guest** | **connection refused** |
| guest `/etc/resolv.conf` | a copy of the host's | the host-side gateway |
| IPv6 | absent entirely | guest emits link-local traffic |

The refusal is the discriminator: the call is being handled by the guest's own loopback
stack, where nothing listens, rather than performed on the host. That is the whole reason the
provider is worth its complexity. A real guest has since been refused by that stack: see the
check 6 transcript in [verification.md](verification.md).

### Host services

Boks exposes no listening service **on the host** to the guest, and no host API the guest can
call. The one thing a guest can talk to is the filtering proxy, which listens inside that
sandbox's own virtual network — reachable from that sandbox alone, offering no way to
enumerate host state, and refusing anything the policy does not permit. It cannot be used to
request a secret: the guest names a destination, and the host decides on its own whether a
credential is attached. Port
forwarding, when implemented, is explicit and host-initiated. The guest cannot ask for a
port to be published, cannot enumerate host state, and cannot request secrets.

Local MCP servers are worth a specific warning: in Docker's model they run *outside* the VM
with host privileges. Any such bridge is a hole through the boundary. Boks does not
implement one, and if it ever does, it will be an explicit, per-server opt-in.

### Docker daemon placement

The nested daemon must run **inside** the guest.

Forwarding the host `/var/run/docker.sock` into a sandbox grants trivial host root — mount
`/` into a privileged container and you own the machine. Boks will never mount a host
container-runtime socket into a guest. This is a design invariant, not a default.

### Credentials

Target property: the raw secret never enters the guest. The guest gets a placeholder; the
host proxy attaches the real credential to requests going to explicitly configured hosts.

This bounds the damage from a compromised agent to *use* of the credential against approved
destinations while the sandbox runs — it does not let the agent keep, print or exfiltrate
the secret. That is a meaningful reduction, not an elimination: an agent that can make
requests through the proxy can still make *authenticated* requests.

The mechanism exists in `internal/secret` and works through `boks proxy`, over HTTP **and
HTTPS**. A credential names a service and owns a set of injection rules; each rule names a
domain, a header and a value format with one `%s` (`bearer` and `basic[:user]` are
shorthands for the two common shapes). Several hosts can share one stored secret, which is
why the model has two levels. A catch-all `*` domain is rejected — under this design it
would not merely send a token anywhere, it would also decide to decrypt everything.

Secrets live in an AES-256-GCM file keyed from a passphrase; names are encrypted too. There
is **no host API a guest can call** to list or fetch a secret, and adding one would end the
guarantee — the only consumer of the store is the proxy's own request path. Values are
wrapped in a type whose printed and JSON forms are redacted, and tests assert a secret
cannot reach a log, including on the intercepted path.

Placeholders are part of the credential, not a constant: a guest is meant to hold something
*shaped like* a real credential for that service, because clients validate credential format
locally and a marker like `boks-managed` makes them fail before a request reaches the proxy.

Three limits worth being explicit about:

- **HTTPS injection costs a TLS interception, per configured host.** See
  [TLS interception and the Boks CA](#tls-interception-and-the-boks-ca). This is the one
  place Boks deliberately reads a guest's traffic, and it happens only for hosts you named.
- **An injected credential is still usable by the guest.** A compromised agent can make
  authenticated requests to the approved destinations for as long as the sandbox runs. It
  cannot keep, print or exfiltrate the value.
- **The file store is only as strong as the passphrase.** It is the portable fallback until
  the OS keychains are implemented. A key file stored beside the encrypted file is
  deliberately not offered: it protects nothing while appearing to.

*(none of it is wired into `boks run`. Any credential you put in a sandbox today is simply
in the sandbox.)*

#### OAuth credentials, and how they change the answer

Most people running the flagship agent have no API key at all. `claude /login` leaves an
OAuth **pair** — an access token and a refresh token — in the macOS Keychain under
`Claude Code-credentials`, and nothing else. A credential model that only knows how to set a
header cannot carry that, so `boks run claude` could not work for them by any route. The
`oauth` mechanism in `internal/secret` is that route, and it follows Docker Sandboxes' kit v2
`oauth` block: sentinels in the guest, substitution on the resource hosts, refresh on the
host.

`boks secret adopt` reads what the agent already wrote and stores it. The guest is given
**sentinels** — fakes carrying the real prefix and the real length, because Claude Code checks
that an OAuth token starts with `sk-ant-oat01-` before it will send one — in an environment
variable, a read-only credential file, or both. On a request to a configured resource host
the proxy replaces the sentinel with the real access token, in the `Authorization` header and
no other.

##### Acquisition: how a credential enters the system on a machine with no login

`adopt` needs a login to read. On a fresh machine there is none, and Boks still will not run
an OAuth flow of its own — every one begins by identifying *this program* to the vendor with
a client id issued to a registered application, and Boks holds none. That refusal stands, and
`boks secret set --oauth` still explains it.

What Boks does instead is what Docker Sandboxes does for the same vendor: **let the agent log
itself in, inside the sandbox, and keep the result.** sbx's own host-side `--oauth` is
documented for one vendor only; its answer for Anthropic is `sbx run claude … -- auth login`.
The agent performs its own OAuth with its own client id, which is legitimate because it *is*
that program, and the sandbox's proxy already terminates TLS for the token endpoint.

    boks secret login claude-code       # arms a credential-shaped hole: no tokens
    boks run claude -- auth login       # the agent logs in; boks keeps what comes back

**The two paths through the token endpoint are opposites, and the difference is the security
argument.** A *refresh* is never forwarded: Boks composes its own request on the host and
answers the guest from the sentinels it already handed out. An *acquisition* is forwarded and
its response kept: the authorization code and PKCE verifier that unlock the exchange exist
only inside the guest, because they came from a redirect Boks never saw. So this is the one
place a guest-composed request reaches a token endpoint, and the one place a response body is
buffered rather than streamed.

Which path applies is decided by the **stored record**, never by anything in the request. A
credential relays exactly once, while it holds no token at all, and the first token it
acquires closes the door permanently. A guest cannot ask to be relayed and cannot re-open the
path. (Docker's kit format documents `passthrough: boolean to skip sentinel masking` on its
`oauth` block, which is very likely this same distinction seen from the other side — a
relayed response is the only kind that needs masking. Boks has no such switch and should not.)

**The window, because that is the question worth asking.** There is none: the guest never
holds a real token at any point.

- Before the exchange the guest holds an authorization code and a PKCE verifier. Neither is a
  token: single-use, redeemable only at the token endpoint, worth nothing once spent.
- The response carrying the real pair is read by Boks, which terminated the TLS session. The
  tokens are taken out and stored, and every occurrence of either value is replaced anywhere
  in the document — not only in the two named fields — before a byte is written to the guest.
  The result is asserted clean; if a real value would survive, the login fails instead.
- What the guest receives is the origin's own JSON with sentinels in it. The agent then
  writes *that* into its own credential file, which was confirmed by driving the real Claude
  Code through a stand-in origin: it persists exactly what the endpoint returned.
- The store is written **before** the guest is answered. The reverse ordering would lose the
  credential entirely on a storage failure, and an authorization code cannot be spent twice.

Four things are fail-closed rather than best-effort, and each would otherwise be a way for a
real token to reach the guest unread: a response body larger than 1 MiB; a response still
carrying a content encoding after the relayed request asked for identity; a **successful**
status whose body Boks could not read as a token response, which is the shape a token in an
unrecognised form would take; and a body in which masking could not remove every occurrence.
All four refuse the exchange. A *failed* login is the one thing passed through untouched,
because only the agent can tell the user that its code was spent — and there is no token in
such an answer to mask.

**What acquisition does not protect against, stated plainly.** Boks captures the exchange it
can see. A guest that reached a token endpoint Boks is not intercepting would keep what it
got — that is not a token Boks ever held, but it is a real credential inside the sandbox. Two
consequences follow. The token endpoint host recorded for a provider has to be the one the
agent actually talks to, which is why it was read off the binary rather than assumed (Claude
Code 2.1.228 uses `platform.claude.com`, and contains no reference to `console.anthropic.com`
at all). And the network policy still has to be narrow: a login is only captured because the
proxy is the only way out.

**What this changes about a compromised guest**, stated as the difference from an API key:

- **The credential is bigger.** An API key is usually scoped and revocable on its own; a
  subscription access token is the account's login. "Use of the credential against approved
  hosts" therefore now means use of the subscription, and the injection domains are the only
  thing bounding it. Name them narrowly.
- **The durable half never enters the sandbox, and cannot be reached from it.** An access
  token expires; a refresh token is the credential that outlives the sandbox, and is the one
  worth stealing. Boks never substitutes a refresh token into any request, and the guest's own
  token request is **answered rather than forwarded** — Boks composes the reply from the
  sentinels it already gave out, so no request a guest can compose reaches the token endpoint
  carrying the real refresh token, and no real token ever travels toward the guest in a
  response body. The alternative design (relay the guest's refresh with the real token
  substituted in, then rewrite the response) was rejected for exactly this: a request Boks did
  not understand would produce a response it could not rewrite.
- **Expiry is a host concern, and the guest is told so.** The credential file and the answer
  to a refresh report a lifetime a year out. The real token is refreshed on the host when it
  nears expiry, and whatever is current at that moment is what goes on the wire. An agent that
  persists what a refresh returned writes back the same sentinels it had, which is why the
  credential file can be mounted read-only — the write fails, and the file was already right.
- **A rotation inside a sandbox is not durable, and neither is an acquisition.** The network
  supervisor deliberately never learns the store's passphrase, so a refresh *or a login* it
  performs lives only as long as that sandbox. For a provider that rotates refresh tokens —
  Anthropic does — the copy in the encrypted store is then stale and `boks secret adopt` has
  to be run again. The decision log says so at the moment it happens. Running the credential
  through `boks proxy` instead does persist, because that process has the passphrase.

  For acquisition this is the sharper limitation of the two, and it is the one thing standing
  between `boks secret login` and the fresh-machine story it exists for: the login works, the
  agent is authenticated, and the credential is gone when the sandbox stops. The fix is the
  writeback channel `internal/secret/memory.go` already names as missing, and it is not built.

Residual exposure worth knowing, none of it hypothetical:

- **An origin that echoes `Authorization` back would hand the guest the real token.** That is
  why substitution is restricted to the headers the credential is actually sent in rather than
  applied to every header a sentinel appears in; a guest can put its sentinel anywhere, and
  only the allowlisted header is swapped. Bodies are streamed and never scanned, so a client
  that puts its token in a body is not supported rather than silently unprotected.
- **A resource host is a decrypted host.** Both the API hosts and the token endpoint are
  credential hosts, so both have their TLS terminated. The token endpoint is decrypted for a
  second reason as well: a request has to be answered, which cannot be done through a tunnel.
- **Other paths on the token endpoint's host are ordinary traffic.** Only the configured path
  is answered; everything else on that host is forwarded with no credential attached.
- **An OAuth token is never written onto a plaintext flow.** The guest chooses the scheme, and
  `http://` to a resource host would otherwise be a downgrade the guest controls. Such a
  request goes out carrying the sentinel and fails at the origin.
- **The macOS Keychain read has never been executed.** It shells out to `security
  find-generic-password`, which cannot run on the Linux machine this was written on. What is
  tested is everything either side of it — the parsing of the document it returns, from a file
  and from stdin, and the refusal on a platform with no Keychain. The `security` invocation
  itself is unproven. Confirming it needs one run of `boks secret adopt claude-code` on a Mac
  where `claude /login` has been used.
- **The refresh request's shape is unproven against the real provider.** Boks composes an
  RFC 6749 `refresh_token` grant, as JSON by default, and posts it to the configured endpoint.
  That has been demonstrated against a local endpoint, not against `platform.claude.com`. The
  *shape* is no longer a guess, though: the real Claude Code's own refresh was observed and is
  a JSON `refresh_token` grant to that endpoint — see
  [verification.md](verification.md#the-real-claude-code-login-flow-observed).
- **No acquisition has ever run against the real provider either.** Everything below the
  network — the relay, the capture, the masking, the refusals — is exercised by tests against
  a local origin, and the *shape* of the exchange those tests replay was copied from a real
  Claude Code driven against a stand-in origin. What has not happened is one real login
  through a real sandbox against Anthropic.
- **The claude agent's own allowlist still names the old endpoint.** `internal/agent` allows
  `console.anthropic.com:443` and not `platform.claude.com:443`, so a login through a sandbox
  is denied at the network layer until a rule is added. `boks secret login` prints the
  `boks policy allow` line for exactly this reason, and the agent row wants correcting.

### Privileged execution

- Boks does not require a setuid helper.
- Boks requires access to `/dev/kvm` on Linux, which normally means membership of the `kvm`
  group. That is host-level privilege worth understanding, not something Boks grants itself.
- The guest process runs as whatever the image specifies, commonly root — inside the guest.
  That is expected and is not a weakening of the boundary.
- Boks does not currently require root on the host beyond whatever your containerd setup
  needs. A rootless containerd is recommended.

## Likely escape surfaces, ranked

1. **libkrun / hypervisor** — virtio device emulation, especially virtiofs. Largest surface,
   least under our control.
2. **nerdbox shim** — parses guest-influenced data and runs on the host with your privileges.
3. **Workspace write-back** — the highest-probability *practical* attack: no exploit needed,
   just a malicious `Makefile` you later run. Mitigated by review, and eventually clone mode.
4. **containerd configuration** — a misconfigured or over-privileged containerd undermines
   everything above it.
5. **Network policy gaps** — the policy is in the datapath and enforced against a real
   guest, so the remaining gaps are specific: a hostname-only rule does not authorise a raw
   connection to the address it resolves to, UDP and ICMP drops are unlogged, a sandbox
   created without Boks' annotations still runs on a transport that reaches host loopback,
   and a sandbox whose supervisor is killed loses its network until something restarts it.
6. **The network supervisor** — a host process holding one sandbox's credentials and, if that
   sandbox intercepts anything, the CA's signing key. It exposes no API and ends with the
   sandbox, which is what bounds it.
7. **Terminal escape sequences** in guest output.
8. **The interception CA** — a signing key on your machine. Its blast radius is bounded by
   what trusts it, which should be sandboxes and nothing else. A key stolen off the host is
   worth a MITM against anything that was told to trust it; that is why it is owner-only, why
   Boks refuses to use a key others can read, and why `boks ca regenerate` exists.

## What Boks does not claim

- It has **not** been security-reviewed or audited.
- It has **not** demonstrated the VM boundary in this project's own testing.
- Its network enforcement **has** been demonstrated against a real guest, once, on one host
  (macOS/Apple silicon, 2026-08-13). One measurement on one platform is evidence, not
  assurance, and nothing here has been reviewed by anyone but its authors.
- It does **not** claim that a sandbox created before this wiring existed is contained. Such
  a sandbox runs on the runtime's default transport and reaches host loopback; Boks says so
  when it sees one, and recreating it is the only fix.
- It does **not** claim end-to-end TLS to every destination any more. Hosts you configure a
  credential for are decrypted by Boks, by design; everything else is not.
- It does **not** claim that reading a credential out of the macOS Keychain works. That path
  has never been executed; see [OAuth credentials](#oauth-credentials-and-how-they-change-the-answer).
- It has **no** defence against hypervisor vulnerabilities beyond keeping libkrun current.

Boks aims to be honest about this. If a property is not listed as tested, assume it does not
hold.
