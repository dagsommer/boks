# Release notes

What has shipped, newest first.

**Nothing has been released yet.** There are no tags and no published binaries. Everything
below describes the state of `main`, and the section headed *Before 0.1.0* is the
hand-written entry that stands in for a history from before the first tag.

<!--
  Contributors: how an entry gets here.

  From 0.1.1 onward each entry is generated from the conventional commits since the previous
  tag:

      make release-notes TAG=v0.1.1                  # print it, and read it
      make release-notes TAG=v0.1.1 INSERT=--insert  # write it into this file

  feat:, fix:, docs: and test: are bucketed; everything else keeps its subject verbatim under
  "Other changes", so no commit is ever dropped for failing to match a regular expression.
  Merge commits are skipped, because a merge's subject describes a branch whose commits are
  already listed. The generated text is a first draft, and editing the prose is expected —
  what must not change is a claim.

  "Before 0.1.0" is written by hand, once, and that is deliberate. The 157 commits leading up
  to it are mostly prefixed, but with this project's own vocabulary — cli:, policy:,
  enforce:, ports: beside feat: and fix: — and the merges are prose. Run over that range the
  generator produces a list that is complete and unreadable, so it is not run over that
  range.

  Cutting a release, in full, is documented at the top of scripts/release-notes.sh. The
  short version: move ImageTag in internal/agent/agent.go, which is the one place the
  version is spelled and which .github/workflows/images.yml refuses to publish against a
  mismatched tag; make check && make docs; generate the notes; read them; commit; tag.
-->

<!-- release-notes: generated entries are inserted directly below this line -->

## Before 0.1.0

The state `main` reached before the first tag. Written by hand, and phrased the way the
things in it were measured.

### The sandbox

A sandbox is a microVM, and that was established rather than assumed: on a real run the guest
reported its own Linux kernel while the host ran Darwin, with its own `boot_id`, an uptime of
hundredths of a second against the host's weeks, and vCPU and memory counts matching the
flags rather than the host's hardware. The procedure and the full evidence are in
[Verification](verification.md).

The lifecycle is complete — `run`, `create`, `ls`, `inspect`, `start`, `stop`, `exec`, `rm`,
`cp` — and sandboxes persist until removed. Files written inside one survive `stop`/`start`,
confirmed behind a real microVM where `start` boots a *new* VM over the same writable
snapshot. Cleanup leaves no containers, tasks, shim processes or mounts behind, including
after Ctrl-C.

Workspaces are shared at their exact host path. Writes reach the host, `:ro` is honoured,
directories above the workspace are not exposed, and a symlink pointing at `/etc` or
`~/.ssh` resolves inside the guest rather than on the host.

### The network

Each sandbox gets a virtual NIC whose far end is a userspace network stack in a Boks process,
with a filtering proxy inside that virtual network. The stack judges every TCP connection the
guest opens, by address and port, before dialling; UDP and ICMP are dropped; DNS is mediated
by the sandbox's own resolver.

This is the part of the project with the most interesting history, and it is worth reading in
the order it happened. It was **measured broken on 2026-08-12**: a denied host answered
normally and a raw TLS handshake reached the real origin, because a guest that ignored the
proxy was unfiltered. It was **measured fixed against a real guest on 2026-08-13**: with every
proxy variable unset, a denied address is refused before anything is dialled while a permitted
one connects end to end.

`--net none` gives a sandbox no network at all, which is the strongest containment Boks
offers and the only one whose enforcement does not depend on code that has met a real guest
recently.

Policy is durable state rather than an argument to a run. Rules survive the command that
wrote them, apply globally or to one sandbox, and resolve under one rule: a deny in any scope
beats an allow in any scope.

### Ports

`boks run -p 3000` publishes a sandbox port on the host, and `boks ports` changes a sandbox
that is already running — the case a dev server actually needs. Bound to loopback, never
`0.0.0.0`. TCP only. The datapath is exercised end to end against a *simulated* guest; a real
VM reaching it through libkrun's virtio-net device has not been tried.

### Credentials

The real value never enters the sandbox: the guest holds a placeholder shaped like a real key
and the host proxy swaps it for the credential on requests to the named hosts and no others.
Those hosts, and only those, have their TLS terminated, which every run says out loud the
first time a sandbox meets each one.

Eleven services are known by name, of which nine carry a rule; the two that do not say so,
because a guessed header fails in a way you cannot diagnose. Each agent brings the
destinations its CLI cannot work without as an ordinary allow layer, so `boks run claude`
works under the deny-by-default preset without anyone reading a hostname out of a log.
Telemetry endpoints are not in any of them.

### Agents and images

Ten agents; nine resolve to a published multi-arch image built from a shared Debian base plus
one thin layer each. `kiro` is registered without an image and needs an explicit
`--template`. **Only the base image has run inside a microVM** — the agent layers were
exercised with `docker run`, which establishes that each CLI is installed and starts, and
nothing about isolation.

### Platforms

macOS on Apple silicon is the platform everything above was measured on. Linux with KVM is
built and designed for and has not been exercised end to end. Native Windows support is in
progress and no sandbox has yet booted there: a Windows Hypervisor Platform backend for
libkrun is being built in this repository's patch series, and every libkrun crate — including
`virtio-net`, the device Boks' enforcement depends on — now compiles for Windows, with
`krun.dll` linking on a real Windows runner in CI. That is the artifact existing, not the
artifact working. Running inside WSL2 should work unchanged and nobody has tried it.

### Known at the time of writing

The gaps are on the [Roadmap](roadmap.md) rather than repeated here. The two most likely to
reach you: a crashed network supervisor leaves a running sandbox with no network until it is
restarted, and a hostname-only rule does not authorise a raw connection to the address that
hostname resolves to.
