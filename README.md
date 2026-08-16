# Boks

Run coding agents and other untrusted developer tooling inside isolated microVMs, on your
own machine.

Point Boks at a directory and the code you do not trust gets a virtual machine of its own:
its own Linux kernel, its own network, and none of your filesystem but the directory you
named. Your API keys stay on the host and are attached to outbound requests there, so they
never enter the sandbox. Everything runs locally — no account, no cloud service, no
telemetry.

> [!WARNING]
> **Boks is experimental.** The VM boundary and the network policy have each been measured
> against a real guest on macOS, Windows and Linux, and both hold on all three — but the
> Linux run was inside WSL2 rather than on bare metal, a single hypervisor sits behind all
> of it, and plenty is still unbuilt. Judge it on what
> [the verification record](docs/verification.md) says was observed, not on what the feature
> list implies.

## Where it stands

Boks boots a real microVM per sandbox and enforces its network policy outside the guest.
Both of those have been watched from inside a real guest on three platforms — not simulated,
and not inferred from the code:

| Platform | What has been observed there |
|---|---|
| **macOS on Apple silicon** | The most thoroughly measured. The VM boundary on 2026-08-11 — a Linux guest against a Darwin host, its own `boot_id`, an uptime of hundredths of a second, vCPU and memory counts tracking the flags rather than the host. Network policy, credential injection and the sandbox lifecycle against a real guest on 2026-08-13. |
| **Windows 11 on x64** | `boks run` boots a sandbox natively through the Windows Hypervisor Platform, from an ordinary unelevated terminal. On 2026-08-15 an allowed host returned HTTP 200 through Boks' own network stack while a denied one was refused, and workspace write-through, persistence across `stop`, eight-vCPU SMP and clean teardown were all confirmed. One machine, x64 only — there is no arm64 build, and no release you can install yet. |
| **Linux with `/dev/kvm`** | Verified end to end for the first time on 2026-08-15, in WSL2 on Ubuntu 26.04: 25 of 26 checks passed, three distinct `boot_id`s, and the network boundary held with its positive control — an allowed address completed TLS against the origin's own certificate while a denied one was refused in the same sandbox. |

Two limits worth reading before you count Linux as done: that run was **inside WSL2, not on
bare metal**, and **creating a sandbox on Linux still needs more privilege than an ordinary
user has** — `boks` mounts the image overlay on the host to read the image config, which
fails with `operation not permitted`. Windows no longer has an equivalent requirement, which
is the wrong way round and is being worked on.

The evidence behind every claim above — hardware, dates, commands, and the checks that
failed — is in [docs/verification.md](docs/verification.md). What is still missing, stated
the way it will be measured rather than the way it would sell, is in
[docs/roadmap.md](docs/roadmap.md).

## Requirements

- Hardware virtualisation: Linux with `/dev/kvm` (and membership of the `kvm` group), or
  macOS on Apple silicon (Hypervisor.framework). macOS additionally needs the nerdbox shim
  codesigned with the `com.apple.security.hypervisor` entitlement and a user-writable
  `/var/run/containerd` — see [docs/verification.md](docs/verification.md#macos-setup-notes).
- [containerd](https://containerd.io/) 2.3 or later, running — not 2.2, which cannot decode
  the shim's bootstrap parameters
- [nerdbox](https://github.com/containerd/nerdbox) — the VM runtime shim
  (`containerd-shim-nerdbox-v1`) on containerd's `PATH`
- [libkrun](https://github.com/containers/libkrun) 1.18 or later
- `erofs-utils` (for `mkfs.erofs`)
- Go 1.26+ to build

Docker Desktop is not required.

Run `boks doctor` — it checks all of the above and tells you what to do about each gap.
Nothing should be `fail`. On macOS one check warns on a perfectly good host and always will:
`virtualization` cannot be probed without booting a VM, so it reports architecture support
and says as much. On Linux that check reads `/dev/kvm` and is `ok` or `fail`.

## Installing

[docs/install.md](docs/install.md) covers every route, per platform, with the prerequisites
each one leaves you holding. The short version:

- **macOS on Apple silicon** — a Homebrew tap installs the CLI, containerd, libkrun,
  erofs-utils, a nerdbox shim signed with the entitlement libkrun needs, and nerdbox's guest
  kernel and root filesystem from a Boks release. `boks doctor` reports those last two as
  `guest image` and fails when they are missing, so a green `doctor` is worth more than it
  used to be. The tap is not published yet, and no Mac has run the formulae.
- **Linux** — a tarball, a `.deb` or an `.rpm`. The packages vendor the runtime alongside the
  CLI, because no distribution ships a containerd new enough and nerdbox is packaged nowhere.
  Expect to run as root for now: sandbox creation still needs more privilege than an ordinary
  user has.
- **Windows** — a winget manifest and a zip, for the native Windows Hypervisor Platform
  route. No release has been cut yet, so building from source is the way in today.

Nothing has been published to a package repository yet — the first release tag is what turns
any of the above from a described route into a downloadable file.

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

Useful flags:

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

Flags follow the usual conventions — `--long` with short forms for the common ones — and
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

The `--policy`, `--allow`, `--deny` and `--net` flags override the stored policy for one
run. `--policy` and `--allow` replace the posture and the
allow list; `--deny` is *added* to what the sandbox already denies, because a prohibition must
not disappear because this invocation typed a different one. A sandbox remembers the selection
it was created with, so a later `boks start` serves the same containment.

The mode is fixed when a sandbox is created, because it is expressed in annotations the
runtime reads at boot: a `--net` that disagrees with how an existing sandbox was wired is
refused, and nothing runs. It cannot be applied, and ignoring it would mean `--net none`
quietly getting a network.

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

A short tour of how the pieces fit — the boundary, where policy is enforced, what persists:
[docs/how-it-works.md](docs/how-it-works.md). Full detail, including what each layer
provides and what Boks still has to build: [docs/architecture.md](docs/architecture.md).

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

## Roadmap

Ordered by what unblocks the most. The VM boundary holds and the network datapath enforces
on all three platforms; what is left is mostly reach and repair rather than the core.

1. A repair path for a crashed network supervisor: the VM does not re-attach to a restarted
   stack, so today the sandbox loses its network until it is restarted
2. Sandbox creation on Linux without root, which today needs a host mount an ordinary user
   cannot make
3. Policy over names for raw flows, so that `--allow example.com` can authorise a direct
   connection to the address it resolves to rather than denying it
4. The interactive dashboard that bare `boks` should open
5. Clone mode measured behind a hypervisor — it is built and verified on runc, where the
   read-only source share is a bind mount rather than virtiofs
6. Docker daemon inside the guest, UDP port publishing, and kits / declarative configuration

The full list, grouped by what each gap costs you, is
[docs/roadmap.md](docs/roadmap.md).

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
