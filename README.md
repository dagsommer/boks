# Boks

Run coding agents and other untrusted developer tooling inside isolated microVMs, on your
own machine.

Boks is a local-first, open-source alternative to Docker Sandboxes (`docker sbx`). No
account, no cloud service, no telemetry.

> [!WARNING]
> **Boks is experimental.** The VM boundary and the network policy have both been measured
> against a real guest (see [Status](#status)), and both hold — but this is a young project
> with one verified platform, one verified hypervisor, and plenty still unbuilt. Judge it on
> what the verification document says was observed, not on what the feature list implies.

## Status

The VM boundary is **verified**. On 2026-08-11, on an Apple M5 Pro running macOS 26.5.2,
`boks run` booted a real microVM: the guest reported **Linux 6.12.44 aarch64** while the
host ran **Darwin 25.5.0**, with its own `boot_id`, an uptime of 0.03 s against the host's
28 days, and vCPU and memory counts matching `--cpus`/`--memory` rather than the host's 18
cores and 48 GiB. A shared-kernel container cannot produce any of that. Full evidence and
procedure: [docs/verification.md](docs/verification.md).

What works, tested locally:

- `boks doctor` — inspects containerd, the VM runtime, virtualisation, snapshotter and its
  host tooling, and explains how to fix what is missing.
- `boks run [agent] [workspace...]` — agent-first, like `sbx run`. Creates a sandbox
  through containerd or re-attaches to the one that agent and directory already have, runs
  the agent (or a command after `--`), streams its output, and returns its exit code.
  `--rm` makes it ephemeral instead.
- **Agents with prepared images** — `claude`, `codex`, `copilot`, `cursor`, `docker-agent`,
  `droid`, `gemini`, `opencode` and `shell` each resolve to an image at
  `ghcr.io/dagsommer/boks/`, built from [`images/`](images/): a shared Debian base with the
  toolchain preinstalled, plus one thin layer per agent. `kiro` is registered without an
  image and needs an explicit `--template`. Published to GHCR and public. The base image has
  since run inside a microVM; the agent CLIs on top of it have not. See
  [images/README.md](images/README.md), including why nothing in them is installed by a
  package manager.
- **The sandbox lifecycle** — `create`, `ls`, `inspect`, `start`, `stop`, `exec`, `rm`, `cp`.
  Sandboxes persist until removed, and files written inside one survive stop/start —
  confirmed behind a real microVM, where `start` boots a *new* VM (the `boot_id` changes)
  over the same writable snapshot.
- **Exact-path workspace sharing** — the workspace appears inside the guest at its absolute
  host path, writes reach the host, `:ro` is honoured, and directories above the workspace
  are not exposed.
- Cleanup leaves no containers, tasks, shim processes or mounts behind, including after
  Ctrl-C.

- **Symlinks do not escape.** A symlink inside the workspace pointing at `/etc` or `~/.ssh`
  resolves inside the guest, not on the host.

- **Network policy is applied to a sandbox.** `boks run` gives the sandbox a virtual NIC
  whose far end is a userspace network stack in a Boks process, and points the guest at a
  filtering proxy *inside* that virtual network. Verified against a real guest: an allowed
  host is fetched, a denied one is refused with a reason, DNS is mediated, and the host's
  own loopback services — which the old TSI transport exposed — are now unreachable.
  The stack itself judges every TCP connection the guest opens, by address and port, before
  it dials anything, so a guest that ignores the proxy is judged rather than unfiltered; UDP
  and ICMP are dropped. **Verified against a real guest on 2026-08-13**: with every proxy
  variable unset, a denied address is refused before anything is dialled, while an address
  the policy permits connects end to end — so the stack judges each flow rather than simply
  dropping what does not use the proxy. `--net none` gives a sandbox no network at all. The
  stack lives in a small
  per-sandbox process so that it lasts as long as the sandbox's VM rather than as long as
  your command — a build running in a sandbox does not lose the network when you press
  Ctrl-C. `boks net ls` shows them, `boks stop` takes them down.
- **Port publishing.** `boks run -p 3000` publishes a sandbox port on the host, and
  `boks ports <name> --publish/--unpublish/--json` changes a sandbox that is already running,
  which is the case a dev server needs: you start the server after the sandbox is up.
  **Bound to loopback, never `0.0.0.0`**: a published port is a hole from your machine into a
  VM running code you have not audited, and on all interfaces it would be a hole from the
  local network into it. Omit the host port to get an ephemeral one. The service inside the
  sandbox has to listen on the VM's external interface (`0.0.0.0`, not only `127.0.0.1`), and
  `boks ports` says so when nothing answers. TCP only — see [what is not done](#status).
  `boks ls` shows the mappings. It is also what an OAuth login with a `127.0.0.1` redirect
  would need, since the browser is on the host and the listener is in the guest — though the
  one flow Boks has actually measured, Claude Code's, turned out to be paste-a-code and needs
  no port at all.
- **Credential injection in a sandbox**, over HTTP and HTTPS. The guest holds a placeholder
  in the environment variable its tooling reads; the real secret stays on the host and is
  attached to requests for the hosts you named. Those hosts — and only those — have their
  TLS terminated, which `boks run` says out loud the first time a sandbox meets each one.
  A forgotten passphrase is not a dead end: `boks secret reset` deletes the store without
  needing to decrypt it, and the failure that sends you there says what that costs.
- **The name is the whole configuration**, for the services Boks knows: `boks secret set
  anthropic` and nothing else. Boks already has each vendor's hosts, header, guest variable
  and key shape, so no `--inject` is needed and nobody has to know Anthropic's header name to
  put a key in a sandbox. Only vendor-documented rules are in the list; two of the eleven
  names carry none, and say so, because a guessed header fails in a way you cannot diagnose.
- **Each agent brings the destinations it cannot work without**, so `boks run claude` works
  under the default deny-by-default preset without anyone reading a hostname out of a log.
  They are an ordinary allow layer — a deny in any scope still beats them — and only
  vendor-documented hosts are in them. Telemetry endpoints are not.

What is **not** done:

- **A hostname-only policy denies raw connections, including to the allowed host.** A raw
  socket carries no name, so it is judged on the address; `--allow example.com` therefore does
  not permit a direct connection to example.com's address. That fails closed, which is the
  safe direction, but "allowed through the proxy" and "allowed on a raw socket" are different
  questions and the CLI does not say so at the point you write the rule.
- **UDP and ICMP drops are silent.** TCP denials are logged with a reason; a guest probing
  UDP or ICMP leaves nothing in `boks policy log`. An observability gap, not a containment
  one.
- **No policy over names, and no UDP.** DNS is mediated by the sandbox's own resolver and
  cannot be sent anywhere else, but the names themselves are not filtered. UDP and ICMP are
  dropped with no way to ask for them, which costs QUIC and `ping`.
- **No nested Docker** and no kits.
- **Published ports are TCP only.** The grammar accepts `udp`, `udp4` and `udp6` because
  sbx's does, and refuses them with the reason: the sandbox's network stack drops UDP at the
  link, so a datagram has no way back. Publishing UDP would mean widening that filter, which
  is a change to the stack's closed posture rather than a port flag.
- **Publishing has never been driven by a real guest.** The datapath is exercised end to end
  against a simulated one — a second gvisor stack on the far end of the real link socket —
  which proves the host side works. A real VM reaching it through libkrun's virtio-net device
  has not been tried.
- **No host-side OAuth login.** `boks secret set NAME --oauth` is recognised and refused,
  because every flow that could acquire a token starts by identifying the program to the
  vendor with a client id issued to a registered application, and Boks holds none. `boks
  secret adopt` takes over a login you have already performed, which covers the same case on
  a machine you have used the agent on and nothing at all on a fresh one.
- **Two of the eleven known services have no rule.** Neither Cursor nor Factory documents the
  host their CLI sends its API key to, so `boks secret set cursor` and `boks secret set droid`
  refuse and explain rather than guessing.
- **A credential cannot be scoped to one sandbox.** A stored credential applies to every
  sandbox; `--no-secrets` turns all of them off for a run, and there is nothing in between.
- **Only the base image has run in a microVM.** The agent layers were exercised with
  `docker run`, which proves each CLI is installed and starts — and nothing about isolation.
  `kiro` has no image (see [images/README.md](images/README.md) for why), and there is still
  no way to define an agent in a file rather than in code.
- **No terminal dashboard** for bare `boks`, and no `--kit`. See the CLI surface section of
  the parity matrix. `--clone` exists, but has only been exercised against runc, so nothing
  about it has been measured behind a hypervisor.
- **A crashed network supervisor is unrecoverable without a restart.** The running VM does
  not re-attach to a fresh stack on the same socket — measured on 2026-08-12 — so the
  sandbox keeps running with no network at all. Boks now says exactly that when it meets
  one, and gives the `stop && start` that fixes it; it does not restart the sandbox itself,
  because that kills whatever is running inside.
- **Linux is untested in practice.** The boundary was verified on macOS/Apple silicon;
  the Linux/KVM path is designed for but has not been exercised end to end.

See [docs/verification.md](docs/verification.md) for exactly what has been observed and the
procedure to confirm the VM boundary on capable hardware.

## Requirements

- Hardware virtualisation: Linux with `/dev/kvm` (and membership of the `kvm` group), or
  macOS on Apple silicon (Hypervisor.framework). macOS additionally needs the nerdbox shim
  codesigned with the `com.apple.security.hypervisor` entitlement and a user-writable
  `/var/run/containerd` — see [docs/verification.md](docs/verification.md#macos-setup-notes).
- [containerd](https://containerd.io/) 2.2 or later, running
- [nerdbox](https://github.com/containerd/nerdbox) — the VM runtime shim
  (`containerd-shim-nerdbox-v1`) on containerd's `PATH`
- [libkrun](https://github.com/containers/libkrun) 1.18 or later
- `erofs-utils` (for `mkfs.erofs`)
- Go 1.26+ to build

Docker Desktop is not required. Docker Sandboxes is not required.

Run `boks doctor` — it checks all of the above and tells you what to do about each gap.
Nothing should be `fail`. On macOS one check warns on a perfectly good host and always will:
`virtualization` cannot be probed without booting a VM, so it reports architecture support
and says as much. On Linux that check reads `/dev/kvm` and is `ok` or `fail`.

## Installing

[docs/install.md](docs/install.md) covers every route, per platform, with the prerequisites
each one leaves you holding. The short version:

- **macOS on Apple silicon** — a Homebrew tap installs the CLI, containerd, libkrun,
  erofs-utils and a nerdbox shim signed with the entitlement libkrun needs. Two files it
  cannot install, nerdbox's guest kernel and root filesystem, are built by
  `scripts/build-nerdbox-guest.sh`. `boks doctor` reports those two as `guest image` and
  fails when they are missing, so a green `doctor` is worth more than it used to be.
- **Linux** — a tarball, a `.deb` or an `.rpm`, each containing the CLI alone. No
  distribution packages a containerd new enough or nerdbox at all, so the rest is yours to
  assemble. Untested end to end.
- **Windows** — no binary, deliberately: `boks run` stops at its own network refusal there,
  and a binary whose only output is a refusal would make the platform look done. The stack
  under it does run a container, through `ctr` rather than `boks`
  ([docs/windows-e2e.md](docs/windows-e2e.md)). For actual use, run the Linux build inside
  WSL2; see [docs/windows.md](docs/windows.md).

## Quick start

Or build it from source, which is what the rest of this document assumes:

```bash
make build

./bin/boks doctor

./bin/boks run                              # a shell in the current directory
./bin/boks run shell . -- uname -a
./bin/boks run shell . -- sh -lc 'pwd && ls'
./bin/boks run --rm shell /home/alice/src/foo -- go test ./...
```

The agent comes first and decides what the sandbox contains; the workspaces follow and
default to the current directory. Each workspace is shared into the guest at the same
absolute path it has on the host, and the first is the process's working directory. Nothing
above them is exposed. Anything after `--` goes to the agent — for `shell`, that is the
command to run.

### Sandboxes persist

A sandbox lives until you remove it. Running the same agent in the same directory
re-attaches to it, so installed packages, caches and shell state are still there:

```bash
./bin/boks run shell             # create, or re-attach to this directory's shell sandbox
./bin/boks run shell -p 3000     # ...with sandbox port 3000 on an ephemeral host port
./bin/boks ls                    # SANDBOX  AGENT  STATUS  PORTS  WORKSPACE
./bin/boks ports <name>          # what it publishes; --publish/--unpublish to change it
./bin/boks exec -it $(./bin/boks ls -q) sh
./bin/boks stop <name>           # keeps everything inside
./bin/boks cp ./file.txt <name>:/root/file.txt
./bin/boks rm <name>             # deletes the sandbox and its filesystem
```

**The name is the identity.** A sandbox is called `<agent>-<workspace directory>` —
`shell-boks` for the `shell` agent in `~/git_repos/boks` — and that derived name is what a
second run looks up, so naming and re-attach are the same mechanism. Two different
directories with the same name are not merged: the second one gets
`<agent>-<dir>-<digest>` and is told why. `--name` overrides the derivation, which is how one
workspace gets several sandboxes, and how a sandbox is reached from anywhere:

```bash
./bin/boks run --name shell-boks  # re-attach by name, from any directory
```

Useful flags, named after sbx's:

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
| `--profile NAME` | apply a stored policy profile (`boks policy profile ls`) |
| `--policy`, `--allow`, `--deny` | override the stored policy for this run |
| `--inject`, `--guest-credential` | attach a host-held credential to named hosts |
| `--no-secrets` | do not attach the credentials in the store to this sandbox |

A pseudo-terminal is allocated when stdin and stdout are both terminals, and never when
either is a pipe, so there is no flag for it. Extra directories are extra `PATH` arguments,
each of which may carry a `:ro` suffix for a read-only share — there is no `--mount`, because
one way to say a thing is enough:

```bash
./bin/boks run shell ~/src/foo ~/src/lib:ro
```

Flags follow the usual conventions: `--long`, `-s` for the short forms sbx has, and
`boks completion bash|zsh|fish|powershell` prints a completion script. `boks <command>
--help` lists what that command takes.

### The network a sandbox gets

```bash
./bin/boks run --net none shell .                    # no network at all: the strongest containment
./bin/boks run --policy locked --allow api.example.com:443 shell .

./bin/boks net ls                            # the stacks currently serving sandboxes
./bin/boks policy log                        # what was allowed or denied, and why
./bin/boks proxy --policy locked -v          # the same proxy, standalone, for anything
```

### Credentials: the name is the configuration

```bash
echo -n "$ANTHROPIC_API_KEY" | ./bin/boks secret set anthropic
./bin/boks run claude .                      # attaches it; no --inject anywhere
```

Boks knows eleven services by name — `boks secret services` lists them — and for each one it
already has the hosts the credential is sent to, the header that carries it, the environment
variable the guest's own client reads it from, and the shape a convincing placeholder has.
Storing a key under one of those names is the whole configuration.

```bash
./bin/boks secret services                   # the services, and what boks knows about each
./bin/boks secret import                     # offer the keys already in this shell, Y/n each
./bin/boks secret adopt claude-code          # take over a subscription login you already have
./bin/boks secret ls                         # names, kinds and destinations — never values
./bin/boks run --no-secrets shell .          # a sandbox that carries none of them
```

The real value never enters the sandbox. The guest gets a placeholder shaped like a real key
— `sk-ant-api03-boksproxymanaged-…`, the vendor's own prefix, so the client's own format check
passes — and the host proxy swaps it for the credential on requests to those hosts and no
others. Two things are worth knowing before you type the first line: those hosts are the ones
whose **TLS Boks terminates**, which every run says out loud the first time; and a credential
rule is not an allow rule, so the host still has to be reachable
(`boks policy allow api.anthropic.com:443`).

Anything Boks does not know a service for is stored under a name of your own and attached by
a rule you write, which is also how you override a built-in one:

```bash
echo -n "$TOKEN" | ./bin/boks secret set my-internal-api
./bin/boks run --inject 'my-internal-api@api.internal.example.com=Authorization:Bearer %s' \
               --guest-credential 'my-internal-api=MY_API_KEY=placeholder' shell .
```

Two of the eleven — `cursor` and `droid` — are known by name and carry **no rule**: neither
vendor documents the host their CLI sends its key to, and Boks will not guess, because a
guessed rule ships your placeholder to the real API instead of your credential. Asking for one
says so, and gives you the two lines above instead.

### Policy is state, not an argument

Rules written with `boks policy` survive the command that wrote them, and are what `boks run`,
`boks start` and `boks exec` all serve a sandbox. A rule applies to every sandbox, or to one:

```bash
./bin/boks policy init --preset locked                     # choose the base posture
./bin/boks policy allow github.com:443 --note "git"        # every sandbox
./bin/boks policy allow --sandbox claude-myproject api.example.com:443
./bin/boks policy deny  metadata.example.com               # deny always wins, in every scope

./bin/boks policy check --sandbox claude-myproject api.example.com:443   # would this be permitted?
./bin/boks policy ls --sandbox claude-myproject                          # stored rules, and what they resolve to
./bin/boks policy profile create ci --preset locked --allow proxy.golang.org:443
./bin/boks run --profile ci shell .
```

**Precedence, in one sentence:** a deny in any scope beats an allow in any scope, and only the
base preset — chosen by `policy init`, a profile, or a `--policy` flag — decides what happens to
a destination no rule mentions. A sandbox-scoped rule can add access the machine's policy
already tolerates and can take access away; it can never widen past a deny someone wrote down.

**An agent brings its own destinations.** `boks run claude` needs `api.anthropic.com`, and
that is a fact about the agent, like its image — so it lives in the agent's record and
resolves as a layer of its own, visible in `boks policy ls --agent claude` beside the preset
and the global scope. It is a set of allows and nothing more: a deny in any scope beats it,
so `boks policy deny api.anthropic.com` denies it for the claude agent too, and `--policy
locked` drops the layer entirely because "deny everything" has to keep meaning that. Only
destinations a vendor documents as required go in one; telemetry endpoints do not, and
agents with no vendor source have an empty list, which their user fills with one
`boks policy allow` after seeing the denial in `boks policy log`.

```bash
./bin/boks policy ls --agent claude          # what running that agent would add, and why
./bin/boks policy log --sandbox claude-myproject --since 30m   # narrow the decision log
```

The `--policy`, `--allow`, `--deny` and `--net` flags are a Boks addition rather than sbx parity:
they override the stored policy for one run. `--policy` and `--allow` replace the posture and the
allow list; `--deny` is *added* to what the sandbox already denies, because a prohibition must
not disappear because this invocation typed a different one. A sandbox remembers the selection
it was created with, so a later `boks start` serves the same containment.

The mode is fixed when a sandbox is created, because it is expressed in annotations the
runtime reads at boot: `--net` on a sandbox that already exists is reported, not obeyed.

**A sandbox with nothing attached still has its network.** It lives in a `boks net serve`
process, one per running sandbox, started on demand and never at boot; it exits when the
sandbox's task exits, so `boks stop` and `boks rm` take it with them.

## Architecture

```
boks CLI → containerd → containerd-shim-nerdbox-v1 → libkrun → microVM
                                                                 ├── workspace via virtiofs
                                                                 └── your command
```

Boks is the orchestration layer, not a hypervisor. It uses containerd's Go API to create a
container whose runtime handler is `io.containerd.nerdbox.v1`, which boots a microVM per
sandbox. Everything below that line is existing open-source software.

Full detail, including what each layer provides and what Boks still has to build:
[docs/architecture.md](docs/architecture.md).

## Security model

Assume code in the sandbox is hostile. The hypervisor is the boundary; in-guest permissions
are not — the agent has root in the guest by design.

Two things to understand before using Boks:

- **Workspace writes are live on your host.** A sandbox can modify `Makefile`,
  `package.json` scripts, Git hooks or CI config, which then run on *your* machine. Review
  diffs before running anything from a workspace a sandbox touched.
- **A guest that ignores the proxy is still judged.** This was measured broken on
  2026-08-12 — a denied host answered normally and a raw TLS handshake reached the real
  origin — and measured fixed against a real VM on 2026-08-13: the same handshake is now
  refused before anything is dialled, and every decision is logged. Enforcement is on the
  address in the packet, so hostname rules do not authorise raw connections.
- **Hosts you configure a credential for are decrypted by Boks**, by design, and `boks run`
  tells you which. Everything else is tunnelled with the origin's own certificate chain.
  It says so the first time a sandbox meets a given host, and again for any host it has not
  named before — including under `--quiet`, which suppresses the rest of the network summary
  but never that. The steady-state run is two lines, because a notice printed fifty lines at
  a time before every command is a notice people learn to skip.

Boks will never mount the host's Docker or containerd socket into a guest.

Full analysis, including trust boundaries and ranked escape surfaces:
[docs/security-model.md](docs/security-model.md).

## Docker Sandbox comparison

Docker Sandboxes is the behavioural reference. Boks reproduces observable behaviour from
public documentation using open-source components; it is not derived from Docker's code.

| | Docker Sandboxes | Boks |
|---|---|---|
| microVM per sandbox | yes | **yes**, verified on macOS |
| exact-path workspace | yes | **yes**, verified |
| workspace `:ro`, no parent exposure | yes | **yes**, verified |
| sandbox lifecycle (`ls`/`exec`/`stop`/`rm`/`cp`) | yes | **yes**, not yet exercised behind a VM |
| agent-first CLI, readable sandbox names | yes | **yes** — same grammar, same naming rule |
| prepared agent images | yes, ten of them | **nine of ten**, multi-arch on a shared base; `kiro` needs `--template` |
| terminal dashboard for bare `sbx` | yes | no — see the parity matrix |
| network policy enforced outside the guest | yes | **yes** — the host stack judges every TCP connection by address before dialling; verified against a real guest |
| UDP and ICMP blocked at the network layer | yes | **yes**, except DNS to the sandbox's own resolver |
| credential injection by host proxy | yes | **yes**, HTTP and HTTPS, verified inside a real guest |
| port publishing, loopback by default | yes | **yes**, TCP; same grammar, same flags, same default. Exercised against a simulated guest only |
| credentials keyed by a known service name | yes, eleven of them | **nine of eleven** — same names, only vendor-documented rules; `cursor` and `droid` are known and unconfigured |
| host-side OAuth login (`--oauth`) | yes | no — needs a client id the vendor issues to a registered app; `boks secret adopt` takes a login you already have instead |
| Docker daemon inside the sandbox | yes | planned |
| kits / declarative config | yes | planned |
| account required | yes | **never** |
| telemetry | yes, opt-out | **none** |
| org governance, audit control plane | yes, paid | won't replicate |

Feature-by-feature detail with priorities:
[docs/docker-sandbox-parity.md](docs/docker-sandbox-parity.md).

## Roadmap

Ordered by what unblocks the most. The VM boundary is done and the network datapath now
enforces — what matters most is watching it work against a real guest.

1. A repair path for a crashed network supervisor: the VM does not re-attach to a restarted
   stack, so today the sandbox loses its network until it is restarted
2. Policy over names for raw flows, so that `--allow example.com` can authorise a direct
   connection to the address it resolves to rather than denying it
3. The interactive dashboard that bare `boks` should open
4. Clone mode measured behind a hypervisor — it is built and verified on runc, where the
   read-only source share is a bind mount rather than virtiofs
5. Docker daemon inside the guest
6. UDP port publishing, which needs the link filter to carry a datagram's return path without
   becoming a general hole
8. Kits / declarative configuration
9. Windows — **the stack underneath Boks runs a container there; `boks run` does not.** On
   2026-08-14, on real Windows 11 hardware, `ctr tasks start` carried a Linux container end to
   end through containerd, a patched nerdbox shim, `krun.dll` and the Windows Hypervisor
   Platform, booting in 1.9 s. What is left for Boks is **virtio-net** — exactly the device its
   enforcement depends on — across which no Ethernet frame has yet passed on Windows, so
   `boks run` refuses to start sandbox networking there rather than pretend. In the meantime
   Boks should run **inside WSL2** with nested virtualisation: unchanged, with workspace paths
   preserved exactly. Untested, but every ingredient is there. See
   [docs/windows.md](docs/windows.md)

## Development

```bash
make build     # build ./bin/boks
make test      # unit tests
make check     # vet + unit tests
make integration   # requires a running containerd; see below

make images       # build the agent images for this architecture; needs docker
make images-test  # each CLI runs, uid 1000, tini as PID 1, an injected CA lands
```

`make images` builds for the host architecture only. Multi-arch images are published from
`.github/workflows/images.yml` on a release tag, using a native runner per architecture.

Integration tests drive a real containerd and are skipped unless `BOKS_INTEGRATION=1`. By
default they run against the isolating runtime, so a pass means the assertions held behind a
VM boundary:

```bash
BOKS_INTEGRATION=1 go test ./internal/sandbox/ -run Integration -v
```

On a host without a hypervisor you can exercise the orchestration path only, by pointing
them at another runtime. This proves the containerd plumbing, **not** isolation, and the
suite says so in its output:

```bash
BOKS_INTEGRATION=1 BOKS_TEST_RUNTIME=io.containerd.runc.v2 \
  BOKS_TEST_SNAPSHOTTER=native go test ./internal/sandbox/ -run Integration -v
```

## License

Apache-2.0. See [LICENSE](LICENSE).
