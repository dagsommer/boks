# Walkthrough

One sitting with Boks, from an empty terminal to a sandbox with a policy, a credential and a
published port — and back to a clean machine. [Get started](get-started.md) gets you to a
passing `boks doctor`; this page assumes you are there, on a platform from
[the table](get-started.md#which-platforms-work), and writes `boks` for whatever your install
put on your `PATH` (`./bin/boks` if you built from source).

Every behaviour on this page is documented in [Usage](usage.md); this is the same material in
the order a first session meets it.

## 1. A shell in a microVM

```bash
boks run
```

That is a shell, in the current directory, inside a microVM. The prompt you get is running
under a different kernel than your host, with the current directory — and nothing above it —
shared in at its exact host path.

Exit the shell. The sandbox stays (more on that below).

## 2. Convince yourself it is a VM

A container would be easier to build, so it is fair to ask. Ask the guest:

```bash
boks run shell . -- uname -a
boks run shell . -- cat /proc/uptime
```

The guest reports its own Linux kernel, and an uptime of seconds against your host's days —
because it *is* a different kernel, booted for this sandbox. A shared-kernel container cannot
produce that. The full procedure, with the evidence from a real run, is in
[Verification](verification.md).

## 3. Let it keep state

A sandbox lives until you remove it. Run the same agent in the same directory and you are
back in the same sandbox — installed packages, caches and shell state are still there:

```bash
boks run                     # same directory: re-attaches, does not create
boks ls                      # SANDBOX  AGENT  STATUS  PORTS  WORKSPACE
boks stop shell-myproject    # keeps everything inside
boks start shell-myproject   # boots a new VM over the same filesystem
```

The name is the identity: a sandbox is called `<agent>-<workspace directory>`, and that
derived name is what a second run looks up. `--name` overrides the derivation; `--rm` gives
you an ephemeral sandbox instead.

## 4. Decide what it can reach

By default a sandbox gets a policed network: a userspace stack in a Boks process judges every
TCP connection the guest opens, by address and port, before it dials anything. You choose the
posture once, and it is durable state rather than an argument you retype:

```bash
boks policy init --preset locked                # deny by default
boks policy allow github.com:443 --note "git"   # every sandbox
boks policy log                                 # what was allowed or denied, and why
```

The strongest containment needs no rules at all:

```bash
boks run --net none shell .                     # no network. At all.
```

Two things worth knowing at the point you write the first rule: a deny in any scope beats an
allow in any scope, and a hostname rule does not authorise a *raw* connection to the address
it resolves to — a raw socket carries no name, so it is judged on the address and fails
closed. [Usage](usage.md#policy-is-state-not-an-argument) has the full precedence story.

## 5. Give it a credential without giving it the secret

```bash
echo -n "$ANTHROPIC_API_KEY" | boks secret set anthropic
boks policy allow api.anthropic.com:443
```

The real key never enters the sandbox. The guest holds a placeholder shaped like a real key,
and the host proxy swaps it for the credential on requests to that vendor's hosts and no
others. The cost, stated plainly: those hosts — and only those — have their TLS terminated by
Boks, and every run says so the first time a sandbox meets each one.

Note the second line: a credential rule says where a value may go, not what is reachable.
Without the `allow`, the run fails at the network layer.

## 6. Run an agent

```bash
boks run claude ~/src/myproject
```

The agent comes first and decides what the sandbox contains — `claude` resolves to a prepared
image with Claude Code installed, and brings `api.anthropic.com:443` as an allow layer of its
own, so this works under the deny-by-default preset without reading a hostname out of a log.
[Agents](agents.md) lists all ten, what each may reach by default, and why.

One honest caveat from the [roadmap](roadmap.md): the shared base image has run inside a
microVM; the per-agent layers on top of it have so far been exercised with `docker run`,
which proves each CLI is installed and starts, and nothing about isolation.

## 7. Reach a server inside it

Start a dev server in the sandbox, then publish its port — in that order, because
`boks ports` changes a sandbox that is already running:

```bash
boks run shell -p 3000       # or: boks ports <name> --publish 3000:3000/tcp
boks ls                      # shows the mapping
```

The host side is bound to loopback, never `0.0.0.0` — a published port is a hole from your
machine into a VM running code you have not audited, and it stays your machine's alone. Inside
the sandbox, the server must listen on `0.0.0.0`, not only `127.0.0.1`; `boks ports` says so
when nothing answers. TCP only, and the datapath has so far been exercised against a
simulated guest — the caveats are in [Usage](usage.md#publishing-a-port).

## 8. Leave the machine clean

```bash
boks ls
boks rm shell-myproject      # deletes the sandbox and its filesystem
```

`rm` takes the sandbox's VM, its writable snapshot and its network stack with it. Cleanup
leaves no containers, tasks, shim processes or mounts behind, including after Ctrl-C.

## Where to next

- **[Usage](usage.md)** — everything above, exhaustively, plus workspaces, resources and
  profiles.
- **[Security model](security-model.md)** — what the boundary is, and the two things about it
  most likely to surprise you.
- **[Troubleshooting](troubleshooting.md)** — when a step above did not do what this page
  said.
