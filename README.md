# Boks

Run coding agents and other untrusted developer tooling inside isolated microVMs, on your
own machine.

Boks is a local-first, open-source alternative to Docker Sandboxes (`docker sbx`). No
account, no cloud service, no telemetry.

> [!WARNING]
> **Boks is experimental and incomplete.** The VM boundary is real and has been measured
> (see [Status](#status)), but there is **no network policy**: a sandbox reaches the
> internet *and* your host's own loopback services. Do not rely on Boks to contain hostile
> code today.

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
- `boks run <workspace> -- <command>` — creates an ephemeral sandbox through containerd,
  runs a command, streams its output, returns its exit code, and cleans up.
- **Exact-path workspace sharing** — the workspace appears inside the guest at its absolute
  host path, writes reach the host, `:ro` is honoured, and directories above the workspace
  are not exposed.
- Cleanup leaves no containers, tasks, shim processes or mounts behind, including after
  Ctrl-C.

- **Symlinks do not escape.** A symlink inside the workspace pointing at `/etc` or `~/.ssh`
  resolves inside the guest, not on the host.

What is **not** done:

- **No network policy — and the default is more open than you may expect.** The guest has
  no virtual NIC, but libkrun's TSI performs its connections on the host, so the sandbox
  reaches the internet *and* anything listening on your host's `127.0.0.1`. There is no
  allowlist, no proxy, no enforcement. This is the biggest gap.
- **No credential injection.** Any secret you put in a sandbox is in the sandbox.
- **No nested Docker**, no persistent sandboxes, no `ls`/`stop`/`rm`/`exec`, no kits.
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

./bin/boks run . -- uname -a
./bin/boks run . -- sh -lc 'pwd && ls'
./bin/boks run /home/alice/src/foo -- sh
```

The workspace is shared into the guest at the same absolute path it has on the host, and is
the process's working directory. Nothing above it is exposed.

Useful flags:

| Flag | Meaning |
|---|---|
| `-image` | guest root filesystem (default `alpine:latest`) |
| `-mount PATH[:ro]` | share an extra directory (repeatable) |
| `-cpus`, `-memory` | guest vCPUs and MiB |
| `-env KEY=VALUE` | set an environment variable (repeatable) |
| `-t` | allocate a pseudo-terminal |

A workspace argument may carry a `:ro` suffix for a read-only share.

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
- **There is no network policy yet.** A guest can reach whatever the runtime permits.

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
| network policy enforced outside the guest | yes | no — guest reaches host loopback |
| credential injection by host proxy | yes | planned |
| Docker daemon inside the sandbox | yes | planned |
| persistent sandboxes, `ls`/`stop`/`rm`/`exec` | yes | planned |
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
4. Verify the Linux/KVM path end to end, as macOS has been
5. Persistent sandboxes and the `ls`/`stop`/`rm`/`exec` lifecycle
6. Clone mode, so guest writes do not land on the host by default
7. Docker daemon inside the guest
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
