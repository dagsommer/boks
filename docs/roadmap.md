# Roadmap

What is not built, and what gets built next. This is a list of gaps rather than a plan with
dates: nothing here is scheduled, and the order is what unblocks the most rather than what is
most wanted.

The VM boundary is done and the network datapath enforces. What matters most now is watching
that work against a real guest on more than one platform.

## Next

1. **A repair path for a crashed network supervisor.** A running VM does not re-attach to a
   restarted stack — measured on 2026-08-12 — so today the sandbox loses its network until it
   is restarted. Boks says exactly that when it meets one and gives the `stop && start` that
   fixes it; it does not restart the sandbox itself, because that kills whatever is running
   inside.
2. **Policy over names for raw flows**, so that `--allow example.com` can authorise a direct
   connection to the address it resolves to rather than denying it.
3. **The interactive dashboard** that bare `boks` should open.
4. **Clone mode**, so guest writes do not land on the host by default.
5. **Docker daemon inside the guest.**
6. **UDP port publishing**, which needs the link filter to carry a datagram's return path
   without becoming a general hole.
7. **Kits / declarative configuration**, which is also what would let an agent be defined in a
   file rather than in code.
8. **Windows** — see below.

## What is not done

Grouped by what it costs you, and stated the way it will be measured rather than the way it
would sell.

### Platforms

- **Linux is untested in practice.** The boundary was verified on macOS on Apple silicon; the
  Linux/KVM path is designed for but has not been exercised end to end.
- **Windows does not run Boks natively** — and the obstacle is one device driver, not the
  platform. libkrun's Windows Hypervisor Platform backend is in progress upstream for
  libkrun 2.0, and nerdbox already builds a Windows shim for it. **`virtio-net` is the single
  device not yet ported**, which is exactly the one Boks' enforcement depends on. In the
  meantime Boks should run **inside WSL2** with nested virtualisation: unchanged, with
  workspace paths preserved exactly. Untested, but every ingredient is there — see
  [Windows](windows.md).

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

- **Published ports are TCP only.** The grammar accepts `udp`, `udp4` and `udp6` because
  Docker Sandboxes' does, and refuses them with the reason: the sandbox's network stack drops
  UDP at the link, so a datagram has no way back. Publishing UDP would mean widening that
  filter, which is a change to the stack's closed posture rather than a port flag.
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

- **No terminal dashboard** for bare `boks`, and no `--clone` or `--kit`. See the CLI surface
  section of the [parity matrix](docker-sandbox-parity.md).
- **No nested Docker** and no kits.
- **Ctrl-C reports badly.** It cleans up completely, but exits 1 with an RPC error rather than
  exiting 130 silently.

## Not planned

- **Org governance and an audit control plane.** Docker Sandboxes has this, as a paid
  offering. Boks will not replicate it.
- **Telemetry of any kind**, opt-out or otherwise.
- **An account.**

## How this list stays honest

Everything above is a gap somebody wrote down after meeting it, not a feature grid with empty
cells. Two of the entries — the crashed supervisor and the raw-socket bypass — exist because
the behaviour was *measured* and the measurement failed;
[Verification](verification.md) records both, including the one that was later measured fixed.

If you find something on this page that is no longer true, that is a bug in the page. The
feature-by-feature comparison with the reference implementation, with priorities, is the
[Docker Sandbox parity matrix](docker-sandbox-parity.md).
