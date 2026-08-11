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

Intended enforcement is a host-side userspace network stack terminating the guest's
virtio-net link, so every packet is examined on the host and unapproved flows are dropped
regardless of guest cooperation. HTTP/HTTPS is steered to a host forward proxy that filters
on the `CONNECT` target and TLS SNI, without interception — no custom CA in the guest, so
end-to-end TLS is preserved and the proxy cannot read request bodies.

#### What exists today

| Piece | State |
|---|---|
| Policy engine — exact/wildcard hosts, IP and CIDR rules, ports, deny precedence, presets, decision log (`internal/policy`) | built, unit-tested |
| Host forward proxy — HTTP, `CONNECT`, SNI cross-check, no TLS interception (`internal/proxy`) | built, exercised end to end against real TLS origins |
| Credential injection, encrypted host-side store (`internal/secret`) | built, unit-tested |
| Network annotations and host-stack supervision (`internal/network`) | built, unit-tested |
| Any of it applied to a running sandbox | **not done** |

**The proxy is not an enforcement boundary.** It filters only traffic a client chooses to
send it. Nothing is wired into `boks run`, and even when it is, a proxy alone would remain a
cooperating-client mechanism. `boks run -allow …` today validates the rules, prints them,
and says plainly that they are not applied.

#### What the enforcement path now rests on

*(verified 2026-08-11, macOS host with a hypervisor.)* An external network provider **does**
displace TSI. With nerdbox's defaults the guest has `lo` only and a host service on
`127.0.0.1` answers it; with a virtio-net link to a host-side stack, the guest has `eth0`
and the same probe is **refused** — the connection is handled by the guest's own loopback
stack instead of being impersonated on the host. So there is now a point at which Boks can
see and drop a flow. **No policy has yet been enforced against a real guest**; the transport
is verified, the enforcement built on it is not.

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
  `-net none`, and it is the strongest containment Boks can currently offer — the only
  posture that does not depend on unfinished enforcement code.

#### Known limits, stated up front

- SNI-based filtering can be evaded by clients that omit or lie about SNI; the netstack's
  destination-IP rules are the backstop. The proxy checks the SNI against policy and drops
  the tunnel on a mismatch, but it can only do so *after* answering `200`, so the client
  sees a broken handshake and the reason lives in the decision log.
- Encrypted Client Hello removes the SNI signal entirely.
- Hostname rules are meaningless for raw sockets — only IP/port rules apply there.
- A hostname allow says nothing about the address it resolves to. The proxy re-checks the
  resolved address against the deny rules before dialling, which stops `evil.test A
  127.0.0.1`; it cannot stop a name whose address changes between check and connect.
- DNS is a covert channel unless resolution is also mediated. Pointing the guest's resolver
  at the host-side gateway is the hook for that; it does not by itself close the channel.
- **Every allowed host is an exfiltration destination.** An allowlist bounds *where* data can
  go, not whether it goes. This is why the default preset is short and exact.

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

The same probes with an external network provider attached *(verified 2026-08-11, same
host)*:

| Probe | TSI (default) | provider attached |
|---|---|---|
| interfaces in the guest | `lo` only | `lo` + `eth0` |
| host service on `127.0.0.1` | **answers the guest** | **connection refused** |
| guest `/etc/resolv.conf` | a copy of the host's | the host-side gateway |
| IPv6 | absent entirely | guest emits link-local traffic |

The refusal is the discriminator: the call is being handled by the guest's own loopback
stack, where nothing listens, rather than performed on the host. That is the whole reason
the provider is worth its complexity — and it is *all* that has been shown. Boks still
applies no policy to a running sandbox.

### Host services

Boks exposes no listening service to the guest, and no host API the guest can call. Port
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

The mechanism exists in `internal/secret` and works through `boks proxy`: bearer, basic and
arbitrary-header schemes, scoped to host patterns you write out, with a catch-all `*`
rejected. Secrets live in an AES-256-GCM file keyed from a passphrase; names are encrypted
too. There is **no host API a guest can call** to list or fetch a secret, and adding one
would end the guarantee — the only consumer of the store is the proxy's own request path.
Values are wrapped in a type whose printed and JSON forms are redacted, and a test asserts a
secret cannot reach a log.

Two limits worth being explicit about:

- **HTTPS injection is not possible without terminating TLS, so it is not done.** Injection
  applies to plaintext HTTP only. Inside a `CONNECT` tunnel the proxy sees ciphertext.
  Docker Sandboxes injects into HTTPS, which means it terminates TLS somewhere; Boks will
  not do that silently. Closing the gap means an explicit, per-host opt-in to interception.
- **The file store is only as strong as the passphrase.** It is the portable fallback until
  the OS keychains are implemented. A key file stored beside the encrypted file is
  deliberately not offered: it protects nothing while appearing to.

*(none of it is wired into `boks run`. Any credential you put in a sandbox today is simply
in the sandbox.)*

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
5. **Network policy gaps** — currently total in the datapath: a policy engine and proxy
   exist, but nothing applies them to a sandbox, and the default runtime configuration still
   reaches host loopback.
6. **Terminal escape sequences** in guest output.

## What Boks does not claim

- It has **not** been security-reviewed or audited.
- It has **not** demonstrated the VM boundary in this project's own testing.
- It provides **no** network isolation in a running sandbox today. A policy engine, a host
  proxy and a host network stack configuration exist and are tested; none is wired into
  `boks run`.
- It provides **no** credential protection in a running sandbox today.
- It has **no** defence against hypervisor vulnerabilities beyond keeping libkrun current.

Boks aims to be honest about this. If a property is not listed as tested, assume it does not
hold.
