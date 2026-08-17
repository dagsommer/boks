# Usage

Everything a sandbox does day to day: creating one, keeping it, sharing directories into it,
deciding what it can reach, giving it a credential without giving it the secret, and reaching
a server inside it.

This page is task-shaped. For the exhaustive list of flags every command takes, see the
[CLI reference](cli.md), which is generated from the command tree.

## Running an agent

```bash
boks run [agent] [workspace...] [-- agent arguments...]
```

The agent comes first and decides what the sandbox contains; the workspaces follow and
default to the current directory. Anything after `--` goes to the agent — for `shell`, that
is the command to run.

```bash
boks run                                     # a shell in the current directory
boks run shell . -- uname -a
boks run claude ~/src/foo                    # Claude Code, in that directory
boks run --rm shell ~/src/foo -- go test ./...
```

A pseudo-terminal is allocated when stdin and stdout are both terminals, and never when
either is a pipe, so there is no flag for it. `boks run -it` is not a thing, and says so
rather than misparsing.

## Sandboxes persist, and the name is the identity

A sandbox lives until you remove it. Running the same agent in the same directory
re-attaches to the one they already have, so installed packages, caches and shell state are
still there.

```bash
boks run shell               # create, or re-attach to this directory's shell sandbox
boks ls                      # SANDBOX  AGENT  STATUS  PORTS  WORKSPACE
boks exec -it <name> sh      # a second terminal in the same sandbox
boks stop <name>             # keeps everything inside
boks start <name>            # boots a new VM over the same filesystem
boks cp ./file.txt <name>:/root/file.txt
boks rm <name>               # deletes the sandbox and its filesystem
```

`boks rm` is one sandbox. The images those sandboxes were built from stay in containerd's
store, and that is where the disk goes: one agent image is about a gigabyte, written twice —
compressed, then unpacked. `boks purge` is the command for all of it, and `boks purge
--dry-run` shows what is there and what each part is before anything is removed. `boks doctor`
names the same directory and its size. See
[Uninstalling](install.md#uninstalling--two-steps-not-one).

A sandbox is called `<agent>-<workspace directory>` — `shell-boks` for the `shell` agent in
`~/git_repos/boks`. That derived name is what a second run looks up, so **naming and
re-attach are the same mechanism**. Two different directories that would derive the same name
are not merged: the second gets `<agent>-<dir>-<digest>` and is told why.

`--name` overrides the derivation, which is how one workspace gets several sandboxes and how
a sandbox is reached from anywhere:

```bash
boks run --name shell-boks   # re-attach by name, from any directory
```

Some things are fixed when a sandbox is created, because they live in the container's OCI spec
and in the annotations the runtime reads when it builds the VM: the agent, `--net`,
`--template`, `--cpus`, `--memory`, `--env` and `--annotation`. Passing one of them with a
value the sandbox was not created with is refused and nothing runs, rather than being quietly
dropped — a `--cpus 2` that was ignored would hand you the eight-vCPU sandbox you already had,
and a `--net none` that was ignored would hand you a network while appearing to have been
obeyed. One run names all of them at once, with the value the sandbox actually has. Remove the
sandbox and run again, or give a second one a `--name` of its own.

Asking for what the sandbox already has is not a disagreement: re-running the same command
line re-attaches in silence.

Two things are reported rather than refused, because ignoring them leaves the guest with
*less* than was asked for rather than more: `--publish` (the sandbox keeps the ports it has;
`boks ports` changes a running one) and extra workspace arguments (a directory that is not
shared is one the guest cannot reach). `--clone` on a sandbox that already exists is a
warning, not a refusal, and says which mode you are actually getting.

The policy flags are the opposite case and are *applied* on a re-attach: `--policy`,
`--profile`, `--allow`, `--deny` and `--inject` resolve into the network stack this run starts
rather than into the container, and they can only narrow what the sandbox reaches.

## Workspaces

Each workspace is shared into the guest **at the same absolute path it has on the host**, and
the first one is the process's working directory. Extra directories are extra arguments —
there is no `--mount` — and each may carry a `:ro` suffix for a read-only share:

```bash
boks run shell ~/src/foo ~/src/lib:ro
```

Three properties, all verified against a real guest:

- Writes reach the host. This is the point, and it is also the sharpest edge in the
  [security model](security-model.md): a sandbox can modify `Makefile`, `package.json`
  scripts, Git hooks or CI config, which then run on *your* machine.
- Directories above a workspace are not exposed.
- A symlink inside the workspace pointing at `/etc` or `~/.ssh` resolves inside the guest,
  not on the host.

Git inside the guest sees a directory owned by the host user's uid while running as root, and
would normally refuse it as "dubious ownership". Boks configures `safe.directory` for each
workspace through Git's command scope, so this does not happen; see
[Troubleshooting](troubleshooting.md#git-refuses-the-workspace-dubious-ownership) for what
that means if you set `GIT_CONFIG_COUNT` yourself.

## Resources

```bash
boks run --cpus 4 -m 8g shell .
```

`--cpus 0` means all host CPUs. `--memory` takes binary units and defaults to half the host's
memory, capped at 32g.

Both size the VM when it is built, so both are fixed when the sandbox is created: passing a
different value to a sandbox that already exists is refused rather than ignored. `boks rm` and
run again to resize one.

## The network a sandbox gets

A sandbox gets a virtual NIC whose far end is a userspace network stack in a Boks process,
and the guest is pointed at a filtering proxy inside that virtual network. **The stack judges
every TCP connection the guest opens, by address and port, before it dials anything** — so a
guest that ignores the proxy is judged rather than unfiltered. UDP and ICMP are dropped. DNS
is mediated by the sandbox's own resolver.

```bash
boks run --net none shell .                  # no network at all: the strongest containment
boks run --policy locked --allow api.example.com:443 shell .

boks net ls                                  # the stacks currently serving sandboxes
boks policy log                              # what was allowed or denied, and why
boks proxy --policy locked -v                # the same proxy, standalone, for anything
```

`--net none` gives the VM a NIC — which is what turns the runtime's own transport off, and
with it the guest's access to the host's loopback — and never wires the container to it.

The network stack lives in a small per-sandbox process so that it lasts as long as the
sandbox's VM rather than as long as your command: a build running in a sandbox does not lose
the network when you press Ctrl-C. `boks stop` and `boks rm` take it with them.

## Policy is state, not an argument

Rules written with `boks policy` survive the command that wrote them, and are what
`boks run`, `boks start` and `boks exec` all serve a sandbox. A rule applies to every
sandbox, or to one.

```bash
boks policy init --preset locked                    # choose the base posture
boks policy allow github.com:443 --note "git"       # every sandbox
boks policy allow --sandbox claude-myproject api.example.com:443
boks policy deny  metadata.example.com              # deny always wins, in every scope

boks policy check --sandbox claude-myproject api.example.com:443
boks policy ls --sandbox claude-myproject           # stored rules, and what they resolve to
boks policy log --sandbox claude-myproject --since 30m
```

**Precedence, in one sentence:** a deny in any scope beats an allow in any scope, and only
the base preset — chosen by `policy init`, a profile, or a `--policy` flag — decides what
happens to a destination no rule mentions. A sandbox-scoped rule can add access the machine's
policy already tolerates and can take access away; it can never widen past a deny someone
wrote down.

### Profiles, and one-run overrides

```bash
boks policy profile create ci --preset locked --allow proxy.golang.org:443
boks run --profile ci shell .
```

`--policy`, `--allow` and `--deny` override the stored policy for a single run. `--policy`
and `--allow` replace the posture and the allow list; `--deny` is *added* to what the sandbox
already denies, because a prohibition must not disappear because this invocation typed a
different one.

### What an agent adds

`boks run claude` needs `api.anthropic.com`, and that is a fact about the agent, like its
image. Each agent carries the destinations its CLI cannot work without as an allow layer of
its own, visible in `boks policy ls --agent claude`. A deny in any scope still beats it, and
`--policy locked` drops the layer entirely, because "deny everything" has to keep meaning
that. See [Agents](agents.md).

### One thing the CLI does not warn you about

A hostname-only rule denies raw connections, **including to the allowed host**. A raw socket
carries no name, so it is judged on the address: `--allow example.com` does not permit a
direct connection to example.com's address. That fails closed, which is the safe direction,
but the CLI does not say so at the point you write the rule.

## Credentials: the name is the configuration

```bash
echo -n "$ANTHROPIC_API_KEY" | boks secret set anthropic
boks policy allow api.anthropic.com:443
boks run claude .                            # attaches it; no --inject anywhere
```

Boks knows eleven services by name, and for each it already has the hosts the credential is
sent to, the header that carries it, the environment variable the guest's client reads it
from, and the shape a convincing placeholder has. Storing a key under one of those names is
the whole configuration.

```bash
boks secret services                         # the services, and what boks knows about each
boks secret import                           # offer the keys already in this shell, Y/n each
boks secret adopt claude-code                # take over a subscription login you already have
boks secret ls                               # names, kinds and destinations — never values
boks run --no-secrets shell .                # a sandbox that carries none of them
```

**The real value never enters the sandbox.** The guest gets a placeholder shaped like a real
key — the vendor's own prefix, so the client's own format check passes — and the host proxy
swaps it for the credential on requests to those hosts and no others.

Two things are worth knowing before you type the first line:

- **Those hosts are the ones whose TLS Boks terminates.** Every run says so out loud the
  first time a sandbox meets each one, including under `--quiet`. Everything else is tunnelled
  with the origin's own certificate chain untouched.
- **A credential rule is not an allow rule.** It says where a value may go, not what is
  reachable, so the host still needs `boks policy allow api.anthropic.com:443`. The two are
  separate on purpose; without the allow, the run fails at the network layer with no hint that
  a credential was involved. `boks secret set` prints the line you need.

Anything Boks does not know a service for is stored under a name of your own and attached by
a rule you write, which is also how you override a built-in one:

```bash
echo -n "$TOKEN" | boks secret set my-internal-api
boks run --inject 'my-internal-api@api.internal.example.com=Authorization:Bearer %s' \
         --guest-credential 'my-internal-api=MY_API_KEY=placeholder' shell .
```

The store is one encrypted file, and the passphrase comes from `BOKS_SECRETS_PASSPHRASE`.
There is no recovery for a forgotten one — that is what encryption means. `boks secret reset`
is the way out, and [Troubleshooting](troubleshooting.md#i-have-forgotten-the-secret-store-passphrase)
says what it costs.

## Publishing a port

```bash
boks run shell -p 3000                       # sandbox port 3000 on an ephemeral host port
boks ports <name>                            # what it publishes
boks ports <name> --publish 8080:8080/tcp
boks ports <name> --unpublish 8080:8080/tcp
boks ports <name> --json
```

`boks ports` changes a sandbox that is already running, which is the case a dev server needs:
you start the server after the sandbox is up. `boks ls` shows the mappings.

**A published port is bound to loopback, never `0.0.0.0`.** It is a hole from your machine
into a VM running code you have not audited; on all interfaces it would be a hole from the
local network into it.

Two things to expect:

- The service inside the sandbox has to listen on the VM's external interface — bind
  `0.0.0.0` or `::`, not only `127.0.0.1`. `boks ports` says so when nothing answers.
- **TCP only.** The grammar accepts `udp`, `udp4` and `udp6`, because that is the port syntax
  people already type, and refuses them with the reason: the sandbox's network stack drops
  UDP at the link, so a datagram has no way back.

Publishing has never been driven by a real guest. The datapath is exercised end to end
against a simulated one, which proves the host side works; a real VM reaching it through
libkrun's virtio-net device has not been tried.

## Flags worth knowing

Spelled the way the equivalent flags are spelled elsewhere, because muscle memory is part of
an interface. The complete list is in the [CLI reference](cli.md).

| Flag | Meaning |
|---|---|
| `-t`, `--template` | guest root filesystem (default: the agent's image) |
| `--name` | name the sandbox instead of deriving it from agent and workspace |
| `-d`, `--detached` | print the sandbox name and exit instead of attaching |
| `--rm` | destroy the sandbox when the command exits |
| `--cpus` | guest vCPUs (0: all host CPUs) |
| `-m`, `--memory` | guest memory, binary units (`1024m`, `8g`; default half the host's, max 32g) |
| `--env KEY=VALUE` | set an environment variable (repeatable) |
| `--net none\|nat` | no network at all, or a policed one (default `nat`) |
| `--profile NAME` | apply a stored policy profile |
| `--policy`, `--allow`, `--deny` | override the stored policy for this run |
| `-p`, `--publish` | publish a sandbox port on the host, bound to loopback |
| `--inject`, `--guest-credential` | attach a host-held credential to named hosts |
| `--no-secrets` | do not attach the credentials in the store to this sandbox |

`boks completion bash|zsh|fish|powershell` prints a completion script, and
`boks <command> --help` lists what that command takes.
