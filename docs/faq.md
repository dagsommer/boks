# FAQ

Questions that are not failures. If something is broken, [Troubleshooting](troubleshooting.md)
is the other page.

## What is Boks?

A tool for running coding agents and other untrusted developer tooling inside isolated
microVMs, on your own machine. Point it at a directory and that code gets a virtual machine
of its own — its own kernel, its own network, and none of your filesystem but the directory
you named. No account, no cloud service, no telemetry.

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

Yes, natively. `boks run` boots a sandbox through the **Windows Hypervisor Platform** — a
user-mode hypervisor API, not WSL2 and not Hyper-V's management stack — driven by a
`krun.dll` this project builds from a patch series against libkrun, alongside a patched
containerd, a patched nerdbox shim and a `mkfs.erofs` for Windows.

On 2026-08-15, on real Windows 11 hardware and from an **ordinary unelevated terminal**, a
Linux container ran in a microVM under Boks' own stack: an allowed host returned HTTP 200
through Boks' network stack while a denied one was refused, the guest's clock was correct to
the second against the host, workspace writes went through in both directions, a marker
survived `boks stop`, eight vCPUs came up, and `boks stop` then `boks rm` left nothing
behind.

What that does not cover: every Windows result comes from **one machine**, and there is no
Windows arm64 build at all — the WHP backend and the guest kernel are both x86_64. No release
has been cut, so Windows means building from source today. See [Install](install.md#windows--winget)
and [Windows](windows.md).

Running the Linux build inside WSL2 also works, and since 2026-08-15 it is the *verified*
route on Linux — see the next answer.

## Does it work on Linux?

Yes, and it was verified end to end for the first time on 2026-08-15 — in WSL2 on Ubuntu
26.04, where 25 of 26 checks passed: three distinct guest `boot_id`s, `nproc` following
`--cpus` downward on an eight-core host, and, with every proxy variable cleared, an allowed
address completing TLS against the origin's own certificate while `1.1.1.1:443` was refused
in the same sandbox.

Two caveats before you count it as done. That run was **inside WSL2, not on bare metal** —
nothing on this project has run on a bare-metal Linux host. And **creating a sandbox still
needs more privilege than an ordinary user has**: as a normal user it fails with `mount
source: "overlay", err: operation not permitted`, so expect to run as root for now. See
[Get started](get-started.md#which-platforms-work).

## Do I need Docker Desktop?

No, and no account anywhere either. Boks needs containerd, nerdbox and libkrun, and
`boks doctor` checks for each.

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

On `exec` it is `--tty`, because that is what `-t` means in Docker and everywhere else you
would type it. On `run` it is `--template`, the image an agent runs in. Muscle memory is part
of an interface, so short forms exist only where an established tool already has one;
inventing extra ones would make that memory wrong.

## Can I define my own agent?

Not in a file yet — the registry is Go, and a loader for declarative definitions is on the
[roadmap](roadmap.md). `--template` points any agent at another image today.

## Can a credential apply to only one sandbox?

No. A stored credential applies to every sandbox; `--no-secrets` turns all of them off for a
run, and there is nothing in between.

## How does this compare to Docker Sandboxes feature by feature?

[Docker Sandbox parity](docker-sandbox-parity.md), which is the honest matrix including the
rows where the answer is "no" or "planned". Boks is an independent implementation built from
open-source components, working from public documentation; it is not derived from Docker's
code.

## How do I know any of this is true?

[Verification](verification.md) — what was observed, on what hardware, on what date, and what
each observation does and does not establish. It includes the checks that failed and what was
done about them, which is the part worth reading.

## What is not built yet?

[Roadmap](roadmap.md), which is the list of gaps in the project's own words rather than a plan
with dates.

## What licence?

Apache-2.0.
