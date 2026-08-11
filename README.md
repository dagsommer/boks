# Boks

Run coding agents and other untrusted developer tooling inside isolated microVMs, on your
own machine.

Boks is a local-first, open-source alternative to Docker Sandboxes (`docker sbx`). No
account, no cloud service, no telemetry.

> [!WARNING]
> **Boks is experimental and incomplete.** The VM boundary it is built around has not yet
> been demonstrated by this project on hardware with virtualisation available — see
> [Status](#status). Do not rely on it to contain hostile code today.

## Status

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

What is **not** done:

- **The VM boundary is unverified.** The development machine is itself a VM without nested
  virtualisation, so no hypervisor was available to boot a guest. The orchestration was
  validated against containerd's `runc` runtime, which shares the host kernel and is *not*
  isolation. `boks run` refuses non-VM runtimes unless you explicitly opt out.
- **No network policy.** A sandbox has whatever network access the runtime gives it. There
  is no allowlist, no proxy, no enforcement.
- **No credential injection.** Any secret you put in a sandbox is in the sandbox.
- **No nested Docker**, no persistent sandboxes, no `ls`/`stop`/`rm`/`exec`, no kits.
- Linux only in practice. macOS is designed for, not tested. Windows is unsupported.

See [docs/verification.md](docs/verification.md) for exactly what has been observed and the
procedure to confirm the VM boundary on capable hardware.

## Requirements

- Linux with hardware virtualisation (`/dev/kvm`, and membership of the `kvm` group)
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
| microVM per sandbox | yes | designed, unverified |
| exact-path workspace | yes | yes (unverified in a VM) |
| network policy enforced outside the guest | yes | planned |
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

Ordered by what unblocks the most:

1. Confirm the VM boundary on hardware with virtualisation, and record the evidence
2. Network isolation: host-side userspace netstack, deny-by-default, allowlist
3. Host forward proxy with hostname filtering (no TLS interception)
4. Credential injection — real secrets stay on the host
5. Persistent sandboxes and the `ls`/`stop`/`rm`/`exec` lifecycle
6. Clone mode, so guest writes do not land on the host by default
7. Docker daemon inside the guest
8. Kits / declarative configuration
9. macOS, then Windows

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
