# Roadmap

What is not built, and what gets built next. This is a list of gaps rather than a plan with
dates: nothing here is scheduled, and the order is what unblocks the most rather than what is
most wanted.

The VM boundary is done and the network datapath enforces, and both have now been watched
from inside a real guest on macOS, Windows and Linux. What matters most now is closing the
gaps those runs exposed — Linux still needs root to create a sandbox — and getting a release
out that people can actually install.

## Next

1. **A repair path for a crashed network supervisor.** A running VM does not re-attach to a
   restarted stack — measured on 2026-08-12 — so today the sandbox loses its network until it
   is restarted. Boks says exactly that when it meets one and gives the `stop && start` that
   fixes it; it does not restart the sandbox itself, because that kills whatever is running
   inside. The commonest way to *reach* that state was boks: `boks run` ended the stack
   whenever the command returned an error, and a guest command that exits non-zero is one.
   Measured on Windows on 2026-08-16, fixed the same day. What is left here is a genuinely
   crashed supervisor.
2. **Sandbox creation on Linux without root**, which today fails for an ordinary user on a
   host mount Boks makes to read the image config — see below.
3. **Policy over names for raw flows**, so that `--allow example.com` can authorise a direct
   connection to the address it resolves to rather than denying it.
4. **The interactive dashboard** that bare `boks` should open.
5. **Clone mode measured behind a hypervisor.** `--clone` is built and verified against runc,
   where the read-only source share is a bind mount rather than virtiofs; nothing about it
   has been measured behind a VM boundary yet.
6. **Docker daemon inside the guest.**
7. **UDP port publishing**, which needs the link filter to carry a datagram's return path
   without becoming a general hole.
8. **Kits / declarative configuration**, which is also what would let an agent be defined in a
   file rather than in code.

## What is not done

Grouped by what it costs you, and stated the way it will be measured rather than the way it
would sell.

### Platforms

- **Linux needs root to create a sandbox.** The platform was verified end to end for the
  first time on 2026-08-15, in WSL2 on Ubuntu 26.04, and the boundary and the network policy
  both held. But as an ordinary user, creation fails with `mount source: "overlay", err:
  operation not permitted` — that is Boks itself host-mounting the image overlay to read the
  image config. Windows no longer has an equivalent requirement, which is the wrong way
  round. Whether the metadata-only path that answered it on Windows transfers to Linux is
  under investigation.
- **No bare-metal Linux run.** The Linux verification happened inside WSL2. KVM is KVM
  either way, but nothing on this project has run on a bare-metal Linux host, and the shim
  also turned out to need containerd ≥ 2.3 rather than the 2.2 this page used to claim.
- **Windows is one machine, and x64 only.** `boks run` boots a sandbox natively through the
  Windows Hypervisor Platform from an ordinary unelevated terminal, with policy enforced and
  teardown clean — but every Windows result comes from a single Windows 11 x64 host. There is
  no Windows arm64 build at all: the WHP backend and the guest kernel are both x86_64. The
  WHP backend lives in this repository's [patch series](../packaging/libkrun-windows/)
  against libkrun rather than upstream. See [Windows](windows.md).
- **No release exists on any platform.** The packaging is built and the manifests are
  written, but no tag has been cut, so every install route is source-built today.

### Network

- **A hostname-only policy denies raw connections, including to the allowed host.** A raw
  socket carries no name, so it is judged on the address; `--allow example.com` therefore does
  not permit a direct connection to example.com's address. That fails closed, which is the
  safe direction, but "allowed through the proxy" and "allowed on a raw socket" are different
  questions and the CLI does not say so at the point you write the rule.
- **UDP and ICMP drops are silent.** TCP denials are logged with a reason; a guest probing UDP
  or ICMP leaves nothing in `boks policy log`. An observability gap, not a containment one.
- **No policy over names, and no UDP.** DNS is mediated by the sandbox's own resolver and
  cannot be sent anywhere else, but the names themselves are not filtered. UDP and ICMP are
  dropped with no way to ask for them, which costs QUIC and `ping`.
- **A crashed network supervisor is unrecoverable without a restart.** See above.

### Ports

- **Published ports are TCP only.** The grammar accepts `udp`, `udp4` and `udp6`, because
  that is the port syntax people already type, and refuses them with the reason: the
  sandbox's network stack drops UDP at the link, so a datagram has no way back. Publishing
  UDP would mean widening that filter, which is a change to the stack's closed posture rather
  than a port flag.
- **Publishing has never been driven by a real guest.** The datapath is exercised end to end
  against a simulated one — a second gvisor stack on the far end of the real link socket —
  which proves the host side works. A real VM reaching it through libkrun's virtio-net device
  has not been tried.

### Credentials

- **No host-side OAuth login.** `boks secret set NAME --oauth` is recognised and refused,
  because every flow that could acquire a token starts by identifying the program to the
  vendor with a client id issued to a registered application, and Boks holds none.
  `boks secret adopt` takes over a login you have already performed, which covers the same
  case on a machine you have used the agent on and nothing at all on a fresh one.
- **Two of the eleven known services have no rule.** Neither Cursor nor Factory documents the
  host their CLI sends its API key to, so `boks secret set cursor` and `boks secret set droid`
  refuse and explain rather than guessing.
- **A credential cannot be scoped to one sandbox.** A stored credential applies to every
  sandbox; `--no-secrets` turns all of them off for a run, and there is nothing in between.

### Agents and images

- **Only the base image has run in a microVM.** The agent layers were exercised with
  `docker run`, which proves each CLI is installed and starts — and nothing about isolation.
- **`kiro` has no image**, and there is still no way to define an agent in a file rather than
  in code. See [Agents](agents.md#the-one-with-no-image).
- **Five agents carry an empty allowlist**, because no vendor documentation naming their
  destinations has been found. Their users write one `boks policy allow` after seeing the
  denial in `boks policy log`.

### CLI

- **No terminal dashboard** for bare `boks`, and no `--kit`. See the CLI surface section of
  the [parity matrix](docker-sandbox-parity.md). `--clone` exists, and has only been
  exercised against runc — see item 5 above.
- **No nested Docker** and no kits.
- **Ctrl-C reports badly.** It cleans up completely, but exits 1 with an RPC error rather than
  exiting 130 silently.

## Not planned

- **Org governance and an audit control plane.** A fleet-management layer is a product in its
  own right, and a different one from this. Boks will not grow one.
- **Telemetry of any kind**, opt-out or otherwise.
- **An account.**

---

If you find something on this page that is no longer true, that is a bug in the page. The
evidence behind every "verified" above is in [Verification](verification.md).
