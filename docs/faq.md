# FAQ

Questions that are not failures. If something is broken, [Troubleshooting](troubleshooting.md)
is the other page.

## What is Boks?

A local-first, open-source alternative to Docker Sandboxes (`docker sbx`). It runs coding
agents and other untrusted developer tooling inside isolated microVMs, on your own machine.
No account, no cloud service, no telemetry.

## Is it a hypervisor?

No. Boks is the orchestration layer:

```
boks CLI → containerd → containerd-shim-nerdbox-v1 → libkrun → microVM
                                                                 ├── workspace via virtiofs
                                                                 └── your command
```

Everything below the CLI is existing open-source software. Boks creates a container whose
runtime handler is `io.containerd.nerdbox.v1`, which boots a microVM per sandbox. See
[Architecture](architecture.md).

## Is it a container?

No, and the difference is the point. A sandbox is a separate kernel. On a real run the guest
reported a Linux kernel while the host ran Darwin, with its own `boot_id`, an uptime of
hundredths of a second against the host's weeks, and vCPU and memory counts matching the
flags rather than the host's. A shared-kernel container cannot produce any of that.
[Verification](verification.md) has the evidence and the procedure to repeat it.

## Can I run it on Windows?

Not yet, natively — though the stack underneath Boks now does. A Windows Hypervisor Platform
backend for libkrun is being built in this repository's patch series, alongside a patched
containerd, a patched nerdbox shim and a `mkfs.erofs` for Windows. On 2026-08-14, on real
Windows 11 hardware, `ctr tasks start` ran a Linux container end to end through all of it:
the container's stdout came back, `uname -a` reported the 6.12.44 guest kernel rather than the
Windows host — which a shared-kernel container cannot do — and `/proc/uptime` advanced.
`t_boot=1.90127s`, `t_create=121.648ms`.

**A container running under `ctr` is not `boks run`.** Boks' own path stops earlier, and
deliberately: it declines to start sandbox networking on Windows, because no Ethernet frame
has yet crossed the virtio-net device there, and its enforcement is the network datapath.
`boks doctor` fails its platform check on Windows for the same reason, correctly. Running a
container also still needs an elevated shell or Developer Mode, for a symlink containerd
creates in the task bundle.

So the honest line has moved, but not to "yes": everything below Boks works on Windows, and
the part Boks is has not been run there.

Inside WSL2 it should work unchanged, with workspace paths preserved exactly, and nobody has
run it. See [Windows](windows.md) and [Troubleshooting](troubleshooting.md#wsl2).

## Does it work on Linux?

The KVM path is built and designed for. It has not been exercised end to end by anyone on
this project, so you would be the first. See
[Get started](get-started.md#which-platforms-work).

## Do I need Docker Desktop?

No. Nor Docker Sandboxes, nor an account anywhere. Boks needs containerd, nerdbox and
libkrun, and `boks doctor` checks for each.

## Does Boks send anything anywhere?

No. There is no telemetry, no analytics and no phone-home, and the documentation site makes
no third-party requests either — no CDN, no fonts, no trackers.

That is not the same as saying a *sandbox* sends nothing: the agent inside it talks to its
vendor's API, which is what you ran it for. What that agent may reach is the
[network policy](usage.md#policy-is-state-not-an-argument), and telemetry endpoints are
deliberately left out of the defaults.

## Does my API key end up inside the sandbox?

No. The guest holds a placeholder shaped like a real key — the vendor's own prefix, so the
client's own format check passes — and the host proxy swaps it for the real credential on
requests to the hosts you named, and no others.

The cost is that those hosts, and only those, have their TLS terminated by Boks. Every run
says so out loud the first time a sandbox meets each one, including under `--quiet`.

## What can escape the sandbox?

Assume code in the sandbox is hostile. The hypervisor is the boundary; in-guest permissions
are not — the agent has root in the guest by design.

The two things most likely to surprise you:

- **Workspace writes are live on your host.** A sandbox can modify `Makefile`,
  `package.json` scripts, Git hooks or CI config, which then run on *your* machine. Review
  diffs before running anything from a workspace a sandbox touched.
- **A published port is a hole from your machine into a VM running code you have not
  audited.** It is bound to loopback and never `0.0.0.0`, which is why.

Boks will never mount the host's Docker or containerd socket into a guest. The full analysis,
including trust boundaries and ranked escape surfaces, is in
[Security model](security-model.md).

## What happens if the agent ignores the proxy?

It is judged anyway. The host's network stack decides every TCP connection the guest opens,
by address and port, before it dials anything.

This is worth stating precisely because it was measured broken first: on 2026-08-12 a denied
host answered normally and a raw TLS handshake reached the real origin. It was measured fixed
against a real VM on 2026-08-13 — the same handshake is now refused before anything is
dialled, and every decision is logged. Both measurements are in
[Verification](verification.md).

One consequence to know: enforcement is on the address in the packet, so **a hostname rule
does not authorise a raw connection**. `--allow example.com` will not permit a direct
connection to example.com's address.

## Why is `ping` broken? Why does QUIC not work?

UDP and ICMP are dropped at the link, with no way to ask for them. DNS is the exception, and
only to the sandbox's own resolver. That costs QUIC and `ping`, and it is why published ports
are TCP only.

The drops are also silent: TCP denials are logged with a reason, and a guest probing UDP or
ICMP leaves nothing in `boks policy log`. That is an observability gap, not a containment
one.

## Why does a sandbox keep everything I install?

Because a sandbox lives until you remove it, and running the same agent in the same directory
re-attaches to the one they already have. `--rm` gives you an ephemeral one instead; `boks rm`
deletes a persistent one and its filesystem.

`start` boots a *new* VM over the same writable snapshot — the `boot_id` changes and the files
do not.

## Why is my sandbox called `shell-boks`?

A sandbox is named `<agent>-<workspace directory>`, and that derived name is what a second run
looks up: naming and re-attach are the same mechanism. Two different directories that would
derive the same name are not merged — the second gets a digest suffix and is told why.
`--name` overrides the derivation.

## Why does `-t` mean two different things?

On `exec` it is `--tty`, because that is what it is in Docker and in Docker Sandboxes. On
`run` it is `--template`, which is Docker Sandboxes' own spelling for the image an agent runs
in. Boks is meant to feel like a drop-in alternative, and a user's muscle memory is part of
that interface, so short forms exist only where the reference has one. Inventing others would
make the muscle memory wrong.

## Can I define my own agent?

Not in a file yet — the registry is Go, and a loader for declarative definitions is on the
[roadmap](roadmap.md). `--template` points any agent at another image today.

## Can a credential apply to only one sandbox?

No. A stored credential applies to every sandbox; `--no-secrets` turns all of them off for a
run, and there is nothing in between.

## How does this compare to Docker Sandboxes feature by feature?

[Docker Sandbox parity](docker-sandbox-parity.md), which is the honest matrix including the
rows where the answer is "no" or "planned". Docker Sandboxes is the behavioural reference:
Boks reproduces observable behaviour from public documentation using open-source components,
and is not derived from Docker's code.

## How do I know any of this is true?

[Verification](verification.md) — what was observed, on what hardware, on what date, and what
each observation does and does not establish. It includes the checks that failed and what was
done about them, which is the part worth reading.

## What is not built yet?

[Roadmap](roadmap.md), which is the list of gaps in the project's own words rather than a plan
with dates.

## What licence?

Apache-2.0.
