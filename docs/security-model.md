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
- **Symlinks.** A workspace symlink pointing outside the shared directory is resolved by
  virtiofs on the host side. Whether that escapes the share depends on the implementation;
  Boks has not tested it. Treat as unverified.

### Network

The design principle: **an environment variable is not a security boundary.** A guest that
ignores `HTTP_PROXY` and opens a raw socket must still be stopped.

Intended enforcement is a host-side userspace network stack terminating the guest's
virtio-net link, so every packet is examined on the host and unapproved flows are dropped
regardless of guest cooperation. HTTP/HTTPS is steered to a host forward proxy that filters
on the `CONNECT` target and TLS SNI, without interception — no custom CA in the guest, so
end-to-end TLS is preserved and the proxy cannot read request bodies.

Known limits of that approach, stated up front:
- SNI-based filtering can be evaded by clients that omit or lie about SNI; the netstack's
  destination-IP rules are the backstop.
- Hostname rules are meaningless for raw sockets — only IP/port rules apply there.
- DNS is a covert channel unless resolution is also mediated.

*(unverified: none of this is implemented. Boks currently applies **no** network policy —
a sandbox has whatever access the runtime gives it.)*

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

*(unverified: not implemented. Any credential you put in a sandbox today is simply in the
sandbox.)*

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
5. **Network policy gaps** — currently total, since no policy exists.
6. **Terminal escape sequences** in guest output.

## What Boks does not claim

- It has **not** been security-reviewed or audited.
- It has **not** demonstrated the VM boundary in this project's own testing.
- It provides **no** network isolation today.
- It provides **no** credential protection today.
- It has **no** defence against hypervisor vulnerabilities beyond keeping libkrun current.

Boks aims to be honest about this. If a property is not listed as tested, assume it does not
hold.
