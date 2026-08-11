# Boks

Run coding agents and other untrusted developer tooling inside isolated microVMs, on your
own machine.

Boks is a local-first, open-source alternative to Docker Sandboxes (`docker sbx`). No
account, no cloud service, no telemetry.

> [!WARNING]
> **Boks is experimental and incomplete.** The VM boundary is real and has been measured
> (see [Status](#status)). Network policy is now applied to a running sandbox — the guest's
> NIC is terminated by a host-side stack that drops what no rule permits — but **that has
> never been demonstrated against a real guest**, because it was built on a machine with no
> hypervisor. Treat it as designed and tested, not proven. Do not rely on Boks to contain
> hostile code today.

## Status

The VM boundary is **verified**. On 2026-08-11, on an Apple M5 Pro running macOS 26.5.2,
`boks run` booted a real microVM: the guest reported **Linux 6.12.44 aarch64** while the
host ran **Darwin 25.5.0**, with its own `boot_id`, an uptime of 0.03 s against the host's
28 days, and vCPU and memory counts matching `-cpus`/`-memory` rather than the host's 18
cores and 48 GiB. A shared-kernel container cannot produce any of that. Full evidence and
procedure: [docs/verification.md](docs/verification.md).

What works, tested locally:

- `boks doctor` — inspects containerd, the VM runtime, virtualisation, snapshotter and its
  host tooling, and explains how to fix what is missing.
- `boks run [agent] [workspace...]` — agent-first, like `sbx run`. Creates a sandbox
  through containerd or re-attaches to the one that agent and directory already have, runs
  the agent (or a command after `--`), streams its output, and returns its exit code.
  `-rm` makes it ephemeral instead.
- **Agents with prepared images** — `claude`, `codex`, `copilot`, `cursor`, `docker-agent`,
  `droid`, `gemini`, `opencode` and `shell` each resolve to an image at
  `ghcr.io/dagsommer/boks/`, built from [`images/`](images/): a shared Debian base with the
  toolchain preinstalled, plus one thin layer per agent. `kiro` is registered without an
  image and needs an explicit `-template`. Every CLI has been run; none has yet been run
  inside a microVM. See [images/README.md](images/README.md), including why nothing in them
  is installed by a package manager.
- **The sandbox lifecycle** — `create`, `ls`, `inspect`, `start`, `stop`, `exec`, `rm`, `cp`.
  Sandboxes persist until removed, and files written inside one survive stop/start. *Tested
  end to end against containerd on the non-VM dev runtime only* — see below.
- **Exact-path workspace sharing** — the workspace appears inside the guest at its absolute
  host path, writes reach the host, `:ro` is honoured, and directories above the workspace
  are not exposed.
- Cleanup leaves no containers, tasks, shim processes or mounts behind, including after
  Ctrl-C.

- **Symlinks do not escape.** A symlink inside the workspace pointing at `/etc` or `~/.ssh`
  resolves inside the guest, not on the host.

- **Network policy is applied to a sandbox.** `boks run` gives the sandbox a virtual NIC
  whose far end is a userspace network stack in a Boks process, and points the guest at a
  filtering proxy *inside* that virtual network. Destinations no rule permits have nothing
  to answer them; `-net none` gives a sandbox no network at all. The stack lives in a small
  per-sandbox process so that it lasts as long as the sandbox's VM rather than as long as
  your command — a build running in a sandbox does not lose the network when you press
  Ctrl-C. `boks net ls` shows them, `boks stop` takes them down.
  **Not demonstrated against a real guest:** see the warning above.
- **Credential injection in a sandbox**, over HTTP and HTTPS. The guest holds a placeholder
  in the environment variable its tooling reads; the real secret stays on the host and is
  attached to requests for the hosts you named. Those hosts — and only those — have their
  TLS terminated, which `boks run` says out loud when you configure it.

What is **not** done:

- **None of the enforcement has met a real guest.** Everything above is exercised against a
  simulated guest attached to the real link socket, on a host with no hypervisor. The
  transport it rests on *was* verified on real hardware; the enforcement built on it was
  not.
- **No nested Docker**, no kits, no port publishing.
- **The agent images have never run in a microVM.** They were built and exercised with
  `docker run`, which proves each CLI is installed and starts — and nothing at all about
  isolation. `kiro` has no image (see [images/README.md](images/README.md) for why), and
  there is still no way to define an agent in a file rather than in code.
- **The images are not published until a release tag is pushed.** Until then the references
  in the agent registry resolve to nothing; build them locally with `make images`.
- **No terminal dashboard** for bare `boks`, no `--clone`, `--kit`, `--profile` or
  `--publish`. See the CLI surface section of the parity matrix.
- **The lifecycle commands have not been exercised behind a VM.** They were built and tested
  against a real containerd on a host with no hypervisor, using the runc dev runtime. The
  containerd orchestration is verified; the VM-specific behaviour of `stop`/`start` (a
  microVM being torn down and rebooted over the same snapshot) is not.
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

## Quick start

```bash
make build

./bin/boks doctor

./bin/boks run                              # a shell in the current directory
./bin/boks run shell . -- uname -a
./bin/boks run shell . -- sh -lc 'pwd && ls'
./bin/boks run -rm shell /home/alice/src/foo -- go test ./...
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
./bin/boks ls                    # SANDBOX  AGENT  STATUS  PORTS  WORKSPACE
./bin/boks exec -it $(./bin/boks ls -q) sh
./bin/boks stop <name>           # keeps everything inside
./bin/boks cp ./file.txt <name>:/root/file.txt
./bin/boks rm <name>             # deletes the sandbox and its filesystem
```

**The name is the identity.** A sandbox is called `<agent>-<workspace directory>` —
`shell-boks` for the `shell` agent in `~/git_repos/boks` — and that derived name is what a
second run looks up, so naming and re-attach are the same mechanism. Two different
directories with the same name are not merged: the second one gets
`<agent>-<dir>-<digest>` and is told why. `-name` overrides the derivation, which is how one
workspace gets several sandboxes, and how a sandbox is reached from anywhere:

```bash
./bin/boks run -name shell-boks  # re-attach by name, from any directory
```

Useful flags, named after sbx's:

| Flag | Meaning |
|---|---|
| `-t`, `-template` | guest root filesystem (default: the agent's image) |
| `-name` | name the sandbox instead of deriving it from agent and workspace |
| `-d`, `-detached` | print the sandbox name and exit instead of attaching |
| `-rm` | destroy the sandbox when the command exits |
| `-mount PATH[:ro]` | share an extra directory (repeatable) |
| `-cpus` | guest vCPUs (0: all host CPUs) |
| `-m`, `-memory` | guest memory, binary units (`1024m`, `8g`; default half the host's, max 32g) |
| `-env KEY=VALUE` | set an environment variable (repeatable) |
| `-net none\|nat` | no network at all, or a policed one (default `nat`) |
| `-policy`, `-allow`, `-deny` | the network policy the sandbox runs under |
| `-inject`, `-guest-credential` | attach a host-held credential to named hosts |

A pseudo-terminal is allocated when stdin and stdout are both terminals, and never when
either is a pipe, so there is no flag for it. A workspace argument may carry a `:ro` suffix
for a read-only share.

### The network a sandbox gets

```bash
./bin/boks run -net none shell .                    # no network at all: the strongest containment
./bin/boks run -policy locked -allow api.example.com:443 shell .
./bin/boks run -inject 'anthropic@api.anthropic.com=x-api-key' \
               -guest-credential 'anthropic=ANTHROPIC_API_KEY=sk-ant-placeholder' shell .

./bin/boks net ls                            # the stacks currently serving sandboxes
./bin/boks policy ls -policy standard        # what a preset resolves to, and why
./bin/boks policy log                        # what was allowed or denied, and why
./bin/boks secret set github                 # a credential the guest never receives
./bin/boks proxy -policy locked -v           # the same proxy, standalone, for anything
```

The mode is fixed when a sandbox is created, because it is expressed in annotations the
runtime reads at boot: `-net` on a sandbox that already exists is reported, not obeyed.

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
- **The network enforcement has never met a real guest.** A sandbox is wired to a host-side
  stack that drops what policy forbids, and that path is tested — against a simulated guest,
  on a machine with no hypervisor. Nobody has yet watched a real VM be refused.
- **Hosts you configure a credential for are decrypted by Boks**, by design, and `boks run`
  tells you which. Everything else is tunnelled with the origin's own certificate chain.

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
| prepared agent images | yes, ten of them | **nine of ten**, multi-arch on a shared base; `kiro` needs `-template` |
| terminal dashboard for bare `sbx` | yes | no — see the parity matrix |
| network policy enforced outside the guest | yes | **yes** — host-side netstack per sandbox; never seen a real guest |
| credential injection by host proxy | yes | **yes**, HTTP and HTTPS; same caveat |
| Docker daemon inside the sandbox | yes | planned |
| kits / declarative config | yes | planned |
| account required | yes | **never** |
| telemetry | yes, opt-out | **none** |
| org governance, audit control plane | yes, paid | won't replicate |

Feature-by-feature detail with priorities:
[docs/docker-sandbox-parity.md](docs/docker-sandbox-parity.md).

## Roadmap

Ordered by what unblocks the most. The VM boundary is done and the network datapath is
wired — what matters now is watching it work against a real guest.

1. **Confirm the enforcement against a real hypervisor.** Does the guest reach the gateway,
   is a denied host refused, does a running VM re-attach if its stack is restarted? See
   [docs/verification.md](docs/verification.md)
2. Confirm the lifecycle against a real hypervisor — it has so far only been exercised
   without one, so stop/start over a live microVM is unverified
3. The interactive dashboard that bare `boks` should open
4. Clone mode, so guest writes do not land on the host by default
5. Docker daemon inside the guest
6. Port publishing, and the `PORTS` column that has nothing to show without it
8. Kits / declarative configuration
9. Windows, once the runtime supports it

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
