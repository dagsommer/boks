# Boks

Run coding agents and other untrusted developer tooling inside isolated microVMs, on your
own machine.

Boks is a local-first, open-source alternative to Docker Sandboxes (`docker sbx`). No
account, no cloud service, no telemetry.

> [!WARNING]
> **Boks is experimental and incomplete.** The VM boundary is real and has been measured
> (see [Status](#status)), but **no network policy is enforced on a running sandbox**: it
> reaches the internet *and* your host's own loopback services. Do not rely on Boks to
> contain hostile code today.

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
- **Agents** — `shell` is the one Boks ships an image for; the other names `sbx` uses
  (`claude`, `codex`, `copilot`, `cursor`, `docker-agent`, `droid`, `gemini`, `kiro`,
  `opencode`) are registered and run with an explicit `-template`.
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

What is **not** done:

- **No network policy in the datapath — and the default is more open than you may expect.**
  The guest has no virtual NIC, but libkrun's TSI performs its connections on the host, so
  the sandbox reaches the internet *and* anything listening on your host's `127.0.0.1`.
  A policy engine, a host forward proxy (`boks proxy`, `boks policy ls|log`) and the
  host-side network configuration now exist and are tested, but **nothing is applied to a
  running sandbox yet**. This is still the biggest gap.
- **No credential injection in a sandbox.** The mechanism exists and works through
  `boks proxy`; `boks run` does not use it. Any secret you put in a sandbox is in the
  sandbox.
- **Credentials cannot be injected into HTTPS at all** without terminating TLS, which Boks
  deliberately does not do. Only plaintext HTTP injection works today.
- **No nested Docker**, no kits, no port publishing.
- **No prepared agent images.** The agent registry is real, but only `shell` has an image
  behind it, and there is no way yet to define an agent in a file rather than in code.
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

A pseudo-terminal is allocated when stdin and stdout are both terminals, and never when
either is a pipe, so there is no flag for it.
| `-policy`, `-allow`, `-deny`, `-net`, `-secret` | network policy — **validated and printed, not yet applied** |

A workspace argument may carry a `:ro` suffix for a read-only share.

Network policy has its own commands while it is not part of a run:

```bash
./bin/boks policy ls -policy standard        # what a preset resolves to, and why
./bin/boks proxy -policy locked -allow api.example.com:443 -v
./bin/boks policy log                        # what was allowed or denied, and why
./bin/boks secret set github                 # a credential the guest never receives
```

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
- **No network policy is enforced yet.** A guest can reach whatever the runtime permits.
  `boks policy ls` shows what a policy would resolve to and `boks proxy` filters traffic
  sent through it, but nothing constrains a running sandbox.

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
| prepared agent images | yes, ten of them | only `shell`; the other names need `-template` |
| terminal dashboard for bare `sbx` | yes | no — see the parity matrix |
| network policy enforced outside the guest | yes | engine + proxy built, not wired into `run` |
| credential injection by host proxy | yes | HTTP only, and not wired into `run` |
| Docker daemon inside the sandbox | yes | planned |
| kits / declarative config | yes | planned |
| account required | yes | **never** |
| telemetry | yes, opt-out | **none** |
| org governance, audit control plane | yes, paid | won't replicate |

Feature-by-feature detail with priorities:
[docs/docker-sandbox-parity.md](docs/docker-sandbox-parity.md).

## Roadmap

Ordered by what unblocks the most. The VM boundary is done — networking is now the gap
that matters, because today a sandbox can reach your host's own services.

1. Network isolation: replace TSI with a virtio-net link into a host-side userspace
   netstack, giving deny-by-default and an allowlist that the guest cannot opt out of
2. Host forward proxy with hostname filtering (no TLS interception)
3. Credential injection — real secrets stay on the host
4. Confirm the lifecycle against a real hypervisor — it has so far only been exercised
   without one, so stop/start over a live microVM is unverified
5. Clone mode, so guest writes do not land on the host by default
6. Docker daemon inside the guest
7. Port publishing, and the dashboard-style listing that needs it
8. Kits / declarative configuration
9. Windows, once the runtime supports it

## Development

```bash
make build     # build ./bin/boks
make test      # unit tests
make check     # vet + unit tests
make integration   # requires a running containerd; see below
```

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
